package engine

// H3 summarization 回归：触发+压缩（主模型输入=摘要视图、原文不可见）+
// 防超窗修剪（摘要输入裁预算内、user 边界起）+ 保真锚定（session 历史原文
// 不动）+ transcript 落域；清窗兜底（摘要模型恒败 → 尾段新窗、运行不炸）。
// 方案 = findings/2026-08-26-h3-summarization-plan.md。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// sumHist 构造 N 轮大文本历史（轮 i 的 assistant 内容带 RiBIG 标记）。
func sumHist(rounds, size int) []*schema.Message {
	var out []*schema.Message
	for i := 1; i <= rounds; i++ {
		out = append(out,
			schema.UserMessage(fmt.Sprintf("问%d", i)),
			schema.AssistantMessage(fmt.Sprintf("R%dBIG ", i)+strings.Repeat("c", size), nil),
		)
	}
	return out
}

// TestSummarizeTriggersAndFidelity 窗口 20000（触发 14000/输入预算 16000）×
// 六轮 15k 字符（≈22.5k token）：触发摘要 → 主模型输入=摘要视图（原文不可见；
// 近期用户消息重内联依赖摘要文本含 <all_user_messages> 标签——脚本摘要不含，
// 该形态归真实模型行为，此处不设断言）；摘要输入防超窗修剪（最老轮被裁、
// user 边界起）；**session 历史原文不动**（保真锚定）；transcript 全文落会话持久域。
func TestSummarizeTriggersAndFidelity(t *testing.T) {
	parent := &scriptedModel{}              // 主模型（Stream；收口「已处理。」）
	sub := &recGenModel{reply: "非流式答复摘要文本"} // 摘要模型（Generate 路径独立记录）
	n := 0
	m, st := newReductionManager(t, 20000, nil, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 { // 构造序：主模型(1) → 摘要模型(2)（genTitle 第 3 次，Generate 不计数）
			return sub, nil
		}
		return parent, nil
	})
	s := m.Registry().Create("张三", "压缩", "plan", contract.UserPrefs{Model: "p/m"})
	s.AppendHistory(sumHist(6, 15000)...)
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "最新指示：继续推进", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if len(sub.inputs) != 1 {
		t.Fatalf("摘要模型应恰一次调用，实得 %d", len(sub.inputs))
	}
	if len(parent.inputs) != 1 {
		t.Fatalf("主模型应恰一次调用，实得 %d", len(parent.inputs))
	}
	// 摘要输入：防超窗——最老轮被裁、最新轮在场、摘要指令在场
	var joined strings.Builder
	for _, m2 := range sub.inputs[0] {
		joined.WriteString(msgTextOf(m2))
	}
	all := joined.String()
	if !strings.Contains(all, "R6BIG") || strings.Contains(all, "R1BIG") {
		t.Fatalf("摘要输入应裁最老轮保最新轮（防超窗）")
	}
	if !strings.Contains(all, "结构化摘要") {
		t.Fatalf("摘要输入应含摘要指令")
	}
	// 主模型输入：摘要视图——旧轮原文不可见、摘要文本在场
	var mainJoined strings.Builder
	for _, m2 := range parent.inputs[0] {
		mainJoined.WriteString(msgTextOf(m2))
	}
	mainAll := mainJoined.String()
	if strings.Contains(mainAll, "R1BIG") || strings.Contains(mainAll, "R5BIG") {
		t.Fatalf("主模型输入应为摘要视图（旧轮原文不可见）")
	}
	if !strings.Contains(mainAll, "非流式答复摘要文本") {
		t.Fatalf("主模型输入应含摘要文本")
	}
	if !strings.Contains(mainAll, "spill/transcript-"+s.SID+".txt") {
		t.Fatalf("摘要信封应含 transcript 溯源路径（Finalize 包装注入）")
	}
	// 保真锚定：session 历史六轮原文全部未动
	for i := 1; i <= 6; i++ {
		found := false
		for _, m2 := range s.CloneHistory() {
			if strings.Contains(m2.Content, fmt.Sprintf("R%dBIG", i)) {
				found = true
			}
		}
		if !found {
			t.Fatalf("红线违例：session 历史第 %d 轮原文在摘要后丢失", i)
		}
	}
	// transcript 全文落域（含被裁的最老轮——模型可溯源）
	if tr, ok := st.ReadUserTreeFile("张三", "sessions/"+s.SID+"/spill/transcript-"+s.SID+".txt"); !ok || !strings.Contains(string(tr), "R1BIG") {
		t.Fatalf("transcript 应含全量历史：ok=%v", ok)
	}
}

