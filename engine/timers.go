package engine

// 审批/提问挂起超时兜底（自产品 internal/agent/interrupt.go 的 Manager 侧
// 迁入）：到点仍挂起且无决议 → 记 *_timeout 事件 → 终态落盘（不自动 Resume
// 消耗模型轮次；checkpoint 保留——续聊时模型从工具拒绝/未作答反馈中自然知晓）。

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
)

// approvalTimeout 挂起超时（默认 30 分钟；测试可缩短）。
var approvalTimeout = 30 * 60 * time.Second

// ApprovalTimeout 当前超时配置。
func ApprovalTimeout() time.Duration { return approvalTimeout }

// newApprovalID 审批标识（a 前缀 + 6 hex）。
func newApprovalID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "a" + hex.EncodeToString(b)
}

// newAskID 提问标识（q 前缀 + 6 hex；与审批 a 前缀区分）。
func newAskID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "q" + hex.EncodeToString(b)
}

// newPlanID 计划标识（p 前缀 + 6 hex；与审批 a/提问 q 前缀区分）。
func newPlanID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "p" + hex.EncodeToString(b)
}

// approvalTimerKey 按 sid 的超时计时器登记（重复挂起替换旧计时器）。
var (
	approvalMu     sync.Mutex
	approvalTimers = map[string]*time.Timer{}
)

// startApprovalTimer 挂起超时兜底（kind: "approval" 审批 | "ask" 提问 |
// "plan" 计划审批）。emit 走仅落库分支（无活跃流——挂起期流已收线）。
func (m *Manager) startApprovalTimer(s *session.Session, appID string, timeoutAt time.Time, kind string) {
	s.SetPendingDue(kind, timeoutAt) // 元数据先落 Session——挂起态随 finishOf 落盘，跨重启续表依据
	delay := time.Until(timeoutAt)
	if delay < 0 {
		delay = 0
	}
	approvalMu.Lock()
	if old, ok := approvalTimers[s.SID]; ok {
		old.Stop()
	}
	approvalTimers[s.SID] = time.AfterFunc(delay, func() {
		approvalMu.Lock()
		delete(approvalTimers, s.SID)
		approvalMu.Unlock()
		m.expirePending(s, appID, kind)
	})
	approvalMu.Unlock()
}

// expirePending 挂起超时动作（到点仍挂起且无决议 → 拒绝/取消 + 终态落盘）。
// 决议已到达即让位：应用端点（决议登记 → Persist → BeginRun → go Resume）
// 与停表之间存在窗口——Resume 的 stopApprovalTimer 是停表唯一入口，决议
// 登记本身不清挂起态；窗口内到点若照常执行，会把用户刚 approve 的决议覆盖
// 成超时拒绝、把 BeginRun 的 running 改写回 ended。部分到达（批量决议逐项
// 写入的非原子窗口）同样让位——未决项由 Resume 重放 fail-closed 兜底。
func (m *Manager) expirePending(s *session.Session, appID, kind string) {
	if s.Stopped() || s.PendingAppID() != appID || s.HasPendingDecision() {
		return // 已决议/已删除/决议已到达（Resume 将消费）
	}
	// 项清单先取：SetPendingApproval("") 会一并清空 pendingItems——先清后取
	// 令合并卡逐项落拒从未生效（实落 "" 槽，重放各工具按 item_id 领不到决议，
	// 走的是「无决议到达」文案的 fail-closed——语义偏差，实测暴露）。
	items := s.PendingItems()
	s.SetPendingApproval("")
	if kind == "ask" {
		// 提问超时：不作答即分支取消（Resume 时 Decision 为 nil → fail-closed）
		s.Record(contract.EvAskTimeout, map[string]string{"ask_id": appID, "reason": "提问超时未作答"})
		s.SetState(session.StateEnded)
		m.reg.Persist(s)
		return
	}
	if kind == "plan" {
		// 计划超时：自动拒绝（fail-closed；文档停在 submitted 态——超时不
		// Resume，工具恢复流不跑，续聊由模型从拒绝反馈中知晓）
		s.SetDecision(contract.ApprovalDecision{Approve: false, Reason: "计划审批超时，自动拒绝"})
		s.Record(contract.EvPlanTimeout, map[string]string{"plan_id": appID, "reason": "计划审批超时，自动拒绝"})
		s.SetState(session.StateEnded)
		m.reg.Persist(s)
		return
	}
	// 审批超时：自动拒绝（fail-closed）。合并决议卡 = 全项拒绝（项清单
	// 逐项落拒——Resume 重放时各工具按 item_id 领到拒绝）；旧单卡/无项
	// 清单 = "" 槽单决议拒绝（既有语义不变）。
	if len(items) > 0 {
		for _, id := range items {
			s.SetDecisionFor(id, contract.ApprovalDecision{Approve: false, Reason: "审批超时，自动拒绝"})
		}
	} else {
		s.SetDecision(contract.ApprovalDecision{Approve: false, Reason: "审批超时，自动拒绝"})
	}
	s.Record(contract.EvApprovalTimeout, map[string]string{"approval_id": appID, "reason": "审批超时，自动拒绝"})
	s.SetState(session.StateEnded)
	m.reg.Persist(s)
}

// stopApprovalTimer 决议到达即停表（approve/reject 抢先于超时）。
func stopApprovalTimer(sid string) {
	approvalMu.Lock()
	if t, ok := approvalTimers[sid]; ok {
		t.Stop()
		delete(approvalTimers, sid)
	}
	approvalMu.Unlock()
}

