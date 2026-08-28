package engine

// H5-1 拓扑装配回归：supervisor/deep 两形态端到端（子 agent 零审批直执红线
// 表波及面——supervisor/deep 派的子 agent 同样 auto 档直落）+ compose 确定性
// 流水线装配示例（采集/索引类无 LLM 场景的原语用法）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/einoext"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// topologySubModel 拓扑子模型剧本：首调白名单内写工具（零审批直執断言面）、
// 续调文本结论。
type topologySubModel struct {
	done  bool
	calls int
	reply string
}

func (t *topologySubModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	t.calls++
	if !t.done {
		t.done = true
		return schema.AssistantMessage("", []schema.ToolCall{tcOf("tc1", "write_tool", `{}`)}), nil
	}
	return schema.AssistantMessage(t.reply, nil), nil
}

func (t *topologySubModel) Stream(ctx context.Context, in []*schema.Message, o ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := t.Generate(ctx, in, o...)
	if err != nil {
		return nil, err
	}
	sr, sw := schema.Pipe[*schema.Message](1)
	sw.Send(msg, nil)
	sw.Close()
	return sr, nil
}

// runTopology 拓扑会话跑通（manual 档——主面写会审批，子面零审批直执才是
// 断言面）。mainFirst/mainThen = 主模型两段剧本（派发/收口）。
func runTopology(t *testing.T, kind, mainTool, mainArgs string, sub *topologySubModel) (*session.Session, *topologySubModel, []string) {
	t.Helper()
	calls := 0
	wt, _ := tools.InferTool("write_tool", "写桩", func(context.Context, struct{}) (map[string]any, error) {
		calls++
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("m1", mainTool, mainArgs)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "主收口"})
	}}
	n := 0
	m, _ := newReductionManager(t, 0, []contract.Tool{wt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 { // 构造序：主(1) → 拓扑子(2)
			return sub, nil
		}
		return fm, nil
	}, func(o *Options) {
		o.Approval = hitl.ApprovalConfig{WriteTools: map[string]bool{"write_tool": true}} // manual 主面会审批——子面 auto 直执才是断言面
		o.Topology = &TopologyConfig{
			Kind: kind,
			SubAgents: []SubAgentSpec{{
				Name: "researcher", Description: "勘察专家",
				Tools: []string{"write_tool"},
			}},
		}
	})
	s := m.Registry().Create("张三", "拓扑", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var evNames []string
	m.Run(context.Background(), s, "派拓扑", nil, func(ev session.Event) { evNames = append(evNames, ev.Event) })
	waitTitleFlight(t, s)
	_ = calls
	return s, sub, evNames
}

// TestSupervisorTopologyE2E supervisor 拓扑：主经 transfer_to_agent 派子 →
// 子零审批直执写工具（manual 档主面会审批，子面不卡）→ 回主收口。
func TestSupervisorTopologyE2E(t *testing.T) {
	sub := &topologySubModel{reply: "子结论：勘察完成"}
	s, sb, evNames := runTopology(t, TopologySupervisor, "transfer_to_agent", `{"agent_name":"researcher"}`, sub)
	if sb.calls < 2 {
		t.Fatalf("子 agent 应执行（工具轮+结论轮），实得 %d 调", sb.calls)
	}
	for _, n := range evNames {
		if n == contract.EvApprovalRequest {
			t.Fatal("拓扑子代理零审批违例：manual 档下子写工具直执不得触发审批卡")
		}
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("supervisor 拓扑应正常收口，终态 %s（事件 %v）", s.StateOf(), evNames)
	}
}

// TestDeepTopologyE2E deep 拓扑：主经 task{subagent_type} 派具名专家 →
// 子零审批直执 → 回主收口。
func TestDeepTopologyE2E(t *testing.T) {
	sub := &topologySubModel{reply: "专家结论：完成"}
	s, sb, evNames := runTopology(t, TopologyDeep, "task", `{"subagent_type":"researcher","description":"勘察"}`, sub)
	if sb.calls < 2 {
		t.Fatalf("子 agent 应执行（工具轮+结论轮），实得 %d 调", sb.calls)
	}
	for _, n := range evNames {
		if n == contract.EvApprovalRequest {
			t.Fatal("deep 拓扑子代理零审批违例")
		}
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("deep 拓扑应正常收口，终态 %s（事件 %v）", s.StateOf(), evNames)
	}
}

