package engine

// C1 模型切换历史注记（dsh model-switch notice 对位）回归：换模型后新模型
// 的输入含「上方回复由旧模型生成」注记（只进模型投影面——AppendHistory 不
// Record 事件，人读 transcript 无此条；model_change 事件管显示不变）。

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// newNoteManager 双模型（p/m、p/m2）分派假模型的测试引擎：mA 收 p/m 轮、
// mB 收 p/m2 轮（按 spec.ID 分派——不依赖构造序）。
func newNoteManager(t *testing.T, mA, mB *scriptedModel, note func(from, to string) string) *Manager {
	t.Helper()
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
			if spec.ID == "m2" {
				return mB, nil
			}
			return mA, nil
		}
		o.ModelChangeNote = note
	})
	return m
}

// runNote 一轮便捷面（轮间回 running 态——对齐 API 层 BeginRun 语义）。
func runNote(m *Manager, s *session.Session, text string) {
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, text, nil, func(session.Event) {})
}

// countMsg 数输入中指定角色+文本的消息条数。
func countMsg(in []*schema.Message, text string) int {
	n := 0
	for _, msg := range in {
		if msg.Role == schema.User && msg.Content == text {
			n++
		}
	}
	return n
}

// idxMsg 首个指定文本消息的下标（无 = -1）。
func idxMsg(in []*schema.Message, text string) int {
	for i, msg := range in {
		if msg.Role == schema.User && msg.Content == text {
			return i
		}
	}
	return -1
}

// TestModelChangeNoteInjected 换模型注入：注记落在旧轮之后、新输入之前；
// transcript 无此条；model_change 事件照旧；同模型后续轮不重注且旧注记持久。
func TestModelChangeNoteInjected(t *testing.T) {
	mA, mB := &scriptedModel{}, &scriptedModel{}
	m := newNoteManager(t, mA, mB, DefaultModelChangeNote)
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})

	runNote(m, s, "第一个问题")
	waitTitleFlight(t, s)
	if got := countMsg(lastInput(mA), DefaultModelChangeNote("p/m", "p/m2")); got != 0 {
		t.Fatalf("首跑（无前序模型）不应注入：%d", got)
	}

	s.SetRunModel("p/m2", "")
	runNote(m, s, "第二个问题")
	waitTitleFlight(t, s)

	note := DefaultModelChangeNote("p/m", "p/m2")
	in := lastInput(mB)
	iNote, iQ := idxMsg(in, note), idxMsg(in, "第二个问题")
	iA := -1
	for i, msg := range in {
		if msg.Role == schema.Assistant && msg.Content == "已处理。" {
			iA = i
		}
	}
	if iNote < 0 || iQ < 0 || iA < 0 || !(iA < iNote && iNote < iQ) {
		t.Fatalf("注记应位于旧轮 assistant 之后、新输入之前：note=%d q=%d lastAssistant=%d", iNote, iQ, iA)
	}
	for _, ev := range s.SnapshotEvents() { // transcript 不可见：注记不进 user_message 事件
		if ev.Event == contract.EvUserMessage {
			if um, ok := ev.Data.(contract.UserMsg); ok && um.Text == note {
				t.Fatal("注记不应 Record 为 user_message 事件（人读 transcript 无此条）")
			}
		}
	}
	sawChange := false
	for _, ev := range s.SnapshotEvents() { // 显示面照旧：model_change 事件在流
		if ev.Event == contract.EvModelChange {
			sawChange = true
		}
	}
	if !sawChange {
		t.Fatal("model_change 事件（显示面）应照旧落流")
	}

	runNote(m, s, "第三个问题") // 同模型：不重注；旧注记随历史持久
	waitTitleFlight(t, s)
	in = lastInput(mB)
	if got := countMsg(in, note); got != 1 {
		t.Fatalf("同模型后续轮应恰一条持久注记：%d", got)
	}
	if idxMsg(in, "第三个问题") < 0 {
		t.Fatal("第三轮输入应在场")
	}
}

// TestModelChangeNoteNilOff 装配缝 nil = 零变化（不注入，历史与事件均无注记）。
func TestModelChangeNoteNilOff(t *testing.T) {
	mA, mB := &scriptedModel{}, &scriptedModel{}
	m := newNoteManager(t, mA, mB, nil)
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	runNote(m, s, "第一个问题")
	waitTitleFlight(t, s)
	s.SetRunModel("p/m2", "")
	runNote(m, s, "第二个问题")
	waitTitleFlight(t, s)
	if got := len(lastInput(mB)); got != 4 { // system + 旧 user + 旧 assistant + 新 user（adk 前置 Instruction 为 system 位）
		t.Fatalf("nil 缝零变化：输入应恰 4 条（无注记）：%d", got)
	}
}

// TestModelChangeNoteCustom 自定义文案生效；返回空串 = 跳过注入。
func TestModelChangeNoteCustom(t *testing.T) {
	mA, mB := &scriptedModel{}, &scriptedModel{}
	m := newNoteManager(t, mA, mB, func(from, to string) string {
		if from == "p/m" {
			return "custom " + from + ">" + to
		}
		return ""
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	runNote(m, s, "第一个问题")
	waitTitleFlight(t, s)
	s.SetRunModel("p/m2", "")
	runNote(m, s, "第二个问题")
	waitTitleFlight(t, s)
	if got := countMsg(lastInput(mB), "custom p/m>p/m2"); got != 1 {
		t.Fatalf("自定义文案应注入：%d", got)
	}
	if got := len(lastInput(mB)); got != 5 { // system + 旧两条 + 注记 + 新 user
		t.Fatalf("恰一条注记：输入应 5 条：%d", got)
	}
}
