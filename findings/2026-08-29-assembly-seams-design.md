# 2026-08-29 · 装配缝补齐设计探索

> 只出设计，未动任何生产代码与既有文档。对象 = `engine.Options` 这张封闭枚举装配面（docs/01-positioning.md:44「需要新的缝，走基座演进」——本文即该演进的先行设计）。每缝六段：现状核实（file:line）/ 场景论证 / API 草图 / 交互与兼容 / 测试策略 / 结论。允许结论为「不开」。
>
> 引用约定：行号以 2026-08-29 工作树为准（HEAD = 6d3969e）；文档引用写 `docs/xx.md:行`。
>
> 前置说明：`sandbox/sandbox.go:6-7`、`runcommand.go:30`、`engine/subagent.go:3`、`engine/reduction.go:12` 等注释引用的 `findings/2026-08-26-*.md` 真源**不在本仓**（仓库无 findings/ 目录，系自产品仓迁入时未随迁）。本文按任务要求创建 findings/ 并沿用同名惯例；涉及 sandbox 真源约束处，仅以代码注释中转述的条款（§1.3 / §2.1 / §5.2 / §7.5）为据。

## 0. 总览（结论表）

| # | 缝 | 结论 | 一句话理由 | 破坏性 |
|---|---|---|---|---|
| 1 | Tools 缺会话维度 | **开** | 定位文档两处自述多租户/数据门禁是业务一等公民（docs/01:66、:86），现状做不到；改动仅 2 个调用点 | 签名破坏（宜趁不稳定窗口） |
| 2 | sessionTools 不可裁剪 | **开** | 与「能力面不裁剪、装配面自由裁剪」（docs/01:105）自相矛盾；极简装配被迫挂上 shell 与文件写工具 | 纯新增字段 |
| 3 | 无自定义中间件缝 | 路线 (i) 消息面中间件**不开**；路线 (ii) 契约层工具包装缝**开** | (i) 需发明 contract.Message 平行 schema，成本/收益失衡且无业务背书；(ii) 与缝 5 合一 | 纯新增字段 |
| 4 | 沙箱无 provider 接口 | **开窄接口**（Backend），per-call 不开 | 仓内 dockerWrap（EINO_RUN_DOCKER）已是容器后端的粗糙前身，正规化有实锚；per-call 无场景 | 新增字段 + sandbox 内部收拢重构 |
| 5 | 事件面无拦截口 | **并入缝 3(ii)**，不单开 | 「工作时间外禁写」本质是 pre-execute 工具判定，emitFn 观测已够；与 ToolWrap 收敛为同一缝 | ——（随缝 3） |
| 6a | 按会话的 Providers 覆写 | **不开** | `NewModel` 收到的 ctx 已含 operator（manager.go:430），BYOK 场景今天即可在工厂内覆写密钥，无需新缝 | —— |
| 6b | text/thinking 出站脱敏（消息面） | **缓开** | 真实场景（合规脱敏）成立但无消费者；若开即缝 3(i) 的窄化版，触发条件与边界已划清（§3.2） | —— |
| 8.1 | 业务 Suspend 的 gob 注册出口缺失 | **开**（小） | docs/03:101-104 邀请业务返回 Suspend，但挂起态过 checkpoint 需 eino `schema.Register`——业务必须 import eino 才能用自己的 State，契约邀请了一条违约路径 | 纯新增 helper |
| 8.2 | SkillsDir 与 Tools 同型不对称 | **并入缝 1** | 与缝 1 完全同型（每轮 assemble 求值却无会话入参，manager.go:702-708），修法一字之差 | 签名破坏（随缝 1） |
| 8.3 | 沙箱环境继承面（凭据下传） | **并入缝 4 设计点** | cleanseEnv 只剥 `LLM_*` 与策略载荷（helper.go:246-256），进程内其余凭据全量下传围栏——Network 开档下即外泄面 | Policy 增字段 |
| 8.4 | 单进程会话域边界 | **不开缝，声明** | Registry 内存态 + Store 文件树形（含路径返回），横向扩展需粘性路由——是边界不是缺陷 | —— |

落地批次见 §7；第二轮严格复审（五缝之外的架构提升面）见 §8；实施方案（PR 切分/迁移指南/验收/决策点/风险）见 §9。

---

## 1. 缝一：Tools 缺会话维度

### 1.1 现状核实

- `Options.Tools func() []contract.Tool` 无参（engine/manager.go:62），`ProcessTools func() []contract.Tool` 同形（manager.go:64）；`Instruction func(sess SessionBrief) string` 却每轮实时注入（manager.go:60）——同一组装根内，提示词有会话概要、工具面没有，不对称。
- `SessionBrief` 现仅 `Mode/Model/Effort`（manager.go:48-52），无 Owner/SID。会话侧身份一直存在：`session.Session` 有 `SID/Owner`（session/session.go:56-57），`briefOf` 只投影三件套（manager.go:751-753）。
- 工具面在**每次 Run/Resume 都重组装**：`assemble`（manager.go:612）经 `runIter`（manager.go:577-583）/`resumeIter`（manager.go:586-596）逐轮调用，`Tools()` 闭包每轮被求值（manager.go:634-639）——即「每轮求值」已成立，缺的只是入参。
- 联动点一：`estimateContext` 直接遍历 `m.Opt.Tools / m.Opt.ProcessTools` 估算工具面 token（manager.go:262）；注意它**本就不计会话域件**（`sessionTools` 不在估算清单内，基座件恒定的近似）。
- 联动点二：子代理白名单源 = 进程级全量面 `ts`（manager.go:652-653 传入 `newSpawnTool`；engine/subagent.go:223-225 `filterSubTools(ts, …)`，subagent.go:184 签名）。`ts` = Tools()+ProcessTools()+sessionTools(s)（manager.go:633-640）。
- 联动点三：`ToolSearchPolicy` 分流发生在 hitl 包装与 `einoext.Adapt` **之后**（manager.go:659-690，toolsearch.go:5-7 自述），操作的是 eino 工具面——签名变更波及不到它。
- `Manager` 是进程单例（manager.go:106-107 注释「进程单例；会话态归 Registry」）；`NewManager(reg, opt)` 一份 Options 配一套 Registry（manager.go:119-127）。按租户拆实例 = 每租户一套 Registry + Options + 探测（`sandbox.Probe` 在 NewManager 触发，manager.go:123-125）。

### 1.2 场景论证

场景 A（多租户工具面，**开**的主依据）：同一业务系统内不同 owner 可见不同工具。定位文档把「领域数据门禁、多租户会话」点名为配置驱动形态塞不进、必须写代码的策略（docs/01:66），且「机制单点，策略分布」一节明确把**工具面**列为各系统互不渗透的策略（docs/01:86）——系统间（N 个组装根）已经成立，系统内按 owner 分面目前只有两条路：① 拆 Manager 实例（连 Registry、CheckPoints、WorkspaceRoot 一起拆，进程单例设计被推翻）；② 在 `Tools()` 闭包里自持 owner→工具映射——但闭包拿不到会话身份，只能靠全局变量/自建上下文传递，绕开装配面。两条都是缝缺失的信号。

场景 B（按会话模式裁剪）：弱。写面已由 hitl 模式矩阵治理（hitl.go:119-129），按 Mode 藏工具反而制造「上一轮调过的工具这轮消失」的面闪烁（模式可随消息切换，manager.go:47 注释）。**不作为依据**，设计上只提供身份（Owner/SID），不鼓励 Mode 驱动面变化（§1.4 指引）。

反面权衡：这是五条缝里唯一必须动既有签名的。仓库刚完成基座抽取（init 提交 9000041，共 5 个提交），API 处于不稳定窗口，docs/03:13 的示例同步改一行即可——现在改最便宜，越晚越贵。

### 1.3 API 草图

```go
// SessionBrief 会话概要（Instruction 与 Tools 共用入参）：三件套随消息可变；
// Owner/SID 会话身份（工具面按租户裁剪、提示词按用户定制的寻址键）。
type SessionBrief struct {
    Mode   string
    Model  string // 复合键 provider/model
    Effort string
    Owner  string // 会话归属用户
    SID    string // 会话标识
}

type Options struct {
    // …
    // Tools 业务工具面（实现 contract.Tool；入参 = 会话概要——多租户按
    // Owner 裁剪工具面；nil = 无业务工具）。闭包每轮 assemble 求值、跨会话
    // 并发调用，应快速返回且无共享可变态。
    Tools func(sess SessionBrief) []contract.Tool
    // ProcessTools 保持进程级语义不变（时钟/网页抓取等基座件的选择加入，
    // 「进程级」正是它的定义性 trait——docs/02:32）。
    ProcessTools func() []contract.Tool
}
```

