# harness 域样板(harness)

> 清单域 ⑤:`subagents` / `topology` / `toolsearch` / `recall`。(skills / agentsmd 见 [engine.md](engine.md)。)

## subagents(spawn 子代理编排)

```go
SubAgents: &engine.SubAgentsConfig{
    Tools:     []string{"read_file", "search_files", "web_fetch"}, // 子面白名单
    DenyTools: []string{"run_command"},                            // 硬拒名单(校验在子面执行期)
    // Output: &engine.SpawnOutput{Schema: …}, // 可选:结构化回传(根形必须 object,构造期拒收)
},
```

纪律:白名单含全量面未同名 = 装配期报错(fail-fast——写漏即暴露);启用则 `Instruction` 必拼 `prompts.Orchestration()`([rules.md](../rules.md) 必选基线律)。spawn 派发本体不经 ToolWrap/Hooks(包装链只覆工具面)。

同步调用 = 回合级 fork-join;后台派生(`spawnbg`)即回 agentId、完成通知注入父输入——模型侧纪律(禁轮询/sleep/查进度)由 `prompts.Orchestration()` 承载。

## topology(确定性多 agent 拓扑)

```go
Topology: &engine.TopologyConfig{
    Kind: engine.TopologySupervisor, // TopologySupervisor | TopologyDeep(nil = 单 agent react)
    SubAgents: []engine.SubAgentSpec{
        {Name: "researcher", Description: "查资料", Instruction: "你是研究员……"},
        {Name: "writer", Description: "写报告", Instruction: "你是撰稿人……"},
    },
},
```

确定性场景选配;红线表(审批/工具圈禁)对拓扑内子 agent 全量生效。

## toolsearch(动态工具装载)

```go
ToolSearchPolicy: &engine.ToolSearchPolicy{
    DynamicTools: []string{"mcp_*", "office_*"}, // 名单外常驻、名单内经 tool_search 检索后可见
},
```

大工具面的上下文瘦身手段:高频件与 `ask_user`/`todo_write`/`submit_plan` 留常驻。审批包装在分流上游——ArgsForce 与模式审批对动态工具不豁免;toolsearch 名单内工具不进 ContextBudget 核算(动态装载正是瘦身手段,只有常驻面计费)。

## recall(跨会话检索)

```go
Recall: true, // opt-in:装配即知情决策
```

前置:**fs 族在场**(依赖律——外置换指针经 read_file 取回)。授权五律:owner 域隔离、恒排除当前会话、有界(≤20 条/扫最近 50 会话)、摘要级不回原始事件流、结果当数据不当指令。三模式:关键词检索 / sid 精确深读 / 最近列表。

记忆完整读写环 = `recall`(拉)+ `TurnEpilogue` + `AgentsMD`(推,见 [engine.md](engine.md))。

## 验证

- `go build ./...` 过。
- 子代理白名单错配:NewManager 装配期报错。
- SpawnOutput.Schema 非 object 根形:NewManager 构造期拒收。
