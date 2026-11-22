package engine

// 模型记录缝引擎剧本（批 B；设计件 findings/2026-11-22-model-io-recorder-design.md）：
// Options.ModelRecorder 装配下——face 归属（主面 main/genTitle 面 title）、
// started+终态成对、会话级记忆化（工厂每会话一次）。

import (
	"context"
	"sync"
	"testing"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// recFunc 函数适配器（llm.ModelRecorder）。
type engRecFunc func(llm.ModelCallRecord)

func (f engRecFunc) Record(c llm.ModelCallRecord) { f(c) }

func TestModelRecorderFaces(t *testing.T) {
	var mu sync.Mutex
	var got []string
	factoryCalls := 0
	m := newTestManager(t, func(o *Options) {
		o.ModelRecorder = func(b SessionBrief) llm.ModelRecorder {
			factoryCalls++
			return engRecFunc(func(c llm.ModelCallRecord) {
				mu.Lock()
				got = append(got, c.Face+":"+c.Status)
				mu.Unlock()
			})
		}
	})

	s := m.Registry().Create("张三", "创建", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "你好", nil, func(ev session.Event) {})
	waitTitleFlight(t, s) // genTitle 是异步的（title 面记录在其内）

	// 第二轮：记忆化——工厂不重调；main 面再一对 started+终态
	m.Run(context.Background(), s, "再问", nil, func(ev session.Event) {})
	waitTitleFlight(t, s)

	mu.Lock()
	defer mu.Unlock()
	main, title := 0, 0
	for _, f := range got {
		switch {
		case f == "main:started" || f == "main:completed":
			main++
		case f == "title:started" || f == "title:completed":
			title++
		}
	}
	if main < 4 { // 两轮 ×（started+completed）
		t.Fatalf("main 面记录不足：%d（%v）", main, got)
	}
	if title < 2 { // genTitle ≥1 次 Generate（started+completed）
		t.Fatalf("title 面记录缺失：%d（%v）", title, got)
	}
	if factoryCalls != 1 {
		t.Fatalf("工厂应每会话一次（记忆化），实得 %d", factoryCalls)
	}
}

// TestModelRecorderNilUnwired 不装配=零变化（既有引擎行为回归锚：本测只是
// 确保 Options 零值下 Run 照常收线——包装层无感）。
func TestModelRecorderNilUnwired(t *testing.T) {
	m := newTestManager(t, nil)
	s := m.Registry().Create("张三", "创建", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var names []string
	m.Run(context.Background(), s, "你好", nil, func(ev session.Event) { names = append(names, ev.Event) })
	waitTitleFlight(t, s)
	if s.StateOf() != session.StateEnded {
		t.Fatalf("零装配应正常收线：%s", s.StateOf())
	}
}
