# recipe · minimal(最小问答核)

## 适用场景

- 嵌入既有系统的最小对话核(客服转人工兜底、内部问答机器人雏形)
- 演示 / 学习 einox 装配
- 后续以此为底逐项加能力(增量模式起点)

## 能力组合表

| 域 | 选择 | 说明 |
|---|---|---|
| ① model | providers: 业务定 | 唯一必选项 |
| ② engine | 四必填 | checkpoint: file + workspace-root |
| ③ hitl | — | 无工具面,审批无从谈起 |
| ④ tools | session-tools-off: [fs, cmd, patch] | 物理移除执行/写面,纯交互 |
| ⑤ harness | — | — |
| ⑥ quality | — | — |
| ⑦ security | — | 无命令面,沙箱无用武之地 |
| ⑧ channels | — | — |
| ⑨ ui | — | 入口形态归应用(命令行/HTTP 均可) |

选择理由:互斥律反面——裁 fs/cmd/patch 后 Instruction 勿拼 `prompts.Coding()`;零写面零命令面,审批与沙箱自然出局。

## 清单

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

## 生成物

[patterns/00-skeleton.md](../patterns/00-skeleton.md) 直出物(骨架即本 recipe 样板)。运行时操作面(Fork/Side/Drain)如需暴露归应用层,不进清单。