实现改动点（共 2 处调用 + 1 处投影）：

- `assemble`（manager.go:634）：`m.Opt.Tools(m.briefOf(s))`；
- `estimateContext`（manager.go:262）：同改（`s` 已在作用域）；
- `briefOf`（manager.go:751-753）：补投影 `Owner: s.Owner, SID: s.SID`。

不选「新增 `SessionTools func(SessionBrief)` 平行字段」：与 `Tools` 语义重叠，两字段并存必然出现「该用哪个」的永久歧义；不稳定窗口内直接改签名是更便宜的一次性成本。

### 1.4 交互与兼容

- **nil/零行为语义**：应用闭包忽略入参即与现状逐字节等价（`func() []contract.Tool` → `func(_ SessionBrief) []contract.Tool` 一行机械改写）；`SessionBrief` 加字段是纯增量，`Instruction` 零改动自动受益（可按 Owner 定制提示词）。
- **filterSubTools 联动**：白名单源自然变为会话面。后果：`SubAgents.DenyTools`/`Tools` 若引用了对某 owner 被裁剪掉的工具名，原本的装配期 fail-fast（subagent.go:195 未知名报错）会变成**该会话 Run 时的 CONFIG 错误事件**（assemble 错误经 `errToEvent` 出 CONFIG 卡，manager.go:444-448、779-788）。可接受——loud 优于 silent；装配指引写明：子代理名单只引用对所有会话恒可见的工具。
- **estimateContext 口径**：维持现状只计 Tools+ProcessTools 的返回面（不含会话域件）——面按 owner 变化后该估算自动跟随，`usage` 事件 `EstTools` 值会因应用裁剪而变，这是估算面如实反映装配面，非破坏。
- **并发**：`assemble` 本就每轮并发执行于多会话（现状已如此），新签名不引入新竞态；在字段注释中写明闭包契约（无共享可变态）。
- **toolsearch / behaviors / spawn 面**：均消费 `ts`/`face` 下游产物（manager.go:641-657、659-690），签名变更自动一致，无独立改点。

### 1.5 测试策略（llmtest）

1. 入参断言：`Tools` 闭包捕获 brief，跑一轮 `Run`（llmtest 单轮纯文本剧本，llmtest/model.go:24-27 形态），断言收到正确 Owner/SID/Mode——白盒（engine 包内测试直取闭包记录）。
2. 行为断言：闭包按 Owner 返回不同工具，llmtest 剧本发起 `ToolCallSpec{Name: "ownerB_only_tool"}`（llmtest ToolCallSpec 具名调用）——owner B 会话执行成功、owner A 会话因工具不存在收错误事件。
3. 子代理联动：`SubAgents{DenyTools: […某个被裁剪名…]}` + 对应 owner 会话 Run——断言 CONFIG 错误事件（非静默空面）。

### 1.6 结论

**开**。场景有定位文档双处实证，成本是五缝最小（2 调用点 + 1 投影 + 签名机械改写），且是唯一受「不稳定窗口」时间压力的项——第一优先落地。

---

## 2. 缝二：sessionTools 不可裁剪

### 2.1 现状核实

`sessionTools(s)` 六族无条件硬挂（engine/sessiontools.go:51-90）：

| 族 | 构造点 | 工具 | 引擎耦合 |
|---|---|---|---|
| todo | sessiontools.go:53-55 | `todo_write` | 事件面 `todo_update`（sessiontools.go:28-30，被动：工具不发则无事件） |
| ask | sessiontools.go:56-58 | `ask_user` | pump 的 ask 中断分支（manager.go:862-880，被动分支） |
| plan | sessiontools.go:59-67 | `submit_plan` | pump 的 plan 中断分支（manager.go:884-900，被动分支）；plan 档 taskGrant 授予链 |
| fs | sessiontools.go:68-71 | `read_file/list_dir/search_files/delete_file` | **reduction 外置换指针的取回件**（reduction.go:66 `ReadFileToolName: "read_file"`） |
| cmd | sessiontools.go:72-74 | `run_command/task_output/task_stop` | 沙箱/egress 的唯一消费面（runcommand.go:32-36） |
| patch | sessiontools.go:75-77 | `apply_patch` | 无直接引擎分支 |

仅 repo 族随 `RepoMounts` 条件装配（sessiontools.go:78-88）——**先例即答案**：条件装配在 `sessionTools` 内已是既有形态，缝二只是把这个先例泛化到六族。

另有安全面事实：未列入 `ApprovalConfig.WriteTools/WritePrefix` 的工具不受审批包装、只过 errFeed+Guard 裸传（hitl.go:210-214），而审批名单是业务内容（docs/03:109-111）——基座无条件挂上 `apply_patch`/`run_command` 这类写/执行件，业务若不知道要把它们写进名单，manual 档下它们也直落。裁剪缝让不想要执行面的部署能物理移除，是比「记得配名单」更硬的收敛。

顺带核实两处现状隐患（不改，仅记录）：① 各族构造错误被静默吞掉（`if ts, err := NewTools(…); err == nil` 形态，sessiontools.go:53-77）——族会无声缺席；② `prompts.Coding()` 的注入时机本就归应用按「工作区工具面在场时」条件拼装（docs/02:65-68，prompts/prompts.go:10-12），引擎侧不存在需要条件化的提示词拼装。

### 2.2 场景论证

场景（极简装配）：纯业务问答 agent（工单助手/知识库问答）——不需要 todo、不需要计划卡、更不该有 shell 与文件写。现状下：

- 工具面/提示词面白白膨胀（`apply_patch`+`run_command`+`fsutil` 族的描述全量入上下文，`estimateContext.EstTools` 虽不计它们但模型面真实发生）；
- 行动面无谓扩大：模型可以试图 `run_command`——一个纯问答系统要在审批名单里防住自己从没要过的工具；
- 与自述矛盾：docs/01:90「新系统从最小装配（四项必填）起步」、docs/01:105「能力面不裁剪、**装配面自由裁剪**：不存在『为了一个能力接受整套产品功能捆绑』」——六族硬挂正是套装捆绑。对照 `ProcessTools` 的选择加入设计（docs/02:32），会话域件是唯一不可选的例外，例外无理由。

不选「包含名单」（默认无、逐族加）：① nil 即全量的零行为语义装不进包含名单；② 基座新增族时包含名单装配的存量应用无感漏装，排除名单则显式拒绝未知名（fail-fast 纪律对齐 `DenyTools`：subagent.go:179-183 注释自述「防拼写错静默失效」）。**排除名单胜**。

### 2.3 API 草图

```go
// engine 包内：会话域工具族名常量（SessionToolsOff 取值）。
const (
    FamilyTodo  = "todo"  // todo_write
    FamilyAsk   = "ask"   // ask_user
    FamilyPlan  = "plan"  // submit_plan
    FamilyFS    = "fs"    // read_file / list_dir / search_files / delete_file
    FamilyCmd   = "cmd"   // run_command / task_output / task_stop
    FamilyPatch = "patch" // apply_patch
)

type Options struct {
    // …
    // SessionToolsOff 排除的会话域工具族（族名见 Family* 常量；nil/空 = 全
    // 挂，零行为变化）。未知名构造期即拒（NewManager fail-fast，对齐
    // DenyTools 纪律）。repo 族不经此缝——仍由 RepoMounts 条件装配
    // （它携带 Resolver 依赖，装配语义已是条件式的）。
    SessionToolsOff []string
}
```

`NewManager` 增加校验。注意实现前提：`NewManager` 现返回 `*Manager` 无 error（manager.go:119-127），校验上抛即签名变更 `(*Manager, error)`——属破坏性改动，正因批次 A 本就是破坏性批次才放这里（决策点 D1 见 §9.4，备选 = 记录错误首个 Run 暴露 CONFIG 卡）：

```go
for _, f := range opt.SessionToolsOff {
    switch f {
    case FamilyTodo, FamilyAsk, FamilyPlan, FamilyFS, FamilyCmd, FamilyPatch:
    default:
        return nil, fmt.Errorf("engine: 未知的会话域工具族 %q（可用 todo/ask/plan/fs/cmd/patch）", f)
    }
}
```

`sessionTools` 内逐族判 `!off[FamilyX]` 再构造（sessiontools.go:51-90 就地加条件，形态与 repo 族 L78 一致）。

### 2.4 交互与兼容（逐耦合核实）

