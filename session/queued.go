package session

// 排队消息域（steering 入队，可编辑/删除——zcode 形态）：Steer/SteerBy 入队、
// 后台完成通知注入（ContinueOrNotify + 自激护栏预算）、排队消息编辑/移除/
// 重排与 TakePending 消费（下一轮 Run 前置带回）。

import (
	"time"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/shortid"
)

// Attachment 用户消息附件（契约形态）。
type Attachment = contract.Attachment

// QueuedMsg 排队消息（契约形态；steering 入队，可编辑/删除）。
type QueuedMsg = contract.QueuedMsg

// newQueueID 排队消息 id（q 前缀——shortid 单点；与 engine 提问 q 不同名空间）。
func newQueueID() string { return shortid.Hex("q", 2) }

// QueueLen 排队消息数（flush 端点空队列守卫）。
func (s *Session) QueueLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingMsgs)
}

// HasQueuedImages 排队消息是否含图片附件（设置通道切模型的守卫：落回轮
// 无图片门禁兜底，纯文本模型 + 含图排队消息会在执行时炸——写入时拒）。
func (s *Session) HasQueuedImages() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.pendingMsgs {
		for _, a := range q.Attachments {
			if a.IsImage {
				return true
			}
		}
	}
	return false
}

// Steer running/pending 中再输入：入队 + 模式即时切换（docs/05）。
// 返回 false = 会话空闲（走正常 Run 而非 steering）。入队落 steer_queued
// 事件——排队态可回放重建（切回/刷新不丢，对齐审批决议回执定案）。
func (s *Session) Steer(msg string, atts []Attachment, mode string) bool {
	return s.SteerBy(nil, msg, atts, mode) // 无说话人——单用户/系统语义零变化
}

// SteerBy 带说话人入队（T6 多参与者：群聊中他人消息排队不丢身份——
// 排队消息与 steer_queued 事件携带 Speaker，运行中注入/下轮前置均署名）。
func (s *Session) SteerBy(actor *contract.Participant, msg string, atts []Attachment, mode string) bool {
	s.mu.Lock()
	if s.State != StateRunning && s.State != StatePendingApproval {
		s.mu.Unlock()
		return false
	}
	s.setModeLocked(mode)
	var q QueuedMsg
	if msg != "" || len(atts) > 0 {
		q = QueuedMsg{ID: newQueueID(), Text: msg, Attachments: atts}
		if actor != nil {
			q.SpeakerID, q.SpeakerName = actor.ID, actor.Name
		}
		s.pendingMsgs = append(s.pendingMsgs, q)
	}
	s.mu.Unlock()
	if q.ID != "" {
		s.Record("steer_queued", contract.SteerEvent{ID: q.ID, Text: q.Text, Attachments: q.Attachments,
			SpeakerID: q.SpeakerID, SpeakerName: q.SpeakerName})
	}
	return true
}

// ContinueOrNotify 后台完成通知的单锁原子注入原语（W-3，形态与竞态依据见
// ContinueOrNotifyKind——kind 固定 "notify" 的委托版）。
func (s *Session) ContinueOrNotify(msg string, wake bool) (began bool, q QueuedMsg) {
	return s.ContinueOrNotifyKind(msg, wake, "notify")
}

// ContinueOrNotifyKind 单锁原子注入原语的 kind 参数化版（GoalDriver 批：
// goal 轮预约注入 Kind="goal"——与 notify 同律「系统注入非人类输入」，注入
// 回执走 EvNotifyQueued/EvNotifyInjected 通道由 payload Kind 区分审计）：
// 锁内一步裁定——running/pending/error → 通知入队（began=false，当前/
// 下一轮 TakePending 消费）；ended（自然收口）+ wake → 翻 running + 挂
// runDone + 通知入队（began=true，调用方锁外 Record 后以
// Run("") 起自续轮——通知经队列注入，输入路径与 steering 唯一）；
// ended + !wake → 只入队（不自动开轮，下轮用户交互消费）。
// **error 终态绝不自续**（对照审查 A-3：用户停止把中断洗成模型请求是被
// 明确拒绝的形态；运行错误同理保守——通知只入队，下轮用户交互消费）。
// 竞态封闭依据：与 BeginRun/Steer 同锁互斥，TOCTOU 丢通知/丢启动窗口不
// 存在（单锁原语义，对照审查定案）。返回的 q 供锁外 Record（Record 自持
// 锁，锁内不可调）。
func (s *Session) ContinueOrNotifyKind(msg string, wake bool, kind string) (began bool, q QueuedMsg) {
	s.mu.Lock()
	q = QueuedMsg{ID: newQueueID(), Text: msg, Kind: kind}
	if s.State != StateEnded {
		s.pendingMsgs = append(s.pendingMsgs, q)
		s.mu.Unlock()
		return false, q
	}
	if !wake {
		s.pendingMsgs = append(s.pendingMsgs, q)
		s.mu.Unlock()
		return false, q
	}
	// ended：抢占执行体（与 BeginRun 同型三步——查态+翻 running+挂 runDone）
	s.State = StateRunning
	s.runDone = make(chan struct{})
	s.UpdatedAt = time.Now()
	s.pendingMsgs = append(s.pendingMsgs, q)
	s.mu.Unlock()
	return true, q
}

