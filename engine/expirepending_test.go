package engine

// 消费者驱动回归（einox-pm 决议端点形态：SetDecision(For) → Record → Persist
// → BeginRun → go Resume）：决议登记与 Resume 首行 stopApprovalTimer 之间
// 存在窗口，超时到点若照常执行会把用户 approve 覆盖成超时拒绝、把 BeginRun
// 的 running 改写回 ended——expirePending 让位守卫的行为钉板。

import (
	"fmt"
	"testing"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
)

// pendingApprovalOf 白盒构造挂起态（session 层 API 组合，不经模型流）。
func pendingApprovalOf(t *testing.T, m *Manager, items int) *session.Session {
	t.Helper()
	s := m.Registry().Create("张三", "审批", "manual", contract.UserPrefs{})
	s.SetPendingApproval("a1")
	ids := make([]string, 0, items)
	for i := 0; i < items; i++ {
		ids = append(ids, fmt.Sprintf("i%d", i))
	}
	if len(ids) > 0 {
		s.SetPendingItems(ids)
	}
	return s
}

// TestExpirePendingYieldsToItemDecision 决议已到达（批量逐项之一）即让位：
// 不覆盖决议、不发超时事件、不置终态——Resume 将消费。
func TestExpirePendingYieldsToItemDecision(t *testing.T) {
	m := newSeamManager(t, nil)
	s := pendingApprovalOf(t, m, 2)
	s.SetDecisionFor("i0", contract.ApprovalDecision{Approve: true, Reason: "批准"})
	m.expirePending(s, "a1", "approval")
	if d := s.TakeDecisionFor("i0"); d == nil || !d.Approve {
		t.Fatalf("已到达决议不得被超时覆盖：%+v", d)
	}
	if s.StateOf() == session.StateEnded {
		t.Fatal("决议已到达时超时不得置终态（BeginRun 的 running 被改写回 ended）")
	}
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvApprovalTimeout {
			t.Fatal("决议已到达时不得发超时事件")
		}
	}
}

// TestExpirePendingYieldsToAskAnswer 提问作答到达同理让位（SetAskDecision
// → go Resume 窗口）。
func TestExpirePendingYieldsToAskAnswer(t *testing.T) {
	m := newSeamManager(t, nil)
	s := m.Registry().Create("张三", "提问", "auto", contract.UserPrefs{})
	s.SetPendingApproval("q1")
	s.SetAskDecision(contract.AskDecision{FreeText: "答案"})
	m.expirePending(s, "q1", "ask")
	if d := s.TakeAskDecision(); d == nil || d.FreeText != "答案" {
		t.Fatalf("已到达作答不得被超时丢弃：%+v", d)
	}
	if s.StateOf() == session.StateEnded {
		t.Fatal("作答已到达时超时不得置终态")
	}
}

// TestExpirePendingStillFiresWithoutDecision 无决议到点照常超时（让位守卫
// 不吞正常超时路径）。
func TestExpirePendingStillFiresWithoutDecision(t *testing.T) {
	m := newSeamManager(t, nil)
	s := pendingApprovalOf(t, m, 1)
	m.expirePending(s, "a1", "approval")
	if d := s.TakeDecisionFor("i0"); d == nil || d.Approve {
		t.Fatalf("无决议到点应自动拒绝（fail-closed）：%+v", d)
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("超时应置 ended 终态，实得 %s", s.StateOf())
	}
}
