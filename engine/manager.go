// Package engine 是 einox 循环引擎（自产品 internal/agent/agent.go 迁入泛化）：
// 每轮 Run 组装——Providers 解析 → NewModel 构造 ChatModel（测试可注入假
// 模型）→ adk ChatModelAgent（Instruction + 工具面 [hitl 审批包装 × einoext
// 适配] + skill middleware）→ Runner（EnableStreaming + CheckPoints 注入）→
// Run + WithCheckPointID（续聊 = 会话域 History 回传，checkpoint 只承担中断/
// 取消恢复）→ 事件泵分类为契约事件族。应用注入面 = Options（提示词内容/
// 工具面/审批配置/模型解析/工作区根——机制归基座，内容归业务）。
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/dynamictool/toolsearch"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"encoding/json"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/einoext"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/mid"
	"github.com/jumeng/einox/sandbox"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/skills"
	"github.com/jumeng/einox/tools/egress"
	"github.com/jumeng/einox/tools/repo"
)

// CheckPointStore 会话检查点存储面（adk Get/Set 同构——结构直配 Runner）。
type CheckPointStore interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, checkpoint []byte) error
}

// SessionBrief 会话概要（Instruction / Tools / SkillsDir 组装入参——三件套
// 随消息可变：mode 每条消息可带，model/effort 会话内可切换，运行边界生效；
// Owner/SID 会话身份：工具面与 skill 目录按租户裁剪的寻址键）。
type SessionBrief struct {
	Mode   string
	Model  string // 复合键 provider/model
	Effort string
	Owner  string // 会话归属用户
	SID    string // 会话标识
}

// Options 引擎组装配置（应用装配层构造）。
type Options struct {
	// Providers 模型解析（组装期调用；空清单 = 未配置模型错误面）。
	Providers func() []llm.ProviderSpec
	// Instruction 系统提示词（应用内容——业务职责段 + 通用段 + 会话配置段 +
	// 模式段拼装归应用；入参 = 会话配置概要，每轮 assemble 实时注入）。
	Instruction func(sess SessionBrief) string
	// Tools 业务工具面（实现 contract.Tool；入参 = 会话概要——多租户按
	// Owner 裁剪工具面、按会话身份定制；nil = 无业务工具）。闭包每轮
	// assemble 求值、跨会话并发调用——应快速返回且无共享可变态。
	Tools func(sess SessionBrief) []contract.Tool
	// ProcessTools 进程级通用件（时间/网络等——应用选择加入的基座件）。
	ProcessTools func() []contract.Tool
	// SessionToolsOff 排除的会话域工具族（族名见 sessiontools.go 的族常量；
	// nil/空 = 全挂（零变化——族构造失败照常上抛 CONFIG，不静默吞错），未知
	// 名 NewManager 即拒——对齐 DenyTools 的 fail-fast 纪律）。repo 族不经此
	// 缝，仍由 RepoMounts 条件装配。
	// 裁 fs 族 = 放弃 reduction 外置换指针取回（外置指针经 read_file 虚拟
	// 路径取回，工具不在场则超长结果只剩截断头尾）——留装配者知情决策，
	// 引擎不联动（上游截断与外置在同一 handler 内一体，禁外置须复制其逻辑）。
	SessionToolsOff []string
	// ToolWrap 工具包装缝（契约层最外包装，挂 hitl 审批包装之外；主面与
	// 子代理面同挂——审计/脱敏/动态准入覆盖主面与子代理面的全部契约工具，
	// spawn 派发本体不经包装）。nil = 不包装，零行为变化。契约义务：
	//  1. Info() 须透传原名（名字是审批名单/子代理白名单/动态装载分流的
	//     寻址键）；
	//  2. 拒绝执行以 {"ok":false,"error":…} 信封返回回喂模型自纠（勿返回
	//     Go error——本缝在 errFeed 外层，Go error 会终止整轮且模型不可见）；
	//  3. 只能收紧不能放宽：收到的是已含审批的实例，透传即保留全部审批
	//     语义（ArgsForce/模式审批不可豁免——以会话域件为界成立，裸实例
	//     在引擎内不可得；业务工具的裸实例本就在应用手中，绕过属应用
	//     自毁）；伪造结果属违约；
	//  4. 包装随每次 assemble 重建（Run/Resume 各一次）——有状态包装的
	//     计数不跨轮；
	//  5. 勿从包装内发起 *contract.Suspend（引擎三卡分叉与决议消费链路
	//     未对应用开放）。
	ToolWrap func(t contract.Tool) contract.Tool
	// NewModel 模型构造口（缺省生产构造 llm.NewChatModel；测试注入假模型）。
	NewModel llm.ModelFactory
	// ImageResolve 图片引用解析（文档仓库路径 → 字节+MIME；nil = 图片不可用——
	// 含图请求即错误面。vision 包装在模型调用边界消费）。
	ImageResolve llm.ImageResolver
	// CheckPoints 会话检查点存储构造（operator+sid 定位）。
	CheckPoints func(operator, sid string) CheckPointStore
	// SkillsDir skill 物化目录（nil/空 = 不挂 skill middleware；物化归应用。
	// 入参 = 会话概要——按租户物化不同 skill 包；与 Tools 同契约：每轮
	// assemble 求值、并发安全）。
	SkillsDir func(sess SessionBrief) string
	// AgentsMD AGENTS.md 注入清单（nil/空清单 = 不挂零变化；绝对路径，按序
	// 注入）。发现逻辑归应用（ZCode 双层形态：用户级文件先、工作区级文件后
	// 收窄覆盖——两文件按序进清单即得）；跨会话记忆注入通道同走此缝（owner
	// 级记忆文件进清单）。与 SkillsDir 同契约：每轮 assemble 求值、并发安全。
	AgentsMD func(sess SessionBrief) []string
	// AgentsMDMaxBytes 注入字节预算（0 = 缺省 32KiB；上游按序装载超限即跳过
	// 余下文件——预算显式化，防提示词面失控）。
	AgentsMDMaxBytes int
	// ContextBudget 常驻上下文预算（token，口径 = estTokens 启发式）：Instruction
	// + 常驻工具面（业务面+进程件+会话域件+spawn：名+描述+参数 schema JSON）
	// 合计的超限告警线。0 = 缺省关（nil 纪律：零配置零变化；推荐值 8192 与
	// 调法见 docs/04）。超限动作 = harness_note（Kind: budget）+ 服务端日志、
	// 不阻断运行（大工具面配 toolsearch 就是合法超标场景）；会话内只发一次
	// （判定扫 Events 既有同 Kind note，跨重启天然不重发）。env
	// EINO_CONTEXT_BUDGET 可覆盖（对齐 EINO_MAX_ITERATIONS 惯例）。toolsearch
	// 名单内工具不进核算——动态装载正是瘦身手段，只有常驻面计费。
	ContextBudget int
	// Approval 审批配置（写工具名单/动作名/参数豁免——业务内容）。
	Approval hitl.ApprovalConfig
	// WorkspaceRoot 会话工作区根（用户域 workspaces/<sid>——repos/ 挂载
	// 持久、其余一轮一清；惰性创建）。
	WorkspaceRoot func(owner, sid string) string
	// SubAgents spawn 子代理装配（H2；nil = 不装配 spawn）。
	SubAgents *SubAgentsConfig
	// Topology 确定性场景多 agent 拓扑（H5：supervisor/deep 官方 prebuilt 接线；
	// nil = 单 agent react 既有主线。红线表对拓扑内子 agent 全量生效）。
	Topology *TopologyConfig
	// ToolSearchPolicy 动态工具装载（H7：名单外常驻、名单内经 tool_search
	// 检索后可见；nil = 全量常驻零变化。审批包装在分流上游——ArgsForce 与
	// 模式审批对动态工具不豁免）。
	ToolSearchPolicy *ToolSearchPolicy
	// RepoMounts 代码仓定位（repo 工具族解析器；nil = 不装配该族）。
	RepoMounts repo.Resolver
	// RepoPatchWriter 补丁导出落盘（export_patch 用；nil = 导出面报未配置）。
	RepoPatchWriter func(name string, content []byte, operator string) error
	// Sandbox run_command 沙箱策略（nil = 不沙箱——产品默认关 opt-in；
	// 机制 = einox/sandbox re-exec 哨兵协议，产品 main 需挂 RunHelper 钩子）。
	Sandbox *sandbox.Policy
	// SandboxProvider 沙箱后端（nil = sandbox.OSProvider 平台内建；容器类
	// 后端如 sandbox.DockerProvider 经此注入——与 Sandbox 正交：策略定
	// 「施加什么」，后端定「怎么施加」）。
	SandboxProvider sandbox.Provider
	// Egress 网络出口校验器（S-9：run_command 命令串 URL 预检；nil = 不
	// 预检零变化。webfetch 侧的注入在应用 ProcessTools（进程级工具归应用
	// 装配），此处只管引擎持有的命令面）。
	Egress *egress.Validator
	// SummarizerFallbackModels 摘要模型 Failover 降级链（H9-10：主摘要模型
	// 失败按序降级的复合键清单，逐个同链包装 vision/shape 后填 adk Failover；
	// 空 = 不配降级零变化（单端点装配配了也白配）；链尽走既有清窗兜底不外抛。
	// ShouldFailover 排除 ctx 取消/中断类、MaxRetries=链长——审查补差内建。
	SummarizerFallbackModels []string
	// FallbackModels 主对话模型 Failover 降级链（复合键清单；空 = 零变化）。
	// 重试先耗尽（有界重连）、RetryExhaustedError 触发按序换链上模型（每档
	// 各享完整重连预算）；粘滞上次成功模型归 adk。切换发 model_change 事件；
	// 致命类（401/403/402 配置错）不降级直接停机；ctx 取消/审批中断不降级。
	// 清单错配（键不在 Providers 内）不阻断运行：降级失效 + harness_note 留痕。
	// 子代理/拓扑子面不挂（链按主模型语境配置，维持 retry-only）。
	FallbackModels []string
	// Recall 跨会话检索工具（记忆拉通道，opt-in）：模型可读本 owner 历史会话
	// 的摘要与消息投影（三模式 sid 深读/query 检索/最近列表；恒排除当前会话、
	// 有界、摘要级——授权五律见 recall.go）。是新能力面：装配即知情决策，
	// false = 不装配零变化。条件装配先例 = repo 族之于 RepoMounts。
	Recall bool
	// TurnEpilogue 轮收尾交接钩子（记忆写通道，nil = 零变化）：自然收束
	// （StateEnded）每轮触发，载荷与 session_end 事件同源（摘要+文件变更）。
	// einox 的 session_end 是轮级——应用自行去重/节流。同步调用应快速返回，
	// 重提取（LLM 蒸馏/外部写）归应用异步；panic 由引擎兜底不影响终态。
	// 最小用法：把摘要追加进 owner 域记忆 markdown，经 AgentsMD 清单注入。
	TurnEpilogue func(sum TurnEndSummary)
	// FinalGate 收束质量门（nil = 零变化）：自然收束后、终态落盘前按
	// GateConfig.Checkers 强制验证——失败经 harness_note 门卡 + 反馈消息
	// 入史回灌重跑（有界，MaxRetries 缺省 2），耗尽 error 收束不静默放行。
	// 闭包入参 SessionBrief：按模式/任务形态决定开门与否与判据清单（判据
	// 归应用——build/test 命令或自包的对抗审查；基座只持门循环机制）。
	// 挂起/中断/错误轮不触发；重试预算随 Run/Resume 执行体。
	FinalGate func(sess SessionBrief) *GateConfig
}

