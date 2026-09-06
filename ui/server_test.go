package ui

// M1 只读回放面回归：列表/详情/事件快照（golden 同款形态）/SSE live
//（先订阅后快照按 ID 去重——无洞无缝）/鉴权缝（403 裁剪）/鉴权 nil 零变化。
// 事件经 s.Record 直录（读面测试不需要跑引擎轮）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/checkpoint"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/engine"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/internal/tstore"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/llmtest"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// newTestServer 最小引擎装配（读面不触模型）+ 录三事件的会话。
func newTestServer(t *testing.T, cfg Config) (http.Handler, *session.Session) {
	t.Helper()
	st := tstore.New(t.TempDir())
	m, err := engine.NewManager(session.NewRegistry(st), engine.Options{
		Providers: func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{{ID: "m", Input: []string{"text"}, Priority: 100}},
			}}
		},
		Instruction: func(engine.SessionBrief) string { return "test" },
		NewModel: func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return nil, fmt.Errorf("读面测试不触模型")
		},
		CheckPoints: func(operator, sid string) engine.CheckPointStore {
			return checkpoint.NewCheckPointStore(st, operator, sid)
		},
		WorkspaceRoot: func(owner, sid string) string { return st.TmpDir() + "/ws/" + owner + "/" + sid },
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	s := m.Registry().Create("张三", "回放", "plan", contract.UserPrefs{Model: "p/m"})
	s.Record(contract.EvUserMessage, contract.UserMsg{Text: "你好", SpeakerID: "u_zhang", SpeakerName: "张三"})
	s.Record(contract.EvTextDelta, contract.Delta{Delta: "已处理。"})
	s.Record(contract.EvSessionEnd, contract.SessionEnd{Summary: "已处理。", HistLen: 2})
	h, err := New(m, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, s
}

func getJSON(t *testing.T, h http.Handler, path string, out any) *http.Response {
	t.Helper()
	rs := httptest.NewRecorder()
	h.ServeHTTP(rs, httptest.NewRequest(http.MethodGet, path, nil))
	if rs.Code != http.StatusOK {
		t.Fatalf("GET %s 应 200，实得 %d：%s", path, rs.Code, rs.Body.String())
	}
	if out != nil {
		_ = json.Unmarshal(rs.Body.Bytes(), out)
	}
	return rs.Result()
}

func TestListAndDetail(t *testing.T) {
	h, _ := newTestServer(t, Config{})
	var items []session.SessionListItem
	getJSON(t, h, "/api/sessions", &items)
	if len(items) != 1 || items[0].Owner != "张三" || items[0].SID == "" {
		t.Fatalf("列表应含张三一会话：%+v", items)
	}
	var list2 []session.SessionListItem
	getJSON(t, h, "/api/sessions?owner=李四", &list2)
	if len(list2) != 0 {
		t.Fatalf("按 owner 过滤应为空：%+v", list2)
	}
	var d session.SessionDetail
	getJSON(t, h, "/api/sessions/"+items[0].SID, &d)
	if d.SID != items[0].SID || len(d.Events) != 3 {
		t.Fatalf("详情应含全部三事件，实得 %d", len(d.Events))
	}
	var d2 session.SessionDetail
	getJSON(t, h, "/api/sessions/"+items[0].SID+"?since=2", &d2)
	if len(d2.Events) != 1 || d2.Events[0].ID != 3 {
		t.Fatalf("since=2 增量应只剩事件 3：%+v", d2.Events)
	}
}

