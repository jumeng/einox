package engine

// T6 多参与者身份链回归。六环判别法（选型报告 3.1.1 通用判别法）：「张三让
// agent 删文件」链路上每一环都知道张三——①user_message 事件 ②审批卡
// Requester ③决议回执 Decider ④工具审计 Operator + 装配面 SessionBrief
// ⑤模型输入「张三：」前缀 ⑥transcript 署名。配套：输入分段（异人各自成条、
// 同人合并）、名册首见登记/在册更新、单用户零变化（全字段 omitempty——
// 既有全量测试即证）。

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// TestParticipantSixRingChain 张三链路六环贯通：Owner（李四，存储归属）与
// 当轮说话人（张三）分离——正是多参与者语义的被测点。
func TestParticipantSixRingChain(t *testing.T) {
	var operatorSeen string                   // ④工具 ctx 的 Operator
	var briefSpeaker, briefSpeakerName string // ④装配面 SessionBrief.TurnSpeaker
	wt, _ := tools.InferTool("write_tool", "写桩", func(ctx context.Context, _ struct{}) (map[string]any, error) {
		operatorSeen = contract.OperatorOf(ctx)
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("cw", "write_tool", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.Tools = func(b SessionBrief) []contract.Tool {
			briefSpeaker, briefSpeakerName = b.TurnSpeakerID, b.TurnSpeakerName
			return []contract.Tool{wt}
		}
		o.Approval = hitl.ApprovalConfig{WriteTools: map[string]bool{"write_tool": true}}
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("u_li", "审批", "manual", contract.UserPrefs{Model: "p/m"})
	s.UpsertParticipant(contract.Participant{ID: "u_zhang", Name: "张三"}) // 名册登记（joined 落流）
	s.SetTurnActor(&contract.Participant{ID: "u_zhang", Name: "张三"})
	s.SetState(session.StateRunning)

	var card *contract.ApprovalReq
	m.Run(context.Background(), s, "删掉临时文件", nil, func(ev session.Event) {
		if ev.Event == contract.EvApprovalRequest {
			req := ev.Data.(contract.ApprovalReq)
			card = &req
		}
	})
	t.Cleanup(func() { stopApprovalTimer(s.SID) })
	if card == nil {
		t.Fatal("manual 档写工具应挂起发审批卡")
	}
	// ②审批卡：Requester = 当轮说话人（张三），非 Owner（李四）
	if card.RequesterID != "u_zhang" || card.RequesterName != "张三" {
		t.Fatalf("审批卡 Requester 应为张三，实得 %s/%s", card.RequesterID, card.RequesterName)
	}
	// ③决议（李四批）→ 回执 Decider
	d := contract.ApprovalDecision{Approve: true, DeciderID: "u_li", DeciderName: "李四"}
	s.SetDecisionFor(card.Items[0].ItemID, d)
	s.RecordDecision(card.ApprovalID, d)
	m.Resume(context.Background(), s, func(session.Event) {})
	waitTitleFlight(t, s)

	var sawUserMsg, sawDecider, sawJoined bool
	for _, ev := range s.SnapshotEvents() {
		switch ev.Event {
		case contract.EvUserMessage: // ①事件：谁说的
			if um := ev.Data.(contract.UserMsg); um.SpeakerID == "u_zhang" && um.SpeakerName == "张三" {
				sawUserMsg = true
			}
		case contract.EvApprovalDecision: // ③回执：谁批的
			if out := ev.Data.(contract.DecisionOut); out.DeciderID == "u_li" && out.DeciderName == "李四" {
				sawDecider = true
			}
		case contract.EvParticipantUpdate: // 名册：首见 joined
			if pe := ev.Data.(contract.ParticipantEvent); pe.Kind == "joined" && pe.Participant.ID == "u_zhang" {
				sawJoined = true
			}
		}
	}
	if !sawUserMsg {
		t.Fatal("①user_message 事件应带张三署名")
	}
	if !sawDecider {
		t.Fatal("③决议回执应带李四 Decider")
	}
	if !sawJoined {
		t.Fatal("名册首见应落 participant_update(joined)")
	}
	// ④工具审计：Operator = 张三（非 Owner 李四）；装配面 SessionBrief 同
	if operatorSeen != "u_zhang" {
		t.Fatalf("④工具 ctx Operator 应为张三，实得 %q", operatorSeen)
	}
	if briefSpeaker != "u_zhang" || briefSpeakerName != "张三" {
		t.Fatalf("④SessionBrief.TurnSpeaker 应为张三，实得 %s/%s", briefSpeaker, briefSpeakerName)
	}
	// ⑤模型输入：「张三：」前缀（历史与当前轮同律）
	prefixed := false
	for _, in := range fm.inputs {
		for _, msg := range in {
			if strings.HasPrefix(msg.Content, "张三：删掉临时文件") {
				prefixed = true
			}
		}
	}
	if !prefixed {
		t.Fatal("⑤模型输入应含「张三：」前缀")
	}
	// 历史原文不带前缀（存储保真——Extra 携署名）
	for _, msg := range s.CloneHistory() {
		if msg.Role == schema.User && strings.HasPrefix(msg.Content, "张三：") {
			t.Fatal("历史原文不应带前缀（投影只发生在模型输入副本）")
		}
	}
}

// TestParticipantInputSegments 输入分段：异人各自成条、同人相邻合并、无署名
// 退化为单段（= 旧单条合并形态）。
func TestParticipantInputSegments(t *testing.T) {
	queued := []session.QueuedMsg{
		{ID: "q1", Text: "默认用户先说"},
		{ID: "q2", Text: "王五说", SpeakerID: "u_wang", SpeakerName: "王五"},
		{ID: "q3", Text: "王五补充", SpeakerID: "u_wang", SpeakerName: "王五"},
	}
	segs := inputSegments(queued, "直接输入", nil, &contract.Participant{ID: "u_zhang", Name: "张三"})
	if len(segs) != 3 {
		t.Fatalf("应 3 段（默认/王五合并/张三），实得 %d", len(segs))
	}
	if segs[0].speaker != nil || len(segs[0].texts) != 1 {
		t.Fatalf("首段应无署名单条：%+v", segs[0])
	}
	if segs[1].speaker == nil || segs[1].speaker.ID != "u_wang" || len(segs[1].texts) != 2 {
		t.Fatalf("王五两条应合并成段：%+v", segs[1])
	}
	if segs[2].speaker == nil || segs[2].speaker.ID != "u_zhang" {
		t.Fatalf("末段应为张三：%+v", segs[2])
	}
	// 单用户退化：全无署名 = 单段
	flat := inputSegments(queued[:1], "直接输入", nil, nil)
	if len(flat) != 1 || flat[0].speaker != nil || len(flat[0].texts) != 2 {
		t.Fatalf("全无署名应退化为单段两文本（旧形态），实得 %+v", flat)
	}
}

// TestParticipantRosterUpsert 名册登记：首见 joined、重登记无变更零事件、
// 改名 updated；ID 空 refuse。
func TestParticipantRosterUpsert(t *testing.T) {
	m := newSeamManager(t, nil)
	s := m.Registry().Create("u_li", "群", "plan", contract.UserPrefs{Model: "p/m"})
	if got := s.UpsertParticipant(contract.Participant{ID: "u_zhang", Name: "张三"}); got != "joined" {
		t.Fatalf("首见应 joined，实得 %q", got)
	}
	if got := s.UpsertParticipant(contract.Participant{ID: "u_zhang", Name: "张三"}); got != "" {
		t.Fatalf("无变更应零事件，实得 %q", got)
	}
	if got := s.UpsertParticipant(contract.Participant{ID: "u_zhang", Name: "张三三"}); got != "updated" {
		t.Fatalf("改名应 updated，实得 %q", got)
	}
	if got := s.UpsertParticipant(contract.Participant{}); got != "" {
		t.Fatalf("空 ID 应拒，实得 %q", got)
	}
	roster := s.ParticipantsOf()
	if len(roster) != 1 || roster[0].Name != "张三三" {
		t.Fatalf("名册应恰一项且改名生效：%+v", roster)
	}
	// 事件面：joined + updated 两事件
	kinds := map[string]bool{}
	for _, ev := range s.SnapshotEvents() {
		if pe, ok := ev.Data.(contract.ParticipantEvent); ok {
			kinds[pe.Kind] = true
		}
	}
	if !kinds["joined"] || !kinds["updated"] {
		t.Fatalf("事件面应含 joined+updated：%v", kinds)
	}
}

// TestParticipantTranscriptSignature transcript 署名（⑥环）：user 消息带
// Extra 署名时存档头「## user（张三）」；无署名零变化（纯「## user」）。
func TestParticipantTranscriptSignature(t *testing.T) {
	m, st := newRunManager(t, nil, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return &scriptedModel{}, nil
	})
	s := m.Registry().Create("u_li", "档", "plan", contract.UserPrefs{Model: "p/m"})
	signed := schema.UserMessage("删掉临时文件")
	signed.Extra = map[string]any{speakerExtraID: "u_zhang", speakerExtraName: "张三"}
	writeTranscript(m.Registry().Store(), s, []*schema.Message{
		signed,
		schema.AssistantMessage("好的", nil),
		schema.UserMessage("无署名消息"),
	})
	raw, ok := st.ReadUserTreeFile("u_li", "sessions/"+s.SID+"/spill/transcript-"+s.SID+".txt")
	if !ok {
		t.Fatal("transcript 应落盘")
	}
	text := string(raw)
	if !strings.Contains(text, "## user（张三）") {
		t.Fatalf("⑥署名 user 头缺失：\n%s", text)
	}
	if !strings.Contains(text, "## assistant\n") || strings.Count(text, "## user（张三）") != 1 {
		t.Fatalf("无署名消息应保持纯「## user」头（零变化）：\n%s", text)
	}
}
