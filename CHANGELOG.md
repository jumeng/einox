# Changelog

einox 版本日志。v0.5.0 起发布时实时维护；更早四版于 2026-09-07 回溯补档——v0.2.0/v0.4.0 为 tag 注文原文搬运（零转述失真），v0.1.1/v0.3.0 为区间提交提炼（无版本说明原文，细节以 git log 区间为准）。

## v0.7.0（2026-11-22）

自 v0.6.0（2026-11-02）以来 2 个提交。版本主线：**eino 地基升 v0.9.23 + llm 契约类型再出口——三层架构红线（eino → einox → einox-work，严禁跨层调用）落地：应用侧 eino 契约消费唯一入口收敛到 `llm` 包**。纯 drop-in 升级 + additive 别名面，零行为变化、零既有 API 改动。

### 新增：契约再出口（llm/types.go）

- 应用（einox-work）消费的模型/消息/流式契约以类型别名再出口，与 eino 原类型同型零运行时成本：`Message`/`AgenticMessage`/`ToolCall`/`FunctionCall`/`RoleType`/`Assistant`（常量）/`AssistantMessage`/`StreamReader`/`StreamWriter`/`Pipe`（泛型函数薄包装）/`BaseModel`（泛型别名，密封联合 `*Message | *AgenticMessage`）/`ChatModel`（=`model.BaseChatModel` 特化）/`Option`——应用摘除 eino 直依赖的唯一适配面，后续 eino 升版只需保证此面稳定。

### 依赖

- **eino v0.9.19 → v0.9.23**：v0.9.21 model usage 暴露缓存写入 token（einox-work 批 B/D5 缓存命中明细的数据源）/ v0.9.22 媒体消息 provider 文件引用 / v0.9.23 流式 tool call 拼接保留 `Extra` + nil map panic 修复 / v0.9.20 adk 取消工具流 checkpoint 保留。官方无破坏性变更标注，drop-in 零源码改动，全量门禁 `go build ./... && go test ./... -race`（24 包）亲跑验证。
- **eino-ext**：claude v0.1.25 → v0.1.26；tool 族十模块快照 2026-09-16 → 2026-11-19（openai v0.1.13 已最新不动）。

## v0.6.0（2026-11-02）

自 v0.5.1（2026-09-07）以来 17 个提交、89 文件 +8599/−471。版本主线：**HITL 审批面四缝扩展 + dsh/zcode 两系能力吸收收口 + eino 地基升 v0.9.19**。无破坏性变更、无 Go API 签名变化——全部为 Options/ApprovalConfig 可选装配缝与 additive 契约字段（nil/缺省 = 零行为变化）；einox-examples 自 v0.5.1 直升本版构建+vet 零改动验收在案。

### 新增：HITL/审批域（应用 einox-work P14–P17 消费；设计件均在 findings/）

- **会话域写授权——「本会话内始终允许」决议档**（1198fa3，findings/2026-09-23-session-write-grant-design.md）：`ApprovalDecision.Always` + session 写授权集（GrantWrite/WriteGranted，随会话记录持久）+ 挂起项工具映射（授权边界 = 只能授权「刚问过的」工具，fail-closed）。
- **guarded 第五档**（73f0331，findings/2026-10-31-hitl-guarded-mode-design.md）：`contract.ModeGuarded` + `ApprovalConfig.GuardedRisk`（参数级风险判定，命中挂起其余直落；ZCode「自主模式，有风险时询问」对位）。
- **WorkspaceMounts 挂载区写放行**（9357fc7，findings/2026-11-01-workspace-mounts-design.md）：ResolveUnder 对宿主自建越区挂载区放行（区内越区/穿越仍拒），applypatch/fsutil 透传——工作区产图与项目会话写轮缺口闭合。
- **批 A′ 三缝**（10d74e2，findings/2026-11-01-hitl-batch-a-prime-design.md）：①edit 第四档（`ModeEdit`+`EditPass` 名单直落、其余回落 manual 逐审；上游 checkEditMode 提取）②D15 提问到点自动续行（`Options.AskAutoResolve`——CAS 登记 SetAskDecisionIf/记 ask_decision(Decider=auto)/Resume；闭包到点求值=实时生效；`DefaultAskResolve`=Options[0]/空文本）③D11 外部持久授权咨询（`Options.WriteGrantSource(SessionBrief, tool)`，会话域授权之后挂起之前，fail-closed；ArgsForce 不被豁免）。

