package engine

// H1②③ reduction 接线回归：
//   截断+外置+agent 内取回（模型面 spill/ 指针 → read_file 端到端读回原文）
//   clear 保真锚定（触发后 session 历史原文不变——深拷贝闸）+ 保尾轮（最近 2 工具轮）
//   清出不足闸不动（ClearAtLeastTokens——白破缓存保护）
//   exclude 名单（tool_search 不截断）
//   shape 出站断言（Inputs：在途保留思考 / 已结算剥离——H1④）

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/checkpoint"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// newReductionManager 可配上下文窗口的测试引擎（window=0 = 窗口未知——只截断不清除；
// mut 逐个套用——SubAgents 等增量装配面注入用）。
func newReductionManager(t *testing.T, window int, ts []contract.Tool, fm llm.ModelFactory, mut ...func(*Options)) (*Manager, *tstore.Store) {
	t.Helper()
	st := tstore.New(t.TempDir())
	spec := llm.ModelSpec{ID: "m", Input: []string{"text"}, Priority: 100}
	if window > 0 {
		spec.Limit = &llm.Limit{Context: window, Output: 4096}
	}
	reg := session.NewRegistry(st)
	m := NewManager(reg, Options{
		Providers: func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{ID: "p", Kind: "openai", Enabled: true, Models: []llm.ModelSpec{spec}}}
		},
		Instruction: func(SessionBrief) string { return "test" },
		Tools:       func() []contract.Tool { return ts },
		NewModel:    fm,
		CheckPoints: func(operator, sid string) CheckPointStore {
			return checkpoint.NewCheckPointStore(st, operator, sid)
		},
		WorkspaceRoot: func(owner, sid string) string { return st.TmpDir() + "/ws/" + owner + "/" + sid },
	})
	for _, f := range mut {
		f(&m.Opt)
	}
	return m, st
}

// factoryOf scriptedModel 包成 ModelFactory。
func factoryOf(fm *scriptedModel) llm.ModelFactory {
	return func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	}
}

// tcOf 构造工具调用。
func tcOf(id, name, args string) schema.ToolCall {
	return schema.ToolCall{ID: id, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args}}
}

// toolMsgOf 取输入序列中全部 tool 消息内容。
func toolMsgOf(msgs []*schema.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == schema.Tool {
			out = append(out, m.Content)
		}
	}
	return out
}

// TestReductionTruncOffloadReadback 截断 → 外置（会话持久域）→ agent 内
// read_file 经 spill/ 虚拟路径端到端读回原文；入史 = 截断版+指针（允许例外①）。
func TestReductionTruncOffloadReadback(t *testing.T) {
	big, _ := tools.InferTool("big_tool", "大结果桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"data": strings.Repeat("x", 10000)}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		switch n {
		case 1:
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{tcOf("c1", "big_tool", `{}`)}})
		case 2: // 模型按截断通知的指针取回全文（read_file 已挂 spill/ 路由）
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c2", "read_file", `{"path":"spill/trunc/c1"}`)}})
		default:
			send(&schema.Message{Role: schema.Assistant, Content: "完成"})
		}
	}}
	m, st := newReductionManager(t, 0, []contract.Tool{big}, factoryOf(fm))
	s := m.Registry().Create("张三", "截断", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var evNames []string
	var errMsg string
	m.Run(context.Background(), s, "跑大工具", nil, func(ev session.Event) {
		evNames = append(evNames, ev.Event)
		if ev.Event == contract.EvError {
			errMsg = fmt.Sprintf("%v", ev.Data)
		}
	})
	waitTitleFlight(t, s)

	if len(fm.inputs) != 3 {
		t.Fatalf("模型应调用 3 次，实得 %d；事件流 %v；错误 %s", len(fm.inputs), evNames, errMsg)
	}
	// 第二调：结果已截断换指针（长度骤降 + 指针在场）
	overTrunc := toolMsgOf(fm.inputs[1])
	if len(overTrunc) != 1 || len(overTrunc[0]) >= 10000 || !strings.Contains(overTrunc[0], "spill/trunc/c1") {
		t.Fatalf("出站工具结果应截断并带 spill 指针：len=%d", len(overTrunc[0]))
	}
	// 入史 = 截断版（允许例外①——原文经 Backend 可取回）
	var histTool string
	for _, m2 := range s.CloneHistory() {
		if m2.Role == schema.Tool && m2.ToolCallID == "c1" {
			histTool = m2.Content
		}
	}
	if !strings.Contains(histTool, "spill/trunc/c1") {
		t.Fatalf("入史应为截断版+指针，实得 %.80s", histTool)
	}
	// 外置原文落会话持久域（跨轮不失效——与 session 同目录树）
	full, ok := st.ReadUserTreeFile("张三", "sessions/"+s.SID+"/spill/trunc/c1")
	if !ok || !strings.Contains(string(full), strings.Repeat("x", 100)) {
		t.Fatalf("外置原文应在会话持久域：ok=%v len=%d", ok, len(full))
	}
	// 第三调：read_file 读回原文（spill/ 路由 → 完整 10k 数据在场）
	back := toolMsgOf(fm.inputs[2])
	if len(back) == 0 || !strings.Contains(back[len(back)-1], strings.Repeat("x", 100)) {
		t.Fatalf("read_file 应经 spill/ 路由读回原文：%d 条", len(back))
	}
}

