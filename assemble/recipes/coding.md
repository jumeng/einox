# recipe · coding(编码 agent)

## 适用场景

- 终端/服务器编码 agent(对标编码形态 CLI 工具的内核)
- 需要:改文件、跑命令、验证闭环、写操作审批、命令沙箱

## 能力组合表

| 域 | 选择 | 说明 |
|---|---|---|
| ① model | providers: 业务定;fallback-chain 可选 | 多供应商 + 降级链增强可用性 |
| ② engine | 四必填 + context-budget: 8192 | 全工具面常驻,预算账本可见 |
| ③ hitl | mode: manual | 写操作逐次审批——编码场景安全底线 |
| ④ tools | 全工具面(off 空);process-tools: [currenttime, webfetch] | fs/cmd/patch/todo 在场 → Instruction 必拼 `prompts.Coding()` |
| ⑤ harness | subagents(白名单归业务) | 并行子任务派发 → 必拼 `prompts.Orchestration()` |
| ⑥ quality | final-gate: build-test | 收束质量门——验证纪律靠门不靠提示词堆叠 |
| ⑦ security | sandbox: workspace-write + network: on + 缓存重定向 | 命令面主执行边界;依赖安装需要网络 |
| ⑧ channels | — | 编码场景入口在终端/IDE,不需要消息渠道 |
| ⑨ ui | web: true | 回放+交互面(本地开发 Authorize 可缺省,生产必配) |

选择理由(rules 锚):必选基线律(Coding/Orchestration 拼装)、final-gate 依赖律(cmd 族在场)、sandbox 档位语义(workspace-write + network on + Env 缓存重定向保命件)。

## 清单

```yaml
apiVersion: einox/v1
name: my-coding-agent
preset: coding
model:
  providers: [deepseek, glm]
  fallback-chain: [deepseek/deepseek-chat, glm/glm-5.3]
engine:
  checkpoint: file
  workspace-root: ./data/workspaces
  context-budget: 8192
hitl:
  mode: manual
tools:
  process-tools: [currenttime, webfetch]
harness:
  subagents: true
quality:
  final-gate: build-test
security:
  sandbox: {mode: workspace-write, network: true, writable-roots: ["./data/cache"], env-mode: minimal}
ui:
  web: true
```

## 装配要点

- Instruction 拼装序:业务职责段 + `prompts.Coding()` + `prompts.Orchestration()` + 会话配置段
- main 顶部必挂 `sandbox.RunHelper(os.Args)`
- patterns 引用:[00-skeleton](../patterns/00-skeleton.md) 打底 + [model.md](../patterns/model.md) + [engine.md](../patterns/engine.md) + [hitl.md](../patterns/hitl.md) + [tools.md](../patterns/tools.md) + [harness.md](../patterns/harness.md) + [quality.md](../patterns/quality.md) + [security.md](../patterns/security.md) + [ui.md](../patterns/ui.md)
