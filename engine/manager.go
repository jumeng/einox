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
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/dynamictool/toolsearch"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/einoext"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/sandbox"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/skills"
	"github.com/jumeng/einox/tools"
	"github.com/jumeng/einox/tools/egress"
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
	// ParentSID 辅助对话父会话（空 = 普通会话——应用可据此调 Instruction/
	// 工具面/skill 目录；side 共享父工作区与外置域，见 wsSID）。
	ParentSID string
	// T6 当轮说话人（空 = 单用户零变化）：应用装配面可见——按人裁剪
	// Instruction/工具面/审批判定（机制与内容分离：身份只透传，策略归应用）。
	TurnSpeakerID   string
	TurnSpeakerName string
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
	// nil/空 = 全挂零行为变化——族构造失败照常上抛 CONFIG，不静默吞错），未知
	// 名 NewManager 即拒——对齐 DenyTools 的 fail-fast 纪律）。
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
	// Hooks 订阅式工具钩子（audit/拦截零样板订阅口，nil = 零变化）：挂
	// ToolWrap 之外的最外层（包装序 hitl → ToolWrap → Hooks → einoext——
	// 审计看终局：Pre 先于审批决策触发、Post 收审批拒绝信封与工具原始
	// 返回）。主面与子代理面同挂；Pre 可否决（error = 拒绝信封回喂，只能
	// 收紧不能放宽——与 ToolWrap 同纪律）。语义全貌见 toolhooks.go。
	Hooks *ToolHooks
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
	// ApprovalRouter 审批路由（T6，nil = 不路由——全员可见谁先点谁决议）：
	// 挂起卡生成时按会话概要与卡面裁决「问谁」，目标随卡下发（前端定向提示）
	// 并入 pending target（DecisionGuard 校验依据）。判据归应用（机制与内容
	// 分离：基座只携带身份与执行校验）。
	ApprovalRouter func(brief SessionBrief, req contract.ApprovalReq) *contract.Participant
	// DecisionGuard 决议校验缝（T6，nil = 不校验——应用自带认证时的零变化）：
	// 挂起卡有路由目标且决议带决议者时校验一致性，mismatch 拒绝（fail-closed
	// ——防越权点批）。
	DecisionGuard func(targetID, deciderID string) error
	// WorkspaceRoot 会话工作区根（用户域 workspaces/<sid>——WorkspaceKeep
	// 声明的持久子区跨任务保留、其余一轮一清；惰性创建）。
	WorkspaceRoot func(owner, sid string) string
	// WorkspaceKeep 任务收尾清理保留的工作区子目录（顶层目录名——挂载区、
	// 参考资料区等由装配层声明，基座不预设名字；nil/空 = 无持久区全清）。
	// 持久子区随会话删除/过期/孤儿清扫整清（Registry.Delete/Sweep 整目录
	// 移除，不看此栏）。
	WorkspaceKeep []string
	// WorkspaceProtect 工作区写保护区（顶层目录名；注入 fsutil/applypatch
	// 写面——delete_file 与补丁目标〔含 Move to 改名目标〕命中即整单拒绝、
	// 读面不受影响；nil/空 = 不设区零变化。命令面 run_command 不在此栏——
	// 命令内容不可静态可靠解析，写的硬约束归 Sandbox 档位）。
	WorkspaceProtect []string
	// SubAgents spawn 子代理装配（H2；nil = 不装配 spawn）。
	SubAgents *SubAgentsConfig
	// Topology 确定性场景多 agent 拓扑（H5：supervisor/deep 官方 prebuilt 接线；
	// nil = 单 agent react 既有主线。红线表对拓扑内子 agent 全量生效）。
	Topology *TopologyConfig
	// ToolSearchPolicy 动态工具装载（H7：名单外常驻、名单内经 tool_search
	// 检索后可见；nil = 全量常驻零变化。审批包装在分流上游——ArgsForce 与
	// 模式审批对动态工具不豁免）。
	ToolSearchPolicy *ToolSearchPolicy
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
	// false = 不装配零变化。条件装配先例 = channels 之于 Options.Channels。
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
	// Channels 消息渠道装配（nil/空 = 不装配零变化）：每条目一个渠道实例
	// （ID 进程内唯一、Sink 出站投递——渲染归适配器）。渠道编排面
	// Manager.Channels() 懒建总可用：入站 Handle 分流（空闲起轮/运行中
	// 排队）、常驻事件订阅出站（覆盖挂起期）、Approve/Answer 决议续流、
	// Cancel/Push。渠道协议归适配器（官方通用件 channels/ 子包；业务
	// 自定义渠道长在业务仓）——同 llm 供应商「内置目录 + 自定义」两层
	// 模式，见 docs/04。
	Channels []ChannelConfig
}