### 新增：引擎/工具面（dsh T14–T21 增量与 zcode 能力吸收）

- **goal 域最小核**（e35ddb8，C4）：持久完成目标 + goal_change 事件 + direct-human 门控 + 模型工具面（get/create/update_goal）+ GoalDriver 自动续行装配位。
- **auto 档模型评审门**（04c3cfe，C5）：`Options.AutoReview`——reviewer 模型分级裁决（low/medium+allow 放行、deny 挂起、high 硬拒、失败 fail-closed）。
- **子代理 inbox 控制面**（d115fb0，C6/T19）：队列所有权围栏 + 渠道排队控制四件套（Approve/Answer/ EditQueue/RemoveQueue）+ 后台任务个体控制（Tasks/TaskOutput/StopTask 查询缝随 a5a95de 落 runcommand）。
- **模型切换历史注入 + 系统提示生命周期**（5e210a5，C1/N1）：model_change 事件 + 上轮/本轮模型差异注记（只进模型投影面）；LastInstruction/AnchorInstruction 锚定语义。
- **图片预算省略决策事件化**（4a2c741，C3）：occurrence 粘性选择集，vision 驱逐决策可审计。
- **MCP resources/instructions 面**（1fd2a36，N2/T21）：资源三共享工具 + server instructions 指令段（空 provider 零成本）。
- **read→spill→read 防环**（1c27522，C2/T15）：read_file 进 TruncExcludeTools。
- **zcode 能力吸收四批**（2382900）：基座边界审查 + 编程工具链主干 + GoalDriver + 沙箱姿态 + 快照/超时转后台。

### 依赖与修复

- **eino v0.9.13 → v0.9.19**（f9f959b）：checkpoint 修复三连（嵌套流 backport/边界流转换器/嵌套载荷去重——adk 挂起恢复与子代理嵌套即目标路径）+ summarization 出错路径 nil ctx 修复 + agentsmd 单文件上限；eino-ext tool 族九模块快照随批同步。纯 drop-in 零源码改动，全量门禁+交叉编译亲跑验证。
- **DeepSeek 预置目录对齐 V4.1 世代**（1a4dd42）：v4-flash 收敛/输出上限 384K/v4-pro 多模态标记；旧名服务端仍路由零兼容动作。
- **两枚 -race 高压 flake 根因修复**：首轮标题在途注册窗口（f2ff084，MarkTitleFlight 前移建立 happens-before，注入延迟 3/3 红→绿实证）；后台通知自续链测试 join 收口（b463b3d，waitBgQuiescent 自然收口，五测试 150 连跑零失败）。

## v0.5.1（2026-09-07）

自 v0.5.0（2026-09-06）以来 13 个提交、22 文件 +1461/−11（含 CHANGELOG 维护两笔）。版本主线：**装配知识层（einox-assemble）+ 挂起轮首轮标题修复**。无破坏性变更、无 Go API 变化——消费者零改动升级。

### 修复

