package engine

// H5-1 确定性场景多 agent 拓扑装配（adk prebuilt 接线——实现序铁律：官方件
// 优先，einox 零自研循环）。红线表（设计基线 §3.2）对拓扑内子 agent 全量
// 生效：白名单硬筛（数据域写与 repo 写工具不进子面——装配纪律）、零审批
// 直执（auto 档）、模型随父同链包装（vision/shape/reduction）。
//
// 形态两档：
//   - supervisor：官方 prebuilt/supervisor（agent transfer 全上下文共享）。
//     官方标注 NOT RECOMMENDED（无子代理隔离，实证未更优）——确定性场景
//     选配；自主编排主线仍是单 agent react + spawn（H2，AgentAsTool）。
//   - deep：官方 prebuilt/deep（task_tool 具名专家委派，官方推荐线）；
//     WithoutWriteTodos/WithoutGeneralSubAgent 双关（einox 自有 todo_write
//     会话域件与 spawn 通用委派，去重不叠床）。
//
// 应用不注入 Options.Topology = 单 agent react 既有主线，产品零变化。

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	"github.com/cloudwego/eino/adk/prebuilt/supervisor"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// 拓扑形态常量（TopologyConfig.Kind）。
const (
	TopologySupervisor = "supervisor"
	TopologyDeep       = "deep"
)

// SubAgentSpec 拓扑子 agent 定义（装配注入：白名单/提示词/模型覆写）。
type SubAgentSpec struct {
	Name        string
	Description string
	Instruction string   // 空 = 引擎缺省子提示词（subInstruction 同款）
	Tools       []string // 工具白名单（红线：数据域写与 repo 写工具不进）
	DenyTools   []string // 硬拒名单（H9-6：交集/未同名装配期 configError，与 spawn 同闸）
	Model       string   // 模型覆写复合键（空 = 随父会话快照）
}

// TopologyConfig 拓扑装配（engine.Options.Topology；nil = 不装配）。
type TopologyConfig struct {
	Kind      string
	SubAgents []SubAgentSpec
}

// supervisorMainName supervisor 形态主 agent 名。adk 的 transfer 派发按
// Name 寻址：supervisor.New 取主 agent 名作子代理转回目标，未命名 = 转回
// 空名 agent → runner 静默收线，子的结论永远到不了主模型（2026-08-27 部署机
// 首跑实锚——happy 路径只断言子执行/零审批故长期漏网）。泵的子事件过滤
// （manager.pump H8-2）排除本名：主 agent 命名后其输出仍走父主流/父历史。
const supervisorMainName = "supervisor"

// newTopologySub 构造拓扑子 agent（两形态共用）：白名单硬筛 + DenyTools
// 装配期硬校验 + auto 直落（零审批裁决）+ 同链模型包装 + reduction（子上下
// 文同窗口压力）+ 失败回喂包装（subAgentFailFeed——H9-5）。
func (m *Manager) newTopologySub(ctx context.Context, s *session.Session, spec SubAgentSpec, ts []contract.Tool) (*adk.ChatModelAgent, error) {
	subTs, err := filterSubTools(ts, spec.Tools, spec.DenyTools)
	if err != nil {
		return nil, err
	}
	key := spec.Model
	if key == "" {
		key = s.Model.Model
	}
	providers := m.Opt.Providers()
	p, mspec, found := llm.FindSpec(providers, key)
	if !found {
		return nil, &configError{"拓扑子代理模型不在可用清单内：" + key}
	}
	cm, err := m.Opt.NewModel(ctx, p, mspec, s.Model.Effort)
	if err != nil {
		return nil, &configError{"拓扑子代理模型构造失败：" + err.Error()}
	}
	cm = llm.NewVisionModel(cm, mspec, m.Opt.ImageResolve)
	cm = llm.NewHistoryShapeModel(cm, p.Kind)
	conf := &adk.ChatModelAgentConfig{
		Name: spec.Name, Description: spec.Description,
		Instruction: spec.Instruction, Model: cm, MaxIterations: maxIterations,
		ModelRetryConfig: m.modelRetryConfig(), // 网络容错 ②：与主链同策略
	}
	if conf.Instruction == "" {
		conf.Instruction = subInstruction
	}
	if len(subTs) > 0 {
		// 零审批直执（§3.2 红线表：supervisor/deep 派的子 agent 同样适用——
		// 中途等审批 = 子任务卡死等人）；ArgsForce 参数级强制仍先于 mode 分支。
		// ToolWrap 与主面同序同挂（wrapFace）。幻觉工具兜底与主面同策略。
		conf.ToolsConfig = adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: m.wrapFace(subTs, s, "auto"), UnknownToolsHandler: newUnknownToolHandler(contractToolNames(subTs), nil),
		}}
	}
	if mw, err := m.newReductionMiddleware(s, windowOf(mspec), false); err != nil {
		return nil, err
	} else {
		conf.Handlers = append(conf.Handlers, mw)
	}
	return adk.NewChatModelAgent(ctx, conf)
}

