// inbound.go 入站接线：消息事件 → Gateway.Handle（渠道编排面分流）；
// 卡片交互回调 → Gateway.Approve/Answer（挂起续流）。
package feishu

import (
	"errors"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/engine"
)

// handleMsg 消息事件 → 入站分流（空闲起轮/运行中排队——归机制核；错误只记
// 日志级丢弃：飞书侧无回执通道，重试语义由用户重发表达）。
func (b *Bot) handleMsg(m inboundMsg) {
	err := b.gw.Handle(engine.InboundMsg{
		Channel:   b.id,
		Chat:      m.chatID,
		Owner:     m.openID,   // owner = 发送者（渠道账号 ↔ 用户绑定体系在业务层）
		SpeakerID: m.openID,   // T6 发送者身份全链透传（p2p 即 owner；群聊后续发送者不再被丢弃——Name 解析需 OpenAPI，归应用补）
		ChatType:  m.chatType, // T6 会话形态透传（p2p|group——应用装配策略依据）
		Text:      m.text,
	})
	if err != nil {
		// 分流失败如实回执（卡片形态——用户可感知可重发）
		b.cards.sendStandalone(engine.ChannelBrief{Channel: b.id, Chat: m.chatID}, "⚠️ "+err.Error())
	}
}

// handleAction 卡片按钮回调 → 决议回写。value 约定：act=approve|reject|answer、
// sid=会话、val=选项值（answer）。
func (b *Bot) handleAction(a cardAction) {
	sid, _ := a.value["sid"].(string)
	act, _ := a.value["act"].(string)
	val, _ := a.value["val"].(string)
	if sid == "" {
		return
	}
	var decider *contract.Participant // T6：点按钮的人（决议者落回执可审计）
	if a.operator != "" {
		decider = &contract.Participant{ID: a.operator}
	}
	switch act {
	case "approve":
		if err := b.gw.Approve(sid, "", decider, contract.ApprovalDecision{Approve: true}); err != nil {
			b.actionFailure(a, err)
		}
	case "reject":
		if err := b.gw.Approve(sid, "", decider, contract.ApprovalDecision{Approve: false}); err != nil {
			b.actionFailure(a, err)
		}
	case "answer":
		if !b.gw.Answer(sid, decider, contract.AskDecision{FreeText: val}) { // 选项按钮按自由文本作答
			// 迟到/已处理：静默（卡片已被 settlePend 定格，用户可见终态）
		}
	}
}

// actionFailure 决议失败回执：幂等迟到（ErrNoPendingDecision）静默——卡片
// 已定格用户可见终态；其余（DecisionGuard 拒绝等）告警卡可见。
func (b *Bot) actionFailure(a cardAction, err error) {
	if errors.Is(err, engine.ErrNoPendingDecision) || a.chatID == "" {
		return
	}
	b.cards.sendStandalone(engine.ChannelBrief{Channel: b.id, Chat: a.chatID}, "⚠️ "+err.Error())
}
