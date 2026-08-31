# 03 · 能力清单

> 与代码同步的全量清单：引擎面 / 事件流 / 工具族 / harness / 提示词 / 沙箱 / 模型面 / eino 地基面 / 其他。每项能力的启停见 [04-assembly.md](04-assembly.md)——除四项必填外全部可选，nil 即不装配、零行为变化。

## 引擎与会话域（engine / session / hitl / checkpoint）

| 能力 | 说明 |
|---|---|
| 循环引擎 | `engine.Manager`（`NewManager(reg, opt)` 返回 `(m, err)`——装配错误启动期即拒）驱动 ReAct 主循环；`MaxIterations` 默认 100 失控护栏（`EINO_MAX_ITERATIONS` 可覆盖） |
| 会话域 | `session.Registry` + `Store` 接口：会话归属/快照（模型与档位粘住会话）、消息历史、事件流落盘、续聊（Reattach 装载）；`Sweeper` TTL 无活动清理 |
| 运行中输入（steering） | running 时再输入三路径：引导插入（每次模型调用前的 hook 把排队消息追加进输入）、排队落回轮（轮次结束后到达的输入并入下一轮）、显式停止（取消运行上下文，循环在步间退出；已落事件不回滚，checkpoint 保留可续；停止即向历史注入打断注记——模型续聊时知晓“工具可能部分执行/后台进程可能仍在跑”，不假设中断前操作都已成功） |
| HITL 审批 | `hitl.WrapTools` 按模式包装工具面：manual 逐写审批 / plan 计划卡（批准 = 任务期写授权）/ auto 直过；`ApprovalConfig` 定名单与 ArgsForce（参数级强制审批——**任何模式/任务期授权不豁免**）；无决议 fail-closed 一律拒绝 |
| 跨会话记忆交接（写/拉） | `TurnEpilogue` 轮收尾钩子（自然收束触发，载荷与 session_end 同源——摘要+文件变更；应用落 owner 域记忆文件经 AgentsMD 注入即成读写环）+ `recall` 检索工具（见工具族表）；推通道 = AgentsMD 注入缝。三通道设计见 findings/2026-08-29-memory-three-channel-design.md（仓外设计笔记，不随仓分发） |
| 收束质量门（FinalGate） | 三层约束（事前审批/事中 ErrFeed）的收束空位：自然收束后按 `Options.FinalGate`（SessionBrief 闭包——按模式/形态开门）强制验证，失败经 harness_note 门卡 + 反馈消息入史回灌重跑（有界——MaxRetries 负数=缺省 2、0=零回灌首验即报错，codex Guardian 普通/cyber 两档对位），耗尽 error 收束不静默放行；checker panic fail-closed；挂起/中断/错误轮不触发。**判据归应用**（`GateChecker`——build/test 命令或自包对抗审查），基座只持门循环机制 |
| 挂起-续流通道 | `contract.Suspend` 哨兵 + 引擎 Interrupt×Resume：审批卡、结构化提问、计划卡共用同一机制。`Resume` 入口整备：单锁原子查清挂起域 + 翻 running + 挂 runDone——重复/并发第二个 Resume 即拒（明确 error 事件而非脏重放：checkpoint 不随 Resume 消费，迟到放行 = 加载旧检查点重执行）；续流执行期状态可见为 running（FlushQueue/Drain 可寻址） |
| 检查点 | `engine.CheckPointStore`（Get/Set 两方法），中断/取消恢复与审批挂起续流的事实载体；三卡 State 经 `schema.Register` gob 注册（hitl/askuser/plan 属主包 init），属主包各持 round-trip 兼容回归测试（字段重命名红——gob 按字段名编码，静默破坏存量检查点） |
| 常驻上下文预算（ContextBudget） | `Options.ContextBudget`（0 = 缺省关；`EINO_CONTEXT_BUDGET` 可覆盖）：Instruction + 常驻工具面（业务面+进程件+**会话域件**+spawn/recall：名+描述+参数 schema JSON）合计的超限告警线——超限发一张 `harness_note`（Kind: budget，含分类账本与瘦身指引）+ 服务端日志，不阻断运行（大工具面配 toolsearch 就是合法超标）；会话内只发一次（判定扫事件流既有同 Kind note，跨重启天然不重发）。toolsearch 名单内工具不计（只有常驻面计费） |
| 会话分叉（Fork） | `Registry.Fork(owner, sid)`：全量快照分叉——record JSON 往返深隔离（零共享指针）、事件 ID 接续、spill 外置域整目录复制、血缘 `harness_note`（Kind: fork，Detail 含源 sid）；挂起域/任务授权/排队消息不带（分叉体无执行体残留，与 Reattach 降级同裁决）。V1 限定源非 running（spill 复制与源外置写并发无锁覆盖）；内存优先、不在内存则磁盘重建（历史会话分叉主场景）；归属不符/未知 sid/源 running 返回 nil |
| 优雅停机（Drain） | `Registry.Drain(deadline)`：取消全部 running 态会话并有界等执行体收尾（走中断链：终态事件+检查点+中断注记全落），到点未收尾的 SID 如实返回（调用方记日志不阻塞停机）；挂起态无执行体不在列（跨重启 RearmPendingTimer 续表）。应用停机序 = HTTP Shutdown → Drain → store Close（终态落盘依赖 store 存活） |

