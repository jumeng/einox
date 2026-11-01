package engine

// D15 提问到点自动续行回归（findings/2026-11-01-hitl-batch-a-prime-design.md）：
// ①到点自动续行全流程——决议=Options[0]、Decider=auto、ask_decision 落流、
// 模型收到答案续跑收口、无 ask_timeout；②闭包返回 nil → 既有取消分支；
// ③Options nil → 零变化（取消分支）；④实时闭包——挂起后才翻转开关，到点
// 生效（闭包到点才被调用，非装配期求值）；⑤并发人工作答让位（CAS，决议
// 不被覆盖、不发超时）；⑥DefaultAskResolve 无选项取空文本 + map 形态载荷
// （重启 Reattach 后事件回读）还原。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools/askuser"
)

// askAutoSetup 装配带 AskAutoResolve 的引擎，首轮 ask_user 挂起后返回。
// resolve 即 Options.AskAutoResolve（测试注入闭包）。
func askAutoSetup(t *testing.T, resolve func(contract.AskReq) *contract.AskDecision) (*Manager, *session.Session, *scriptedModel) {
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
	m := newTestManagerOn(t, tstore.New(t.TempDir()), func(o *Options) {
		o.Tools = func(SessionBrief) []contract.Tool { return at }
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
		o.AskAutoResolve = resolve
	})
	s := m.Registry().Create("张三", "问一下", "auto", contract.UserPrefs{Model: "p/m"})
	res.s = s
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "写周报", nil, func(session.Event) {})
	if s.StateOf() != session.StatePendingApproval || s.PendingAppID() == "" {
		t.Fatalf("应挂起提问，实得 state=%s appID=%q", s.StateOf(), s.PendingAppID())
	}
	t.Cleanup(func() { stopApprovalTimer(s.SID); waitTitleFlight(t, s) })
	return m, s, fm
}

// askAutoWaitState 轮询终态（Resume 在到点 goroutine 内驱动）。
func askAutoWaitState(t *testing.T, s *session.Session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.StateOf() == session.StatePendingApproval || s.StateOf() == session.StateRunning {
		if time.Now().After(deadline) {
			t.Fatalf("等待续行收口超时，state=%s", s.StateOf())
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitTitleFlight(t, s)
}

func TestAskAutoResolveContinuesWithFirstOption(t *testing.T) {
	old := approvalTimeout
	approvalTimeout = 150 * time.Millisecond
	t.Cleanup(func() { approvalTimeout = old })

	var consulted atomic.Int32
	m, s, fm := askAutoSetup(t, func(req contract.AskReq) *contract.AskDecision {
		consulted.Add(1)
		return DefaultAskResolve(req)
	})
	_ = m
	askAutoWaitState(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("自动续行后应正常收口，实得 %s", s.StateOf())
	}
	if consulted.Load() == 0 {
		t.Fatal("闭包应被咨询")
	}
	// ask_decision 落流且 Decider=auto；无 ask_timeout。
	sawDecision, sawTimeout := false, false
	for _, ev := range s.SnapshotEvents() {
		switch ev.Event {
		case contract.EvAskDecision:
			sawDecision = true
			if d, ok := ev.Data.(map[string]any); ok {
				if d["decider_id"] != "auto" {
					t.Fatalf("自动续行决议 Decider 应为 auto，实得 %v", d["decider_id"])
				}
			} else {
				t.Fatalf("ask_decision 载荷形态异常：%T", ev.Data)
			}
		case contract.EvAskTimeout:
			sawTimeout = true
		}
	}
	if !sawDecision {
		t.Fatal("ask_decision 事件应落流（回放重建提问卡终态的真源）")
	}
	if sawTimeout {
		t.Fatal("自动续行不应再走超时取消分支")
	}
	// 模型续行轮应收到 Options[0] 答案（v2.8）。
	ins := fm.inputsOf()
	if len(ins) < 2 {
		t.Fatalf("自动续行应有第二次模型输入，实得 %d 轮", len(ins))
	}
	fed := false
	for _, msg := range ins[len(ins)-1] {
		if msg.Role == schema.Tool && strings.Contains(msg.Content, "v2.8") {
			fed = true
		}
	}
	if !fed {
		t.Fatal("续行轮应回喂首选项答案 v2.8")
	}
}

func TestAskAutoResolveNilDecisionFallsBackToCancel(t *testing.T) {
	m, s, _ := askAutoSetup(t, func(contract.AskReq) *contract.AskDecision { return nil })
	appID := s.PendingAppID()
	m.expirePending(s, appID, "ask")
	if s.StateOf() != session.StateEnded {
		t.Fatalf("闭包返回 nil 应回落取消分支，实得 %s", s.StateOf())
	}
	saw := false
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvAskTimeout {
			saw = true
		}
	}
	if !saw {
		t.Fatal("取消分支应落 ask_timeout")
	}
}

func TestAskAutoResolveNilOptionNoChange(t *testing.T) {
	m, s, _ := askAutoSetup(t, nil) // Options 缝 nil
	appID := s.PendingAppID()
	m.expirePending(s, appID, "ask")
	if s.StateOf() != session.StateEnded {
		t.Fatalf("nil 缝应零变化（取消分支），实得 %s", s.StateOf())
	}
}

func TestAskAutoResolveRealtimeClosure(t *testing.T) {
	old := approvalTimeout
	approvalTimeout = 250 * time.Millisecond
	t.Cleanup(func() { approvalTimeout = old })

	// 装配时开关关闭；挂起后翻转——闭包到点才被调用（实时生效）。
	enabled := &atomic.Bool{}
	m, s, fm := askAutoSetup(t, func(req contract.AskReq) *contract.AskDecision {
		if !enabled.Load() {
			return nil
		}
		return DefaultAskResolve(req)
	})
	_ = m
	enabled.Store(true) // 挂起态翻转（挂起超时 250ms 内）
	askAutoWaitState(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("翻转后到点应自动续行收口，实得 %s", s.StateOf())
	}
	saw := false
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvAskDecision {
			saw = true
		}
		if ev.Event == contract.EvAskTimeout {
			t.Fatal("翻转生效后不应走取消分支")
		}
	}
	if !saw {
		t.Fatal("应落 ask_decision（实时闭包生效证明）")
	}
	if len(fm.inputsOf()) < 2 {
		t.Fatal("续行轮应发生")
	}
}