// TurnEndSummary 轮收尾交接载荷（session_end 事件同源 + 会话身份）。
type TurnEndSummary struct {
	Owner   string
	SID     string
	Title   string
	Task    string
	Summary string               // 会话累计文本聚合（session_end 同源；列表摘要口径截 60 字，非单轮）
	Files   []contract.FileChange // 文件变更清单（有改动才非空）
	EndedAt time.Time
}

// Manager 引擎管理器（进程单例；会话态归 Registry）。
type Manager struct {
	reg *session.Registry
	Opt Options

	// 后台派生域（W-2）：会话域注册表+信号量（同步/后台同一池）。bgMu 保护
	// map 懒建/回收；条目竞态归 bgRegistry 自锁。
	bgMu sync.Mutex
	bg   map[string]*bgRegistry
}

// NewManager 构造（reg = 会话注册表；opt 必填项：Providers/Instruction/
// CheckPoints/WorkspaceRoot——缺一即报错，缺省 NewModel 生产构造）。
// SessionToolsOff 含未知族名即报错（装配错误启动期暴露，不拖到首会话）。
func NewManager(reg *session.Registry, opt Options) (*Manager, error) {
	// 必填项 nil 即拒（docs/04 装配面「四项必填」的构造期兑现——此前 nil
	// 会拖到首 Run 才空指针 panic）。
	var missing []string
	if opt.Providers == nil {
		missing = append(missing, "Providers")
	}
	if opt.Instruction == nil {
		missing = append(missing, "Instruction")
	}
	if opt.CheckPoints == nil {
		missing = append(missing, "CheckPoints")
	}
	if opt.WorkspaceRoot == nil {
		missing = append(missing, "WorkspaceRoot")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("engine: Options 缺必填项 %s（不可为 nil）", strings.Join(missing, "/"))
	}
	off := make(map[string]bool, len(opt.SessionToolsOff))
	for _, f := range opt.SessionToolsOff {
		switch f {
		case FamilyTodo, FamilyAsk, FamilyPlan, FamilyFS, FamilyCmd, FamilyPatch:
			off[f] = true
		default:
			return nil, fmt.Errorf("engine: 未知的会话域工具族 %q（可用 %s/%s/%s/%s/%s/%s）",
				f, FamilyTodo, FamilyAsk, FamilyPlan, FamilyFS, FamilyCmd, FamilyPatch)
		}
	}
	if opt.NewModel == nil {
		opt.NewModel = llm.NewChatModel
	}
	if opt.Sandbox != nil && !off[FamilyCmd] {
		// 装配期探测（C1）：未挂钩/内核不可用即启动日志告警（cmd 族被裁即
		// 无消费面，不探）。经注入的 provider 探测（nil 归一 OSProvider）。
		if opt.SandboxProvider != nil {
			opt.SandboxProvider.Probe()
		} else {
			sandbox.Probe()
		}
	}
	return &Manager{reg: reg, Opt: opt}, nil
}

// Registry 会话注册表出口。
func (m *Manager) Registry() *session.Registry { return m.reg }

// emitFn 事件写出回调（应用层接自己的传输；事件已由 Session.Record 落会话
// 记录——Record 对已删除会话返回零值即静默丢弃）。
type emitFn func(session.Event)

// emit 记录 + 转发（停止态静默）。
func (m *Manager) emit(s *session.Session, fn emitFn, name string, data any) {
	ev := s.Record(name, data)
	if ev.ID == 0 {
		return
	}
	fn(ev)
}

// runAccum 本轮输出累积（流式 chunk 拼装）。msgs = 轮内完整消息序列
// （assistant 分段 + tool 结果，入史真源）；text/thinking 另做整轮聚合
// （标题生成路径用）。超长工具结果截断归 reduction 中间件（出站即截 8192
// +外置换指针，事件泵收到的已是截断版——单一截断面，2026-08-26 退役
// 既有 newSpiller 双重截断）。
type runAccum struct {
	text      string // 整轮文本聚合（标题生成路径用）
	segText   string // 当前 assistant 段缓冲
	segThink  string
	toolCalls []schema.ToolCall // 当前段 tool_calls 缓冲
	tcSlots   map[int]int       // 流式分片槽：Index 锚 → toolCalls 位
	msgs      []*schema.Message // 轮内消息序列（历史回传用）
}

func (a *runAccum) addText(t string) {
	a.text += t
	a.segText += t
}

func (a *runAccum) addThinking(t string) { a.segThink += t }

// addToolResult 工具结果入序列（react 顺序：位于其 assistant 段之后）。
// 入史供下轮续聊；截断/外置归 reduction 中间件（此处零加工）。
func (a *runAccum) addToolResult(callID, content string) {
	if callID == "" {
		return
	}
	a.msgs = append(a.msgs, schema.ToolMessage(content, callID))
}

// addToolCall 流式 tool call 分片归并（OpenAI 协议：首片原子带 id/name，
// 续片仅 arguments 增量、以 Index 为锚——eino 引擎侧同语义归并后才执行）。
// 不归并则历史 tool call 参数为空，下轮回传被 omitempty 省略 arguments 键，
// 供应商 400 missing field `arguments`。
func (a *runAccum) addToolCall(tc schema.ToolCall) {
	if tc.Index == nil { // 非分片形态（完整调用）：直接追加
		a.toolCalls = append(a.toolCalls, tc)
		return
	}
	if pos, ok := a.tcSlots[*tc.Index]; ok {
		m := &a.toolCalls[pos]
		if tc.ID != "" {
			m.ID = tc.ID
		}
		if tc.Type != "" {
			m.Type = tc.Type
		}
		if tc.Function.Name != "" {
			m.Function.Name = tc.Function.Name
		}
		m.Function.Arguments += tc.Function.Arguments
		return
	}
	a.toolCalls = append(a.toolCalls, tc)
	if a.tcSlots == nil {
		a.tcSlots = map[int]int{}
	}
	a.tcSlots[*tc.Index] = len(a.toolCalls) - 1
}

