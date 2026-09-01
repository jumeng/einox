// client.go SDK 边界真实实现：长连接（ws）事件 → 处理闭包；卡片消息收发。
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	disp "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// inboundMsg 入站消息事件（SDK 类型到此为止——处理面只依赖本结构）。
type inboundMsg struct {
	chatID   string // 会话键（单聊/群聊统一 chat_id）
	openID   string // 发送者（owner 判定）
	chatType string // p2p | group
	text     string // 纯文本（mention 已剥离）
}

// cardAction 卡片交互回调（按钮 value 透传）。
type cardAction struct {
	value map[string]any
}

// realClient 长连接客户端：事件经 dispatcher 进处理闭包；发消息/更新卡片走
// OpenAPI（tenant token 由 SDK 管理）。
type realClient struct {
	ws  *larkws.Client
	cli *lark.Client
}

// newClient 构造（事件订阅：消息接收 + 卡片交互；长连接模式免公网回调与
// 签名验证——verificationToken/encryptKey 留空）。
func newClient(cfg Config, onMsg func(inboundMsg), onCard func(cardAction)) (*realClient, error) {
	if cfg.AppID == "" || cfg.AppSecret == "" {
		return nil, fmt.Errorf("feishu: AppID/AppSecret 缺失（开放平台应用凭证）")
	}
	d := disp.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(_ context.Context, ev *larkim.P2MessageReceiveV1) error {
			m := parseMessageEvent(ev)
			if m != nil {
				onMsg(*m)
			}
			return nil // 事件级错误不上抛（SDK 会重投整条事件——渲染缺陷不该放大为重放）
		}).
		OnP2CardActionTrigger(func(_ context.Context, ev *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			if ev.Event != nil && ev.Event.Action != nil {
				onCard(cardAction{value: ev.Event.Action.Value})
			}
			return &callback.CardActionTriggerResponse{}, nil
		})
	return &realClient{
		ws:  larkws.NewClient(cfg.AppID, cfg.AppSecret, larkws.WithEventHandler(d)),
		cli: lark.NewClient(cfg.AppID, cfg.AppSecret),
	}, nil
}

// Run 长连接事件循环（阻塞）。
func (c *realClient) Run(ctx context.Context) error { return c.ws.Start(ctx) }

// SendCard 发卡片消息（receive_id_type = chat_id）。
func (c *realClient) SendCard(ctx context.Context, chatID string, card []byte) (string, error) {
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(&larkim.CreateMessageReqBody{
			ReceiveId: larkcore.StringPtr(chatID),
			MsgType:   larkcore.StringPtr("interactive"),
			Content:   larkcore.StringPtr(string(card)),
		}).Build()
	resp, err := c.cli.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: 发卡片失败: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("feishu: 发卡片失败: %d %s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.MessageId == nil {
		return "", fmt.Errorf("feishu: 发卡片响应缺 message_id")
	}
	return *resp.Data.MessageId, nil
}

// UpdateCard 更新卡片（流式刷新/终态定格——飞书更新卡片接口）。
func (c *realClient) UpdateCard(ctx context.Context, msgID string, card []byte) error {
	req := larkim.NewUpdateMessageReqBuilder().
		MessageId(msgID).
		Body(&larkim.UpdateMessageReqBody{
			MsgType: larkcore.StringPtr("interactive"),
			Content: larkcore.StringPtr(string(card)),
		}).Build()
	resp, err := c.cli.Im.V1.Message.Update(ctx, req)
	if err != nil {
		return fmt.Errorf("feishu: 更新卡片失败: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu: 更新卡片失败: %d %s", resp.Code, resp.Msg)
	}
	return nil
}

// parseMessageEvent 事件 → 入站消息（nil = 忽略：非文本消息/缺字段——图片等
// 富媒体经事件流通道是升级位，v1 只收文本）。
func parseMessageEvent(ev *larkim.P2MessageReceiveV1) *inboundMsg {
	if ev.Event == nil || ev.Event.Message == nil || ev.Event.Sender == nil {
		return nil
	}
	msg := ev.Event.Message
	if msg.MessageType == nil || *msg.MessageType != "text" || msg.Content == nil {
		return nil
	}
	var body struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(*msg.Content), &body) != nil {
		return nil
	}
	m := &inboundMsg{text: stripMentions(body.Text, msg.Mentions)}
	if msg.ChatId != nil {
		m.chatID = *msg.ChatId
	}
	if msg.ChatType != nil {
		m.chatType = *msg.ChatType
	}
	if ev.Event.Sender.SenderId != nil && ev.Event.Sender.SenderId.OpenId != nil {
		m.openID = *ev.Event.Sender.SenderId.OpenId
	}
	if m.chatID == "" || m.openID == "" || m.text == "" {
		return nil
	}
	return m
}

// stripMentions 剥离 @占位符（群聊 @机器人 的 "@_user_1" 等占位标记）。
func stripMentions(text string, mentions []*larkim.MentionEvent) string {
	for _, m := range mentions {
		if m == nil || m.Key == nil {
			continue
		}
		text = strings.ReplaceAll(text, *m.Key, "")
	}
	return strings.TrimSpace(text)
}