### 事件流（contract.Event）

事件流是 **einox 自建的契约面**（eino 的流式原语经引擎消化，不外露）：`contract.Event{ID, Event, Data, Ts}`，传输无关（SSE/WebSocket/CLI 编码归应用）；`Run`/`Resume` 的回调实时扇出，同时落会话记录——**回放与 live 同源**（切回/回放时，审批卡/提问卡/计划卡的终态以事件流重建）。落盘节奏：`Record` 实时入会话 Events（内存），persist 在状态迁移点（用户消息入史/挂起/轮末收束/中断）+ **工具边界节流**（每个 tool_result 后补一次 persist——轮内崩溃不丢已完工具轮，频率有界 = 工具调用数）。31 种事件按域分组：

| 域 | 事件 |
|---|---|
| 生成流 | `text_delta` / `thinking_delta` / `usage`（含整形后出站口径估算与“整形节省”注记；`spawn_id` 非空 = 后台子代理面用量上卷〔估算四项为零〕；est_tools 口径 = 名+描述+参数 schema JSON、含会话域件，存量值较早期版本偏大属纠偏） |
| 工具 | `tool_call`（参数摘要 + 行为标记）/ `tool_result`（Digest+Preview、文件变更信封 `+A -D`） |
| 挂起交互 | `approval_request/decision/timeout`（**合并决议卡**：一轮并行写聚合一卡 N 项、逐项决议）/ `ask_user_request/decision/timeout/ignored` / `plan_request/decision/timeout` |
| steering 与通知 | `steer_queued/updated/removed/reordered/injected` / `notify_queued/notify_injected`（后台子代理完成回传）/ `user_message` |
| 过程 | `todo_update` / `harness_note`（系统通知卡，**Kind 取值封闭集**：`offload` 外置 / `compaction` 摘要压缩 / `gate` 质量门回灌 / `failover` 降级链装配失败留痕 / `budget` 常驻面超预算告警 / `fork` 会话分叉血缘——新 Kind 属前端可观察的软契约增长，增改须同步本表）/ `subagent`（子代理过程流，SpawnID 归组，done/failed 终态）/ `model_change` / `transport_retry`（重连在途——前端回卷当前段半截显示） |
| 收束 | `session_end`（摘要 + 文件变更清单）/ `error`（Code：CONFIG / SERVER / TRANSPORT / ABORTED / AUTH / RATE_LIMIT）/ `interrupted`（打断收尾，非故障形态） |

## 工具族（tools/）

工具按装配方分三类：**会话域件**（引擎随会话装配，root 自动圈进会话工作区：todo / 提问 / 计划 / 文件面 / 命令 / 补丁 / repo）、**工作区件**（`office`——需工作区根，由应用在 `Tools` 内构造）、**进程级件**（无会话态，应用经 `ProcessTools` 选择加入：时钟 / 网页抓取）。会话域件可经 `SessionToolsOff` 按族裁剪（todo/ask/plan/fs/cmd/patch；极简装配物理移除执行/写面，repo 族仍由 `RepoMounts` 条件装配，`recall` 由 `Options.Recall` 选择装配——见下行；裁 fs 族即放弃 reduction 外置换指针取回——超长工具结果只剩截断头尾，见 [04-assembly.md](04-assembly.md) 裁剪表）。

