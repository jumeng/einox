# 02 · 能力清单

> 与代码同步的全量清单：引擎面 / 事件流 / 工具族 / harness / 提示词 / 沙箱 / 模型面 / eino 地基面 / 其他。
> 每项能力的启停方式见 [03-assembly.md](03-assembly.md)——除四项必填外全部可选，nil 即不装配、零行为变化。

## 引擎与会话域（engine / session / hitl / checkpoint）

| 能力 | 说明 |
|---|---|
| 循环引擎 | `engine.Manager`（`NewManager(reg, opt)`）驱动 ReAct 主循环；`MaxIterations` 默认 100 失控护栏（`EINO_MAX_ITERATIONS` 可覆盖） |
| 会话域 | `session.Registry` + `Store` 接口：会话归属/快照（模型与档位粘住会话）、消息历史、事件流落盘、续聊（Reattach 装载）；`Sweeper` TTL 无活动清理 |
| 运行中输入（steering） | running 时再输入三路径：引导插入（模型调用前 hook 注入）、排队落回轮、显式停止（安全点取消 + 超时升级强杀；已落事件不回滚，checkpoint 保留可续） |
| HITL 审批 | `hitl.WrapTools` 按模式包装工具面：manual 逐写审批 / plan 计划卡（批准 = 任务期写授权）/ auto 直过；`ApprovalConfig` 定名单与 ArgsForce（参数级强制审批——**任何模式/任务期授权不豁免**）；无决议 fail-closed 一律拒绝 |
| 挂起-续流通道 | `contract.Suspend` 哨兵 + 引擎 Interrupt×Resume：审批卡、结构化提问、计划卡共用同一机制 |
| 检查点 | `engine.CheckPointStore`（Get/Set 两方法），中断/取消恢复与审批挂起续流的事实载体 |

### 事件流（contract.Event——会话记录与渲染的真源）

事件流是 **einox 自建的契约面**（eino 的流式原语经引擎消化，不外露——业务 0 import eino 的一部分）：`contract.Event{ID, Event, Data, Ts}`，传输无关（SSE/WebSocket/CLI 编码归应用）；`Run`/`Resume` 的回调实时扇出，同时落会话记录——**回放与 live 同源**（切回/回放时，审批卡/提问卡/计划卡的终态以事件流重建）。约 30 种事件按域分组：

| 域 | 事件 |
|---|---|
| 生成流 | `text_delta` / `thinking_delta` / `usage`（含整形后出站口径估算与「整形节省」注记） |
| 工具 | `tool_call`（参数摘要 + 行为标记）/ `tool_result`（Digest+Preview、文件变更信封 `+A -D`） |
| 挂起交互 | `approval_request/decision/timeout`（**合并决议卡**：一轮并行写聚合一卡 N 项、逐项决议）/ `ask_user_request/decision/timeout/ignored` / `plan_request/decision/timeout` |
| steering 与通知 | `steer_queued/updated/removed/reordered/injected` / `notify_queued/notify_injected`（后台子代理完成回传）/ `user_message` |
| 过程 | `todo_update` / `harness_note`（reduction 外置、summarization 压缩的系统通知卡）/ `subagent`（子代理过程流，SpawnID 归组，done/failed 终态）/ `model_change` / `transport_retry`（重连在途——前端回卷当前段半截显示） |
| 收束 | `session_end`（摘要 + 文件变更清单）/ `error`（Code：CONFIG / SERVER / TRANSPORT / ABORTED / AUTH / RATE_LIMIT）/ `interrupted`（打断收尾，非故障形态） |

## 工具族（tools/）

工具分两类装配：**会话域件**（root 圈进会话工作区，引擎随会话装配：todo / 提问 / 计划 / 文件面 / 命令 / 补丁 / repo）与**进程级件**（应用经 `ProcessTools` 选择加入：时钟 / 网页抓取）。

