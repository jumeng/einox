// outbound.go 出站渲染：事件流 → 飞书卡片。每轮一张会话主卡（用户消息 +
// 助手文本聚合 + 工具行；text_delta 聚合节流更新——飞书卡片接口有频控，
// 增量直发必触限）；挂起交互独立卡片（审批/计划：批准/拒绝按钮；提问：
// 选项按钮），决议/超时事件到达即定格终态。
package feishu

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/engine"
	"github.com/jumeng/einox/session"
)

// cardHub 出站卡片状态机（per-chat 会话卡 + per-sid 挂起卡）。
type cardHub struct {
	mu     sync.Mutex
	chats  map[string]*chatCards // key = chatID
	pends  map[string]*pendCard  // key = sid（挂起域单值——一会话同时至多一张挂起卡）
	cli    client
	ctx    context.Context
	cancel context.CancelFunc // ctx 收线面：close 时取消，挂住的 Send/Update 尽快返回（审查 P2-12）

	flushCh   chan struct{} // 节流泵唤醒信号（永不 close——滞留泵迟到发送落空于缓冲即可）
	done      chan struct{} // 关闭信号（收线路径，独立于 flushCh——杜绝 send-on-closed）
	closeOnce sync.Once     // close 幂等守卫（Bot.Close 可能被应用多次调用）
	wg        sync.WaitGroup
}

// chatCards 单会话出站态（主卡聚合 + 节流）。sendMu 串行化建卡序列
// （读 mainID → SendCard → 写 mainID）——flushLoop 与终稿 flush 可并发触达
// 同一 chat，无锁窗口内双读 mainID=="" 即双发卡（安全审查 2026-09-06）；
// per-chat 持锁跨 IO 不阻塞其他会话（h.mu 不参与，SDK 15s 超时兜上界）。
type chatCards struct {
	mainID      string
	userText    string
	userSpeaker string // T6 当轮说话人展示名（空 = 「用户」——单用户零变化）
	text        strings.Builder
	tools       []string
	dirty       bool
	sendMu      sync.Mutex
}

// pendCard 挂起交互卡（sid 维度；决议/超时定格）。
type pendCard struct {
	msgID string
	kind  string // approval | plan | ask
	title string
}

func (h *cardHub) init() {
	h.chats = map[string]*chatCards{}
	h.pends = map[string]*pendCard{}
	// 可取消 ctx（审查 P2-12）：原 Background 永不取消，Send/Update 挂住时
	// flushLoop 卡在推送、close 的 wg.Wait() 无界——Bot.Close 可永久阻塞。
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.flushCh = make(chan struct{}, 1)
	h.done = make(chan struct{})
	h.wg.Add(1)
	go h.flushLoop()
}

// close 收线（审查 P2-12）：只关 done + 取消 ctx，flushCh 永不 close——网关泵
// 与 Bot.Close 不是同一实体（网关 Close 超时不保证泵已停），滞留泵的
// wakeFlush 仍会发送，close(flushCh) 即 send-on-closed panic。done 语义是
// 「泵该退出了」，flushCh 只是唤醒信号，二者解耦后迟到发送至多落空于缓冲。
func (h *cardHub) close() {
	h.closeOnce.Do(func() {
		close(h.done)
		h.cancel()
	})
	h.wg.Wait()
}