// TestEventsReplayShape 回放档 = golden 同款形态（{id,event,data} 键序一致，
// T7 基线可直接作本端点夹具）。
func TestEventsReplayShape(t *testing.T) {
	h, s := newTestServer(t, Config{})
	var evs []session.Event
	getJSON(t, h, "/api/sessions/"+s.SID+"/events", &evs)
	if len(evs) != 3 || evs[0].Event != contract.EvUserMessage {
		t.Fatalf("快照应三事件且保序：%+v", evs)
	}
	if evs[0].ID != 1 || evs[2].ID != 3 {
		t.Fatalf("事件 ID 应保序：%+v", evs)
	}
	// wire 形态含 speaker（T6 载荷直通）
	b, _ := json.Marshal(evs[0])
	if !strings.Contains(string(b), `"speaker_name":"张三"`) {
		t.Fatalf("载荷 wire 形态应含 T6 字段：%s", b)
	}
	var tail []session.Event
	getJSON(t, h, "/api/sessions/"+s.SID+"/events?since=2", &tail)
	if len(tail) != 1 {
		t.Fatalf("since 过滤应剩一事件：%+v", tail)
	}
}

// TestSSELive live 档：先快照补齐（含订阅前事件）再实时转发（Record 后到达），
// 按 ID 去重。
func TestSSELive(t *testing.T) {
	h, s := newTestServer(t, Config{})
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/sessions/"+s.SID+"/events?live=1", nil)
	rs, err := srv.Client().Do(req) // http.Client 不做 SSE 半包缓冲——逐块可读
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Body.Close()
	buf := make([]byte, 4096)
	var got strings.Builder
	// 1) 快照段：三事件（data: 行）
	deadline := time.Now().Add(3 * time.Second)
	for strings.Count(got.String(), "data: ") < 3 && time.Now().Before(deadline) {
		n, _ := rs.Body.Read(buf)
		if n > 0 {
			got.Write(buf[:n])
		}
	}
	if strings.Count(got.String(), "data: ") < 3 {
		t.Fatalf("SSE 应先补齐三事件快照：%q", got.String())
	}
	// 2) live 段：Record 新事件应实时到达
	s.Record(contract.EvTextDelta, contract.Delta{Delta: "新到"})
	for !strings.Contains(got.String(), "新到") && time.Now().Before(deadline) {
		n, _ := rs.Body.Read(buf)
		if n > 0 {
			got.Write(buf[:n])
		}
	}
	if !strings.Contains(got.String(), "新到") {
		t.Fatal("live 档应实时转发新事件")
	}
	// 3) 无重复（去重按 ID：快照与订阅重叠段只发一次）
	if c := strings.Count(got.String(), `"event":"session_end"`); c != 1 {
		t.Fatalf("重叠段应按 ID 去重（session_end 恰一次），实得 %d", c)
	}
}

// TestAuthorizeSeam 鉴权缝：403 拦截；owner 从注册表解析（不可 URL 伪造）。
func TestAuthorizeSeam(t *testing.T) {
	h, s := newTestServer(t, Config{
		Authorize: func(r *http.Request, owner, sid string) error {
			if owner != "张三" {
				return fmt.Errorf("会话归属 %q 不可见", owner)
			}
			return nil
		},
	})
	code := func(path string) int {
		rs := httptest.NewRecorder()
		h.ServeHTTP(rs, httptest.NewRequest(http.MethodGet, path, nil))
		return rs.Code
	}
	if c := code("/api/sessions"); c != http.StatusForbidden { // owner 空 → 缝见 ("","") → 拒
		t.Fatalf("列表全量面缝应 403，实得 %d", c)
	}
	if c := code("/api/sessions?owner=张三"); c != http.StatusOK {
		t.Fatalf("张三列表应放行，实得 %d", c)
	}
	if c := code("/api/sessions/" + s.SID); c != http.StatusOK { // owner 从注册表解析（非 URL 参数）
		t.Fatalf("张三会话应放行，实得 %d", c)
	}
	if c := code("/api/sessions/" + s.SID + "/events"); c != http.StatusOK {
		t.Fatalf("张三事件面应放行，实得 %d", c)
	}
	if c := code("/api/sessions/nonexistent"); c != http.StatusNotFound {
		t.Fatalf("不存在应 404，实得 %d", c)
	}
}

