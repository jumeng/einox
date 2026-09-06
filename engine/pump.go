package engine

// 流翻译泵（Run/Resume 共用）：迭代 runner 事件 → 契约事件分类——轮内输出
// 累积（runAccum 流式拼装）、单事件分类（handleOutput）、挂起收尾
// （suspendTurn 三通道同构）与中断/错误收尾。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"encoding/json"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/strutil"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/mid"
	"github.com/jumeng/einox/session"
)

// runAccum 本轮输出累积（流式 chunk 拼装）。msgs = 轮内完整消息序列
// （assistant 分段 + tool 结果，入史真源）；text/thinking 另做整轮聚合
// （标题生成路径用）。超长工具结果截断归 reduction 中间件（出站即截 8192
// +外置换指针，事件泵收到的已是截断版——单一截断面，2026-08-26 退役
// 既有 newSpiller 双重截断）。
type runAccum struct {
	text      string // 整轮文本聚合（标题生成路径用）
	segText   string // 当前 assistant 段缓冲
	segThink  string
	toolCalls []schema.ToolCall // 当前段 tool_calls 缓冲
	tcSlots   map[int]int       // 流式分片槽：Index 锚 → toolCalls 位
	msgs      []*schema.Message // 轮内消息序列（历史回传用）
}

func (a *runAccum) addText(t string) {
	a.text += t
	a.segText += t
}

func (a *runAccum) addThinking(t string) { a.segThink += t }

// addToolResult 工具结果入序列（react 顺序：位于其 assistant 段之后）。
// 入史供下轮续聊；截断/外置归 reduction 中间件（此处零加工）。
func (a *runAccum) addToolResult(callID, content string) {
	if callID == "" {
		return
	}
	a.msgs = append(a.msgs, schema.ToolMessage(content, callID))
}

// addToolCall 流式 tool call 分片归并（OpenAI 协议：首片原子带 id/name，
// 续片仅 arguments 增量、以 Index 为锚——eino 引擎侧同语义归并后才执行）。
// 不归并则历史 tool call 参数为空，下轮回传被 omitempty 省略 arguments 键，
// 供应商 400 missing field `arguments`。
func (a *runAccum) addToolCall(tc schema.ToolCall) {
	if tc.Index == nil { // 非分片形态（完整调用）：直接追加
		a.toolCalls = append(a.toolCalls, tc)
		return
	}
	if pos, ok := a.tcSlots[*tc.Index]; ok {
		m := &a.toolCalls[pos]
		if tc.ID != "" {
			m.ID = tc.ID
		}
		if tc.Type != "" {
			m.Type = tc.Type
		}
		if tc.Function.Name != "" {
			m.Function.Name = tc.Function.Name
		}
		m.Function.Arguments += tc.Function.Arguments
		return
	}
	a.toolCalls = append(a.toolCalls, tc)
	if a.tcSlots == nil {
		a.tcSlots = map[int]int{}
	}
	a.tcSlots[*tc.Index] = len(a.toolCalls) - 1
}

// endAssistantMsg 单条 assistant 流结束：段封账入序列（空段跳过），清段缓冲
// 与分片槽（同轮多条 assistant 消息 Index 各自独立编号，槽不跨消息复用）。
func (a *runAccum) endAssistantMsg() {
	defer func() {
		a.segText, a.segThink, a.toolCalls, a.tcSlots = "", "", nil, nil
	}()
	if a.segText == "" && a.segThink == "" && len(a.toolCalls) == 0 {
		return
	}
	m := &schema.Message{Role: schema.Assistant, Content: a.segText, ReasoningContent: a.segThink}
	if len(a.toolCalls) > 0 {
		m.ToolCalls = a.toolCalls
	}
	a.msgs = append(a.msgs, m)
}

// discardSeg 丢弃当前半截段（网络容错 ② 重连路径：失败尝试的半截增量已被
// 事件层实时转发，adk 在模型调用边界内重启本次调用——本段不入史）。text
// 同步回卷：addText 同步追加保证整轮聚合尾部恒等于本段文本（TrimSuffix 安全）。
func (a *runAccum) discardSeg() {
	a.text = strings.TrimSuffix(a.text, a.segText)
	a.segText, a.segThink, a.toolCalls, a.tcSlots = "", "", nil, nil
}