- **ask_user ↔ pump ask 分支**：分支只在 `Interrupted` 载荷为 `AskCard` 时触发（manager.go:863），卡片唯一来源是 ask_user 工具的 Suspend——工具不在场则分支永不触发。**优雅退化**。
- **submit_plan ↔ plan 分支 / plan 模式**：plan 分支同理被动（manager.go:884）。裁掉 plan 族后 plan**模式**仍可用：plan 档未授权时首个写调用中断、批准授本轮（hitl.go:125-128），任务期授权（taskGrant）随计划批准才有的路径消失——降级为「逐首轮授权」。已知瑕疵：hitl plan 档卡片文案硬编码提及 submit_plan（hitl.go:194），工具缺席时提示词指向不存在的东西——记录为可接受的文案债（改文案属外科手术范围外）。
- **todo ↔ todo_update 事件面**：事件由工具的 Store 写触发（sessiontools.go:28-30），无工具即无事件，前端待办区自然空置。**优雅退化**。
- **fs ↔ reduction 外置**：**真实耦合**。reduction 把超长工具结果外置到 `spill/` 并让模型经 `read_file` 取回（reduction.go:63-73、:87-90），裁掉 fs 族后外置指针不可取回（截断仍发生）。处置：不在引擎侧自动联动（reduction 是消息级中间件、无工具在场感知），在 `SessionToolsOff` 的字段注释与 docs/03 裁剪表写明「裁 fs 族 = 放弃外置换指针取回，长结果只剩截断头尾」——留给装配者知情决策。
- **cmd ↔ Sandbox/Probe**：裁 cmd 族后 `Sandbox` 策略无消费面，`NewManager` 的 `sandbox.Probe()`（manager.go:123-125）应一并跳过（条件加 `!off[FamilyCmd]`），避免无意义的启动告警。
- **子代理名单**：`filterSubTools` 的已知名集合随面缩小（subagent.go:186-198）——DenyTools 引用被裁族内的名字将从「通过」变为 configError。这是收紧方向的变严（loud），可接受。
- **ToolSearchPolicy**：`DynamicTools` 引用被裁名现状本就静默不命中（manager.go:664-689 按名分流，缺失即归静态、不报错）——无新交互。
- **提示词**：`prompts.Coding()` 归应用拼装已条件化（§2.1），引擎零改动；装配指引补一句「裁掉 fs/cmd/patch 族时勿拼 Coding() 段」。

### 2.5 测试策略（llmtest + 白盒）

1. NewManager 校验：`SessionToolsOff: []string{"shell"}` → 构造即报错（纯单测，无模型）。
2. 面断言（白盒，engine 包内直调 `m.sessionTools(s)`）：nil = 现有全量名集合（快照断言防回归）；`{"cmd","patch"}` → 名集合恰减两族。
3. 行为断言：裁 patch 族后 llmtest 剧本 `ToolCallSpec{Name: "apply_patch"}` → 错误事件（工具不存在）；同剧本裁前正常执行（对照）。
4. 降级链路：裁 ask 族 + llmtest 剧本正常收尾 → 全程无 ask 类事件、`session_end` 正常（退化不炸泵）。

### 2.6 结论

**开**。矛盾是文档自述级的（装配面自由裁剪 vs 六族硬挂），安全面收益真实（物理移除执行/写面），实现是 sessiontools.go 内的逐族条件 + NewManager 校验，零破坏。

---

## 3. 缝三：无自定义中间件缝

### 3.1 现状核实

- handlers 链在 `assemble` 内固定拼装，无应用注入位：steering（manager.go:698）→ toolsearch（manager.go:699-701）→ skills（manager.go:702-708）→ reduction（manager.go:709-716，注释自述「末位挂接：在 steering/skills 注入后的最终态上计数与清除」）→ summarization（manager.go:717-722）。
- 链上件类型 = `adk.ChatModelAgentMiddleware`（eino 类型，steering.go:26 等）——`Options` 若直接开 `[]adk.ChatModelAgentMiddleware` 字段，业务组装根必须 import eino，击穿「业务 0 import eino」（docs/01:104）与 contract 零 eino 类型（boundary_test.go:85-104 守卫），且业务仓的 NoEinoImports 测试（docs/03:131-151）会拦住组装代码本身。
- 工具面已有**契约层**包装先例：`hitl.WrapTools` + `mid.ErrFeed/mid.Guard` 全在 contract.Tool 层（hitl.go:206-223、mid/mid.go:20、:47），eino 类型只在 `einoext.Adapt` 桥之后出现（adapt.go:61-67）——契约层包装是本仓已验证的成熟形态。
- 业务今天确实塞不进任何东西：模型工厂 `NewModel` 是 eino 返回类型（llm/model.go:33）业务无法实现；事件回调 `emitFn` 纯消费（manager.go:134-143）。

### 3.2 场景论证与两条路线的裁定

业务诉求归三类：

**(a) 工具调用审计/脱敏/动态准入**（出站工具结果脱敏、全量调用审计、「工作时间外禁写」）——**高频、具体、缝 5 同源**（见 §5）。这类诉求作用在工具执行边界，契约层包装可完整覆盖：工具结果正是上下文增量的主通道。

**(b) 消息流整形**（text/thinking/用户历史入模前脱敏、token 预算）——场景真实（合规行业 PII）但当前无消费者，且必须走 adk middleware（消息级），即路线 (i)。

**(c) 纯观测**（最终出站态审计）——emitFn 事件流已含 text/thinking/tool 全量（contract/event.go:13-45），够用，不开。

**路线 (i) 评估（contract 层中间件抽象 + 引擎内适配）——不开。** 要不漏 eino 形状，就必须发明 contract.Message 平行 schema：role/text/reasoning/tool_calls/多模态 parts（schema.Message 的完整面，manager.go:150-219 的 runAccum 展示了其复杂度）——一个永久双轨翻译层，每随 eino schema 演进一次就追一次。换来的抽象粒度（哪些 hooks？BeforeModel 改写？流中改写？）没有业务输入可定形，必然过设计。**eino 重叠标注**：adk 的 `ChatModelAgentMiddleware` 就是既有挂点（eino 地基面已列，docs/02:114「中间件位」）——einox 内部已在消费；不缺的是「业务侧不 import eino 也能写中间件」的契约化，而这是 einox 自建的翻译成本，不是 eino 缺口。按实现序铁律反写：此处不是「eino 无 X 故自研」，而是「契约化 X 的成本无场景对价」。

**若将来开 (i)，挂接位先定清**（本节存档结论，避免届时重想）：
- **改写类必须挂 reduction/summarization 之前**——reduction 的 TokenCounter 数「整形后出站口径」（reduction.go:70、:107-125），业务改写若在其后，计数面 ≠ 发送面，clear 阈值失真；
- **只读审计类可挂最终出站态**（reduction 之后）——看到的就是发出的。
- 与本缝 (ii) 的关系：工具结果脱敏**必须在源头**（执行时改写），不能等消息层——reduction 的外置文件落的是消息内容、`read_file` 可反复取回（reduction.go:147-159），入模前最后一刻脱敏会让 spill 明文常驻盘上。这是「源头改写 vs 出站改写」的硬边界：**凡会落盘/入史的载荷，脱敏必须前置到产生点**。

**路线 (ii)（contract 层工具包装缝）——开，与缝 5 合一。** 见 §3.3。

### 3.3 API 草图（与缝 5 合一后的 ToolWrap）

```go
type Options struct {
    // …
    // ToolWrap 工具包装缝（契约层最外包装，挂 hitl 审批包装之外；主面与
    // 子代理面同挂）。nil = 不包装，零行为变化。
    //
    // 契约义务：
    //  1. Info() 须透传原名——名字是审批名单/子代理白名单/toolsearch 分流/
    //     行为标记的寻址键（manager.go:641-646 等）；
    //  2. 拒绝执行以 {"ok":false,"error":…} 信封返回回喂模型自纠（勿返回
    //     Go error——本缝在 errFeed 外层，Go error 会终止整轮，模型不可见）；
    //  3. 只能收紧不能放宽：收到的是已含审批的实例，透传即保留全部审批
    //     语义；伪造结果属违约（引擎不可校验，见设计文档单调收紧论证）；
    //  4. 包装随每次 assemble 重建（Run/Resume 各一次）——有状态包装的
    //     计数不跨轮（与 mid.Guard 同语义，mid/mid.go:45-46）；
    //  5. v1 不支持从包装内发起 *contract.Suspend（引擎三卡分叉
    //     manager.go:862-933 与决议消费链路未对应用开放）。
    ToolWrap func(t contract.Tool) contract.Tool
}
```

引擎侧统一包装序（三处 hitl.WrapTools+Adapt 调用点收拢为一个助手：manager.go:649-650 主面、subagent.go:271-273 spawn 子面、topology.go:99-100 拓扑子面）：

