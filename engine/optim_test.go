package engine

// 优化方案（findings/2026-08-30-optimization-plan.md）评审修订版的行为回归：
// A1 Resume 入口整备（重复/并发双 Resume 拒绝 + 执行期 running 可见）
// A4 模型能力门控（NoToolCalls 组装期 fail fast）
// A6 Drain 优雅停机收尾
// B1 ContextBudget 常驻面告警（只发一次 / 0=关 / 会话域件计入口径）
// B2 后台子代理 usage 上卷（SpawnID 归组）
// C2 工具边界节流落盘（轮内崩溃不丢已完工具轮）

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/checkpoint"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/llmtest"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// ---- A1：Resume 入口整备 ----

// writeToolOf 记录执行次数并构造 write_tool（审批面：manual 档挂起）。
func writeToolOf(calls *int) contract.Tool {
	wt, _ := tools.InferTool("write_tool", "写桩", func(context.Context, struct{}) (map[string]any, error) {
		if calls != nil {
			*calls++
		}
		return map[string]any{"ok": true}, nil
	})
	return wt
}

// approveItemID 挂起审批卡首项标识（合并决议卡须按 item 槽落决议）。
func approveItemID(t *testing.T, s *session.Session) string {
	t.Helper()
	for _, ev := range s.SnapshotEvents() {
		if ev.Event != contract.EvApprovalRequest {
			continue
		}
		if d, ok := ev.Data.(contract.ApprovalReq); ok {
			if len(d.Items) > 0 {
				return d.Items[0].ItemID
			}
			return ""
		}
	}
	t.Fatal("事件流应含 approval_request")
	return ""
}

// TestResumeDoubleCallRejected 迟到的第二次 Resume：明确报错而非脏重放
// （checkpoint 不随 Resume 消费，放行即加载旧检查点重执行）。
func TestResumeDoubleCallRejected(t *testing.T) {
	var calls int
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "write_tool", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, _ := newRunManager(t, []contract.Tool{writeToolOf(&calls)}, factoryOf(fm))
	s := m.Registry().Create("张三", "审批", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "写一个", nil, func(session.Event) {})
	if s.StateOf() != session.StatePendingApproval {
		t.Fatalf("应挂起审批：%s", s.StateOf())
	}
	endCount := countEvents(s, contract.EvSessionEnd)

	s.SetDecisionFor(approveItemID(t, s), contract.ApprovalDecision{Approve: true})
	done := make(chan struct{})
	go func() { defer close(done); m.Resume(context.Background(), s, func(session.Event) {}) }()
	<-done
	waitTitleFlight(t, s)
	if s.StateOf() != session.StateEnded {
		t.Fatalf("批准续流应正常收束：%s", s.StateOf())
	}
	if calls != 1 {
		t.Fatalf("写工具应恰执行一次：%d", calls)
	}

	// 迟到第二次 Resume：恰一条 error 事件、无新增 session_end（脏重放标志）、状态不被翻动
	var names []string
	m.Resume(context.Background(), s, func(ev session.Event) { names = append(names, ev.Event) })
	if !contains(names, contract.EvError) || len(names) != 1 {
		t.Fatalf("第二次 Resume 应恰一条 error 事件：%v", names)
	}
	if got := countEvents(s, contract.EvSessionEnd); got != endCount+1 {
		t.Fatalf("迟到 Resume 不得产生新收束（脏重放标志）：前 %d 后 %d", endCount, got)
	}
}

// TestResumeConcurrentDoubleRejected 并发双 Resume：BeginResume 单锁原子，
// 恰一个续流成功、另一个明确报错（TOCTOU 窗口闭合）。
func TestResumeConcurrentDoubleRejected(t *testing.T) {
	var calls int
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "write_tool", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, _ := newRunManager(t, []contract.Tool{writeToolOf(&calls)}, factoryOf(fm))
	s := m.Registry().Create("张三", "并发", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "写", nil, func(session.Event) {})
	s.SetDecisionFor(approveItemID(t, s), contract.ApprovalDecision{Approve: true})

	var mu sync.Mutex
	errEvents := 0
	fn := func(ev session.Event) {
		if ev.Event == contract.EvError {
			mu.Lock()
			errEvents++
			mu.Unlock()
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.Resume(context.Background(), s, fn) }()
	}
	wg.Wait()
	waitTitleFlight(t, s)
	if errEvents != 1 {
		t.Fatalf("并发双 Resume 应恰一个报错（另一个成功续流）：%d", errEvents)
	}
	if s.StateOf() != session.StateEnded || calls != 1 {
		t.Fatalf("成功侧应恰一次执行并收束：state=%s calls=%d", s.StateOf(), calls)
	}
}

