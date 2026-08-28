package engine

// H2 spawn 子代理回归：端到端隔离（子输入=单条 user 零父历史、结论经 tool
// 结果进父窗口、子过程零进父历史）/ 白名单硬筛 + 失败显式回传（子调白名单
// 外工具 → 子运行报错 → errFeed JSON 回喂父、父运行不炸）。
// 细化方案 = findings/2026-08-26-h2-spawn-plan.md。

import (
	"context"
	"strings"
	"sync/atomic"
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

// TestSpawnEndToEndIsolation 父派子：子独立上下文（Generate 路径——agent_tool
// 非流式执行体）、结论内联父窗口、子中间过程零进父历史。
func TestSpawnEndToEndIsolation(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		switch n {
		case 1: // 父首调：派发 spawn
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "spawn", `{"task":"勘察仓库并统计文件","tools":"read_tool","expect":"文件数"}`)}})
		default: // 父收到结论后收口
			send(&schema.Message{Role: schema.Assistant, Content: "完成"})
		}
	}}
	sub := &recGenModel{reply: "子代理结论：仓库共 3 文件"} // 子模型（Generate 路径）独立记录
	n := 0
	m, _ := newReductionManager(t, 0, []contract.Tool{rt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 { // 构造序：主模型(1) → spawn 子代理(2)（genTitle 是第 3 次）
			return sub, nil
		}
		return fm, nil
	}, func(o *Options) {
		o.SubAgents = &SubAgentsConfig{Tools: []string{"read_tool"}}
	})
	s := m.Registry().Create("张三", "派子", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "帮我勘察", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if len(sub.inputs) != 1 {
		t.Fatalf("子代理应恰一次模型调用（Generate 路径），实得 %d", len(sub.inputs))
	}
	// 子输入 = 独立上下文（adk 给子模型前插 system 指令——末条才是任务载荷
	// user；零父历史）
	si := sub.inputs[0]
	if len(si) > 2 || si[len(si)-1].Role != schema.User || !strings.Contains(si[len(si)-1].Content, "勘察仓库并统计文件") {
		t.Fatalf("子输入应为 [system?] + 单条 user 载荷（独立上下文），实得 %d 条", len(si))
	}
	// 父第 2 调可见 spawn tool 结果（结论内联父窗口）
	over := toolMsgOf(fm.inputs[len(fm.inputs)-1])
	found := false
	for _, c := range over {
		if strings.Contains(c, "子代理结论") {
			found = true
		}
	}
	if !found {
		t.Fatalf("父窗口应内联子结论，实得 %+v", over)
	}
	// 父历史：子中间过程零进（父 assistant 段 = 派发段 + 收口段共 2 条；
	// 子结论只以 tool 结果形态内联，不得成为父 assistant 消息）
	nAssistant := 0
	for _, m2 := range s.CloneHistory() {
		if m2.Role == schema.Assistant {
			nAssistant++
			if strings.Contains(m2.Content, "子代理结论") {
				t.Fatal("子产出不得成为父 assistant 消息（只经 tool 结果内联）")
			}
		}
	}
	if nAssistant != 2 {
		t.Fatalf("父 assistant 段应恰 2 条（派发+收口），实得 %d", nAssistant)
	}
}

