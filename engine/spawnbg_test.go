package engine

// Phase W 后台派生回归（B 方案 B-1~B-10 用例；方案 = findings/2026-08-28-
// background-spawn-plan.md）。覆盖：即回 agentId / 完成通知注入（running 排队
// 与 idle 自续两路）/ spawn_id 会话域唯一与回放兼容零值 / 取消传播（停止全停）
// / ArgsForce fail-closed（hitl bg 档）/ 自激护栏（连续自续预算）与停止终态
// 不自续（防中断洗成模型请求）/ 会话域并发闸保留额。
// 执行环境 = 部署机容器（Windows 宿主 Application Control 拦测试二进制）。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// gateGenModel 可控子模型：Stream 阻塞等 release（或 ctx 取消）后吐结论——
// 后台任务生命周期由测试握住。
type gateGenModel struct {
	release chan struct{}
	reply   string
	mu      sync.Mutex
	streams int
	live    int
	peak    int
}

func (g *gateGenModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	// Generate 直通不等 release（genTitle 走此路径——控闸只挂 Stream：后台
	// 子代理经 Runner EnableStreaming 全走 Stream；若 Generate 也挂闸，
	// 异步标题生成会与测试时序互锁——首跑实证）
	return schema.AssistantMessage(g.reply, nil), nil
}

func (g *gateGenModel) Stream(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	g.mu.Lock()
	g.streams++
	g.live++
	if g.live > g.peak {
		g.peak = g.live
	}
	g.mu.Unlock()
	sr, sw := schema.Pipe[*schema.Message](2)
	go func() {
		defer func() {
			sw.Close()
			g.mu.Lock()
			g.live--
			g.mu.Unlock()
		}()
		select {
		case <-g.release:
			sw.Send(schema.AssistantMessage(g.reply, nil), nil)
		case <-ctx.Done(): // 取消传播：流即终（泵收 ctx.Canceled → failed 终态封口）
		}
	}()
	return sr, nil
}

func (g *gateGenModel) started() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.streams
}

func (g *gateGenModel) peakLive() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

// waitFor 轮询等待条件（3s 超时——后台 goroutine 异步语义的确定性锚）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// subeventsOf 事件流里取全部 EvSubAgent 载荷。
func subeventsOf(s *session.Session) []contract.SubAgentEvent {
	var out []contract.SubAgentEvent
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvSubAgent {
			if d, ok := ev.Data.(contract.SubAgentEvent); ok {
				out = append(out, d)
			}
		}
	}
	return out
}

// TestBgSpawnImmediateReturnAndKey B-1/B-4/B-7：background:true 即回 agentId
// JSON、父回合照常收口；后台泵事件带会话域 spawn_id；子代理流式事件归组键
// 齐备；旧事件零值序列化省略（回放兼容的契约面）。
func TestBgSpawnImmediateReturnAndKey(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		switch n {
		case 1:
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "spawn", `{"task":"后台勘察","background":true}`)}})
		default:
			send(&schema.Message{Role: schema.Assistant, Content: "已派后台，继续本职"})
		}
	}}
	gate := &gateGenModel{release: make(chan struct{}), reply: "后台结论：共 7 文件"}
	n := 0
	m, _ := newReductionManager(t, 0, []contract.Tool{rt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 {
			return gate, nil
		}
		return fm, nil
	}, func(o *Options) {
		o.SubAgents = &SubAgentsConfig{Tools: []string{"read_tool"}, EmitEvents: true}
	})
	s := m.Registry().Create("张三", "后台派生", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "派后台", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("父回合应正常收口（B-1），终态 %s", s.StateOf())
	}
	// 父第 2 调的 tool 消息 = agentId JSON（即回）
	over := toolMsgOf(fm.inputs[len(fm.inputs)-1])
	if len(over) == 0 || !strings.Contains(over[0], `"status":"background"`) || !strings.Contains(over[0], `"spawn_id":"sp1"`) {
		t.Fatalf("background 工具结果应即回 agentId JSON（B-1），实得 %v", over)
	}

	// 释放后台任务 → done 终态事件（spawn_id 归组键 + 结论全文）
	close(gate.release)
	waitFor(t, "后台 done 事件", func() bool {
		for _, e := range subeventsOf(s) {
			if e.SpawnID == "sp1" && e.Kind == "done" && strings.Contains(e.Text, "7 文件") {
				return true
			}
		}
		return false
	})
	// B-7 契约面：同步路径事件零值 spawn_id 序列化省略（旧数据回退启发式前提）
	b, _ := json.Marshal(contract.SubAgentEvent{Agent: "spawn", Kind: "text", Text: "x"})
	if strings.Contains(string(b), "spawn_id") {
		t.Fatalf("零值 spawn_id 应省略（回放兼容），实得 %s", b)
	}
}