// TestStaticPage embed 静态回放页可达（M1 验收链的地板：页面 + 数据面同源装配）。
func TestStaticPage(t *testing.T) {
	h, _ := newTestServer(t, Config{})
	rs := httptest.NewRecorder()
	h.ServeHTTP(rs, httptest.NewRequest(http.MethodGet, "/", nil))
	if rs.Code != http.StatusOK || !strings.Contains(rs.Body.String(), "会话回放") {
		t.Fatalf("根路径应返回回放页，实得 %d", rs.Code)
	}
}

// ── M2 交互面 ─────────────────────────────────────────────────────────────

// newE2EServer 交互链装配：manual 档写工具 + llmtest 剧本（首调写工具调用、
// 续调收口）——审批挂起→决议→续流全链的引擎侧。
func newE2EServer(t *testing.T, cfg Config) (http.Handler, *session.Session) {
	t.Helper()
	st := tstore.New(t.TempDir())
	fm := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.ToolCallSpec{{ID: "t1", Name: "write_tool", Args: "{}"}}},
		llmtest.Turn{Text: "完成"},
	).Factory()
	m, err := engine.NewManager(session.NewRegistry(st), engine.Options{
		Providers: func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{{ID: "m", Input: []string{"text"}, Priority: 100}}}}
		},
		Instruction: func(engine.SessionBrief) string { return "test" },
		Tools: func(engine.SessionBrief) []contract.Tool {
			wt, err := tools.InferTool("write_tool", "写桩", func(context.Context, struct{}) (map[string]any, error) {
				return map[string]any{"ok": true}, nil
			})
			if err != nil {
				t.Fatalf("InferTool: %v", err)
			}
			return []contract.Tool{wt}
		},
		NewModel: func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm(context.Background(), llm.ProviderSpec{}, llm.ModelSpec{}, "")
		},
		CheckPoints: func(operator, sid string) engine.CheckPointStore {
			return checkpoint.NewCheckPointStore(st, operator, sid)
		},
		WorkspaceRoot: func(owner, sid string) string { return st.TmpDir() + "/ws/" + owner + "/" + sid },
		Approval:      hitl.ApprovalConfig{WriteTools: map[string]bool{"write_tool": true}},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	s := m.Registry().Create("张三", "交互", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateEnded)
	h, err := New(m, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, s
}

// decodeAs wire 往返载荷重解码（Data 经 JSON 后是 map，非具体契约类型）。
func decodeAs(t *testing.T, data any, target any) {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("载荷重编码失败：%v", err)
	}
	if err := json.Unmarshal(b, target); err != nil {
		t.Fatalf("载荷重解码失败：%v", err)
	}
}

// postJSON 控制端点便捷面。
func postJSON(t *testing.T, h http.Handler, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	rs := httptest.NewRecorder()
	h.ServeHTTP(rs, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)))
	var out map[string]any
	_ = json.Unmarshal(rs.Body.Bytes(), &out)
	return rs, out
}

// pollEvents 轮询事件面直到条件满足（带界）。
func pollEvents(t *testing.T, h http.Handler, sid string, cond func([]session.Event) bool, what string) []session.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var evs []session.Event
		getJSON(t, h, "/api/sessions/"+sid+"/events", &evs)
		if cond(evs) {
			return evs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待超时：" + what)
	return nil
}

