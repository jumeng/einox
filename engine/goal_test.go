package engine

// C4 goal 域模型工具面回归（设计件 findings/2026-09-21-goal-domain-design.md）：
// Options.Goal 装配（nil 零变化）、direct-human 权威（create/edit/pause/resume/
// complete/blocked）、resume-paused 硬拒（模型不能解除暂停——dsh 窗口修复核心
// GOAL_TOOL_RESUME_PAUSED 对位）、参数互斥表、运行中用户补充置位权威。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// goalCallModel 按序发工具调用的假模型：calls 逐条消费（每轮一次调用），
// 耗尽后纯文本收束。记录全部输入（tool 结果回喂面 = 断言数据源）。
type goalCallModel struct {
	mu     sync.Mutex
	calls  []schema.ToolCall
	inputs [][]*schema.Message
	gate   chan struct{} // 非空时首次调用阻塞至 close（运行中 steering 用例的同步面）
}

// PushCall 追加剧本（轮 1 服务端产物回填真实 goal_id 后再补 update 剧本——
// 预录 JSON 无法预知随机 hex id）。
func (f *goalCallModel) PushCall(c schema.ToolCall) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
}

func (f *goalCallModel) inputsOf() [][]*schema.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]*schema.Message(nil), f.inputs...)
}

func (f *goalCallModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, append([]*schema.Message(nil), input...))
	var call *schema.ToolCall
	if len(f.calls) > 0 {
		call = &f.calls[0]
		f.calls = f.calls[1:]
	}
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate // 首次调用驻留：测试在驻留窗口内入队 steering
	}
	if call != nil {
		return schema.AssistantMessage("", []schema.ToolCall{*call}), nil
	}
	return schema.AssistantMessage("收束", nil), nil
}

func (f *goalCallModel) Stream(ctx context.Context, in []*schema.Message, o ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := f.Generate(ctx, in, o...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()
	return sr, nil
}

// gcOf 工具调用便捷构造。
func gcOf(name, args string) schema.ToolCall { return tcOf("g1", name, args) }

// newGoalManager 装配 goal 工具面的测试引擎。
func newGoalManager(t *testing.T, fm llm.ModelFactory) *Manager {
	t.Helper()
	return newTestManager(t, func(o *Options) {
		o.NewModel = fm
		o.Goal = &GoalConfig{}
	})
}

// runGoal 起一轮便捷面（回 running 态对齐 BeginRun 语义；join 标题在途写）。
func runGoal(t *testing.T, m *Manager, s *session.Session, text string) {
	t.Helper()
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, text, nil, func(session.Event) {})
	waitTitleFlight(t, s)
}

// lastToolResult 末次输入中最后一条 tool 结果文本。
func lastToolResult(f *goalCallModel) string {
	ins := f.inputsOf()
	for i := len(ins) - 1; i >= 0; i-- {
		for j := len(ins[i]) - 1; j >= 0; j-- {
			if ins[i][j].Role == schema.Tool {
				return ins[i][j].Content
			}
		}
	}
	return ""
}

