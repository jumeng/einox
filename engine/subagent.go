package engine

// spawn 子代理（Phase H2，多 agent 最小面；方案 = findings/2026-08-26-h2-spawn-plan.md）：
// 执行体 = adk agent_tool 包装子 ChatModelAgent（eino 原生循环，零自研）——
// 默认输入即 request 单串 = 独立上下文；子事件不进父 runSession/checkpoint；
// 子面零审批直执（auto 档）——子审批中断经 CompositeInterrupt 穿透父审批链
// 仅作异常路径防御（approvalCardOf 根因链提取），正常形态不发生。
// spawn 壳参数 {task, tools?, expect?} 经 WithAgentInputSchema 定义，整段 JSON
// 成为子代理 user 消息（子提示词解析）。红线③：数据域写不经子代理——白名单
// 装配层硬筛（写工具不进白名单即物理不可达）；hitl 包装子面同样生效
// （ArgsForce 拓扑无关）。失败显式回传：interrupt 错误原样穿透，其余转
// errFeed JSON 回喂父模型（不杀父运行）。应用不注入 SubAgents = 不装配
// spawn（产品当前形态零变化）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/einoext"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/mid"
	"github.com/jumeng/einox/session"
)

// SubAgentsConfig 子代理装配（engine.Options.SubAgents；nil = 不装配 spawn）。
type SubAgentsConfig struct {
	Tools         []string // 工具白名单（全量面按名筛；内容归业务——数据域写工具不列入）
	DenyTools     []string // 硬拒名单（H9-6 纪律升机制：与白名单交集非空或含全量面未同名 → 装配期 configError；不配 = 行为不变）
	Instruction   string   // 子提示词（空 = 引擎缺省模板）
	Model         string   // 模型覆写复合键（空 = 随父会话快照）
	MaxConcurrent int      // 并发 spawn 上限（0 = 不限）
	// EmitEvents 全量转发档（H8-2 事件折叠三档之一，默认关）：开 = 子代理
	// 内部事件经泵翻译为 EvSubAgent 转发父流（只读流——子过程不进父上下文，
	// 折叠卡展开态消费；并行多 spawn 同名事件交错，per-invocation 归组为
	// 升级位）。关 = 静默/折叠一行（spawn 的 EvToolCall/EvToolResult 即卡面）。
	EmitEvents bool
}

// spawnToolName 父模型见的工具名（= 子 agent Name）。
const spawnToolName = "spawn"

// subEventsOn 全量转发档开启判定（装配态；pump 内事件过滤用）。
func (m *Manager) subEventsOn() bool {
	return m.Opt.SubAgents != nil && m.Opt.SubAgents.EmitEvents
}

