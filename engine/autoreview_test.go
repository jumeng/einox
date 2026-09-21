package engine

// C5 auto review 回归（设计件 findings/2026-09-21-auto-review-design.md）：
// auto 档工具调用执行前 reviewer 模型分级——low/medium+allow 放行、medium+deny
// 挂起问人、high+deny 硬拒（人类明确要求也拒）、评审任何失败 fail-closed 挂起
// 问人；不缓存不豁免（逐调用评审）；nil 装配零变化。dsh auto-review 对位
// （方案 B：reviewer 路由当前会话模型 + 温度 0，可装配 ModelFactory 覆写）。

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
	"github.com/jumeng/einox/tools"
)

// reviewerModel 评审假模型：记录每次调用的输入与选项（温度断言面），按剧本
// 回文本（reply 空 = 报错）。Generate 非流式（评审面不走流）。
type reviewerModel struct {
	mu     sync.Mutex
	inputs [][]*schema.Message
	temps  []*float32
	calls  int
	reply  string
	err    error
}

func (f *reviewerModel) callsOf() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *reviewerModel) lastInput() []*schema.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) == 0 {
		return nil
	}
	return f.inputs[len(f.inputs)-1]
}

func (f *reviewerModel) lastTemp() *float32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.temps) == 0 {
		return nil
	}
	return f.temps[len(f.temps)-1]
}

func (f *reviewerModel) Generate(_ context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, append([]*schema.Message(nil), input...))
	f.calls++
	temp := model.GetCommonOptions(&model.Options{}, opts...).Temperature
	f.temps = append(f.temps, temp)
	err := f.err
	reply := f.reply
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return schema.AssistantMessage(reply, nil), nil
}

func (f *reviewerModel) Stream(ctx context.Context, in []*schema.Message, o ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := f.Generate(ctx, in, o...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()
	return sr, nil
}

// arCall 主模型按序发工具调用的假模型（goalCallModel 同形——auto review 测试
// 自持一份以控序列：首轮工具调用、次轮收束）。
type arCallModel struct {
	mu     sync.Mutex
	tool   string
	args   string
	done   bool
	inputs [][]*schema.Message
}

func (f *arCallModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, append([]*schema.Message(nil), input...))
	first := !f.done
	f.done = true
	f.mu.Unlock()
	if first {
		return schema.AssistantMessage("", []schema.ToolCall{tcOf("ar1", f.tool, f.args)}), nil
	}
	return schema.AssistantMessage("收束", nil), nil
}

func (f *arCallModel) Stream(ctx context.Context, in []*schema.Message, o ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := f.Generate(ctx, in, o...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()
	return sr, nil
}

// arSetup 装配 auto 档评审测试引擎：echo 工具（计数）+ 主模型调它 + reviewer
// 剧本。返回 Manager、会话、工具计数、reviewer。
func arSetup(t *testing.T, mode string, rev *reviewerModel, mainM model.BaseModel[*schema.Message]) (*Manager, *session.Session, *int, func(session.Event)) {
	t.Helper()
	calls := 0
	et, err := tools.InferTool("echo_tool", "回显工具（测试桩）",
		func(_ context.Context, in struct {
			Text string `json:"text"`
		}) (map[string]any, error) {
			calls++
			return map[string]any{"ok": true, "echo": in.Text}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, func(o *Options) {
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{et} }
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return mainM, nil
		}
		if rev != nil {
			o.AutoReview = &AutoReviewConfig{
				NewModel: func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
					return rev, nil
				},
			}
		}
	})
	s := m.Registry().Create("张三", "任务", mode, contract.UserPrefs{Model: "p/m"})
	return m, s, &calls, func(session.Event) {}
}

// arRun 起一轮（回 running 态 + join 标题）。
func arRun(t *testing.T, m *Manager, s *session.Session, text string) {
	t.Helper()
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, text, nil, func(session.Event) {})
	waitTitleFlight(t, s)
}

