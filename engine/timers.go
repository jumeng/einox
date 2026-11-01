package engine

// 审批/提问挂起超时兜底（自产品 internal/agent/interrupt.go 的 Manager 侧
// 迁入）：到点仍挂起且无决议 → 记 *_timeout 事件 → 终态落盘（不自动 Resume
// 消耗模型轮次；checkpoint 保留——续聊时模型从工具拒绝/未作答反馈中自然知晓）。

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/shortid"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// approvalTimeout 挂起超时（默认 30 分钟；测试可缩短）。
var approvalTimeout = 30 * 60 * time.Second

// ApprovalTimeout 当前超时配置。
func ApprovalTimeout() time.Duration { return approvalTimeout }

// 挂起域短标识（shortid 单点实现——审查 P3-2：rand-hex 生成曾六处各写，
// 前缀语义表分散多包注释）。a=审批 q=提问 p=计划（session 侧另有 s=会话、
// q=队列〔不同名空间已核实无碰撞面〕、hitl 侧 i=项——分配表见 internal/shortid）。
func newApprovalID() string { return shortid.Hex("a", 3) }

func newAskID() string { return shortid.Hex("q", 3) }

func newPlanID() string { return shortid.Hex("p", 3) }

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
		// A2：游离 goroutine 护栏——回调 panic 即进程崩，且挂在「无人值守等
		// 超时」这类最晚暴露的路径上（coze safego.Go 同纪律）。收敛为日志 +
		// 尽力落超时终态（简单字段操作；此处再 panic 属进程级日志已留痕的终
		// 局，不再嵌套防御）。
		defer func() {
			if r := recover(); r != nil {
				log.Printf("einox: 会话 %s 审批超时器回调 panic（已收敛，兜底翻终态）：%v", s.SID, r)
				if !s.Stopped() && s.PendingAppID() == appID {
					s.SetPendingApproval("")
					s.SetState(session.StateEnded)
					m.reg.Persist(s)
				}
			}
		}()
		m.expirePending(s, appID, kind)
	})
	approvalMu.Unlock()
}

// expirePending 挂起超时动作（到点仍挂起且无决议 → 拒绝/取消 + 终态落盘；
// kind=ask 且 AskAutoResolve 在场 → 自动续行替代取消，见 autoResolveAsk）。
// 决议已到达即让位：应用端点（决议登记 → Persist → BeginRun → go Resume）
// 与停表之间存在窗口——Resume 的 stopApprovalTimer 是停表唯一入口，决议
// 登记本身不清挂起态；窗口内到点若照常执行，会把用户刚 approve 的决议覆盖
// 成超时拒绝、把 BeginRun 的 running 改写回 ended。部分到达（批量决议逐项
// 写入的非原子窗口）同样让位——未决项由 Resume 重放 fail-closed 兜底。
func (m *Manager) expirePending(s *session.Session, appID, kind string) {
	if s.Stopped() {
		return // 已删除
	}
	if kind == "ask" && m.autoResolveAsk(s, appID) {
		return // D15 自动续行已接管（决议 CAS 成功 + Resume 已派发）
	}
	// 条件原子清挂起：锁内校验「仍挂起该 appID 且无决议」才清（项清单随清
	// 返回）——闭合「守卫检查→清挂起」两步间的 TOCTOU（用户 approve 与超时
	// 并发时抢到锁者赢，另一方让位；fail-closed 兜底语义不变）。
	items, ok := s.ClearPendingIf(appID)
	if !ok {
		return // 已决议/决议已到达（Resume 将消费）
	}
	if kind == "ask" {
		// 提问超时：不作答即分支取消（Resume 时 Decision 为 nil → fail-closed）
		s.Record(contract.EvAskTimeout, map[string]string{"ask_id": appID, "reason": "提问超时未作答"})
		m.finishOf(s)(session.StateEnded)
		return
	}
	if kind == "plan" {
		// 计划超时：自动拒绝（fail-closed；文档停在 submitted 态——超时不
		// Resume，工具恢复流不跑，续聊由模型从拒绝反馈中知晓）
		s.SetDecision(contract.ApprovalDecision{Approve: false, Reason: "计划审批超时，自动拒绝"})
		s.Record(contract.EvPlanTimeout, map[string]string{"plan_id": appID, "reason": "计划审批超时，自动拒绝"})
		m.finishOf(s)(session.StateEnded)
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
	m.finishOf(s)(session.StateEnded)
}

// autoResolveAsk 提问到点自动续行（D15；Options.AskAutoResolve 在场才有面，
// 返回 true = 已接管本次到点）。判定序（每步失败回落既有取消分支，fail-closed）：
// 回溯挂起 AskReq 载荷 → 闭包求值（nil = 应用侧关闭/已 snooze）→ CAS 登记
// （SetAskDecisionIf：并发人工作答抢先即让位）→ ask_decision 落流（Decider
// 缺省补 "auto"，可审计）→ Persist（决议先落盘再续流，与 Channels().Answer
// 同序）→ go Resume（BeginResume 原子消费挂起态；失败路径与 Answer 同构）。
func (m *Manager) autoResolveAsk(s *session.Session, askID string) bool {
	if m.Opt.AskAutoResolve == nil {
		return false
	}
	req, ok := lastAskReqOf(s, askID)
	if !ok {
		return false // 事件流缺提问载荷（不喂闭包空载荷）
	}
	d := m.Opt.AskAutoResolve(req)
	if d == nil {
		return false // 应用侧开关关闭/该问已 snooze → 取消分支
	}
	if d.DeciderID == "" {
		d.DeciderID, d.DeciderName = "auto", "auto" // Decider=auto（回执可审计）
	}
	if !s.SetAskDecisionIf(askID, *d) {
		return false // 人工作答已抢先到达（让位；Answer 的 Resume 将消费）
	}
	s.RecordAskDecision(askID, *d)
	m.reg.Persist(s)
	go m.Resume(context.Background(), s, noopEmit)
	return true
}

// lastAskReqOf 回溯本次挂起的提问载荷（事件流倒序找 ask_id 匹配的
// ask_user_request）。两种 Data 形态：在役会话 = contract.AskReq 原生；
// 重启 Reattach 后 = JSON 回读的 map 形态（经 marshal/unmarshal 还原）。
func lastAskReqOf(s *session.Session, askID string) (contract.AskReq, bool) {
	evs := s.SnapshotEvents()
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Event != contract.EvAskRequest {
			continue
		}
		switch d := evs[i].Data.(type) {
		case contract.AskReq:
			if d.AskID == askID {
				return d, true
			}
		case map[string]any:
			if id, _ := d["ask_id"].(string); id != askID {
				continue
			}
			raw, err := json.Marshal(d)
			if err != nil {
				continue
			}
			var req contract.AskReq
			if json.Unmarshal(raw, &req) == nil {
				return req, true
			}
		}
	}
	return contract.AskReq{}, false
}

