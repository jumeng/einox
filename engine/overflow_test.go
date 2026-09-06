package engine

// 溢出恢复回归（吸收设计 §3.3-②）：OVERFLOW 类终态错误 → manager 重装配
// 本轮输入（清窗兜底同源尾段裁剪 + 任务锚）重试一次；出站口径降不下来
// （当前轮单条巨消息即全部历史）不重试、如实报错。

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

// overflowModel errs 按序以对应错误收流（Stream 路径），耗尽后走内层成功
// 模型（Generate 恒委托内层——genTitle 路径不消费错误预算）。
type overflowModel struct {
	mu    sync.Mutex
	errs  []error
	inner *scriptedModel
}

func (o *overflowModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	o.mu.Lock()
	var err error
	if len(o.errs) > 0 {
		err, o.errs = o.errs[0], o.errs[1:]
	}
	o.mu.Unlock()
	if err != nil {
		sr, sw := schema.Pipe[*schema.Message](2)
		go func() {
			defer sw.Close()
			sw.Send(nil, err)
		}()
		return sr, nil
	}
	return o.inner.Stream(ctx, in, opts...)
}

func (o *overflowModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return o.inner.Generate(ctx, in, opts...)
}

func overflowErr() error {
	return &einoopenai.APIError{HTTPStatusCode: 400, Code: "context_length_exceeded",
		Message: "This model's maximum context length is 65536 tokens"}
}

// TestOverflowRecoversWithTrimmedInput 多轮历史超窗：首调炸 OVERFLOW →
// 重装配（尾段 + 任务锚）重试成功——通知卡在场、无错误卡、正常收束。
func TestOverflowRecoversWithTrimmedInput(t *testing.T) {
	ok := &scriptedModel{}
	om := &overflowModel{errs: []error{overflowErr()}, inner: ok}
	m := newTestManager(t, func(o *Options) {
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return om, nil
		}
	})
	s := m.Registry().Create("张三", "超窗", "plan", contract.UserPrefs{Model: "p/m"})
	s.AppendHistory(sumHist(4, 8000)...)
	s.SetState(session.StateRunning)

	var evs []session.Event
	m.Run(context.Background(), s, "继续", nil, func(e session.Event) { evs = append(evs, e) })
	waitTitleFlight(t, s)

	if n := len(ok.inputsOf()); n != 1 {
		t.Fatalf("重试后应恰一次成功调用，实得 %d", n)
	}
	var joined strings.Builder
	for _, msg := range ok.inputsOf()[0] {
		joined.WriteString(msgTextOf(msg))
	}
	all := joined.String()
	if strings.Contains(all, "R1BIG") {
		t.Fatalf("重试输入应为尾段裁剪视图（旧轮不可见）")
	}
	if !strings.Contains(all, "任务状态锚") {
		t.Fatalf("重试输入应含任务锚")
	}
	sawNote, sawErr := false, false
	for _, e := range evs {
		switch e.Event {
		case contract.EvHarnessNote:
			if n, ok2 := e.Data.(contract.HarnessNote); ok2 && n.Kind == "compaction" && strings.Contains(n.Title, "超窗") {
				sawNote = true
			}
		case contract.EvError:
			sawErr = true
		}
	}
	if !sawNote {
		t.Fatalf("应有超窗裁剪通知卡")
	}
	if sawErr {
		t.Fatalf("恢复成功不应有错误卡")
	}
	if st := s.StateOf(); st != session.StateEnded {
		t.Fatalf("恢复后应正常收束，实得 %s", st)
	}
}

// TestOverflowNoShrinkReportsError 当前轮单条巨消息即全部历史：裁剪无从
// 下降 → 不重试、如实发 OVERFLOW 错误卡、error 收束。
func TestOverflowNoShrinkReportsError(t *testing.T) {
	ok := &scriptedModel{}
	om := &overflowModel{errs: []error{overflowErr()}, inner: ok}
	m := newTestManager(t, func(o *Options) {
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return om, nil
		}
	})
	s := m.Registry().Create("张三", "巨窗", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)

	var evs []session.Event
	m.Run(context.Background(), s, strings.Repeat("巨", 40000), nil, func(e session.Event) { evs = append(evs, e) })
	waitTitleFlight(t, s)

	if n := len(ok.inputsOf()); n != 0 {
		t.Fatalf("口径无从下降不应重试，实得 %d 次成功调用", n)
	}
	sawOverflowCard := false
	for _, e := range evs {
		if e.Event == contract.EvError {
			if c, ok2 := e.Data.(contract.ErrorOut); ok2 && c.Code == llm.CodeOverflow {
				sawOverflowCard = true
			}
		}
	}
	if !sawOverflowCard {
		t.Fatalf("应有 OVERFLOW 错误卡")
	}
	if st := s.StateOf(); st != session.StateError {
		t.Fatalf("应 error 收束，实得 %s", st)
	}
}

// TestOverflowRetryRebuildFailsReportsRealError 重装配后 runIter 失败（如模型
// 构造炸）：如实报真实原因（configError→CONFIG 卡）——不得吞错归因到旧
// overflow 错误（第三轮审查 P2：原实现静默 return false，用户看到的是已过期
// 的超窗错误卡，排障方向被误导）。
func TestOverflowRetryRebuildFailsReportsRealError(t *testing.T) {
	ok := &scriptedModel{}
	om := &overflowModel{errs: []error{overflowErr()}, inner: ok}
	n := 0
	m := newTestManager(t, func(o *Options) {
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			n++
			if n >= 2 { // 二次 assemble（overflowRetry 的 runIter）失败——真实原因
				return nil, &configError{"重装配时模型构造炸了"}
			}
			return om, nil
		}
	})
	s := m.Registry().Create("张三", "重装失败", "plan", contract.UserPrefs{Model: "p/m"})
	s.AppendHistory(sumHist(4, 8000)...)
	s.SetState(session.StateRunning)

	var evs []session.Event
	m.Run(context.Background(), s, "继续", nil, func(e session.Event) { evs = append(evs, e) })
	waitTitleFlight(t, s)

	sawReal := false
	for _, e := range evs {
		if e.Event == contract.EvError {
			if c, ok2 := e.Data.(contract.ErrorOut); ok2 && strings.Contains(c.Message, "重装配时模型构造炸了") {
				sawReal = true
			}
		}
	}
	if !sawReal {
		t.Fatalf("重装配失败应如实报真实原因（CONFIG 卡含构造错误），实得事件流：%v", evs)
	}
	if n := len(ok.inputsOf()); n != 0 {
		t.Fatalf("重装配失败不应有成功模型调用，实得 %d", n)
	}
}
