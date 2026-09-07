# Changelog

einox 版本日志。本文件自 v0.5.0 起入库维护；更早版本未回溯整理，变更见 git log 与各 tag 区间提交信息。

## v0.5.1（2026-09-07）

自 v0.5.0（2026-09-06）以来 11 个提交、21 文件 +1387/−3。版本主线：**装配知识层（einox-assemble）+ 挂起轮首轮标题修复**。无破坏性变更、无 Go API 变化——消费者零改动升级。

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

## 更早版本

| 版本 | 日期 | 说明 |
|---|---|---|
| v0.4.0 | 2026-09-03 | 未维护 changelog，变更见 git log |
| v0.3.0 | 2026-08-31 | 同上 |
| v0.2.0 | 2026-08-29 | 同上 |
| v0.1.1 | 2026-08-29 | 同上 |