// errToEvent 错误分类（CONFIG/SERVER/AUTH/RATE_LIMIT/TRANSPORT）。
func errToEvent(err error, s *session.Session) contract.ErrorOut {
	if s.Stopped() {
		return contract.ErrorOut{}
	}
	var ce *configError
	if errors.As(err, &ce) {
		return contract.ErrorOut{Code: contract.ErrCodeConfig, Message: ce.msg}
	}
	return errCard(err)
}

// unwrapRetryExhausted 展开重试耗尽（分类/文案落点回到末次真实错误）。
func unwrapRetryExhausted(err error) error {
	var re *adk.RetryExhaustedError
	if errors.As(err, &re) {
		return re.LastErr
	}
	return err
}

// emitTransportRetry 重连通知（WillRetryError 的 0 基失败序 → 1 基重连序）。
// 耗尽前的最后一次失败信号不发通知——错误卡随后即到，避免「N+1/N」越界提示。
func (m *Manager) emitTransportRetry(s *session.Session, fn emitFn, wr *adk.WillRetryError) {
	if n := wr.RetryAttempt + 1; n <= llm.MaxRetries {
		m.emit(s, fn, contract.EvTransportRetry, contract.TransportRetry{Attempt: n, Max: llm.MaxRetries})
	}
}

// errCard 分类驱动的错误卡（网络容错 ③）：重试耗尽先展开末次真实错误；
// 文案 = 分类器中文信息（含重试注记）。
func errCard(err error) contract.ErrorOut {
	var re *adk.RetryExhaustedError
	if errors.As(err, &re) {
		c := llm.Classify(re.LastErr)
		return contract.ErrorOut{Code: c.Code, Message: strutil.Truncate(
			fmt.Sprintf("%s（已自动重试 %d 次）", c.Message, re.TotalRetries), 200)}
	}
	c := llm.Classify(err)
	return contract.ErrorOut{Code: c.Code, Message: strutil.Truncate(c.Message, 200)}
}

// flushAcc 本轮累积封账入史（endAssistantMsg 收口 + AppendHistory——
// settleTurn 前的五处共用序列：pump 三挂起通道 / 门回灌 / 超窗重装配，
// 曾五处各写，审查 P2-5）。clear = 清账（acc 复用至后续收尾路径时防同批
// 二次入史——门回灌/超窗重装配）；挂起路径不清（acc 返回调用方后由
// settleTurn 的 pending 分支弃用）。
func flushAcc(s *session.Session, acc *runAccum, clear bool) {
	acc.endAssistantMsg()
	if len(acc.msgs) > 0 {
		s.AppendHistory(acc.msgs...)
		if clear {
			acc.msgs = nil
		}
	}
}

// suspendTurn 挂起收尾（ask/plan/approval 三通道刻意同构——机制段单点化，
// 「与审批同通道」的设计等价性由同一函数兑现）。挂起轮已产出段先入史：
// 批准后 Resume 的 tool 结果接在其后才是完整序列（丢弃则批准结果成孤儿
// tool 消息，续聊回传即 400；超时无人续由 sanitizeHistory 剥悬空 tool_calls）。
func (m *Manager) suspendTurn(s *session.Session, acc *runAccum, id string, timeoutAt time.Time, kind string) (*runAccum, string, error) {
	flushAcc(s, acc, false)
	m.startApprovalTimer(s, id, timeoutAt, kind)
	m.finishOf(s)(session.StatePendingApproval)
	return acc, session.StatePendingApproval, nil
}