// TestReductionClearFidelityRetention clear 触发后：出站视图旧轮换指针、保
// 最近 2 工具轮；**session 历史原文不变**（ClearAtLeastTokens>0 深拷贝闸——
// 保真红线锚定）。
func TestReductionClearFidelityRetention(t *testing.T) {
	// 窗口 20000：clear 阈值 6000 / 清出下限 1000 / 摘要触发 14000——五轮
	// ×8k 字符（≈2k token/轮，共 ≈10k）落在 (6000, 14000) 区间：clear 触发、
	// summarization 不触发（本测只验 clear 层）；r1~r3 可清（≈6k > 1000），
	// r4/r5 保尾。
	var hist []*schema.Message
	for i := 1; i <= 5; i++ {
		hist = append(hist,
			schema.UserMessage(fmt.Sprintf("问%d", i)),
			&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf(fmt.Sprintf("r%d", i), "old_tool", `{"q":"`+fmt.Sprint(i)+`"}`)}},
			schema.ToolMessage(fmt.Sprintf("R%dDATA ", i)+strings.Repeat("y", 8000), fmt.Sprintf("r%d", i)),
		)
	}
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, st := newReductionManager(t, 20000, nil, factoryOf(fm))
	s := m.Registry().Create("张三", "清除", "plan", contract.UserPrefs{Model: "p/m"})
	s.AppendHistory(hist...)
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "继续", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if len(fm.inputs) != 1 {
		t.Fatalf("应单次模型调用，实得 %d", len(fm.inputs))
	}
	over := toolMsgOf(fm.inputs[0])
	if len(over) != 5 {
		t.Fatalf("消息数不变（clear 原位替换不删消息），tool 消息应 5 条，实得 %d", len(over))
	}
	for i := 0; i < 3; i++ { // r1~r3 已清除 → 指针
		if !strings.Contains(over[i], "spill/clear/") {
			t.Fatalf("旧轮 r%d 应清除换指针，实得 %.60s", i+1, over[i])
		}
	}
	for i := 3; i < 5; i++ { // r4/r5 保尾
		if !strings.Contains(over[i], fmt.Sprintf("R%dDATA", i+1)) {
			t.Fatalf("保尾轮 r%d 原文应在场", i+1)
		}
	}
	// 保真锚定：session 历史五轮原文全部未动
	toolIdx := 0
	for _, m2 := range s.CloneHistory() {
		if m2.Role != schema.Tool {
			continue
		}
		toolIdx++
		if !strings.Contains(m2.Content, fmt.Sprintf("R%dDATA", toolIdx)) {
			t.Fatalf("红线违例：clear 后 session 历史第 %d 轮原文被改写（%.60s）", toolIdx, m2.Content)
		}
	}
	// 清除外置落会话持久域
	if full, ok := st.ReadUserTreeFile("张三", "sessions/"+s.SID+"/spill/clear/r1"); !ok || !strings.Contains(string(full), "R1DATA") {
		t.Fatalf("清除外置原文应可取回：ok=%v", ok)
	}
}