// Deliver engine.ChannelSink 出站投递（渠道编排面调用；单会话串行）。
func (h *cardHub) Deliver(b engine.ChannelBrief, ev session.Event) {
	if h.cli == nil {
		return // Start 前（测试装配后不会出现——防御）
	}
	switch ev.Event {
	case contract.EvUserMessage:
		if m, ok := session.EventAs[contract.UserMsg](ev); ok { // 双形态取值（盘面回放 map 亦达）
			h.newTurn(b, m.Text, m.SpeakerName) // T6 说话人随轮入卡
		}
	case contract.EvTextDelta:
		if d, ok := session.EventAs[contract.Delta](ev); ok {
			h.appendText(b, d.Delta)
		}
	case contract.EvToolCall:
		if tc, ok := session.EventAs[contract.ToolCall](ev); ok {
			h.appendTool(b, "🔧 "+tc.Tool)
		}
	case contract.EvToolResult:
		if tr, ok := session.EventAs[contract.ToolResult](ev); ok {
			h.appendTool(b, "↳ "+tr.Digest)
		}
	case contract.EvApprovalRequest, contract.EvPlanRequest:
		h.sendPendCard(b, ev, contract.EvApprovalRequest == ev.Event)
	case contract.EvAskRequest:
		h.sendAskCard(b, ev)
	case contract.EvApprovalDecision, contract.EvPlanDecision, contract.EvAskDecision:
		h.settlePend(b, ev, "已收到答复 ✅")
	case contract.EvApprovalTimeout, contract.EvPlanTimeout, contract.EvAskTimeout, contract.EvAskIgnored:
		h.settlePend(b, ev, "超时自动处理 ⏱")
	case contract.EvSessionEnd:
		h.flush(b)
		h.evictChat(b.Chat) // 轮末淘汰会话卡态（防 chats 表无界增长）
	case contract.EvError:
		if e, ok := session.EventAs[contract.ErrorOut](ev); ok {
			h.appendTool(b, "❌ "+e.Code+"："+e.Message)
			h.flush(b)
		}
	case contract.EvHarnessNote:
		if n, ok := session.EventAs[contract.HarnessNote](ev); ok && n.Kind == "channel_push" {
			h.sendStandalone(b, "📢 "+n.Title)
		}
	}
}

// Deliver engine.ChannelSink 出站投递（Bot 委托卡片状态机——装配面只见 Bot）。
func (b *Bot) Deliver(br engine.ChannelBrief, ev session.Event) { b.cards.Deliver(br, ev) }

// —— 会话主卡 ——

func (h *cardHub) cardsOf(chatID string) *chatCards {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.chats[chatID]
	if !ok {
		c = &chatCards{}
		h.chats[chatID] = c
	}
	return c
}

// newTurn 新轮新卡（上一轮已定格，本轮用户消息开新卡）。
func (h *cardHub) newTurn(b engine.ChannelBrief, userText, speaker string) {
	c := h.cardsOf(b.Chat)
	h.mu.Lock()
	c.userText, c.userSpeaker, c.tools = userText, speaker, nil
	c.text.Reset()
	c.mainID = ""
	c.dirty = true
	h.mu.Unlock()
	h.wakeFlush()
}

func (h *cardHub) appendText(b engine.ChannelBrief, delta string) {
	if delta == "" {
		return
	}
	c := h.cardsOf(b.Chat)
	h.mu.Lock()
	c.text.WriteString(delta)
	c.dirty = true
	h.mu.Unlock()
	h.wakeFlush()
}

func (h *cardHub) appendTool(b engine.ChannelBrief, line string) {
	c := h.cardsOf(b.Chat)
	h.mu.Lock()
	c.tools = append(c.tools, line)
	c.dirty = true
	h.mu.Unlock()
	h.wakeFlush()
}

// render 主卡渲染（markdown：用户消息引用块 + 助手聚合 + 工具行）。
func (c *chatCards) render() []byte {
	var b strings.Builder
	if c.userText != "" {
		speaker := c.userSpeaker // T6 说话人措辞（空 = 单用户零变化）
		if speaker == "" {
			speaker = "用户"
		}
		b.WriteString("> **" + speaker + "**：" + c.userText + "\n\n")
	}
	if t := c.text.String(); t != "" {
		b.WriteString(t + "\n")
	}
	for _, line := range c.tools {
		b.WriteString(line + "\n")
	}
	if b.Len() == 0 {
		b.WriteString("…")
	}
	return simpleCard(b.String())
}

// flushLoop 节流泵：脏卡聚合 400ms 批量更新（卡片接口频控友好；session_end
// 的终稿 flush 直发不等节拍）。done 即收线（审查 P2-12）：待刷卡片直接放弃
// ——终稿已由 session_end 的 flush 定格，残余增量无补于卡面；强刷反而拖着已
// 取消的 ctx 多等一轮，收线应尽快让 wg.Wait 返回。
func (h *cardHub) flushLoop() {
	defer h.wg.Done()
	for {
		select {
		case <-h.done:
			return
		case <-h.flushCh:
			time.Sleep(400 * time.Millisecond)
			h.mu.Lock()
			dirty := map[string]*chatCards{}
			for chat, c := range h.chats {
				if c.dirty {
					dirty[chat] = c
					c.dirty = false
				}
			}
			cli := h.cli
			h.mu.Unlock()
			for chat, c := range dirty {
				h.pushMain(cli, chat, c)
			}
		}
	}
}

