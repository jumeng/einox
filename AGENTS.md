# AGENTS.md

面向 AI 编码代理与人类贡献者的仓库工作说明。

## 项目概览

einox 是通用 agent 基座库——三层栈 eino（LLM 框架）→ einox（agent 运行时基座）→ 业务 agent 中的机制层：循环引擎、会话、审批、沙箱、harness、通用工具族。文档自述用语保持一致：可嵌入运行时库、端口-适配器（`contract` = 端口面）、组装根（`engine.Options` 构造期装配）、封闭枚举的装配缝——不是插件系统。选型与能力面见 [docs/](docs/01-why-eino.md)。

## 架构速览

依赖方向（只允许向左）：

```
应用 ─→ contract 及各机制包公开面 ─→ engine / llm / tools …（基座内部）─→ eino / eino-ext
```

- `contract/` 是对应用的唯一契约面，零 eino 类型；eino 只在基座内部被消费（engine / llm / einoext）
- 一次运行的通路：`Manager.Run` 驱动 adk ReAct 循环（eino 模型组件调 LLM）→ 引擎把流翻译为 `contract.Event` → `Session.Record` 落会话记录 + emit 回调实时扇出；自然收束可经 `FinalGate` 门循环回灌重跑（可选——判据归应用，见 docs/03）；挂起交互（`Suspend`）转引擎 Interrupt，经 `Resume` 续流
- 工具装配链（组装期逐层包装，由内向外）：业务工具 → mid validate / errFeed / guard → hitl 审批包装 → `ToolWrap`（应用包装缝，nil 不挂——只能收紧不能放宽）→ einoext 桥（eino ParamsOneOf；幻觉工具名兜底信封回喂 + panic 单点收敛）；会话域件可经 `SessionToolsOff` 按族裁剪（todo/ask/plan/fs/cmd/patch，未知名构造期即拒）
- 会话态归 `session`（Registry / Session / 快照 / 排队消息）；检查点经 `engine.CheckPointStore`；工具面路径圈进会话工作区
- 测试：`boundary_test.go` 守两道依赖边界；`engine` 各测试用 `llmtest` 假模型验证行为，不碰真实端点

## 常用命令

- 构建：`go build ./...`
- 交叉编译（改动平台分支文件 `_linux/_windows/_darwin` 或构建标签时必跑；CI 有同款矩阵门）：`GOOS=windows go build ./... && GOOS=linux go build ./...`——注意编译门只证可构建，平台分支的语义同步靠评审核对共享逻辑的各平台变体
- 测试：`go test ./...`（提交前应全绿；沙箱相关包按平台构建标签分流，非目标平台自动跳过）
- 引擎与工具循环的测试使用 `llmtest` 假模型，不依赖真实模型端点

## 架构约定

- `contract/` 不 import eino；外部依赖收敛在 `boundary_test.go` 的白名单内——新增依赖需同步更新清单，守卫拒绝清单外的一切模块
- 能力统一经 `engine.Options` 装配：新能力建模为可选配置字段，nil 即不生效、不改变既有行为
- 机制与内容分离：业务工具、提示词内容、审批名单等业务资产不属于本仓
- eino 已有的能力（中间件 / 原语 / 预置拓扑）优先复用；确需自研时在 PR 中说明 eino 的缺口

## 许可与溯源

- 新增依赖限宽松协议（MIT / Apache-2.0 / BSD 等），不引入 GPL / AGPL / MPL 系
- 引入或改编第三方代码与文本：文件头注明来源与协议，并在 [NOTICE.md](NOTICE.md) 登记
- 不引入没有明确许可的第三方内容（含闭源产品的代码与文本——参考其行为语义不受限）

## 协作原则

### 1. 编码前先思考

**不要假设。不要掩饰困惑。明确呈现权衡。**

- 不确定就提问，不要默默选择
- 有更简单的方法就直接指出
- 有困惑就停下来，说清楚再继续

### 2. 简单优先

**只写解决问题所需的最少代码。**

- 不做超出需求的扩展、抽象或"灵活性"
- 不为不会发生的场景写错误处理
- 200 行能减到 50 行，就重写

### 3. 外科手术式修改

**只改必须改的内容。**

- 不顺手优化无关代码、注释或格式
- 保持现有风格
- 每一行改动都能追溯到用户请求

### 4. 目标驱动执行

**先定义成功标准，再推进验证。**

- 多步骤任务先列计划，每步有可验证的检查项

以上原则转述自业界通行的 agent 协作建议（源自 Andrej Karpathy 的公开分享）。

## 代码风格

- 注释与文档用中文，标识符保持英文；目录名从简（单词优先）
