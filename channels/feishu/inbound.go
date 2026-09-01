// inbound.go 入站接线：消息事件 → Gateway.Handle（渠道编排面分流）；
// 卡片交互回调 → Gateway.Approve/Answer（挂起续流）。
package feishu

import (
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/engine"
)

// handleMsg 消息事件 → 入站分流（空闲起轮/运行中排队——归机制核；错误只记
// 日志级丢弃：飞书侧无回执通道，重试语义由用户重发表达）。
func (b *Bot) handleMsg(m inboundMsg) {
	err := b.gw.Handle(engine.InboundMsg{
		Channel: b.id,
		Chat:    m.chatID,
		Owner:   m.openID, // owner = 发送者（渠道账号 ↔ 用户绑定体系在业务层）
		Text:    m.text,
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
	var ok bool
	switch act {
	case "approve":
		ok = b.gw.Approve(sid, "", contract.ApprovalDecision{Approve: true})
	case "reject":
		ok = b.gw.Approve(sid, "", contract.ApprovalDecision{Approve: false})
	case "answer":
		ok = b.gw.Answer(sid, contract.AskDecision{FreeText: val}) // 选项按钮按自由文本作答
	}
	if !ok {
		// 迟到/已处理：静默（卡片已被 settlePend 定格，用户可见终态）
		_ = ok
	}
}