```go
// wrapFace 契约面统一包装序：hitl 审批 → ToolWrap（应用缝，最外）→ 适配。
// 子代理面与主面同序——审计/准入对 spawn 与拓扑子代理同样生效。
func (m *Manager) wrapFace(ts []contract.Tool, s *session.Session, mode string) []tool.BaseTool {
    wrapped := hitl.WrapTools(ts, s, mode, m.Opt.Approval)
    if m.Opt.ToolWrap != nil {
        for i, t := range wrapped {
            wrapped[i] = m.Opt.ToolWrap(t)
        }
    }
    return einoext.Adapt(wrapped)
}
```

典型用法（应用侧，零 eino import）：

```go
// 工作时间外禁写（缝五场景）：18:00 后拒写面工具，信封回喂
ToolWrap: func(t contract.Tool) contract.Tool {
    return &workHoursGate{inner: t}
}

type workHoursGate struct{ inner contract.Tool }

func (w *workHoursGate) Info() *contract.ToolInfo { return w.inner.Info() }

func (w *workHoursGate) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
    if info := w.inner.Info(); info != nil && info.Behavior == contract.BehaviorWrite && !inWorkHours(time.Now()) {
        return json.RawMessage(`{"ok":false,"error":"策略拒绝：当前非工作时间，写操作被门禁拦截——请改期或向用户说明"}`), nil
    }
    return w.inner.Invoke(ctx, args)
}
```

### 3.4 交互与兼容

- **包装序与单调收紧（结构性论证）**：`ToolWrap` 收到的是 `approvalTool`（写）或 `errFeed+Guard`（读）已包装实例（hitl.go:209-221）。调用 `t.Invoke` 必经内层审批判定——包装**没有任何语法路径**绕开 ArgsForce（hitl.go:134-141 判定先于一切放行分支）或模式审批；它只能「额外拒绝」。这就是单调收紧的实现：不是靠运行时检查（不可校验伪造结果的包装），而是靠组合方向——应用缝永远在审批外层。同理它覆盖会话域件（`ts` 含 sessionTools，manager.go:640、649）与子代理面（§3.3 收拢点），补上了「审批名单是业务内容、基座件可能没进名单」（§2.1 安全面事实）时的最后一道应用侧闸。
- **与 toolsearch 分流**：分流在 Adapt 之后按名分（manager.go:664-689），`Info()` 透传故名字不变，包装件被移入动态面后仍带包装。ArgsForce 不因动态装载豁免的既有不变量（toolsearch.go:5-7）随包装序保持。
- **与审批挂起/续流**：manual 档写工具先中断，`Resume` 重放时经 checkpoint 还原再进包装→审批→执行（adapt.go:47-58 恢复态注回；hitl.go:149-181 恢复分支）——包装在重放路径上同样被穿过，时间判定在重放时刻重算（正确语义：批准在 10 点、重放在 18 点 → 拒）。
- **deny 信封语义**：与全仓工具失败信封同构（runcommand.go:411-413 `fail`、docs/03:102「业务性失败回喂模型自纠」）——模型可见、可调整方案或 ask_user，不杀轮。
- **三态（allow/deny/ask）的裁定**：deny 经信封；ask 需从包装内发 `Suspend(ApprovalCard)`，机制上能走通（pump approval 分支按卡型分叉，manager.go:905、timers.go:181）但决议消费（`DecisionSource.TakeDecisionFor` 多槽，hitl.go:77-86）未对应用开放、业务挂起态的 gob 注册（hitl.go:102-106 形态）也缺应用侧出口——**v1 不开**，真实场景出现时再评估（届时优先考虑把 `DecisionSource` 契约化而非扩 ToolWrap）。
- **零行为**：nil 时 `wrapFace` 退化为现两行，逐字节等价。

### 3.5 测试策略（llmtest）

1. 零变化：nil ToolWrap 下既有全量测试不动作即回归。
2. 审批不可豁免：manual 档 + 写工具 + passthrough 包装 → 仍发 `approval_request`（断言事件），包装计数只在 Resume 决议后才 +1（证明包装在审批外层）。
3. deny 回喂：剧本轮 1 调写工具（被包装拒）→ 断言收到 `tool_result` 而非 error 事件，且结果信封文案可辨；轮 2 剧本改调 ask_user 正常走（模型自纠路径畅通）。
4. 会话域件覆盖：包装计数 `apply_patch`/`run_command` 的调用（sessionTools 过缝）。
5. 子代理面覆盖：`SubAgents{EmitEvents: true}` + 剧本经 spawn 调白名单内工具 → 包装计数 +1。
6. 重放时刻判定：审批挂起后改门禁时间窗，Resume → 拒（信封）——时间语义在重放点重算。

### 3.6 结论

路线 (i) **不开**（平行消息 schema 的翻译税无场景对价；eino 挂点本就存在，缺的是契约化对价）；路线 (ii) **开**，与缝 5 合一为 `ToolWrap`——一个缝覆盖审计/脱敏/动态准入三类诉求，纯新增字段、包装序有结构性单调收紧保证。

---

## 4. 缝四：沙箱无 provider 接口

### 4.1 现状核实

- `sandbox.Policy` 静态单份：三档 Mode/Network/WritableRoots/Env/ProtectedReadOnly/Limit（sandbox.go:54-65），注释自述「静态一次装配（真源 §7.5——PM 会话工作区固定，per-call 留演进）」（sandbox.go:54-55）。
- 消费链：`Options.Sandbox *sandbox.Policy`（manager.go:92-94）→ `runcommand.Config.Sandbox`（runcommand.go:34）→ `buildCmd` 的沙箱分支调 `sandbox.Wrap`（runcommand.go:258-281）。
- 后端按平台**编译期**分流，无运行时接口：`Wrap` → `wrapOSBackend`（helper.go:196-208；Linux re-exec 哨兵 / darwin 哨兵内包 sandbox-exec / windows token 侧挂），构建标签见 docs/02:74-79。
- 探测三态如实上报是明文设计约束（sandbox.go:33-40 Enforcement；helper.go:105-113 Probe 进程级缓存）。
- **仓内已有容器后端的粗糙前身**：`dockerWrap`（runcommand.go:214-234）——`EINO_RUN_DOCKER=1` 环境变量开关，命令包进一次性容器（工作区挂载/内存/网络硬编码）；优先级定死 docker > 沙箱并告警「沙箱 policy 被绕过」（runcommand.go:243-256）。它绕过 Policy（策略形同虚设）、走 env 魔法而非装配面、参数不可配——正是缺 provider 接口逼出来的临时形态。
- 真源 `findings/2026-08-26-einox-sandbox-design.md` 不在本仓（§前置说明）；其可考约束：§1.3 后端姿态（off/auto/require，sandbox.go:24-31）、§2.1 re-exec 哨兵协议（helper.go:1-7）、§5.2 装配优先序（runcommand.go:243-249）、§7.5 静态策略裁决（sandbox.go:54-55）。`require` 档与 Docker daemon 探测被真源明示为出界项/留待（helper.go:117-118 注释「Docker daemon 探测随 Docker 后端落地再接（真源 §5.3 出界项）」）。

### 4.2 场景论证

场景 A（容器/强隔离后端，**开**的依据）：部署形态表把「需要强隔离再上 gVisor / 微 VM」写进了边界选型（docs/02:91），服务器形态「服务整体容器化 = 第一层粗粒度边界」（docs/02:89）——内核沙箱在容器内本就降效（Landlock 管容器内横向，管不住容器面），容器原生后端（挂载/网络/配额经容器运行时施加）是正交的真实形态。仓内 dockerWrap 的存在证明需求已露头，只是现在以 env 魔法 + 绕过策略的方式活着。

场景 B（per-call 策略）：无场景。「不同命令不同围栏」可由静态 workspace-write 档 + 审批矩阵（run_command 本就是写工具，ArgsSkip 白名单只读豁免已有，runcommand.go:376-409）近似覆盖；真源 §7.5 的静态裁决没有出现被推翻的事实。**不开，仅记录演进位**。

自研后端是否强制同一哨兵？分层裁定：**OS 级后端（进程内施加）必须遵守哨兵协议**——围栏施加点在 fork 后 exec 前（helper.go:2-4 的技术必然性），绕开哨兵 = 绕开应用 main 挂钩点 = `Probe` 探测不到（helper.go:119-131 哨兵握手是探测第一步）；**容器/远程后端不依赖哨兵**——围栏在容器运行时/远端施加，命令不 re-exec 本体，但 Probe 义务不变（daemon 不可达 = unusable，如实上报）。

### 4.3 API 草图