// TestBgConclusionSpill B-2 配套：超长结论（>4000 rune）外置 spill 域——
// done 事件带 read_file 取回指引；落盘文件经 Store 可读回全文。
func TestBgConclusionSpill(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "spawn", `{"task":"超长勘察","background":true}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "已派"})
	}}
	long := strings.Repeat("甲", 5000)
	gate := &gateGenModel{release: make(chan struct{}), reply: long}
	n := 0
	m, st := newReductionManager(t, 0, []contract.Tool{rt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 {
			return gate, nil
		}
		return fm, nil
	}, func(o *Options) {
		o.SubAgents = &SubAgentsConfig{Tools: []string{"read_tool"}}
	})
	s := m.Registry().Create("张三", "外置", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "派后台", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	close(gate.release)
	waitFor(t, "done 事件带取回指引", func() bool {
		for _, e := range subeventsOf(s) {
			if e.SpawnID == "sp1" && e.Kind == "done" && strings.Contains(e.Text, "read_file spill/spawn/sp1") {
				return true
			}
		}
		return false
	})
	// 落盘全文可读回（虚拟前缀 spill/spawn/sp1 ↔ sessions/<sid>/spill/spawn/sp1）
	full, ok := st.ReadUserTreeFile(s.Owner, "sessions/"+s.SID+"/spill/spawn/sp1")
	if !ok || len([]rune(string(full))) != 5000 {
		t.Fatalf("spill 全文应 5000 rune 落盘可读回，实得 ok=%v len=%d", ok, len(full))
	}
}

// TestBgNotifyQueuedWhileRunning B-2：父 running 时通知入队（notify_queued），
// 下一轮 Run 头部消费（notify_injected）且结论进模型输入。
func TestBgNotifyQueuedWhileRunning(t *testing.T) {
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "收到"})
	}}
	m, _ := newReductionManager(t, 0, nil, factoryOf(fm))
	s := m.Registry().Create("张三", "通知", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning) // 父在跑（模拟）

	m.NotifyOwner(s, "[后台子代理完成] 勘察\n结论：\n共 3 文件")
	waitFor(t, "notify_queued 事件", func() bool {
		for _, ev := range s.SnapshotEvents() {
			if ev.Event == contract.EvNotifyQueued {
				return true
			}
		}
		return false
	})
	if s.QueueLen() != 1 {
		t.Fatalf("通知应入队（B-2 running 排队），队列 %d", s.QueueLen())
	}
	// 下一轮消费：翻态 notify_injected + 结论进输入
	m.Run(context.Background(), s, "继续", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	injected := false
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvNotifyInjected {
			injected = true
		}
	}
	if !injected {
		t.Fatal("应落 notify_injected 翻态回执")
	}
	last := fm.inputs[len(fm.inputs)-1]
	found := false
	for _, msg := range last {
		if msg.Role == schema.User && strings.Contains(msg.Content, "共 3 文件") {
			found = true
		}
	}
	if !found {
		t.Fatalf("通知结论应进下一轮模型输入，实得 %+v", last)
	}
}

// TestBgNotifyIdleContinues B-3：父 idle 时通知自续一轮（模型可引用结论）。
func TestBgNotifyIdleContinues(t *testing.T) {
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "消化了后台结论"})
	}}
	m, _ := newReductionManager(t, 0, nil, factoryOf(fm))
	s := m.Registry().Create("张三", "自续", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateEnded) // idle

	m.NotifyOwner(s, "[后台子代理完成] 勘察\n结论：\n迁移清单 5 项")
	// 等自续轮真实发生（模型调用数增长——state 会先翻 running 再回 ended，
	// 等 ended 的写法会被初始 ended 态立即误过——首跑实证）
	fm.mu.Lock()
	base := len(fm.inputs)
	fm.mu.Unlock()
	waitFor(t, "自续轮模型调用发生", func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return len(fm.inputs) > base
	})
	waitFor(t, "自续轮收口", func() bool { return s.StateOf() == session.StateEnded })
	fm.mu.Lock()
	last := fm.inputs[len(fm.inputs)-1]
	fm.mu.Unlock()
	found := false
	for _, msg := range last {
		if msg.Role == schema.User && strings.Contains(msg.Content, "迁移清单 5 项") {
			found = true
		}
	}
	if !found {
		t.Fatalf("自续轮输入应含通知结论（B-3），实得 %+v", last)
	}
}

// TestBgCancelSpawns B-5：全停（停止按钮/删除钩子）→ 后台任务取消收线、
// 注册表清空、额度释放（后续派生不受残留占用）。
func TestBgCancelSpawns(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "spawn", `{"task":"长勘察","background":true}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "已派"})
	}}
	gate := &gateGenModel{release: make(chan struct{}), reply: "不该到达"}
	n := 0
	m, _ := newReductionManager(t, 0, []contract.Tool{rt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 {
			return gate, nil
		}
		return fm, nil
	}, func(o *Options) {
		o.SubAgents = &SubAgentsConfig{Tools: []string{"read_tool"}, MaxConcurrent: 6}
	})
	s := m.Registry().Create("张三", "全停", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "派后台", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	waitFor(t, "后台任务起跑", func() bool { return gate.started() > 0 })

	m.CancelSpawns(s.SID) // 停止=全停
	m.bgMu.Lock()
	reg := m.bg[s.SID]
	m.bgMu.Unlock()
	if reg == nil {
		t.Fatal("注册表应在")
	}
	waitFor(t, "注册表清空（B-5 goroutine 退出）", func() bool {
		reg.mu.Lock()
		defer reg.mu.Unlock()
		return len(reg.entries) == 0
	})
	waitFor(t, "取消终态封口（A3：failed 事件）", func() bool {
		for _, e := range subeventsOf(s) {
			if e.SpawnID != "" && e.Kind == "failed" && strings.Contains(e.Text, "已停止") {
				return true
			}
		}
		return false
	})
	// 取消收线不注入通知（用户刚按停止，不唤醒——防中断洗成模型请求）
	if s.QueueLen() != 0 {
		t.Fatalf("取消收线不应注入通知，队列 %d", s.QueueLen())
	}
}

