# einox

[![Go Version](https://img.shields.io/badge/go-1.26+-00ADD8)](https://go.dev)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Status](https://img.shields.io/badge/status-early_development-orange)](docs/01-why-eino.md)

**A general-purpose agent base built on [cloudwego/eino](https://github.com/cloudwego/eino).**

通用 agent 基座：在 eino 之上提供完整的 agent 运行时——循环引擎、会话、HITL 审批、沙箱、检查点、skill 机制、多代理编排、通用工具族。面向把 agent 作为**组件嵌入业务系统**的场景：**可嵌入的 agent 运行时库**——`contract` 为端口面（端口-适配器边界），`engine.Options` 为组装根（构造期装配、nil 即不生效）；业务对基座只装配、不扩展。

> einox 是独立的第三方开源项目，与 CloudWeGo / ByteDance 无隶属关系。

## 为什么需要 einox

einox 的判断：**每个系统各自智能化，而不是一个智能体去管理不同系统**——基座做 agent 工厂，不做又一个 agent 产品。agent 的价值不在通用聊天框，而在嵌入领域系统后获得的数据、权限与流程；定位是**可装配的库，不是应用**——交互形态与业务面归应用，机制归基座。

三层栈：eino（LLM 应用框架）→ **einox**（agent 运行时基座）→ 业务 agent。会话生命周期、HITL 审批、沙箱执行、网络容错、上下文整形这些「怎么跑起来」的运行时关切不在 eino 的面里，也不该散落在每个业务系统里各造一遍——einox 把它们收敛为机制，一次实现、处处装配。一份基座可服务多套业务系统：各自组装根装配各自的 agent 形态，机制单点修复、策略互不渗透。选型论证见 [docs/01-why-eino.md](docs/01-why-eino.md)，基座层论证与场景适配见 [docs/02-why-einox.md](docs/02-why-einox.md)。

## 核心能力

- **循环引擎**：ReAct 主循环、失控护栏、确定性拓扑（supervisor/deep）选配
- **会话域**：会话归属与快照、续聊、排队消息、运行中改向（steering）、TTL 清理
- **HITL 审批**：三档模式（manual 逐写审批 / plan 计划卡 / auto 直过）+ 参数级强制审批（任何模式不豁免）、fail-closed
- **沙箱**：Linux Landlock + seccomp、Windows restricted token、macOS Seatbelt；出口治理（私网默认阻断 + DNS pinning）
- **网络容错**：流式空闲哨兵、错误分类（致命停机/可重试）、有界重连
- **harness**：出站上下文经济（超长结果外置换指针）、长会话摘要、子代理编排（同步/后台派生）、动态工具装载
- **通用工具族**：apply_patch / 文件面 / 命令执行 / 代码仓 worktree / 网页提取 / docx·xlsx 等
- **测试假模型**：`llmtest` 零真实端点跑通引擎与工具循环

全量清单见 [docs/03-capabilities.md](docs/03-capabilities.md)。

## 快速开始

要求 Go 1.26+。

```bash
go get github.com/jumeng/einox
```

最小装配（四项必填 Options + 运行面）见 [docs/04-assembly.md](docs/04-assembly.md)；测试注入 `llmtest` 假模型即可零真实端点跑通引擎 + 工具循环。

## 文档

| 关注点 | 文档 |
|---|---|
| 为什么是 eino：思路、架构与交付形态 | [docs/01-why-eino.md](docs/01-why-eino.md) |
| 为什么需要 einox：基座的设计与分层 | [docs/02-why-einox.md](docs/02-why-einox.md) |
| 能力清单：引擎 / 工具族 / harness / 提示词 / 沙箱 / 模型面 | [docs/03-capabilities.md](docs/03-capabilities.md) |
| 装配：快速使用、自由裁剪、业务扩展 | [docs/04-assembly.md](docs/04-assembly.md) |
| 沙箱：执行围栏、部署前提与平台限制 | [docs/05-sandbox.md](docs/05-sandbox.md) |

## 状态

早期开发中，API 不稳定，随时破坏兼容。欢迎 issue 讨论与试用反馈。

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
| `prompts/` | 内置提示词 |
| `einoext/` | eino 双向桥 |
| `llmtest/` | 测试假模型 |

## 贡献

欢迎 issue 与 PR。仓库工作说明（命令、架构约定、许可与溯源）见 [AGENTS.md](AGENTS.md)；提交前 `go build ./... && go test ./...` 全绿。

## 许可

Apache-2.0（见 [LICENSE](LICENSE)）；仓库含改编自第三方开源项目的部分，署名详见 [NOTICE.md](NOTICE.md)。
