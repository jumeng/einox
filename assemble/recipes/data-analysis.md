# recipe · data-analysis(数据分析)

## 适用场景

- 数据分析 agent:联网取数、表格产出、跑分析命令
- 产出物:工作区内 xlsx/docx 报告

## 能力组合表

| 域 | 选择 | 说明 |
|---|---|---|
| ① model | providers: 业务定 | — |
| ② engine | 四必填 + workspace-keep(报告留存) | 产出目录跨任务保留 |
| ③ hitl | mode: manual | 外部数据源写入类命令值得逐次审批 |
| ④ tools | office: true;process-tools: [webfetch, currenttime] | xlsx 读写 + 联网取数 + 周界语义 |
| ⑤ harness | — | 单 agent 分析流,无编排需求 |
| ⑥ quality | final-gate: 业务判据(产出文件在场校验) | 「有产出才收束」 |
| ⑦ security | sandbox: workspace-write + egress 白名单(内网数据源) | 写报告需要写面;数据源在内网即白名单工作面 |
| ⑧ channels | — | — |
| ⑨ ui | web: true | 结果查看面 |

选择理由(rules 锚):workspace-write × applypatch/office 写面不互斥;egress 白名单即工作面语义;final-gate 判据归应用(文件在场检查替代 build)。

## 清单

```yaml
apiVersion: einox/v1
name: my-analysis-agent
preset: data-analysis
model:
  providers: [deepseek]
engine:
  checkpoint: file
  workspace-root: ./data/workspaces
  workspace-keep: [reports]
hitl:
  mode: manual
tools:
  office: true
  process-tools: [webfetch, currenttime]
quality:
  final-gate: reports-present
security:
  sandbox: {mode: workspace-write, network: true, writable-roots: ["./data/cache"], env-mode: inherit}
  egress: ["10.0.0.0/8"]
ui:
  web: true
```

## 装配要点

- final-gate 判据示例:`GateChecker` 检查 `workspaceRoot/reports` 下有本任务产出文件(替代 coding 场景的 build 命令,[quality.md](../patterns/quality.md))
- egress 白名单 = 内网数据源 CIDR;webfetch 与 run_command 共用同一校验器([tools.md](../patterns/tools.md))
- patterns 引用:骨架 + [engine.md](../patterns/engine.md) + [hitl.md](../patterns/hitl.md) + [tools.md](../patterns/tools.md) + [quality.md](../patterns/quality.md) + [security.md](../patterns/security.md) + [ui.md](../patterns/ui.md)