// TestSpawnWhitelistAndFailFeed 白名单硬筛（write_tool 不在子面 → 子调用即败）
// + 失败显式回传（errFeed JSON 回喂父、父运行正常收口不炸）。
func TestSpawnWhitelistAndFailFeed(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "spawn", `{"task":"尝试写操作","tools":"write_tool"}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "收到失败"})
	}}
	// 子（Generate 路径）调 write_tool：不在白名单 → 子面无此工具 → 子运行报错。
	// scriptedModel.Generate 恒回文本——改用本地子剧本模型驱动工具调用：
	subFM := &toolCallOnceModel{call: "write_tool"}
	m, _ := newReductionManager(t, 0, []contract.Tool{rt}, subFM.factory(fm), func(o *Options) {
		o.SubAgents = &SubAgentsConfig{Tools: []string{"read_tool"}}
	})
	s := m.Registry().Create("张三", "白名单", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "派子写", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("子失败不得杀父运行，终态 %s", s.StateOf())
	}
	over := toolMsgOf(fm.inputs[len(fm.inputs)-1]) // 父收口前最后一调（genTitle 走 Generate 不入 inputs）
	found := false
	for _, c := range over {
		if strings.Contains(c, "子代理执行失败") {
			found = true
		}
	}
	if !found {
		t.Fatalf("子失败应显式回传父（errFeed JSON），实得 %+v", over)
	}
}

// TestSpawnZeroApprovalDirectExec 子代理零审批直执（2026-08-26 裁决，codex
// 对齐 approval=never）：白名单含审批面写工具（ApprovalConfig.WriteTools 命中）
// 时子调用直接执行——零 approval_request、子任务不挂起、父正常收口。
func TestSpawnZeroApprovalDirectExec(t *testing.T) {
	calls := 0
	wt, _ := tools.InferTool("write_tool", "写桩", func(context.Context, struct{}) (map[string]any, error) {
		calls++
		return map[string]any{"ok": true}, nil
	})
	parent := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "spawn", `{"task":"写一个"}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	sub := &toolCallOnceModel{call: "write_tool"} // 子首调写工具、续调文本收口
	n := 0
	m, _ := newReductionManager(t, 0, []contract.Tool{wt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 {
			return sub, nil
		}
		return parent, nil
	}, func(o *Options) {
		o.SubAgents = &SubAgentsConfig{Tools: []string{"write_tool"}}
		o.Approval = hitl.ApprovalConfig{WriteTools: map[string]bool{"write_tool": true}}
	})
	s := m.Registry().Create("张三", "零审批", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var evNames []string
	m.Run(context.Background(), s, "派子写", nil, func(ev session.Event) { evNames = append(evNames, ev.Event) })
	waitTitleFlight(t, s)

	if calls != 1 {
		t.Fatalf("子代理写工具应直执恰一次，实得 %d", calls)
	}
	for _, n := range evNames {
		if n == contract.EvApprovalRequest {
			t.Fatal("零审批裁决违例：子代理触发了审批卡（任务将卡死等人）")
		}
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("父应正常收口，终态 %s（事件 %v）", s.StateOf(), evNames)
	}
}

// TestSpawnEmitEventsForwarded 全量转发档（H8-2）：EmitEvents 开 → 子代理
// 内部事件翻译 EvSubAgent 进父流（tool_call/text 只读流）；子过程零进父
// 上下文不变（转发件不入父 runSession——官方注释实证）。
func TestSpawnEmitEventsForwarded(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "spawn", `{"task":"勘察","tools":"read_tool"}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	sub := &toolCallOnceModel{call: "read_tool"} // 子首调读工具、续调文本收口
	n := 0
	m, _ := newReductionManager(t, 0, []contract.Tool{rt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 {
			return sub, nil
		}
		return fm, nil
	}, func(o *Options) {
		o.SubAgents = &SubAgentsConfig{Tools: []string{"read_tool"}, EmitEvents: true}
	})
	s := m.Registry().Create("张三", "转发", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var subEvs []contract.SubAgentEvent
	m.Run(context.Background(), s, "派子", nil, func(ev session.Event) {
		if ev.Event == contract.EvSubAgent {
			subEvs = append(subEvs, ev.Data.(contract.SubAgentEvent))
		}
	})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("转发档不得影响收口，终态 %s", s.StateOf())
	}
	hasCall, hasResult, hasText := false, false, false
	for _, e := range subEvs {
		if e.Kind == "tool_call" && e.Tool == "read_tool" {
			hasCall = true
		}
		if e.Kind == "tool_result" && e.Tool == "read_tool" && e.OK {
			hasResult = true // 契约语义回填：工具名（非 callID）+ 成败判定
		}
		if e.Kind == "text" && strings.Contains(e.Text, "子完成") {
			hasText = true
		}
	}
	if !hasCall || !hasResult || !hasText {
		t.Fatalf("转发档应含子工具调用/结果（工具名+成败）与结论文本（只读流），实得 %+v", subEvs)
	}
	for _, msg := range s.CloneHistory() {
		if msg.Role == schema.Assistant && strings.Contains(msg.Content, "子完成") {
			t.Fatal("子结论文本不得成为父 assistant 消息（只读流不入史）")
		}
	}
}

// recGenModel 记录输入、恒回固定文本的子/摘要模型（Generate 路径专用——
// scriptedModel.Generate 不记录，避免 genTitle 调用混入计数）。
type recGenModel struct {
	reply  string
	inputs [][]*schema.Message
}

func (r *recGenModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	r.inputs = append(r.inputs, append([]*schema.Message(nil), input...))
	return schema.AssistantMessage(r.reply, nil), nil
}

func (r *recGenModel) Stream(ctx context.Context, in []*schema.Message, o ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := r.Generate(ctx, in, o...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()
	return sr, nil
}

// toolCallOnceModel 首调发起工具调用、后续恒文本的子模型（父模型仍用
// scriptedModel——工厂按模型键分叉：spawn 子模型键与父相同，需按调用序分派）。
type toolCallOnceModel struct {
	call string
	done bool
}

func (t *toolCallOnceModel) factory(parent *scriptedModel) llm.ModelFactory {
	n := 0
	return func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 { // 第 2 个构造的模型 = 子代理（assemble：父模型先构造，spawn 随后）
			return t, nil
		}
		return parent, nil
	}
}

func (t *toolCallOnceModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if t.done {
		return schema.AssistantMessage("子完成", nil), nil
	}
	t.done = true
	return schema.AssistantMessage("", []schema.ToolCall{tcOf("sc1", t.call, `{}`)}), nil
}

func (t *toolCallOnceModel) Stream(ctx context.Context, in []*schema.Message, o ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := t.Generate(ctx, in, o...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()
	return sr, nil
}

// --- H4-1 并行 spawn 三测（方案 = findings/2026-08-26-h4-parallel-aggregation-plan.md）---

// probeModel 并发探针子模型：Generate 在途计数 + 驻留窗口，maxCur 记录
// 同时在途峰值（并发重叠断言数据面）；replies 按调用序轮转取文（两次
// InvokableRun 共享一个模型实例——结论各取一条，顺序无关断言）。
type probeModel struct {
	replies []string
	dwell   time.Duration
	seq     atomic.Int32
	cur     atomic.Int32
	maxCur  atomic.Int32
}

func (p *probeModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	n := p.cur.Add(1)
	for {
		old := p.maxCur.Load()
		if n <= old || p.maxCur.CompareAndSwap(old, n) {
			break
		}
	}
	time.Sleep(p.dwell) // 驻留窗口：真并发时两子同窗在途，串行则峰值恒 1
	p.cur.Add(-1)
	idx := int(p.seq.Add(1) - 1)
	return schema.AssistantMessage(p.replies[idx%len(p.replies)], nil), nil
}

func (p *probeModel) Stream(ctx context.Context, in []*schema.Message, o ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := p.Generate(ctx, in, o...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()
	return sr, nil
}

// twoSpawnRound 父首调一轮双 spawn（ToolsNode 并发执行面）。
func twoSpawnRound(fm *scriptedModel, t1, t2 string) {
	fm.onStream = func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "spawn", `{"task":"`+t1+`","tools":"read_tool"}`),
				tcOf("c2", "spawn", `{"task":"`+t2+`","tools":"read_tool"}`),
			}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}
}

// runTwoSpawns 双 spawn 会话跑通（返回父模型/探针/会话）。
func runTwoSpawns(t *testing.T, probe *probeModel, opt func(*Options)) (*scriptedModel, *probeModel, *session.Session) {
	t.Helper()
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{}
	twoSpawnRound(fm, "勘察甲区", "勘察乙区")
	n := 0
	m, _ := newReductionManager(t, 0, []contract.Tool{rt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 { // 构造序：父(1) → 子(2)（genTitle 第 3 次走 fm）
			return probe, nil
		}
		return fm, nil
	}, func(o *Options) {
		o.SubAgents = &SubAgentsConfig{Tools: []string{"read_tool"}}
		if opt != nil {
			opt(o)
		}
	})
	s := m.Registry().Create("张三", "双派", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "并行勘察", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	return fm, probe, s
}

// TestSpawnConcurrentOverlap 一轮双 spawn 并发执行（默认不限流）：两子
// Generate 同窗在途（maxCur=2），双结论均内联父窗口。
func TestSpawnConcurrentOverlap(t *testing.T) {
	probe := &probeModel{replies: []string{"子结论：甲区 3 文件", "子结论：乙区 5 文件"}, dwell: 150 * time.Millisecond}
	fm, p, s := runTwoSpawns(t, probe, nil)
	if p.maxCur.Load() != 2 {
		t.Fatalf("双 spawn 应并发执行（同时在途峰值 2），实得 %d", p.maxCur.Load())
	}
	over := toolMsgOf(fm.inputs[len(fm.inputs)-1])
	for _, want := range []string{"甲区", "乙区"} {
		found := false
		for _, c := range over {
			if strings.Contains(c, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("父窗口应内联双结论（含 %q），实得 %+v", want, over)
		}
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("父应正常收口，终态 %s", s.StateOf())
	}
}

// TestSpawnSemaphoreSerializes MaxConcurrent=1 信号量限流：双 spawn 串行
// （在途峰值恒 1），双结论仍都送达（限流不丢结果）。
func TestSpawnSemaphoreSerializes(t *testing.T) {
	probe := &probeModel{replies: []string{"子结论：甲区完成", "子结论：乙区完成"}, dwell: 150 * time.Millisecond}
	fm, p, s := runTwoSpawns(t, probe, func(o *Options) {
		o.SubAgents.MaxConcurrent = 1
	})
	if p.maxCur.Load() != 1 {
		t.Fatalf("MaxConcurrent=1 应串行执行（在途峰值 1），实得 %d", p.maxCur.Load())
	}
	over := toolMsgOf(fm.inputs[len(fm.inputs)-1])
	nHit := 0
	for _, c := range over {
		if strings.Contains(c, "子结论") {
			nHit++
		}
	}
	if nHit != 2 {
		t.Fatalf("限流下双结论应都送达父窗口，实得 %d 条（%+v）", nHit, over)
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("父应正常收口，终态 %s", s.StateOf())
	}
}

// failOneModel 子剧本模型：一次调用轮转——调用 1 直接文本结论（成功）、
// 调用 2 发起白名单外工具调用（子面无此工具 → 子运行报错 → errFeed）。
type failOneModel struct {
	seq atomic.Int32
}

func (f *failOneModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	if f.seq.Add(1) == 2 {
		return schema.AssistantMessage("", []schema.ToolCall{tcOf("gc1", "ghost_tool", `{}`)}), nil
	}
	return schema.AssistantMessage("子结论：勘察成功", nil), nil
}

func (f *failOneModel) Stream(ctx context.Context, in []*schema.Message, o ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := f.Generate(ctx, in, o...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()
	return sr, nil
}

// TestSpawnPartialFailure 一败一成同一轮：失败者 errFeed JSON、成功者结论
// 照常、父运行不炸正常收口（spawnFailFeed 既有语义的并行回归）。
func TestSpawnPartialFailure(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{}
	twoSpawnRound(fm, "勘察甲区", "尝试越权工具")
	sub := &failOneModel{}
	n := 0
	m, _ := newReductionManager(t, 0, []contract.Tool{rt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 {
			return sub, nil
		}
		return fm, nil
	}, func(o *Options) {
		o.SubAgents = &SubAgentsConfig{Tools: []string{"read_tool"}}
	})
	s := m.Registry().Create("张三", "部分失败", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "并行勘察", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("一败一成不得杀父运行，终态 %s", s.StateOf())
	}
	over := toolMsgOf(fm.inputs[len(fm.inputs)-1])
	hasOK, hasFail := false, false
	for _, c := range over {
		if strings.Contains(c, "子结论：勘察成功") {
			hasOK = true
		}
		if strings.Contains(c, "子代理执行失败") {
			hasFail = true
		}
	}
	if !hasOK || !hasFail {
		t.Fatalf("父窗口应同时含成功结论与失败 errFeed（一败一成都显式回传），实得 %+v", over)
	}
}

// TestSpawnDenyToolsValidation DenyTools 装配期硬校验（H9-6 纪律升机制）：
// 白名单 ∩ DenyTools 非空 → configError；DenyTools 含全量面未同名 →
// configError（loud validation——dsh tools.restrict 同语义，防拼写错静默
// 失效）；合法形态（交集空+名字全存在/不配置）行为不变。
func TestSpawnDenyToolsValidation(t *testing.T) {
	mk := func(name string) contract.Tool {
		tt, _ := tools.InferTool(name, "桩", func(context.Context, struct{}) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		})
		return tt
	}
	ts := []contract.Tool{mk("read_tool"), mk("write_tool")}

	if _, err := filterSubTools(ts, []string{"read_tool", "write_tool"}, []string{"write_tool"}); err == nil {
		t.Fatal("白名单与 DenyTools 交集应装配期报错")
	}
	if _, err := filterSubTools(ts, []string{"read_tool"}, []string{"no_such_tool"}); err == nil {
		t.Fatal("DenyTools 含全量面未同名应装配期报错（防拼写错静默失效）")
	}
	got, err := filterSubTools(ts, []string{"read_tool", "write_tool"}, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("不配置 DenyTools 行为不变：err=%v len=%d", err, len(got))
	}
	got, err = filterSubTools(ts, []string{"read_tool"}, []string{"write_tool"})
	if err != nil || len(got) != 1 || got[0].Info().Name != "read_tool" {
		t.Fatalf("合法防线形态（交集空）应照常筛白名单：err=%v len=%d", err, len(got))
	}
	// 经装配链（newTopologySub）同样硬拒——红线机制不是约定是闸门
	sub := &toolCallOnceModel{call: "read_tool"}
	n := 0
	m, _ := newReductionManager(t, 0, ts, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		return sub, nil
	})
	s := m.Registry().Create("张三", "校验", "plan", contract.UserPrefs{Model: "p/m"})
	if _, err := m.newTopologySub(context.Background(), s,
		SubAgentSpec{Tools: []string{"read_tool", "write_tool"}, DenyTools: []string{"write_tool"}}, ts); err == nil {
		t.Fatal("拓扑装配链应同样装配期报错（白名单∩DenyTools）")
	}
}