// TurnEndSummary 轮收尾交接载荷（session_end 事件同源 + 会话身份）。
type TurnEndSummary struct {
	Owner   string
	SID     string
	Title   string
	Task    string
	Summary string                // 会话累计文本聚合（session_end 同源；列表摘要口径截 60 字，非单轮）
	Files   []contract.FileChange // 文件变更清单（有改动才非空）
	EndedAt time.Time
}

// Manager 引擎管理器（进程单例；会话态归 Registry）。
type Manager struct {
	reg *session.Registry
	Opt Options

	// 渠道编排域（懒建——Channels() 首触即建，见 channel.go）。
	chanMu sync.Mutex
	chanGW *ChannelGateway

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
	if err := tools.CheckTopDirs("WorkspaceKeep", opt.WorkspaceKeep); err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	if err := tools.CheckTopDirs("WorkspaceProtect", opt.WorkspaceProtect); err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	// SpawnOutput.Schema 根形构造期拒收（设计 §2.3-③——纯静态配置不依赖
	// 会话面，NewManager 即拒与 SessionToolsOff 的 fail-fast 同位；此前在
	// 首轮 assemble 报 CONFIG 卡，拒收时点与设计措辞不符）。
	if opt.SubAgents != nil && opt.SubAgents.Output != nil {
		if sc := opt.SubAgents.Output.Schema; sc == nil || sc.Type != "object" {
			return nil, fmt.Errorf("engine: SpawnOutput.Schema 根形必须为 object（结构化结论契约拒收）")
		}
	}
	// Topology.Kind 构造期即拒（与 SessionToolsOff/SpawnOutput.Schema
	// 的 fail-fast 同位——纯静态配置不依赖会话面；此前拖到首轮 assemble 才
	// 报 CONFIG 卡，同类校验时点不一致，审查 P2-17。空 Kind 同拒：Topology
	// 非 nil 即装配意图，零值形态必在 assemble 报错——无「合法空档」，安全
	// 审查 2026-09-06 补齐）。
	if opt.Topology != nil {
		switch opt.Topology.Kind {
		case TopologySupervisor, TopologyDeep:
		default:
			return nil, fmt.Errorf("engine: 未知的拓扑形态 %q（supervisor|deep）", opt.Topology.Kind)
		}
	}
	if opt.NewModel == nil {
		opt.NewModel = llm.NewChatModel
	}
	chanIDs := make(map[string]bool, len(opt.Channels))
	for _, c := range opt.Channels {
		if c.ID == "" {
			return nil, fmt.Errorf("engine: Options.Channels 条目 ID 不可为空")
		}
		if c.Sink == nil {
			return nil, fmt.Errorf("engine: 渠道 %q 缺 Sink（出站投递面）", c.ID)
		}
		if chanIDs[c.ID] {
			return nil, fmt.Errorf("engine: 渠道 ID %q 重复（消息路由键须唯一）", c.ID)
		}
		chanIDs[c.ID] = true
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

// Run 执行一轮会话（同步阻塞至本轮结束/中断/错误；事件写出经 fn 回调）。
// 调用方前置：State 已置 running；审批中断时本方法置 pending_approval 返回。
// beginTurn 轮执行体装配（Run/Resume 共用——曾两处逐字复制：新增一个 ctx
// 携带值漏改一处即续流路径行为分叉，恰落在审批续流这类测试最难覆盖面上）。
// 契约值注入（工具层审计主体 = 当轮说话人——Run 起新轮、Resume 跨审批中断
// 保留；文件变更记录；读图门禁；failover live 转发面）+ cancel 登记 + 收尾
// （cancel / 摘登记 / RunFinished）。
func (m *Manager) beginTurn(ctx context.Context, s *session.Session, fn emitFn) (context.Context, func()) {
	runCtx, cancel := context.WithCancel(ctx)
	runCtx = contract.WithOperator(runCtx, m.operatorOf(s))
	runCtx = contract.WithChangeRecorder(runCtx, s.RecordFileChange)
	runCtx = contract.WithImageInput(runCtx, m.imageCapableOf(s))
	runCtx = withEmitFn(runCtx, fn)
	s.SetCancel(cancel)
	return runCtx, func() {
		cancel()
		s.SetCancel(nil)
		s.RunFinished()
	}
}

// steering 排队兜底：上轮运行中排队的消息前置并入本轮输入。
func (m *Manager) Run(ctx context.Context, s *session.Session, userMsg string, atts []session.Attachment, fn emitFn) {
	// 首轮标记锚定 Run 入口（U-1）：挂起段会 flushAcc 入史，收尾时再判
	// 「历史已有 assistant」会把被挂起切断的首轮误判为非首轮——标题恒回退。
	// 此处判定先于本轮任何入史，跨挂起保留到 Resume 收尾消费。
	s.SetTurnFirst(!hasAssistant(s.CloneHistory()))
	s.ClearTurnGrant()
	s.SetPendingApproval("")
	actor := s.TurnActorOf() // T6 当轮说话人（nil = 单用户零变化）
	queued := s.TakePending()
	for _, q := range queued { // 翻「已注入」回执：steer_queued/notify_queued 已建条目，翻态而非补建 user_message（回放不重复）；notify 条目独立事件名（审计区分系统通知与用户输入）
		if q.Kind == "notify" {
			s.Record(contract.EvNotifyInjected, contract.SteerEvent{ID: q.ID, Text: q.Text, Kind: q.Kind})
			continue
		}
		s.RestoreNotifyBudget() // 用户真实输入到达本轮输入：恢复自续预算（W-3——通知自身不恢复）
		s.Record(contract.EvSteerInjected, contract.SteerEvent{ID: q.ID, Text: q.Text, Attachments: q.Attachments,
			SpeakerID: q.SpeakerID, SpeakerName: q.SpeakerName}) // T6 谁的排队消息
	}
	if userMsg != "" {
		s.RestoreNotifyBudget() // 直接输入（非排队）同属用户消息
	}
	if userMsg != "" || len(atts) > 0 {
		var spID, spName string // T6 谁说的（Name = 快照）
		if actor != nil {
			spID, spName = actor.ID, actor.Name
		}
		s.Record(contract.EvUserMessage, contract.UserMsg{Text: userMsg, Attachments: atts, SpeakerID: spID, SpeakerName: spName})
	}
	// T6 输入分段：排队消息 + 直接输入按说话人分段——同人相邻合并、异人各自
	// 成条（Extra 署名，装配期 renderSpeakers 渲染前缀）；说话人全空 = 单段 =
	// 旧单条合并形态（通知段无 speaker 并入——与旧行为一致；空源跳过）。
	var userMsgs []*schema.Message
	{
		segs := inputSegments(queued, userMsg, atts, actor)
		disp := make([]string, 0, len(segs))
		for _, seg := range segs {
			joined := strings.Join(seg.texts, "\n\n")
			msg := userMessageWithImages(joined, seg.atts)
			if seg.speaker != nil {
				if msg.Extra == nil {
					msg.Extra = map[string]any{}
				}
				msg.Extra[speakerExtraID] = seg.speaker.ID
				msg.Extra[speakerExtraName] = seg.speaker.Name
			}
			userMsgs = append(userMsgs, msg)
			disp = append(disp, joined)
		}
		s.SetTurnUserMsg(strings.Join(disp, "\n\n"))
	}

	runCtx, endTurn := m.beginTurn(ctx, s, fn)
	defer endTurn()

	finish := m.finishOf(s)

	// T8 方案甲：缓存信封 + 水位后历史（dsh surface replace 的轻量对位——压
	// 一次、后续 Run 免重摘要）。水位切片先于 sanitize（净化剥除会使索引漂移
	//）；失锚（水位越界 = 落盘异常）fail-open 全量重放。原文不动——session
	// 历史始终全量（保真锚定）。
	hist := s.CloneHistory()
	if env, wm, ok := m.loadCompactCache(s); ok && wm <= len(hist) {
		tail := sanitizeHistory(hist[wm:])
		hist = append([]*schema.Message{schema.UserMessage(env)}, tail...)
	} else {
		hist = sanitizeHistory(hist)
	}
	history := renderSpeakers(hist)                                                            // T6 署名前缀投影（副本上——原文不带，Extra 携）
	iter, behaviors, err := m.runIter(runCtx, s, append(history, renderSpeakers(userMsgs)...)) // 当前轮消息同律投影
	if err != nil {
		m.emit(s, fn, contract.EvError, errToEvent(err, s))
		// 中断保险同律（审查 P2-13）：装配失败（CONFIG 类——未配模型/不在清单/
		// 构造失败）也保提问脉络——user_message 事件已落流而历史缺消息，用户
		// 修好配置发「继续」时模型上下文里没有原问题。
		s.AppendHistory(userMsgs...)
		m.reg.Persist(s)
		finish(session.StateError)
		return
	}

	// 中断保险：本轮用户消息即刻入史落盘——进程死在轮中（或断连取消），
	// Reattach 仍保有完整提问脉络，模型不失忆；assistant 终态由 settleTurn
	// 轮末补录（user_message 事件的即时记录只保回放，不保模型上下文）。
	s.AppendHistory(userMsgs...)
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
		m.emit(s, fn, contract.EvError, contract.ErrorOut{Code: contract.ErrCodeServer,
			Message: "会话无挂起可恢复（可能已被并发恢复或超时翻转）"})
		return
	}

	runCtx, endTurn := m.beginTurn(ctx, s, fn)
	defer endTurn()

	finish := m.finishOf(s)
	iter, behaviors, err := m.resumeIter(runCtx, s)
	if err != nil {
		m.emit(s, fn, contract.EvError, contract.ErrorOut{Code: contract.ErrCodeServer, Message: "审批恢复失败：" + err.Error()})
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
	if s.StateOf() != session.StateRunning || s.Stopped() {
		return false
	}
	s.MarkFlush()
	s.CancelRun()
	deadline := time.Now().Add(15 * time.Second)
	retry := time.NewTimer(500 * time.Millisecond) // 复用 timer（每 500ms 新建 time.After 属卫生债）
	defer retry.Stop()
	exited := false
	for !exited {
		done := s.RunDone()
		if done == nil {
			break
		}
		select {
		case <-done:
			exited = true
		case <-retry.C:
			if time.Now().After(deadline) {
				s.TakeFlushMark() // 让位：清残留标记（后续停止事件的形态不受污染）
				return false
			}
			s.CancelRun() // 重发取消（执行体起跑竞态：cancel 尚未挂上的窗口兜底）
			retry.Reset(500 * time.Millisecond)
		}
	}
	// 自然收尾竞争（打断前已自行结束）：旧执行体不走中断路径，清残留标记；
	// 等待期间排队消息被删空则无事可做——打断已发生，交正常发消息续聊
	s.TakeFlushMark()
	if s.QueueLen() == 0 || s.Stopped() { // 停止竞态窗（Delete 并发）：不起死会话的空轮
		return false
	}
	if !s.BeginRun("") {
		return false
	}
	s.SetTurnActor(nil) // 系统接管轮：清陈旧说话人（排队消息自带 per-message 署名；operator 回退 Owner）
	go m.Run(context.Background(), s, "", nil, noopEmit)
	return true
}

// turnEpilogue 轮收尾交接（记忆写通道）：载荷与 session_end 事件同源。钩子
// panic 由引擎兜底（此时终态已落盘，不该被应用钩子拖垮）；同步调用——重
// 提取归应用异步。
func (m *Manager) turnEpilogue(s *session.Session) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("engine: TurnEpilogue 钩子 panic（终态已落盘，不拖垮引擎）：%v", r)
		}
	}()
	m.Opt.TurnEpilogue(TurnEndSummary{
		Owner: s.Owner, SID: s.SID, Title: s.TitleOf(), Task: s.TaskOf(),
		Summary: s.SummaryOf(), Files: s.FileChangesSnapshot(), EndedAt: time.Now(),
	})
}

