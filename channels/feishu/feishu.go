// Package feishu 飞书机器人渠道（官方通用件——channel 三层的第②层，见
// docs/04）：长连接接收消息/卡片回调，出站事件流渲染为飞书卡片（文本增量
// 聚合节流更新）。编排机制全走 engine.ChannelGateway（绑定/分流/挂起续流/
// 补投），本包只做协议与渲染。装配两段式：New 建出站 Sink（进
// Options.Channels）→ Start(m) 绑网关并起长连接。
package feishu

import (
	"context"
	"sync"

	"github.com/jumeng/einox/engine"
)

// Config 飞书渠道配置（开放平台应用凭证——事件订阅须选长连接模式）。
type Config struct {
	AppID     string
	AppSecret string
	// Model 渠道会话缺省模型复合键（provider/model，透传 engine.ChannelConfig）。
	Model string
	// ID 渠道路由键（缺省 "feishu"；多机器人实例时自定义）。
	ID string
}

// client SDK 边界（真实实现见 client.go；测试假实现注入）。
type client interface {
	// SendCard 发卡片消息（返回消息 ID——流式更新的锚点）。
	SendCard(ctx context.Context, chatID string, card []byte) (string, error)
	// UpdateCard 更新卡片内容（流式刷新/终态定格）。
	UpdateCard(ctx context.Context, msgID string, card []byte) error
	// Run 长连接事件循环（阻塞；ctx 取消即收线）。
	Run(ctx context.Context) error
}

// Bot 飞书机器人渠道。实现 engine.ChannelSink（出站）；入站经 client 事件
// 回调进 handleMsg/handleAction（见 inbound.go）。
type Bot struct {
	id   string
	cfg  Config
	cli  client
	gw   *engine.ChannelGateway
	cards cardHub

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New 构造（纯内存——不连网；返回值即出站 Sink，进 engine.Options.Channels）。
// NewManager 须带 Channels: []engine.ChannelConfig{{ID: b.ID(), Model: cfg.Model,
// Sink: b}}，随后 Start 绑网关。
func New(cfg Config) *Bot {
	id := cfg.ID
	if id == "" {
		id = "feishu"
	}
	b := &Bot{id: id, cfg: cfg}
	b.cards.init()
	return b
}

// ID 渠道路由键（engine.InboundMsg.Channel 匹配值）。
func (b *Bot) ID() string { return b.id }

// Start 绑网关并起长连接（真实实现阻塞在事件循环——放 goroutine；客户端
// 构造失败如实返回）。
func (b *Bot) Start(m *engine.Manager) error {
	b.gw = m.Channels()
	cli, err := newClient(b.cfg, b.handleMsg, b.handleAction)
	if err != nil {
		return err
	}
	b.cli = cli
	ctx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		_ = b.cli.Run(ctx) // 收线/断连重连归 SDK；此处退出即停机
	}()
	return nil
}

// Close 收线（停长连接与卡片节流泵）。
func (b *Bot) Close() {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
	b.cards.close()
}
