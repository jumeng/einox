package engine

// 收束质量门回归（findings §10 分层修订版——einox 层=门循环机制）：
// 门过直通；门拒回灌（反馈入史、门卡通知、修复后过）；重试耗尽诚实报错
// （不静默放行）；挂起/错误轮不触发；checker panic fail-closed；nil 零变化。

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// TestGatePassDirect 门过：checker 一次调用、无门卡、正常收束。
func TestGatePassDirect(t *testing.T) {
	var calls atomic.Int32
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.FinalGate = func(SessionBrief) *GateConfig {
			return &GateConfig{Checkers: []GateChecker{
				func(context.Context, string) error { calls.Add(1); return nil },
			}}
		}
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "做", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("门过应正常收束：%s", s.StateOf())
	}
	if calls.Load() != 1 {
		t.Fatalf("checker 应恰调用一次：%d", calls.Load())
	}
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvHarnessNote || ev.Event == contract.EvError {
			t.Fatal("门过不应有门卡/报错")
		}
	}
}

// TestGateFailReinjectThenPass 门拒回灌：首轮拒→反馈入史驱动第二轮→过；
// 门卡 harness_note(kind=gate) 落流。
func TestGateFailReinjectThenPass(t *testing.T) {
	var calls atomic.Int32
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "修好了"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.FinalGate = func(SessionBrief) *GateConfig {
			return &GateConfig{MaxRetries: 2, Checkers: []GateChecker{func(_ context.Context, _ string) error {
				if calls.Add(1) == 1 {
					return errors.New("构建失败：main.go 未通过编译")
				}
				return nil // 第二次过（模拟模型修复后）
			}}}
		}
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "做", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded {
		t.Fatalf("修复后应正常收束：%s", s.StateOf())
	}
	if calls.Load() != 2 {
		t.Fatalf("checker 应调用两次（拒+过）：%d", calls.Load())
	}
	// 回灌反馈必须出现在第二轮模型输入（反馈驱动修复的直接证据）
	found := false
	for _, in := range fm.inputs {
		for _, msg := range in {
			if msg.Role == schema.User && strings.Contains(msg.Content, "质量门未过") &&
				strings.Contains(msg.Content, "构建失败") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("回灌反馈应出现在后续模型输入（共 %d 次调用）", len(fm.inputs))
	}
	gateCard := false
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvHarnessNote {
			if d, ok := ev.Data.(contract.HarnessNote); ok && d.Kind == "gate" {
				gateCard = true
			}
		}
	}
	if !gateCard {
		t.Fatal("门拒应发 harness_note(kind=gate) 门卡")
	}
}

// TestGateExhaustsHonestError 重试耗尽：error 收束、报错含质量门语义与重试
// 注记、checker 恰 MaxRetries+1 次。
func TestGateExhaustsHonestError(t *testing.T) {
	var calls atomic.Int32
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "还是不行"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.FinalGate = func(SessionBrief) *GateConfig {
			return &GateConfig{MaxRetries: 1, Checkers: []GateChecker{
				func(context.Context, string) error { calls.Add(1); return errors.New("测试未过：断言失败") },
			}}
		}
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "做", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateError {
		t.Fatalf("耗尽应 error 收束（不静默放行）：%s", s.StateOf())
	}
	if calls.Load() != 2 { // MaxRetries=1：首验 + 1 次回灌后重验
		t.Fatalf("checker 应恰 2 次：%d", calls.Load())
	}
	msg := ""
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvError {
			if d, ok := ev.Data.(contract.ErrorOut); ok {
				msg = d.Message
			}
		}
	}
	if !strings.Contains(msg, "质量门未过") || !strings.Contains(msg, "已重试 1") {
		t.Fatalf("报错应含质量门语义与重试注记：%q", msg)
	}
}

// TestGateSkipsOnNonNaturalEnd 挂起轮（写工具审批）与错误轮都不触发门——
// 门只卡收束点。
func TestGateSkipsOnNonNaturalEnd(t *testing.T) {
	// 挂起轮：manual 模式写工具 → 审批挂起
	var calls atomic.Int32
	wt, _ := tools.InferTool("write_tool", "写桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			tcOf("w1", "write_tool", `{}`)}})
	}}
	m, _ := newRunManager(t, []contract.Tool{wt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	})
	m.Opt.FinalGate = func(SessionBrief) *GateConfig {
		return &GateConfig{Checkers: []GateChecker{
			func(context.Context, string) error { calls.Add(1); return nil },
		}}
	}
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "写", nil, func(session.Event) {})
	if s.StateOf() != session.StatePendingApproval {
		t.Fatalf("应挂起审批（实得 %s）", s.StateOf())
	}
	if calls.Load() != 0 {
		t.Fatalf("挂起轮不应过门：%d", calls.Load())
	}
	stopApprovalTimer(s.SID)

	// 错误轮：致命错误 → 门不触发
	calls.Store(0)
	m2 := newSeamManager(t, func(o *Options) {
		o.FinalGate = func(SessionBrief) *GateConfig {
			return &GateConfig{Checkers: []GateChecker{
				func(context.Context, string) error { calls.Add(1); return nil },
			}}
		}
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return &brokenModel{err: errors.New("硬错")}, nil
		}
	})
	s2 := m2.Registry().Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	s2.SetState(session.StateRunning)
	m2.Run(context.Background(), s2, "做", nil, func(session.Event) {})
	waitTitleFlight(t, s2)
	if s2.StateOf() != session.StateError || calls.Load() != 0 {
		t.Fatalf("错误轮不应过门（state=%s calls=%d）", s2.StateOf(), calls.Load())
	}
}