// endAssistantMsg 单条 assistant 流结束：段封账入序列（空段跳过），清段缓冲
// 与分片槽（同轮多条 assistant 消息 Index 各自独立编号，槽不跨消息复用）。
func (a *runAccum) endAssistantMsg() {
	defer func() {
		a.segText, a.segThink, a.toolCalls, a.tcSlots = "", "", nil, nil
	}()
	if a.segText == "" && a.segThink == "" && len(a.toolCalls) == 0 {
		return
	}
	m := &schema.Message{Role: schema.Assistant, Content: a.segText, ReasoningContent: a.segThink}
	if len(a.toolCalls) > 0 {
		m.ToolCalls = a.toolCalls
	}
	a.msgs = append(a.msgs, m)
}

// discardSeg 丢弃当前半截段（网络容错 ② 重连路径：失败尝试的半截增量已被
// 事件层实时转发，adk 在模型调用边界内重启本次调用——本段不入史）。text
// 同步回卷：addText 同步追加保证整轮聚合尾部恒等于本段文本（TrimSuffix 安全）。
func (a *runAccum) discardSeg() {
	a.text = strings.TrimSuffix(a.text, a.segText)
	a.segText, a.segThink, a.toolCalls, a.tcSlots = "", "", nil, nil
}

// finishOf 状态收尾闭包（终态落盘）。
func (m *Manager) finishOf(s *session.Session) func(string) {
	return func(state string) {
		if s.Stopped() {
			return
		}
		s.SetState(state)
		m.reg.Persist(s)
	}
}

// ctxEstimates 上下文分类估算（usage 事件的分类三项来源；每轮 Run 计算一次）。
// messages = 整形后出站口径（H8-1）；saved = 原始口径与整形口径差额（整形
// 节省注记——reasoning 剥离 + 空壳剔除的量化，不含 reduction 外置/摘要）。
type ctxEstimates struct{ instruction, tools, messages, saved int }

// estTokens 无分词器的字符启发式：CJK ≈ 1 token/字，其余 ≈ 1/4。
func estTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2e80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + other/4
}

// estimateContext 上下文分类估算（Run 在泵前已把本轮用户消息入史——中断保险，
// CloneHistory 天然含本轮，无须另计）。工具面口径（B1 补齐）：业务面 + 进程件
// + 会话域件 + recall + spawn，名+描述+参数 schema JSON 均计——会话域件恒
// 常驻故须计入（此前全漏）；toolsearch 名单内工具不计（动态装载正是瘦身
// 手段，只有常驻面计费；分流与 assemble 同源名单）。
func (m *Manager) estimateContext(s *session.Session) ctxEstimates {
	brief := m.briefOf(s)
	est := ctxEstimates{instruction: estTokens(m.Opt.Instruction(brief))}
	dyn := map[string]bool{}
	if pol := m.Opt.ToolSearchPolicy; pol != nil {
		for _, n := range pol.DynamicTools {
			dyn[n] = true
		}
	}
	addFace := func(ts []contract.Tool) {
		for _, t := range ts {
			if info := t.Info(); info != nil && !dyn[info.Name] {
				est.tools += estTokens(info.Name) + estTokens(info.Desc) + schemaTokens(info.Params)
			}
		}
	}
	if m.Opt.Tools != nil {
		addFace(m.Opt.Tools(brief)) // 会话面：随 Owner/SID 裁剪后的真实业务面
	}
	if m.Opt.ProcessTools != nil {
		addFace(m.Opt.ProcessTools())
	}
	if sts, err := m.sessionTools(s); err == nil { // 会话域件实际面（族裁剪后）；构造失败随 assemble 报，此处不计
		addFace(sts)
	}
	if m.Opt.Recall {
		if rt, err := newRecallTool(m.reg, s); err == nil {
			addFace([]contract.Tool{rt})
		}
	}
	if m.Opt.SubAgents != nil { // spawn 面走静态估算（构造工具本体需建模板 agent——重）
		est.tools += estTokens(spawnToolName) + estTokens(spawnDesc) + spawnSchemaTokens()
	}
	// H8-1 口径：est_messages = 整形后出站视图（真实发送面——与 H1 TokenCounter
	// 同规则函数 llm.ShapeMessages）；saved = 原始口径差额（「整形节省」注记）。
	history := s.CloneHistory()
	msgTok := func(msg *schema.Message) int {
		n := estTokens(msgTextOf(msg)) + estTokens(msg.ReasoningContent) + 8 // 8 ≈ 角色开销
		for _, tc := range msg.ToolCalls {
			n += estTokens(tc.Function.Name) + estTokens(tc.Function.Arguments)
		}
		return n
	}
	for _, msg := range llm.ShapeMessages(history) {
		est.messages += msgTok(msg)
	}
	for _, msg := range history {
		est.saved += msgTok(msg)
	}
	est.saved -= est.messages
	return est
}

// schemaTokens 参数 schema 的 JSON 形估算（nil = 0；marshal 失败容错 0——
// 估算是治理信号不是精确账）。
func schemaTokens(sc *contract.Schema) int {
	if sc == nil {
		return 0
	}
	if b, err := json.Marshal(sc); err == nil {
		return estTokens(string(b))
	}
	return 0
}

// emitUsage 流末 usage chunk 到达即发（每轮模型调用一次，react 多轮后值
// 覆盖——最后一条 = 最终上下文规模）。Record 落事件流 → 刷新回放可恢复。
// spawnID 非空 = 子代理面用量上卷（B2：估算四项传零——子面无 estimateContext；
// 消费侧按 SpawnID 归组聚合）。
func (m *Manager) emitUsage(s *session.Session, fn emitFn, u *schema.TokenUsage, est ctxEstimates, spawnID string) {
	if u == nil || u.PromptTokens <= 0 {
		return
	}
	m.emit(s, fn, contract.EvUsage, contract.UsageOut{
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
		TotalTokens:   u.TotalTokens,
		SpawnID:       spawnID,
		EstInstruction: est.instruction, EstTools: est.tools, EstMessages: est.messages,
		EstSaved: est.saved, // 整形节省注记（H8-1；原始-整形差额，>0 才有意义）
	})
}

// envContextBudget env 覆盖的常驻上下文预算（0 = 未设；显式 Options 值优先）。
var envContextBudget int

// contextBudgetOf 生效预算（显式 Options 值优先，次 env；0 = 关）。
func (m *Manager) contextBudgetOf() int {
	if m.Opt.ContextBudget > 0 {
		return m.Opt.ContextBudget
	}
	return envContextBudget
}

// checkContextBudget 常驻面超限告警（B1）：Instruction+常驻工具面合计超线即发
// harness_note（Kind: budget）+ 日志，不阻断运行（大工具面配 toolsearch 就是
// 合法超标场景）；会话内只发一次——判定扫 Events 既有同 Kind note（免持久化
// 标记位：Reattach 后 Events 恢复即含旧告警，跨重启天然不重发；盘面重建的
// Data 是 map 形态，两形态同判）。
func (m *Manager) checkContextBudget(s *session.Session, fn emitFn, est ctxEstimates) {
	budget := m.contextBudgetOf()
	if budget <= 0 {
		return
	}
	resident := est.instruction + est.tools
	if resident <= budget {
		return
	}
	for _, ev := range s.SnapshotEvents() {
		if ev.Event != contract.EvHarnessNote {
			continue
		}
		switch d := ev.Data.(type) {
		case contract.HarnessNote:
			if d.Kind == "budget" {
				return
			}
		case map[string]any:
			if d["kind"] == "budget" {
				return
			}
		}
	}
	m.emit(s, fn, contract.EvHarnessNote, contract.HarnessNote{
		Kind:  "budget",
		Title: "常驻上下文超预算",
		Detail: fmt.Sprintf("Instruction ≈%d + 常驻工具面 ≈%d = ≈%d token，超预算线 %d（estTokens 启发式口径；瘦身：精简工具描述 / SessionToolsOff 裁族 / ToolSearchPolicy 动态装载）",
			est.instruction, est.tools, resident, budget),
	})
	log.Printf("einox: 会话 %s 常驻上下文 ≈%d token 超预算 %d（instruction %d + tools %d）",
		s.SID, resident, budget, est.instruction, est.tools)
}