// TestGoalToolsNotMountedWhenNil Options.Goal nil = 工具不在场（幻觉兜底信封
// ——零变化断言）。
func TestGoalToolsNotMountedWhenNil(t *testing.T) {
	f := &goalCallModel{calls: []schema.ToolCall{gcOf("get_goal", `{}`)}}
	m := newTestManager(t, func(o *Options) {
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return f, nil
		}
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	runGoal(t, m, s, "读目标")
	if res := lastToolResult(f); !strings.Contains(res, "不存在") {
		t.Fatalf("nil 装配应得幻觉兜底信封（零变化）：%s", res)
	}
}

// TestGoalCreateAndGet 直接人类轮：create_goal 建立目标、get_goal 双态。
func TestGoalCreateAndGet(t *testing.T) {
	f := &goalCallModel{calls: []schema.ToolCall{
		gcOf("create_goal", `{"objective":"完成重构","max_goal_rounds":5}`),
		gcOf("get_goal", `{}`),
	}}
	var cur model.BaseModel[*schema.Message] = f
	m := newGoalManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return cur, nil
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	runGoal(t, m, s, "建个目标")
	g := s.GoalOf()
	if g == nil || g.Objective != "完成重构" || g.Phase != contract.GoalActive || g.MaxGoalRounds != 5 {
		t.Fatalf("目标应建立：%+v", g)
	}
	res := lastToolResult(f)
	if !strings.Contains(res, `"ok":true`) || !strings.Contains(res, "完成重构") || !strings.Contains(res, "5") {
		t.Fatalf("get_goal 应返回目标态：%s", res)
	}
	// 目标缺省轮数：未带 max_goal_rounds → 缺省 256
	f2 := &goalCallModel{calls: []schema.ToolCall{gcOf("create_goal", `{"objective":"省缺省"}`)}}
	cur = f2
	s2 := m.Registry().Create("李四", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	runGoal(t, m, s2, "建目标")
	if g2 := s2.GoalOf(); g2 == nil || g2.MaxGoalRounds != 256 {
		t.Fatalf("缺省轮数应 256：%+v", g2)
	}
	// get_goal 无目标态：{goal:null}
	f3 := &goalCallModel{calls: []schema.ToolCall{gcOf("get_goal", `{}`)}}
	cur = f3
	s3 := m.Registry().Create("王五", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	runGoal(t, m, s3, "读目标")
	if res := lastToolResult(f3); !strings.Contains(res, `"goal":null`) {
		t.Fatalf("无目标态应 {goal:null}：%s", res)
	}
}

// TestGoalAuthorityDirectHuman 非人类轮（空输入系统轮）权威拒绝；notify 轮同拒。
func TestGoalAuthorityDirectHuman(t *testing.T) {
	f := &goalCallModel{calls: []schema.ToolCall{gcOf("create_goal", `{"objective":"偷建"}`)}}
	m := newGoalManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return f, nil
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	runGoal(t, m, s, "") // 空输入起轮：无 direct-human
	if g := s.GoalOf(); g != nil {
		t.Fatalf("非人类轮不应建目标：%+v", g)
	}
	if res := lastToolResult(f); !strings.Contains(res, "用户") {
		t.Fatalf("应得权威拒绝信封（需用户直接指令）：%s", res)
	}

	// notify 注入轮：系统通知非人类输入（后台完成自续形态——排队 notify + 空输入）
	f2 := &goalCallModel{calls: []schema.ToolCall{gcOf("update_goal", `{"goal_id":"g","revision":1,"action":"complete"}`)}}
	m2 := newGoalManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return f2, nil
	})
	s2 := m2.Registry().Create("李四", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	if !s2.BeginRun("manual") {
		t.Fatal("抢占失败")
	}
	if began, _ := s2.ContinueOrNotify("后台子代理已完成", false); began {
		t.Fatal("running 态应只入队不自续")
	}
	m2.Run(context.Background(), s2, "", nil, func(session.Event) {})
	waitTitleFlight(t, s2)
	if res := lastToolResult(f2); !strings.Contains(res, "用户") {
		t.Fatalf("notify 轮 complete 应权威拒绝：%s", res)
	}

	// 排队用户补充轮：获权威（用户排队消息 = direct-human）
	f3 := &goalCallModel{calls: []schema.ToolCall{gcOf("create_goal", `{"objective":"排队建"}`)}}
	m3 := newGoalManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return f3, nil
	})
	s3 := m3.Registry().Create("王五", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	if !s3.BeginRun("manual") {
		t.Fatal("抢占失败")
	}
	s3.Steer("帮我建目标：排队建", nil, "")
	s3.SetState(session.StateEnded) // 让下一轮可起
	runGoal(t, m3, s3, "")
	if g3 := s3.GoalOf(); g3 == nil || g3.Objective != "排队建" {
		t.Fatalf("排队用户补充轮应获权威：%+v", g3)
	}
}

// TestGoalResumePausedHardReject 硬拒核心：模型不能解除暂停——direct-human
// 在场也拒（暂停只能由用户经应用面 ResumeGoal 解除）。
func TestGoalResumePausedHardReject(t *testing.T) {
	f := &goalCallModel{}
	m := newGoalManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return f, nil
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	g, err := s.CreateGoal("目标", 5) // 经 Session 面建（人类通道预置）
	if err != nil {
		t.Fatal(err)
	}
	s.PauseGoal(g.ID, g.Revision)
	f.calls = []schema.ToolCall{
		gcOf("update_goal", `{"goal_id":"`+g.ID+`","revision":2,"action":"resume"}`),
	}
	runGoal(t, m, s, "继续推进目标") // 直接人类轮
	if cur := s.GoalOf(); cur.Phase != contract.GoalPaused {
		t.Fatalf("模型 resume 不应生效：%+v", cur)
	}
	if res := lastToolResult(f); !strings.Contains(res, "暂停") {
		t.Fatalf("应得 resume-paused 硬拒信封：%s", res)
	}
	// 人类通道解除：应用面 ResumeGoal
	if _, err := s.ResumeGoal(g.ID, 2); err != nil {
		t.Fatalf("人类通道 resume 应过：%v", err)
	}
}

// TestGoalUpdateMutualExclusion 参数互斥表 + CAS 拒绝信封回喂。
func TestGoalUpdateMutualExclusion(t *testing.T) {
	f := &goalCallModel{}
	var cur model.BaseModel[*schema.Message] = f
	m := newGoalManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return cur, nil
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	g, _ := s.CreateGoal("目标", 5)

	cases := []struct {
		name string
		args string
		want string
	}{
		{"edit 零替换栏", `{"goal_id":"` + g.ID + `","revision":1,"action":"edit"}`, "至少"},
		{"pause 带替换栏", `{"goal_id":"` + g.ID + `","revision":1,"action":"pause","objective":"x"}`, "edit"},
		{"resume 带 blocked_reason", `{"goal_id":"` + g.ID + `","revision":1,"action":"resume","blocked_reason":"x"}`, "blocked"},
		{"complete 带 blocked_reason", `{"goal_id":"` + g.ID + `","revision":1,"action":"complete","blocked_reason":"x"}`, "blocked"},
		{"blocked 缺 reason", `{"goal_id":"` + g.ID + `","revision":1,"action":"blocked"}`, "blocked_reason"},
		{"陈旧 revision", `{"goal_id":"` + g.ID + `","revision":99,"action":"pause"}`, "revision"},
		{"未知 action", `{"goal_id":"` + g.ID + `","revision":1,"action":"fly"}`, "action"},
	}
	for _, tc := range cases {
		f.calls = []schema.ToolCall{gcOf("update_goal", tc.args)}
		s.SetState(session.StateRunning)
		m.Run(context.Background(), s, "用户指令", nil, func(session.Event) {})
		waitTitleFlight(t, s)
		if res := lastToolResult(f); !strings.Contains(res, tc.want) || !strings.Contains(res, `"ok":false`) {
			t.Fatalf("%s 应拒绝（含 %q）：%s", tc.name, tc.want, res)
		}
	}
	if cur2 := s.GoalOf(); cur2.Revision != 1 || cur2.Phase != contract.GoalActive {
		t.Fatalf("全部拒绝不应有副作用：%+v", cur2)
	}

	// 合法路径：人类轮内 pause 生效
	f.calls = []schema.ToolCall{
		gcOf("update_goal", `{"goal_id":"`+g.ID+`","revision":1,"action":"pause"}`),
	}
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "暂停目标", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if cur2 := s.GoalOf(); cur2.Phase != contract.GoalPaused {
		t.Fatalf("pause 应生效：%+v", cur2)
	}
	// blocked 从 paused 应拒（域转移表：block 仅 active）
	f.calls = []schema.ToolCall{
		gcOf("update_goal", `{"goal_id":"`+g.ID+`","revision":2,"action":"blocked","blocked_reason":"卡了"}`),
	}
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "报阻塞", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if res := lastToolResult(f); !strings.Contains(res, `"ok":false`) {
		t.Fatalf("paused 报 blocked 应拒（转移表）：%s", res)
	}
}

// TestGoalSteeringSetsHuman 运行中用户补充 = 本轮获得 direct-human（steering
// 注入点置位——dsh open-turn 扫描的等价面）：空输入起轮首调用驻留，驻留窗内
// 入队用户补充，次调用注入并置位后 create_goal 应过权威。
func TestGoalSteeringSetsHuman(t *testing.T) {
	gate := make(chan struct{})
	f := &goalCallModel{
		calls: []schema.ToolCall{ // 调用 1：get_goal（读面无权威要求）；调用 2：create_goal
			gcOf("get_goal", `{}`),
			gcOf("create_goal", `{"objective":"中途建"}`),
		},
		gate: gate,
	}
	m := newGoalManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return f, nil
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	if !s.BeginRun("manual") {
		t.Fatal("抢占失败")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(context.Background(), s, "", nil, func(session.Event) {})
	}()
	// 首调用驻留窗内入队用户补充（排队 → 调用 2 的 BeforeModelRewriteState 注入）
	if !s.Steer("建个目标：中途建", nil, "") {
		t.Fatal("运行中入队应成功")
	}
	close(gate)
	<-done
	waitTitleFlight(t, s)
	if g := s.GoalOf(); g == nil || g.Objective != "中途建" {
		t.Fatalf("运行中用户补充后应获 direct-human 权威：%+v", g)
	}
}

// TestGoalToolsExcludedFromSubagentFace 子代理面结构性排除守卫：goal 工具不进
// spawn 白名单源（ts）——子任务声明/调用 get_goal 得幻觉兜底信封，不触父目标。
func TestGoalToolsExcludedFromSubagentFace(t *testing.T) {
	parent := &goalCallModel{calls: []schema.ToolCall{gcOf("spawn", `{"task":"读当前目标","tools":"get_goal"}`)}}
	child := &goalCallModel{calls: []schema.ToolCall{gcOf("get_goal", `{}`)}}
	n := 0
	m := newGoalManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 { // 第 2 个构造 = 子代理模型（assemble：父先 spawn 后）
			return child, nil
		}
		return parent, nil
	})
	m.Opt.SubAgents = &SubAgentsConfig{Tools: []string{"get_goal", "todo_write"}}
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	if _, err := s.CreateGoal("父目标", 5); err != nil { // 预置目标：子不可读即证隔离
		t.Fatal(err)
	}
	runGoal(t, m, s, "派子任务读目标")
	if res := lastToolResult(child); !strings.Contains(res, "不存在") {
		t.Fatalf("子代理声明 get_goal 应得幻觉兜底信封（结构性不可见）：%s", res)
	}
}