// genFailModel Generate 恒败（摘要模型桩——测清窗兜底）；Stream 委托内层。
type genFailModel struct{ inner *scriptedModel }

func (g *genFailModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return nil, errors.New("摘要模型不可用")
}

func (g *genFailModel) Stream(ctx context.Context, in []*schema.Message, o ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return g.inner.Stream(ctx, in, o...)
}

// TestSummarizeFallbackClearsWindow 摘要模型恒败（Retry 耗尽）→ 清窗兜底：
// 主模型输入=自最后 user 起的尾段新窗（旧内容不可见）、运行正常收口无 error 终态。
func TestSummarizeFallbackClearsWindow(t *testing.T) {
	parent := &scriptedModel{}
	n := 0
	fm := func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 { // 构造序：主模型(1) → 摘要模型(2)
			return &genFailModel{inner: parent}, nil
		}
		return parent, nil
	}
	m, st := newReductionManager(t, 20000, nil, fm)
	s := m.Registry().Create("张三", "兜底", "plan", contract.UserPrefs{Model: "p/m"})
	s.AppendHistory(sumHist(6, 15000)...)
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "新任务开始", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("摘要失败经清窗兜底不得杀运行，终态 %s", s.StateOf())
	}
	// 主模型输入 = 尾段新窗：仅最后 user（本轮消息），旧轮原文不可见
	//（parent.inputs 只含主模型调用——genFail 的 Generate 恒败且不记录输入）
	if len(parent.inputs) != 1 {
		t.Fatalf("主模型应恰一次调用，实得 %d", len(parent.inputs))
	}
	mainIn := parent.inputs[0]
	joined := strings.Builder{}
	for _, m2 := range mainIn {
		joined.WriteString(msgTextOf(m2))
	}
	all := joined.String()
	if strings.Contains(all, "BIG") {
		t.Fatalf("清窗兜底后旧内容应不可见：%.80s", all)
	}
	if !strings.Contains(all, "新任务开始") {
		t.Fatalf("清窗兜底应保留最后 user 起尾段：%.80s", all)
	}
	// 兜底先落 transcript 原文（防「压缩失败 = 全文丢失」）+ 发清窗通知卡
	//（经 s.Record 落事件流——生产 live 流靠订阅扇出，与 reduction note 同通道）
	if tr, ok := st.ReadUserTreeFile("张三", "sessions/"+s.SID+"/spill/transcript-"+s.SID+".txt"); !ok || !strings.Contains(string(tr), "R1BIG") {
		t.Fatalf("清窗兜底应先落域 transcript 全文：ok=%v", ok)
	}
	var note *contract.HarnessNote
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvHarnessNote {
			if hn, ok := ev.Data.(contract.HarnessNote); ok {
				note = &hn
			}
		}
	}
	if note == nil || !strings.Contains(note.Title, "清窗") {
		t.Fatalf("清窗兜底应发 harness_note 通知卡，实得 %+v", note)
	}
}