| 包 | 工具 | 说明 |
|---|---|---|
| `tools/applypatch` | `apply_patch` | `*** Begin Patch` 格式补丁改文件：多文件增/改/删/改名、四档模糊匹配、多块锚点、事务性（任一失败全部不落盘） |
| `tools/fsutil` | `read_file` / `list_dir` / `search_files` / `delete_file` | 工作区文件面：行号区间读（超宽行截断可放宽）、目录清单、glob+正则内容搜；路径圈进工作区根，穿越显式拒绝 |
| `tools/runcommand` | `run_command` / `task_output` / `task_stop` | 工作区内 shell：超时、输出头尾截断（中间省略）、后台任务制；`IsSafeReadCommand` 白名单供审批豁免 |
| `tools/repo` | `open_repo` / `repo_status` / `repo_diff` / `repo_commit` / `export_patch` | 代码仓 worktree 挂载进工作区 `repos/<短名>/`；任务分支 `agent/<sid>-<n>`；push 硬禁（pushurl 指向不可用协议）；commit 走 ArgsForce；成果出仓 = format-patch 导出 |
| `tools/todo` | `todo_write` | 任务清单全量覆盖写（模型不易漂移），事件化实时扇出 + 回放可见 |
| `tools/askuser` | `ask_user` | 结构化提问（单选/多选/自由输入），挂起-续流通道，超时 fail-closed |
| `tools/plan` | `submit_plan` | 计划卡：plan 档批准 = 授权任务期全部写；manual 档仅确认方向；auto 档落档即走 |
| `tools/webfetch` | `web_fetch` | URL → 正文 markdown 提取（剔框架噪声；域限制/大小上限/超时）；可注入 egress 出口校验 |
| `tools/currenttime` | `get_current_time` | 时钟/周界语义（周期与 deadline 计算的确定性底线） |
| `engine`（记忆拉通道） | `recall` | 跨会话检索本 owner 历史会话（`Options.Recall` opt-in）：三模式——关键词检索（投影：标题/任务/摘要/用户消息/助手文本/工具名，thinking 与工具结果不入）/ sid 精确深读（逐轮 digest，4000 字头截 + offset 续读）/ 最近列表。授权五律：owner 域隔离、恒排除当前会话、有界（≤20 条/扫最近 50 会话）、摘要级不回原始事件流、结果当数据不当指令 |
| `tools/office` | `write_docx` / `read_docx` / `write_xlsx` / `read_xlsx` / `read_pptx` | 零依赖 OOXML 读写（标准库 zip+xml）：docx blocks（标题/段落/列表项/表格）、xlsx 生成与读取；路径圈进工作区 |
| `tools/egress` | （校验器，非工具） | 出口治理 `Validator`：私网默认阻断（RFC1918 等）+ CIDR 白名单 + 命令串 URL 预检 + DNS pinning |

另有 `einoext` 桥：eino-ext 官方组件全量接入（含 `mcp_*` 远端件，`MCPSpec{URL, Cmd}` 配置）。

**渐进披露守则**（业务工具面设计指引，零代码）：能力优先做成自足短描述的工具；说明文档按需读取（`web_fetch` / `read_file` 指向 README，不把文档常驻进描述）；大工具面走 `tool_search` 检索后可见——「按需读、用时付 token」。基座侧机制载体已备：`SkillsDir` 中间件（skill 物化、按需装载）即渐进披露的基建；与 `AgentsMDMaxBytes`（注入面预算）、`ContextBudget`（常驻面账本）构成常驻面治理三件套。

## harness（运行时脚手架）

