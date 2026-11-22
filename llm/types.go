package llm

import (
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// 契约类型再出口(2026-11-22,三层架构红线):应用层(einox-work)禁止直接
// 依赖 eino——凡消费模型契约类型一律经本包取用,本文件即应用侧唯一适配面
// (升级 eino 版本时只需保证此面稳定)。以下均为类型别名/等价直通,与 eino
// 原类型完全同型,零运行时成本。

// —— 消息契约(schema)——

// Message 会话消息(角色/内容/工具调用)。
type Message = schema.Message

// AgenticMessage 代理型消息(BaseModel 密封消息联合的另一支)。
type AgenticMessage = schema.AgenticMessage

// ToolCall 工具调用(含 ID/名称/参数)。
type ToolCall = schema.ToolCall

// FunctionCall 函数调用(名称/参数)。
type FunctionCall = schema.FunctionCall

// RoleType 消息角色类型。
type RoleType = schema.RoleType

// 角色常量(批 B 补齐:记录/投影面按角色分类消息——Tool 角色的字符串内容
// 即工具输出,应用侧投影需要判别)。
const (
	System    = schema.System
	User      = schema.User
	Assistant = schema.Assistant
	Tool      = schema.Tool
)

// AssistantMessage 构造 assistant 角色消息(等价直通)。
var AssistantMessage = schema.AssistantMessage

// —— 流式契约(schema)——

// StreamReader 流式读取端。
type StreamReader[T any] = schema.StreamReader[T]

// StreamWriter 流式写入端。
type StreamWriter[T any] = schema.StreamWriter[T]

// Pipe 建流(泛型函数无法以别名再出口,此处等价薄包装)。
func Pipe[T any](cap int) (*StreamReader[T], *StreamWriter[T]) {
	return schema.Pipe[T](cap)
}

// —— 模型契约(components/model)——

// BaseModel 通用基座模型接口;M 为密封消息类型(*Message | *AgenticMessage,
// 与 eino 侧 messageType 约束同型)。Generate 阻塞取全量,Stream 增量回流。
type BaseModel[M interface {
	*Message | *AgenticMessage
}] = model.BaseModel[M]

// ChatModel 消息型基座模型(BaseModel[*Message] 特化,应用常用形态,
// 直通 eino 的 BaseChatModel)。
type ChatModel = model.BaseChatModel

// Option 模型调用选项。
type Option = model.Option