// TestControlApprovalE2E M2 验收：起轮（带说话人）→ 审批挂起 → 页面决议
// （带 decider）→ 续流收束 → 决议回执带 decider（T6 链一致）。
func TestControlApprovalE2E(t *testing.T) {
	h, s := newE2EServer(t, Config{})

	// 起轮：张三说话（首见登记名册 + 当轮说话人）
	rs, out := postJSON(t, h, "/api/sessions/"+s.SID+"/run", runReq{
		Text: "写一下", SpeakerID: "u_zhang", SpeakerName: "张三"})
	if rs.Code != http.StatusOK || out["started"] != true {
		t.Fatalf("起轮应 started，实得 %d %v", rs.Code, out)
	}
	// 挂起：approval_request 到达（Requester=张三）
	evs := pollEvents(t, h, s.SID, func(es []session.Event) bool {
		for _, e := range es {
			if e.Event == contract.EvApprovalRequest {
				return true
			}
		}
		return false
	}, "审批挂起")
	var req contract.ApprovalReq
	for _, e := range evs {
		if e.Event == contract.EvApprovalRequest {
			decodeAs(t, e.Data, &req) // wire 往返后 Data 是 map——重解码取载荷
		}
	}
	if req.RequesterID != "u_zhang" {
		t.Fatalf("审批卡 Requester 应为张三，实得 %q", req.RequesterID)
	}
	// 运行中再发：转排队
	rs, out = postJSON(t, h, "/api/sessions/"+s.SID+"/run", runReq{Text: "补充"})
	if rs.Code != http.StatusOK || out["queued"] != true {
		t.Fatalf("挂起期应转排队，实得 %d %v", rs.Code, out)
	}
	// 决议：李四点批（decider 落链）
	rs, _ = postJSON(t, h, "/api/sessions/"+s.SID+"/approve", approveReq{
		Approve: true, ItemID: req.Items[0].ItemID, DeciderID: "u_li", DeciderName: "李四"})
	if rs.Code != http.StatusOK {
		t.Fatalf("决议应放行，实得 %d：%s", rs.Code, rs.Body.String())
	}
	// 续流收束：session_end + 决议回执带 decider
	evs = pollEvents(t, h, s.SID, func(es []session.Event) bool {
		var end, dec bool
		for _, e := range es {
			if e.Event == contract.EvSessionEnd {
				end = true
			}
			if e.Event == contract.EvApprovalDecision {
				var d contract.DecisionOut
				decodeAs(t, e.Data, &d)
				if d.DeciderID == "u_li" {
					dec = true
				}
			}
		}
		return end && dec
	}, "续流收束+decider 回执")
	// 名册：张三已登记（run 首见）
	var ps []contract.Participant
	getJSON(t, h, "/api/sessions/"+s.SID+"/participants", &ps)
	if len(ps) != 1 || ps[0].ID != "u_zhang" {
		t.Fatalf("名册应含张三：%+v", ps)
	}
}

// TestApproveIdempotentLate 迟到决议 409（幂等——前端当已处理）。
func TestApproveIdempotentLate(t *testing.T) {
	h, s := newE2EServer(t, Config{})
	s.SetState(session.StateEnded) // 无挂起
	rs, _ := postJSON(t, h, "/api/sessions/"+s.SID+"/approve", approveReq{Approve: true})
	if rs.Code != http.StatusConflict {
		t.Fatalf("无挂起决议应 409，实得 %d", rs.Code)
	}
}

// TestParticipantUpsert 名册端点：登记→查询→事件面 participant_update。
func TestParticipantUpsert(t *testing.T) {
	h, s := newTestServer(t, Config{})
	rs, out := postJSON(t, h, "/api/sessions/"+s.SID+"/participants",
		map[string]string{"id": "u_wang", "name": "王五"})
	if rs.Code != http.StatusOK || out["kind"] != "joined" {
		t.Fatalf("首见应 joined：%d %v", rs.Code, out)
	}
	var ps []contract.Participant
	getJSON(t, h, "/api/sessions/"+s.SID+"/participants", &ps)
	if len(ps) != 1 || ps[0].Name != "王五" {
		t.Fatalf("名册应含王五：%+v", ps)
	}
	var evs []session.Event
	getJSON(t, h, "/api/sessions/"+s.SID+"/events", &evs)
	found := false
	for _, e := range evs {
		if e.Event == contract.EvParticipantUpdate {
			found = true
		}
	}
	if !found {
		t.Fatal("名册变更应落 participant_update 事件")
	}
}