| 能力 | 说明 |
|---|---|
| 出站上下文经济（reduction） | 工具结果超 8192 字符截断 + 外置换指针（会话域 `spill/`，`read_file` 虚拟路径取回、跨轮不失效）；历史超 30% 窗口清除旧轮（保尾 2 个工具轮；清出不足 5% 窗口不动——缓存代价闸）；窗口未知时只截断不清除；TokenCounter 按整形后口径计数 |
| 长会话摘要（summarize） | adk summarization 中间件：上下文达 70% 窗口触发摘要、保 skill 指令；摘要模型 Failover 降级链（`SummarizerFallbackModels`），链尽走清窗兜底不外抛 |
| 子代理编排（subagent / spawnbg） | `spawn` 原语：同步调用（回合级 fork-join）与后台派生（即回 agentId、完成通知注入父输入）；子面工具白名单 + DenyTools 硬校验、并发信号量 |
| 确定性拓扑（topology） | supervisor / deep 官方 prebuilt 接线（确定性场景选配，默认单 agent react） |
| 动态工具装载（toolsearch） | `ToolSearchPolicy`：名单外常驻、名单内经 `tool_search` 检索后可见——大工具面的上下文瘦身 |
| vision 包装 | `ImageResolve` 注入图片引用解析，工具结果图片在**模型调用边界**升级为携图 user part（tool 角色不收图；模型不支持时明确报错） |
| 工具中间件链（mid） | `Validate`（入参 schema 子集校验——type/enum/数值边界/items/嵌套递归，违规带字段路径转信封回喂；不校验 required——反射 schema 把全部非指针字段标 required，与零值可接受的工具语义普遍不符）、`ErrFeed`（可恢复错误转结果回喂模型自纠——Go error 会终止整轮且模型不可见）、`Guard`（防死循环提醒 + 单工具执行硬上限）、`digest`（审批卡/事件流参数摘要，不倾倒原始 JSON） |
| 幻觉工具兜底 | 模型调用不存在的工具名 → `{"ok":false}` 信封回喂自纠（不终止整轮）：toolsearch 名单内工具 miss 附“先 tool_search 检索”指引；名单外报可用名单 + 归一化拼写建议（唯一命中才提示、绝不代执行）。主面/拓扑子面/spawn 子面三处同挂 |
| 工具 panic 隔离 | einoext 桥单点 recover：包装链任一层 panic 收敛为错误信封回喂（进程不崩、模型可换参自纠；Guard 死循环计数照常防 panic-重试循环） |
| skill 机制 | `SkillsDir` 指向物化目录即挂 skill middleware（agentskills.io 标准发现）；物化归应用 |
| AGENTS.md 注入 | `AgentsMD` 清单按序注入（eino agentsmd 中间件白拿）：transient 不入历史/检查点、@import 递归（深度 5）、字节预算超限即止（跳过留服务端日志，codex tracing::warn 同级——非用户弹窗）、挂 summarization 之后不被压缩。发现逻辑归应用清单（ZCode 双层形态 = 用户级文件先、工作区级文件后收窄覆盖） |

## 提示词（prompts/）

| 出口 | 内容 | 注入时机 |
|---|---|---|
| `prompts.Coding()` | 编码工作模式：编辑纪律（apply_patch 优先/不动不明来源/破坏性命令先问）、验证纪律（改完必须验证、失败读输出定位再改）、apply_patch 格式规范 | 工作区工具面在场时由应用拼进 Instruction（`SessionToolsOff` 裁掉 fs/cmd/patch 族时勿拼） |
| `prompts.Orchestration()` | 子代理编排：何时派发（可并行独立子任务）/如何派发（简报自足）/后台派生纪律（禁止轮询·sleep·查进度·重复在途工作）/结论聚合 | spawn 装配时注入；不装配子代理可忽略 |

## 沙箱（sandbox/）

`run_command` 的执行面沙箱，后端经 `sandbox.Provider` 注入。策略模型、部署前提与平台限制见 [05-sandbox.md](05-sandbox.md)：

| 平台 | 机制 | 构建标签 |
|---|---|---|
| Linux x86_64/arm64 | Landlock（文件访问围栏）+ seccomp 经典 BPF（default allow + deny 清单：ptrace / process_vm_rw / io_uring 恒禁；断网档追加禁 connect 等网络 syscall，AF_UNIX 同禁——防 docker.sock 类逃逸）+ rlimit | `linux && (amd64 \|\| arm64)` |
| Linux 其他架构 | stub（探测即不可用告警） | 其余 linux |
| Windows | restricted token（drop 权限 SID + 受限进程令牌） | `windows` |
| macOS | Seatbelt（sandbox-exec 配置档） | `darwin` |