// msgTextOf 消息文本（多模态消息 Content 为空——文本只进 text part，估算回退
// 读 parts）。
func msgTextOf(m *schema.Message) string {
	if len(m.UserInputMultiContent) == 0 {
		return m.Content
	}
	var b strings.Builder
	for _, p := range m.UserInputMultiContent {
		if p.Type == schema.ChatMessagePartTypeText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// sanitizeHistory 历史回传防御（① ②原地修复消息，下一轮 persist 落盘即为
// 净数据；③剔除后返回新序列）：
// ① 空 arguments 的 tool call 经 openai 序列化 omitempty 会整个省略 arguments
// 键，严格供应商直接 400。分片归并修复前落盘的存量脏历史在此自愈——回灌 "{}"。
// ② 悬空 tool_calls：assistant 带 tool_calls 后必须紧跟每个 tool_call_id 的
// tool 消息，缺失即 400。存量脏历史自愈——剥离未应答项（部分应答保留已应答
// 项，消息文本保留）。
// ③ 空 assistant 消息剔除：错误轮次落盘的空 final 回传即 400；纯悬空
// tool_calls 剥离后变空的消息一并剔除（须在②之后）。
// ④ 孤儿 tool 消息剔除：tool 消息前无带对应 tool_call 的 assistant——回传
// 即 400。
func sanitizeHistory(msgs []*schema.Message) []*schema.Message {
	for _, m := range msgs {
		for i := range m.ToolCalls {
			if m.ToolCalls[i].Function.Arguments == "" {
				m.ToolCalls[i].Function.Arguments = "{}"
			}
		}
	}
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role != schema.Assistant || len(m.ToolCalls) == 0 {
			continue
		}
		answered := map[string]bool{}
		for j := i + 1; j < len(msgs) && msgs[j].Role == schema.Tool; j++ {
			answered[msgs[j].ToolCallID] = true
		}
		kept := m.ToolCalls[:0]
		for _, tc := range m.ToolCalls {
			if answered[tc.ID] {
				kept = append(kept, tc)
			}
		}
		if len(kept) == 0 {
			m.ToolCalls = nil
		} else {
			m.ToolCalls = kept
		}
	}
	out := msgs[:0]
	for _, m := range msgs {
		if m.Role == schema.Assistant && m.Content == "" && m.ReasoningContent == "" && len(m.ToolCalls) == 0 {
			continue
		}
		out = append(out, m)
	}
	// ④ 孤儿 tool 消息剔除（前无带对应 tool_call 的 assistant）；须在②③后
	//（剥悬空/剔空都可能制造新孤儿）。
	kept := out[:0]
	open := map[string]bool{}
	for _, m := range out {
		switch m.Role {
		case schema.Assistant:
			clear(open)
			for _, tc := range m.ToolCalls {
				open[tc.ID] = true
			}
		case schema.Tool:
			if !open[m.ToolCallID] {
				continue
			}
			delete(open, m.ToolCallID)
		default:
			clear(open)
		}
		kept = append(kept, m)
	}
	return kept
}

// Run 执行一轮会话（同步阻塞至本轮结束/中断/错误；事件写出经 fn 回调）。
// 调用方前置：State 已置 running；审批中断时本方法置 pending_approval 返回。
// steering 排队兜底：上轮运行中排队的消息前置并入本轮输入。
func (m *Manager) Run(ctx context.Context, s *session.Session, userMsg string, atts []session.Attachment, fn emitFn) {
	s.ClearTurnGrant()
	s.SetPendingApproval("")
	queued := s.TakePending()
	for _, q := range queued { // 翻「已注入」回执：steer_queued/notify_queued 已建条目，翻态而非补建 user_message（回放不重复）；notify 条目独立事件名（审计区分系统通知与用户输入）
		if q.Kind == "notify" {
			s.Record(contract.EvNotifyInjected, contract.SteerEvent{ID: q.ID, Text: q.Text, Kind: q.Kind})
			continue
		}
		s.RestoreNotifyBudget() // 用户真实输入到达本轮输入：恢复自续预算（W-3——通知自身不恢复）
		s.Record(contract.EvSteerInjected, contract.SteerEvent{ID: q.ID, Text: q.Text, Attachments: q.Attachments})
	}
	if userMsg != "" {
		s.RestoreNotifyBudget() // 直接输入（非排队）同属用户消息
	}
	if userMsg != "" || len(atts) > 0 {
		s.Record(contract.EvUserMessage, contract.UserMsg{Text: userMsg, Attachments: atts})
	}
	if len(queued) > 0 {
		texts := make([]string, 0, len(queued)+1)
		var allAtts []session.Attachment
		for _, q := range queued {
			texts = append(texts, withAttachments(q.Text, q.Attachments))
			allAtts = append(allAtts, q.Attachments...)
		}
		texts = append(texts, withAttachments(userMsg, atts))
		userMsg = strings.Join(texts, "\n\n")
		atts = append(allAtts, atts...)
	} else {
		userMsg = withAttachments(userMsg, atts)
	}
	userMsgFinal := userMessageWithImages(userMsg, atts)
	s.SetTurnUserMsg(userMsg)

	runCtx, cancel := context.WithCancel(ctx)
	runCtx = contract.WithOperator(runCtx, s.Owner) // 工具层审计主体 = 会话发起人
	runCtx = contract.WithChangeRecorder(runCtx, s.RecordFileChange)
	runCtx = contract.WithImageInput(runCtx, m.imageCapableOf(s)) // 读图工具门禁：会话模型明示能力
	runCtx = withEmitFn(runCtx, fn)                               // failover 切换事件的 live 转发面
	s.SetCancel(cancel)
	defer func() {
		cancel()
		s.SetCancel(nil)
		s.RunFinished()
	}()

	finish := m.finishOf(s)

	history := sanitizeHistory(s.CloneHistory())
	iter, behaviors, err := m.runIter(runCtx, s, append(history, userMsgFinal))
	if err != nil {
		m.emit(s, fn, contract.EvError, errToEvent(err, s))
		finish(session.StateError)
		return
	}

	// 中断保险：本轮用户消息即刻入史落盘——进程死在轮中（或断连取消），
	// Reattach 仍保有完整提问脉络，模型不失忆；assistant 终态由 settleTurn
	// 轮末补录（user_message 事件的即时记录只保回放，不保模型上下文）。
	s.AppendHistory(userMsgFinal)
	m.reg.Persist(s)

	acc, endState := m.drive(runCtx, s, fn, iter, behaviors)
	m.settleTurn(s, acc, endState, fn, finish)
}

// Resume 审批决议后续流（hitl 配套；决议已由应用端点 SetDecision）。
// 入口整备（A1）：BeginResume 单锁原子查清挂起域 + 翻 running + 挂 runDone
// ——重复/并发第二个 Resume 即拒（checkpoint 不随 Resume 消费，迟到调用放行
// 是脏重放：旧检查点被加载重执行、决议已被消费回喂 fail-closed 信封）；执行
// 期状态可见为 running（FlushQueue/Drain 可寻址，此前恒显 pending）。
func (m *Manager) Resume(ctx context.Context, s *session.Session, fn emitFn) {
	stopApprovalTimer(s.SID)
	if !s.BeginResume() {
		m.emit(s, fn, contract.EvError, contract.ErrorOut{Code: "SERVER",
			Message: "会话无挂起可恢复（可能已被并发恢复或超时翻转）"})
		return
	}

	runCtx, cancel := context.WithCancel(ctx)
	runCtx = contract.WithOperator(runCtx, s.Owner)
	runCtx = contract.WithChangeRecorder(runCtx, s.RecordFileChange)
	runCtx = contract.WithImageInput(runCtx, m.imageCapableOf(s))
	runCtx = withEmitFn(runCtx, fn) // failover 切换事件的 live 转发面
	s.SetCancel(cancel)
	defer func() {
		cancel()
		s.SetCancel(nil)
		s.RunFinished()
	}()

	finish := m.finishOf(s)
	iter, behaviors, err := m.resumeIter(runCtx, s)
	if err != nil {
		m.emit(s, fn, contract.EvError, contract.ErrorOut{Code: "SERVER", Message: "审批恢复失败：" + err.Error()})
		finish(session.StateError)
		return
	}
	acc, endState := m.drive(runCtx, s, fn, iter, behaviors)
	m.settleTurn(s, acc, endState, fn, finish)
}

// FlushQueue 立即处理排队消息：打断当前执行体 → 等其收尾 → 排队消息为输入
// 重启一轮（Run 头部 TakePending 消费）。仅 running 态（挂起审批无执行体可
// 打断——排队消息随决议 Resume 注入）；等待有界：执行体不响应取消时 false
// 让位（调用方报错，用户可重试或显式停止）。
func (m *Manager) FlushQueue(s *session.Session) bool {
	if s.StateOf() != session.StateRunning {
		return false
	}
	s.MarkFlush()
	s.CancelRun()
	deadline := time.Now().Add(15 * time.Second)
	exited := false
	for !exited {
		done := s.RunDone()
		if done == nil {
			break
		}
		select {
		case <-done:
			exited = true
		case <-time.After(500 * time.Millisecond):
			if time.Now().After(deadline) {
				s.TakeFlushMark() // 让位：清残留标记（后续停止事件的形态不受污染）
				return false
			}
			s.CancelRun() // 重发取消（执行体起跑竞态：cancel 尚未挂上的窗口兜底）
		}
	}
	// 自然收尾竞争（打断前已自行结束）：旧执行体不走中断路径，清残留标记；
	// 等待期间排队消息被删空则无事可做——打断已发生，交正常发消息续聊
	s.TakeFlushMark()
	if s.QueueLen() == 0 {
		return false
	}
	if !s.BeginRun("") {
		return false
	}
	go m.Run(context.Background(), s, "", nil, func(session.Event) {})
	return true
}

// turnEpilogue 轮收尾交接（记忆写通道）：载荷与 session_end 事件同源。钩子
// panic 由引擎兜底（此时终态已落盘，不该被应用钩子拖垮）；同步调用——重
// 提取归应用异步。
func (m *Manager) turnEpilogue(s *session.Session) {
	defer func() { _ = recover() }()
	m.Opt.TurnEpilogue(TurnEndSummary{
		Owner: s.Owner, SID: s.SID, Title: s.TitleOf(), Task: s.TaskOf(),
		Summary: s.SummaryOf(), Files: s.FileChangesSnapshot(), EndedAt: time.Now(),
	})
}

// hasAssistant 历史中是否已有 assistant 终态（首轮判定——用户消息自 Run
// 开头即入史，不能再以「历史为空」判首轮）。
func hasAssistant(msgs []*schema.Message) bool {
	for _, m := range msgs {
		if m.Role == schema.Assistant {
			return true
		}
	}
	return false
}

// settleTurn 轮次收尾：自然结束 → 历史追加 + session_end + 终态落盘；
// 审批挂起 → 不收尾（turnUserMsg 保留供 Resume 完成时追加）。
func (m *Manager) settleTurn(s *session.Session, acc *runAccum, endState string, fn emitFn, finish func(string)) {
	if s.Stopped() || endState == session.StatePendingApproval || endState == "" {
		return // 挂起/静默收线：轮次未完或无产出
	}
	firstTurn := !hasAssistant(s.CloneHistory())
	turnUser := s.TurnUserMsgOf()
	// 收尾顺序：session_end 先记录（与回放一致）→ 历史追加 → 终态落盘 →
	// 最后写事件——客户端见 end 时状态已在写队列
	endEv := s.Record(contract.EvSessionEnd, contract.SessionEnd{Summary: s.SummaryOf(), Files: s.FileChangesSnapshot()})
	if endEv.ID == 0 {
		return // 已删除：静默
	}
	if acc != nil {
		acc.endAssistantMsg() // 末段保险（流分支已逐段封账，空段跳过）
		if len(acc.msgs) > 0 {
			s.AppendHistory(acc.msgs...) // 用户消息已由 Run 开头入史（中断保险）
		}
	}
	finish(endState)
	if endState == session.StateEnded {
		m.wipeWorkspace(s) // 任务正常收尾：临时区即清（挂起/异常保留待续）
		s.ClearTaskGrant() // 任务期授权随任务收尾结束（一轮任务一授权）
	}
	s.ClearTurnGrant()
	s.SetPendingApproval("")
	fn(endEv)
	if m.Opt.TurnEpilogue != nil && endState == session.StateEnded {
		m.turnEpilogue(s) // 记忆写通道：自然收束触发（挂起/中断/删除路径不触发）
	}
	if firstTurn && s.TitleOf() == "" {
		done := s.MarkTitleFlight() // 在途信号挂会话：Run 后写可 join（测试收尾/删除方等待锚点）
		go func() {
			defer done()
			m.genTitle(s, turnUser, accText(acc)) // 标题：异步总结，失败静默回退 Task
		}()
	}
}

// runIter 组装 + Run（输入 = 历史 + 本轮用户消息）。
func (m *Manager) runIter(ctx context.Context, s *session.Session, input []*schema.Message) (*adk.AsyncIterator[*adk.AgentEvent], map[string]string, error) {
	_, runner, behaviors, err := m.assemble(ctx, s)
	if err != nil {
		return nil, nil, err
	}
	return runner.Run(ctx, input, adk.WithCheckPointID(s.SID)), behaviors, nil
}

// resumeIter 组装 + Resume（checkpoint 决定续点）。
func (m *Manager) resumeIter(ctx context.Context, s *session.Session) (*adk.AsyncIterator[*adk.AgentEvent], map[string]string, error) {
	_, runner, behaviors, err := m.assemble(ctx, s)
	if err != nil {
		return nil, nil, err
	}
	iter, err := runner.Resume(ctx, s.SID)
	if err != nil {
		return nil, nil, err
	}
	return iter, behaviors, nil
}

// maxIterations 单轮 Run 的模型调用轮次预算（react 循环每次模型调用扣 1；
// 读密集任务 20 远不够，默认 100，EINO_MAX_ITERATIONS 可覆盖——环境变量名
// 沿产品旧名，部署配置不破）。
var maxIterations = 100

func init() {
	if v := os.Getenv("EINO_MAX_ITERATIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxIterations = n
		}
	}
	if v := os.Getenv("EINO_CONTEXT_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			envContextBudget = n
		}
	}
}

