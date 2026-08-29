# einox

[![Go Version](https://img.shields.io/badge/go-1.26+-00ADD8)](https://go.dev)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Status](https://img.shields.io/badge/status-early_development-orange)](docs/01-why-eino.md)

**A general-purpose agent base built on [cloudwego/eino](https://github.com/cloudwego/eino).**

在 eino 之上提供 agent 运行时：循环引擎、会话、HITL 审批、沙箱、检查点、skill、多代理编排、通用工具族。einox 是可嵌入的运行时库：`contract` 是端口面，`engine.Options` 是组装根（构造期装配、nil 即不生效），业务只装配、不扩展。

> einox 是独立的第三方开源项目，与 CloudWeGo / ByteDance 无隶属关系。

## 为什么需要 einox

agent 的价值来自嵌入领域系统后获得的数据、权限与流程；通用聊天框中这三者都要外挂补齐。einox 面向「每个系统各自智能化」：交互形态与业务面归应用，机制归基座。

三层栈：eino（LLM 应用框架）→ einox（agent 运行时基座）→ 业务 agent。会话生命周期、HITL 审批、沙箱执行、网络容错、上下文整形等运行时关切不在 eino 的能力面内，也不应在每个业务系统重复实现——einox 将其收敛为机制，一次实现、处处装配：一份基座服务多套业务系统，机制单点修复、策略互不渗透。选型论证见 [docs/01](docs/01-why-eino.md)，分层与场景适配见 [docs/02](docs/02-why-einox.md)。

## 核心能力

- **循环引擎**：ReAct 主循环、失控护栏、确定性拓扑（supervisor/deep）选配
- **会话域**：会话归属与快照、续聊、排队消息、运行中改向（steering）、TTL 清理
- **HITL 审批**：三档模式（manual 逐写审批 / plan 计划卡 / auto 直过）、参数级强制审批（任何模式不豁免）、fail-closed
- **沙箱**：Linux Landlock + seccomp、Windows restricted token、macOS Seatbelt；出口治理（私网默认阻断 + DNS pinning）
- **网络容错**：流式空闲哨兵、错误分类（致命停机/可重试）、有界重连
- **harness**：出站上下文经济（超长结果外置换指针）、长会话摘要、子代理编排（同步/后台派生）、动态工具装载
- **通用工具族**：apply_patch / 文件面 / 命令执行 / 代码仓 worktree / 网页提取 / docx·xlsx 等
- **测试假模型**：`llmtest` 无需真实端点即可跑通引擎与工具循环

全量清单见 [docs/03-capabilities.md](docs/03-capabilities.md)。

## 快速开始

要求 Go 1.26+。einox 是库不是应用：在业务模块中 import 子包（根包无 Go 代码），依赖随 `go mod tidy` 进入 go.mod。最小装配四项必填：

```go
import (
    "path/filepath"

    "github.com/jumeng/einox/engine"
    "github.com/jumeng/einox/llm"
    "github.com/jumeng/einox/session"
)

reg := session.NewRegistry(store) // store 实现 session.Store（落盘归应用）

m, err := engine.NewManager(reg, engine.Options{
    Providers:     func() []llm.ProviderSpec { return llm.ResolveMerged(cfg) },
    Instruction:   func(b engine.SessionBrief) string { return myInstruction(b) },
    CheckPoints:   func(operator, sid string) engine.CheckPointStore { return myStore(operator, sid) },
    WorkspaceRoot: func(owner, sid string) string { return filepath.Join(dataDir, owner, "workspaces", sid) },
})

// 事件经回调扇出（SSE/WebSocket/CLI 传输归应用），同时落会话记录
m.Run(ctx, sess, userMsg, attachments, func(ev session.Event) { /* 编码转发 */ })
```

四项必填之外全部可选（nil 即不生效）；运行面全貌（Resume/FlushQueue）、业务工具与审批接入见 [docs/04-assembly.md](docs/04-assembly.md)。完整可运行示例见 [einox-examples](https://github.com/jumeng/einox-examples)（最小装配 / hitl 审批续流 / 多轮与跨进程续聊，剧本假模型驱动、零端点零密钥可跑）。

## 文档

| 关注点 | 文档 |
|---|---|
| 为什么是 eino | [docs/01-why-eino.md](docs/01-why-eino.md) |
| 为什么需要 einox | [docs/02-why-einox.md](docs/02-why-einox.md) |
| 能力清单 | [docs/03-capabilities.md](docs/03-capabilities.md) |
| 装配 | [docs/04-assembly.md](docs/04-assembly.md) |
| 沙箱 | [docs/05-sandbox.md](docs/05-sandbox.md) |

## 布局

| 目录 | 职责 |
|---|---|
| `contract/` | 契约面（零 eino import，业务只见此面） |
| `engine/` | 循环引擎 |
| `session/` | 会话与快照 |
| `hitl/` | 人在环审批 |
| `mid/` | 工具中间件链 |
| `llm/` | 模型工厂与网络容错 |
| `checkpoint/` | 检查点 |
| `skills/` | skill 机制 |
| `workspace/` | 工作区 |
| `tools/` | 通用工具族 |
| `sandbox/` | 命令执行沙箱（OS 级后端） |
| `prompts/` | 内置提示词 |
| `einoext/` | eino 双向桥 |
| `llmtest/` | 测试假模型 |

## 状态

早期开发中，API 不稳定，可能随时破坏兼容。欢迎 issue 与试用反馈。

## 贡献

仓库工作说明（命令、架构约定、许可与溯源）见 [AGENTS.md](AGENTS.md)；提交前 `go build ./... && go test ./...` 需全绿。

## 许可

Apache-2.0（见 [LICENSE](LICENSE)）；仓库含改编自第三方开源项目的部分，署名见 [NOTICE.md](NOTICE.md)。
