# HITL 批 A′ 三缝设计(edit 档 / D15 提问自动续行 / D11 项目级授权咨询)

日期:2026-11-01 · 状态:已实施 · 来源:P17 批 A′(einox-work 执行提示词 docs/prompts/2026-11-01-p17-batch-a-prime-prompt.md;三缝同属 hitl/审批域,合一设计件分节写)

上游 ground truth(只读镜像 `apps/zcode-cli/packages/core/src/permission/service.ts`,ZCode v3.17.1 = a60ba6a):

- **edit 档**(service.ts:580 checkEditMode):`permissionName==="edit" && sideEffectScope==="workspace"` → 放行;**其余回落 build 档逐审**。
- **D15 提问自动续行**(bootstrap/src/zcode-protocol-v4/interaction-registry.ts):askUserQuestion 挂起配 hiddenGrace→visibleCountdown→deadline 计时;到点 resolve `{action:"accept", content:{answers:{}}}` 续行(accept+空 answers = 显式自动继续语义,ask-user-question.ts:133「The user did not provide answers... Continue using your best judgment」);snooze=首次有效操作**永久暂停**该问的自动结束(幂等,重复/迟到 noop);设置关闭→取消全部计时并持久 snoozed,重开只对新问题计时。
- **D11 项目级规则**(service.ts:285 matchesProjectRules + 175-222):ruleset `{allow/ask/deny:[{toolName, ruleContent}]}`,按工具名匹配(Write/Edit 互通名)+ 入参 subjects(command/url/file_path/path/pattern/patch_text)内容匹配;deny/ask 先于放行链,allow 在 plan 限制之后。

**与上游的两处登记差异**(P17 提示词裁定口径,应用侧登记 FEATURE-MAP):

1. 自动续行决议取 **AskReq.Options[0]**(无选项取空文本)——提示词 2026-11-01 明文裁定;上游为 accept+空 answers(模型自会「用最佳判断继续」)。本仓取首选项 = 给模型确定性答案,语义更收敛。
2. 设置关闭后挂起中的问句**回落既有超时取消分支**(fail-closed 兜底不变);上游为持久 snoozed 无限等待。理由:本基座挂起域的 30 分钟兜底是全 kind 统一安全网,保留优于摘除。

## 缝① edit 档(EditPass)

### 动机

ZCode 协作模式值域 `build|edit|plan|yolo|guarded`(v4 switchCollaborationMode);本基座 hitl 已有 manual/plan/auto/guarded 四档,缺 edit(「Edit automatically」——编辑免审、其余逐审)。P14 断点项 D1 剩余。

### 机制

矩阵增第六行(既有行不动;上游语义对位——编辑类=文件写族,直落面由名单圈定):

| 写工具 edit | EditPass 命中 → 裸传直落;未命中 → 回落 manual 逐审(会话域 Always/外部授权咨询同 manual) |
| ArgsForce/ArgsForceBy | 任何模式一律挂起(不变量不变——判断权在人的写值,edit 不豁免) |
| 读工具 | 任何模式裸传(不变量不变) |

批准续行路径与 manual 共用;edit 档批准不授本轮/任务期权(逐次语义,与 guarded 同)。

### 契约面

- `contract.ModeEdit = "edit"`(与 ModeGuarded 并列)。
- `hitl.ApprovalConfig.EditPass map[string]bool`(可选;nil 或未命中 = edit 退化 manual 逐审零新增行为)。map 而非 func:上游判定是工具级静态属性(permissionName+sideEffectScope),无参数维度——名单内容归应用风险规则表编译注入(基座只持机制)。
- `needsApproval` 增 `case "edit"` 分支,置于 default(manual)之前。

## 缝② D15 提问自动续行(AskAutoResolve)

### 动机

ask_user 挂起到点现行=分支取消(EvAskTimeout + ended,模型从拒绝反馈中知晓)。ZCode 语义=到点自动选默认答案续行(无人值守任务不停摆)。生效时机按 2026-11-01 裁定=**实时**(推翻 PHASE3-DESIGN §3.1 重启生效建议):闭包到点才被调用,设置翻转经原子值即时生效——闭包在装配期挂一次,读法是每次调用时求值。

### 机制

expirePending(kind="ask") 到点行为开关化,判定序:

1. `m.Opt.AskAutoResolve == nil` → 既有取消分支(零变化)。
2. 从事件流回溯本次挂起的 `EvAskRequest` 载荷(倒序扫描,`ask_id` 匹配;重启后 Data 为 map 形态,经 marshal/unmarshal 还原)。找不到 → 取消分支(fail-closed,不给闭包喂空载荷)。
3. 调 `AskAutoResolve(req)` 返回 nil → 取消分支(应用侧开关关闭/该问已被 snooze——snooze 状态归应用,闭包自查)。
4. 返回决议 → **CAS 登记** `s.SetAskDecisionIf(askID, d)`:仍挂起该 ask 且无作答才写(抢到锁者赢;并发人工作答/Answer 端点抢先即让位——与 ClearPendingIf 同 TOCTOU 纪律)。CAS 失败 → return(人答将消费)。
5. CAS 成功:DeciderID/Name 缺省补 `"auto"`(可审计)→ `RecordAskDecision`(ask_decision 事件=回放重建提问卡终态真源)→ `Persist`(决议先落盘再续流,与 Channels().Answer 同序)→ `go Resume`(BeginResume 原子消费挂起态)。