// assemble 模型解析 + agent 组装 + runner 构造（Run/Resume 共用）。
func (m *Manager) assemble(ctx context.Context, s *session.Session) (*adk.ChatModelAgent, *adk.Runner, map[string]string, error) {
	providers := m.Opt.Providers()
	if len(llm.FlattenModels(providers)) == 0 {
		return nil, nil, nil, &configError{"未配置模型供应商——请先在模型页选择厂家并填 API Key 添加"}
	}
	p, spec, found := llm.FindSpec(providers, s.Model.Model)
	if !found {
		return nil, nil, nil, &configError{"模型不在可用清单内：" + s.Model.Model + "（模型页检查配置）"}
	}
	cm, err := m.Opt.NewModel(ctx, p, spec, s.Model.Effort)
	if err != nil {
		return nil, nil, nil, &configError{"模型构造失败：" + err.Error()}
	}
	cm = llm.NewVisionModel(cm, spec, m.Opt.ImageResolve) // 图片引用解析/驱逐/门禁（请求边界）
	cm = llm.NewHistoryShapeModel(cm, p.Kind)             // reasoning 出站整形（请求边界，H1①）
	agConf := &adk.ChatModelAgentConfig{
		Instruction:      m.Opt.Instruction(m.briefOf(s)),
		Model:            cm,
		MaxIterations:    maxIterations,
		ModelRetryConfig: m.modelRetryConfig(), // 网络容错 ②：有界重试（机制默认挂接，应用零配置）
		ModelFailoverConfig: m.modelFailoverConfig(ctx, s), // 主模型降级链（空清单 nil 零变化）
	}
	var ts []contract.Tool
	if m.Opt.Tools != nil {
		ts = append(ts, m.Opt.Tools(m.briefOf(s))...)
	}
	if m.Opt.ProcessTools != nil {
		ts = append(ts, m.Opt.ProcessTools()...)
	}
	sts, err := m.sessionTools(s) // 会话域件（todo/ask_user/工作区族，可经 SessionToolsOff 裁剪）
	if err != nil {
		return nil, nil, nil, err
	}
	ts = append(ts, sts...)
	if m.Opt.Recall { // 记忆拉通道（opt-in；会话域件形态——owner/sid 装配期捕获）
		rt, err := newRecallTool(m.reg, s)
		if err != nil {
			return nil, nil, nil, err
		}
		ts = append(ts, rt)
	}
	if spec.NoToolCalls && (len(ts) > 0 || m.Opt.SubAgents != nil) { // A4 能力门控（组装期 fail fast）
		return nil, nil, nil, &configError{"模型 " + s.Model.Model + " 不支持函数调用（NoToolCalls 置位），不能装配工具面（含会话域件/spawn）"}
	}
	behaviors := make(map[string]string, len(ts)) // UI-B2：行为标记快照（tool_call 事件携带——前端分组数据源；值由工具自declare，引擎不判别）
	for _, t := range ts {
		if info := t.Info(); info != nil && info.Behavior != "" {
			behaviors[info.Name] = info.Behavior
		}
	}
	var face []tool.BaseTool
	if len(ts) > 0 {
		face = m.wrapFace(ts, s, s.ModePublic()) // hitl 审批 → ToolWrap（应用缝）→ 适配
	}
	if m.Opt.SubAgents != nil { // spawn 子代理（H2；白名单源 = 全量面）
		sp, err := m.newSpawnTool(ctx, s, m.Opt.SubAgents, ts)
		if err != nil {
			return nil, nil, nil, err
		}
		face = append(face, sp)
	}
	// H7 动态工具装载（toolsearch）：按 Policy 名单分流静态常驻/动态检索面。
	// 分流在 hitl.WrapTools + einoext.Adapt 之后（审批包装在分流上游——
	// ArgsForce 与模式审批对动态工具不豁免）；tool_search 元工具由中间件
	// 自动追加。应用不注入 Policy = 全量常驻，零变化。
	var searchMW adk.ChatModelAgentMiddleware
	if pol := m.Opt.ToolSearchPolicy; pol != nil && len(face) > 0 {
		dyn := map[string]bool{}
		for _, n := range pol.DynamicTools {
			dyn[n] = true
		}
		staticFace := make([]tool.BaseTool, 0, len(face))
		var dynamicFace []tool.BaseTool
		for _, t := range face {
			info, err := t.Info(ctx)
			if err != nil {
				return nil, nil, nil, err
			}
			if dyn[info.Name] {
				dynamicFace = append(dynamicFace, t)
			} else {
				staticFace = append(staticFace, t)
			}
		}
		if len(dynamicFace) > 0 {
			mw, err := toolsearch.New(ctx, &toolsearch.Config{DynamicTools: dynamicFace})
			if err != nil {
				return nil, nil, nil, err
			}
			searchMW = mw
			face = staticFace
		}
	}
	if len(face) > 0 {
		// 幻觉工具兜底（known 名单 = 静态面 + 动态装载名单；miss 分流见
		// unknowntool.go）。
		known := make([]string, 0, len(face)+8)
		for _, t := range face {
			if info, err := t.Info(ctx); err == nil && info != nil && info.Name != "" {
				known = append(known, info.Name)
			}
		}
		var dyn map[string]bool
		if pol := m.Opt.ToolSearchPolicy; pol != nil {
			dyn = make(map[string]bool, len(pol.DynamicTools))
			for _, n := range pol.DynamicTools {
				dyn[n] = true
				known = append(known, n)
			}
		}
		tc := adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: face, UnknownToolsHandler: newUnknownToolHandler(known, dyn),
		}}
		if m.Opt.SubAgents != nil && m.Opt.SubAgents.EmitEvents {
			tc.EmitInternalEvents = true // H8-2 全量转发档：子代理内部事件 → 父流（泵翻译 EvSubAgent）
		}
		agConf.ToolsConfig = tc
	}
	agConf.Handlers = append(agConf.Handlers, newSteeringMiddleware(s)) // steering 注入（运行中补充）
	if searchMW != nil {                                                // toolsearch（H7）：工具面先定，消息整形/计数在其后
		agConf.Handlers = append(agConf.Handlers, searchMW)
	}
	if m.Opt.SkillsDir != nil {
		if dir := m.Opt.SkillsDir(m.briefOf(s)); dir != "" {
			if mw := skills.NewMiddleware(ctx, dir); mw != nil {
				agConf.Handlers = append(agConf.Handlers, mw)
			}
		}
	}
	// 出站上下文经济（H1②③，末位挂接：在 steering/skills 注入后的最终态上
	// 计数与清除）；窗口未知（0）= 只截断不清除。
	window := windowOf(spec)
	mw, err := m.newReductionMiddleware(s, window, true)
	if err != nil {
		return nil, nil, nil, err
	}
	agConf.Handlers = append(agConf.Handlers, mw)
	if window > 0 { // H3 语义压缩（机械 clear 兜不住的文本膨胀；窗口未知不装配）
		sm, err := m.newSummarizationMiddleware(ctx, s, window)
		if err != nil {
			return nil, nil, nil, err
		}
		agConf.Handlers = append(agConf.Handlers, sm)
	}
	// AGENTS.md 注入（推通道）：挂 summarization 之后（上游官方建议位——注入
	// 内容不进摘要基底、不会被压缩掉；transient 不入历史/检查点）。
	if m.Opt.AgentsMD != nil {
		if mw := newAgentsMDMiddleware(ctx, m.Opt.AgentsMD(m.briefOf(s)), m.Opt.AgentsMDMaxBytes); mw != nil {
			agConf.Handlers = append(agConf.Handlers, mw)
		}
	}
	ag, err := adk.NewChatModelAgent(ctx, agConf)
	if err != nil {
		return nil, nil, nil, err
	}
	agent := adk.ResumableAgent(ag)
	if m.Opt.Topology != nil { // H5 拓扑装配（supervisor/deep——主配置全量复用）
		if agent, err = m.buildTopology(ctx, s, agConf, ts); err != nil {
			return nil, nil, nil, err
		}
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           agent,
		EnableStreaming: true,
		CheckPointStore: m.Opt.CheckPoints(s.Owner, s.SID),
	})
	return ag, runner, behaviors, nil
}

