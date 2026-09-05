package engine

// ToolHooks 回归：Pre 否决（信封回喂不终止轮）/ Post 观察载荷 / panic
// 防护 / nil 钩子零变化 / 真实运行挂点。失败语义见 findings/2026-09-05。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

type hookProbeTool struct {
	name    string
	invoked int
}

func (p *hookProbeTool) Info() *contract.ToolInfo {
	return &contract.ToolInfo{Name: p.name, Desc: "探针", Behavior: contract.BehaviorRead}
}
func (p *hookProbeTool) Invoke(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	p.invoked++
	return json.RawMessage(`{"ok":true,"v":"ran"}`), nil
}

func TestHookFacePreDenyAndPostObserve(t *testing.T) {
	tool := &hookProbeTool{name: "probe"}
	var postGot ToolHookResult
	m := &Manager{Opt: Options{Hooks: &ToolHooks{
		Pre: func(c ToolHookCall) error {
			if c.Name != "probe" || string(c.Args) != `{"x":1}` || c.Sess.SID != "s1" {
				t.Errorf("Pre 载荷：name=%s args=%s sid=%s", c.Name, c.Args, c.Sess.SID)
			}
			return errors.New("策略拦截")
		},
		Post: func(c ToolHookCall, r ToolHookResult) { postGot = r },
	}}}
	face := m.hookFace([]contract.Tool{tool}, SessionBrief{SID: "s1"})
	res, err := face[0].Invoke(context.Background(), json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("否决应信封回喂而非 Go error（轮不终止）：%v", err)
	}
	if tool.invoked != 0 {
		t.Fatal("否决后工具不得执行")
	}
	if !strings.Contains(string(res), `"ok":false`) || !strings.Contains(string(res), "pre-hook") {
		t.Fatalf("否决信封：%s", res)
	}
	if !strings.Contains(string(postGot.Result), "pre-hook") {
		t.Fatalf("Post 应收否决信封：%s", postGot.Result)
	}
}

func TestHookFacePostPayloadAndPanicGuard(t *testing.T) {
	tool := &hookProbeTool{name: "probe"}

	// Pre panic → fail-closed 转拒绝
	m := &Manager{Opt: Options{Hooks: &ToolHooks{
		Pre:  func(ToolHookCall) error { panic("钩子崩") },
		Post: func(ToolHookCall, ToolHookResult) {},
	}}}
	face := m.hookFace([]contract.Tool{tool}, SessionBrief{})
	res, err := face[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(res), "pre-hook") {
		t.Fatalf("Pre panic 应转拒绝信封：%v %s", err, res)
	}
	if tool.invoked != 0 {
		t.Fatal("Pre panic 后工具不得执行")
	}

	// Post panic → 观察不破坏运行
	var postGot ToolHookResult
	m2 := &Manager{Opt: Options{Hooks: &ToolHooks{
		Post: func(_ ToolHookCall, r ToolHookResult) { postGot = r; panic("观察崩") },
	}}}
	face2 := m2.hookFace([]contract.Tool{tool}, SessionBrief{})
	start := time.Now()
	res2, err2 := face2[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err2 != nil || !strings.Contains(string(res2), `"ok":true`) || tool.invoked != 1 {
		t.Fatalf("Post panic 不应破坏执行：%v %s", err2, res2)
	}
	if postGot.Err != nil || !strings.Contains(string(postGot.Result), "ran") || postGot.Dur < 0 || time.Since(start) < postGot.Dur {
		t.Fatalf("Post 观察载荷异常：%+v", postGot)
	}
}

func TestHookFaceNilZeroChange(t *testing.T) {
	tool := &hookProbeTool{name: "probe"}
	for _, opt := range []Options{{}, {Hooks: &ToolHooks{}}} {
		m := &Manager{Opt: opt}
		face := m.hookFace([]contract.Tool{tool}, SessionBrief{})
		if len(face) != 1 || face[0] != contract.Tool(tool) {
			t.Fatal("nil/空 Hooks 应原样返回零变化")
		}
	}
}

// TestDenyEnvelopeValidJSONWithSpecials 拒绝信封对特殊字符（引号/换行）的
// JSON 转义——钩子错误文案是应用任意字符串，直拼即产非法 JSON。
func TestDenyEnvelopeValidJSONWithSpecials(t *testing.T) {
	res := denyEnvelope(errors.New("含\"引号\"\n与换行"))
	if !json.Valid(res) {
		t.Fatalf("信封应是合法 JSON：%s", res)
	}
	var probe struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(res, &probe); err != nil || probe.OK || !strings.Contains(probe.Error, "pre-hook") {
		t.Fatalf("信封形态应为 ok=false 布尔 + error 文案：%s（%v）", res, err)
	}
}

func TestHooksFireInRealRun(t *testing.T) {
	tool := &hookProbeTool{name: "probe"}
	var mu sync.Mutex
	fired := 0
	briefSID := ""
	fm := &scriptedModel{}
	fm.onStream = func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "probe", Arguments: "{}"},
			}}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "已完成。"})
	}
	m := newSeamManager(t, func(o *Options) {
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{tool} }
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
		o.Hooks = &ToolHooks{Post: func(c ToolHookCall, _ ToolHookResult) {
			mu.Lock()
			fired++
			briefSID = c.Sess.SID
			mu.Unlock()
		}}
	})
	s := m.Registry().Create("张三", "注", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "问", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if s.StateOf() != session.StateEnded {
		t.Fatalf("应正常收线：%s", s.StateOf())
	}
	mu.Lock()
	defer mu.Unlock()
	if fired == 0 {
		t.Fatal("真实运行应触发 Post（工具实际执行）")
	}
	if briefSID != s.SID {
		t.Fatalf("Post 载荷应带会话身份：%q", briefSID)
	}
}
