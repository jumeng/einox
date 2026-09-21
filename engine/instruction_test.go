package engine

// N1 系统提示生命周期回归：①instruction_change 真源事件（首调基线 + 变更，
// Run/Resume 共用 assemble 都落）②InHistorySystem 能力模型 position-0 锚冻结
// + 变更历史内追加（前缀缓存保真）③切换非能力模型时边界归一（最新 system
// 归位 position-0、mid-system 剔除）。设计件 =
// findings/2026-09-20-n1-system-prompt-lifecycle-design.md。

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// newInstrManager 双模型（p/m 普通模型、p/mc InHistorySystem 能力模型）分派
// 假模型的测试引擎；返回可变 Instruction 句柄（轮间改写驱动变更场景）。
func newInstrManager(t *testing.T, mA, mB *scriptedModel) (*Manager, *string) {
	t.Helper()
	instr := "V1"
	m := newTestManager(t, func(o *Options) {
		o.Providers = func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{
					{ID: "m", Input: []string{"text"}, Priority: 100},
					{ID: "mc", Input: []string{"text"}, Priority: 90, InHistorySystem: true},
				},
			}}
		}
		o.Instruction = func(SessionBrief) string { return instr }
		o.NewModel = func(_ context.Context, _ llm.ProviderSpec, spec llm.ModelSpec, _ string) (model.BaseModel[*schema.Message], error) {
			if spec.ID == "mc" {
				return mB, nil
			}
			return mA, nil
		}
	})
	return m, &instr
}

// instrEvents 取 instruction_change 事件的 Text 序列。
func instrEvents(s *session.Session) []string {
	var out []string
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvInstructionChange {
			if ic, ok := ev.Data.(contract.InstructionChange); ok {
				out = append(out, ic.Text)
			}
		}
	}
	return out
}

// countMidSystem 数 index>0 的 system 消息条数。
func countMidSystem(in []*schema.Message) int {
	n := 0
	for i, msg := range in {
		if i > 0 && msg.Role == schema.System {
			n++
		}
	}
	return n
}

// TestInstructionChangeEvents 真源事件（普通模型）：首调落基线、变更落新值；
// position-0 即最新（既有行为零变化）。
func TestInstructionChangeEvents(t *testing.T) {
	mA, mB := &scriptedModel{}, &scriptedModel{}
	m, instr := newInstrManager(t, mA, mB)
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})

	runNote(m, s, "问1")
	waitTitleFlight(t, s)
	if got := instrEvents(s); len(got) != 1 || got[0] != "V1" {
		t.Fatalf("首调应落基线事件：%v", got)
	}
	if in := lastInput(mA); in[0].Role != schema.System || in[0].Content != "V1" {
		t.Fatalf("position-0 应为当前 Instruction：%s", in[0].Content)
	}

	*instr = "V2"
	runNote(m, s, "问2")
	waitTitleFlight(t, s)
	if got := instrEvents(s); len(got) != 2 || got[1] != "V2" {
		t.Fatalf("变更应落事件：%v", got)
	}
	if in := lastInput(mA); in[0].Content != "V2" {
		t.Fatalf("普通模型 position-0 应即时换新：%s", in[0].Content)
	}
	if got := countMidSystem(lastInput(mA)); got != 0 {
		t.Fatalf("普通模型不应有 mid-system：%d", got)
	}
}

// TestInHistorySystemAnchorAndAppend 能力模型：锚冻结 + 变更历史内追加 +
// 同指令不重追加。
func TestInHistorySystemAnchorAndAppend(t *testing.T) {
	mA, mB := &scriptedModel{}, &scriptedModel{}
	m, instr := newInstrManager(t, mA, mB)
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/mc"})

	runNote(m, s, "问1")
	waitTitleFlight(t, s)
	in := lastInput(mB)
	if in[0].Role != schema.System || in[0].Content != "V1" {
		t.Fatalf("能力首跑 position-0 应为当前 Instruction：%s", in[0].Content)
	}
	if got := countMidSystem(in); got != 0 {
		t.Fatalf("首跑无追加：%d", got)
	}

	*instr = "V2"
	runNote(m, s, "问2")
	waitTitleFlight(t, s)
	in = lastInput(mB)
	if in[0].Content != "V1" {
		t.Fatalf("变更轮 position-0 应冻结为锚 V1：%s", in[0].Content)
	}
	if got := countMidSystem(in); got != 1 {
		t.Fatalf("变更应恰追加一条 mid-system：%d", got)
	}
	iMid, iQ := -1, -1
	for i, msg := range in {
		if i > 0 && msg.Role == schema.System {
			iMid = i
		}
		if msg.Role == schema.User && msg.Content == "问2" {
			iQ = i
		}
	}
	if iMid < 0 || iQ < 0 || iMid > iQ {
		t.Fatalf("追加应位于新输入之前：mid=%d q=%d", iMid, iQ)
	}
	if in[iMid].Content != "V2" {
		t.Fatalf("追加内容应为新 Instruction：%s", in[iMid].Content)
	}

	runNote(m, s, "问3") // 同指令：锚仍冻结、不重追加
	waitTitleFlight(t, s)
	in = lastInput(mB)
	if in[0].Content != "V1" {
		t.Fatalf("锚应跨轮冻结：%s", in[0].Content)
	}
	if got := countMidSystem(in); got != 1 {
		t.Fatalf("同指令不应重追加：%d", got)
	}
}

// TestInHistorySystemNormalizeOnPlainSwitch 能力期追加后切普通模型：边界
// 归一生效（最新 system 归位 position-0、无 mid-system）。
func TestInHistorySystemNormalizeOnPlainSwitch(t *testing.T) {
	mA, mB := &scriptedModel{}, &scriptedModel{}
	m, instr := newInstrManager(t, mA, mB)
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/mc"})

	runNote(m, s, "问1")
	waitTitleFlight(t, s)
	*instr = "V2"
	runNote(m, s, "问2")
	waitTitleFlight(t, s)

	s.SetRunModel("p/m", "")
	runNote(m, s, "问3")
	waitTitleFlight(t, s)
	in := lastInput(mA)
	if in[0].Role != schema.System || in[0].Content != "V2" {
		t.Fatalf("普通模型 position-0 应为最新 Instruction（归一）：%s", in[0].Content)
	}
	if got := countMidSystem(in); got != 0 {
		t.Fatalf("mid-system 应被边界归一剔除：%d", got)
	}
}
