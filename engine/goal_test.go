package engine

// C4 goal 域模型工具面回归（设计件 findings/2026-09-21-goal-domain-design.md）：
// Options.Goal 装配（nil 零变化）、direct-human 权威（create/edit/pause/resume/
// complete/blocked）、resume-paused 硬拒（模型不能解除暂停——dsh 窗口修复核心
// GOAL_TOOL_RESUME_PAUSED 对位）、参数互斥表、运行中用户补充置位权威。

import (
	"context"
	"strings"
	"sync"
	"testing"

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