// emitSubAgent 子代理内部消息 → EvSubAgent 只读流（H8-2）：text 增量 /
// 完整 tool_call / 工具结果摘要；思考流不转发（不倾倒原则——展开态是折叠
// 日志非逐字动画）。agent = 子代理名（spawn/拓扑子）；calls = 本泵 callID →
// 工具名配对表（tool_result 只带 callID，工具名经此回填——契约语义）。流式
// 变体本分支独占排空（主流不见内事件）；流式 text 累积整段单发（不逐 chunk
// 倾倒事件流）。spawnID = per-invocation 实例键（W-1：后台派生泵必带；
// 同步父泵转发事件无调用标识，传零值——前端回退「归最近」启发式）。
// last 非 nil 时记录末段 assistant 文本（覆盖式——后台泵取结论用，多轮
// 工具循环只留最后一段；**本函数是流的唯一消费者**，关档路径改用
// recordLastText 自行排空——流不可双读）。
func (m *Manager) emitSubAgent(s *session.Session, fn emitFn, agent string, calls map[string]string, v *adk.TypedMessageVariant[*schema.Message], spawnID string, last *string) {
	switch v.Role {
	case schema.Tool:
		var content, callID string
		if v.IsStreaming && v.MessageStream != nil {
			var b strings.Builder
			for {
				chunk, err := v.MessageStream.Recv()
				if err != nil {
					break
				}
				if chunk != nil {
					b.WriteString(chunk.Content)
					if chunk.ToolCallID != "" {
						callID = chunk.ToolCallID
					}
				}
			}
			content = b.String()
		} else if v.Message != nil {
			content, callID = v.Message.Content, v.Message.ToolCallID
		}
		ok, _, _ := mid.ToolResultDigest(content) // 失败信封/非零 exit → 红（与主流 ToolResult 同判定）
		m.emit(s, fn, contract.EvSubAgent, contract.SubAgentEvent{
			SpawnID: spawnID, Agent: agent, Kind: "tool_result", Tool: calls[callID],
			Text: truncateRunes(content, 200), OK: ok,
		})
	case schema.Assistant:
		emitText := func(t string) {
			if t != "" {
				if last != nil {
					*last = t // 末段覆盖（结论=最后一段非空 assistant 文本）
				}
				m.emit(s, fn, contract.EvSubAgent, contract.SubAgentEvent{SpawnID: spawnID, Agent: agent, Kind: "text", Text: t})
			}
		}
		emitCall := func(tc schema.ToolCall) {
			if tc.ID == "" {
				return // 流式碎片（Index 无 ID）
			}
			calls[tc.ID] = tc.Function.Name // 配对表：后续 tool_result 回填工具名
			m.emit(s, fn, contract.EvSubAgent, contract.SubAgentEvent{
				SpawnID: spawnID, Agent: agent, Kind: "tool_call", Tool: tc.Function.Name,
				Args: mid.ArgsDigest(tc.Function.Arguments),
			})
		}
		if v.IsStreaming && v.MessageStream != nil {
			var text strings.Builder
			for {
				chunk, err := v.MessageStream.Recv()
				if err != nil {
					var wr *adk.WillRetryError
					if errors.As(err, &wr) {
						// 网络容错 ②：重试在途——半截段弃发（重试尝试整段重发，
						// 防子代理卡文本重复；重试通知归父主流，子面不重复发）
						text.Reset()
					}
					break
				}
				if chunk == nil {
					continue
				}
				text.WriteString(chunk.Content)
				for _, tc := range chunk.ToolCalls {
					emitCall(tc)
				}
			}
			emitText(text.String())
			return
		}
		if v.Message == nil {
			return
		}
		emitText(v.Message.Content)
		for _, tc := range v.Message.ToolCalls {
			emitCall(tc)
		}
	}
}

// 写回聚合围栏记号（H4-2b 提示词约定案：原始字符串不能含反引号，经拼接注入）。
const (
	wsOpen  = "```write-suggestions"
	wsClose = "```"
)

// subInstruction 缺省子提示词（任务载荷解析/工具纪律/结论形态 + 写回聚合
// 产出约定——H4-2b 提示词约定案：子面无数据域写权限，涉及数据变更以写建议
// 清单回传、父用自有写工具代执行走父审批）。
const subInstruction = `你是被父代理派发的子代理，独立执行一个子任务。
请求载荷是 JSON：{"task": 任务描述, "tools": 本次任务声明可用的工具（逗号分隔，可选）, "expect": 预期产物（可选）}。
纪律：
- 只使用任务声明的工具（未声明则不调工具，直接尽力作答）
- 任务所述之外的写操作一律不做
- 你没有业务数据域的写权限（创建/修改需求、文档、配置等）：任务涉及此类
  变更时不要尝试直接写——在结论末尾以 ` + wsOpen + ` 围栏给出写建议清单，
  由父代理代为执行：
  ` + wsOpen + `
  [{"tool": "父代理可用工具名", "args": {该工具完整入参 JSON}}, ...]
  ` + wsClose + `
  tool 名严格按父代理的工具命名；无数据变更建议则不带此块
- 完成后输出面向发起方的结论：结果摘要 + 关键产物路径/指针（如有）；简洁，不复述过程`

// spawnParam spawn 工具入参 schema（tools 逗号分隔——LLM 友好且 schema 面最简；
// background = 后台派生开关 W-2：true 即回 agentId、父回合继续、完成通知回传）。
var spawnParam = schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
	"task":       {Desc: "子任务简报（目标/约束/上下文），子代理独立执行", Required: true, Type: schema.String},
	"tools":      {Desc: "本次任务声明可用的工具名，逗号分隔（可选；装配白名单外的名字不可用）", Type: schema.String},
	"expect":     {Desc: "预期产物（可选，如：路径/清单/结论形态）", Type: schema.String},
	"background": {Desc: "后台派生（可选，默认 false 同步等待）：true = 立即返回 agentId、父任务继续，子代理完成后以通知注入结论——适合长任务且父还有并行工作；等待期间勿轮询勿 sleep", Type: schema.Boolean},
})

