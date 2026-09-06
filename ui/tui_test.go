package ui

// TUI 交互模型回归（M3）：过滤/光标有界与视窗跟随、live 追加去重跟尾、
// Kind 摘要（T6 身份字段）与域分类、未知 Kind 软降级、渲染级回归（安全审查
// 2026-09-06：检查器切片越界曾在「渲染不测」的盲区下带病发布——out 改注入
// 面后渲染可测）。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
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

// TestTuiEventVocabularyReconciliation 事件词表对账（审查 P1-3）：TUI 与
// web 回放页（ui/static/index.html 的 DOMAINS/SUMMARIES）两份手写实现维护
// 同一份词表，曾各自漂移——本测试锁 Go 侧口径，contract 新增事件后 TUI 漏配
// 即失败。Go 反射无法枚举包级常量，清单手工维护：与 contract/event.go 的
// Ev* 常量全集同步（新增常量须同步补本清单、tuiDomainOf/tuiSummary 及
// index.html 两侧词表）；计数守卫逼清单随常量增删同步。
func TestTuiEventVocabularyReconciliation(t *testing.T) {
	doms := map[string]tuiDomain{ // 域分类口径 = index.html DOMAINS 同款
		contract.EvTextDelta: domGen, contract.EvThinkingDelta: domGen, contract.EvUsage: domGen,
		contract.EvToolCall: domTool, contract.EvToolResult: domTool,
		contract.EvApprovalRequest: domHitl, contract.EvApprovalDecision: domHitl, contract.EvApprovalTimeout: domHitl,
		contract.EvAskRequest: domHitl, contract.EvAskDecision: domHitl, contract.EvAskTimeout: domHitl, contract.EvAskIgnored: domHitl,
		contract.EvPlanRequest: domHitl, contract.EvPlanDecision: domHitl, contract.EvPlanTimeout: domHitl,
		contract.EvSteerQueued: domSteer, contract.EvSteerUpdated: domSteer, contract.EvSteerRemoved: domSteer,
		contract.EvSteerInjected: domSteer, contract.EvSteerReordered: domSteer,
		contract.EvNotifyQueued: domSteer, contract.EvNotifyInjected: domSteer,
		contract.EvUserMessage: domSteer, contract.EvParticipantUpdate: domSteer,
		contract.EvTodoUpdate: domProc, contract.EvHarnessNote: domProc, contract.EvSubAgent: domProc,
		contract.EvModelChange: domProc, contract.EvTransportRetry: domProc,
		contract.EvSessionEnd: domEnd, contract.EvError: domEnd, contract.EvInterrupted: domEnd,
	}
	if len(doms) != 32 { // contract/event.go 现有 Ev* 常量恰 32 个；增删即失败逼同步
		t.Fatalf("词表清单应 32 项（与 contract/event.go 的 Ev* 常量数一致），实得 %d", len(doms))
	}
	for kind, dom := range doms {
		if got := tuiDomainOf(kind); got != dom {
			t.Errorf("词表事件 %s 域分类应 %v 实 %v", kind, dom, got)
		}
		if s := tuiSummary(mkev(1, kind, nil)); s == "（未知事件——软降级通用行）" {
			t.Errorf("词表事件 %s 摘要落软降级通用行（tuiSummary 需补）", kind)
		}
	}
	// 真正未知的软降级路径仍可达（非词表名——封闭词表外的兜底行为不变）
	if tuiDomainOf("future_kind") != domUnknown {
		t.Fatal("非词表名域应为 unknown")
	}
}

// TestTuiRenderInspectorSmallPayload 渲染级回归（安全审查 2026-09-06 P1）：
// 24 行终端开检查器、载荷行数少于可用高度——修复前 strings.Split(...)[:9]
// 越界 panic（[:cap>len]）。
func TestTuiRenderInspectorSmallPayload(t *testing.T) {
	reg := session.NewRegistry(tstore.New(t.TempDir()))
	s := reg.Create("张三", "任务", "auto", contract.UserPrefs{})
	s.Record(contract.EvTextDelta, contract.Delta{Delta: "hi"}) // 载荷 3 行 < 可用 9 行
	var buf strings.Builder
	app := &tuiApp{s: s, width: 80, height: 24,
		out: func(str string) { buf.WriteString(str) }}
	app.model.events = s.SnapshotEvents()
	app.model.inspOpen = true
	app.render() // 修复前此处 panic：slice bounds out of range
	if !strings.Contains(buf.String(), "#1") {
		t.Fatal("检查器应渲染选中事件头")
	}
	// 小终端（8 行）同样安全
	small := &tuiApp{s: s, width: 40, height: 8, out: func(string) {}}
	small.model.events = s.SnapshotEvents()
	small.model.inspOpen = true
	small.render()
}

// TestTuiFilterUTF8AcrossReads 过滤输入跨读 UTF-8（安全审查 2026-09-06）：
// 键盘泵每次读 ≤8 字节，CJK rune 被切在边界时逐字节拼接产替换符——现按
// rune 消费（不完整前缀留待下一批）；退格按 rune 截尾。
func TestTuiFilterUTF8AcrossReads(t *testing.T) {
	app := &tuiApp{width: 80, height: 24, out: func(string) {}}
	app.model.filterIn = true
	app.key([]byte{0xe4}) // 「中」首字节（不完整前缀）
	app.key([]byte{0xb8}) // 次字节
	if app.model.filter != "" {
		t.Fatalf("不完整前缀不应进过滤器：%q", app.model.filter)
	}
	app.key([]byte{0xad}) // 尾字节 → 「中」
	if app.model.filter != "中" {
		t.Fatalf("跨读拼出完整 rune 应进过滤器：%q", app.model.filter)
	}
	app.key([]byte{0xe4, 0xb8, 0xad, 0xff}) // 完整 rune + 坏字节
	if app.model.filter != "中中" {
		t.Fatalf("坏字节应丢弃、完整 rune 照收：%q", app.model.filter)
	}
	app.key([]byte{0x7f}) // 退格按 rune 截
	if app.model.filter != "中" {
		t.Fatalf("退格应按 rune 截尾：%q", app.model.filter)
	}
}
