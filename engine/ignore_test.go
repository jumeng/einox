package engine

// ask 卡「忽略」回归（IgnoreAsk）：挂起 → 忽略 → ended 终态（超时兜底随
// 忽略取消）+ 悬空 ask_user 调用补搁置回执闭环历史（区别于超时路径的
// sanitize 剥离——唤醒轮模型记得问过）+ 唤醒 = 下一次用户发消息开新轮，
// 搁置回执随历史回传、新消息在尾，正常收口。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools/askuser"
)

// sessAskResolver 会话域作答源（产品装配同构：TakeAskDecision）。
type sessAskResolver struct{ s *session.Session }

func (r *sessAskResolver) Decision() *contract.AskDecision { return r.s.TakeAskDecision() }

// ignoreSetup ask_user 挂起装配：首轮发 ask_user 调用 → 挂起态返回。
func ignoreSetup(t *testing.T) (*Manager, *session.Session, *scriptedModel) {
	t.Helper()
	res := &sessAskResolver{}
	at, err := askuser.NewTools(askuser.Config{Resolver: res})
	if err != nil {
		t.Fatal(err)
	}
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{tcOf("q1", "ask_user",
				`{"question":"用哪个版本？","options":[{"label":"v2.8"},{"label":"v2.9"}],"allow_multi":false,"allow_free_text":true}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "收到，继续"})
	}}
	m, _ := newRunManager(t, at, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	})
	s := m.Registry().Create("张三", "问一下", "plan", contract.UserPrefs{Model: "p/m"})
	res.s = s
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "写周报", nil, func(session.Event) {})
	if s.StateOf() != session.StatePendingApproval || s.PendingAppID() == "" {
		t.Fatalf("应挂起提问，实得 state=%s appID=%q", s.StateOf(), s.PendingAppID())
	}
	t.Cleanup(func() { stopApprovalTimer(s.SID) })
	return m, s, fm
}

// TestIgnoreAskSnoozes 忽略即搁置：ended 终态 + 挂起标记清 + 回执事件落流 +
// 悬空调用补搁置回执闭环 + 停表（过超时点不触发 ask_timeout）。
func TestIgnoreAskSnoozes(t *testing.T) {
	old := approvalTimeout
	approvalTimeout = 150 * time.Millisecond
	t.Cleanup(func() { approvalTimeout = old })

	m, s, _ := ignoreSetup(t)
	m.IgnoreAsk(s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("忽略应置 ended 终态，实得 %s", s.StateOf())
	}
	if s.PendingAppID() != "" {
		t.Fatal("忽略应清挂起标记")
	}
	found := false
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvAskIgnored {
			found = true
		}
		if ev.Event == contract.EvAskTimeout {
			t.Fatal("忽略后不应触发超时收场")
		}
	}
	if !found {
		t.Fatal("忽略回执事件应落流（回放重建卡终态的真源）")
	}
	if ids := danglingToolCalls(s.CloneHistory()); len(ids) != 0 {
		t.Fatalf("忽略后不应残留悬空 tool_call：%v", ids)
	}
	snoozed := false
	for _, msg := range s.CloneHistory() {
		if msg.Role == schema.Tool && strings.Contains(msg.Content, "用户搁置") {
			snoozed = true
		}
	}
	if !snoozed {
		t.Fatal("悬空 ask_user 调用应补搁置回执入史（唤醒轮模型记得问过）")
	}
	// 停表验证：过超时点（150ms 兜底 + 余量）后无 ask_timeout 落流
	time.Sleep(300 * time.Millisecond)
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvAskTimeout {
			t.Fatal("停表失效：忽略后超时兜底仍触发")
		}
	}
}

// TestIgnoreAskWakeOnNewMessage 唤醒：下一次用户发消息开新轮（ended 放行），
// 搁置回执随历史回传 + 新消息在输入尾，正常收口无错误。
func TestIgnoreAskWakeOnNewMessage(t *testing.T) {
	m, s, fm := ignoreSetup(t)
	m.IgnoreAsk(s)

	// api 层 BeginRun 的测试直设等价（Run 前置：State 已置 running）
	s.SetState(session.StateRunning)
	var names []string
	m.Run(context.Background(), s, "先别问，直接做", nil, func(ev session.Event) { names = append(names, ev.Event) })
	waitTitleFlight(t, s)
	if s.StateOf() != session.StateEnded {
		t.Fatalf("唤醒轮应正常收口，实得 %s", s.StateOf())
	}
	if contains(names, contract.EvError) {
		t.Fatalf("唤醒轮不应报错：%v", names)
	}
	fm.mu.Lock()
	inputs := fm.inputs
	fm.mu.Unlock()
	if len(inputs) < 2 {
		t.Fatalf("唤醒轮应有第二次模型输入，实得 %d 轮", len(inputs))
	}
	last := inputs[len(inputs)-1]
	snoozeFed, newUser := false, false
	for _, msg := range last {
		if msg.Role == schema.Tool && strings.Contains(msg.Content, "用户搁置") {
			snoozeFed = true
		}
		if msg.Role == schema.User && strings.Contains(msg.Content, "先别问") {
			newUser = true
		}
	}
	if !snoozeFed {
		t.Fatal("搁置回执应随历史回传（模型从搁置语境衔接新消息）")
	}
	if !newUser {
		t.Fatal("唤醒消息应在模型输入中")
	}
}

// TestIgnoreAskIdempotentGuard 无挂起时 IgnoreAsk 幂等空操作（不 panic、
// 不动状态——入口守卫在应用端点，基座兜底）。
func TestIgnoreAskIdempotentGuard(t *testing.T) {
	m, s, _ := ignoreSetup(t)
	m.IgnoreAsk(s)
	if s.StateOf() != session.StateEnded {
		t.Fatalf("忽略应置 ended，实得 %s", s.StateOf())
	}
	m.IgnoreAsk(s) // 已清挂起——幂等空操作（不 panic、不动状态）
	if s.StateOf() != session.StateEnded {
		t.Fatalf("幂等空操作不得改状态，实得 %s", s.StateOf())
	}
}