// TestTopologySubFailFeed 拓扑子 agent 恒败（H9-5）：adk 裸形态此场景 =
// 子 run Err 事件上抛杀整个 run（08-27 实锚）；subAgentFailFeed 包装后应
// 「完成+错误结论」回喂——父运行不死（StateEnded）+ 主模型收口轮输入可见
// 错误结论 + 无 panic。supervisor（transfer 路径）/deep（task 工具路径）双形态。
func TestTopologySubFailFeed(t *testing.T) {
	run := func(t *testing.T, kind, mainTool, mainArgs string) {
		t.Helper()
		wt, _ := tools.InferTool("write_tool", "写桩", func(context.Context, struct{}) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		})
		fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
			if n == 1 {
				send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
					tcOf("m1", mainTool, mainArgs)}})
				return
			}
			send(&schema.Message{Role: schema.Assistant, Content: "主收口"})
		}}
		n := 0
		m, _ := newReductionManager(t, 0, []contract.Tool{wt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			n++
			if n == 2 { // 构造序：主(1) → 拓扑子(2)
				return &failSubModel{err: errors.New("子模型不可用")}, nil
			}
			return fm, nil
		}, func(o *Options) {
			o.Topology = &TopologyConfig{
				Kind:      kind,
				SubAgents: []SubAgentSpec{{Name: "researcher", Description: "勘察专家"}},
			}
		})
		s := m.Registry().Create("张三", "拓扑败", "manual", contract.UserPrefs{Model: "p/m"})
		s.SetState(session.StateRunning)
		m.Run(context.Background(), s, "派拓扑", nil, func(session.Event) {})
		waitTitleFlight(t, s)

		if s.StateOf() != session.StateEnded {
			t.Fatalf("%s：子 agent 恒败不得杀父运行，终态 %s", kind, s.StateOf())
		}
		if len(fm.inputs) < 2 {
			t.Fatalf("%s：主模型应至少两调（派发+收口），实得 %d", kind, len(fm.inputs))
		}
		var joined strings.Builder
		for _, msg := range fm.inputs[len(fm.inputs)-1] {
			joined.WriteString(msgTextOf(msg))
		}
		if !strings.Contains(joined.String(), "子代理执行失败") {
			t.Fatalf("%s：错误结论应回喂主模型可见：%.160s", kind, joined.String())
		}
	}
	run(t, TopologySupervisor, "transfer_to_agent", `{"agent_name":"researcher"}`)
	run(t, TopologyDeep, "task", `{"subagent_type":"researcher","description":"勘察"}`)
}

// failSubModel 拓扑子模型恒败（H9-5 失败源）。
type failSubModel struct{ err error }

func (f *failSubModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, f.err
}

func (f *failSubModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, f.err
}

// TestDeterministicPipelineCompose compose 确定性流水线装配示例（H5-1：
// 采集/索引类无 LLM 场景——contract 工具经 einoext.Adapt 入 compose 链，
// 固定序执行非 agent 自主面）。
func TestDeterministicPipelineCompose(t *testing.T) {
	extract, _ := tools.InferTool("extract", "抽取", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"items": []string{"a", "b", "c"}}, nil
	})
	indexed := 0
	idx, _ := tools.InferTool("index", "索引", func(_ context.Context, in struct {
		Items []string `json:"items"`
	}) (map[string]any, error) {
		indexed = len(in.Items)
		return map[string]any{"count": len(in.Items)}, nil
	})
	ts := einoext.Adapt([]contract.Tool{extract, idx})
	step1 := ts[0].(tool.InvokableTool)
	step2 := ts[1].(tool.InvokableTool)

	ctx := context.Background()
	chain := compose.NewChain[string, string]()
	chain.
		AppendLambda(compose.InvokableLambda(func(ctx context.Context, _ string) (string, error) {
			return step1.InvokableRun(ctx, `{}`)
		})).
		AppendLambda(compose.InvokableLambda(func(ctx context.Context, prev string) (string, error) {
			return step2.InvokableRun(ctx, prev)
		}))
	r, err := chain.Compile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Invoke(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	if indexed != 3 || !strings.Contains(out, `"count":3`) {
		t.Fatalf("确定性流水线应固定序贯通（抽取 3 项 → 索引计数 3），实得 indexed=%d out=%s", indexed, out)
	}
}
