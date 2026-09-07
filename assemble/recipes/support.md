# recipe · support(客服问答)

## 适用场景

- 客服/工单 agent:接消息渠道、查历史工单记忆、出结构化报告
- 低危场景:计划卡授权代替逐次审批

## 能力组合表

| 域 | 选择 | 说明 |
|---|---|---|
| ① model | providers: 业务定 | 单供应商即可起步 |
| ② engine | 四必填 + agentsmd(owner 域记忆) | 记忆推通道 |
| ③ hitl | mode: plan | 计划卡批准 = 任务期授权——客服流程节奏合适 |
| ④ tools | office: true;ask/plan 族在场 | ask_user 结构化提问收集信息;office 出工单报告 |
| ⑤ harness | recall: true | 拉通道——查本 owner 历史会话(前置:fs 族在场) |
| ⑥ quality | — | 对话型产出,无 build 类判据 |
| ⑦ security | sandbox: read-only | 纯读形态,无写面互斥成立 |
| ⑧ channels | [feishu] | 客户在哪渠道就在哪 |
| ⑨ ui | web: true | 运营侧管理面 |

选择理由(rules 锚):recall 依赖律(fs 族在场);read-only × 无写工具互斥律成立;agentsmd + TurnEpilogue 记忆读写环(engine.md)。

## 清单

```yaml
apiVersion: einox/v1
name: my-support-agent
preset: support
model:
  providers: [glm]
engine:
  checkpoint: file
  workspace-root: ./data/workspaces
  agentsmd: ["./data/owners/{owner}/memory.md"]
hitl:
  mode: plan
tools:
  office: true
harness:
  recall: true
security:
  sandbox: {mode: read-only, network: false, env-mode: inherit}
channels: [feishu]
ui:
  web: true
```

## 装配要点

- `TurnEpilogue` 落 owner 域 memory.md + `AgentsMD` 注入 = 记忆读写环([engine.md](../patterns/engine.md))
- feishu 两段式装配与停机序([channels.md](../patterns/channels.md));SDK 依赖进构建
- patterns 引用:骨架 + [engine.md](../patterns/engine.md) + [hitl.md](../patterns/hitl.md) + [tools.md](../patterns/tools.md) + [harness.md](../patterns/harness.md) + [security.md](../patterns/security.md) + [channels.md](../patterns/channels.md) + [ui.md](../patterns/ui.md)