- **经挂起 + Resume 收尾的首轮不生成会话标题**（U-1，装配知识层实测发现）：挂起时 `flushAcc` 已把半轮 assistant 消息入史，轮收尾以「历史已有 assistant」判非首轮——被挂起切断的首轮误判，标题恒回退 Task。修法：`session` 增内存态 `turnFirst` 标记（`Run` 入口锚定 `!hasAssistant`、跨挂起保留、Resume 收尾消费；不落盘——跨进程恢复的挂起轮零值回退，与标题输入 `turnUserMsg` 同降级），`settleTurn` 改读标记。红→绿护栏 `TestSuspendedFirstTurnGeneratesTitle`。
- **消费面注意**：依赖「挂起轮无标题槽位」的 llmtest 剧本需补标题槽（`genTitle` 走 Generate 同耗剧本）；`TurnEpilogue` 先于异步标题执行的既有时序不变（记忆环首条仍是 Task 回退）。

### 新增

- **`assemble/` 装配知识层（einox-assemble skill，纯文档资产零代码）**：业务 agent 开发的选型与装配知识真源——`SKILL.md`（六步流程：场景定位/逐域确认/清单落盘/生成内核/对账验收/增量模式）+ `manifest-spec.md`（`einox.agent.yaml` 能力装配清单格式，九域三态）+ `rules.md`（必选/依赖/互斥律 + 档位语义 + 选型域序）+ `recipes/`×4（minimal/coding/support/data-analysis 预设组合）+ `patterns/`（骨架 + 九域装配套路样板，骨架经 `/tmp` 实测 `go build` 通过）。知识层随 module 分发（`go list -m -f '{{.Dir}}'` 定位，与 require 版本严格同版）；复制到 `~/.agents/skills/einox-assemble` 即成引导式 skill。经零上下文子代理全流程实测（产物 = einox-examples memory 示例），八处实测缺口已回改。
- README 快速开始增「AI 装配」节（零克隆分发通道与业务仓 AGENTS.md 自发现指引）。

## v0.5.0（2026-09-06）

自 v0.4.0（2026-09-03）以来 12 个提交、144 文件 +10991/−3284。版本主线：**多参与者会话模型 + 装配式默认界面（web/TUI）+ 会话派生族**三块新能力面，配套全仓五轮审查修复（含竞态与安全面）。事件词表自此冻结（只增不改名不删除）。

### ⚠️ 破坏性变更与迁移

- **`ChannelGateway.Approve`** 签名变更：`(sid, itemID string, d ApprovalDecision) bool` → `(sid, itemID string, decider *contract.Participant, d ApprovalDecision) error`。新增 decider 入参（决议者身份，进回执）；返回值 bool → error（`ErrNoPendingDecision` = 幂等迟到静默，其余拒绝告警可见）。
- **`ChannelGateway.Answer`** 签名变更：新增 `decider *contract.Participant` 入参。
- **`llm.NormalizeEffort` → `contract.NormalizeEffort`**：函数迁移至契约面（消除 session→llm 依赖边），llm 侧无兼容垫片，调用点需改 import。
- **`tools.ModelArgError` → `contract.ModelArgError`**：同上（einoext 桥与工具构造器共用，错误面归契约），tools 侧无垫片。
- **构造期 fail-fast 前移**：`Topology.Kind` 未知名、`SubAgentsConfig.Output.Schema` 根非 object 现于 `NewManager` 构造期即拒（原为首次 assemble 时）——持有非法静态配置的应用启动即红，不再延迟到首轮运行。

### 新增

**多参与者会话模型**（一个会话多个人用，分得清谁是谁）

- `contract.Participant` 与会话名册：`UpsertParticipant` 首见 joined / 在册 updated，落 `participant_update` 事件（回放重建 roster 真源）；名册随 Fork/ForkAt/Side/Reattach 继承。
- 说话人身份链全链路：UserMsg/QueuedMsg/SteerEvent 带 speaker、审批卡带 Requester、决议带 Decider、模型历史 `Extra[einox.speaker]` 投影 + 「名字：」前缀渲染、transcript 署名；`Operator` 升级为当轮说话人（空回退 Owner，单用户零变化）。
- 审批路由与决议守卫（装配缝，nil 不挂零变化）：`Options.ApprovalRouter` 按会话概要裁决「问谁」（target 随卡 + pending 跨重启续接）；`Options.DecisionGuard` 决议者与目标不一致 fail-closed 拒绝。
- `hitl.ArgsForceBy` 按人参数级强制审批（取当轮说话人；只能收紧，按人豁免有意不做）。
- feishu 群聊第二个及以后发送者不再被静默丢弃（open_id 全链透传）。