// DefaultAskResolve 提问自动续行的默认决议（上游语义基座化，D15）：
// 决议 = Options[0]（首选项 value；上游「自动选默认答案续行」），无选项取
// 空文本（AllowFreeText 问句的默认答案即空）。应用闭包 = 开关读数 + 本助手
// 两行即得标准行为；需自定义决议形态（如登记差异口径）也可不用本助手。
func DefaultAskResolve(req contract.AskReq) *contract.AskDecision {
	if len(req.Options) > 0 {
		return &contract.AskDecision{Answers: []string{req.Options[0].Value}}
	}
	return &contract.AskDecision{FreeText: ""}
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
		// 回执走 marshal（手拼 JSON 文案带引号即非法——形态与 tools.Fail 同源，审查 P2-11）
		env, _ := json.Marshal(tools.Fail("用户搁置了本次提问（未作答），本轮任务暂停未推进——用户将以新消息继续，请结合后续消息决定下一步"))
		s.AppendHistory(&schema.Message{
			Role: schema.Tool, ToolCallID: callID,
			Content: string(env),
		})
	}
	s.Record(contract.EvAskIgnored, map[string]string{"ask_id": id, "reason": "用户忽略，未作答"})
	m.finishOf(s)(session.StateEnded)
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

// cardOf 从中断事件提取首个指定形态的卡（顶层 Data 直传形态优先，复合
// 中断走根因 InterruptContexts 链）。ask/plan 曾两份逐字同构（审查 P3-12
// 泛型收敛）；approval 侧为多卡聚合（approvalCardsOf），语义不同不并。
func cardOf[T any](ii *adk.InterruptInfo) (T, bool) {
	var zero T
	if ii == nil {
		return zero, false
	}
	if c, ok := ii.Data.(T); ok {
		return c, true
	}
	for _, c := range ii.InterruptContexts {
		if c == nil {
			continue
		}
		if cc, ok := c.Info.(T); ok {
			return cc, true
		}
	}
	return zero, false
}

// askCardOf 从中断事件提取提问卡。
func askCardOf(ii *adk.InterruptInfo) (contract.AskCard, bool) {
	return cardOf[contract.AskCard](ii)
}

// planCardOf 从中断事件提取计划卡。
func planCardOf(ii *adk.InterruptInfo) (contract.PlanCard, bool) {
	return cardOf[contract.PlanCard](ii)
}