**放哪一层**：`sandbox` 包，不进 `contract/`。理由：`contract/` 是业务写适配器的端口面（Tool/Suspend/事件），而 Backend 的出入参是 argv/env 这类 OS 执行概念；且 `Options` 直接引用机制包类型已是既有惯例（`hitl.ApprovalConfig`/`sandbox.Policy`/`egress.Validator`/`repo.Resolver`，manager.go:75、:89、:94、:98）。**形状**：不发明新形态——就是把既有自由函数 `sandbox.Wrap(pol, workspace, cmdLine) (argv, env)`（helper.go:203-208）方法化，任务书设想的 `confine(argv, policy)` 形态即此。

```go
// sandbox 包内：

// Backend 沙箱后端面：把 Policy 翻译成一次命令的执行参数。默认 OSBackend
// （平台内建，构建标签分流——现状行为原样收拢，零变化）；应用注入容器/
// gVisor/微 VM 等自定义后端（engine.Options.SandboxBackend）。
//
// 义务：①Probe 如实上报三态（Enforcement 语义同现状，sandbox.go:33-40），
// 策略映射不全时报 partial + Uncovered（如 ProtectedReadOnly 容器映射不了）；
// ②只许收紧不许放宽——readonly 档不得给出可写挂载，宽于 Policy 的翻译即
// 后端实现违约；③不修改 pol（并发共享的静态单份）。
type Backend interface {
    // Wrap 一次命令的沙箱化执行参数。nil argv = 本次不可沙箱（调用方按
    // Backend 姿态降级：auto 裸跑已告警；require 拒跑——姿态接线仍留待
    // 首个消费者，真源 §1.3）。
    Wrap(pol *Policy, workspace, cmdLine string) (argv, env []string)
    // Probe 后端可用性（缓存策略归实现自担；OSBackend 维持进程级一次）。
    Probe() Status
}

// OSBackend 平台内建后端（现状 sandbox.Wrap / sandbox.Probe 的收拢壳）。
var OSBackend Backend = osBackend{}
```

```go
// engine.Options：
// SandboxBackend 自定义沙箱后端（nil = sandbox.OSBackend）。与 Sandbox
// 策略正交：策略（静态单份）定「施加什么」，后端定「怎么施加」。
SandboxBackend sandbox.Backend
```

消费链改动：`runcommand.Config` 增 `Backend sandbox.Backend`（nil 归一 OSBackend），`buildCmd` 的 `sandbox.Wrap(...)` 调用点（runcommand.go:259）改经后端；`NewManager` 的 `sandbox.Probe()`（manager.go:124）改经后端。**首个消费者 = dockerWrap 正规化**：迁入 `sandbox` 包实现 Backend（Policy.Mode → 挂载读写模式、Network → --network、Env/参数可配化），退役 `EINO_RUN_DOCKER` 魔法与「策略被绕过」告警分支（runcommand.go:250-256）——自验证接口完备性。

per-call 演进位（存档不开）：`buildCmd` 调用点（runcommand.go:320）是唯一的策略取用处——未来若开，形态为 `Options.Sandbox` 从 `*Policy` 增一姊妹字段 `SandboxFor func(cmdLine string) *Policy`（nil = 静态单份），`run()` 在 `buildCmd` 前解析。触发条件：出现「同会话内命令分级围栏」的真实部署诉求。

### 4.4 交互与兼容

- **零行为**：`SandboxBackend` nil + `runcommand.Config.Backend` nil 全走 OSBackend，收拢重构前后 argv/env 逐字节一致（现测试 runcommand_sandbox_test.go / _linux_test.go 是回归锚）。
- **Policy 序列化不受影响**：`policyPayload` 仍只在 OS 后端哨兵链路使用（helper.go:43-47、218-222）；容器后端不序列化 Policy（翻译成容器参数），接口不含 JSON 面。
- **`Policy.Validate` 构造期校验保留**（runcommand.go:173-177 现状）——对自定义后端同样先校验再翻译（校验是策略面的，与后端无关）。
- **依赖白名单**：零新增（接口在仓内；容器后端如引 SDK 才需更新 boundary_test.go:18-40——设计上不引，docker 经 exec 调 CLI 即可，与 dockerWrap 现状同路径）。
- **探测告警语义**：NewManager 只在 `Sandbox != nil` 时探测（manager.go:123-125）——扩为经后端探测；缝二裁 cmd 族时两者联动跳过（§2.4）。

### 4.5 测试策略

1. OSBackend 等价回归：既有 sandbox 测试全量不动（构建标签分流照旧）。
2. 假后端注入（纯单测）：fake Backend 返回定值 argv/env → `buildCmd` 断言走注入后端；fake `Probe` unusable → auto 语义裸跑路径（现状 Wrap 返回 nil 分支，helper.go:204-206）。
3. dockerWrap 迁移回归：`EINO_RUN_DOCKER` 行为等价进新 Backend（argv 前缀断言），退役开关后旧 env 不再生效（防止双轨残留）。
4. 收紧义务测试（示例后端）：readonly Policy 下后端给出可写挂载属实现违约——接口层不可校验，落在 Backend 文档义务 + 示例后端测试（诚实记录这是约定不是机制）。

### 4.6 结论

**开窄接口**（Backend + Options.SandboxBackend + dockerWrap 正规化为首个消费者）；**per-call 不开**（真源 §7.5 裁决无推翻事实）。优先级低于前三缝（无迫近消费者，但接口形状宜在更多沙箱消费面出现前定稳）。

---

## 5. 缝五：事件面无拦截口（→ 收敛入缝三 ToolWrap）

### 5.1 现状核实

- 应用经 `emitFn` 是纯消费者：`Run/Resume` 的回调只收已定型事件（manager.go:134-143 emit；docs/02:19「回放与 live 同源」），事件在回调前已 `s.Record` 落会话记录——即便允许改写回调入参也改不了已落盘的记录。
- 动态策略无挂点：`ApprovalConfig.ArgsSkip/ArgsForce` 是 `map[string]func(args string) bool`（hitl.go:42-49）——func 但注册期固定，判定入参只有参数串，无时间/会话/调用者上下文，且**只能影响审批包装内部**（豁免或强制审批），不能表达「独立于审批的准入拒绝」。
- 工具实现内硬编码是现状唯一出路——与「机制归基座」相悖：每个业务工具自嵌时间窗判定，策略散落不可审计。

### 5.2 收敛裁定

「工作时间外禁写」拆开看：**判定时机 = 工具执行前**（pre-execute），**作用面 = 工具调用**（含基座会话域件），**可见性要求 = 模型可感知拒绝并可改道**（deny 回喂）。这四项全部落在工具执行边界——正是缝三路线 (ii) 的 ToolWrap 定义域；而「事件面」只是这件事的观察窗（deny 发生后 tool_result 事件自然携带信封，event.go:107-114）。**事件面本身不需要拦截口**：观测已有 emitFn，改写事件既改不了落盘真源也无场景。故裁定：**缝五不单开，收敛为缝三的 ToolWrap**（合一 API 见 §3.3），避免两条重叠的口。

### 5.3 单调收紧语义的保证（补充 §3.4 的专节论证）

要求：动态钩子只能比静态审批更严，绝不能放行豁免审批/ArgsForce。三层保证：

1. **结构层（机制性）**：包装组合方向固定为 `ToolWrap ∘ hitl ∘ errFeed/Guard ∘ raw`（§3.3 wrapFace）——应用包装持有的 `t` 是闭住审批的不透明实例，Go 类型系统内不存在「拆开内层」的路径。放行豁免在语法上不可表达。
2. **语义层（约定性）**：伪造结果（不调 `t.Invoke` 自行返回成功信封）在机制上不可校验——任何最外层包装都有此理论自由度（包括现状的 hitl 本身相对工具实现亦然）。处置与全仓一致：契约义务写进字段文档（§3.3 义务 3），违约属应用自毁，不设计运行时防御（简单优先）。
3. **事件层（可观测性）**：deny 走信封 → `tool_result{ok:false}` 入事件流（event.go:107-114）——拒绝行为对审计可见，事后可查。这是「钩子曾经收紧过」的证据链，配合 emitFn 覆盖审计诉求。

deny 回喂模型与 ArgsForce 的 bg 档 fail-closed 语义（hitl.go:139-141）天然一致：子代理内 deny 信封同样回喂子模型改道，不挂起（后台无人决议）——零额外设计。

### 5.4 测试策略 / 5.5 结论

随 §3.5 合并执行（同一缝）。**结论：并入缝三，不另开缝。**

---

## 6. 探索中发现的额外缝

### 6.1 按会话（owner）的 Providers 覆写——**不开**