// TestResumeRunningStateVisible 续流执行期状态可见为 running 且挂 runDone
// （此前恒显 pending：FlushQueue 误报不可打断、Drain 枚举漏执行体）。
func TestResumeRunningStateVisible(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "write_tool", `{}`)}})
			return
		}
		entered <- struct{}{} // 续流第 2 次模型调用入场
		<-release
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	var calls int
	m, _ := newRunManager(t, []contract.Tool{writeToolOf(&calls)}, factoryOf(fm))
	s := m.Registry().Create("张三", "可见", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "写", nil, func(session.Event) {})
	s.SetDecisionFor(approveItemID(t, s), contract.ApprovalDecision{Approve: true})

	resumeDone := make(chan struct{})
	go func() { defer close(resumeDone); m.Resume(context.Background(), s, func(session.Event) {}) }()
	<-entered
	if s.StateOf() != session.StateRunning {
		t.Fatalf("续流执行期应可见 running：%s", s.StateOf())
	}
	if s.RunDone() == nil {
		t.Fatal("续流执行期应挂 runDone（Drain/FlushQueue 寻址锚）")
	}
	close(release)
	<-resumeDone
	waitTitleFlight(t, s)
	if s.StateOf() != session.StateEnded {
		t.Fatalf("收束态：%s", s.StateOf())
	}
}

// countEvents 事件流计数（按事件名）。
func countEvents(s *session.Session, name string) int {
	n := 0
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == name {
			n++
		}
	}
	return n
}

// budgetNotes 预算告警 note 计数（Kind=budget）。
func budgetNotes(s *session.Session) int {
	n := 0
	for _, ev := range s.SnapshotEvents() {
		if ev.Event != contract.EvHarnessNote {
			continue
		}
		if d, ok := ev.Data.(contract.HarnessNote); ok && d.Kind == "budget" {
			n++
		}
	}
	return n
}

// ---- A4：模型能力门控 ----

