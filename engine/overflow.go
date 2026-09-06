package engine

// 溢出恢复（吸收设计 §3.3-②；dsh compaction-basic request-error 钩子的对位
// 形态）：真超窗 Failover 救不了（窗口更小的备模型更糟）、重试救不了（确定
// 性失败）——恢复位只能在 manager 输入面：重装配本轮输入（清窗兜底同源尾段
// 裁剪 + 任务锚）后重试一次，有界。判据 = 出站口径确实下降（dsh「只有压缩
// 确实发生才重试」——降不下来重试必再炸）。范围：主运行面（Run/Resume/门
// 回灌共用 drive）；子代理内部运行不在此面（子面挂 reduction 中间件，超窗由
// 其先行消化，终态走 spawn 信封回父）。

import (
	"context"
	"fmt"
	"log"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
)

// pumpWithOverflow 泵 + 超窗有界恢复。pump 对 OVERFLOW 类终态错误不就地
// 发卡（rerr 非空 ⟺ 超窗未发卡——其余终态错误泵面已发卡、rerr 为 nil），
// 交此处裁决：可裁剪即重装配重试；不可行或重试后仍超窗——如实发卡。
func (m *Manager) pumpWithOverflow(runCtx context.Context, s *session.Session, fn emitFn,
	iter *adk.AsyncIterator[*adk.AgentEvent], behaviors map[string]string) (*runAccum, string) {
	acc, endState, rerr := m.pump(s, iter, fn, m.estimateContext(s), behaviors)
	if endState != session.StateError || rerr == nil {
		return acc, endState
	}
	iter2, behaviors2, rerr2, ok := m.overflowRetry(runCtx, s, fn, acc, behaviors)
	if !ok {
		if rerr2 != nil {
			// 重装配失败：真实原因如实发卡（errToEvent 保 configError→CONFIG
			// 分类）——旧 overflow 错误此时已非真相，发它会误导排障方向。
			m.emit(s, fn, contract.EvError, errToEvent(rerr2, s))
		} else {
			m.emit(s, fn, contract.EvError, errCard(rerr)) // 口径降不下来等：如实报旧错
		}
		return acc, endState
	}
	acc, endState, rerr = m.pump(s, iter2, fn, m.estimateContext(s), behaviors2)
	if rerr != nil { // 重试后仍超窗：有界 1 次，如实报错
		m.emit(s, fn, contract.EvError, errCard(rerr))
	}
	return acc, endState
}

// overflowRetry 超窗重装配：半截产出先入史（settleTurn/门回灌同款封账——
// acc.msgs 清账防恢复失败路径二次入史）→ 清窗兜底同源裁剪（自最后 user 起
// 尾段 + 任务锚）为本轮唯一输入重开。出站口径（shapedTokenCounter 消息面，
// 与摘要通知卡同口径）未下降即放弃；裁掉前段全文经 transcript 外置（通知卡
// 承诺可溯源）。通知卡复用 compaction 口径（零新 Kind——docs/03 软契约不变）。
// 第四返回值非空 = 重装配后重新组装失败（runIter 错误——调用方以真实原因
// 发卡，不再吞错归因到旧 overflow 错误）。
func (m *Manager) overflowRetry(runCtx context.Context, s *session.Session, fn emitFn,
	acc *runAccum, behaviors map[string]string) (*adk.AsyncIterator[*adk.AgentEvent], map[string]string, error, bool) {
	if s.Stopped() || runCtx.Err() != nil {
		return nil, nil, nil, false
	}
	if acc != nil {
		acc.endAssistantMsg()
		if len(acc.msgs) > 0 {
			s.AppendHistory(acc.msgs...)
			acc.msgs = nil
		}
	}
	hist := sanitizeHistory(s.CloneHistory())
	input := append(append([]*schema.Message{}, tailFromLastUser(hist)...), taskAnchor(s, lastTodoState(hist)))
	input = renderSpeakers(input) // T6 署名前缀投影（重装配输入同主输入同律）
	before, _ := shapedTokenCounter(context.Background(), hist, nil)
	after, _ := shapedTokenCounter(context.Background(), input, nil)
	if after >= before {
		return nil, nil, nil, false // 单条巨消息即全部历史等形态：降不下来，重试必再炸
	}
	writeTranscript(m.reg.Store(), s, hist)
	m.emit(s, fn, contract.EvHarnessNote, contract.HarnessNote{
		Kind:  "compaction",
		Title: fmt.Sprintf("上下文超窗，已裁剪重试（%d → %d token）", before, after),
		Detail: "本轮输入超出模型窗口：按清窗兜底同源裁剪（保留最近上下文与任务锚）重试一次。" +
			"被裁前段全文 " + transcriptPath(s) + "（read_file 可溯源）",
	})
	iter, behaviors, err := m.runIter(runCtx, s, input)
	if err != nil {
		log.Printf("overflow: 重装配后重新组装失败（%s）：%v", s.SID, err)
		return nil, nil, err, false
	}
	return iter, behaviors, nil, true
}