// wakeFlush 非阻塞唤醒（审查 P2-12）：先看 done——已收线直接丢弃不再投递；
// flushCh 从不 close，与 done 的检查间即使有竞态，迟到发送也只可能落空于
// 缓冲（cap 1 + default），不可能 panic。
func (h *cardHub) wakeFlush() {
	select {
	case <-h.done:
		return
	default:
	}
	select {
	case h.flushCh <- struct{}{}:
	default:
	}
}

// pushMain 主卡落发（无卡先建、有卡更新；建卡序列 per-chat 串行——见
// chatCards.sendMu）。
func (h *cardHub) pushMain(cli client, chatID string, c *chatCards) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	h.mu.Lock()
	card, has := c.render(), c.mainID != ""
	h.mu.Unlock()
	if !has {
		id, err := cli.SendCard(h.ctx, chatID, card)
		if err == nil {
			h.mu.Lock()
			c.mainID = id
			h.mu.Unlock()
		}
		return
	}
	h.updateMain(cli, c, card)
}

func (h *cardHub) updateMain(cli client, c *chatCards, card []byte) {
	h.mu.Lock()
	id := c.mainID
	h.mu.Unlock()
	if id != "" {
		_ = cli.UpdateCard(h.ctx, id, card)
	}
}

// flush 立即定格（终稿直发，不等节拍；建卡序列 per-chat 串行——见
// chatCards.sendMu）。
func (h *cardHub) flush(b engine.ChannelBrief) {
	c := h.cardsOf(b.Chat)
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	h.mu.Lock()
	c.dirty = false
	cli := h.cli
	card, id := c.render(), c.mainID
	h.mu.Unlock()
	if id == "" {
		if nid, err := cli.SendCard(h.ctx, b.Chat, card); err == nil {
			h.mu.Lock()
			c.mainID = nid
			h.mu.Unlock()
		}
		return
	}
	_ = cli.UpdateCard(h.ctx, id, card)
}

// evictChat 轮末淘汰主卡会话态（安全审查 2026-09-06：chats 表曾只增不减，
// 群聊形态下大量不同 chatID 触达即缓慢泄漏；下一轮 newTurn 经 cardsOf 重建
// ——挂起的淘汰不与在途发送竞态：flush 已同步返回、sendMu 保证序列闭合）。
func (h *cardHub) evictChat(chatID string) {
	h.mu.Lock()
	delete(h.chats, chatID)
	h.mu.Unlock()
}

// —— 挂起交互卡 ——

// sendPendCard 审批/计划卡（批准/拒绝按钮——v1 全批/全拒单决议；合并决议卡
// 逐项审阅是升级位，value 已带 itemID 字段位）。
func (h *cardHub) sendPendCard(b engine.ChannelBrief, ev session.Event, approval bool) {
	title, body := "🔧 审批请求", ""
	if approval {
		if req, ok := session.EventAs[contract.ApprovalReq](ev); ok {
			title = "🔧 审批请求：" + req.Tool
			if req.Diff != "" {
				body = req.Diff
			} else if len(req.Plan) > 0 {
				var lines []string
				for _, p := range req.Plan {
					lines = append(lines, "- "+p.Action+" "+p.Summary)
				}
				body = strings.Join(lines, "\n")
			}
			if len(req.Items) > 1 {
				body = "本轮共 " + itoa(len(req.Items)) + " 项写操作（批准即全部执行）\n" + body
			}
		}
	} else if req, ok := session.EventAs[contract.PlanReq](ev); ok {
		title = "📋 计划请求：" + req.Task
		var lines []string
		for _, s := range req.Steps {
			lines = append(lines, "- "+s.Title)
		}
		body = strings.Join(lines, "\n")
	}
	card := actionCard(title, body, b.SID, []actionBtn{
		{Label: "批准", Type: "primary", Act: "approve"},
		{Label: "拒绝", Type: "danger", Act: "reject"},
	})
	if id, err := h.cli.SendCard(h.ctx, b.Chat, card); err == nil {
		h.mu.Lock()
		h.pends[b.SID] = &pendCard{msgID: id, kind: "approval", title: title}
		h.mu.Unlock()
	}
}