// TestSummarizeFallbackTaskAnchor 清窗兜底任务锚（H9-9）：摘要恒败 + 被清
// 历史含 todo_write 轮 → 清窗后主模型输入含最后 todo 清单状态 + 锚指引 +
// transcript 路径；锚为单轮注入不落 session 真源（合成消息不进 events）。
func TestSummarizeFallbackTaskAnchor(t *testing.T) {
	parent := &scriptedModel{}
	n := 0
	fm := func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 { // 构造序：主模型(1) → 摘要模型(2)
			return &genFailModel{inner: parent}, nil
		}
		return parent, nil
	}
	m, _ := newReductionManager(t, 20000, nil, fm)
	s := m.Registry().Create("张三", "锚点", "plan", contract.UserPrefs{Model: "p/m"})
	hist := sumHist(5, 15000)
	callID := "call-todo"
	todoArgs, _ := json.Marshal(map[string]any{"todos": []map[string]any{
		{"content": "勘察仓库结构", "status": "completed", "priority": "high"},
		{"content": "汇总差异清单", "status": "in_progress", "priority": "high"},
	}})
	hist = append(hist, schema.AssistantMessage("", []schema.ToolCall{{ID: callID, Function: schema.FunctionCall{Name: "todo_write", Arguments: string(todoArgs)}}}),
		&schema.Message{Role: schema.Tool, ToolCallID: callID, Content: `{"ok":true,"count":2,"completed":1,"todos":[{"content":"勘察仓库结构","status":"completed","priority":"high"},{"content":"汇总差异清单","status":"in_progress","priority":"high"}]}`})
	hist = append(hist, sumHist(1, 15000)...)
	s.AppendHistory(hist...)
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "继续汇总", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("清窗兜底不得杀运行，终态 %s", s.StateOf())
	}
	if len(parent.inputs) != 1 {
		t.Fatalf("主模型应恰一次调用，实得 %d", len(parent.inputs))
	}
	joined := strings.Builder{}
	for _, m2 := range parent.inputs[0] {
		joined.WriteString(msgTextOf(m2))
	}
	all := joined.String()
	if !strings.Contains(all, "[completed] 勘察仓库结构") || !strings.Contains(all, "[in_progress] 汇总差异清单") {
		t.Fatalf("任务锚应含最后 todo 清单状态：%.160s", all)
	}
	if !strings.Contains(all, "任务状态锚") || !strings.Contains(all, "spill/transcript-"+s.SID+".txt") || !strings.Contains(all, "不要从头重做") {
		t.Fatalf("任务锚应含锚指引与 transcript 路径：%.160s", all)
	}
	// 锚不落 session 真源（单轮注入——保真：合成消息不进历史）
	for _, m2 := range s.CloneHistory() {
		if m2 != nil && m2.Role == schema.User && strings.Contains(m2.Content, "任务状态锚") {
			t.Fatalf("锚不得落入 session 历史（单轮注入语义）")
		}
	}
}

// TestSummarizeFallbackAnchorNoTodo 无 todo_write 轮 → 锚降级为纯指引
// （不含清单段，指引与 transcript 路径仍在）。
func TestSummarizeFallbackAnchorNoTodo(t *testing.T) {
	parent := &scriptedModel{}
	n := 0
	fm := func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 {
			return &genFailModel{inner: parent}, nil
		}
		return parent, nil
	}
	m, _ := newReductionManager(t, 20000, nil, fm)
	s := m.Registry().Create("张三", "无清单", "plan", contract.UserPrefs{Model: "p/m"})
	s.AppendHistory(sumHist(6, 15000)...)
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "新任务", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if len(parent.inputs) != 1 {
		t.Fatalf("主模型应恰一次调用，实得 %d", len(parent.inputs))
	}
	joined := strings.Builder{}
	for _, m2 := range parent.inputs[0] {
		joined.WriteString(msgTextOf(m2))
	}
	all := joined.String()
	if !strings.Contains(all, "任务状态锚") || !strings.Contains(all, "spill/transcript-"+s.SID+".txt") {
		t.Fatalf("无 todo 轮时锚应降级为纯指引：%.160s", all)
	}
	if strings.Contains(all, "当前任务清单状态") {
		t.Fatalf("无 todo 轮时锚不应含清单段：%.160s", all)
	}
}