// TestBgHitlArgsForceFailClosed B-6：hitl bg 档——ArgsForce 命中直接拒绝回喂
// （不挂起、不 Suspended）；对照 manual 档仍挂起（fail-closed 语义分叉）。
func TestBgHitlArgsForceFailClosed(t *testing.T) {
	rt, _ := tools.InferTool("write_tool", "写桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	cfg := hitl.ApprovalConfig{
		WriteTools: map[string]bool{"write_tool": true},
		ArgsForce:  map[string]func(string) bool{"write_tool": func(string) bool { return true }},
	}
	src := &decisionSourceStub{}
	bg := hitl.WrapTools([]contract.Tool{rt}, src, "bg", cfg)
	out, err := bg[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("bg 档应拒绝回喂不报错，实得 err=%v", err)
	}
	if !strings.Contains(string(out), "后台子代理内禁用") || !strings.Contains(string(out), `"ok":false`) {
		t.Fatalf("bg 档 ArgsForce 应拒绝 JSON 回喂，实得 %s", out)
	}
	manual := hitl.WrapTools([]contract.Tool{rt}, src, "manual", cfg)
	if _, err := manual[0].Invoke(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("manual 档 ArgsForce 应挂起（Suspend 错误面）")
	}
}

// decisionSourceStub 最小决议源（bg 档不消费决议——接口占位）。
type decisionSourceStub struct{}

func (*decisionSourceStub) TakeDecision() *contract.ApprovalDecision          { return nil }
func (*decisionSourceStub) TakeDecisionFor(string) *contract.ApprovalDecision { return nil }
func (*decisionSourceStub) TurnGranted() bool                                 { return false }
func (*decisionSourceStub) GrantTurn()                                        {}
func (*decisionSourceStub) TaskGranted() bool                                 { return false }
func (*decisionSourceStub) GrantTask()                                        {}

