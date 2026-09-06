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

	"github.com/jumeng/einox/contract"
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

// speakerExtraID/Name 历史消息 Extra 的说话人署名双键（T6——Extra 是 adk
// 体系公共 map 通道，键带命名空间防撞）。平铺存字符串而非嵌套 map：adk
// 检查点是 gob 序列化，接口槽位放 map 需 gob.Register（feishu 实测挂起点
// 存检查点即炸）——纯内建 string 值免注册，JSON/gob 往返天然安全。
const (
	speakerExtraID   = "einox.speaker.id"
	speakerExtraName = "einox.speaker.name"
)

// inputSegment 输入分段（T6 多参与者）：同人相邻合并、异人各自成条。
type inputSegment struct {
	speaker *contract.Participant // nil = 无署名（单用户/系统通知）
	texts   []string
	atts    []session.Attachment
}

// inputSegments 排队消息 + 直接输入按说话人分段（T6）；空源跳过；说话人
// 全空退化为单段 = 旧单条合并形态（含通知段并入——与旧行为一致）。
func inputSegments(queued []session.QueuedMsg, userMsg string, atts []session.Attachment, actor *contract.Participant) []inputSegment {
	var segs []inputSegment
	add := func(sp *contract.Participant, text string, as []session.Attachment) {
		if text == "" && len(as) == 0 {
			return
		}
		if n := len(segs); n > 0 && sameSpeaker(segs[n-1].speaker, sp) {
			segs[n-1].texts = append(segs[n-1].texts, withAttachments(text, as))
			segs[n-1].atts = append(segs[n-1].atts, as...)
			return
		}
		segs = append(segs, inputSegment{speaker: sp, texts: []string{withAttachments(text, as)}, atts: as})
	}
	for _, q := range queued {
		var sp *contract.Participant
		if q.SpeakerID != "" {
			sp = &contract.Participant{ID: q.SpeakerID, Name: q.SpeakerName}
		}
		add(sp, q.Text, q.Attachments)
	}
	add(actor, userMsg, atts)
	return segs
}

func sameSpeaker(a, b *contract.Participant) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.ID == b.ID
}

// renderSpeakers 模型输入投影（T6）：带署名的 user 消息渲染「名字：」前缀——
// 说话人原生进模型面（einox 自持历史投影的账二兑现位）。copy-on-write 不动
// 入参（存储保真——history 原文不带前缀，Extra 携署名）；多模态消息前缀进
// 首个 text part。
func renderSpeakers(msgs []*schema.Message) []*schema.Message {
	out, copied := msgs, false
	for i, m := range msgs {
		if m.Role != schema.User || m.Extra == nil {
			continue
		}
		name, _ := m.Extra[speakerExtraName].(string)
		if name == "" {
			continue
		}
		prefix := name + "："
		cp := *m
		switch {
		case cp.Content != "":
			cp.Content = prefix + cp.Content
		case len(cp.UserInputMultiContent) > 0 && cp.UserInputMultiContent[0].Type == schema.ChatMessagePartTypeText:
			parts := append([]schema.MessageInputPart(nil), cp.UserInputMultiContent...)
			parts[0].Text = prefix + parts[0].Text
			cp.UserInputMultiContent = parts
		default:
			continue
		}
		if !copied {
			out = append([]*schema.Message(nil), msgs...)
			copied = true
		}
		out[i] = &cp
	}
	return out
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
// 回执同律（审查 P2-15）：此处排空的消息与 Run 头部路径一样翻「已注入」
// 事件——此前运行中到达的排队消息不落 steer_injected/notify_injected，回放
// 里永远停在 queued 态；notify 独立标签（非用户补充——审计可辨）。
// 跨轮保真（安全审查 2026-09-06）：注入消息同时入会话历史——此前只进
// state.Messages（本轮运行态），轮结束后下一轮 CloneHistory 不含注入原文，
// 模型对「用户运行中说过什么」失忆（摘要/门回灌/recall/Fork 同受影响）。
// 顺序天然正确：注入先入史，本轮后续 assistant 终态由 settleTurn 追加其后。
func (m *steeringMiddleware) BeforeModelRewriteState(
	ctx context.Context, state *adk.TypedChatModelAgentState[*schema.Message], mc *adk.ModelContext,
) (context.Context, *adk.TypedChatModelAgentState[*schema.Message], error) {
	for _, msg := range m.sess.TakePending() {
		if msg.Kind == "notify" {
			m.sess.Record(contract.EvNotifyInjected, contract.SteerEvent{ID: msg.ID, Text: msg.Text, Kind: msg.Kind})
			injected := userMessageWithImages("（系统通知）"+msg.Text, msg.Attachments)
			state.Messages = append(state.Messages, injected)
			m.sess.AppendHistory(injected)
			continue
		}
		m.sess.Record(contract.EvSteerInjected, contract.SteerEvent{ID: msg.ID, Text: msg.Text,
			Attachments: msg.Attachments, SpeakerID: msg.SpeakerID, SpeakerName: msg.SpeakerName})
		label := "（用户运行中补充）"
		if msg.SpeakerName != "" { // T6 谁的运行中补充
			label = "（" + msg.SpeakerName + " 运行中补充）"
		}
		injected := userMessageWithImages(label+msg.Text, msg.Attachments)
		state.Messages = append(state.Messages, injected)
		m.sess.AppendHistory(injected)
	}
	return ctx, state, nil
}
