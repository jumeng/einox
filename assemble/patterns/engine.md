# 引擎域样板(engine)

> 清单域 ②:`workspace-root` / `checkpoint`(必填,骨架已含)/ `workspace-keep` / `workspace-protect` / `context-budget`。叠加位:[00-skeleton](00-skeleton.md) 的 `Options` 同名字段。

## workspace-keep / workspace-protect

```go
// WorkspaceKeep:任务收尾清理保留的工作区子目录(顶层目录名——挂载区、
// 参考资料区等由装配层声明,基座不预设名字;nil/空 = 无持久区全清)
WorkspaceKeep: []string{"mounts", "refs"},

// WorkspaceProtect:工作区写保护区(顶层目录名;注入 fsutil/applypatch 写面
// ——delete_file 与补丁目标〔含 Move to 改名目标〕命中即整单拒绝、读面不受影响)
WorkspaceProtect: []string{"protected"},
```

注意:Keep 持久子区随会话删除/过期/孤儿清扫**整清**(Registry.Delete/Sweep 整目录移除,不看此栏);Protect 不管 `run_command`(命令内容不可静态可靠解析,写的硬约束归 Sandbox 档位)。

## context-budget(常驻上下文预算)

```go
ContextBudget: 8192, // 0 = 缺省关;EINO_CONTEXT_BUDGET 可覆盖
```

口径 = Instruction + 常驻工具面(名+描述+参数 schema JSON)合计的 `estTokens` 启发式。超线动作 = `harness_note`(Kind: budget,含分类账本与瘦身指引)+ 服务端日志,**不阻断**;会话内只发一次。大工具面配 toolsearch 就是合法超标([tools.md](tools.md) toolsearch 节)。

## agentsmd(AGENTS.md 注入 / 记忆推通道)

```go
AgentsMD: func(sess engine.SessionBrief) []string {
    // 按序注入;发现逻辑归应用(如用户级文件先、工作区级文件后收窄覆盖)
    return []string{
        filepath.Join(dataDir, "owners", sess.Owner, "memory.md"), // owner 域记忆(跨会话)
        "AGENTS.md", // 工作区级
    }
},
AgentsMDMaxBytes: 8192, // 0 = 缺省 32KiB;按序装载超限即跳过余下文件
```

特性:transient 不入历史/检查点、@import 递归(深度 5)、挂 summarization 之后不被压缩。

**记忆读写环**(跨会话记忆,可选增强):`AgentsMD` 是推通道,配 `TurnEpilogue` 写通道即成环——

```go
TurnEpilogue: func(sum engine.TurnEndSummary) { // 自然收束每轮触发;同步调用应快速返回
    // 时序:epilogue 先于异步标题生成执行——首轮 sum.Title 为空,回退 Task
    title := sum.Title
    if title == "" {
        title = sum.Task
    }
    path := filepath.Join(dataDir, "owners", sum.Owner, "memory.md")
    os.MkdirAll(filepath.Dir(path), 0o755)
    f, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
    defer f.Close()
    fmt.Fprintf(f, "\n## %s\n- 任务: %s\n- 摘要: %s\n", title, sum.Task, sum.Summary)
},
```

`TurnEndSummary{Owner, SID, Title, Task, Summary, Files, EndedAt}`——与 `session_end` 事件同源。重提取(LLM 蒸馏/外部写)归应用异步;panic 引擎兜底不影响终态。einox 的 session_end 是轮级,应用自行去重/节流。

## skills(skill 物化目录)

```go
SkillsDir: func(sess engine.SessionBrief) string {
    return filepath.Join(dataDir, "owners", sess.Owner, "skills") // 物化归应用
},
```

指向物化目录即挂 skill middleware(agentskills.io 标准发现)。与 `Tools` 同契约:每轮 assemble 求值、并发安全(闭包应快速返回且无共享可变态)。

## agentsmd 验证锚点

- 注入形态:AGENTS.md 内容作为 **transient user 消息**进模型输入(不入历史/检查点)——断言面在模型输入侧(llmtest 经 `Model.Inputs()` 直查),不在事件流。
- 记忆环场景**建议在 AgentsMD 闭包内保证记忆文件存在**(空文件占位)——否则首会话「记忆从零增长」时,每次模型调用上游日志打一条 `warning: file not found, skipping`(einox 有意的留痕面:清单文件缺失可能是装配路径写错,静音会失去排障线索;应用侧占位即两全——噪音消失、排障能力不损失):

```go
AgentsMD: func(sess engine.SessionBrief) []string {
    mem := filepath.Join(dataDir, "owners", sess.Owner, "memory.md")
    if _, err := os.Stat(mem); os.IsNotExist(err) { // 幂等占位,消 not found 留痕
        os.MkdirAll(filepath.Dir(mem), 0o755)
        os.WriteFile(mem, nil, 0o644)
    }
    return []string{mem}
},
```

## 验证

- `go build ./...` 过。
- Keep/Protect 顶层名非法(`CheckTopDirs`:非顶层路径)→ NewManager 即拒。
