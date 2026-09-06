package ui

// M1 只读回放面回归：列表/详情/事件快照（golden 同款形态）/SSE live
//（先订阅后快照按 ID 去重——无洞无缝）/鉴权缝（403 裁剪）/鉴权 nil 零变化。
// 事件经 s.Record 直录（读面测试不需要跑引擎轮）。

import (
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
	"github.com/jumeng/einox/internal/tstore"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
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