策略 `sandbox.Policy`（`Options.Sandbox` 为 nil = 不沙箱）：`Mode`（`read-only` / `workspace-write` / `danger-full-access`——字符串即枚举值，构造期 `Validate()` 校验）+ `Network` 开关 + `WritableRoots`（围栏内可写根，如持久缓存目录）+ `Env` 注入（缓存重定向——围栏内 HOME 不可写，不重定向 go build 硬失败）+ `EnvMode` 环境档（缺省 inherit 全继承；`minimal` 白名单——凭据面默认不进围栏，业务所需环境经 `Env` 显式注入）。

缺省后端 `OSProvider` 平台内建——OS 级走 re-exec 哨兵协议，应用 `main` 需挂 `sandbox.RunHelper` 钩子，装配期探测、不可用启动告警；容器等自定义后端经注入位替换，无哨兵依赖。`DockerProvider` 是一次性容器过渡形态（策略翻译进容器参数，`ProtectedReadOnly` 经嵌套 ro bind 可治；daemon 不可达 = 容器隔离失效，按姿态降级裸跑 + 启动告警，详见 [05-sandbox.md](05-sandbox.md)），终局为每会话长驻容器，接口形状不变。

### 隔离边界随部署形态选择

内核沙箱只是防线之一，**边界选在哪一层是部署决策**——沙箱、出口治理、HITL 审批 + 工作区圈限各层独立生效，组合成纵深：

| 部署形态 | 边界选择 |
|---|---|
| 服务器形态（B/S、多用户） | 服务整体容器化 = 第一层粗粒度边界；应用面靠审批矩阵 + 会话工作区圈限；OS 沙箱选配开启 = 第二层纵深（防容器内横向移动与运维配置失误） |
| 终端形态（应用以 CLI 跑在用户本机、单用户） | 无容器边界，命令直接落在宿主 OS——Landlock/seccomp / Seatbelt / restricted token 就是主执行边界（终端编码 agent 的通行形态） |
| 学习 / 研究 | 容器即边界，内层沙箱可省：接受容器内不设防（agent 可达容器内一切），以容器边界护住宿主。注意默认容器**共享内核、是粗粒度边界而非强安全边界**——保持非特权运行、裁剪能力集；需要强隔离再上 gVisor / 微 VM |

与沙箱正交的**用户态出口治理**：`egress.New(allowCIDRs)` 覆盖 web_fetch 前置与 run_command 命令串 URL 预检（内网段白名单即工作面）。

**单进程会话域**（架构边界声明）：会话态在进程内存（`session.Registry`）、Store 契约为文件树形——多副本部署需按 owner 粘性路由到创建会话的进程；库形态单二进制部署下此边界自然成立。

## 模型面（llm/）