// NotifyBudget 自激护栏预算（W-3）：budget = 连续自续上限。true = 还有预算
// （消费一格）；false = 已耗尽（调用方降级仅记事件）。用户消息消费恢复预算
// （RestoreNotifyBudget——通知自身不恢复，dsh spentWakes 同款）。
func (s *Session) NotifyBudget(budget int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notifySpent >= budget {
		return false
	}
	s.notifySpent++
	return true
}

// RestoreNotifyBudget 用户输入到达本轮输入（Run/Resume 头部消费到非 notify
// 条目）时恢复全部自续预算。
func (s *Session) RestoreNotifyBudget() {
	s.mu.Lock()
	s.notifySpent = 0
	s.mu.Unlock()
}

// queuedOwnerFenced 条目围栏判定（C6 队列变异所有权，设计件 §2.1）：署名
// 条目（SpeakerID 非空）遇带身份 editor（非空）时，仅本人或会话 Owner 可变。
// editor 空 = 匿名控制面（单用户/系统路径零变化）；匿名条目不设栏。调用方
// 持锁。
func queuedOwnerFenced(entry QueuedMsg, editor, owner string) bool {
	if entry.SpeakerID == "" || editor == "" {
		return false
	}
	return editor != entry.SpeakerID && editor != owner
}

// EditQueued 编辑排队消息（false = 不存在、系统通知只读或所有权围栏拒）；
// 落 steer_updated 回执。editor = 控制面身份（空 = 匿名/单用户零变化）。
func (s *Session) EditQueued(editor, id, text string) bool {
	s.mu.Lock()
	ok := false
	for i := range s.pendingMsgs {
		if s.pendingMsgs[i].ID != id {
			continue
		}
		if s.pendingMsgs[i].Kind == "notify" {
			s.mu.Unlock()
			return false // 系统通知只读（后台子代理结论不可编辑）
		}
		if queuedOwnerFenced(s.pendingMsgs[i], editor, s.Owner) {
			s.mu.Unlock()
			return false // C6：署名条目他人不可改（本人/Owner 例外）
		}
		s.pendingMsgs[i].Text = text
		ok = true
		break
	}
	s.mu.Unlock()
	if ok {
		s.Record("steer_updated", contract.SteerEvent{ID: id, Text: text})
	}
	return ok
}

// RemoveQueued 移除排队消息（false = 不存在、系统通知不可删或所有权围栏拒）；
// 落 steer_removed 回执。editor 语义同 EditQueued。
func (s *Session) RemoveQueued(editor, id string) bool {
	s.mu.Lock()
	ok := false
	for i := range s.pendingMsgs {
		if s.pendingMsgs[i].ID != id {
			continue
		}
		if s.pendingMsgs[i].Kind != "" {
			s.mu.Unlock()
			return false // 系统注入不可删（结论注入是模型面承诺）
		}
		if queuedOwnerFenced(s.pendingMsgs[i], editor, s.Owner) {
			s.mu.Unlock()
			return false // C6：署名条目他人不可删（本人/Owner 例外）
		}
		s.pendingMsgs = append(s.pendingMsgs[:i], s.pendingMsgs[i+1:]...)
		ok = true
		break
	}
	s.mu.Unlock()
	if ok {
		s.Record("steer_removed", contract.SteerEvent{ID: id})
	}
	return ok
}

// ReorderQueued 排队消息重排（UI-B3 拖拽排序——ids = 期望顺序的完整清单；
// 集合与现存不一致整体拒（多/少/未知 id，防并发丢消息）；成功落
// steer_reordered 回执——回放重建顺序的真源）。
func (s *Session) ReorderQueued(ids []string) bool {
	s.mu.Lock()
	if len(ids) != len(s.pendingMsgs) {
		s.mu.Unlock()
		return false
	}
	pos := make(map[string]int, len(s.pendingMsgs))
	for i, q := range s.pendingMsgs {
		pos[q.ID] = i
	}
	ordered := make([]QueuedMsg, 0, len(ids))
	for _, id := range ids {
		p, ok := pos[id]
		if !ok {
			s.mu.Unlock()
			return false
		}
		ordered = append(ordered, s.pendingMsgs[p])
	}
	s.pendingMsgs = ordered
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
	s.Record(contract.EvSteerReordered, contract.SteerReorder{IDs: ids})
	return true
}

// TakePending 取走排队消息（Run 前置带回——排队兜底路径）。
func (s *Session) TakePending() []QueuedMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.pendingMsgs
	s.pendingMsgs = nil
	return msgs
}