// pump 事件泵：迭代 runner 事件 → 契约事件分类（Run/Resume 共用）。
// 返回 (本轮累积, 终态, 终态模型错误)：StatePendingApproval = 审批挂起
// （调用方不收尾）；endState 空 = 静默收线（停止/断连）。est = 上下文分类
// 估算（usage 事件用）。第三返回值非空 ⟺ OVERFLOW 类终态错误且未发卡
// （其余终态错误就地发卡后归零——超窗裁决权在上层 pumpWithOverflow）。
func (m *Manager) pump(s *session.Session, iter *adk.AsyncIterator[*adk.AgentEvent], fn emitFn, est ctxEstimates, behaviors map[string]string) (*runAccum, string, error) {
	acc := &runAccum{}
	endState := session.StateEnded
	subCalls := map[string]string{} // 子代理 callID → 工具名（EvSubAgent tool_result 契约语义=工具名，配对回填）
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			if s.Stopped() {
				return nil, "", nil // 删除：静默（磁盘零残留）
			}
			if errors.Is(ev.Err, context.Canceled) {
				m.interruptUnlessStopped(s, fn) // 断连/停止：中断收尾
				return nil, "", nil
			}
			var wr *adk.WillRetryError
			if errors.As(ev.Err, &wr) {
				// 网络容错 ②：重试在途（Generate 路径的事件化形态——流式路径
				// 的半截处理在 handleOutput）：非故障，通知 + 继续泵
				m.emitTransportRetry(s, fn, wr)
				continue
			}
			// 轮次耗尽：不是故障是预算——历史已入史（中断保险），发消息即可以
			// 全新预算接续；裸抛 NodeRunError 用户无从知道能继续
			if errors.Is(ev.Err, adk.ErrExceedMaxIterations) {
				m.emit(s, fn, contract.EvError, contract.ErrorOut{Code: contract.ErrCodeServer, Message: fmt.Sprintf(
					"本轮模型调用轮次已达上限（%d）——任务暂停而非失败，发送消息（如「继续」）即可接续执行",
					maxIterations)})
				endState = session.StateError
				break
			}
			// 超窗：不在泵面发卡——交 pumpWithOverflow 裁剪重装配裁决
			//（rerr 非空 ⟺ 超窗未发卡；其余终态错误就地发卡后归零）
			if isOverflowErr(ev.Err) {
				return acc, session.StateError, ev.Err
			}
			m.emit(s, fn, contract.EvError, errCard(ev.Err))
			endState = session.StateError
			break
		}
		if ev.Action != nil && ev.Action.Interrupted != nil {
			// ask_user 提问中断（askuser 工具发起）：发 ask_user_request → 挂起态
			// 落盘 → 流收线（answer 端点 Resume 续流）——与审批同通道。
			if card, ok := askCardOf(ev.Action.Interrupted); ok && card.Question != "" {
				askID := newAskID()
				timeoutAt := time.Now().Add(ApprovalTimeout())
				m.emit(s, fn, contract.EvAskRequest, contract.AskReq{
					AskID: askID, Question: card.Question, Options: card.Options,
					AllowMulti: card.AllowMulti, AllowFreeText: card.AllowFreeText, TimeoutAt: timeoutAt,
				})
				s.SetPendingApproval(askID)
				return m.suspendTurn(s, acc, askID, timeoutAt, "ask")
			}
			// 计划提交中断（plan 工具发起）：发 plan_request → 挂起态落盘 → 流
			// 收线（approve 端点按 pending_kind=plan 分叉回执，Resume 续流）。
			// 文档已由工具先落盘（跨重启在盘），此处只挂起等审批。
			if card, ok := planCardOf(ev.Action.Interrupted); ok && card.Task != "" {
				planID := newPlanID()
				timeoutAt := time.Now().Add(ApprovalTimeout())
				m.emit(s, fn, contract.EvPlanRequest, contract.PlanReq{
					PlanID: planID, Task: card.Task, Summary: card.Summary, Steps: card.Steps,
					Risks: card.Risks, Path: card.Path, Seq: card.Seq, Mode: card.Mode, TimeoutAt: timeoutAt,
				})
				s.ClearTaskGrant() // 新计划提交 = 任务改向，旧任务期授权作废（批准后重新授予）
				s.SetPendingApproval(planID)
				return m.suspendTurn(s, acc, planID, timeoutAt, "plan")
			}
			// 审批中断（写工具 wrapper 发起）：聚合发一卡（H4-2 合并决议——一轮
			// 并行写调用的全部审批上下文收进一张 EvApprovalRequest N 项）→ 挂起态
			// 落盘 → 流收线（approve 端点批量决议 Resume 续流）。checkpoint 已由
			// runner 保存。
			if cards := approvalCardsOf(ev.Action.Interrupted); len(cards) > 0 {
				appID := newApprovalID()
				timeoutAt := time.Now().Add(ApprovalTimeout())
				req := contract.ApprovalReq{ApprovalID: appID, TimeoutAt: timeoutAt}
				// T6 审批身份链：谁的动作（当轮说话人；回退 Owner ID——
				//「问谁」路由与审计「这条指令谁下的」的数据前提）
				if a := s.TurnActorOf(); a != nil {
					req.RequesterID, req.RequesterName = a.ID, a.Name
				} else {
					req.RequesterID = s.Owner
				}
				// T6 路由：问谁（应用判据裁决；目标随卡 + 入 pending target）
				s.SetPendingTarget("")
				if m.Opt.ApprovalRouter != nil {
					if tgt := m.Opt.ApprovalRouter(m.briefOf(s), req); tgt != nil && tgt.ID != "" {
						req.TargetID, req.TargetName = tgt.ID, tgt.Name
						s.SetPendingTarget(tgt.ID)
					}
				}
				ids := make([]string, 0, len(cards))
				for _, c := range cards {
					ids = append(ids, c.ItemID)
					req.Items = append(req.Items, contract.ApprovalItem{
						ItemID: c.ItemID, Tool: c.Tool, Action: c.Action, Plan: c.Plan,
						PlanMode: c.PlanMode, Note: c.Note, Diff: c.Diff,
						RequesterID: req.RequesterID, RequesterName: req.RequesterName, // T6 逐项署名（同轮同 actor）
					})
				}
				// 顶层旧字段 = 首项镜像（旧回放/旧前端按 N=1 单卡渲染——兼容）
				req.Tool, req.Action, req.Plan = cards[0].Tool, cards[0].Action, cards[0].Plan
				req.PlanMode, req.Note, req.Diff = cards[0].PlanMode, cards[0].Note, cards[0].Diff
				m.emit(s, fn, contract.EvApprovalRequest, req)
				s.SetPendingApproval(appID)
				s.SetPendingItems(ids) // 超时批量拒 / 端点覆盖校验依据
				return m.suspendTurn(s, acc, appID, timeoutAt, "approval")
			}
		}
		// H8-2 全量转发档：子代理内部事件（AgentName 非空 = 子 agent——父
		// agent 除 supervisor 形态外不命名，spawn 与拓扑子 agent 均在内；
		// supervisorMainName 是主 agent 本体（transfer 转回寻址用），排除后
		// 其输出仍走父主流/父历史；ToolsConfig.EmitInternalEvents 开启时
		// agent_tool 转发到父流）翻译为 EvSubAgent 只读流——不进父上下文/
		// 主流（官方注释实证：转发件不入父 runSession；非空名一并拦截，堵
		// 拓扑子事件落穿误入父历史）。
		if m.subEventsOn() && ev.AgentName != "" && ev.AgentName != supervisorMainName &&
			ev.Action == nil && ev.Output != nil && ev.Output.MessageOutput != nil {
			m.emitSubAgent(s, fn, ev.AgentName, subCalls, ev.Output.MessageOutput, "", nil)
			continue
		}
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		if stop, terr := m.handleOutput(s, fn, acc, ev.Output.MessageOutput, est, behaviors); stop != outContinue {
			// 已删除/断连：删除静默，断连中断收尾；传输致命：错误卡已发，
			// 立即收线 error 态（不再赌下一次 iter.Next 送错——事件层客户
			// 副本被中途弃读后内部管线可能互等，即「卡 running」旧病根）
			if stop == outDeleted {
				m.interruptUnlessStopped(s, fn)
				return nil, "", nil
			}
			if stop == outOverflow {
				return acc, session.StateError, terr // 超窗未发卡：交 pumpWithOverflow 裁决
			}
			endState = session.StateError
			break
		}
	}
	if s.Stopped() {
		return nil, "", nil
	}
	return acc, endState, nil
}