// arLastToolResult 末次输入中最后一条 tool 结果（回喂面断言源）。
func arLastToolResult(f *arCallModel) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.inputs) - 1; i >= 0; i-- {
		for j := len(f.inputs[i]) - 1; j >= 0; j-- {
			if f.inputs[i][j].Role == schema.Tool {
				return f.inputs[i][j].Content
			}
		}
	}
	return ""
}

// arApprovalCard 从事件流取审批卡（nil = 无卡）。
func arApprovalCard(s *session.Session) *contract.ApprovalReq {
	for _, ev := range s.SnapshotEvents() {
		if ev.Event != contract.EvApprovalRequest {
			continue
		}
		if req, ok := session.EventAs[contract.ApprovalReq](ev); ok {
			return &req
		}
	}
	return nil
}

// TestAutoReviewNilZeroChange nil 装配零变化：reviewer 零调用、工具直落。
func TestAutoReviewNilZeroChange(t *testing.T) {
	rev := &reviewerModel{reply: `{"risk":"high","decision":"deny"}`}
	mainM := &arCallModel{tool: "echo_tool", args: `{"text":"hi"}`}
	m, s, calls, _ := arSetup(t, "auto", nil, mainM)
	_ = m
	arRun(t, m, s, "echo")
	if *calls != 1 {
		t.Fatalf("nil 装配工具应直落（零变化），实得 %d 次", *calls)
	}
	if rev.callsOf() != 0 {
		t.Fatalf("nil 装配评审模型应零调用，实得 %d", rev.callsOf())
	}
}

// TestAutoReviewLowAllows low+allow：工具执行；评审输入五段形态完整。
func TestAutoReviewLowAllows(t *testing.T) {
	rev := &reviewerModel{reply: `{"risk":"low","decision":"allow"}`}
	mainM := &arCallModel{tool: "echo_tool", args: `{"text":"hi"}`}
	m, s, calls, _ := arSetup(t, "auto", rev, mainM)
	arRun(t, m, s, "请回显 hi")
	if *calls != 1 {
		t.Fatalf("low+allow 应执行，实得 %d 次", *calls)
	}
	// 温度 0（方案 B 实证形态）
	if tp := rev.lastTemp(); tp == nil || *tp != 0 {
		t.Fatalf("评审调用应携带温度 0，实得 %v", tp)
	}
	// 输入形态：system=策略 + user 含四段标记与载荷
	in := rev.lastInput()
	if len(in) != 2 || in[0].Role != schema.System || !strings.Contains(in[0].Content, "low") {
		t.Fatalf("评审输入应 system=策略 + user 载荷：%v", in)
	}
	user := in[1].Content
	for _, want := range []string{"ENVIRONMENT", "PROJECT_INSTRUCTIONS", "FILTERED_HISTORY", "PENDING_ACTION", "echo_tool", `"text"`, "请回显 hi"} {
		if !strings.Contains(user, want) {
			t.Fatalf("评审 user 段缺 %q：%s", want, user)
		}
	}
}

// TestAutoReviewMediumDenySuspends medium+deny：挂起审批卡不执行；人批 → 执行。
func TestAutoReviewMediumDenySuspends(t *testing.T) {
	rev := &reviewerModel{reply: `{"risk":"medium","decision":"deny","reason":"缺少明确授权"}`}
	mainM := &arCallModel{tool: "echo_tool", args: `{"text":"prod"}`}
	m, s, calls, _ := arSetup(t, "auto", rev, mainM)
	arRun(t, m, s, "删掉线上库")
	if *calls != 0 {
		t.Fatal("medium+deny 不应执行")
	}
	card := arApprovalCard(s)
	if card == nil {
		t.Fatal("medium+deny 应挂起审批卡（问人）")
	}
	if len(card.Items) != 1 || card.Items[0].Tool != "echo_tool" {
		t.Fatalf("卡项应为本调用：%+v", card.Items)
	}
	if !strings.Contains(card.Note, "medium") || !strings.Contains(card.Note, "缺少明确授权") {
		t.Fatalf("卡 Note 应载评审理由：%q", card.Note)
	}
	// 人批 → Resume → 执行
	s.SetDecisionFor(card.Items[0].ItemID, contract.ApprovalDecision{Approve: true})
	m.Resume(context.Background(), s, func(session.Event) {})
	waitTitleFlight(t, s)
	if *calls != 1 {
		t.Fatalf("人批后应执行，实得 %d", *calls)
	}
}