// settleTurn 轮次收尾：自然结束 → 历史追加 + session_end + 终态落盘；
// 审批挂起 → 不收尾（turnUserMsg 保留供 Resume 完成时追加）。
func (m *Manager) settleTurn(s *session.Session, acc *runAccum, endState string, fn emitFn, finish func(string)) {
	if s.Stopped() || endState == session.StatePendingApproval || endState == "" {
		return // 挂起/静默收线：轮次未完或无产出
	}
	firstTurn := s.TurnFirstOf() // Run 入口的锚定值（挂起段入史不污染——U-1）
	turnUser := s.TurnUserMsgOf()
	// 收尾顺序：session_end 先记录（与回放一致）→ 历史追加 → 终态落盘 →
	// 最后写事件——客户端见 end 时状态已在写队列。HistLen 在记录时点先于
	// 追加：追加前长度 + 本轮待追加消息数 = 轮后精确值（ForkAt 锚定依据）。
	// 末段保险必须先于 HistLen 计算收口——流中致命错误轮带着未封账的半截
	// 段到达此处，晚封账即少计一条（错误轮锚恰是「从失败点重试」场景）。
	if acc != nil {
		acc.endAssistantMsg() // 封账只改 acc 不发事件（流分支已逐段封账，空段跳过）
	}
	histLen := s.HistoryLen()
	if acc != nil {
		histLen += len(acc.msgs)
	}
	endEv := s.Record(contract.EvSessionEnd, contract.SessionEnd{
		Summary: s.SummaryOf(), Files: s.FileChangesSnapshot(), HistLen: histLen,
	})
	if endEv.ID == 0 {
		return // 已删除：静默
	}
	if acc != nil && len(acc.msgs) > 0 {
		s.AppendHistory(acc.msgs...) // 用户消息已由 Run 开头入史（中断保险）
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
			// 游离 goroutine 护栏（timers 同纪律）：genTitle 走应用注入的
			// NewModel 与外网 Generate——panic 不拖垮进程，收敛为日志。
			defer func() {
				if r := recover(); r != nil {
					log.Printf("engine: genTitle panic（标题回退 Task）：%v", r)
				}
			}()
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

// newShapedModel 会话模型构造链（FindSpec → NewModel → Vision → HistoryShape）
// ——H1 出站整形口径（vision 图片引用解析/驱逐 + reasoning 剥离）在主模型/
// 子代理/摘要/拓扑子/降级链五面同一包装序：单一实现防漂移（曾五处各写）。
// what = 错误文案主体（「模型」「子代理模型」「摘要模型」…）；effort 由调用方
// 持快照传入（PUT settings 并发写——不裸读 s.Model）。spec 回传（assemble 的
// NoToolCalls 能力门控消费；其余调用方忽略）。
func (m *Manager) newShapedModel(ctx context.Context, key, effort, what string) (model.BaseModel[*schema.Message], llm.ModelSpec, error) {
	p, spec, ok := llm.FindSpec(m.Opt.Providers(), key)
	if !ok {
		return nil, spec, &configError{what + "不在可用清单内：" + key}
	}
	cm, err := m.Opt.NewModel(ctx, p, spec, effort)
	if err != nil {
		return nil, spec, &configError{what + "构造失败：" + err.Error()}
	}
	cm = llm.NewVisionModel(cm, spec, m.Opt.ImageResolve)
	cm = llm.NewHistoryShapeModel(cm, p.Kind)
	return cm, spec, nil
}

// assemble 模型解析 + agent 组装 + runner 构造（Run/Resume 共用）。
func (m *Manager) assemble(ctx context.Context, s *session.Session) (*adk.ChatModelAgent, *adk.Runner, map[string]string, error) {
	ms := s.ModelSnapshot() // 持锁快照（PUT settings 随时写并发——本轮组装口径统一）
	providers := m.Opt.Providers()
	if len(llm.FlattenModels(providers)) == 0 {
		return nil, nil, nil, &configError{"未配置模型供应商——请先在模型页选择厂家并填 API Key 添加"}
	}
	if _, _, found := llm.FindSpec(providers, ms.Model); !found {
		return nil, nil, nil, &configError{"模型不在可用清单内：" + ms.Model + "（模型页检查配置）"}
	}
	s.NoteModelCall(ms.Model) // 调用边界比对：与上次实际调用不同才落切换注记（选择器切换不落）
	cm, spec, err := m.newShapedModel(ctx, ms.Model, ms.Effort, "模型")
	if err != nil {
		return nil, nil, nil, err
	}
	agConf := &adk.ChatModelAgentConfig{
		Instruction:         m.Opt.Instruction(m.briefOf(s)),
		Model:               cm,
		MaxIterations:       maxIterations,
		ModelRetryConfig:    m.modelRetryConfig(),          // 网络容错 ②：有界重试（机制默认挂接，应用零配置）
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
		return nil, nil, nil, &configError{"模型 " + ms.Model + " 不支持函数调用（NoToolCalls 置位），不能装配工具面（含会话域件/spawn）"}
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
		dyn := dynamicToolSet(pol)
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
			dyn = dynamicToolSet(pol)
			known = append(known, pol.DynamicTools...)
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

// wrapFace 契约面统一包装序：hitl 审批 → ToolWrap（应用缝）→ Hooks（订阅
// 钩子，最外）→ einoext 适配。主面与子代理面（spawn/拓扑）同序——审计/准入
// 对子代理同样生效；应用包装在审批外层即单调收紧（透传保留审批，额外拒绝
// 生效，豁免不可达）。nil ToolWrap / nil Hooks = 跳过该层，零变化。
func (m *Manager) wrapFace(ts []contract.Tool, s *session.Session, mode string) []tool.BaseTool {
	// T8 输出契约：装配期探测（mid 包装前——包装链类型不可穿透）、契约包装
	// 成最内层（未实现契约的工具零变化）。§1.3-① 修订形态。
	for i, t := range ts {
		if oc, ok := t.(contract.OutputContract); ok {
			ts[i] = &outputContractTool{Tool: t, oc: oc}
		}
	}
	wrapped := hitl.WrapTools(ts, s, mode, m.Opt.Approval)
	if m.Opt.ToolWrap != nil {
		for i, t := range wrapped {
			wrapped[i] = m.Opt.ToolWrap(t)
		}
	}
	return einoext.Adapt(m.hookFace(wrapped, m.briefOf(s)))
}

// imageCapableOf 会话模型是否声明图片输入（read_image 工具门禁——当前路由
// 明示能力才放行，对齐官方 harness 的路由能力断言：未知即拒）。持锁快照
// （PUT settings 随时写并发——与 assemble/briefOf 同纪律，不裸读 s.Model）。
func (m *Manager) imageCapableOf(s *session.Session) bool {
	ms := s.ModelSnapshot()
	_, spec, ok := llm.FindSpec(m.Opt.Providers(), ms.Model)
	return ok && llm.SupportsImage(spec)
}

// briefOf 会话 → Instruction 入参概要（每轮 assemble/estimate 实时取——
// 会话内切换模型/effort 后下一轮提示即更新，永不陈旧）。
func (m *Manager) briefOf(s *session.Session) SessionBrief {
	ms := s.ModelSnapshot() // 持锁快照（PUT settings 随时写并发）
	b := SessionBrief{Mode: s.ModePublic(), Model: ms.Model, Effort: ms.Effort,
		Owner: s.Owner, SID: s.SID, ParentSID: s.ParentOf()}
	if a := s.TurnActorOf(); a != nil { // T6 当轮说话人（空 = 零变化）
		b.TurnSpeakerID, b.TurnSpeakerName = a.ID, a.Name
	}
	return b
}

// operatorOf 工具审计主体（T6）：当轮说话人优先、回退会话 Owner——单用户
// 行为零变化（Owner 语义不动）；多参与者下「谁调的工具」由此可答。
func (m *Manager) operatorOf(s *session.Session) string {
	if a := s.TurnActorOf(); a != nil && a.ID != "" {
		return a.ID
	}
	return s.Owner
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