// interruptUnlessStopped 断连/停止收尾（删除会话静默跳过）：翻中断终态 +
// 事件 + 落盘——不留 running 僵尸。覆盖页面关闭/刷新/停止按钮。审批挂起
// （pending_approval）不受影响——那是有意的跨页面等待，超时器兜底。
// FlushQueue 的打断走 interrupted 行（非故障形态——紧跟的新一轮以排队消息
// 为输入）。打断语义告知（codex interrupted marker 对位）：中断轮的历史追
// 加一条系统注记——模型续聊时知晓打断语境与「工具可能部分执行」语义，不
// 假设中断前操作都已成功。
func (m *Manager) interruptUnlessStopped(s *session.Session, fn emitFn) {
	if s.Stopped() {
		return
	}
	if s.TakeFlushMark() {
		m.appendInterruptMarker(s)
		m.emit(s, fn, contract.EvInterrupted, contract.InterruptOut{Message: "已打断当前任务，立即处理排队消息"})
		m.finishOf(s)(session.StateError)
		return
	}
	m.appendInterruptMarker(s)
	m.emit(s, fn, contract.EvError, contract.ErrorOut{Code: contract.ErrCodeAborted, Message: "手动停止，任务中断（已执行的操作不回滚）"})
	m.finishOf(s)(session.StateError)
}