| 能力                | 说明                                                                                                                                                                                                                                                                                                                                                                                                 |
| ----------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 模型工厂              | `llm.NewChatModel(ctx, provider, model, effort)`：kind=anthropic → Anthropic Messages 组件，kind=openai → Chat Completions 组件                                                                                                                                                                                                                                                                          |
| 厂家预设              | `BuiltinProviders` 内置 DeepSeek 两端点（openai 协议主推 + anthropic 协议）与智谱 GLM 端点（openai 协议 Chat Completion：GLM-5.3 纯文本 + GLM-5.3-Flash 多模态）；`Resolve` 原样 / `ResolveMerged` by-ID 参数补全（用户显式值优先，密钥/启用权不受内置影响）                                                                                                                                                                                                |
| 网络容错              | ① 流式空闲哨兵（两 chunk 间静默默认 120s，非整请求超时——长思考流不误杀）+ Generate 总超时 300s；② 有界重连（默认 3 次，引擎在模型调用边界内重启，半截段不入史）；③ 错误分类（`Classify` 单一权威：429/5xx/网络类可重试，401/403/402/400/422 致命立即停机，未知保守不重试；openai 协议错误体业务码细化——智谱 429 族欠费/套餐类码致命不空转重试）；④ 主模型 Failover 降级链（`FallbackModels` 复合键清单——重试耗尽按序换备模型、每档各享完整重连预算，切换发 model_change 事件，致命类不降级）。env：`EINO_LLM_IDLE_TIMEOUT` / `EINO_LLM_GENERATE_TIMEOUT` / `EINO_LLM_RETRIES` |
| 采样参数              | `ModelSpec.Temperature/TopP`（nil = 不发字段走端点默认——推理端点常拒绝显式 temperature；随会话模型快照粘住，会话内不变即前缀缓存友好）                                                                                                                                                                                                                                                                                                        |
| 能力门控（NoToolCalls） | `ModelSpec.NoToolCalls` 明示模型不支持函数调用（人工维护元数据；能力是模型属性故在 ModelSpec——同 provider 各模型可不同）：置位且工具面非空（含会话域件/spawn）→ assemble 期 CONFIG 错误，不等首轮运行期报端点方言各异的错                                                                                                                                                                                                                                                   |
| thinking 双协议映射    | effort 四档（off/low/high/max，关档 2026-08-31 回归——能力归机制，模型能否真关由端点定）：anthropic 协议 = 关档不发思考块、其余档预算分档（BudgetTokens），openai 协议 = 思考方言（`dialect=deepseek` / `dialect=glm` 关档 thinking disabled、开档发扩展字段+档位直传 / `dialect=effort` 通用 reasoning_effort〔off→none、max→high 对齐 OpenAI 词表〕/ 空方言零思考字段）                                                                                                                                                                                                                     |
| 出站整形              | `NewHistoryShapeModel`：在途带 tool_calls 轮的思维链保留、其余剥离（DeepSeek 等端点的协议要求）；会话存储保真不动                                                                                                                                                                                                                                                                                                                     |
| `NormalizeEffort` | 档位归一唯一权威（四档原样；旧值 on/max→max、off 恢复关档本义，未知→默认 low）                                                                                                                                                                                                                                                                                                                                                             |

## eino 地基面

业务代码不直接依赖 eino，下列能力全部经 einox 的装配面透出。开发业务 agent 时需要知道底层有什么、从哪层来、经哪个面用：

| 能力 | 来源 | 经 einox 的消费面 |
|---|---|---|
| 消息 schema 与模型接口 | eino 核心 | 引擎内部；业务只见 `contract` 的事件与结果 |
| 模型协议组件（Anthropic Messages / OpenAI Chat Completions） | eino-ext `model/claude` `model/openai` | `llm.NewChatModel` 按 `ProviderSpec.Kind` 选型 |
| adk agent 内核（ReAct 循环、Interrupt×Resume、checkpoint 接口、中间件位、summarization） | eino adk | 引擎装配——挂起-续流（审批/提问/计划卡）、检查点、长会话摘要以其为底座 |
| 模型重试协议 `ModelRetryConfig` | eino adk | 网络容错第②层的挂点（`llm.Classify` 作分类回调，白拿既有重试循环） |
| 编排原语与预置拓扑（compose / supervisor / deep） | eino compose+adk | `Topology`（TopologyConfig）确定性场景选配 |
| 官方工具生态：搜索（bing / google / duckduckgo / searxng）、浏览器、命令行、HTTP 请求、顺序思考、维基、MCP 远端件 | eino-ext `components/tool/*` | `einoext.NewExtTools(root, MCPSpec)` 桥接为 `contract.Tool` |

## 其他

| 能力 | 说明 |
|---|---|
| `llmtest` | 测试假模型：注入 `Options.NewModel` 即可零真实端点跑引擎/工具循环测试，支持注错（`Turn.Err`） |
| `workspace` | 会话工作区布局（用户域 `workspaces/<sid>`；repos/ 挂载持久、其余任务收尾清理） |
| `contract` | 最小契约面：`Tool` / `ToolInfo` / `Schema` / 事件 / `Suspend` / 行为标记（BehaviorRead/Write/Exec）——业务只见此包 |