// TestReductionClearAtLeastGate 可清量不足 5% 窗口 → 整体不动（白破缓存保护）
// 且不写外置文件。
func TestReductionClearAtLeastGate(t *testing.T) {
	// 窗口 100000：阈值 30000 / 下限 5000。r1/r2 小（可清 ≈250 token）、
	// r3/r4 大（合计 ≈30.2k 过阈值但保尾）——可清量远低于下限 → 不动。
	var hist []*schema.Message
	sizes := []int{500, 500, 60000, 60000}
	for i, size := range sizes {
		hist = append(hist,
			schema.UserMessage(fmt.Sprintf("问%d", i+1)),
			&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf(fmt.Sprintf("r%d", i+1), "old_tool", `{}`)}},
			schema.ToolMessage(fmt.Sprintf("R%dDATA ", i+1)+strings.Repeat("y", size), fmt.Sprintf("r%d", i+1)),
		)
	}
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, st := newReductionManager(t, 100000, nil, factoryOf(fm))
	s := m.Registry().Create("张三", "闸", "plan", contract.UserPrefs{Model: "p/m"})
	s.AppendHistory(hist...)
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "继续", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	over := toolMsgOf(fm.inputs[0])
	if !strings.Contains(over[0], "R1DATA") {
		t.Fatalf("清出不足下限应整体不动，r1 原文应在场：%.60s", over[0])
	}
	if _, ok := st.ReadUserTreeFile("张三", "sessions/"+s.SID+"/spill/clear/r1"); ok {
		t.Fatal("闸拒绝时不应写外置文件")
	}
}

// TestReductionExcludeToolSearch exclude 名单工具不截断（H7 前置占位语义）。
func TestReductionExcludeToolSearch(t *testing.T) {
	ts, _ := tools.InferTool("tool_search", "检索桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"hits": strings.Repeat("h", 10000)}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{tcOf("c1", "tool_search", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, _ := newReductionManager(t, 0, []contract.Tool{ts}, factoryOf(fm))
	s := m.Registry().Create("张三", "排除", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "搜", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	over := toolMsgOf(fm.inputs[1])
	if len(over) != 1 || !strings.Contains(over[0], strings.Repeat("h", 100)) || strings.Contains(over[0], "spill/") {
		t.Fatalf("tool_search 结果不应截断/外置：len=%d", len(over[0]))
	}
}

// TestShapeOutboundStripsSettledReasoning H1④ Inputs 断言：同轮在途思考保留、
// 跨轮（已结算）剥离——shape 包装器在模型边界生效。
func TestShapeOutboundStripsSettledReasoning(t *testing.T) {
	ping, _ := tools.InferTool("ping", "桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ReasoningContent: "本轮思考"})
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{tcOf("c1", "ping", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, _ := newReductionManager(t, 0, []contract.Tool{ping}, factoryOf(fm))
	s := m.Registry().Create("张三", "整形", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "问1", nil, func(session.Event) {}) // 轮1：c1 在途
	waitTitleFlight(t, s)

	inFlight := false
	for _, m2 := range fm.inputs[1] {
		if m2.Role == schema.Assistant && m2.ReasoningContent == "本轮思考" {
			inFlight = true
		}
	}
	if !inFlight {
		t.Fatal("在途轮（最后 user 后带 tool_calls）思考应保留")
	}

	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "问2", nil, func(session.Event) {}) // 轮2：c1 已结算
	waitTitleFlight(t, s)
	for _, m2 := range fm.inputs[2] {
		if m2.Role == schema.Assistant && m2.ReasoningContent != "" {
			t.Fatalf("已结算轮思考应剥离：%.40s", m2.ReasoningContent)
		}
	}
}
