package engine

// 会话域写授权（「本会话内始终允许」决议档）回归——设计件
// findings/2026-09-23-session-write-grant-design.md：
// ① manual 档 Always 批准后，后续同工具写调用免逐次审批（不再挂起）；
// ② 全批形态双工具并发一卡，Always 全批 → 两工具均获授权；
// ③ 拒绝 + Always 不授权；
// ④ 授权随会话记录持久（Reattach 续接——用户显式让渡不因重启失效）。

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// countApprovals 记录面审批卡张数（续流执行体走 noopEmit——回调面收不到，
// 事件流真源在会话记录，断言一律走 SnapshotEvents）。
func countApprovals(s *session.Session) int {
	n := 0
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvApprovalRequest {
			n++
		}
	}
	return n
}

// grantSetup 单写工具 manual 档挂起：剧本 = 前 writes 轮每轮一次 write_tool
// 调用（首调挂起；决议续流后按授权状态分叉），之后收口。
func grantSetup(t *testing.T, writes int) (*Manager, *session.Session, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	wt, _ := tools.InferTool("write_tool", "写", func(context.Context, struct{}) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n <= writes {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf(fmt.Sprintf("c%d", n), "write_tool", `{}`),
			}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, _ := newRunManager(t, []contract.Tool{wt}, factoryOf(fm))
	s := m.Registry().Create("张三", "连续写", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "写", nil, func(ev session.Event) {})
	t.Cleanup(func() { stopApprovalTimer(s.SID) })
	if s.StateOf() != session.StatePendingApproval {
		t.Fatalf("首次写调用应挂起，实得 %s", s.StateOf())
	}
	return m, s, &calls
}

// TestWriteGrantSkipsSecondSuspension Always 批准后续调免审：整个剧本两次
// write_tool，仅第一张审批卡；两调都执行；正常收口。
func TestWriteGrantSkipsSecondSuspension(t *testing.T) {
	m, s, calls := grantSetup(t, 2)
	if err := m.Channels().Approve(s.SID, "", nil, contract.ApprovalDecision{
		Approve: true, Always: true}); err != nil {
		t.Fatalf("Always 批准应成功：%v", err)
	}
	waitFor(t, "Always 批准后应两调连跑正常收口", func() bool { return s.StateOf() == session.StateEnded })
	waitTitleFlight(t, s)
	if got := calls.Load(); got != 2 {
		t.Fatalf("两次写调用都应执行，实得 %d", got)
	}
	if got := countApprovals(s); got != 1 {
		t.Fatalf("授权后第二次调用不得再挂起（应仅 1 张审批卡），实得 %d", got)
	}
	if !s.WriteGranted("write_tool") {
		t.Fatal("Always 批准应登记会话域写授权")
	}
}

// TestWriteGrantOnceWithoutAlways 仅本次批准：无 Always 的批准不授权，
// 第二次写调用再次挂起。
func TestWriteGrantOnceWithoutAlways(t *testing.T) {
	m, s, calls := grantSetup(t, 2)
	if err := m.Channels().Approve(s.SID, "", nil, contract.ApprovalDecision{Approve: true}); err != nil {
		t.Fatalf("单次批准应成功：%v", err)
	}
	waitFor(t, "第二次写调用应再次挂起", func() bool { return countApprovals(s) == 2 && s.StateOf() == session.StatePendingApproval })
	waitTitleFlight(t, s)
	if got := calls.Load(); got != 1 {
		t.Fatalf("仅首次调用执行，实得 %d", got)
	}
	if s.WriteGranted("write_tool") {
		t.Fatal("无 Always 批准不得登记授权")
	}
}

// TestWriteGrantDenyIgnoredAlways 拒绝 + Always：忽略授权位（fail-closed——
// 拒绝语义不产生任何让渡），第二次写调用照常挂起。
func TestWriteGrantDenyIgnoredAlways(t *testing.T) {
	m, s, calls := grantSetup(t, 2)
	if err := m.Channels().Approve(s.SID, "", nil, contract.ApprovalDecision{
		Approve: false, Always: true, Reason: "先别写"}); err != nil {
		t.Fatalf("拒绝应成功：%v", err)
	}
	waitFor(t, "拒绝后第二次写调用应再次挂起", func() bool { return countApprovals(s) == 2 && s.StateOf() == session.StatePendingApproval })
	waitTitleFlight(t, s)
	if got := calls.Load(); got != 0 {
		t.Fatalf("拒绝的调用不得执行，实得 %d", got)
	}
	if s.WriteGranted("write_tool") {
		t.Fatal("拒绝 + Always 不得登记授权")
	}
}

// TestWriteGrantPersistsAcrossReattach 授权持久：Always 批准收口后经
// Reattach 重建会话，授权仍在（后续调用免审直达）。
func TestWriteGrantPersistsAcrossReattach(t *testing.T) {
	m, s, _ := grantSetup(t, 2)
	if err := m.Channels().Approve(s.SID, "", nil, contract.ApprovalDecision{
		Approve: true, Always: true}); err != nil {
		t.Fatalf("Always 批准应成功：%v", err)
	}
	waitFor(t, "Always 批准后应正常收口", func() bool { return s.StateOf() == session.StateEnded })
	waitTitleFlight(t, s)
	restored := m.Registry().Reattach(s.Owner, s.SID)
	if restored == nil {
		t.Fatal("Reattach 应重建会话")
	}
	if !restored.WriteGranted("write_tool") {
		t.Fatal("会话域写授权应随记录持久（重启续接）")
	}
}

// TestWriteGrantBatchBothTools 全批形态：双写并发一卡两项，Always 全批 →
// 两工具均获授权，后续双写调用不再挂起。
func TestWriteGrantBatchBothTools(t *testing.T) {
	var callsA, callsB atomic.Int32
	wa, _ := tools.InferTool("write_a", "写甲", func(context.Context, struct{}) (map[string]any, error) {
		callsA.Add(1)
		return map[string]any{"ok": true}, nil
	})
	wb, _ := tools.InferTool("write_b", "写乙", func(context.Context, struct{}) (map[string]any, error) {
		callsB.Add(1)
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n <= 2 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf(fmt.Sprintf("ca%d", n), "write_a", `{}`),
				tcOf(fmt.Sprintf("cb%d", n), "write_b", `{}`),
			}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, _ := newRunManager(t, []contract.Tool{wa, wb}, factoryOf(fm))
	m.Opt.Approval.WriteTools["write_a"] = true
	m.Opt.Approval.WriteTools["write_b"] = true
	s := m.Registry().Create("张三", "双写两轮", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "写两轮", nil, func(ev session.Event) {})
	t.Cleanup(func() { stopApprovalTimer(s.SID) })
	if countApprovals(s) != 1 {
		t.Fatalf("首轮双写应聚合一卡，实得 %d", countApprovals(s))
	}
	if err := m.Channels().Approve(s.SID, "", nil, contract.ApprovalDecision{
		Approve: true, Always: true}); err != nil {
		t.Fatalf("全批 Always 应成功：%v", err)
	}
	waitFor(t, "授权后第二轮双写应免审直达并收口", func() bool { return s.StateOf() == session.StateEnded })
	waitTitleFlight(t, s)
	if callsA.Load() != 2 || callsB.Load() != 2 {
		t.Fatalf("两轮双写都应执行，实得 a=%d b=%d", callsA.Load(), callsB.Load())
	}
	if got := countApprovals(s); got != 1 {
		t.Fatalf("授权后不得再挂起（应恒 1 张卡），实得 %d", got)
	}
	if !s.WriteGranted("write_a") || !s.WriteGranted("write_b") {
		t.Fatal("全批 Always 应对两项工具都登记授权")
	}
}