// TestSummarizeFallbackAnchorExternalTodo todo 结果被外置成指针文本 →
// 解析失败降级（锚不含清单段，不解析 spill 内容）。
func TestSummarizeFallbackAnchorExternalTodo(t *testing.T) {
	parent := &scriptedModel{}
	n := 0
	fm := func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 {
			return &genFailModel{inner: parent}, nil
		}
		return parent, nil
	}
	m, _ := newReductionManager(t, 20000, nil, fm)
	s := m.Registry().Create("张三", "外置", "plan", contract.UserPrefs{Model: "p/m"})
	hist := sumHist(5, 15000)
	callID := "call-todo2"
	hist = append(hist, schema.AssistantMessage("", []schema.ToolCall{{ID: callID, Function: schema.FunctionCall{Name: "todo_write", Arguments: `{"todos":[{"content":"x","status":"pending"}]}`}}}),
		&schema.Message{Role: schema.Tool, ToolCallID: callID, Content: "工具结果已外置（24000 字符）：spill/trunc/" + callID + "（read_file 可取回）"})
	hist = append(hist, sumHist(1, 15000)...)
	s.AppendHistory(hist...)
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "继续", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if len(parent.inputs) != 1 {
		t.Fatalf("主模型应恰一次调用，实得 %d", len(parent.inputs))
	}
	joined := strings.Builder{}
	for _, m2 := range parent.inputs[0] {
		joined.WriteString(msgTextOf(m2))
	}
	all := joined.String()
	if strings.Contains(all, "当前任务清单状态") {
		t.Fatalf("todo 结果已外置时锚应降级（不解析 spill）：%.160s", all)
	}
	if !strings.Contains(all, "任务状态锚") {
		t.Fatalf("降级锚的指引段应在场：%.160s", all)
	}
}

// TestSummarizeSmallWindowSkillsBudget H9-3 PreserveSkills 预算窗口钳制：
// window=30000（触发线 21000，预算钳 3000）+ 末尾两个 skill 调用对（各
// 8000 字节 ≈2000 adk-token，不触 reduction 8192 截断线）——两份合计超
// 钳制预算只保最近一份，老的 drop；摘要后视图 < 触发线，摘要模型恰一次
// 调用（不逐调用重摘要）。adk 默认 25k 窗口无关常数下两份都会保留（老
// skill 原文进视图）——「老 skill 不在视图」即钳制效果的区分断言。
func TestSummarizeSmallWindowSkillsBudget(t *testing.T) {
	parent := &scriptedModel{}
	sub := &recGenModel{reply: "摘要文本"}
	n := 0
	m, _ := newReductionManager(t, 30000, nil, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 {
			return sub, nil
		}
		return parent, nil
	})
	s := m.Registry().Create("张三", "小窗", "plan", contract.UserPrefs{Model: "p/m"})
	skillRound := func(id, body string) (*schema.Message, *schema.Message) {
		call := schema.AssistantMessage("", []schema.ToolCall{{ID: id, Function: schema.FunctionCall{Name: "skill", Arguments: `{"skill":"` + id + `"}`}}})
		return call, &schema.Message{Role: schema.Tool, ToolCallID: id, Content: body}
	}
	c1, r1 := skillRound("call-skill-a", strings.Repeat("A", 8000))
	c2, r2 := skillRound("call-skill-b", strings.Repeat("B", 8000))
	hist := append(sumHist(6, 15000), c1, r1, c2, r2)
	s.AppendHistory(hist...)
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "继续", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("运行应收口，终态 %s", s.StateOf())
	}
	if len(sub.inputs) != 1 {
		t.Fatalf("钳制预算下摘要应恰一次（不逐调用重摘要），实得 %d", len(sub.inputs))
	}
	var joined strings.Builder
	for _, m2 := range parent.inputs[0] {
		joined.WriteString(msgTextOf(m2))
	}
	if strings.Contains(joined.String(), "AAAAAA") {
		t.Fatalf("超预算的老 skill 内容不应保留进摘要后视图（两份 2000 token > 3000 预算只保最近）")
	}
	if !strings.Contains(joined.String(), "BBBBBB") {
		t.Fatalf("最近的 skill 内容应在钳制预算内保留")
	}
}

