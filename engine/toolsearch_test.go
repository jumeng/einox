package engine

// H7 toolsearch 回归：动态面初始不可见 → tool_search 命中后加载（Run 内
// sticky）；常驻面恒可见；**动态工具的审批包装不豁免**（分流在 hitl.
// WrapTools 之后——manual 档下动态写工具命中加载后调用仍中断审批）。

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// toolVisModel 可见性记录模型：每轮记录模型实际收到的工具名清单
// （model.WithTools 选项提取），剧本按轮次派发。
type toolVisModel struct {
	script func(n int) *schema.Message
	seen   [][]string
}

func (t *toolVisModel) next() *schema.Message { return t.script(len(t.seen)) }

func (t *toolVisModel) Stream(_ context.Context, _ []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if o := model.GetCommonOptions(&model.Options{}, opts...); o != nil {
		names := make([]string, 0, len(o.Tools))
		for _, ti := range o.Tools {
			names = append(names, ti.Name)
		}
		t.seen = append(t.seen, names)
	} else {
		t.seen = append(t.seen, nil)
	}
	msg := t.next()
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()
	return sr, nil
}

func (t *toolVisModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	// Generate（genTitle 用）不记录可见性（Tools 面只在主循环 Stream 传）
	if _, err := t.Stream(ctx, in, opts...); err != nil {
		return nil, err
	}
	return t.next(), nil
}

// hasTool 轮可见清单含工具名。
func hasTool(round []string, name string) bool {
	for _, n := range round {
		if n == name {
			return true
		}
	}
	return false
}

// TestToolSearchLoadAndApprovalNotExempt 动态装载全链：①首两轮动态写工具
// 不可见、常驻读工具与 tool_search 恒可见 ②tool_search 命中后加载（sticky）
// ③动态写工具调用在 manual 档仍中断审批（分流上游包装——ArgsForce/模式
// 审批不豁免）④批准后执行收口。
func TestToolSearchLoadAndApprovalNotExempt(t *testing.T) {
	calls := 0
	rt, _ := tools.InferTool("read_core", "常驻读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	wt, _ := tools.InferTool("write_dyn", "动态写桩：批量归档旧数据", func(context.Context, struct{}) (map[string]any, error) {
		calls++
		return map[string]any{"ok": true}, nil
	})
	fm := &toolVisModel{script: func(n int) *schema.Message {
		switch n {
		case 1: // 首轮：只见常驻面 + tool_search（动态工具隐藏）
			return schema.AssistantMessage("", []schema.ToolCall{
				tcOf("s1", "tool_search", `{"query":"归档 写"}`)})
		case 2: // 检索命中加载后：调用动态写工具（manual 档应中断审批）
			return schema.AssistantMessage("", []schema.ToolCall{
				tcOf("w1", "write_dyn", `{}`)})
		default:
			return schema.AssistantMessage("完成", nil)
		}
	}}
	m, _ := newReductionManager(t, 0, []contract.Tool{rt, wt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	}, func(o *Options) {
		o.Approval = hitl.ApprovalConfig{WriteTools: map[string]bool{"write_dyn": true}}
		o.ToolSearchPolicy = &ToolSearchPolicy{DynamicTools: []string{"write_dyn"}}
	})
	s := m.Registry().Create("张三", "检索", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var names []string
	var card *contract.ApprovalReq
	m.Run(context.Background(), s, "归档", nil, func(ev session.Event) {
		names = append(names, ev.Event)
		if ev.Event == contract.EvApprovalRequest {
			req := ev.Data.(contract.ApprovalReq)
			card = &req
		}
	})
	t.Cleanup(func() { stopApprovalTimer(s.SID) })

	if card == nil {
		t.Fatalf("动态写工具调用应中断审批（分流上游包装不豁免），事件 %v", names)
	}
	if card.Tool != "write_dyn" {
		t.Fatalf("审批卡应为动态加载的 write_dyn，实得 %s", card.Tool)
	}
	// 批准 → 执行收口
	s.SetDecisionFor(card.Items[0].ItemID, contract.ApprovalDecision{Approve: true})
	m.Resume(context.Background(), s, func(session.Event) {})
	waitTitleFlight(t, s)
	if calls != 1 {
		t.Fatalf("批准后动态写工具应执行，实得 %d", calls)
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("应正常收口，终态 %s", s.StateOf())
	}

	// 可见性断言：逐轮检查（tool_search 轮 → 审批挂起前轮）
	if len(fm.seen) < 2 {
		t.Fatalf("至少两轮模型调用，实得 %d（%v）", len(fm.seen), fm.seen)
	}
	for i, round := range fm.seen[:2] {
		if !hasTool(round, "read_core") || !hasTool(round, "tool_search") {
			t.Fatalf("第 %d 轮常驻面与 tool_search 应恒可见：%v", i+1, round)
		}
	}
	if hasTool(fm.seen[0], "write_dyn") {
		t.Fatalf("首轮动态工具应不可见：%v", fm.seen[0])
	}
	if !hasTool(fm.seen[1], "write_dyn") {
		t.Fatalf("tool_search 命中后动态工具应加载（Run 内 sticky）：%v", fm.seen[1])
	}
}