**装配式默认界面 `ui/` 新包**（应用不 import 不进构建；鉴权缝 `Config.Authorize`，nil = 零变化）

- Web 回放 + 交互面：REST+SSE（列表/详情/增量事件、live 无洞无缝 + 断线 since 续传、SSE 慢消费补投）；composer 带身份发送、审批交互卡（批准/拒绝带 decider）、参与者名册端点。
- Go TUI（`ui.Serve`）：进程内零协议层，单二进制/SSH/loong64；会话列表 + 事件时间轴 + 检查器 + 过滤 + live 跟尾。

**会话派生族**

- `ForkAt` 锚定分叉：按 HistLen 锚截历史与事件流，负锚/零值锚 fail-closed；anchor=0 即 Fork 同义。
- `Side` 辅助对话：继承父历史快照（父运行中可用——执行中问快速问题的核心场景）、工作区/spill/transcript 共享父域、父删除级联。

**引擎机制**

- `Options.Hooks` 订阅式工具钩子：Pre 可否决（信封回喂自纠、panic fail-closed）+ Post 观察；主面与子代理面同挂，审批拒绝的调用也过钩子（审计看终局）。
- 溢出恢复：`llm.Classify` 增 OVERFLOW 类（不可重试、排除 failover）；超窗有界恢复一次（清窗兜底同源重装配，出站口径降才重试）。
- 摘要收敛不变量：压缩产物 ≥ 被压缩段即拒落地，走清窗兜底（防「越压越大」病态摘要）。
- 子代理终态细分：`SubAgentEvent` 增 `StopReason`（completed|aborted|error|max_tokens）与 `Partial`（失败带半成品）；同步/后台同语义。
- spawn 结构化回传：`SubAgentsConfig.Output{Schema}`；子面注入 `spawn_submit`（校验失败自纠有界、已提交后模型调用合成截断自然终结零空跑）。
- `OutputContract` 输出契约：工具声明 `OutputSchema+Render`，装配期探测，校验失败信封回喂自纠。
- 摘要缓存：阈上会话二次 Run 零摘要请求（固化 + 水位 + 失锚 fail-open 重放；ForkAt/Side 不拷贝）。
- 便利面：`session.EventAs[T]` 泛型取值器、`contract.ErrCode*` 错误码常量、`engine.ErrDecisionRejected` sentinel、`DecisionOut.Items` 逐项回执（分歧态回放真源）。

### 修复

**正确性/竞态**

- channel.go 常驻订阅泵数据竞态（`-race` 实红级——Unbind 后泵仍投递、锁外读）；`s.Model` 裸读违反 ModelSnapshot 纪律 ×3；NotifyOwner 滞留尾巴绕后台通知预算守卫；turnActor 跨轮残留致审计/审批归因失真；Run 早失败丢本轮消息入史；运行中注入消息不进会话历史；bg spawn 预算边缘丢已提交结论；TUI 摘要对活会话全空白；TUI 检查器切片越界 panic；ui 控制面 goroutine 被请求 ctx 掐死；SSE 慢消费丢事件（补投 + 节拍追赶）；前端 TDZ 与 EventSource 泄漏。

**安全**

- ui `?owner=` 查询参数路径穿越（改「在册身份查表」+ `session.ValidOwner` 围栏 + tstore 隔离名三层）；`ResolveUnder` symlink 逃逸（EvalSymlinks 双侧锚定）；`IsSafeReadCommand` 写型命令漏网（find -delete/-exec、git branch 等收窄纯列表形态 + 11 反例）；`PathBlocked` 大小写折叠；runcommand 进程组整组终结 + NUL/64KB 输入加固；office zip 条目 64MB 解压上限；egress 补 224/4、240/4、64:ff9b::/96。