**现状**：`Options.Providers func() []llm.ProviderSpec` 进程级（manager.go:56-57），`assemble`/`imageCapableOf`/`newSpawnTool`/`newTopologySub`/`genTitle` 五处消费（manager.go:613、:745；subagent.go:234；topology.go:77；manager.go:1137）。
**场景**：多租户 BYOK（租户自带 API key）。表面上需要 `func(owner string) []ProviderSpec`。
**不开的理由**：BYOK 的真实形态是**同一 provider 清单、按 owner 换密钥**——`NewModel` 工厂收到的 `ctx` 就是带着 operator 的 `runCtx`（manager.go:430 `contract.WithOperator` → :621 传入），业务自定义 `NewModel` 内 `contract.OperatorOf(ctx)` 查表覆写 `p.APIKey`（ProviderSpec 按值传入，llm/llm.go:58-67）即可，**现有装配面已覆盖，零新缝**。局限如实记录：① 清单本身按 owner 全新分集（不同租户不同 provider 集合）做不到——该场景未现身；② `genTitle` 的 ctx 无 operator（manager.go:1135），标题生成回落进程级密钥——可接受的降级，非破坏。
**触发重评条件**：出现「按 owner 的 provider 集合分集」的真实部署。

### 6.2 text/thinking 出站脱敏（消息面）——**缓开**（边界已划清）

归并入 §3.2 路线 (i) 的存档结论：真实场景（合规 PII）成立但无消费者；**若开**须遵守 §3.2 挂接位裁定（改写类在 reduction 之前、审计类在后；凡落盘载荷脱敏必须前置到产生点——本缝因 text 直接入史/入 replay 而必然是「产生点改写」，即 assistant 出流时逐段脱敏，成本高于工具面）。与缝 3(ii) 的分工：工具结果脱敏走 ToolWrap（源头），纯文本面走本缝（缓议）。**不单独成缝，重评随缝 3(i) 触发条件。**

### 6.3 事件面观测增强——**不开**

emitFn + 事件落盘已覆盖（§5.2）；「拦截/改写事件流」与「回放 = live 同源」（docs/02:19）的架构不变量冲突，无场景背书。

---

## 7. 优先级排序与落地批次

排序原则：① 破坏性变更优先于纯新增（不稳定窗口的折旧曲线：越晚定的签名越贵）；② 场景强度（定位文档实证 > 仓内实锚 > 合理推演）；③ 依赖关系（缝 5 随缝 3、缝 2 的 cmd 联动随缝 4）。

| 优先 | 项 | 形态 | 场景强度 | 依据 |
|---|---|---|---|---|
| P0 | 缝 1：Tools 会话化 + SessionBrief 扩 Owner/SID | **签名破坏** | 定位文档双处实证（docs/01:66、:86） | §1 |
| P0 | 缝 2：SessionToolsOff 排除名单 | 新增字段 | 自述矛盾 + 安全面物理收敛（docs/01:105） | §2 |
| P1 | 缝 3+5 合一：ToolWrap | 新增字段 | 双缺口收敛（审计/脱敏/动态准入），零 import eino | §3、§5 |
| P2 | 缝 4：sandbox.Backend + dockerWrap 正规化 | 新增字段 + 收拢重构 | 仓内实锚（runcommand.go:214-256）+ 部署文档预留（docs/02:91） | §4 |

**批次 A（同一批落定，一次改完装配面叙事）**：缝 1 + 缝 2 + 8.2（SkillsDir 同型签名，随缝 1）+ 8.8（构造吞错小修，随缝 2 改动区重叠）。两者共同完成「多租户工具面」的完整故事——基座族按部署裁（缝 2）、业务件按 owner 裁（缝 1）、skill 包按 owner 物化（8.2）；docs/03 装配面表与示例同步一次改齐。API 不稳定窗口内破坏性签名只此一处，窗口越早用掉越便宜。
**批次 B**：缝 3+5 的 ToolWrap（纯新增，可与批次 A 同 PR 评审但独立可回退；`wrapFace` 收拢是它的全部引擎侧改动）+ 8.1（`einoext.RegisterSuspendState` 注册出口——契约面 Suspend 邀请与 0-import 承诺之间的实测漏洞补齐）。
**批次 C（有触发条件）**：缝 4 + 8.3（Policy 环境档，同域一并定稿）。触发 = 首个真实容器/gVisor 需求，或 dockerWrap 绕过语义被部署实际投诉；接口形状（§4.3）先定稳即可，实现可等消费者。8.3 的凭据外泄面若先于容器需求被部署提出，可单独先行（纯 Policy 字段）。
**文档声明项（随首个落地 PR）**：8.4 单进程会话域边界补进 docs/02 部署形态节。
**明确不开（记录在案）**：缝 3 路线 (i) 全量消息中间件抽象（§3.2）；per-call 沙箱策略（§4.2 场景 B）；owner 级 Providers（§6.1）；事件面拦截（§5.2、§6.3）；ToolWrap 的 ask 三态（§3.4）；远程会话域（§8.4）；文案国际化/会话级迭代预算/流式工具契约/CloneHistory 类型面（§8.9）。

**依赖白名单影响**：全部设计零新增外部依赖（缝 4 的 docker 后端经 exec 调 CLI，与 dockerWrap 现状同路径）——boundary_test.go:18-40 白名单不动。

**eino 重叠汇总**（实现序铁律的对账）：缝 1/2/3(ii)/5 均为 einox 装配面自身概念，eino 无对应物（adk ToolsConfig 消费的是 einox 已组装好的面）；缝 3(i) 若开则复用 adk middleware 既有挂点（docs/02:114）——不缺挂点缺契约化对价，故不开；缝 4 与 eino 无关（LLM 框架不含执行沙箱），自研已按「平台内建优先」路线完成，接口化只是把既有实现收拢为默认后端。

---

## 8. 第二轮严格复审：五缝之外的架构提升面

> 复审方法：不再以任务书五缝为锚，按层重扫——契约面（contract/）、会话域（session/）、装配面（engine.Options 全字段逐个）、持久化与生命周期（persist/sweep/checkpoint）、安全面（env/凭据/序列化）、性能面。五缝结论全部维持原判，且各获一条新佐证（8.1 佐证缝 3 的 Suspend 讨论、8.2 佐证缝 1 的不对称论证、8.3 佐证缝 4 的后端设计、8.8 升级 §2.1 的记录项）。新发现按「值得开 / 并入既有缝 / 观察项 / 不开」四档处置。

### 8.1 业务 Suspend 的 gob 注册出口缺失——**开**（小而实）

**现状**：adk checkpoint 是 gob 编码、接口字段持有的具体类型必须注册——eino 序列化真源明文（eino@v0.9.13 schema/serialization.go:60-88「Concrete types that are assigned to interface fields」；adk/react.go:96、:217）。仓内每个 Suspend 生产者都因此在自己的 init 里调 eino `schema.Register`：askuser（askuser.go:32-33）、plan（plan.go:33-35）、hitl（hitl.go:103-105）、einoext 桥（adapt.go:24）。

**缺口**：docs/03:101-104 邀请业务工具「需要挂起交互（审批同构）：return nil, &contract.Suspend{…}」——但业务的 State/Info 类型过 checkpoint 同样必须 gob 注册，唯一注册口是 eino 的 `schema.Register`。即：**契约面邀请了一条实现上强制业务 import eino 的路径**（不注册则首个挂起在 checkpoint 序列化时即报 gob 未注册错误，非跨进程才暴露）。这是「业务 0 import eino」承诺上的实测漏洞，且完全沉默——文档无一句提及。

**API 草图**：

```go
// einoext 包内（业务可 import einox 机制包——Options 本就引用 hitl/sandbox 类型）：

// RegisterSuspendState 注册业务挂起态类型（Suspend.Info/State 过 checkpoint
// 的 gob 要求；等价转发 eino schema.Register——业务勿直接 import eino）。
// 在业务包 init() 中对每个用作 Suspend 载荷的具体类型调用一次。
func RegisterSuspendState[T any]() { schema.Register[T]() }
```

配套：docs/03 扩展业务工具节的 Suspend 示例补此行（随实现 PR 一并，本文不改既有文档）。仓内四个既有注册点是否迁移到该 helper——**不迁**（外科手术：仓内 import eino 合法，改动无收益）。

**测试**：定义带自定义 State 的测试工具经 llmtest 走一轮挂起→Resume（对照：不注册的用例收序列化错误，注册后全链通过）——即该 helper 的正反例。

**结论**：**开**，批次 B（与缝 3 的 Suspend 讨论同域，落地互为印证）。

### 8.2 SkillsDir 与 Tools 同型不对称——**并入缝 1**

**现状**：`Options.SkillsDir func() string` 进程级（manager.go:72-73），但与 Tools 一样是**每轮 assemble 求值**（manager.go:702-708）——缝 1 论证的「每轮求值却无会话身份」不对称在此重复出现，且 skill 是业务内容（docs/02:61「物化归应用」）、多租户 skill 包（按 owner 物化不同技能目录）与多租户工具面是同一场景族。

