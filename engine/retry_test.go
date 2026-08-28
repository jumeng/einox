package engine

// 网络容错 ②③ 引擎侧回归：流中途断连重试（transport_retry + 半截不入史 +
// 重试尝试续流）、致命 API 错误立即停机（欠费类不重试）、重试耗尽收线
// （TRANSPORT 错误卡 + 有界调用数）。adk 重试为真实装配（非桩）——
// ShouldRetry 分类走 llm.Classify 生产路径。

import (
	"context"
	"strings"
	"sync"
	"testing"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// errStreamModel 本地假模型：onTurn 返回非 nil = 该次流以错误收线（发送
// 剧本内容之后注入）；nil = 正常 EOF。
type errStreamModel struct {
	mu     sync.Mutex
	calls  int
	onTurn func(n int, send func(*schema.Message)) error
}

func (f *errStreamModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("非流式", nil), nil
}

func (f *errStreamModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	sr, sw := schema.Pipe[*schema.Message](8)
	go func() {
		defer sw.Close()
		if err := f.onTurn(n, func(m *schema.Message) { sw.Send(m, nil) }); err != nil {
			sw.Send(nil, err)
		}
	}()
	return sr, nil
}

func (f *errStreamModel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// runChat 起一轮会话并收集事件（name+error 载荷）。
func runChat(t *testing.T, m *Manager, fm *errStreamModel, text string) (*session.Session, []string, []contract.ErrorOut) {
	t.Helper()
	s := m.Registry().Create("张三", text, "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var names []string
	var errs []contract.ErrorOut
	m.Run(context.Background(), s, text, nil, func(ev session.Event) {
		names = append(names, ev.Event)
		if ev.Event == contract.EvError {
			errs = append(errs, ev.Data.(contract.ErrorOut))
		}
	})
	waitTitleFlight(t, s)
	return s, names, errs
}

func retryCount(names []string) int {
	n := 0
	for _, v := range names {
		if v == contract.EvTransportRetry {
			n++
		}
	}
	return n
}

func TestTransportRetryMidStream(t *testing.T) {
	fm := &errStreamModel{onTurn: func(n int, send func(*schema.Message)) error {
		if n == 1 { // 首次：半截内容后断连（空闲哨兵 = 可重试传输类）
			send(&schema.Message{Role: schema.Assistant, Content: "半截内容"})
			return llm.ErrIdleTimeout
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完整答复"})
		return nil
	}}
	m, _ := newRunManager(t, nil, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	})
	s, names, errs := runChat(t, m, fm, "问")
	if s.StateOf() != session.StateEnded {
		t.Fatalf("重试成功应正常终态：%s（errs=%v）", s.StateOf(), errs)
	}
	if retryCount(names) != 1 {
		t.Fatalf("应恰一次重连通知：%v", names)
	}
	if fm.count() != 2 {
		t.Fatalf("应恰两次模型调用（1 失败 + 1 重试）：%d", fm.count())
	}
	for _, msg := range s.CloneHistory() {
		if msg.Role == schema.Assistant && (strings.Contains(msg.Content, "半截")) {
			t.Fatalf("半截段不得入史：%q", msg.Content)
		}
	}
	var final string
	for _, msg := range s.CloneHistory() {
		if msg.Role == schema.Assistant {
			final = msg.Content
		}
	}
	if final != "完整答复" {
		t.Fatalf("历史末段应为重试后完整答复，实得 %q", final)
	}
}

func TestFatalAPIErrorStopsRun(t *testing.T) {
	fm := &errStreamModel{onTurn: func(n int, send func(*schema.Message)) error {
		send(&schema.Message{Role: schema.Assistant, Content: "开头"})
		return &einoopenai.APIError{HTTPStatusCode: 402, Message: "Insufficient Balance"}
	}}
	m, _ := newRunManager(t, nil, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	})
	s, names, errs := runChat(t, m, fm, "问")
	if s.StateOf() != session.StateError {
		t.Fatalf("欠费类致命错误应停机 error 态：%s", s.StateOf())
	}
	if fm.count() != 1 {
		t.Fatalf("致命错误不得重试：%d", fm.count())
	}
	if retryCount(names) != 0 {
		t.Fatalf("致命错误不得发重连通知：%v", names)
	}
	if len(errs) != 1 || errs[0].Code != "SERVER" || !strings.Contains(errs[0].Message, "余额不足") {
		t.Fatalf("错误卡应为欠费文案：%+v", errs)
	}
	hasEnd := false
	for _, v := range names {
		if v == contract.EvSessionEnd {
			hasEnd = true
		}
	}
	if !hasEnd {
		t.Fatalf("致命停机应确定性收线（session_end）：%v", names)
	}
}

func TestRetryExhaustedStopsRun(t *testing.T) {
	old := llm.MaxRetries
	llm.MaxRetries = 2
	t.Cleanup(func() { llm.MaxRetries = old })

	fm := &errStreamModel{onTurn: func(n int, send func(*schema.Message)) error {
		send(&schema.Message{Role: schema.Assistant, Content: "尝试内容"})
		return llm.ErrIdleTimeout // 恒可重试 → 耗尽
	}}
	m, _ := newRunManager(t, nil, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	})
	s, names, errs := runChat(t, m, fm, "问")
	if s.StateOf() != session.StateError {
		t.Fatalf("重试耗尽应停机 error 态：%s", s.StateOf())
	}
	if fm.count() != 3 { // 1 初始 + 2 重试
		t.Fatalf("应恰三次调用（有界）：%d", fm.count())
	}
	if retryCount(names) != 2 { // 耗尽前两次失败发通知；末次不发（错误卡即到）
		t.Fatalf("应恰两次重连通知：%v", names)
	}
	if len(errs) != 1 || errs[0].Code != "TRANSPORT" || !strings.Contains(errs[0].Message, "已自动重试 2 次") {
		t.Fatalf("错误卡应为传输类+重试注记：%+v", errs)
	}
}
