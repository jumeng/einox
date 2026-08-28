package engine

// steering 运行中输入注入（自产品 internal/agent/steering.go 迁入）：
// BeforeModelRewriteState（每次模型调用前触发）drain 会话 pending 队列 →
// 追加为 user message——当前调用链完整走完后模型自然看到，不中断已执行操作。
// 排队兜底（Run 结束后才到达的输入 → 下一轮前置）在 Manager.Run 头部承担。

import (
	"context"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// steeringMiddleware 会话级 steering 注入钩子。
type steeringMiddleware struct {
	adk.TypedBaseChatModelAgentMiddleware[*schema.Message]
	sess *session.Session
}

// newSteeringMiddleware 构造。
func newSteeringMiddleware(sess *session.Session) adk.TypedChatModelAgentMiddleware[*schema.Message] {
	return &steeringMiddleware{sess: sess}
}

// withAttachments 附件引用拼接（模型输入形态：路径引用，读文档工具消费）。
func withAttachments(text string, atts []session.Attachment) string {
	if len(atts) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text)
	if text != "" {
		b.WriteString("\n\n")
	}
	b.WriteString("（附件）")
	for _, a := range atts {
		b.WriteString("\n- ")
		b.WriteString(a.Path)
		if a.IsImage {
			b.WriteString("（图片）")
		}
	}
	return b.String()
}

// userMessageWithImages 含图附件升级为多模态用户消息（官方路线：图片以轻引用
// part 直接进模型输入——请求边界由 llm 视觉包装解析为 base64/驱逐/门禁）；
// 纯文本附件保持原拼接形态，零行为变化。带 parts 时 Content 必须为空——
// openai 适配层把 Content 与 UserInputMultiContent 同时拷入 ChatCompletionMessage，
// SDK MarshalJSON 拒绝并存（文本只进 text part）。
func userMessageWithImages(text string, atts []session.Attachment) *schema.Message {
	n := 0
	for _, a := range atts {
		if a.IsImage {
			n++
		}
	}
	if n == 0 {
		return schema.UserMessage(text)
	}
	parts := make([]schema.MessageInputPart, 0, n+1)
	parts = append(parts, schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: text})
	for _, a := range atts {
		if !a.IsImage {
			continue
		}
		u := llm.AttRefPrefix + a.Path
		parts = append(parts, schema.MessageInputPart{
			Type:  schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{URL: &u}},
		})
	}
	return &schema.Message{Role: schema.User, UserInputMultiContent: parts}
}

// BeforeModelRewriteState 模型调用前注入 pending 补充（标注来源，模型可辨识）。
func (m *steeringMiddleware) BeforeModelRewriteState(
	ctx context.Context, state *adk.TypedChatModelAgentState[*schema.Message], mc *adk.ModelContext,
) (context.Context, *adk.TypedChatModelAgentState[*schema.Message], error) {
	for _, msg := range m.sess.TakePending() {
		state.Messages = append(state.Messages, userMessageWithImages("（用户运行中补充）"+msg.Text, msg.Attachments))
	}
	return ctx, state, nil
}
