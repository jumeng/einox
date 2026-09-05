package engine

// session_end.HistLen 锚定数据回归：自然收束轮末事件携带轮后历史长度
// （ForkAt 截断依据）——单轮与续轮递增形态。

import (
	"context"
	"errors"
	"testing"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llmtest"
	"github.com/jumeng/einox/session"
)

func TestSessionEndCarriesHistLen(t *testing.T) {
	m := newSeamManager(t, nil)
	s := m.Registry().Create("张三", "注", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "问", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if s.StateOf() != session.StateEnded {
		t.Fatalf("应正常收线：%s", s.StateOf())
	}
	var last *contract.SessionEnd
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvSessionEnd {
			if d, ok := ev.Data.(contract.SessionEnd); ok {
				last = &d
			}
		}
	}
	if last == nil {
		t.Fatal("应有 session_end 事件")
	}
	// 轮后历史 = 用户消息 + assistant 终态
	if last.HistLen != s.HistoryLen() || last.HistLen != 2 {
		t.Fatalf("HistLen 应等于轮后历史长度：got %d want %d", last.HistLen, s.HistoryLen())
	}

	// 二轮：HistLen 随轮递增
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "再问", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	last = nil
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvSessionEnd {
			if d, ok := ev.Data.(contract.SessionEnd); ok {
				last = &d
			}
		}
	}
	if last == nil || last.HistLen != 4 {
		t.Fatalf("二轮 HistLen 应为 4：%v", last)
	}
}

// TestSessionEndHistLenOnFatalStreamError 错误轮终的 HistLen 精确性：流中
// 致命错误携带未封账的半截 assistant 段——末段保险封账后入史，HistLen 必须
// 把它计入（错误轮锚是「从失败点重试」合法场景，见设计文档）。
func TestSessionEndHistLenOnFatalStreamError(t *testing.T) {
	fm := llmtest.New(llmtest.Turn{Text: "半截输出", Err: errors.New("连接中断")})
	m := newSeamManager(t, func(o *Options) { o.NewModel = fm.Factory() })
	s := m.Registry().Create("张三", "注", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "问", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if s.StateOf() != session.StateError {
		t.Fatalf("应错误收线：%s", s.StateOf())
	}
	var last *contract.SessionEnd
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvSessionEnd {
			if d, ok := ev.Data.(contract.SessionEnd); ok {
				last = &d
			}
		}
	}
	if last == nil {
		t.Fatal("错误轮终应有 session_end")
	}
	if last.HistLen != s.HistoryLen() {
		t.Fatalf("错误轮 HistLen 应含封账后的半截段：HistLen=%d HistoryLen=%d", last.HistLen, s.HistoryLen())
	}
	if s.HistoryLen() != 2 {
		t.Fatalf("半截 assistant 段应入史：%d", s.HistoryLen())
	}
}