// filterSubTools 子面工具筛（spawn/拓扑共用）：白名单 ∩ 全量面；DenyTools
// 装配期硬校验（H9-6）——白名单是可达性闸，DenyTools 是红线第二道闸（应用
// 显式声明数据域写/repo 写，基座无业务域信息不猜 IsWrite）；名单含全量面
// 不存在的名字或与白名单交集非空 = configError（dsh tools.restrict 未知名
// loud validation 同语义，防拼写错静默失效）。
func filterSubTools(ts []contract.Tool, allow, deny []string) ([]contract.Tool, error) {
	if len(deny) > 0 {
		known := map[string]bool{}
		for _, t := range ts {
			if info := t.Info(); info != nil {
				known[info.Name] = true
			}
		}
		denySet := make(map[string]bool, len(deny))
		for _, n := range deny {
			if !known[n] {
				return nil, &configError{"DenyTools 含全量面不存在的工具名（拼写错？）：" + n}
			}
			denySet[n] = true
		}
		for _, n := range allow {
			if denySet[n] {
				return nil, &configError{"子面白名单与 DenyTools 交集：" + n + "——意图冲突，请修正装配"}
			}
		}
	}
	names := make(map[string]bool, len(allow))
	for _, n := range allow {
		names[n] = true
	}
	var out []contract.Tool
	for _, t := range ts {
		if info := t.Info(); info != nil && names[info.Name] {
			out = append(out, t)
		}
	}
	return out, nil
}

