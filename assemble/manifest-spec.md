# 能力装配清单格式(manifest-spec)

> `einox.agent.yaml` 是人选能力的唯一契约——终端逐项确认、web 勾选、配置选配三种形式殊途同归,最终都产出这份清单;AI 编程代理读清单 + [rules.md](rules.md) + [patterns/](patterns/) 组装 agent 内核。清单落业务仓根。

## 格式总则

- **YAML**:人可手写、AI 可解析、可 diff 可版本控制。
- **版本**:`apiVersion: einox/v1` 起版;字段**只增不改名不删除**(对齐事件词表冻结纪律——旧清单在新版知识层下仍可消费)。
- **preset 打底 + 逐项覆盖**:`preset` 引 [recipes/](recipes/) 一篇作为基线;清单内显式项覆盖基线值。
- **三态**:字段不出现 = 用 preset 基线;`false` = 显式排除(留 diff 痕迹);参数对象 = 启用并定参。
- **清单是能力选择,不 1:1 映射 `engine.Options` 字段**——按业务语义组织;能力到 Options 装配代码的翻译归 [patterns/](patterns/)(即 AI 的活)。清单格式稳定,Options 演进不破坏清单。
- **必填与业务内容边界**:四必填中 `Instruction` 与 `Tools` 是业务内容——清单只承载 `name`,内容由 AI 生成时与用户协作产出(业务职责段)。`Fork`/`ForkAt`/`Side`/`Drain` 是运行时操作面不是装配开关,不进清单(recipes 按场景提示暴露)。

## 九域字段规范

| 域 | 字段 | 类型 | 说明 |
|---|---|---|---|
| ① model | `providers` | list | 必填。供应商清单:`BuiltinProviders` 名(deepseek/glm)或自定义 spec(baseurl+key+kind) |
| | `fallback-chain` | list | 主模型 Failover 降级链(provider/model 复合键) |
| | `vision` | bool | 图片引用解析 + 模型视觉位 |
| ② engine | `checkpoint` | string | 必填。检查点存储形态(样板 = `checkpoint.FileCheckPointStore`) |
| | `workspace-root` | string | 必填。会话工作区根路径 |
| | `workspace-keep` | list | 任务收尾保留的工作区子目录(顶层名) |
| | `workspace-protect` | list | 工作区写保护区(顶层名) |
| | `context-budget` | int | 常驻上下文预算告警线(0 = 关;推荐 8192) |
| ③ hitl | `mode` | string | manual / plan / auto(应用建议的会话缺省档) |
| | `args-force` | list | 参数级强制审批的工具名(名单逻辑归业务) |
| | `router` | bool | 审批路由「问谁」(T6) |
| | `decision-guard` | bool | 决议者一致性校验(越权点批防护,T6) |
| ④ tools | `session-tools-off` | list | 裁剪的会话域族:todo/ask/plan/fs/cmd/patch |
| | `process-tools` | list | 进程级件:currenttime/webfetch |
| | `office` | bool | 工作区件:docx/xlsx/pptx 读写(经 `Tools` 装配) |
| | `einoext` | list | eino-ext 生态件(含 MCP:URL/Cmd 条目) |
| ⑤ harness | `subagents` | bool | spawn 子代理编排(白名单归业务内容) |
| | `topology` | string | react / supervisor / deep |
| | `toolsearch` | list | 动态装载名单(名单内经 tool_search 检索后可见) |
| | `skills` | bool | skill 物化目录机制(物化归应用) |
| | `agentsmd` | list | AGENTS.md 注入清单(跨会话记忆推通道同走此缝) |
| | `recall` | bool | 跨会话检索工具(前置:fs 族在场) |
| ⑥ quality | `final-gate` | string/bool | 收束质量门:build-test 或业务判据标识 |
| | `summarizer-fallback` | list | 摘要模型降级链 |
| ⑦ security | `sandbox.mode` | string | read-only / workspace-write / danger-full-access |
| | `sandbox.network` | bool | 围栏内网络开关 |
| | `sandbox.writable-roots` | list | 围栏内额外可写根(缓存目录) |
| | `sandbox.env-mode` | string | inherit / minimal |
| | `egress` | list | 出口治理 CIDR 白名单(缺省私网阻断) |
| ⑧ channels | `channels` | list | feishu / voice / 自定义渠道标识 |
| ⑨ ui | `ui.web` | bool | 官方 web 回放+交互面 |
| | `ui.tui` | bool | 官方终端回放浏览器 |

## 完整样例(coding 场景)

```yaml
apiVersion: einox/v1
name: my-coding-agent
preset: coding                        # recipes/coding.md 打底,以下显式项覆盖
model:                                # ① 模型面
  providers: [deepseek, glm]          #   必填:BuiltinProviders 名或自定义 spec
  fallback-chain: false               #   主模型 Failover 降级链
  vision: false                       #   ImageResolve + 模型视觉位
engine:                               # ② 引擎域
  checkpoint: file                    #   必填:检查点存储形态(样板 = checkpoint.FileCheckPointStore)
  workspace-root: ./data/workspaces   #   必填:会话工作区根
  workspace-keep: []                  #   持久子区豁免声明
  workspace-protect: []               #   写保护区声明
  context-budget: 8192                #   常驻上下文预算告警线
hitl:                                 # ③ 审批域
  mode: manual                        #   manual|plan|auto
  args-force: []                      #   参数级强制审批(业务内容,留占位)
  router: false                       #   ApprovalRouter(T6 问谁)
  decision-guard: false               #   DecisionGuard(T6 越权校验)
tools:                                # ④ 工具面
  session-tools-off: []               #   裁剪族:todo/ask/plan/fs/cmd/patch
  process-tools: [currenttime]        #   进程级件:currenttime/webfetch
  office: false                       #   工作区件:docx/xlsx/pptx 读写
  einoext: []                         #   eino-ext 生态件(含 MCP)
harness:                              # ⑤ harness 域
  subagents: false                    #   spawn 编排(白名单归业务)
  topology: react                     #   react|supervisor|deep
  toolsearch: []                      #   动态装载名单
  skills: false                       #   SkillsDir(物化归应用)
  agentsmd: []                        #   AGENTS.md 注入清单
  recall: false                       #   跨会话记忆检索工具
quality:                              # ⑥ 质量域
  final-gate: false                   #   收束质量门
  summarizer-fallback: false          #   摘要模型降级链
security:                             # ⑦ 安全面
  sandbox: {mode: workspace-write, network: false, writable-roots: [], env-mode: inherit}
  egress: []                          #   出口治理 CIDR 白名单
channels: []                          # ⑧ 渠道
ui: {web: false, tui: false}          # ⑨ 界面
```

## 示例清单

最短可用清单(minimal 场景,可直接抄):

```yaml
apiVersion: einox/v1
name: my-minimal-agent
preset: minimal
model:
  providers: [deepseek]
engine:
  checkpoint: file
  workspace-root: ./data/workspaces
tools:
  session-tools-off: [fs, cmd, patch]
```

四场景完整示例见 [recipes/](recipes/)(minimal / coding / support / data-analysis,各篇内含直出清单与装配要点)。