**渠道/工具**

- feishu SDK 无超时（单 flushLoop 黑洞即全站卡片冻结）、cardHub 退出协议（滞留泵 panic + Close 永久阻塞）、chats 轮末淘汰、建卡序列串行化；hitl 拒绝信封手拼 JSON（reason 含引号即非法 JSON）；Guard ⚠ 前缀击穿 Digest 判红；ArgsForce plan 档批准误 GrantTurn 整轮放权；applypatch 落盘段事务化（预检 + 留底回滚 + 拒覆盖）。

### 内部改进（无 API 影响）

- manager.go（1745→899 行）拆 pump/usage/history/title 四件、session.go（2230→820 行）拆 registry/queued/fork/sweeper 四件——纯搬家，符号逐一核对。
- 重复实现单点化：token 估算 msgTokenEst、模型构造链 newShapedModel、工作区圈禁 tools.ResolveUnder、失败信封 tools.Fail、session 读取 readSessionRecord、internal/{strutil,shortid,calendar}。
- TUI/JS 事件词表对齐 32 事件 + 对账测试（词表漂移机器守卫）；golden 快照测试基建四场景；测试补强批（steering 注入全锚/名册继承矩阵/PathBlocked 17 例等）。
- boundary 守卫增内部依赖方向断言三规则；CI 交叉编译矩阵扩六目标 + test job 增 `-race` 门（engine/session/channels/ui）。

### 依赖

- 新增 `golang.org/x/term v0.45.0`（TUI 终端原始模式，MIT）。
- `golang.org/x/sys` 0.35.0 → 0.47.0。

### 验证

22 包 `-count=1` 全绿；`-race`（engine/session/channels/ui）绿；交叉编译 linux/{amd64,arm64}、windows/amd64、darwin/{amd64,arm64}、linux/loong64 全过；gofmt/vet 干净。五轮独立审查收口（详见提交信息）。

## v0.4.0（2026-09-03）

> tag 注文原文搬运。自 v0.3.0 以来 10 个提交、51 文件 +2851/−1089。

**破坏性变更：**

- repo 挂载族下沉应用层：repo 工具族与工作区持久/写保护机制移出基座默认面、转装配面能力件——应用自备挂载族（勘察件/写保护/只读按需装配），基座只留契约与注入缝
- 会话 Session.Scope 字段整删：准遗留展示字段（创建写死、此后无机制消费），记录/列表/详情/分叉链不再携带 scope；存量 JSON 旧字段回放自然忽略，零迁移

**新增：**

- 消息渠道能力面三层：engine 渠道编排泵（入站 Handle 分流/出站事件订阅补投/决议回写续流/Cancel/Push/(channel,chat)→sid 绑定表落盘重启找回）+ 官方件 channels/feishu（长连接+出站卡片流式+审批提问卡按钮）+ channels/voice 流型语音占位 + Options.Channels 装配缝（nil 零变化+构造期校验）
- 应用预置注入面与挑选权：ResolveFileCatalog/ResolveCatalog 应用自备完整目录（呈现与运行时同源、目录外条目不被补全）、MergeProviders 泛化合并原语、llm.RewriteSpec 规格改写工厂缝（厂家私有适配归应用层，基座只按改写后规格构造客户端）
- 模型切换标志调用边界语义：选择器切换只写快照不发事件；NoteModelCall 每次模型调用前比对「本次将用 vs 上次实际调用」不同才落 model_change；last_used_model 随会话记录持久化跨重启不断
- webfetch TLS 姿态注入口：Config.Insecure/RootCAs——缺省严格校验；企业自签内网装根为正解、显式跳过为逃生口

**调整：**