// newSpawnTool 构造 spawn 工具（eino tool.BaseTool——与 einoext.Adapt 产物同列
// 直接入 ToolsNode）。ts = 全量工具面（白名单筛选源）；providers = 模型解析。
// W-2 后：外层 bgSpawnTool 按 background 参数分叉——false/缺省走同步链路
// （throttledSpawn[会话域信号量] → spawnFailFeed → agent_tool，H2 语义零变化）；
// true 走 spawnbg.go 后台派生（bg 档子代理懒构造，与同步面共享 subTs 白名单）。
func (m *Manager) newSpawnTool(ctx context.Context, s *session.Session, cfg *SubAgentsConfig, ts []contract.Tool) (tool.BaseTool, error) {
	// 白名单硬筛 + DenyTools 装配期硬校验（子面 = 全量面 ∩ 名单 − 拒绝面）
	subTs, err := filterSubTools(ts, cfg.Tools, cfg.DenyTools)
	if err != nil {
		return nil, err
	}
	// 子模型：覆写键 ?? 父快照；与父同链包装（vision/shape）+ reduction 同挂
	key := cfg.Model
	if key == "" {
		key = s.Model.Model
	}
	providers := m.Opt.Providers()
	p, spec, found := llm.FindSpec(providers, key)
	if !found {
		return nil, &configError{"子代理模型不在可用清单内：" + key}
	}
	subCM, err := m.Opt.NewModel(ctx, p, spec, s.Model.Effort)
	if err != nil {
		return nil, &configError{"子代理模型构造失败：" + err.Error()}
	}
	subCM = llm.NewVisionModel(subCM, spec, m.Opt.ImageResolve)
	subCM = llm.NewHistoryShapeModel(subCM, p.Kind)

	instruction := subInstruction
	if cfg.Instruction != "" {
		instruction = cfg.Instruction
	}
	// buildSub 子代理构造闭包（同步 "auto" / 后台 "bg" 两档——bg 档 ArgsForce
	// 拒绝回喂不挂起，hitl fail-closed；白名单/提示词/模型/reduction 全共享）
	window := windowOf(spec)
	buildSub := func(bctx context.Context, mode string) (*adk.ChatModelAgent, error) {
		conf := &adk.ChatModelAgentConfig{
			Name:             spawnToolName,
			Description:      "派发子代理独立执行子任务（独立上下文，适合勘察/检索/读仓等并行工作），回传结论",
			Instruction:      instruction,
			Model:            subCM,
			MaxIterations:    maxIterations,
			ModelRetryConfig: m.modelRetryConfig(), // 网络容错 ②：与主链同策略
		}
		if len(subTs) > 0 {
			// 子代理零审批直执（2026-08-26 裁决，codex 对齐：delegates require
			// approval policy never）：auto/bg 档直落——子代理在父的工具调用内
			// （或后台 goroutine 中）同步运行，中途等审批 = 子任务卡死等人（并行
			// 场景更糟），委派的无人值守价值归零。ArgsForce 参数级强制审批仍先生效
			// （hitl 判定先于 mode 分支，红线「任何拓扑不豁免」）：同步路径中断走
			// 父审批链（CompositeInterrupt 防御）；后台路径 bg 档直接拒绝回喂
			// （fail-closed——挂起无人决议宁可失败）。装配纪律：数据域写与 repo
			// 写工具不进子面白名单。
			wrapped := hitl.WrapTools(subTs, s, mode, m.Opt.Approval)
			conf.ToolsConfig = adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: einoext.Adapt(wrapped),
			}}
		}
		if mw, err := m.newReductionMiddleware(s, window, false); err != nil {
			return nil, err
		} else {
			conf.Handlers = append(conf.Handlers, mw)
		}
		return adk.NewChatModelAgent(bctx, conf)
	}

	syncAgent, err := buildSub(ctx, "auto")
	if err != nil {
		return nil, err
	}
	at := adk.NewAgentTool(ctx, syncAgent, adk.WithAgentInputSchema(spawnParam))
	it, ok := at.(tool.InvokableTool) // agent_tool 恒为 invokable（直跑 + 中断桥接走此面）
	if !ok {
		return nil, fmt.Errorf("agent_tool 未实现 InvokableTool（eino 形态变更，需适配）")
	}
	var inner tool.InvokableTool = &spawnFailFeed{inner: it}
	if cfg.MaxConcurrent > 0 {
		// 会话域信号量（W-2 修正：原 per-Run chan 跨轮后台任务会无界累积）——
		// 同步/后台同一池，同步保留额由 bgGate 保证（见 spawnbg.go）
		reg := m.spawnReg(s, cfg.MaxConcurrent)
		inner = &throttledSpawn{inner: it, sem: reg.sem}
	}
	return &bgSpawnTool{m: m, s: s, cfg: cfg, inner: inner, buildSub: buildSub}, nil
}

// spawnFailFeed 失败语义包装：interrupt 错误原样穿透（审批/提问续流链路），
// 其余转 errFeed JSON 回喂父模型——显式回传不静默、不杀父运行。
type spawnFailFeed struct{ inner tool.InvokableTool }

func (f *spawnFailFeed) Info(ctx context.Context) (*schema.ToolInfo, error) { return f.inner.Info(ctx) }

func (f *spawnFailFeed) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	out, err := f.inner.InvokableRun(ctx, args, opts...)
	if err == nil {
		return out, nil
	}
	var sig *adk.InterruptSignal
	if errors.As(err, &sig) {
		return "", err // 中断原样穿透（审批/提问续流链路）
	}
	b, _ := json.Marshal(map[string]any{"ok": false, "error": "子代理执行失败：" + err.Error()})
	return string(b), nil
}

// throttledSpawn 并发限流包装（信号量 per-Run；超限阻塞排队）。
type throttledSpawn struct {
	inner tool.InvokableTool
	sem   chan struct{}
}

func (t *throttledSpawn) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

func (t *throttledSpawn) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	f := &spawnFailFeed{inner: t.inner}
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
		return f.InvokableRun(ctx, args, opts...)
	case <-ctx.Done():
		return "", fmt.Errorf("spawn 等待并发额度被取消：%w", ctx.Err())
	}
}

// windowOf 模型上下文窗口（0/未知 = reduction 只截断不清除）。
func windowOf(spec llm.ModelSpec) int {
	if spec.Limit != nil {
		return spec.Limit.Context
	}
	return 0
}
