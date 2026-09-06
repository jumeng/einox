// Package channels 官方通用渠道件目录（对标 llm 供应商「内置目录 + 自定义」
// 两层模式）：feishu 实装、voice 占位。渠道协议与渲染归适配器——引 SDK 依赖，
// 应用不 import 则不进构建；业务自定义渠道长在业务仓，实现 engine 的
// ChannelSink/Handle/Approve/Answer 面即成一个渠道（编排机制核在
// engine/channel.go，三层结构与接入蓝图见 docs/04）。
package channels