决议内容(上游语义基座化):默认决议 = Options[0].Value(无选项取空 FreeText)——基座提供 `engine.DefaultAskResolve(req) *contract.AskDecision` 助手,应用闭包=`开关读数 + DefaultAskResolve` 两行;应用也可自定决议形态(缝返回什么就用什么)。

**重启边界**:进程停机期间已过点的挂起问句,由 Registry.RecoverInterrupted 镜像超时(cancel 分支)——自动续行不追溯;未过点的经 RearmPendingTimer 续表,到点正常走本缝(map 形态载荷还原)。上游将倒计时持久化为 snoozed 态跨重启续算,本仓不做(30 分钟兜底窗内重启属边缘态,差异随应用登记)。

### 契约面

- `engine.Options.AskAutoResolve func(contract.AskReq) *contract.AskDecision`(nil = 现行取消分支零变化)。挂 engine(挂起超时域归 engine/timers.go,非 hitl 装配域)。
- `session.SetAskDecisionIf(askID string, d AskDecision) bool`(原子 CAS;与 ClearPendingIf 同锁同纪律)。
- `engine.DefaultAskResolve(req) *contract.AskDecision`(上游默认决议语义:Options[0]/空文本)。

snooze 不进基座:上游幂等语义(首次有效操作永久暂停该问)在应用闭包内实现(snoozed 集合自查,返回 nil 即回落取消分支)——基座无需感知。

## 缝③ D11 项目级授权咨询(WriteGrantSource)

### 动机

PHASE3-DESIGN §3.2:项目级持久审批规则(approval-rules.json)的**存储/匹配/UI 全在应用**;基座只缺一个外部授权源咨询缝(fail-closed)。会话档 Always(1198fa3)已落,项目级档与其同约束(只授权问过的同类写工具,不给通配)。

### 机制

needsApproval 判定链增一环——**在会话域授权(writeGrants/turnGrant/taskGrant)之后、回落挂起之前**咨询:

```
manual/edit 回落: WriteGranted(会话 Always) → WriteGrantSource(外部项目规则) → 挂起
plan:            TurnGranted → TaskGranted → WriteGranted → WriteGrantSource → 挂起
```

- `ArgsForce` 不被本缝豁免(Invoke 中强制判定先于 needsApproval,既有不变量保持——外部规则放行不了「判断权在人」的写值)。
- auto/guarded/bg 档不咨询(本就直落,无授权判定面)。
- fail-closed:nil 或返回 false = 照常挂起——缝只能放行,不能收紧出新模式(收紧=挂起,本就是缺省)。
- 信任边界:咨询入参=(SessionBrief, tool 名)。规则内容匹配(路径前缀/参数 subjects)归应用闭包内部——基座不见规则形态。

### 契约面

- `engine.Options.WriteGrantSource func(SessionBrief, tool string) bool`(nil = 零变化)。
- `hitl.ApprovalConfig.WriteGrantSource func(tool string) bool`(会话级绑定形态;engine.wrapFace 装配期把 Options 的 SessionBrief 形态经 briefOf 绑成闭包注入——与 Tools/Instruction 的 per-session 绑定同式)。hitl 直挂 ApprovalConfig 而非 Options:needsApproval 无 brief 语境,且 WrapTools 每轮重建,闭包绑定天然随会话。

## 测试计划

- `hitl/edit_test.go` 五剧本(guarded_test.go 同形态):①EditPass 命中直落零中断;②未命中回落 manual 挂起+批准续行;③EditPass nil 退化 manual;④edit+ArgsForce 仍挂起(优先级);⑤edit 批准不授轮权/任务权+逐次再问(含 Always 会话授权豁免回归)。
- `engine/askautoresolve_test.go`(scriptedModel 全流程+白盒):①到点自动续行——决议=Options[0]、Decider=auto、ask_decision 落流、模型收到答案续跑收口、无 ask_timeout;②闭包返回 nil → 既有取消分支;③Options nil → 零变化(取消);④实时闭包——挂起后才翻转原子开关,到点生效;⑤并发人工作答让位(SetAskDecisionIf CAS,决议不被覆盖);⑥DefaultAskResolve 无选项取空文本。
- `hitl/writegrant_test.go` fail-closed 剧本:①nil 零变化(manual 挂起);②命中放行(manual/plan 两档);③返回 false 照常挂起;④plan 档在 Turn/Task 授权之后咨询(已有授权时不调用缝——避免信息泄露面,也省一次回调);⑤ArgsForce 不被外部授权豁免。
- `go build ./... && go test -race ./...` 全绿。

## 应用侧接线(随后于 einox-work 仓提交,此处仅记录边界)

- compat fromV4Mode:edit → ModeEdit 直通;SetMode 词表增 edit。
- assembly:risktable.go 编译产出 EditPass(文件编辑类)/GuardedRisk(危险命令表)/WriteTools/ArgsForce/ArgsSkip/WriteGrantSource(approval-rules.json 读侧)。
- D15 设置开关真接线:原子值 + bridge 热更 + snooze 集合;D11 approval-rules.json 读写 + 审批卡项目级档 + 设置页清单。