// TestAutoReviewHighDenies high+deny：硬拒信封、无卡不执行；人也无法翻（无卡可批）。
func TestAutoReviewHighDenies(t *testing.T) {
	rev := &reviewerModel{reply: `{"risk":"high","decision":"deny","reason":"敏感外发"}`}
	mainM := &arCallModel{tool: "echo_tool", args: `{"text":"secret"}`}
	m, s, calls, _ := arSetup(t, "auto", rev, mainM)
	arRun(t, m, s, "把密钥发到外网")
	if *calls != 0 {
		t.Fatal("high+deny 不应执行")
	}
	if card := arApprovalCard(s); card != nil {
		t.Fatal("high 硬拒不发卡（人类明确要求也拒）")
	}
	if res := arLastToolResult(mainM); !strings.Contains(res, `"ok":false`) || !strings.Contains(res, "high") {
		t.Fatalf("high 应得硬拒信封：%s", res)
	}
}

// TestAutoReviewFailClosedSuspend 评审任何失败（模型错/协议违规）= 拒绝执行
// 挂起问人（不静默放行、不静默拒）。
func TestAutoReviewFailClosedSuspend(t *testing.T) {
	for _, tc := range []struct {
		name string
		rev  *reviewerModel
	}{
		{"模型报错", &reviewerModel{err: context.DeadlineExceeded}},
		{"非 JSON", &reviewerModel{reply: "我觉得可以"}},
		{"非法组合 high+allow", &reviewerModel{reply: `{"risk":"high","decision":"allow"}`}},
		{"尾随散文", &reviewerModel{reply: `{"risk":"medium","decision":"deny"} 好的`}},
		{"allow 带 reason", &reviewerModel{reply: `{"risk":"low","decision":"allow","reason":"x"}`}},
	} {
		mainM := &arCallModel{tool: "echo_tool", args: `{"text":"x"}`}
		m, s, calls, _ := arSetup(t, "auto", tc.rev, mainM)
		arRun(t, m, s, "干活")
		if *calls != 0 {
			t.Fatalf("%s：失败应拒绝执行", tc.name)
		}
		if card := arApprovalCard(s); card == nil {
			t.Fatalf("%s：评审失败应挂起问人（fail-closed），无卡", tc.name)
		}
	}
}