**处置**：随缝 1 同批改 `SkillsDir func(sess SessionBrief) string`——同型签名、一字之差的调用点改动（manager.go:703），单独立项反成噪音。**并入批次 A。**

### 8.3 沙箱环境继承面（凭据下传）——**并入缝 4 设计点**

**现状**：`cleanseEnv` 只剔除策略载荷与 `LLM_*` 前缀（helper.go:246-256）——进程环境里的云凭据（`AWS_*`/`GITHUB_TOKEN`/数据库 DSN 等业务自设变量）**全量随命令下传围栏**。断网档（Network 默认 false，sandbox.go:58）下不可达；但内网形态文档明示 Network 须开（docs/03:71「内网形态须配 on」）——开了网络又继承凭据，围栏内命令即可携凭据出网。denylist 式清洗（记住要剥谁）对未知凭据名结构性失效。

**处置**：不是新缝，是缝 4 Policy 的数据面补充——`Policy` 增环境档字段（语义：`inherit` 现状缺省零变化 / `minimal` 仅 PATH/TMP/HOME 假根 + `Policy.Env` 显式注入——allowlist 式）。纳入批次 C 与 Backend 一并设计（两者都在「沙箱策略表达力」域内，一个 PR 定稿避免 Policy 两动）。**场景实**（凭据外泄面），优先级随缝 4。

```go
// sandbox 包内：
type EnvMode string

const (
    EnvInherit EnvMode = ""        // 缺省零值 = 现状：全继承 − LLM_* − 策略载荷
    EnvMinimal EnvMode = "minimal" // 白名单：PATH/TMPDIR/HOME(指向可写根或省略) + Policy.Env
)

type Policy struct {
    // …既有字段不变…
    // EnvMode 围栏内环境档：denylist（缺省，零行为变化）→ allowlist 切换。
    // minimal 档下业务需要的环境一律经 Env 显式注入（缓存重定向既有惯例）。
    EnvMode EnvMode
}
```

实现点：`payloadEnv`（helper.go:218-222）按档切换 base——`os.Environ()`（inherit）或最小集（minimal）；windows 裸跑降级路径（runcommand.go:271-274）复用同一 env 数组，天然同档。验收锚：minimal 档下围栏内 env 断言不含 `AWS_*`/`GITHUB_TOKEN` 类样例凭据。

### 8.4 单进程会话域边界——**不开缝，声明为架构边界**

**现状**：`Registry` 会话表在进程内存（session/session.go:763），`Store` 契约是文件树形且 `UserTreeDir(operator) string` 直接返回本地路径（session.go:44-46——spill 取回 reduction.go:89 与工作区都吃这个路径假设）。横向多副本部署下会话必须粘性路由到创建它的进程。

**处置**：这不是缺陷——定位文档的目标形态是「单一 Go 静态二进制」（docs/01:98）与库形态嵌入（docs/01:36），单进程会话域是形态的自然推论。开「远程会话域」缝 = 推翻 Store 契约 + Registry 重构，零场景对价。**处置 = 文档声明**：建议随首个落地 PR 在 docs/02 部署形态节补一句「会话域为单进程内存态，多副本部署需按 owner 粘性路由」（本文红线不改既有 docs，记录为建议）。**触发重评**：出现真实多副本部署诉求（届时优先评估 Store 去 UserTreeDir 化，而非直接上远程会话）。

### 8.5 长会话持久化 O(n²) 重写——观察项

`Registry.persist` 每次全量序列化整个 `sessionRecord`（含全部 Events 与 Messages，session.go:1035-1061，MarshalIndent），而 persist 在每个状态迁移点触发（轮末 finishOf、轮中入史 manager.go:454、标题生成 manager.go:1157）——n 事件的会话累计 O(n²) 字节写。长会话（数千事件）下这是可测的量级。**处置**：不开缝不预优化；触发条件 = 实测长会话出现秒级轮末延迟（届时增量事件日志或 Events/Messages 分文件即可，Store 契约不用动）。

### 8.6 每轮模型组件重建——观察项

`assemble` 每 Run/Resume 全量重建（模型组件 manager.go:621、agent、handlers、工具面）。组装体本身是廉价结构体；唯一存疑点是 `NewChatModel` 每轮新建模型组件是否损失 SDK 层连接复用（取决于 eino-ext 组件内部 http.Client 形态，未实测）。**处置**：不预设；触发条件 = 高频会话场景实测连接建立开销显著（届时缓存粒度是「会话模型快照 → 组件」一级，Options 面无需变化）。

### 8.7 Sweeper 无自主触发——观察项

TTL 清理搭 `Registry.Create` 便车触发（session.go:793、:1608 注释），无定时器——长寿命服务若长期无新会话，过期清理不跑。**处置**：观察项；触发条件 = 部署反馈磁盘残留（补一个 `Sweeper.Run(ctx, interval)` 由应用装配即够，无需 Options 面）。

### 8.8 sessionTools 构造错误静默吞——随缝 2 的小修（升级 §2.1 记录项）

§2.1 已记录：六族构造 `if ts, err := NewTools(…); err == nil` 静默吞错（sessiontools.go:53-77），族会无声缺席——对「装配错误在启动期暴露」的形态承诺（docs/01:40）是破洞，且缝 2 落地时恰好要逐族加条件（改动区重叠）。**处置**：随缝 2 同 PR 修复（严格说这是缺陷修复而非缝，单列避免混入缝语义）。实现边界（修正：本节初稿曾称 fs/cmd/patch 族校验可前移 NewManager——**不成立**，三族的 Root = `workspaceOf(s)` 是会话级路径（sessiontools.go:68、manager.go:94-95 `WorkspaceRoot(owner, sid)`），NewManager 时无从求值）：`sessionTools` 签名改 `([]contract.Tool, error)`，唯一调用方 assemble（manager.go:640）把错误转 `configError` → CONFIG 错误事件——与 DenyTools 的 per-Run 暴露形态一致（subagent.go:195）；族名拼错则由缝 2 的 NewManager 校验在更早的启动期拦下（§2.3）。

### 8.9 复审后明确不开的其余项（记录在案）

- **基座文案中文硬编码**：错误卡（manager.go:809-818）、沙箱拒绝提示（sandbox.go:95）、审批卡 Note（hitl.go:194）等用户可见文案全为中文定死。非中文业务需消息目录缝——**不开**（无场景；触发 = 首个非中文业务系统接入）。
- **maxIterations 进程级全局**：全局 var + env 覆盖（manager.go:601-609），非 Options 字段——多租户无法按会话限流。**不开**（会话级轮次预算无场景；现状全局护栏已兜底失控）。
- **contract.Tool 无流式面**：契约仅同步 Invoke（contract/tool.go:12-18），eino 有 stream tool 形态未透出。**不开**（无流式工具场景；工具结果的流式呈现已被事件面 tool_call/result 卡片模式覆盖）。
- **session.CloneHistory 露 eino 类型**：公开方法返回 `[]*schema.Message`（session.go:387-388，session 包 import eino schema，session.go:22）。引擎内部消费面（Run 组装输入），业务无须调用；Go 允许不 import 类型包而持有其值，0-import 承诺实践上不破。**处置**：文档级澄清（contract 是端口面、session 是应用协作面但非端口——CloneHistory 非业务 API），不改代码。

### 8.10 复审总结论

五缝即「装配缝」维度的完备集——第二轮按契约/会话/持久化/安全/性能五个纵面重扫，**没有推翻任何一项原判**，新发现中真正新增可开的只有 8.1（Suspend 注册出口，小）；8.2/8.3 是既有缝的同域增强（分别并入缝 1 与缝 4）；其余为边界声明、观察项与记录在案。架构面上最大的非缝事实是 8.4（单进程会话域）——它应当被声明而不是被修补。

### 8.11 并发 spawn 的存量数据竞态（批次 B 落地时实测发现，非本批引入）

`go test -race` 下三个既有测试告警（TestSpawnConcurrentOverlap / TestBgNotifyBudgetGuard / TestBgSessionGateReserve，前两个偶发）：竞态点在 eino adk 内部——`chatmodel.go:1145/1147`（buildMessageReActRunFunc 闭包写实例态）。成因链：`newSpawnTool` 构造**单个** ChatModelAgent 复用于全部 spawn 调用（subagent.go:284-288 → adk.NewAgentTool），同一 agent 实例被并发 Run 时 einox 侧复用模式 × eino 实例态无锁 = 竞态。**已在批次 A 提交点复现（暂存批次 B 改动后 -race 仍报），非批次 B 引入**；批次 B 全部新测试（含子代理面覆盖）在 -race 下通过。处置方向（未实施，需单独分析）：每调用新建子 agent（与「assemble 每轮重建」的既有形态一致）或上游修复；触发条件 = 并发 spawn 场景上生产前必须解决。仓内测试基线（`go test ./...`，AGENTS.md 约定）不含 -race，本发现不阻塞批次提交。