// TestNoToolCallsGate 模型标记不支持函数调用 + 工具面非空 → Run 首轮前 CONFIG
// 错误（不等运行期端点方言报错）；不标记零变化。
func TestNoToolCallsGate(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := session.NewRegistry(st)
	spec := llm.ModelSpec{ID: "m", Input: []string{"text"}, Priority: 100, NoToolCalls: true}
	m, err := NewManager(reg, Options{
		Providers:    func() []llm.ProviderSpec { return []llm.ProviderSpec{{ID: "p", Kind: "openai", Enabled: true, Models: []llm.ModelSpec{spec}}} },
		Instruction:  func(SessionBrief) string { return "test" },
		Tools:        func(SessionBrief) []contract.Tool { return []contract.Tool{writeToolOf(nil)} },
		NewModel:     factoryOf(&scriptedModel{}),
		CheckPoints:  func(operator, sid string) CheckPointStore { return checkpoint.NewCheckPointStore(st, operator, sid) },
		WorkspaceRoot: func(owner, sid string) string {
			return st.TmpDir() + "/ws/" + owner + "/" + sid
		},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	s := m.Registry().Create("张三", "门控", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var names []string
	m.Run(context.Background(), s, "写", nil, func(ev session.Event) { names = append(names, ev.Event) })
	waitTitleFlight(t, s)
	found := false
	for _, ev := range s.SnapshotEvents() {
		if ev.Event != contract.EvError {
			continue
		}
		if d, ok := ev.Data.(contract.ErrorOut); ok && d.Code == "CONFIG" && strings.Contains(d.Message, "不支持函数调用") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应组装期 CONFIG 拒绝（NoToolCalls + 工具面）：%v", names)
	}
}

// ---- A6：Drain ----

// TestDrainCollectsRunning running 态会话被取消并在限期内收尾（runDone 关闭）。
func TestDrainCollectsRunning(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := session.NewRegistry(st)
	s1 := reg.Create("u", "a", "auto", contract.UserPrefs{Model: "p/m"})
	if !s1.BeginRun("") {
		t.Fatal("抢占执行体")
	}
	s1.SetCancel(func() { // 模拟执行体响应取消：延迟收尾
		go func() {
			time.Sleep(30 * time.Millisecond)
			s1.RunFinished()
		}()
	})
	if left := reg.Drain(2 * time.Second); len(left) != 0 {
		t.Fatalf("应全部收尾：%v", left)
	}
}

// TestDrainStuckReportsSid 不响应取消的执行体：到点如实上报 SID 不拖死停机。
func TestDrainStuckReportsSid(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := session.NewRegistry(st)
	s1 := reg.Create("u", "a", "auto", contract.UserPrefs{Model: "p/m"})
	s1.BeginRun("")
	s1.SetCancel(func() {}) // 失控执行体：不响应取消
	left := reg.Drain(300 * time.Millisecond)
	if len(left) != 1 || left[0] != s1.SID {
		t.Fatalf("应如实上报未收尾会话：%v", left)
	}
	s1.RunFinished() // 测试收尾
}

// ---- B1：ContextBudget ----

// TestContextBudgetWarnOnce 超限恰一张 budget note；第二轮 Run 不重发。
func TestContextBudgetWarnOnce(t *testing.T) {
	m, _ := newReductionManager(t, 0, nil, factoryOf(&scriptedModel{}), func(o *Options) {
		o.ContextBudget = 20 // instruction 1 + 会话域件数百 → 必超
	})
	s := m.Registry().Create("张三", "预算", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "跑", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if n := budgetNotes(s); n != 1 {
		t.Fatalf("超限应恰一张 budget note：%d", n)
	}
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "再跑", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if n := budgetNotes(s); n != 1 {
		t.Fatalf("会话内只发一次（第二轮不得重发）：%d", n)
	}
}

// TestContextBudgetOffByDefault 0 = 缺省关（nil 纪律：零配置零变化）。
func TestContextBudgetOffByDefault(t *testing.T) {
	m, _ := newReductionManager(t, 0, nil, factoryOf(&scriptedModel{}))
	s := m.Registry().Create("张三", "零配", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "跑", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if n := budgetNotes(s); n != 0 {
		t.Fatalf("0 = 缺省关零事件：%d", n)
	}
}

// TestEstimateIncludesSessionTools 口径补齐回归：零业务面 + 预算只容纳
// instruction（1 token）+40——若会话域件未计入则恒不超标；计入则告警。
func TestEstimateIncludesSessionTools(t *testing.T) {
	m, _ := newReductionManager(t, 0, nil, factoryOf(&scriptedModel{}), func(o *Options) {
		o.ContextBudget = 40
	})
	s := m.Registry().Create("张三", "口径", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "跑", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if n := budgetNotes(s); n != 1 {
		t.Fatalf("会话域件应计入常驻面（缺口 3 回归锚）：%d", n)
	}
}

// ---- B2：后台子代理 usage 上卷 ----

// TestBackgroundSpawnUsageRollup 后台子代理的模型用量以带 SpawnID 的 usage
// 事件上卷父会话事件流（同步转发无调用标识不上卷——主面口径不污染）。
func TestBackgroundSpawnUsageRollup(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "spawn", `{"task":"后台勘察","background":true}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "已派出"})
	}}
	sub := llmtest.New(llmtest.Turn{Text: "子结论", Usage: &llmtest.Usage{Prompt: 120, Completion: 30, Total: 150}})
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
	s := m.Registry().Create("张三", "上卷", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "派后台", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if subUsageOf(s) != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id := subUsageOf(s); id == "" {
		t.Fatal("后台子代理 usage 应带 SpawnID 上卷父事件流")
	}
}

// subUsageOf 首个带 SpawnID 的 usage 事件（空 = 无）。
func subUsageOf(s *session.Session) string {
	for _, ev := range s.SnapshotEvents() {
		if ev.Event != contract.EvUsage {
			continue
		}
		if d, ok := ev.Data.(contract.UsageOut); ok && d.SpawnID != "" {
			return d.SpawnID
		}
	}
	return ""
}

// ---- C2：工具边界节流落盘 ----

// countStore 计 session.json 写入次数的测试存储（C2 节流落盘回归用）。
type countStore struct {
	*tstore.Store
	mu     sync.Mutex
	writes int
}

func (c *countStore) WriteUserTreeFile(op, rel string, data []byte) error {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return c.Store.WriteUserTreeFile(op, rel, data)
}

func (c *countStore) sessionWrites() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

// TestToolBoundaryPersist 轮内含工具调用的会话：session.json 写入次数 >
// 无工具调用轮（工具边界节流落盘——崩溃不丢已完工具轮）。
func TestToolBoundaryPersist(t *testing.T) {
	toolCaller := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "read_tool", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	run := func(fm *scriptedModel, ts []contract.Tool) int {
		st := &countStore{Store: tstore.New(t.TempDir())}
		reg := session.NewRegistry(st)
		m, err := NewManager(reg, Options{
			Providers: func() []llm.ProviderSpec {
				return []llm.ProviderSpec{{ID: "p", Kind: "openai", Enabled: true,
					Models: []llm.ModelSpec{{ID: "m", Input: []string{"text"}, Priority: 100}}}}
			},
			Instruction: func(SessionBrief) string { return "test" },
			Tools:       func(SessionBrief) []contract.Tool { return ts },
			NewModel:    factoryOf(fm),
			CheckPoints: func(operator, sid string) CheckPointStore { return checkpoint.NewCheckPointStore(st.Store, operator, sid) },
			WorkspaceRoot: func(owner, sid string) string {
				return st.TmpDir() + "/ws/" + owner + "/" + sid
			},
		})
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		s := m.Registry().Create("张三", "落盘", "auto", contract.UserPrefs{Model: "p/m"})
		s.SetState(session.StateRunning)
		m.Run(context.Background(), s, "跑", nil, func(session.Event) {})
		waitTitleFlight(t, s)
		return st.sessionWrites()
	}
	withTool := run(toolCaller, []contract.Tool{rt})
	without := run(&scriptedModel{}, nil)
	if withTool <= without {
		t.Fatalf("工具轮应触发边界节流落盘（写入数应多于纯文本轮）：%d vs %d", withTool, without)
	}
}
