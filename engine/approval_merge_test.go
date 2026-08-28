package engine

// H4-2a 合并决议回归（方案 = findings/2026-08-26-h4-parallel-aggregation-plan.md
// 2a 节；08-25 挂账场景的正反例）：一轮双写工具并发中断 → 泵聚合一张卡两项 →
// 批量批准两工具都执行 / 批一拒一各自生效 / 超时全项拒绝（fail-closed 批形态）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// mergedSetup 双写工具并发中断挂起（manual 档一轮两调用），返回管理器/会话/
// 挂起卡/两工具计数。父模型：首调一轮双写，续调收口。
func mergedSetup(t *testing.T) (*Manager, *session.Session, *contract.ApprovalReq, *int32, *int32) {
	t.Helper()
	var callsA, callsB int32
	wa, _ := tools.InferTool("write_a", "写甲", func(context.Context, struct{}) (map[string]any, error) {
		callsA++
		return map[string]any{"ok": true, "who": "a"}, nil
	})
	wb, _ := tools.InferTool("write_b", "写乙", func(context.Context, struct{}) (map[string]any, error) {
		callsB++
		return map[string]any{"ok": true, "who": "b"}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("ca", "write_a", `{}`),
				tcOf("cb", "write_b", `{}`),
			}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, _ := newRunManager(t, []contract.Tool{wa, wb}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	})
	m.Opt.Approval = hitl.ApprovalConfig{WriteTools: map[string]bool{"write_a": true, "write_b": true}}
	s := m.Registry().Create("张三", "双写", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var card *contract.ApprovalReq
	m.Run(context.Background(), s, "写两个", nil, func(ev session.Event) {
		if ev.Event == contract.EvApprovalRequest {
			req := ev.Data.(contract.ApprovalReq)
			card = &req
		}
	})
	t.Cleanup(func() { stopApprovalTimer(s.SID) })
	if card == nil {
		t.Fatal("双写并发中断应发审批卡")
	}
	return m, s, card, &callsA, &callsB
}

// TestMergedApprovalOneCardTwoItems 双写并发中断聚合一张卡两项（非两张卡）：
// 项标识互异、顶层旧字段=首项镜像（回放兼容）、挂起期两工具零执行。
func TestMergedApprovalOneCardTwoItems(t *testing.T) {
	_, s, card, callsA, callsB := mergedSetup(t)
	if len(card.Items) != 2 {
		t.Fatalf("一轮双写应聚合一卡两项，实得 %d 项：%+v", len(card.Items), card.Items)
	}
	if card.Items[0].ItemID == card.Items[1].ItemID {
		t.Fatal("两项 item_id 应互异（Resume 按项领决议的依据）")
	}
	tools := map[string]bool{card.Items[0].Tool: true, card.Items[1].Tool: true}
	if !tools["write_a"] || !tools["write_b"] {
		t.Fatalf("两项应覆盖两工具，实得 %+v", card.Items)
	}
	if card.Tool != card.Items[0].Tool {
		t.Fatalf("顶层旧字段应=首项镜像（旧回放兼容），%q vs %q", card.Tool, card.Items[0].Tool)
	}
	if *callsA != 0 || *callsB != 0 {
		t.Fatal("挂起期不得执行写工具")
	}
	if s.StateOf() != session.StatePendingApproval {
		t.Fatalf("应挂起，实得 %s", s.StateOf())
	}
	if ids := s.PendingItems(); len(ids) != 2 {
		t.Fatalf("挂起项清单应两项（超时批量拒依据），实得 %v", ids)
	}
}

// TestMergedApprovalBatchApprove 批量批准：一卡两项一次决议 → 两工具都执行、
// 正常收口。
func TestMergedApprovalBatchApprove(t *testing.T) {
	m, s, card, callsA, callsB := mergedSetup(t)
	for _, it := range card.Items {
		s.SetDecisionFor(it.ItemID, contract.ApprovalDecision{Approve: true})
	}
	m.Resume(context.Background(), s, func(session.Event) {})
	waitTitleFlight(t, s)
	if *callsA != 1 || *callsB != 1 {
		t.Fatalf("批量批准后两工具都应执行，实得 a=%d b=%d", *callsA, *callsB)
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("批量批准应正常收口，终态 %s", s.StateOf())
	}
}

// TestMergedApprovalApproveOneRejectOne 批一拒一：各决议按项生效——批准者
// 执行、拒绝者不执行且回喂 disapproved、父运行不炸。
func TestMergedApprovalApproveOneRejectOne(t *testing.T) {
	m, s, card, callsA, callsB := mergedSetup(t)
	approved := ""
	for _, it := range card.Items {
		if it.Tool == "write_a" {
			approved = it.ItemID
			s.SetDecisionFor(it.ItemID, contract.ApprovalDecision{Approve: true})
		} else {
			s.SetDecisionFor(it.ItemID, contract.ApprovalDecision{Approve: false, Reason: "乙不要"})
		}
	}
	if approved == "" {
		t.Fatal("卡上应含 write_a 项")
	}
	m.Resume(context.Background(), s, func(session.Event) {})
	waitTitleFlight(t, s)
	if *callsA != 1 || *callsB != 0 {
		t.Fatalf("批一拒一应各自生效，实得 a=%d b=%d", *callsA, *callsB)
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("批一拒一后父应正常收口，终态 %s", s.StateOf())
	}
	// 拒绝回喂：父历史末轮 tool 结果含 disapproved（模型可见可调整）
	seen := false
	for _, msg := range s.CloneHistory() {
		if msg.Role == schema.Tool && strings.Contains(msg.Content, "disapproved") {
			seen = true
		}
	}
	if !seen {
		t.Fatal("拒绝项应以 disapproved 工具结果回喂父模型")
	}
}

// TestMergedApprovalTimeoutRejectsAll 超时批量拒（fail-closed 批形态）：
// 到点无决议 → 全项拒绝落决议表 → 终态 ended、零执行、超时事件入流。
func TestMergedApprovalTimeoutRejectsAll(t *testing.T) {
	old := approvalTimeout
	approvalTimeout = 100 * time.Millisecond
	t.Cleanup(func() { approvalTimeout = old })

	m, s, card, callsA, callsB := mergedSetup(t)
	_ = m
	_ = card
	deadline := time.Now().Add(2 * time.Second)
	for s.StateOf() == session.StatePendingApproval && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("超时应自动拒绝终态：%s", s.StateOf())
	}
	if *callsA != 0 || *callsB != 0 {
		t.Fatalf("超时全项拒绝不得执行，实得 a=%d b=%d", *callsA, *callsB)
	}
	waitTitleFlight(t, s)
	d, ok := m.Registry().Detail(s.SID, 0)
	if !ok {
		t.Fatal("详情应可读")
	}
	for _, ev := range d.Events {
		if ev.Event == contract.EvApprovalTimeout {
			return // 通过
		}
	}
	t.Fatalf("回放应含 approval_timeout：%+v", d.Events)
}