// appendInterruptMarker 打断历史标记（部分执行语义三义：被打断/后台进程可
// 能仍在跑/工具可能部分执行——续聊轮的模型可见面；悬空 tool_call 的配对
// 修补归 timers/sanitizeHistory，此处只补语义告知半边）。
func (m *Manager) appendInterruptMarker(s *session.Session) {
	s.AppendHistory(schema.UserMessage(
		"（系统注记）上一轮执行被中断：部分工具调用可能未完成或未生效，后台进程可能仍在运行。" +
			"继续任务前先用只读工具核对现场（文件状态/后台任务输出），不要假设中断前的操作都已成功。"))
	m.reg.Persist(s)
}

// summaryOf 死代码已删（审查 P2-3：零调用者的冗余间接层——消费方均直调
// s.SummaryOf）。

// outVerdict handleOutput 处置判定。
type outVerdict int

const (
	outContinue outVerdict = iota // 继续泵
	outDeleted                    // 会话已删：静默弃（调用方 interruptUnlessStopped 兜删除分支）
	outFatal                      // 传输致命：错误卡已发，立即收线 error 态
	outOverflow                   // 上下文超窗：未发卡，错误经第二返回值上交（pumpWithOverflow 裁决）
)

// isOverflowErr 超窗类错误判定（泵 ev.Err 面与 handleOutput 流内错误面同
// 判据——曾两处逐字，重试耗尽解包是共同前置）。
func isOverflowErr(err error) bool {
	return llm.Classify(unwrapRetryExhausted(err)).Code == llm.CodeOverflow
}

// drainToolVariant 工具结果变体排空：流式拼装完整内容并跟踪 callID（协议
// 约定 tool 结果单流一次消费，err 即收口含 EOF），非流式直取。handleOutput
// （主面）与 emitSubAgent（子面）共用——曾两处逐字复制，流协议字段变更漏改
// 一面即主面/子面工具结果内容或配对分叉（审查 P2-1）。
func drainToolVariant(v *adk.TypedMessageVariant[*schema.Message]) (content, callID string) {
	if v.IsStreaming && v.MessageStream != nil {
		var b strings.Builder
		for {
			chunk, err := v.MessageStream.Recv()
			if err != nil {
				break
			}
			if chunk != nil {
				b.WriteString(chunk.Content)
				if chunk.ToolCallID != "" {
					callID = chunk.ToolCallID
				}
			}
		}
		return b.String(), callID
	}
	if v.Message != nil {
		return v.Message.Content, v.Message.ToolCallID
	}
	return "", ""
}