| 包 | 工具 | 说明 |
|---|---|---|
| `tools/applypatch` | `apply_patch` | codex `*** Begin Patch` 格式补丁改文件：多文件增/改/删/改名、四档模糊匹配、多块锚点、事务性（任一失败全部不落盘） |
| `tools/fsutil` | `read_file` / `list_dir` / `search_files` / `delete_file` | 工作区文件面：行号区间读（超宽行截断可放宽）、目录清单、glob+正则内容搜；路径圈进工作区根，穿越显式拒绝 |
| `tools/runcommand` | `run_command` / `task_output` / `task_stop` | 工作区内 shell：超时、输出头尾截断（中间省略）、后台任务制；`IsSafeReadCommand` 白名单供审批豁免 |
| `tools/repo` | `open_repo` / `repo_status` / `repo_diff` / `repo_commit` / `export_patch` | 代码仓 worktree 挂载进工作区 `repos/<短名>/`；任务分支 `agent/<sid>-<n>`；push 硬禁（pushurl 指向不可用协议）；commit 走 ArgsForce；成果出仓 = format-patch 导出 |
| `tools/todo` | `todo_write` | 任务清单全量覆盖写（模型不易漂移），事件化实时扇出 + 回放可见 |
| `tools/askuser` | `ask_user` | 结构化提问（单选/多选/自由输入），挂起-续流通道，超时 fail-closed |
| `tools/plan` | `submit_plan` | 计划卡：plan 档批准 = 授权任务期全部写；manual 档仅确认方向；auto 档落档即走 |
| `tools/webfetch` | `web_fetch` | URL → 正文 markdown 提取（剔框架噪声；域限制/大小上限/超时）；可注入 egress 出口校验 |
| `tools/currenttime` | `get_current_time` | 时钟/周界语义（周期与 deadline 计算的确定性底线） |
| `tools/office` | docx / xlsx 读写 | 极小合法 docx（blocks：标题/段落/列表项/表格）与 xlsx 生成/读取 |
| `tools/egress` | （校验器，非工具） | 出口治理 `Validator`：私网默认阻断（RFC1918 等）+ CIDR 白名单 + 命令串 URL 预检 + DNS pinning |

另有 `einoext` 桥：eino-ext 官方组件全量接入（含 `mcp_*` 远端件，`MCPSpec{URL, Cmd}` 配置）。

## harness（运行时脚手架）

| 能力 | 说明 |
|---|---|
| 出站上下文经济（reduction） | 工具结果超 8192 截断 + 外置换指针（会话域 `spill/`，`read_file` 虚拟路径取回、跨轮不失效）；历史超 30% 窗口清除旧轮（保尾 2 工具轮）；TokenCounter 按整形后口径计数 |
| 长会话摘要（summarize） | adk summarization 中间件：ContextTokens 阈值自动摘要、保 skill 指令；摘要模型 Failover 降级链（`SummarizerFallbackModels`） |
| 子代理编排（subagent / spawnbg） | `spawn` 原语：同步调用（回合级 fork-join）与后台派生（即回 agentId、完成通知注入父输入）；子面工具白名单 + DenyTools 硬校验、并发信号量 |
| 确定性拓扑（topology) | supervisor / deep 官方 prebuilt 接线（确定性场景选配，默认单 agent react） |
| 动态工具装载（toolsearch) | `ToolSearchPolicy`：名单外常驻、名单内经 `tool_search` 检索后可见——大工具面的上下文瘦身 |
| vision 包装 | `ImageResolve` 注入图片引用解析，工具结果图片在**模型调用边界**升级为携图 user part（tool 角色不收图；模型不支持时明确报错） |
| 工具中间件链（mid) | `ErrFeed`（可恢复错误转结果回喂模型自纠——Go error 会终止整轮且模型不可见）、`Guard`（防死循环提醒 + 单工具执行硬上限）、`digest`（审批卡/事件流参数摘要，不倾倒原始 JSON） |
| skill 机制 | `SkillsDir` 指向物化目录即挂 skill middleware（agentskills.io 标准发现）；物化归应用 |

## 提示词（prompts/）

| 出口 | 内容 | 注入时机 |
|---|---|---|
| `prompts.Coding()` | 编码工作模式：编辑纪律（apply_patch 优先/不动不明来源/破坏性命令先问）、验证纪律（改完必须验证、失败读输出定位再改）、apply_patch 格式规范 | 工作区工具面在场时由应用拼进 Instruction |
| `prompts.Orchestration()` | 子代理编排：何时派发（可并行独立子任务）/如何派发（简报自足）/后台派生纪律（禁止轮询·sleep·查进度·重复在途工作）/结论聚合 | spawn 装配时注入；不装配子代理可忽略 |