// IgnoreAsk 用户忽略挂起提问（ask 卡「忽略」按钮）：停表（超时兜底随
// 之取消）+ 悬空调用补搁置回执闭环历史 + 记事件 + ended 终态落盘。不
// Resume——任务停在本步，用户下一次发消息开新轮唤醒（排队消息随新轮
// 注入）。与超时分支的刻意差异：补配对 tool 消息而非留悬空待
// sanitizeHistory 剥离——唤醒轮模型记得问过什么，新消息语境才能衔接；
// 且回执随 Persist 落盘，跨重启不失（超时的内存决议重启即丢）。
func (m *Manager) IgnoreAsk(s *session.Session) {
	id := s.PendingAppID()
	if id == "" {
		return // 无挂起（幂等空操作；入口守卫在应用端点）
	}
	stopApprovalTimer(s.SID)
	s.SetPendingApproval("")
	for _, callID := range danglingToolCalls(s.CloneHistory()) {
		s.AppendHistory(&schema.Message{
			Role: schema.Tool, ToolCallID: callID,
			Content: `{"ok":false,"error":"用户搁置了本次提问（未作答），本轮任务暂停未推进——用户将以新消息继续，请结合后续消息决定下一步"}`,
		})
	}
	s.Record(contract.EvAskIgnored, map[string]string{"ask_id": id, "reason": "用户忽略，未作答"})
	s.SetState(session.StateEnded)
	m.reg.Persist(s)
}

// danglingToolCalls 挂起轮悬空 tool_call 定位（assistant 带 tool_calls 但
// 无配对 tool 消息——挂起时本轮段已入史、结果未跟）。从尾部回溯首条带
// 调用的 assistant 即挂起段（挂起后无人续史），按声明顺序返回未应答 ID。
func danglingToolCalls(msgs []*schema.Message) []string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != schema.Assistant || len(m.ToolCalls) == 0 {
			continue
		}
		answered := map[string]bool{}
		for j := i + 1; j < len(msgs) && msgs[j].Role == schema.Tool; j++ {
			answered[msgs[j].ToolCallID] = true
		}
		var ids []string
		for _, tc := range m.ToolCalls {
			if !answered[tc.ID] {
				ids = append(ids, tc.ID)
			}
		}
		return ids
	}
	return nil
}

// RearmPendingTimer 重启后续表（Reattach 恢复挂起态后调用）：进程重启丢内存
// 计时器，不续则挂起永无超时兜底（排队消息卡死、列表常显等待确认）。已过点
// 即时触发（AfterFunc 负延迟归 0——镜像停机期间到点的超时动作）。
func (m *Manager) RearmPendingTimer(s *session.Session) {
	id := s.PendingAppID()
	kind, due := s.PendingDueOf()
	if id == "" || due.IsZero() {
		return
	}
	m.startApprovalTimer(s, id, due, kind)
}

// approvalCardsOf 从中断事件提取全部审批卡（H4-2 合并决议：一轮并行写调用
// 中断聚合为一个 InterruptInfo 多 InterruptContexts——逐上下文收卡，泵侧
// 一卡 N 项发事件；适配层把 Suspend.Info 直传为中断 info，工具中断经
// ToolsNode 包装为 Composite——info 在根因 InterruptCtx 上，顶层 Data 为
// 复合自身 info 通常空）。
func approvalCardsOf(ii *adk.InterruptInfo) []contract.ApprovalCard {
	if ii == nil {
		return nil
	}
	var out []contract.ApprovalCard
	if c, ok := ii.Data.(contract.ApprovalCard); ok {
		out = append(out, c)
	}
	for _, c := range ii.InterruptContexts {
		if c == nil {
			continue
		}
		if cc, ok := c.Info.(contract.ApprovalCard); ok {
			out = append(out, cc)
		}
	}
	return out
}

// approvalCardOf 从中断事件提取首张审批卡（旧单卡语义——ask/plan 分支与
// 兼容路径用）。
func approvalCardOf(ii *adk.InterruptInfo) (contract.ApprovalCard, bool) {
	if cards := approvalCardsOf(ii); len(cards) > 0 {
		return cards[0], true
	}
	return contract.ApprovalCard{}, false
}

// askCardOf 从中断事件提取提问卡（与 approvalCardOf 同构）。
func askCardOf(ii *adk.InterruptInfo) (contract.AskCard, bool) {
	if ii == nil {
		return contract.AskCard{}, false
	}
	if c, ok := ii.Data.(contract.AskCard); ok {
		return c, true
	}
	for _, c := range ii.InterruptContexts {
		if c == nil {
			continue
		}
		if cc, ok := c.Info.(contract.AskCard); ok {
			return cc, true
		}
	}
	return contract.AskCard{}, false
}

// planCardOf 从中断事件提取计划卡（与 approvalCardOf 同构）。
func planCardOf(ii *adk.InterruptInfo) (contract.PlanCard, bool) {
	if ii == nil {
		return contract.PlanCard{}, false
	}
	if c, ok := ii.Data.(contract.PlanCard); ok {
		return c, true
	}
	for _, c := range ii.InterruptContexts {
		if c == nil {
			continue
		}
		if cc, ok := c.Info.(contract.PlanCard); ok {
			return cc, true
		}
	}
	return contract.PlanCard{}, false
}
