package engine

// engine 单元回归（自产品 agent_test 的纯函数段迁入）：sanitizeHistory 防御
// 修复。行为级端到端回归（会话/审批/steering/usage 全链）在产品装配层测试
// （internal/agent）经 llmtest 假模型驱动；超长工具结果截断/外置归
// reduction_test.go（2026-08-26 newSpiller 退役，截断面统一移交 reduction）。

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestSanitizeHistoryRepairsEmptyArgs(t *testing.T) {
	// ① 空 arguments 回灌 "{}"
	msgs := []*schema.Message{
		schema.UserMessage("问题"),
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "t", Arguments: ""},
		}}},
		schema.ToolMessage("ok", "c1"),
	}
	out := sanitizeHistory(msgs)
	if out[1].ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("空 arguments 应回灌：%.30s", out[1].ToolCalls[0].Function.Arguments)
	}

	// ② 悬空 tool_calls 剥离（无应答 tool 消息）
	msgs = []*schema.Message{
		schema.UserMessage("问题"),
		{Role: schema.Assistant, Content: "我先调工具", ToolCalls: []schema.ToolCall{{ID: "c9", Function: schema.FunctionCall{Name: "t"}}}},
		schema.UserMessage("继续"),
	}
	out = sanitizeHistory(msgs)
	if len(out) != 3 || len(out[1].ToolCalls) != 0 || out[1].Content != "我先调工具" {
		t.Fatalf("悬空 tool_calls 应剥离（文本保留）：n=%d calls=%d", len(out), len(out[1].ToolCalls))
	}

	// ③ 空 assistant 剔除
	msgs = []*schema.Message{
		schema.UserMessage("问题"),
		{Role: schema.Assistant},
		schema.AssistantMessage("答复", nil),
	}
	out = sanitizeHistory(msgs)
	if len(out) != 2 {
		t.Fatalf("空 assistant 应剔除：%d", len(out))
	}

	// ④ 孤儿 tool 消息剔除
	msgs = []*schema.Message{
		schema.UserMessage("问题"),
		schema.ToolMessage("孤儿", "c0"),
		schema.AssistantMessage("答复", nil),
	}
	out = sanitizeHistory(msgs)
	if len(out) != 2 {
		t.Fatalf("孤儿 tool 消息应剔除：%d", len(out))
	}
}