// TestControlAuthorize 控制面鉴权缝：403 拦截写操作。
func TestControlAuthorize(t *testing.T) {
	h, s := newE2EServer(t, Config{
		Authorize: func(r *http.Request, owner, sid string) error {
			return fmt.Errorf("只读访客")
		},
	})
	for _, path := range []string{"/run", "/resume", "/stop", "/approve", "/answer"} {
		rs, _ := postJSON(t, h, "/api/sessions/"+s.SID+path, map[string]any{})
		if rs.Code != http.StatusForbidden {
			t.Fatalf("POST %s 应 403，实得 %d", path, rs.Code)
		}
	}
}

// TestReplayOrderFidelity 回放端点序保真（M1 验收强化——大量事件经端点
// 回放，ID 连续升序且事件名逐位保序：可完整步进的形式化判据）。
func TestReplayOrderFidelity(t *testing.T) {
	h, s := newTestServer(t, Config{})
	kinds := []string{contract.EvUserMessage, contract.EvTextDelta, contract.EvToolCall,
		contract.EvToolResult, contract.EvHarnessNote, contract.EvSessionEnd, "participant_update"}
	for i := 0; i < 42; i++ {
		s.Record(kinds[i%len(kinds)], map[string]any{"i": i})
	}
	var evs []session.Event
	getJSON(t, h, "/api/sessions/"+s.SID+"/events", &evs)
	if len(evs) != 45 { // 3 既有 + 42
		t.Fatalf("事件数应 45，实得 %d", len(evs))
	}
	for i, ev := range evs {
		if ev.ID != i+1 {
			t.Fatalf("ID 应连续升序：位 %d 实得 #%d", i, ev.ID)
		}
		if i > 2 && ev.Event != kinds[(i-3)%len(kinds)] {
			t.Fatalf("事件名保序破坏：位 %d 实得 %s", i, ev.Event)
		}
	}
}

// TestRunClearsStaleTurnActor 跨轮清零（审查 P1-2 回归）：张三轮后匿名起轮，
// user_message 不得继承张三署名。
func TestRunClearsStaleTurnActor(t *testing.T) {
	h, s := newE2EServer(t, Config{})
	postJSON(t, h, "/api/sessions/"+s.SID+"/run", runReq{
		Text: "第一轮", SpeakerID: "u_zhang", SpeakerName: "张三"})
	// 首轮：manual 档写工具 → 挂起 → 决议 → 收束（剧本首轮必走审批）
	evs1 := pollEvents(t, h, s.SID, func(es []session.Event) bool {
		for _, e := range es {
			if e.Event == contract.EvApprovalRequest {
				return true
			}
		}
		return false
	}, "首轮挂起")
	var req1 contract.ApprovalReq
	for _, e := range evs1 {
		if e.Event == contract.EvApprovalRequest {
			decodeAs(t, e.Data, &req1)
		}
	}
	postJSON(t, h, "/api/sessions/"+s.SID+"/approve", approveReq{
		Approve: true, ItemID: req1.Items[0].ItemID, DeciderID: "u_li", DeciderName: "李四"})
	_ = pollEvents(t, h, s.SID, func(es []session.Event) bool {
		for _, e := range es {
			if e.Event == contract.EvSessionEnd {
				return true
			}
		}
		return false
	}, "首轮收束")
	// 匿名第二轮：不携带 speaker → 须清陈旧归属
	postJSON(t, h, "/api/sessions/"+s.SID+"/run", runReq{Text: "第二轮"})
	evs := pollEvents(t, h, s.SID, func(es []session.Event) bool {
		for _, e := range es {
			if e.Event == contract.EvUserMessage && e.ID > 3 {
				return true
			}
		}
		return false
	}, "第二轮 user_message")
	for _, e := range evs {
		if e.Event == contract.EvUserMessage && e.ID > 3 {
			var um contract.UserMsg
			decodeAs(t, e.Data, &um)
			if um.SpeakerID == "u_zhang" {
				t.Fatal("匿名轮不得继承张三署名（turnActor 未清）")
			}
		}
	}
}
