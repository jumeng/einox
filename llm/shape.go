package llm

// historyShape 出站历史整形（Phase H1①，请求边界视图变换）：
// DeepSeek thinking_mode 两分回传——在途轮（最后一条 user 之后）带 tool_calls
// 的 assistant 必须回传 ReasoningContent，其余轮端点不消费、逐轮回传纯耗
// prompt token（样例会话 reasoning 占 28%）。本包装在每次模型调用前变换
// 出站视图，存储保真——session 历史原文不动：
//   剥离 = 已结算历史轮 + 非工具轮（探针 B 态 200 实证可剥）
//   剔除 = 剥后零负载消息（content 空 + 无 tool_calls；C4 态 200 实证剔除
//     后形态合法，保留亦合法〔C2/C3 200〕，取剔除省框架费去噪）
// 无 reasoning 历史原样透传（快路径：指针级零开销）。协议依据与探针定案 =
// findings/2026-08-26-h1-probe-reasoning-passback.md。
//
// H9-2 anthropic 协议分叉：thinking 出站存 Extra 双键（eino-ext claude
// convSchemaMessage 读该键构造 thinking block，不读 ReasoningContent——本地
// v0.1.25 实核，上游无出站开关改动）。anthropic 分支在剥 ReasoningContent
// 之外删双键；官方协议「历史轮可省（API ignores/4.0-4.1 自动剥离不计费）/
// 在途 tool_use 轮必带（complete unmodified 含 signature）」与 openai 系
// 两分同构。键名为 eino-ext 包内私有常量（message_extra.go keyOfThinking/
// keyOfThinkingSignature）的硬编码镜像——上游改键名即失效，shape_test 防
// 退化断言锚定。计数口径注记：anthropic 入站双写 Extra 与 ReasoningContent
// 同文（claude.go 入站路径），shapedTokenCounter 经 kind="" 剥 ReasoningContent
// 计数即与 anthropic 出站同口径——无需计数变体（除非出现两字段不同步形态）。

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// kindAnthropic ProviderSpec.Kind 协议标识（与 llm.Resolve 的 kind 值一致）。
const kindAnthropic = "anthropic"

// eino-ext claude 组件的 Extra 键名镜像（私有常量无法 import；出处 =
// eino-ext/components/model/claude message_extra.go keyOfThinking /
// keyOfThinkingSignature）。
const (
	keyClaudeThinking          = "_eino_claude_thinking"
	keyClaudeThinkingSignature = "_eino_claude_thinking_signature"
)

// NewHistoryShapeModel 出站历史整形包装（kind = provider 协议标识；当前
// openai/anthropic 两协议同规则——在途例外与官方 thinking 指南一致，
// anthropic 分叉多剥 Extra 双键）。
func NewHistoryShapeModel(inner model.BaseModel[*schema.Message], kind string) model.BaseModel[*schema.Message] {
	return &historyShapeModel{inner: inner, kind: kind}
}

// ShapeMessages 出站视图整形（规则同上；导出面 = reduction TokenCounter
// 复用——计数与真实出站同一规则函数，口径漂移结构性不可能。anthropic 计
// 数口径说明见包注释）。
func ShapeMessages(input []*schema.Message) []*schema.Message {
	return shapeMessages(input, "")
}

// ShapeMessagesForKind 带协议分叉的整形（H9-2：kind=anthropic 时额外删
// Extra thinking 双键；kind 空/openai 与 ShapeMessages 同规则）。
func ShapeMessagesForKind(input []*schema.Message, kind string) []*schema.Message {
	return shapeMessages(input, kind)
}

func shapeMessages(input []*schema.Message, kind string) []*schema.Message {
	lastUser := -1
	for i := len(input) - 1; i >= 0; i-- {
		if input[i].Role == schema.User {
			lastUser = i
			break
		}
	}
	anthropic := kind == kindAnthropic
	changed := false
	out := make([]*schema.Message, 0, len(input))
	for i, m := range input {
		// 在途区带 tool_calls 的 assistant 保留 thinking，其余带 reasoning 轮剥离
		inFlight := i > lastUser && len(m.ToolCalls) > 0
		if m.Role == schema.Assistant && !inFlight && (m.ReasoningContent != "" || (anthropic && hasExtraThinking(m))) {
			clone := *m
			clone.ReasoningContent = ""
			if anthropic && len(clone.Extra) > 0 {
				ex := make(map[string]any, len(clone.Extra))
				for k, v := range clone.Extra {
					if k != keyClaudeThinking && k != keyClaudeThinkingSignature {
						ex[k] = v
					}
				}
				clone.Extra = ex
			}
			if clone.Content != "" || len(clone.ToolCalls) > 0 {
				out = append(out, &clone)
			} // 剥后零负载（reasoning-only 轮）：整条剔除出出站视图
			changed = true
			continue
		}
		out = append(out, m)
	}
	if !changed {
		return input
	}
	return out
}

// hasExtraThinking anthropic 形态判定（Extra 带内容型 thinking 键）。
func hasExtraThinking(m *schema.Message) bool {
	v, ok := m.Extra[keyClaudeThinking]
	return ok && v != nil && v != ""
}

type historyShapeModel struct {
	inner model.BaseModel[*schema.Message]
	kind  string
}

func (h *historyShapeModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return h.inner.Generate(ctx, ShapeMessagesForKind(input, h.kind), opts...)
}

func (h *historyShapeModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return h.inner.Stream(ctx, ShapeMessagesForKind(input, h.kind), opts...)
}