---

## 9. 实施方案（PR 切分 / 迁移指南 / 验收 / 决策点 / 风险）

> 把 §7 批次推进到可执行粒度。所有改动以本文引用的行号为锚，落地时以实际代码为准。红线继续有效：本节仍是设计，不预写生产代码。

### 9.1 PR 切分与验收标准（DoD）

**PR-A1 · 批次 A 核心（缝 1 + 8.2 + 缝 2 + 8.8）**

| 改动 | 位置 | 内容 |
|---|---|---|
| SessionBrief 扩字段 | manager.go:48-52 | +`Owner`/`SID`；briefOf 补投影（manager.go:751-753） |
| Tools 签名 | manager.go:62 | `func(sess SessionBrief) []contract.Tool`；注释补并发契约（跨会话并发求值、应快速返回） |
| SkillsDir 签名 | manager.go:72-73、:703 | 同型改（决策点 D2） |
| 调用点 | manager.go:634、:262 | assemble / estimateContext 传入 `m.briefOf(s)` |
| 裁剪面 | sessiontools.go | Family* 常量 + `Options.SessionToolsOff` 字段 + 逐族 off 判定；`sessionTools` 签名改 `([]contract.Tool, error)`（8.8），assemble（manager.go:640）转 configError |
| NewManager 校验 | manager.go:119-127 | 未知族名上抛（决策点 D1：签名 `(*Manager, error)`）；Probe 增 cmd 族在场判定（manager.go:123-125） |

DoD：§1.5 三用例 + §2.5 四用例全绿；既有测试的 Tools 闭包机械改写后全绿；`go build ./...` + boundary 两测不动即过。
规模估计：生产代码净变化 ~70-100 行，测试 ~150 行（含 5+ 测试文件的闭包签名适配）。

**PR-A2 · 批次 A 文档同步**：见 §9.3 清单前两行。独立成 PR 便于代码评审聚焦。

**PR-B1 · 批次 B（缝 3+5 ToolWrap + 8.1）**

| 改动 | 位置 | 内容 |
|---|---|---|
| Options.ToolWrap 字段 | manager.go Options | 注释载明五条契约义务（§3.3） |
| wrapFace 收拢 | manager.go:647-651 | 新助手（§3.3 草图）；assemble 改用 |
| 子面接线 | subagent.go:271-273、topology.go:99-100 | 改用 wrapFace（mode 实参："auto"/"bg" 与 "auto"） |
| RegisterSuspendState | einoext 新增 | `func RegisterSuspendState[T any]() { schema.Register[T]() }`（8.1） |

DoD：§3.5 六用例 + 8.1 正反例（不注册收序列化错 / 注册后挂起-Resume 全链通）；nil ToolWrap 下全量既有测试零变化（收拢重构的等价性证明）。
规模估计：生产 ~80 行，测试 ~200 行。

**PR-C1 · 批次 C（缝 4 + 8.3）**

| 改动 | 位置 | 内容 |
|---|---|---|
| Backend 接口 + osBackend | sandbox 包 | §4.3 草图；既有 `Wrap`/`Probe` 行为原样收拢（argv/env 逐字节等价，既有 sandbox 测试为安全网） |
| Policy.EnvMode | sandbox.go:56-65、helper.go:218-256 | §8.3 草图；payloadEnv 分档 |
| Options.SandboxBackend | manager.go Options + :123-125 + sessiontools.go:72 | 注入链：NewManager Probe 经后端、runcommand.Config 增 Backend 字段（buildCmd 调用点 runcommand.go:259） |
| dockerWrap 正规化 | runcommand.go:214-234、:250-256 → sandbox 包 | 迁为首个 Backend 消费者；退役 `EINO_RUN_DOCKER` 分支与绕过告警（决策点 D4） |

DoD：§4.5 四用例 + minimal 档 env 白名单断言；EINO_RUN_DOCKER 旧 env 不再生效（防双轨）；既有 runcommand/sandbox 全量测试绿。
规模估计：生产 ~250-350 行（含 docker 迁移），测试 ~150 行。

### 9.2 批次 A 迁移指南（业务仓机械改写）

```go
// 前
Tools:     func() []contract.Tool { return myTools() },
SkillsDir: func() string { return skillsDir },
m := engine.NewManager(reg, opt)

// 后（忽略入参 = 行为不变；需要按租户裁剪时用 sess.Owner / sess.SID）
Tools:     func(_ engine.SessionBrief) []contract.Tool { return myTools() },
SkillsDir: func(_ engine.SessionBrief) string { return skillsDir },
m, err := engine.NewManager(reg, opt)   // 仅 D1 采纳时
if err != nil { /* 启动失败 */ }
```

要点：① 闭包被多会话并发求值（每轮 assemble 一次），勿在闭包内做无锁共享写；② `SessionToolsOff`/`ToolWrap`/`SandboxBackend` 均可不填（零行为变化）；③ 业务仓的 NoEinoImports 守卫（docs/03:131-151）不受影响——新增 API 全在 einox 包内。

### 9.3 文档同步清单（随对应 PR）

| PR | 文档 | 改点 |
|---|---|---|
| A2 | docs/03:7-27、:30-49 | 装配面表：Tools 行注会话入参、+SessionToolsOff 行、SkillsDir 行改签名；最小装配示例改写（含 NewManager error 形态——D1 定稿后） |
| A2 | docs/02:32 | 工具族分类句补「会话域件可经 SessionToolsOff 裁剪；裁 fs/cmd/patch 族勿拼 prompts.Coding()」 |
| B1 | docs/03:101-104、装配面表 | Suspend 示例补 `einoext.RegisterSuspendState[MyState]()` 行；+ToolWrap 行（含契约义务摘要） |
| B1 | AGENTS.md 架构速览 | 工具装配链句补 ToolWrap 环节 |
| C1 | docs/02 沙箱节、docs/03 装配面表 | Policy 增 EnvMode 说明、+SandboxBackend 行、EINO_RUN_DOCKER 退役说明 |
| 首个落地 PR | docs/02:87-93 部署形态节 | 8.4 单进程会话域声明（粘性路由） |

### 9.4 开放决策点（maintainer 拍板项）

| # | 决策 | 推荐 | 备选与代价 |
|---|---|---|---|
| D1 | NewManager 是否改 `(*Manager, error)` | **采纳**——唯一真正兑现「启动期暴露」（docs/01:40）的形态；批次 A 本就是破坏性批次，边际成本一行 | 记录错误、首个 Run 暴露 CONFIG 卡：零破坏但启动期不暴露，与缝 2 的 fail-fast 论证自相矛盾 |
| D2 | SkillsDir 签名是否随批次 A | **随**——同型同窗口，单独立项是噪音 | 延后：SessionBrief 扩展先落（D2 无关字段），SkillsDir 后补小 PR；代价是多一次评审周期 |
| D3 | EnvMode 形态 | string 枚举、零值 = inherit（零行为变化靠零值天然成立） | bool `EnvMinimal`：更少代码但 Policy 面再演进时要改型 |
| D4 | 批次 C 首个 PR 是否即迁 dockerWrap | **迁**——接口无消费者即无验证，dockerWrap 是现成被验对象 | 接口先行、迁移后补：接口未经使用即定稿，违背「先挖尽/实证」精神 |

### 9.5 风险与回退

- **批次 A**（唯一破坏性批次）：上游影响面 = 各业务组装根一处 + 本仓 5+ 测试文件，全部机械改写；回退 = revert 单 PR。风险集中在「测试适配遗漏」——`go vet`/编译期即暴露（签名变更无处可藏），风险低。
- **批次 B**：纯新增字段 + wrapFace 收拢（三处既有调用行是唯一触碰点）；nil 语义下收拢前后逐字节等价由全量既有测试背书。回退 = revert。
- **批次 C**：sandbox 收拢重构的等价性以既有平台测试为安全网（构建标签分流照旧）；docker 迁移**改变 EINO_RUN_DOCKER 既有部署行为**（env 开关退役）——changelog 显著标注，属批次 C 明示的破坏点。回退 = revert（接口与迁移同 PR，不存在半迁移态）。
- **共同不变量**：全程零新增外部依赖（boundary_test.go:18-40 白名单不动）；contract/ 零 eino（boundary_test.go:85-104 不动）；四批全部 nil 缺省零行为变化。