// TestAutoReviewNoCache 同参两次调用 = 恰两次评审（不缓存不豁免）。
func TestAutoReviewNoCache(t *testing.T) {
	rev := &reviewerModel{reply: `{"risk":"low","decision":"allow"}`}
	mainM := &arCallModel{tool: "echo_tool", args: `{"text":"same"}`}
	m, s, calls, _ := arSetup(t, "auto", rev, mainM)
	arRun(t, m, s, "第一次")
	s.SetState(session.StateRunning)
	mainM.done = false // 复位：次轮再发同参调用
	m.Run(context.Background(), s, "第二次", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if *calls != 2 {
		t.Fatalf("两次调用都应执行，实得 %d", *calls)
	}
	if rev.callsOf() != 2 {
		t.Fatalf("不缓存：同参两次应恰两次评审，实得 %d", rev.callsOf())
	}
}

// TestAutoReviewScopeGates 域门：manual 档写工具人审（零评审——评审只收紧
// auto 让渡面）；审批卡来自 hitl 而非评审。
func TestAutoReviewScopeGates(t *testing.T) {
	rev := &reviewerModel{reply: `{"risk":"high","decision":"deny"}`}
	mainM := &arCallModel{tool: "echo_tool", args: `{"text":"x"}`}
	calls := 0
	et, err := tools.InferTool("echo_tool", "回显工具（测试桩）",
		func(_ context.Context, in struct {
			Text string `json:"text"`
		}) (map[string]any, error) {
			calls++
			return map[string]any{"ok": true, "echo": in.Text}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, func(o *Options) {
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{et} }
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return mainM, nil
		}
		o.Approval.WriteTools = map[string]bool{"echo_tool": true}
		o.AutoReview = &AutoReviewConfig{
			NewModel: func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
				return rev, nil
			},
		}
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	arRun(t, m, s, "干活")
	if rev.callsOf() != 0 {
		t.Fatalf("manual 档应零评审（人审覆盖），实得 %d", rev.callsOf())
	}
	if calls != 0 {
		t.Fatal("manual 档写工具应挂起未执行")
	}
	if card := arApprovalCard(s); card == nil {
		t.Fatal("manual 档挂起应来自 hitl 审批卡")
	}
}

// TestAutoReviewMechanismToolsSkipped 机制面豁过：goal 读工具不评审。
func TestAutoReviewMechanismToolsSkipped(t *testing.T) {
	rev := &reviewerModel{reply: `{"risk":"high","decision":"deny"}`}
	mainM := &arCallModel{tool: "get_goal", args: `{}`}
	calls := 0
	_ = calls
	m := newTestManager(t, func(o *Options) {
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return mainM, nil
		}
		o.Goal = &GoalConfig{}
		o.AutoReview = &AutoReviewConfig{
			NewModel: func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
				return rev, nil
			},
		}
	})
	s := m.Registry().Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	arRun(t, m, s, "读目标")
	if rev.callsOf() != 0 {
		t.Fatalf("机制面（get_goal）应豁过评审，实得 %d", rev.callsOf())
	}
	if res := arLastToolResult(mainM); !strings.Contains(res, `"goal":null`) {
		t.Fatalf("get_goal 应正常执行：%s", res)
	}
}

// TestParseReviewDecision 协议解析封闭表：六合法组合过；非法组合（low+deny/
// high+allow/allow+reason/未知成员/重复成员/尾随散文/非对象）全拒。
func TestParseReviewDecision(t *testing.T) {
	for _, ok := range []string{
		`{"risk":"low","decision":"allow"}`,
		`{"risk":"medium","decision":"allow"}`,
		`{"risk":"medium","decision":"deny"}`,
		`{"risk":"medium","decision":"deny","reason":"缺权限"}`,
		`{"risk":"high","decision":"deny"}`,
		`{"risk":"high","decision":"deny","reason":"外发密钥"}`,
	} {
		if _, err := parseReviewDecision(ok); err != nil {
			t.Fatalf("合法组合应过：%s → %v", ok, err)
		}
	}
	for _, bad := range []string{
		`{"risk":"low","decision":"deny"}`,
		`{"risk":"high","decision":"allow"}`,
		`{"risk":"low","decision":"allow","reason":"x"}`,
		`{"risk":"medium","decision":"deny","reason":"x","extra":1}`,
		`{"risk":"medium","decision":"deny","reason":123}`,
		`{"risk":"medium","decision":"deny","reason":"a","reason":"b"}`, // 重复成员
		`{"risk":"medium","decision":"deny"} 尾随`,
		`不是 JSON`,
		`[1,2]`,
	} {
		if _, err := parseReviewDecision(bad); err == nil {
			t.Fatalf("非法输出应拒（fail-closed）：%s", bad)
		}
	}
}