// TestBgNotifyBudgetGuard B-8：连续自续预算 3——第 4 次只入队不自续；用户
// 消息消费恢复预算。
func TestBgNotifyBudgetGuard(t *testing.T) {
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "续"})
	}}
	m, _ := newReductionManager(t, 0, nil, factoryOf(fm))
	s := m.Registry().Create("张三", "预算", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateEnded)

	for i := 1; i <= 3; i++ {
		waitFor(t, "上一次自续收口", func() bool { return s.StateOf() == session.StateEnded })
		m.NotifyOwner(s, "[后台子代理完成] t\n结论：\nx")
		waitFor(t, "自续轮发生", func() bool { return s.StateOf() == session.StateRunning })
	}
	waitFor(t, "第 3 次自续收口（预算耗尽）", func() bool { return s.StateOf() == session.StateEnded })
	// 第 4 次：预算尽 → 只入队不自续（B-8 降级）
	m.NotifyOwner(s, "[后台子代理完成] t4\n结论：\nx4")
	time.Sleep(200 * time.Millisecond)
	if s.StateOf() != session.StateEnded || s.QueueLen() != 1 {
		t.Fatalf("预算耗尽应只入队不自续（state=%s queue=%d）", s.StateOf(), s.QueueLen())
	}
	// 用户消息消费恢复预算（Run 头部 RestoreNotifyBudget）
	m.Run(context.Background(), s, "用户来了", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	m.NotifyOwner(s, "[后台子代理完成] t5\n结论：\nx5")
	waitFor(t, "预算恢复后自续发生", func() bool { return s.StateOf() == session.StateRunning })
	waitFor(t, "恢复的自续轮收口", func() bool { return s.StateOf() == session.StateEnded })
}

// TestBgNotifyTailErrorTerminal B-9：error 终态（用户停止）通知只入队不自续
// ——把用户中断洗成模型请求是被明确拒绝的形态（对照审查 A-3）。
func TestBgNotifyTailErrorTerminal(t *testing.T) {
	fm := &scriptedModel{}
	m, _ := newReductionManager(t, 0, nil, factoryOf(fm))
	s := m.Registry().Create("张三", "停止终态", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateError) // 用户按停止后的终态

	m.NotifyOwner(s, "[后台子代理完成] t\n结论：\nx")
	time.Sleep(200 * time.Millisecond)
	if s.StateOf() != session.StateError {
		t.Fatalf("error 终态不得自续（A-3），实得 %s", s.StateOf())
	}
	if s.QueueLen() != 1 {
		t.Fatalf("error 终态通知应只入队（下轮用户交互消费），队列 %d", s.QueueLen())
	}
	if len(fm.inputs) != 0 {
		t.Fatalf("不应发生任何模型调用，实得 %d", len(fm.inputs))
	}
}

// TestBgSessionGateReserve B-10：MaxConcurrent=3（保留额 2 → 后台额度 1）——
// 两后台任务串行（并发峰 1）；同步路径保留额恒可用（sem 剩余 ≥2）。
func TestBgSessionGateReserve(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "spawn", `{"task":"甲","background":true}`),
				tcOf("c2", "spawn", `{"task":"乙","background":true}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "双派"})
	}}
	gate := &gateGenModel{release: make(chan struct{}), reply: "结论"}
	n := 0
	m, _ := newReductionManager(t, 0, []contract.Tool{rt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n >= 2 {
			return gate, nil // 共享 gate：live/peak 内置计数（模型调用峰≈占用峰）
		}
		return fm, nil
	}, func(o *Options) {
		o.SubAgents = &SubAgentsConfig{Tools: []string{"read_tool"}, MaxConcurrent: 3}
	})
	s := m.Registry().Create("张三", "闸", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "双后台", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	// bgGate=1：甲先起跑（乙在子闸排队）；释放后甲完成放闸 → 乙自动接续
	// （release 已 close，乙直通）——两任务串行完成
	waitFor(t, "首个后台任务起跑", func() bool { return gate.started() >= 1 })
	close(gate.release)
	waitFor(t, "两任务全部 done", func() bool {
		done := 0
		for _, e := range subeventsOf(s) {
			if e.Kind == "done" {
				done++
			}
		}
		return done == 2
	})
	if p := gate.peakLive(); p != 1 {
		t.Fatalf("后台并发峰应为 1（bgGate=cap-reserve），实得 %d", p)
	}
	// 保留额：收尾后全池空闲（3 槽全放）——同步 spawn 恒可占用
	m.bgMu.Lock()
	reg := m.bg[s.SID]
	m.bgMu.Unlock()
	waitFor(t, "额度全释放", func() bool {
		if reg == nil || reg.sem == nil {
			return true
		}
		return len(reg.sem) == 0
	})
	// spawn_id 会话域唯一：两任务 sp1/sp2
	ids := map[string]bool{}
	for _, e := range subeventsOf(s) {
		if e.SpawnID != "" {
			ids[e.SpawnID] = true
		}
	}
	if len(ids) != 2 {
		t.Fatalf("应有两个唯一 spawn_id，实得 %v", ids)
	}
}