// wrapFace 契约面统一包装序：hitl 审批 → ToolWrap（应用缝，最外）→ einoext
// 适配。主面与子代理面（spawn/拓扑）同序——审计/准入对子代理同样生效；
// 应用包装在审批外层即单调收紧（透传保留审批，额外拒绝生效，豁免不可达）。
// nil ToolWrap = 只走 hitl，零变化。
func (m *Manager) wrapFace(ts []contract.Tool, s *session.Session, mode string) []tool.BaseTool {
	wrapped := hitl.WrapTools(ts, s, mode, m.Opt.Approval)
	if m.Opt.ToolWrap != nil {
		for i, t := range wrapped {
			wrapped[i] = m.Opt.ToolWrap(t)
		}
	}
	return einoext.Adapt(wrapped)
}

// imageCapableOf 会话模型是否声明图片输入（read_image 工具门禁——当前路由
// 明示能力才放行，对齐官方 harness 的路由能力断言：未知即拒）。
func (m *Manager) imageCapableOf(s *session.Session) bool {
	_, spec, ok := llm.FindSpec(m.Opt.Providers(), s.Model.Model)
	return ok && llm.SupportsImage(spec)
}

// briefOf 会话 → Instruction 入参概要（每轮 assemble/estimate 实时取——
// 会话内切换模型/effort 后下一轮提示即更新，永不陈旧）。
func (m *Manager) briefOf(s *session.Session) SessionBrief {
	return SessionBrief{Mode: s.ModePublic(), Model: s.Model.Model, Effort: s.Model.Effort,
		Owner: s.Owner, SID: s.SID}
}

// configError 配置类错误（error 事件 code=CONFIG）。
type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

// modelRetryConfig 模型调用重试策略（网络容错 ②——assemble 无条件默认挂接）。
// 重试执行白拿 eino adk 协议（有界次数+指数退避+流式中途重试+WillRetryError
// 事件化）；分类器归 llm.Classify：只对正向识别的传输信号重试，未知错误保守
// 放行为致命（eino 默认「任何错误都重试」不采纳）。ctx 取消/审批中断类 eino
// 内建排除，此处不重复。历史正确性由 adk 保证（react 只见最终被接受的流）；
// 事件层会实时转发失败尝试的半截增量——泵侧 WillRetryError 分支负责丢弃。
func (m *Manager) modelRetryConfig() *adk.ModelRetryConfig {
	return &adk.ModelRetryConfig{
		MaxRetries: llm.MaxRetries,
		ShouldRetry: func(ctx context.Context, rc *adk.RetryContext) *adk.RetryDecision {
			if ctx.Err() != nil || !llm.Classify(rc.Err).Retryable {
				return nil // 接受现状（错误原样上抛 / 成功放行）
			}
			return &adk.RetryDecision{Retry: true} // 退避走 eino 默认（指数+抖动）
		},
	}
}

// errToEvent 错误分类（CONFIG/SERVER/AUTH/RATE_LIMIT/TRANSPORT）。
func errToEvent(err error, s *session.Session) contract.ErrorOut {
	if s.Stopped() {
		return contract.ErrorOut{}
	}
	var ce *configError
	if errors.As(err, &ce) {
		return contract.ErrorOut{Code: "CONFIG", Message: ce.msg}
	}
	return errCard(err)
}

// unwrapRetryExhausted 展开重试耗尽（分类/文案落点回到末次真实错误）。
func unwrapRetryExhausted(err error) error {
	var re *adk.RetryExhaustedError
	if errors.As(err, &re) {
		return re.LastErr
	}
	return err
}

// emitTransportRetry 重连通知（WillRetryError 的 0 基失败序 → 1 基重连序）。
// 耗尽前的最后一次失败信号不发通知——错误卡随后即到，避免「N+1/N」越界提示。
func (m *Manager) emitTransportRetry(s *session.Session, fn emitFn, wr *adk.WillRetryError) {
	if n := wr.RetryAttempt + 1; n <= llm.MaxRetries {
		m.emit(s, fn, contract.EvTransportRetry, contract.TransportRetry{Attempt: n, Max: llm.MaxRetries})
	}
}

// errCard 分类驱动的错误卡（网络容错 ③）：重试耗尽先展开末次真实错误；
// 文案 = 分类器中文信息（含重试注记）。
func errCard(err error) contract.ErrorOut {
	var re *adk.RetryExhaustedError
	if errors.As(err, &re) {
		c := llm.Classify(re.LastErr)
		return contract.ErrorOut{Code: c.Code, Message: truncateRunes(
			fmt.Sprintf("%s（已自动重试 %d 次）", c.Message, re.TotalRetries), 200)}
	}
	c := llm.Classify(err)
	return contract.ErrorOut{Code: c.Code, Message: truncateRunes(c.Message, 200)}
}