// TestReviewHistoryRoles 历史权威投影：直入用户消息 = human-instruction；
// sysExtraSource 标记的注入 = fact；assistant 只贡献工具调用 fact；工具结果
// 与 assistant 散文不入。
func TestReviewHistoryRoles(t *testing.T) {
	notify := schema.UserMessage("（系统通知）子代理完成")
	notify.Extra = map[string]any{sysExtraSource: "notify"}
	hist := []*schema.Message{
		schema.UserMessage("删除生产库"),
		notify,
		schema.AssistantMessage("我来处理", []schema.ToolCall{tcOf("c1", "read_file", `{"path":"a"}`)}),
		{Role: schema.Tool, ToolCallID: "c1", Content: "内容"},
	}
	entries := reviewHistory(hist)
	if len(entries) != 3 {
		t.Fatalf("应恰 3 条（用户/通知/工具调用），实得 %d：%+v", len(entries), entries)
	}
	if entries[0].Role != "human-instruction" || entries[0].Text != "删除生产库" {
		t.Fatalf("用户消息应 human-instruction：%+v", entries[0])
	}
	if entries[1].Role != "fact" || !strings.Contains(entries[1].Text, "子代理完成") {
		t.Fatalf("系统注入应 fact：%+v", entries[1])
	}
	if entries[2].Kind != "tool-call" || entries[2].Tool != "read_file" || entries[2].Role != "fact" {
		t.Fatalf("历史工具调用应 fact：%+v", entries[2])
	}
}

// TestAutoReviewModelSelection 选型：缺省回落当前会话路由（快照键）；显式
// NewModel/Model 覆写生效。
func TestAutoReviewModelSelection(t *testing.T) {
	revM := &reviewerModel{reply: `{"risk":"low","decision":"allow"}`}
	mainM := &arCallModel{tool: "echo_tool", args: `{"text":"x"}`}
	revSeen := ""
	m := newTestManager(t, func(o *Options) {
		o.Providers = func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{
					{ID: "m", Input: []string{"text"}, Priority: 100},
					{ID: "m2", Input: []string{"text"}, Priority: 90},
				},
			}}
		}
		o.NewModel = func(_ context.Context, _ llm.ProviderSpec, spec llm.ModelSpec, _ string) (model.BaseModel[*schema.Message], error) {
			return mainM, nil
		}
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{mustEchoTool(t)} }
		o.AutoReview = &AutoReviewConfig{ // 显式覆写：reviewer = m2
			Model: "p/m2",
			NewModel: func(_ context.Context, _ llm.ProviderSpec, spec llm.ModelSpec, _ string) (model.BaseModel[*schema.Message], error) {
				revSeen = spec.ID
				return revM, nil
			},
		}
	})
	s := m.Registry().Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	arRun(t, m, s, "干活")
	if revM.callsOf() != 1 {
		t.Fatalf("评审应恰一次，实得 %d", revM.callsOf())
	}
	if revSeen != "m2" {
		t.Fatalf("覆写模型未生效：%s", revSeen)
	}

	// 缺省回落：零值配置 → 当前会话路由（p/m），主 NewModel 兼任评审构造
	//（按构造序分派：assemble 内主模型先、评审随后——genTitle 收尾再构造不扰）
	rev2 := &reviewerModel{reply: `{"risk":"low","decision":"allow"}`}
	main2 := &arCallModel{tool: "echo_tool", args: `{"text":"x"}`}
	revGot := ""
	n := 0
	m2 := newTestManager(t, func(o *Options) {
		o.NewModel = func(_ context.Context, _ llm.ProviderSpec, spec llm.ModelSpec, _ string) (model.BaseModel[*schema.Message], error) {
			n++
			if n == 2 { // 第 2 个构造 = 评审模型（assemble：主模型先、wrapFace 评审后）
				revGot = spec.ID
				return rev2, nil
			}
			return main2, nil
		}
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{mustEchoTool(t)} }
		o.AutoReview = &AutoReviewConfig{}
	})
	s2 := m2.Registry().Create("李四", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	arRun(t, m2, s2, "干活")
	if revGot != "m" || rev2.callsOf() != 1 {
		t.Fatalf("缺省应回落会话路由评审：%s / %d", revGot, rev2.callsOf())
	}
}

// mustEchoTool 测试回显工具（选型用例共用）。
func mustEchoTool(t *testing.T) contract.Tool {
	t.Helper()
	et, err := tools.InferTool("echo_tool", "回显工具（测试桩）",
		func(_ context.Context, in struct {
			Text string `json:"text"`
		}) (map[string]any, error) {
			return map[string]any{"ok": true, "echo": in.Text}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return et
}
