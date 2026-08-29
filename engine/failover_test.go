package engine

// 主模型 Failover 降级链引擎级回归：重试耗尽后换备模型续跑 + model_change
// 事件落流；致命类（未知错误保守致命）不降级直接停机、备模型零调用。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// brokenModel 恒以 err 收流（可重试或致命由 err 决定）。
type brokenModel struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (b *brokenModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return nil, b.err
}

func (b *brokenModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	sr, sw := schema.Pipe[*schema.Message](2)
	go func() {
		defer sw.Close()
		sw.Send(nil, b.err)
	}()
	return sr, nil
}

func (b *brokenModel) count() int { b.mu.Lock(); defer b.mu.Unlock(); return b.calls }

// newFailoverManager 双模型装配（p/m 主 + p/f 备）+ FallbackModels 降级链。
func newFailoverManager(t *testing.T, main, fb model.BaseModel[*schema.Message]) *Manager {
	t.Helper()
	return newSeamManager(t, func(o *Options) {
		o.Providers = func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{
					{ID: "m", Input: []string{"text"}, Priority: 100},
					{ID: "f", Input: []string{"text"}, Priority: 50},
				},
			}}
		}
		o.NewModel = func(_ context.Context, _ llm.ProviderSpec, spec llm.ModelSpec, _ string) (model.BaseModel[*schema.Message], error) {
			if spec.ID == "f" {
				return fb, nil
			}
			return main, nil
		}
		o.FallbackModels = []string{"p/f"}
	})
}

func TestFailoverSwitchesAndContinues(t *testing.T) {
	old := llm.MaxRetries
	llm.MaxRetries = 1 // 快速耗尽（1 初始 + 1 重试）
	t.Cleanup(func() { llm.MaxRetries = old })

	main := &brokenModel{err: llm.ErrIdleTimeout} // 恒可重试 → 耗尽 → 降级
	fb := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "降级成功"})
	}}
	m := newFailoverManager(t, main, fb)
	s := m.Registry().Create("张三", "降级", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var texts []string
	m.Run(context.Background(), s, "问", nil, func(ev session.Event) {
		if ev.Event == contract.EvTextDelta {
			texts = append(texts, ev.Data.(contract.Delta).Delta)
		}
	})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("降级续跑应正常收线，终态 %s（events：%v）", s.StateOf(), eventNames(s))
	}
	if !strings.Contains(strings.Join(texts, ""), "降级成功") {
		t.Fatalf("文本应来自备模型：%v", texts)
	}
	mc := false
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvModelChange {
			if d, ok := ev.Data.(contract.ModelChange); ok && d.From == "p/m" && d.To == "p/f" {
				mc = true
			}
		}
	}
	if !mc {
		t.Fatal("降级切换应发 model_change（From=p/m To=p/f）")
	}
}

func TestFailoverFatalNoSwitch(t *testing.T) {
	old := llm.MaxRetries
	llm.MaxRetries = 1
	t.Cleanup(func() { llm.MaxRetries = old })

	main := &brokenModel{err: errors.New("未知硬错")} // 未知类保守致命：不重试也不降级
	fb := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "不应到达"})
	}}
	m := newFailoverManager(t, main, fb)
	s := m.Registry().Create("张三", "致命", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "问", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateError {
		t.Fatalf("致命类应直接停机不降级，终态 %s", s.StateOf())
	}
	if n := fbCount(fb); n != 0 {
		t.Fatalf("备模型不应被调用：%d", n)
	}
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvModelChange {
			t.Fatal("致命停机不应发 model_change")
		}
	}
}

// fbCount scriptedModel 调用数（inputs 长度——其自身无锁，测试内串行断言）。
func fbCount(m model.BaseModel[*schema.Message]) int {
	if sm, ok := m.(*scriptedModel); ok {
		return len(sm.inputs)
	}
	return 0
}

// eventNames 事件名清单（失败诊断用）。
func eventNames(s *session.Session) []string {
	var out []string
	for _, ev := range s.SnapshotEvents() {
		out = append(out, ev.Event)
	}
	return out
}

// TestFailoverCancelNoSwitch 取消类错误不进降级（上游 needFailover 排除
// ctx 取消 + 本地 ShouldFailover 走 Classify 不可重试面双层排除）：备模型
// 零调用、无 model_change、轮按中断语义收束。
func TestFailoverCancelNoSwitch(t *testing.T) {
	main := &brokenModel{err: context.Canceled}
	fb := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "不应到达"})
	}}
	m := newFailoverManager(t, main, fb)
	s := m.Registry().Create("张三", "取消", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "问", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateError {
		t.Fatalf("取消类应中断收束不降级，终态 %s", s.StateOf())
	}
	if n := fbCount(fb); n != 0 {
		t.Fatalf("备模型不应被调用：%d", n)
	}
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvModelChange {
			t.Fatal("取消类不应发 model_change")
		}
	}
}