// buildTopology 按形态组装（agConf = 主 agent 已装配完的配置——模型/指令/
// 工具面/中间件全量复用；两形态只换组织方式）。
func (m *Manager) buildTopology(ctx context.Context, s *session.Session, agConf *adk.ChatModelAgentConfig, ts []contract.Tool) (adk.ResumableAgent, error) {
	cfg := m.Opt.Topology
	subs := make([]adk.Agent, 0, len(cfg.SubAgents))
	for _, spec := range cfg.SubAgents {
		if spec.Name == supervisorMainName {
			return nil, &configError{"子代理名不得为 " + supervisorMainName + "（supervisor 形态主 agent 保留名）"}
		}
		sub, err := m.newTopologySub(ctx, s, spec, ts)
		if err != nil {
			return nil, err
		}
		// 失败回喂包装（H9-5）：子 run 普通错误不杀整个拓扑 run
		subs = append(subs, &subAgentFailFeed{inner: sub})
	}
	switch cfg.Kind {
	case TopologySupervisor:
		agConf.Name = supervisorMainName // 转回寻址依据（见常量注释）
		main, err := adk.NewChatModelAgent(ctx, agConf)
		if err != nil {
			return nil, err
		}
		return supervisor.New(ctx, &supervisor.Config{Supervisor: main, SubAgents: subs})
	case TopologyDeep:
		return deep.New(ctx, &deep.Config{
			Name:         "deep",
			ChatModel:    agConf.Model,
			Instruction:  agConf.Instruction,
			ToolsConfig:  agConf.ToolsConfig,
			MaxIteration: agConf.MaxIterations,
			SubAgents:    subs,
			// einox 自有 todo_write（会话域件）与 spawn（通用委派）——关内置
			// 双件去重；deep 形态 = 具名专家委派面
			WithoutWriteTodos:      true,
			WithoutGeneralSubAgent: true,
			Handlers:               agConf.Handlers, // steering/skills/reduction/summarization 同挂
		})
	}
	return nil, &configError{"未知拓扑形态：" + cfg.Kind + "（supervisor|deep）"}
}

// subAgentFailFeed 拓扑子 agent 失败语义包装（H9-5）：adk 现状 = 子 run 以
// Err 事件收尾时原样上抛（agent_tool 事件循环遇 Err 直接 return、transfer
// 子事件原样转发——杀整个 run，2026-08-27 实锚）；本包装在事件流层把普通
// error 收尾转换为「完成+错误结论」的最终 assistant 消息——上层（deep
// task_tool / supervisor transfer）将其作为子代理结论收入父上下文，父运行
// 不死。spawn 主线 spawnFailFeed 的 agent 层同款。中断（InterruptSignal）与
// ctx 取消原样穿透（审批/提问续流链路、级联取消）。
type subAgentFailFeed struct {
	inner adk.ResumableAgent
}

func (w *subAgentFailFeed) Name(ctx context.Context) string        { return w.inner.Name(ctx) }
func (w *subAgentFailFeed) Description(ctx context.Context) string { return w.inner.Description(ctx) }

func (w *subAgentFailFeed) Run(ctx context.Context, input *adk.AgentInput, opts ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return feedSubAgentErr(w.inner.Run(ctx, input, opts...))
}

func (w *subAgentFailFeed) Resume(ctx context.Context, info *adk.ResumeInfo, opts ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	return feedSubAgentErr(w.inner.Resume(ctx, info, opts...))
}

// feedSubAgentErr 事件流错误转回喂：Err 事件 → 一条带错误结论的正常输出
// 事件（上层消费者按子代理最终输出收尾回喂父模型）；穿透面 = 中断与 ctx
// 取消（不吞审批续流与级联取消）。
func feedSubAgentErr(it *adk.AsyncIterator[*adk.AgentEvent]) *adk.AsyncIterator[*adk.AgentEvent] {
	out, gen := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer gen.Close()
		for {
			ev, ok := it.Next()
			if !ok {
				return
			}
			var sig *adk.InterruptSignal
			if ev.Err == nil || errors.As(ev.Err, &sig) ||
				errors.Is(ev.Err, context.Canceled) || errors.Is(ev.Err, context.DeadlineExceeded) {
				gen.Send(ev)
				continue
			}
			msg := schema.AssistantMessage(
				fmt.Sprintf("子代理执行失败：%v（结论不可用——按错误反馈调整或重派，不中断其余工作）", ev.Err), nil)
			gen.Send(&adk.AgentEvent{
				AgentName: ev.AgentName,
				RunPath:   ev.RunPath,
				Output:    &adk.AgentOutput{MessageOutput: &adk.MessageVariant{Role: schema.Assistant, Message: msg}},
			})
		}
	}()
	return out
}