// ---- GoalDriver 自动续行（设计件 findings/2026-09-22-goal-driver-design.md）----

// newGoalDriverManager AutoContinue 开启的测试引擎。
func newGoalDriverManager(t *testing.T, fm llm.ModelFactory) *Manager {
	t.Helper()
	return newTestManager(t, func(o *Options) {
		o.NewModel = fm
		o.Goal = &GoalConfig{AutoContinue: true}
	})
}

// ---- GoalDriver 自动续行（设计件 findings/2026-09-22-goal-driver-design.md）----

// goalRoundInputsOf goal 轮注入计数（假模型 inputs 记录每次 Generate——一轮
// ReAct 含工具循环多次调用，轮数锚不可用；goal 轮输入含「[goal 轮」前缀
// user 消息可辨；标题生成输入不含该形态天然排除）。
func goalRoundInputsOf(f *goalCallModel) int {
	n := 0
	for _, in := range f.inputsOf() {
		for _, msg := range in {
			if msg.Role == schema.User && strings.Contains(msg.Content, "[goal 轮 ") {
				n++
				break
			}
		}
	}
	return n
}

// waitGoalStable 等 goal 链静默：goal 轮注入达 want 且 200ms 无增量（链停在
// complete/blocked/pause 后不再注入）。
func waitGoalStable(t *testing.T, f *goalCallModel, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if goalRoundInputsOf(f) >= want {
			time.Sleep(200 * time.Millisecond)
			if goalRoundInputsOf(f) == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goal 链未静默于 %d 个 goal 轮（当前 %d）", want, goalRoundInputsOf(f))
}

// waitGoalBlocked 等 phase=blocked（round-limit 终态锚）。
func waitGoalBlocked(t *testing.T, s *session.Session) *contract.Goal {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if g := s.GoalOf(); g != nil && g.Phase == contract.GoalBlocked {
			return g
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goal 未转 blocked：%+v", s.GoalOf())
	return nil
}

// TestGoalAutoContinueChain 全链：用户轮建目标 → 自然收束 → goal 轮自动起
// （引擎注入非 human 轮）→ goal 轮内 complete 放行（第二权威源）→ 链停。
func TestGoalAutoContinueChain(t *testing.T) {
	f := &goalCallModel{calls: []schema.ToolCall{
		gcOf("create_goal", `{"objective":"完成重构","max_goal_rounds":5}`),
	}}
	var cur model.BaseModel[*schema.Message] = f
	m := newGoalDriverManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return cur, nil
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	runGoal(t, m, s, "建个目标") // 轮 1：用户直接输入（direct-human）
	g := s.GoalOf()
	if g == nil {
		t.Fatal("目标应建立")
	}
	// 真实 goal_id 回填：goal 轮内 complete 剧本（revision=1——轮预约不动 Revision）
	f.PushCall(gcOf("update_goal", fmt.Sprintf(`{"goal_id":%q,"revision":1,"action":"complete"}`, g.ID)))
	// 终态锚（PushCall 与异步 goal 轮起跑存在调度竞态——注入轮次不定，
	// complete 落地后 goalDrive 恒不续，链必停于此相位）
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if g := s.GoalOf(); g != nil && g.Phase == contract.GoalComplete {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	end := s.GoalOf()
	if end == nil || end.Phase != contract.GoalComplete {
		t.Fatalf("goal 轮内应 complete（第二权威源）：%+v", end)
	}
	if end.RoundsStarted < 1 {
		t.Fatalf("goal 轮应推进计数：%+v", end)
	}
	// 注入文案含轮位与目标文本
	found := false
	for _, in := range f.inputsOf() {
		for _, msg := range in {
			if msg.Role == schema.User && strings.Contains(msg.Content, "[goal 轮 ") && strings.Contains(msg.Content, "完成重构") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("goal 轮文案应注入自续轮输入")
	}
	// complete 终态静默窗：不再有新 goal 轮
	n := goalRoundInputsOf(f)
	time.Sleep(300 * time.Millisecond)
	if goalRoundInputsOf(f) != n {
		t.Fatalf("complete 后链应停（%d → %d）", n, goalRoundInputsOf(f))
	}
}

// TestGoalRoundLimit 预算耗尽：单轮预算目标 goal 轮纯文本收束 → 第二次驱动
// 原子转 blocked（round-limit）链停。
func TestGoalRoundLimit(t *testing.T) {
	f := &goalCallModel{calls: []schema.ToolCall{
		gcOf("create_goal", `{"objective":"一轮预算","max_goal_rounds":1}`),
	}}
	var cur model.BaseModel[*schema.Message] = f
	m := newGoalDriverManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return cur, nil
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	runGoal(t, m, s, "建目标")
	waitGoalStable(t, f, 1) // goal 轮 1（预算内）
	g := waitGoalBlocked(t, s)
	if g.BlockedCode != "round-limit" {
		t.Fatalf("应为 round-limit：%+v", g)
	}
	if goalRoundInputsOf(f) != 1 {
		t.Fatalf("预算 1 只应一个 goal 轮（%d）", goalRoundInputsOf(f))
	}
}

// TestGoalPausedNoContinue 暂停即停：用户轮建目标 → 收束注入 goal 轮（链起）
// → 应用通道域直调 pause（与 goal 轮执行并发——轮预约不动 Revision，CAS 恒
// 可过）→ pause 落地后驱动器不再注入（收束后自然停，无栅栏）。
func TestGoalPausedNoContinue(t *testing.T) {
	f := &goalCallModel{calls: []schema.ToolCall{
		gcOf("create_goal", `{"objective":"暂停目标","max_goal_rounds":5}`),
	}}
	var cur model.BaseModel[*schema.Message] = f
	m := newGoalDriverManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return cur, nil
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	runGoal(t, m, s, "建目标")
	g := s.GoalOf()
	if g == nil {
		t.Fatal("目标应建立")
	}
	// 应用通道 pause（真实用户语义：/goal 命令接 Session 公开方法）
	if _, err := s.PauseGoal(g.ID, g.Revision); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// pause 落地后静默：无新 goal 轮注入（链停在当前轮收束）
	time.Sleep(300 * time.Millisecond)
	if cur := s.GoalOf(); cur == nil || cur.Phase != contract.GoalPaused {
		t.Fatalf("应 paused：%+v", cur)
	}
	n := goalRoundInputsOf(f)
	time.Sleep(300 * time.Millisecond)
	if goalRoundInputsOf(f) != n {
		t.Fatalf("paused 后不应再注入（%d → %d）", n, goalRoundInputsOf(f))
	}
}

// TestGoalRoundAuthorityScope goal 轮权威只放宽 complete/blocked：goal 轮内
// pause 仍需 direct-human（拒）——链经预算耗尽终止。
func TestGoalRoundAuthorityScope(t *testing.T) {
	f := &goalCallModel{calls: []schema.ToolCall{
		gcOf("create_goal", `{"objective":"两轮预算","max_goal_rounds":2}`),
	}}
	var cur model.BaseModel[*schema.Message] = f
	m := newGoalDriverManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return cur, nil
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	runGoal(t, m, s, "建目标")
	g := s.GoalOf()
	if g == nil {
		t.Fatal("目标应建立")
	}
	f.PushCall(gcOf("update_goal", fmt.Sprintf(`{"goal_id":%q,"revision":1,"action":"pause"}`, g.ID))) // goal 轮内应拒
	bg := waitGoalBlocked(t, s)                                                                        // 预算耗尽终态（goal 轮内 pause 拒后纯文本收束——链续至耗尽）
	if bg.BlockedCode != "round-limit" {
		t.Fatalf("预算耗尽应 blocked：%+v", bg)
	}
	// goal 轮内 pause 拒（需 direct-human——第二权威源不覆盖 pause）
	if res := lastToolResult(f); !strings.Contains(res, "需用户直接指令") {
		t.Fatalf("goal 轮内 pause 应拒：%s", res)
	}
}

// TestGoalNoContinueWhenDisabled AutoContinue=false：建目标自然收束不自续
// （零变化——既有默认面）。
func TestGoalNoContinueWhenDisabled(t *testing.T) {
	f := &goalCallModel{calls: []schema.ToolCall{
		gcOf("create_goal", `{"objective":"不自动","max_goal_rounds":5}`),
	}}
	var cur model.BaseModel[*schema.Message] = f
	m := newGoalManager(t, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return cur, nil
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	runGoal(t, m, s, "建目标")
	time.Sleep(300 * time.Millisecond)
	if n := goalRoundInputsOf(f); n != 0 {
		t.Fatalf("AutoContinue=false 不应自续（%d 个 goal 轮）", n)
	}
}
