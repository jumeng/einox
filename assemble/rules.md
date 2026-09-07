# 装配规则矩阵

> 本文件是 [docs/03-capabilities.md](../docs/03-capabilities.md) 与 [docs/04-assembly.md](../docs/04-assembly.md) 装配面的结构化重述——必选/依赖/互斥三律 + 档位语义 + 选型域序,**不新增语义**。消费方:AI 编程代理(装配期逐项自查依据)+ 人(选型参考)。能力面变更时本文件与 docs 同批同步;发现漂移以 docs 为准回改本文件。
>
> 注:skill 复制出 einox 仓后 docs/ 不随行——`docs/03` `docs/04` 均指 einox 仓内路径,自主装配(不装 skill)场景直接读仓内文件;装 skill 场景下逐项详情以内嵌说明为准。

## 必选基线律

| 规则 | 锚 |
|---|---|
| 四必填:`Providers` / `Instruction` / `CheckPoints` / `WorkspaceRoot`——缺一 `NewManager` 即拒(不拖到首会话) | engine/manager.go `NewManager` |
| fs/cmd/patch 三族**任一在场**(未全裁)→ `Instruction` 必拼 `prompts.Coding()`(编码工作模式:编辑纪律/验证纪律/补丁格式——已裁族相关段落属可接受噪音);三族全裁 → 勿拼 | prompts 包 |
| subagents 启用 → `Instruction` 必拼 `prompts.Orchestration()`(派发纪律/后台派生纪律) | prompts 包 |
| Instruction 拼装序:业务职责段(应用写)+ `prompts.Coding()`(工具面在场时)+ `prompts.Orchestration()`(spawn 装配时)+ 会话配置段(mode 语义) | docs/04 最小装配 |

## 依赖律

| 能力 | 前置 | 违反后果 |
|---|---|---|
| `recall`(跨会话检索) | fs 族在场(未裁 `FamilyFS`) | 外置换指针经 `read_file` 虚拟路径取回;fs 已裁则超长工具结果只剩截断头尾 |
| `final-gate: build-test` | cmd 族在场(未裁 `FamilyCmd`) | 质量门判据无命令可跑,门形同虚设 |
| `office` | workspace-root(必填恒在;office 是工作区件,经 `Tools` 装配圈进工作区) | — |
| `vision` | `ImageResolve` 实现 + 模型 `ModelSpec.Input` 含 image | nil resolve 下含图请求即错误面 |
| `channels: feishu` | import `channels/feishu`(SDK 依赖进构建) | — |
| `toolsearch` 非空 | 大工具面场景(常驻面瘦身动机) | 无实际损害;`ContextBudget` 超线发告警卡 |
| `agentsmd` 作记忆推通道 | 配 `TurnEpilogue` 落 owner 域记忆文件 | 只读不写,记忆不增长(读写环断) |
| `subagents` 结构化回传 | `SubAgentsConfig.Output.Schema` 根形必须 object | NewManager 构造期拒收 |

## 互斥律

| 冲突 | 判定 |
|---|---|
| `session-tools-off` 含 `fs` × `recall: true` | **拒**——AI 装配期发现即回用户改清单 |
| 模型 `NoToolCalls` × 工具面非空(含会话域件/spawn) | assemble 期 CONFIG 错(引擎已 fail-fast;清单期预检提示,避免生成必死装配) |
| `sandbox: read-only` × applypatch/office 写面在场 | **拒**——写工具在只读围栏内无处置写 |
| `hitl: auto` × `run_command` 高危命令面 | 提示风险不硬拒(业务自担;可经 `ArgsForce` 参数级强制审批兜底) |

## 档位语义表

### hitl 模式(manual / plan / auto)

| 档 | 语义 | 取舍 |
|---|---|---|
| `manual` | 写工具逐次挂起审批 | 最稳;交互成本最高 |
| `plan` | 计划卡批准 = 任务期写授权(一次批全轮) | 平衡;信任计划质量 |
| `auto` | 直过零挂起 | 面向可信/低危场景;高危命令需 `args-force` 兜底 |

模式是**会话级**参数(`Registry.Create(owner, task, mode, prefs)`),清单 `hitl.mode` 即应用建议的缺省档。任何模式下 `ArgsForce` 参数级强制审批不豁免。

### sandbox(Mode / Network / EnvMode / WritableRoots / Env)

| 项 | 语义 |
|---|---|
| `mode: read-only` | 只读围栏——适合纯分析/问答形态 |
| `mode: workspace-write` | 工作区可写 + `WritableRoots` 额外可写根(缓存目录)——编码形态主力档 |
| `mode: danger-full-access` | 全开——明确知情才选 |
| `network` | 断网档下依赖安装必死且模型无法自纠——内网/需拉依赖形态须配 on |
| `env-mode: minimal` | 环境白名单,凭据面默认不进围栏;缺省 inherit 全继承 |
| `env`(K=V 清单) | 缓存重定向是保命件:围栏内 HOME 不可写,不重定向 `GOCACHE`/`GOMODCACHE` 则 go build 硬失败 |

OS 后端走 re-exec 哨兵协议——**应用 main 顶部必挂 `sandbox.RunHelper(os.Args)`**,漏挂 = 沙箱不可用仅启动告警。容器后端(`DockerProvider`)注入时无哨兵依赖。

### egress(CIDR 白名单)

`egress.New(allowCIDRs)`——私网默认阻断(RFC1918 等)+ 白名单即工作面;`web_fetch` 前置与 `run_command` 命令串预检共用同一校验器。白名单是否强制由应用装配层决定。

### topology(react / supervisor / deep)

`react` 缺省单 agent;`supervisor`/`deep` 确定性场景选配(子面经 `SubAgentSpec` 定义;红线表对拓扑内子 agent 全量生效)。

### ContextBudget

常驻上下文预算告警线(0 = 缺省关;推荐 8192)——超线一张 `harness_note`(Kind: budget,含账本与瘦身指引)+ 日志,不阻断;大工具面配 toolsearch 就是合法超标。

## 选型域序

三种交互形式(终端逐项确认 / web 勾选 / 配置文件选配)共享同一流程定义:

```
① 模型面 model      → ② 引擎域 engine   → ③ 审批域 hitl
④ 工具面 tools       → ⑤ harness 域      → ⑥ 质量域 quality
⑦ 安全面 security    → ⑧ 渠道 channels   → ⑨ 界面 ui
```

逐项三选一:**加入**(定参数)/ **跳过**(用 preset 基线)/ **排除**(`false` 落清单,留 diff 痕迹)。

每项确认时的呈现格式:能力名 + 一句话说明 + 对业务的影响;用户询问详情时展开 docs/03 对应行(自主装配场景读 einox 仓内 docs/03)。