- 内置目录裁撤 deepseek-anthropic 兼容端点条目（只留官方 openai 协议端点；存量引用转未命中原样透传，零迁移）

**修复：**

- 质量审查轮批次：feishu 出站断线（cards.cli 漏接+sendStandalone nil 防御）、三竞态闭合（sanitizeHistory 深拷贝×persist 锁外 marshal、审批超时 ClearPendingIf 单锁原子清挂起、guardTool 有状态加锁）、吞错日志化四处（tag 注文截于此，余见 v0.3.0..v0.4.0 区间提交）

## v0.3.0（2026-08-31）

> 区间提炼（无版本说明原文）。自 v0.2.0 以来 8 个提交、29 文件 +1403/−133。主线：**模型面双供应商 + 引擎机制批 + 选型文档定稿**。

- 智谱（BigModel）供应商落地：BuiltinProviders 增 zhipu 条目（glm-5.3 纯文本 / glm-5.3-flash 多模态）、`dialect=glm` 思考方言（线格式与 deepseek 同形共用）、Classify 业务码细化（智谱 429 族欠费/套餐类致命码不空转重试）
- 思考等级四档化（关档回归）：NormalizeEffort 增 off 一等档——旧值 on/max→max、off 恢复关档本义、未知→默认 low（**旧值归一行为变化**，消费面详见区间提交 9f15879）；anthropic 关档不发思考块 / deepseek·glm 方言关档 disabled / 通用 effort 方言 off→none
- 优化方案评审修订版落地（fa2e6ea，18 个测试）：Resume 入口整备（BeginResume 单锁原子查清挂起域，重复/并发第二 Resume 明确 error）、审批超时器 recover、hitl/askuser/plan 三包 gob round-trip 兼容回归、ModelSpec.NoToolCalls 能力门控（assemble 期 CONFIG 拒）、Registry.Drain 优雅停机、Options.ContextBudget 常驻面预算告警、后台子代理 usage 上卷、Registry.Fork 全量快照分叉、tool_result 后节流 persist
- store 逻辑键拼接统一 path.Join（UserTree 读写跨平台键形态一致，Windows 反斜杠修复）
- docs/01 五路线叙事四连精化（总表/复合形态分析法/README 定位句）

## v0.2.0（2026-08-29）

> tag 注文原文搬运。自 v0.1.1 以来 17 个提交、60 文件 +4791/−377。

**变更（不兼容）：**

- engine.NewManager 返回值改为 (*Manager, error)，必填项缺失在构造期报错
- Options.Tools / SkillsDir 闭包签名改为 func(SessionBrief)，支持按会话身份裁剪
- 移除 EINO_RUN_DOCKER 环境变量，容器沙箱改由 Options.SandboxProvider 注入
- 会话域工具族构造失败由静默跳过改为报错
- 文档篇目改号（能力 02→03、装配 03→04），旧链接失效

**新增：**

- 模型故障转移（FallbackModels）：主模型重试耗尽后按序切换备用模型
- FinalGate 收束质量门：轮次结束按应用判据验证，不合格自动重跑
- 跨会话记忆：recall 检索历史会话，TurnEpilogue 轮收尾回调
- 容器沙箱后端（sandbox.DockerProvider）
- AGENTS.md 注入（AgentsMD）

## v0.1.1（2026-08-29）

> 区间提炼（无版本说明原文）。初版建档：Initial commit 后 6 个提交、128 文件 +24240——einox 通用 agent 基座初始公开面。

- 循环引擎（Manager/ReAct）、会话域（Registry/Store/快照）、HITL 审批（挂起-续流）、四平台沙箱、通用工具族（fsutil/runcommand/applypatch/todo/askuser/plan）、契约面 contract（零 eino）初始形态
- AGENTS.md 仓库工作说明定稿；boundary_test 边界守卫（contract 零 eino + 外部依赖白名单——清单外 import 即测试失败）；装配指南与装配缝设计文档（五缝逐缝论证）
