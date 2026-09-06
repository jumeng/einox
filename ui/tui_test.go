package ui

// TUI 交互模型回归（M3）：过滤/光标有界与视窗跟随、live 追加去重跟尾、
// Kind 摘要（T6 身份字段）与域分类、未知 Kind 软降级。渲染与终端 IO 不测
//（人工验收面——SSH 步进）。

import (
	"encoding/json"
	"testing"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
)

func mkev(id int, kind string, data any) session.Event {
	if data == nil {
		data = map[string]any{}
	}
	return session.Event{ID: id, Event: kind, Data: data}
}

func wireData(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTuiModelFilterAndCursor(t *testing.T) {
	mo := tuiModel{events: []session.Event{
		mkev(1, contract.EvUserMessage, nil),
		mkev(2, contract.EvTextDelta, nil),
		mkev(3, contract.EvToolCall, nil),
		mkev(4, contract.EvTextDelta, nil),
	}}
	if got := len(mo.matched()); got != 4 {
		t.Fatalf("空过滤应全量，实得 %d", got)
	}
	mo.filter = "text_delta"
	if got := len(mo.matched()); got != 2 {
		t.Fatalf("过滤应剩 2，实得 %d", got)
	}
	mo.move(10, 10)
	if mo.cursor != 1 { // 0 基：末位索引 1
		t.Fatalf("光标应钳到末位，实得 %d", mo.cursor)
	}
	mo.move(-10, 10)
	if mo.cursor != 0 {
		t.Fatalf("光标应钳到首位，实得 %d", mo.cursor)
	}
	// 视窗跟随：高度 1，move(2) → offset 跟到末行
	mo.move(2, 1)
	if mo.offset != 1 || mo.cursor != 1 {
		t.Fatalf("视窗应跟随（offset=1 cursor=1），实得 %d/%d", mo.offset, mo.cursor)
	}
}

func TestTuiModelAppendDedupeAndTail(t *testing.T) {
	mo := tuiModel{events: []session.Event{mkev(1, "a", nil)}, live: true}
	mo.append(mkev(1, "a", nil), 10) // 重复 ID 丢弃
	if len(mo.events) != 1 {
		t.Fatalf("重复 ID 应丢弃，实得 %d", len(mo.events))
	}
	mo.append(mkev(2, "b", nil), 10)
	if len(mo.events) != 2 || mo.cursor != 1 {
		t.Fatalf("live 追加应跟尾（cursor=1），实得 cursor=%d len=%d", mo.cursor, len(mo.events))
	}
	mo.live = false
	mo.cursor = 0
	mo.append(mkev(3, "c", nil), 10)
	if mo.cursor != 0 {
		t.Fatalf("非 live 追加不应动光标，实得 %d", mo.cursor)
	}
}

func TestTuiSummaryAndDomain(t *testing.T) {
	// T6 身份字段渲染（与 web 回放页同口径）
	if got := tuiSummary(mkev(1, contract.EvUserMessage,
		wireData(t, contract.UserMsg{Text: "你好", SpeakerName: "张三"}))); got != "消息（张三）：你好" {
		t.Fatalf("用户消息摘要应含署名：%q", got)
	}
	if got := tuiSummary(mkev(2, contract.EvApprovalRequest,
		wireData(t, contract.ApprovalReq{Tool: "write_tool", RequesterName: "张三", TargetName: "值班主任"}))); got != "审批：write_tool（张三 的动作） → 问 值班主任" {
		t.Fatalf("审批摘要应含 Requester/Target：%q", got)
	}
	if got := tuiSummary(mkev(3, contract.EvApprovalDecision,
		wireData(t, contract.DecisionOut{Approve: true, DeciderName: "李四"}))); got != "批准（李四）" {
		t.Fatalf("决议摘要应含 Decider：%q", got)
	}
	// 域分类 + 未知 Kind 软降级
	for kind, dom := range map[string]tuiDomain{
		contract.EvTextDelta: domGen, contract.EvToolCall: domTool,
		contract.EvApprovalRequest: domHitl, contract.EvSteerQueued: domSteer,
		contract.EvHarnessNote: domProc, contract.EvSessionEnd: domEnd,
	} {
		if tuiDomainOf(kind) != dom {
			t.Fatalf("域分类错误：%s", kind)
		}
	}
	if got := tuiSummary(mkev(9, "future_kind", nil)); got != "（未知事件——软降级通用行）" {
		t.Fatalf("未知 Kind 应软降级：%q", got)
	}
	if tuiDomainOf("future_kind") != domUnknown {
		t.Fatal("未知 Kind 域应为 unknown")
	}
}

// TestTuiSummaryTypedPayload 直喂类型化载荷（审查 P1-1 回归——活会话
// Record 落的是结构体，原实现仅 map 断言使全部摘要空白；wireData 夹具
// 曾掩蔽此缺陷）。
func TestTuiSummaryTypedPayload(t *testing.T) {
	if got := tuiSummary(session.Event{ID: 1, Event: "user_message",
		Data: contract.UserMsg{Text: "你好", SpeakerName: "张三"}}); got != "消息（张三）：你好" {
		t.Fatalf("typed 直喂摘要应工作：%q", got)
	}
	if got := tuiSummary(session.Event{ID: 2, Event: "session_end",
		Data: contract.SessionEnd{HistLen: 7}}); got != "轮末（历史 7 条）" {
		t.Fatalf("typed 直喂摘要应工作：%q", got)
	}
	if got := tuiSummary(session.Event{ID: 3, Event: "tool_call",
		Data: contract.ToolCall{Tool: "write_tool"}}); got != "调用 write_tool" {
		t.Fatalf("typed 直喂摘要应工作：%q", got)
	}
}