// handleOutput 单事件分类。est = 上下文分类估算（usage 事件载荷）。
// 返回值：outContinue 继续泵；outDeleted 会话已删（调用方静默弃）；
// outFatal 传输致命（错误卡已发，调用方立即收线 error 态）；outOverflow
// 超窗未发卡（错误随第二返回值上交——裁剪重装配归 pumpWithOverflow）。
func (m *Manager) handleOutput(s *session.Session, fn emitFn, acc *runAccum, v *adk.TypedMessageVariant[*schema.Message], est ctxEstimates, behaviors map[string]string) (outVerdict, error) {
	switch v.Role {
	case schema.Tool:
		// 工具结果（streaming 时拼装完整内容；digest 截断）
		content, callID := drainToolVariant(v)
		if s.Stopped() {
			return outDeleted, nil
		}
		ok, digest, preview := mid.ToolResultDigest(content) // 语义摘要 + 原始头（展开用）
		var cr struct {
			Counts string `json:"counts"`
			Verb   string `json:"verb"`
		}
		_ = json.Unmarshal([]byte(content), &cr) // B4 信封提取（无键的工具零值省略）
		m.emit(s, fn, contract.EvToolResult, contract.ToolResult{CallID: callID, OK: ok, Digest: digest, Preview: preview, Counts: cr.Counts, Verb: cr.Verb})
		acc.addToolResult(callID, content) // 入史：assistant(tool_calls) 须紧跟 tool 结果，缺失回传即 400
		m.reg.Persist(s)                   // 工具边界节流落盘（C2）：轮内崩溃不丢已完工具轮——频率有界（工具调用数）、单文件全量格式不变
		return outContinue, nil

	case schema.Assistant:
		if v.IsStreaming && v.MessageStream != nil {
			for {
				chunk, err := v.MessageStream.Recv()
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					if s.Stopped() || errors.Is(err, context.Canceled) {
						return outDeleted, nil
					}
					var wr *adk.WillRetryError
					if errors.As(err, &wr) {
						// 网络容错 ②：传输类已分类可重试，adk 在模型调用边界内
						// 重启本次调用。失败尝试的半截增量已实时转发到事件流（eino
						// 协议：客户端自行 reset）——丢弃半截段（不入史）+ 通知
						// 前端回卷显示；重试尝试的新流作为下一事件自然到达。
						m.emitTransportRetry(s, fn, wr)
						acc.discardSeg()
						return outContinue, nil
					}
					// 超窗：不发卡——交 pumpWithOverflow 裁剪重装配裁决（与
					// ev.Err 分支同纪律）
					if isOverflowErr(err) {
						return outOverflow, err
					}
					// 致命（欠费/认证/参数错/重试耗尽/未知）：分类错误卡 + 立即收线
					m.emit(s, fn, contract.EvError, errCard(err))
					return outFatal, nil
				}
				if chunk == nil {
					continue
				}
				if chunk.ResponseMeta != nil {
					m.emitUsage(s, fn, chunk.ResponseMeta.Usage, est, "")
				}
				if chunk.ReasoningContent != "" {
					acc.addThinking(chunk.ReasoningContent)
					m.emit(s, fn, contract.EvThinkingDelta, contract.Delta{Delta: chunk.ReasoningContent})
				}
				if chunk.Content != "" {
					acc.addText(chunk.Content)
					m.emit(s, fn, contract.EvTextDelta, contract.Delta{Delta: chunk.Content})
				}
				for _, tc := range chunk.ToolCalls {
					acc.addToolCall(tc) // 分片归并（首片带 id/name，续片仅 arguments 增量）
				}
				if s.Stopped() {
					return outDeleted, nil
				}
			}
			// 工具调用事件在消息段收口后发：arguments 分片此时归并完整——
			// 流中首片发事件只能拿到残缺 JSON，参数摘要（ArgsDigest）必失真
			for _, tc := range acc.toolCalls {
				m.emit(s, fn, contract.EvToolCall, contract.ToolCall{CallID: tc.ID, Tool: tc.Function.Name, ArgsDigest: mid.ToolArgsDigest(tc.Function.Arguments), Behavior: behaviors[tc.Function.Name]})
			}
			acc.endAssistantMsg()
			return outContinue, nil
		}
		// 非流式完整消息
		msg := v.Message
		if msg == nil {
			return outContinue, nil
		}
		if msg.ResponseMeta != nil {
			m.emitUsage(s, fn, msg.ResponseMeta.Usage, est, "")
		}
		if msg.ReasoningContent != "" {
			acc.addThinking(msg.ReasoningContent)
			m.emit(s, fn, contract.EvThinkingDelta, contract.Delta{Delta: msg.ReasoningContent})
		}
		if msg.Content != "" {
			acc.addText(msg.Content)
			m.emit(s, fn, contract.EvTextDelta, contract.Delta{Delta: msg.Content})
		}
		for _, tc := range msg.ToolCalls {
			acc.addToolCall(tc)
			m.emit(s, fn, contract.EvToolCall, contract.ToolCall{CallID: tc.ID, Tool: tc.Function.Name, ArgsDigest: mid.ToolArgsDigest(tc.Function.Arguments), Behavior: behaviors[tc.Function.Name]})
		}
		acc.endAssistantMsg()
		return outContinue, nil

	default:
		return outContinue, nil
	}
}

// accText 累积文本（nil 防御）。
func accText(a *runAccum) string {
	if a == nil {
		return ""
	}
	return a.text
}