// pump 事件泵：迭代 runner 事件 → 契约事件分类（Run/Resume 共用）。
// 返回 (本轮累积, 终态)：StatePendingApproval = 审批挂起（调用方不收尾）；
// endState 空 = 静默收线（停止/断连）。est = 上下文分类估算（usage 事件用）。
func (m *Manager) pump(s *session.Session, iter *adk.AsyncIterator[*adk.AgentEvent], fn emitFn, est ctxEstimates, behaviors map[string]string) (*runAccum, string) {
	acc := &runAccum{}
	endState := session.StateEnded
	subCalls := map[string]string{} // 子代理 callID → 工具名（EvSubAgent tool_result 契约语义=工具名，配对回填）
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			if s.Stopped() {
				return nil, "" // 删除：静默（磁盘零残留）
			}
			if errors.Is(ev.Err, context.Canceled) {
				m.interruptUnlessStopped(s, fn) // 断连/停止：中断收尾
				return nil, ""
			}
			var wr *adk.WillRetryError
			if errors.As(ev.Err, &wr) {
				// 网络容错 ②：重试在途（Generate 路径的事件化形态——流式路径
				// 的半截处理在 handleOutput）：非故障，通知 + 继续泵
				m.emitTransportRetry(s, fn, wr)
				continue
			}
			// 轮次耗尽：不是故障是预算——历史已入史（中断保险），发消息即可以
			// 全新预算接续；裸抛 NodeRunError 用户无从知道能继续
			if errors.Is(ev.Err, adk.ErrExceedMaxIterations) {
				m.emit(s, fn, contract.EvError, contract.ErrorOut{Code: "SERVER", Message: fmt.Sprintf(
					"本轮模型调用轮次已达上限（%d）——任务暂停而非失败，发送消息（如「继续」）即可接续执行",
					maxIterations)})
				endState = session.StateError
				break
			}
			m.emit(s, fn, contract.EvError, errCard(ev.Err))
			endState = session.StateError
			break
		}
		if ev.Action != nil && ev.Action.Interrupted != nil {
			// ask_user 提问中断（askuser 工具发起）：发 ask_user_request → 挂起态
			// 落盘 → 流收线（answer 端点 Resume 续流）——与审批同通道。
			if card, ok := askCardOf(ev.Action.Interrupted); ok && card.Question != "" {
				askID := newAskID()
				timeoutAt := time.Now().Add(ApprovalTimeout())
				m.emit(s, fn, contract.EvAskRequest, contract.AskReq{
					AskID: askID, Question: card.Question, Options: card.Options,
					AllowMulti: card.AllowMulti, AllowFreeText: card.AllowFreeText, TimeoutAt: timeoutAt,
				})
				s.SetPendingApproval(askID)
				// 挂起轮已产出段先入史（与审批同因：批准后 Resume 的 tool 结果
				// 须接在其后才是完整序列）
				acc.endAssistantMsg()
				if len(acc.msgs) > 0 {
					s.AppendHistory(acc.msgs...)
				}
				m.startApprovalTimer(s, askID, timeoutAt, "ask")
				m.finishOf(s)(session.StatePendingApproval)
				return acc, session.StatePendingApproval
			}
			// 计划提交中断（plan 工具发起）：发 plan_request → 挂起态落盘 → 流
			// 收线（approve 端点按 pending_kind=plan 分叉回执，Resume 续流）。
			// 文档已由工具先落盘（跨重启在盘），此处只挂起等审批。
			if card, ok := planCardOf(ev.Action.Interrupted); ok && card.Task != "" {
				planID := newPlanID()
				timeoutAt := time.Now().Add(ApprovalTimeout())
				m.emit(s, fn, contract.EvPlanRequest, contract.PlanReq{
					PlanID: planID, Task: card.Task, Summary: card.Summary, Steps: card.Steps,
					Risks: card.Risks, Path: card.Path, Seq: card.Seq, Mode: card.Mode, TimeoutAt: timeoutAt,
				})
				s.ClearTaskGrant() // 新计划提交 = 任务改向，旧任务期授权作废（批准后重新授予）
				s.SetPendingApproval(planID)
				acc.endAssistantMsg()
				if len(acc.msgs) > 0 {
					s.AppendHistory(acc.msgs...)
				}
				m.startApprovalTimer(s, planID, timeoutAt, "plan")
				m.finishOf(s)(session.StatePendingApproval)
				return acc, session.StatePendingApproval
			}
			// 审批中断（写工具 wrapper 发起）：聚合发一卡（H4-2 合并决议——一轮
			// 并行写调用的全部审批上下文收进一张 EvApprovalRequest N 项）→ 挂起态
			// 落盘 → 流收线（approve 端点批量决议 Resume 续流）。checkpoint 已由
			// runner 保存。
			if cards := approvalCardsOf(ev.Action.Interrupted); len(cards) > 0 {
				appID := newApprovalID()
				timeoutAt := time.Now().Add(ApprovalTimeout())
				req := contract.ApprovalReq{ApprovalID: appID, TimeoutAt: timeoutAt}
				ids := make([]string, 0, len(cards))
				for _, c := range cards {
					ids = append(ids, c.ItemID)
					req.Items = append(req.Items, contract.ApprovalItem{
						ItemID: c.ItemID, Tool: c.Tool, Action: c.Action, Plan: c.Plan,
						PlanMode: c.PlanMode, Note: c.Note, Diff: c.Diff,
					})
				}
				// 顶层旧字段 = 首项镜像（旧回放/旧前端按 N=1 单卡渲染——兼容）
				req.Tool, req.Action, req.Plan = cards[0].Tool, cards[0].Action, cards[0].Plan
				req.PlanMode, req.Note, req.Diff = cards[0].PlanMode, cards[0].Note, cards[0].Diff
				m.emit(s, fn, contract.EvApprovalRequest, req)
				s.SetPendingApproval(appID)
				s.SetPendingItems(ids) // 超时批量拒 / 端点覆盖校验依据
				// 挂起轮已产出段（assistant(tool_calls)）先入史——批准后 Resume 的
				// tool 结果接在其后才是完整序列；丢弃则批准结果成孤儿 tool 消息，
				// 续聊回传即 400。超时无人续：悬空 tool_calls 由 sanitizeHistory 剥离。
				acc.endAssistantMsg()
				if len(acc.msgs) > 0 {
					s.AppendHistory(acc.msgs...)
				}
				m.startApprovalTimer(s, appID, timeoutAt, "approval")
				m.finishOf(s)(session.StatePendingApproval)
				return acc, session.StatePendingApproval
			}
		}
		// H8-2 全量转发档：子代理内部事件（AgentName 非空 = 子 agent——父
		// agent 除 supervisor 形态外不命名，spawn 与拓扑子 agent 均在内；
		// supervisorMainName 是主 agent 本体（transfer 转回寻址用），排除后
		// 其输出仍走父主流/父历史；ToolsConfig.EmitInternalEvents 开启时
		// agent_tool 转发到父流）翻译为 EvSubAgent 只读流——不进父上下文/
		// 主流（官方注释实证：转发件不入父 runSession；非空名一并拦截，堵
		// 拓扑子事件落穿误入父历史）。
		if m.subEventsOn() && ev.AgentName != "" && ev.AgentName != supervisorMainName &&
			ev.Action == nil && ev.Output != nil && ev.Output.MessageOutput != nil {
			m.emitSubAgent(s, fn, ev.AgentName, subCalls, ev.Output.MessageOutput, "", nil)
			continue
		}
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		if stop := m.handleOutput(s, fn, acc, ev.Output.MessageOutput, est, behaviors); stop != outContinue {
			// 已删除/断连：删除静默，断连中断收尾；传输致命：错误卡已发，
			// 立即收线 error 态（不再赌下一次 iter.Next 送错——事件层客户
			// 副本被中途弃读后内部管线可能互等，即「卡 running」旧病根）
			if stop == outDeleted {
				m.interruptUnlessStopped(s, fn)
				return nil, ""
			}
			endState = session.StateError
			break
		}
	}
	if s.Stopped() {
		return nil, ""
	}
	return acc, endState
}

// interruptUnlessStopped 断连/停止收尾（删除会话静默跳过）：翻中断终态 +
// 事件 + 落盘——不留 running 僵尸。覆盖页面关闭/刷新/停止按钮。审批挂起
// （pending_approval）不受影响——那是有意的跨页面等待，超时器兜底。
// FlushQueue 的打断走 interrupted 行（非故障形态——紧跟的新一轮以排队消息
// 为输入）。打断语义告知（codex interrupted marker 对位）：中断轮的历史追
// 加一条系统注记——模型续聊时知晓打断语境与「工具可能部分执行」语义，不
// 假设中断前操作都已成功。
func (m *Manager) interruptUnlessStopped(s *session.Session, fn emitFn) {
	if s.Stopped() {
		return
	}
	if s.TakeFlushMark() {
		m.appendInterruptMarker(s)
		m.emit(s, fn, contract.EvInterrupted, contract.InterruptOut{Message: "已打断当前任务，立即处理排队消息"})
		m.finishOf(s)(session.StateError)
		return
	}
	m.appendInterruptMarker(s)
	m.emit(s, fn, contract.EvError, contract.ErrorOut{Code: "ABORTED", Message: "手动停止，任务中断（已执行的操作不回滚）"})
	m.finishOf(s)(session.StateError)
}