// sendAskCard 提问卡（选项按钮；AllowFreeText 提示直接回复文字——文字语义
// 解析归适配升级位，v1 走排队注入。「文字作答」按钮已撤（安全审查
// 2026-09-06：无 Value 按钮经 answer 分支提交空 FreeText——立即 settle 提问
// 并 Resume，空作答落流、真正的文字回复反成下一轮——按钮废掉了它承诺的
// 路径；提示文案替代）。
func (h *cardHub) sendAskCard(b engine.ChannelBrief, ev session.Event) {
	req, ok := session.EventAs[contract.AskReq](ev)
	if !ok {
		return
	}
	btns := make([]actionBtn, 0, len(req.Options))
	for _, opt := range req.Options {
		btns = append(btns, actionBtn{Label: opt.Label, Type: "default", Act: "answer", Value: firstOr(opt.Value, opt.Label)})
	}
	body := ""
	if req.AllowFreeText {
		body = "提示：可直接回复文字作答（文字答复经排队注入，随下一轮送达）"
	}
	card := actionCard("❓ "+req.Question, body, b.SID, btns)
	if id, err := h.cli.SendCard(h.ctx, b.Chat, card); err == nil {
		h.mu.Lock()
		h.pends[b.SID] = &pendCard{msgID: id, kind: "ask", title: "❓ " + req.Question}
		h.mu.Unlock()
	}
}

// settlePend 挂起卡定格（决议/超时改写卡面，按钮失效由内容替换表达）。
func (h *cardHub) settlePend(b engine.ChannelBrief, _ session.Event, note string) {
	h.mu.Lock()
	p, ok := h.pends[b.SID]
	delete(h.pends, b.SID)
	cli := h.cli
	h.mu.Unlock()
	if ok {
		_ = cli.UpdateCard(h.ctx, p.msgID, simpleCard("**"+p.title+"**\n\n"+note))
	}
}

// sendStandalone 独立通知卡（channel_push 等）。cli 未接（Start 前回调 /
// 构造失败路径）静默跳过——nil 接口调用会 panic 且发生在 SDK 事件 goroutine。
func (h *cardHub) sendStandalone(b engine.ChannelBrief, text string) {
	if h.cli == nil {
		return
	}
	_, _ = h.cli.SendCard(h.ctx, b.Chat, simpleCard(text))
}

// —— 卡片 JSON ——

type actionBtn struct {
	Label string
	Type  string // primary | danger | default
	Act   string // approve | reject | answer
	Value string // 选项值（answer）
}

func itoa(n int) string { return strconv.Itoa(n) }

func firstOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// simpleCard 纯文本卡片。
func simpleCard(md string) []byte {
	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"elements": []any{map[string]any{"tag": "markdown", "content": md}},
	}
	out, _ := json.Marshal(card)
	return out
}

// actionCard 标题 + 说明 + 按钮卡（value 携带 sid/动作——回调路由键）。
func actionCard(title, body, sid string, btns []actionBtn) []byte {
	elements := []any{map[string]any{"tag": "markdown",
		"content": "**" + title + "**" + ternaryStr(body != "", "\n\n"+body, "")}}
	if len(btns) > 0 {
		actions := make([]any, 0, len(btns))
		for _, b := range btns {
			value := map[string]any{"act": b.Act, "sid": sid}
			if b.Value != "" {
				value["val"] = b.Value
			}
			actions = append(actions, map[string]any{
				"tag":   "button",
				"text":  map[string]any{"tag": "plain_text", "content": b.Label},
				"type":  b.Type,
				"value": value,
			})
		}
		elements = append(elements, map[string]any{"tag": "action", "actions": actions})
	}
	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"elements": elements,
	}
	out, _ := json.Marshal(card)
	return out
}

func ternaryStr(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
