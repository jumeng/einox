package engine

// 历史整形（模型输入回传前的防御视图）：sanitizeHistory 剥悬空/孤儿/空壳
// 修复存量脏历史、cloneMsgs 副本纪律、msgTextOf 多模态文本回退——原文
// 不动（会话真源保真），仅本轮回传视图生效。

import (
	"strings"

	"github.com/cloudwego/eino/schema"
)

// msgTextOf 消息文本（多模态消息 Content 为空——文本只进 text part，估算回退
// 读 parts）。
func msgTextOf(m *schema.Message) string {
	if len(m.UserInputMultiContent) == 0 {
		return m.Content
	}
	var b strings.Builder
	for _, p := range m.UserInputMultiContent {
		if p.Type == schema.ChatMessagePartTypeText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// sanitizeHistory 历史回传防御（深拷贝副本上修复——会话真源不动，仅本轮
// 回传视图生效；① ② ③ 剔除后返回新序列）：
// ① 空 arguments 的 tool call 经 openai 序列化 omitempty 会整个省略 arguments
// 键，严格供应商直接 400。分片归并修复前落盘的存量脏历史在此自愈——回灌 "{}"。
// ② 悬空 tool_calls：assistant 带 tool_calls 后必须紧跟每个 tool_call_id 的
// tool 消息，缺失即 400。存量脏历史自愈——剥离未应答项（部分应答保留已应答
// 项，消息文本保留）。
// ③ 空 assistant 消息剔除：错误轮次落盘的空 final 回传即 400；纯悬空
// tool_calls 剥离后变空的消息一并剔除（须在②之后）。
// ④ 孤儿 tool 消息剔除：tool 消息前无带对应 tool_call 的 assistant——回传
// 即 400。
func sanitizeHistory(msgs []*schema.Message) []*schema.Message {
	// 深拷贝改写：源切片与 persist 的持锁 marshal 共享同批 *Message（CloneHistory
	// 浅拷贝），锁外原地改写 ToolCalls/Arguments 构成竞态——在副本上改写零共享。
	msgs = cloneMsgs(msgs)
	for _, m := range msgs {
		for i := range m.ToolCalls {
			if m.ToolCalls[i].Function.Arguments == "" {
				m.ToolCalls[i].Function.Arguments = "{}"
			}
		}
	}
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role != schema.Assistant || len(m.ToolCalls) == 0 {
			continue
		}
		answered := map[string]bool{}
		for j := i + 1; j < len(msgs) && msgs[j].Role == schema.Tool; j++ {
			answered[msgs[j].ToolCallID] = true
		}
		kept := m.ToolCalls[:0]
		for _, tc := range m.ToolCalls {
			if answered[tc.ID] {
				kept = append(kept, tc)
			}
		}
		if len(kept) == 0 {
			m.ToolCalls = nil
		} else {
			m.ToolCalls = kept
		}
	}
	out := msgs[:0]
	for _, m := range msgs {
		if m.Role == schema.Assistant && m.Content == "" && m.ReasoningContent == "" && len(m.ToolCalls) == 0 {
			continue
		}
		out = append(out, m)
	}
	// ④ 孤儿 tool 消息剔除（前无带对应 tool_call 的 assistant）；须在②③后
	//（剥悬空/剔空都可能制造新孤儿）。
	kept := out[:0]
	open := map[string]bool{}
	for _, m := range out {
		switch m.Role {
		case schema.Assistant:
			clear(open)
			for _, tc := range m.ToolCalls {
				open[tc.ID] = true
			}
		case schema.Tool:
			if !open[m.ToolCallID] {
				continue
			}
			delete(open, m.ToolCallID)
		default:
			clear(open)
		}
		kept = append(kept, m)
	}
	return kept
}

// cloneMsgs 消息浅层组拷贝（消息值拷贝 + ToolCalls 切片拷贝——后续改写
// ToolCalls 元素不触共享底数组；Content 等标量字段值语义天然隔离）。
func cloneMsgs(msgs []*schema.Message) []*schema.Message {
	out := make([]*schema.Message, len(msgs))
	for i, m := range msgs {
		cp := *m
		if len(m.ToolCalls) > 0 {
			cp.ToolCalls = append([]schema.ToolCall(nil), m.ToolCalls...)
		}
		if len(m.Extra) > 0 { // T6：Extra map 一层拷贝——map 共享引用会被 sanitizeHistory 原地改写竞态（值只读不深拷，与 Messages 浅层纪律同款）
			cp.Extra = make(map[string]any, len(m.Extra))
			for k, v := range m.Extra {
				cp.Extra[k] = v
			}
		}
		out[i] = &cp
	}
	return out
}