// appendInterruptMarker 打断历史标记（部分执行语义三义：被打断/后台进程可
// 能仍在跑/工具可能部分执行——续聊轮的模型可见面；悬空 tool_call 的配对
// 修补归 timers/sanitizeHistory，此处只补语义告知半边）。
func (m *Manager) appendInterruptMarker(s *session.Session) {
	s.AppendHistory(schema.UserMessage(
		"（系统注记）上一轮执行被中断：部分工具调用可能未完成或未生效，后台进程可能仍在运行。" +
			"继续任务前先用只读工具核对现场（文件状态/后台任务输出），不要假设中断前的操作都已成功。"))
	m.reg.Persist(s)
}

// summaryOf 取列表摘要。
func summaryOf(s *session.Session) string { return s.SummaryOf() }

// outVerdict handleOutput 处置判定。
type outVerdict int

const (
	outContinue outVerdict = iota // 继续泵
	outDeleted                    // 会话已删：静默弃（调用方 interruptUnlessStopped 兜删除分支）
	outFatal                      // 传输致命：错误卡已发，立即收线 error 态
)

// handleOutput 单事件分类。est = 上下文分类估算（usage 事件载荷）。
// 返回值：outContinue 继续泵；outDeleted 会话已删（调用方静默弃）；
// outFatal 传输致命（错误卡已发，调用方立即收线 error 态）。
func (m *Manager) handleOutput(s *session.Session, fn emitFn, acc *runAccum, v *adk.TypedMessageVariant[*schema.Message], est ctxEstimates, behaviors map[string]string) outVerdict {
	switch v.Role {
	case schema.Tool:
		// 工具结果（streaming 时拼装完整内容；digest 截断）
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
			content = v.Message.Content
			callID = v.Message.ToolCallID
		}
		if s.Stopped() {
			return outDeleted
		}
		ok, digest, preview := mid.ToolResultDigest(content) // 语义摘要 + 原始头（展开用）
		var cr struct {
			Counts string `json:"counts"`
			Verb   string `json:"verb"`
		}
		_ = json.Unmarshal([]byte(content), &cr) // B4 信封提取（无键的工具零值省略）
		m.emit(s, fn, contract.EvToolResult, contract.ToolResult{CallID: callID, OK: ok, Digest: digest, Preview: preview, Counts: cr.Counts, Verb: cr.Verb})
		acc.addToolResult(callID, content) // 入史：assistant(tool_calls) 须紧跟 tool 结果，缺失回传即 400
		m.reg.Persist(s)                   // 工具边界节流落盘（C2）：轮内崩溃不丢已完工具轮——频率有界（工具调用数）、单文件全量格式不变
		return outContinue

	case schema.Assistant:
		if v.IsStreaming && v.MessageStream != nil {
			for {
				chunk, err := v.MessageStream.Recv()
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					if s.Stopped() || errors.Is(err, context.Canceled) {
						return outDeleted
					}
					var wr *adk.WillRetryError
					if errors.As(err, &wr) {
						// 网络容错 ②：传输类已分类可重试，adk 在模型调用边界内
						// 重启本次调用。失败尝试的半截增量已实时转发到事件流（eino
						// 协议：客户端自行 reset）——丢弃半截段（不入史）+ 通知
						// 前端回卷显示；重试尝试的新流作为下一事件自然到达。
						m.emitTransportRetry(s, fn, wr)
						acc.discardSeg()
						return outContinue
					}
					// 致命（欠费/认证/参数错/重试耗尽/未知）：分类错误卡 + 立即收线
					m.emit(s, fn, contract.EvError, errCard(err))
					return outFatal
				}
				if chunk == nil {
					continue
				}
				if chunk.ResponseMeta != nil {
					m.emitUsage(s, fn, chunk.ResponseMeta.Usage, est, "")
				}
				if chunk.ReasoningContent != "" {
					acc.addThinking(chunk.ReasoningContent)
					m.emit(s, fn, contract.EvThinkingDelta, contract.Delta{Delta: chunk.ReasoningContent})
				}
				if chunk.Content != "" {
					acc.addText(chunk.Content)
					m.emit(s, fn, contract.EvTextDelta, contract.Delta{Delta: chunk.Content})
				}
				for _, tc := range chunk.ToolCalls {
					acc.addToolCall(tc) // 分片归并（首片带 id/name，续片仅 arguments 增量）
				}
				if s.Stopped() {
					return outDeleted
				}
			}
			// 工具调用事件在消息段收口后发：arguments 分片此时归并完整——
			// 流中首片发事件只能拿到残缺 JSON，参数摘要（ArgsDigest）必失真
			for _, tc := range acc.toolCalls {
				m.emit(s, fn, contract.EvToolCall, contract.ToolCall{CallID: tc.ID, Tool: tc.Function.Name, ArgsDigest: mid.ToolArgsDigest(tc.Function.Arguments), Behavior: behaviors[tc.Function.Name]})
			}
			acc.endAssistantMsg()
			return outContinue
		}
		// 非流式完整消息
		msg := v.Message
		if msg == nil {
			return outContinue
		}
		if msg.ResponseMeta != nil {
			m.emitUsage(s, fn, msg.ResponseMeta.Usage, est, "")
		}
		if msg.ReasoningContent != "" {
			acc.addThinking(msg.ReasoningContent)
			m.emit(s, fn, contract.EvThinkingDelta, contract.Delta{Delta: msg.ReasoningContent})
		}
		if msg.Content != "" {
			acc.addText(msg.Content)
			m.emit(s, fn, contract.EvTextDelta, contract.Delta{Delta: msg.Content})
		}
		for _, tc := range msg.ToolCalls {
			acc.addToolCall(tc)
			m.emit(s, fn, contract.EvToolCall, contract.ToolCall{CallID: tc.ID, Tool: tc.Function.Name, ArgsDigest: mid.ToolArgsDigest(tc.Function.Arguments), Behavior: behaviors[tc.Function.Name]})
		}
		acc.endAssistantMsg()
		return outContinue

	default:
		return outContinue
	}
}

// accText 累积文本（nil 防御）。
func accText(a *runAccum) string {
	if a == nil {
		return ""
	}
	return a.text
}

// genTitle 首轮收尾后异步生成会话标题（≤16 字中文，直接输出）：会话模型快照 +
// effort low（标题是短生成，固定低档思考）+ 15s 超时；失败/超时/已删除断路，
// 列表 title = Title || Task 回退。
func (m *Manager) genTitle(s *session.Session, userMsg, assistant string) {
	if strings.TrimSpace(userMsg) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	providers := m.Opt.Providers()
	p, spec, ok := llm.FindSpec(providers, s.Model.Model)
	if !ok {
		return
	}
	cm, err := m.Opt.NewModel(ctx, p, spec, "low")
	if err != nil {
		return
	}
	prompt := "为下面的任务对话生成一个不超过16个字的中文标题，直接输出标题本身，不要引号、句号或任何解释。\n\n用户：" +
		truncateRunes(userMsg, 2000) + "\n\n助手：" + truncateRunes(assistant, 500)
	out, err := cm.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)})
	if err != nil || s.Stopped() {
		return
	}
	title := sanitizeTitle(out.Content)
	if title == "" {
		return
	}
	s.SetTitle(title)
	m.reg.Persist(s)
}

// sanitizeTitle 标题清洗：单行、去引号包裹与句读、截 16 字。
func sanitizeTitle(in string) string {
	in = strings.ReplaceAll(in, "\n", " ")
	for _, q := range []string{"\"", "'", "“", "”", "‘", "’", "「", "」", "『", "』", "《", "》"} {
		in = strings.ReplaceAll(in, q, "")
	}
	in = strings.TrimSpace(in)
	in = strings.Trim(in, "。．.…！!？?；;，, ")
	return truncateRunes(in, 16)
}

// truncateRunes 截断加省略号。
func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// DayHeader 通用日期头（提示词机制件——业务段拼装归应用，自产品
// instruction.go 的日期头拆出）。
func DayHeader(now time.Time) string {
	off := (int(now.Weekday()) + 6) % 7 // ISO 周以周一为首
	_, w := now.ISOWeek()
	return fmt.Sprintf("今天是 %s %s（%s，本周 %s 至 %s）。",
		now.Format("2006-01-02"),
		[...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[int(now.Weekday())],
		fmt.Sprintf("W%02d", w),
		now.AddDate(0, 0, -off).Format("2006-01-02"),
		now.AddDate(0, 0, 6-off).Format("2006-01-02"))
}