## 沙箱（sandbox/）

`run_command` 执行面沙箱（re-exec 哨兵协议：应用 main 挂 `RunHelper` 钩子，装配期 `sandbox.Probe()` 探测可用性）：

| 平台 | 机制 | 构建标签 |
|---|---|---|
| Linux x86_64/arm64 | Landlock（文件访问围栏）+ seccomp 经典 BPF（syscall 白名单，connect 全禁——防 docker.sock 类逃逸）+ rlimit | `linux && (amd64 \|\| arm64)` |
| Linux 其他架构 | stub（探测即不可用告警） | 其余 linux |
| Windows | restricted token（drop 权限 SID + 受限进程令牌） | `windows` |
| macOS | Seatbelt（sandbox-exec 配置档） | `darwin` |

策略 `sandbox.Policy`：`Mode`（readonly / workspace-write / danger-full-access；nil = 不沙箱）+ `Network` 开关 + `WritableRoots`（围栏内可写根，如持久缓存目录）+ `Env` 注入（缓存重定向——围栏内 HOME 不可写，不重定向 go build 硬失败）。

### 隔离边界随部署形态选择（纵深防御）

内核沙箱只是防线之一，**边界选在哪一层是部署决策**——沙箱、出口治理、HITL 审批 + 工作区圈限各层独立生效，组合成纵深：

| 部署形态 | 边界选择 |
|---|---|
| 服务器形态（B/S、多用户） | 服务整体容器化 = 第一层粗粒度边界；应用面靠审批矩阵 + 会话工作区圈限；OS 沙箱选配开启 = 第二层纵深（防容器内横向移动与运维配置失误） |
| 终端形态（应用以 CLI 跑在用户本机、单用户） | 无容器边界，命令直接落在宿主 OS——Landlock/seccomp / Seatbelt / restricted token 就是主执行边界（终端编码 agent 的通行形态） |
| 学习 / 研究 | 容器即边界，内层沙箱可省：接受容器内不设防（agent 可达容器内一切），以容器边界护住宿主。注意默认容器**共享内核、是粗粒度边界而非强安全边界**——保持非特权运行、裁剪能力集；需要强隔离再上 gVisor / 微 VM |

与沙箱正交的**用户态出口治理**：`egress.New(allowCIDRs)` 覆盖 web_fetch 前置与 run_command 命令串 URL 预检（内网段白名单即工作面）。

## 模型面（llm/）

| 能力 | 说明 |
|---|---|
| 模型工厂 | `llm.NewChatModel(ctx, provider, model, effort)`：kind=anthropic → Anthropic Messages 组件，kind=openai → Chat Completions 组件 |
| 厂家预设 | `BuiltinProviders` 内置 DeepSeek 两端点（openai 协议主推 + anthropic 协议）；`Resolve` 原样 / `ResolveMerged` by-ID 参数补全（用户显式值优先，密钥/启用权不受内置影响） |
| 网络容错 | ① 流式空闲哨兵（两 chunk 间静默默认 120s，非整请求超时——长思考流不误杀）+ Generate 总超时 300s；② 有界重连（默认 3 次，引擎在模型调用边界内重启，半截段不入史）；③ 错误分类（`Classify` 单一权威：429/5xx/网络类可重试，401/403/402/400/422 致命立即停机，未知保守不重试）。env：`EINO_LLM_IDLE_TIMEOUT` / `EINO_LLM_GENERATE_TIMEOUT` / `EINO_LLM_RETRIES` |
| thinking 双协议映射 | effort 三档（low/high/max）：anthropic 协议 = 预算分档（BudgetTokens），openai 协议 = 思考方言（`dialect=deepseek` 发扩展字段 / `dialect=effort` 通用 reasoning_effort / 空方言零思考字段） |
| 出站整形 | `NewHistoryShapeModel`：在途带 tool_calls 轮的思维链保留、其余剥离（DeepSeek 等端点的协议要求）；会话存储保真不动 |
| `NormalizeEffort` | 档位归一唯一权威（旧值 on/max→max、off→low，未知→low） |

## eino 地基面（经 einox 透出，业务 0 import eino）

业务代码不直接依赖 eino——下列能力全部经 einox 的装配面透出。列在这里是因为开发业务 agent 时需要知道底层有什么、从哪层来、经哪个面用：

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