// TestGateCheckerPanicFailClosed checker panic 转门失败（fail-closed 参与
// 重试，不拖垮运行）。
func TestGateCheckerPanicFailClosed(t *testing.T) {
	var calls atomic.Int32
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "试试"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.FinalGate = func(SessionBrief) *GateConfig {
			return &GateConfig{MaxRetries: 1, Checkers: []GateChecker{
				func(context.Context, string) error { calls.Add(1); panic("checker 崩了") },
			}}
		}
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "做", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if s.StateOf() != session.StateError {
		t.Fatalf("checker panic 应按门失败收束：%s", s.StateOf())
	}
	if calls.Load() != 2 {
		t.Fatalf("panic 应按失败参与重试（首验+回灌重验）：%d", calls.Load())
	}
}

// TestGateReinjectAssembleFailNoDupHistory 门拒后回灌装配失败（模型配置
// 中途失效等）：首轮产出应恰入史一次——settleTurn 不得对门循环内已入史的
// acc 复用账目二次追加（双入史回归锚）。
func TestGateReinjectAssembleFailNoDupHistory(t *testing.T) {
	var gateCalls, modelCalls atomic.Int32
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "第一版"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.FinalGate = func(SessionBrief) *GateConfig {
			return &GateConfig{Checkers: []GateChecker{func(context.Context, string) error {
				gateCalls.Add(1)
				return errors.New("构建失败：缺 main.go")
			}}}
		}
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			if modelCalls.Add(1) >= 2 {
				return nil, errors.New("装配失败") // 回灌轮 assemble 构造失败
			}
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "做", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateError {
		t.Fatalf("回灌装配失败应 error 收束：%s", s.StateOf())
	}
	if gateCalls.Load() != 1 {
		t.Fatalf("checker 应恰一次（拒后回灌即失败）：%d", gateCalls.Load())
	}
	n := 0
	for _, msg := range s.CloneHistory() {
		if msg.Role == schema.Assistant && strings.Contains(msg.Content, "第一版") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("首轮产出应恰入史一次，实得 %d 次", n)
	}
}

// TestGateStrictZeroRetries 零回灌档（MaxRetries=0，codex Guardian cyber 档
// 同型）：首验失败即报错收束、checker 恰一次、无回灌重跑。
func TestGateStrictZeroRetries(t *testing.T) {
	var calls atomic.Int32
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "一稿"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.FinalGate = func(SessionBrief) *GateConfig {
			return &GateConfig{MaxRetries: 0, Checkers: []GateChecker{
				func(context.Context, string) error { calls.Add(1); return errors.New("构建失败") },
			}}
		}
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "做", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateError {
		t.Fatalf("零回灌档首验失败应即报错：%s", s.StateOf())
	}
	if calls.Load() != 1 {
		t.Fatalf("checker 应恰一次：%d", calls.Load())
	}
	if len(fm.inputs) != 1 {
		t.Fatalf("不应有回灌重跑（模型调用 %d 次）", len(fm.inputs))
	}
}

// TestGateMultiRoundEpilogueOnce 多轮门回灌后过门：收尾钩子恰触发一次
//（回灌轮不重复 settle——单次收尾语义）。
func TestGateMultiRoundEpilogueOnce(t *testing.T) {
	var gateCalls, epilogues atomic.Int32
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "修复版"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.FinalGate = func(SessionBrief) *GateConfig {
			return &GateConfig{MaxRetries: 2, Checkers: []GateChecker{func(context.Context, string) error {
				if gateCalls.Add(1) == 1 {
					return errors.New("构建失败")
				}
				return nil
			}}}
		}
		o.TurnEpilogue = func(TurnEndSummary) { epilogues.Add(1) }
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "做", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	if s.StateOf() != session.StateEnded || gateCalls.Load() != 2 {
		t.Fatalf("应两验过门收束（state=%s checks=%d）", s.StateOf(), gateCalls.Load())
	}
	if epilogues.Load() != 1 {
		t.Fatalf("多轮门循环后收尾钩子应恰一次，实得 %d", epilogues.Load())
	}
}

// TestGateNilClosureZeroChange 闭包按会话形态返回 nil = 不开门（零变化）。
func TestGateNilClosureZeroChange(t *testing.T) {
	var calls atomic.Int32
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.FinalGate = func(b SessionBrief) *GateConfig {
			if b.Mode != "coding" {
				return nil // 非编码形态不开门（应用侧按形态开门的用法）
			}
			return &GateConfig{Checkers: []GateChecker{
				func(context.Context, string) error { calls.Add(1); return nil },
			}}
		}
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"}) // auto ≠ coding
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "做", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if s.StateOf() != session.StateEnded || calls.Load() != 0 {
		t.Fatalf("nil 门配置应零变化（state=%s calls=%d）", s.StateOf(), calls.Load())
	}
}