// TestSummarizerFailoverChain H9-10 摘要降级链：主摘要模型恒败 → 链二
// （flash 形态）承接摘要成功（同修剪输入）→ 运行收口、主模型见摘要视图。
func TestSummarizerFailoverChain(t *testing.T) {
	parent := &scriptedModel{}
	sub := &recGenModel{reply: "降级链摘要文本"}
	n := 0
	fm := func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		switch n {
		case 2: // 构造序：主(1) → 主摘要(2, 恒败) → 降级(3, 成功)
			return &genFailModel{inner: parent}, nil
		case 3:
			return sub, nil
		}
		return parent, nil
	}
	m, _ := newReductionManager(t, 20000, nil, fm, func(o *Options) {
		o.SummarizerFallbackModels = []string{"p/m"} // 同键复用构造序分派（链位由 n 区分）
	})
	s := m.Registry().Create("张三", "降级", "plan", contract.UserPrefs{Model: "p/m"})
	s.AppendHistory(sumHist(6, 15000)...)
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "新任务", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("降级承接后应收口（不清窗不外抛），终态 %s", s.StateOf())
	}
	if len(sub.inputs) != 1 {
		t.Fatalf("降级模型应恰承接一次摘要，实得 %d", len(sub.inputs))
	}
	if len(parent.inputs) != 1 {
		t.Fatalf("主模型应恰一次调用，实得 %d", len(parent.inputs))
	}
	var joined strings.Builder
	for _, m2 := range parent.inputs[0] {
		joined.WriteString(msgTextOf(m2))
	}
	if !strings.Contains(joined.String(), "降级链摘要文本") {
		t.Fatalf("主模型应见降级产出的摘要视图：%.120s", joined.String())
	}
	// 不走清窗兜底：无「摘要失败」通知卡
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvHarnessNote {
			if hn, ok := ev.Data.(contract.HarnessNote); ok && strings.Contains(hn.Title, "摘要失败") {
				t.Fatal("降级承接成功不应触发清窗兜底通知")
			}
		}
	}
}

// TestSummarizerFailoverExhausted H9-10 全链败：主摘要+降级全恒败 → 链尽
// 走既有清窗兜底（不外抛、发清窗通知卡、主模型尾段新窗续跑）。
func TestSummarizerFailoverExhausted(t *testing.T) {
	parent := &scriptedModel{}
	n := 0
	fm := func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 || n == 3 { // 主摘要与降级位均恒败
			return &genFailModel{inner: parent}, nil
		}
		return parent, nil
	}
	m, _ := newReductionManager(t, 20000, nil, fm, func(o *Options) {
		o.SummarizerFallbackModels = []string{"p/m"}
	})
	s := m.Registry().Create("张三", "链尽", "plan", contract.UserPrefs{Model: "p/m"})
	s.AppendHistory(sumHist(6, 15000)...)
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "新任务", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("链尽应走清窗兜底不杀运行，终态 %s", s.StateOf())
	}
	var note *contract.HarnessNote
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvHarnessNote {
			if hn, ok := ev.Data.(contract.HarnessNote); ok {
				note = &hn
			}
		}
	}
	if note == nil || !strings.Contains(note.Title, "清窗") {
		t.Fatalf("链尽应触发清窗兜底通知卡，实得 %+v", note)
	}
}