func TestAskAutoResolveYieldsToHumanAnswer(t *testing.T) {
	m, s, _ := askAutoSetup(t, func(req contract.AskReq) *contract.AskDecision {
		return DefaultAskResolve(req)
	})
	// 人工作答先到（Answer 端点窗口：SetAskDecision 已写、Resume 在途）。
	human := contract.AskDecision{FreeText: "用户亲答", DeciderID: "local", DeciderName: "local"}
	s.SetAskDecision(human)
	m.expirePending(s, s.PendingAppID(), "ask")
	if d := s.TakeAskDecision(); d == nil || d.FreeText != "用户亲答" {
		t.Fatalf("人工作答不得被自动决议覆盖：%+v", d)
	}
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvAskTimeout {
			t.Fatal("作答已到达不得发超时事件")
		}
	}
}

func TestAskAutoResolveRestoresMapShapedPayload(t *testing.T) {
	// 重启 Reattach 后事件 Data 为 JSON 回读的 map 形态——载荷还原走 map 分支。
	m := newTestManager(t, func(o *Options) {
		o.AskAutoResolve = func(req contract.AskReq) *contract.AskDecision {
			if req.AskID != "q9" || req.Question != "重启后的问句" || len(req.Options) != 1 {
				t.Fatalf("map 形态载荷还原异常：%+v", req)
			}
			return DefaultAskResolve(req)
		}
	})
	s := m.Registry().Create("张三", "重启问句", "auto", contract.UserPrefs{})
	s.Record(contract.EvAskRequest, map[string]any{
		"ask_id": "q9", "question": "重启后的问句",
		"options": []any{map[string]any{"label": "继续", "value": "继续"}},
	})
	s.SetPendingApproval("q9")
	if !m.autoResolveAsk(s, "q9") {
		t.Fatal("map 形态载荷应还原并续行")
	}
	waitResumeSettled(t, s)
	if d := s.TakeAskDecision(); d == nil || len(d.Answers) != 1 || d.Answers[0] != "继续" || d.DeciderID != "auto" {
		t.Fatalf("自动决议应取 Options[0] 且 Decider=auto：%+v", d)
	}
	saw := false
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvAskDecision {
			saw = true
		}
	}
	if !saw {
		t.Fatal("ask_decision 应落流")
	}
}

func TestDefaultAskResolveEmptyWhenNoOptions(t *testing.T) {
	d := DefaultAskResolve(contract.AskReq{Question: "自由输入", AllowFreeText: true})
	if d == nil || len(d.Answers) != 0 || d.FreeText != "" {
		t.Fatalf("无选项应取空文本决议：%+v", d)
	}
}

// waitResumeSettled 等待到点派发的 go Resume 落定（BeginResume 抢占后经
// resumeIter 失败收束 StateError——白盒会话无 checkpoint，恰可观测翻转）。
func waitResumeSettled(t *testing.T, s *session.Session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.StateOf() == session.StatePendingApproval {
		if time.Now().After(deadline) {
			t.Fatalf("等待 Resume 抢占超时，state=%s", s.StateOf())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
