package engine

// 收束质量门（FinalGate，findings/2026-08-29-capability-hardening-design.md §10
// 分层修订版）：三层约束（事前审批/事中 ErrFeed/收束门）的唯一空位补齐。
//
// 分层定案（2026-08-29 裁决）：
//   einox 层 = 门循环机制——收束拦截（pump 自然收束后、settleTurn 前）、
//     checker 按序执行、失败回灌重入（反馈入史 + harness_note 门卡）、
//     有界重试、耗尽诚实报错（不静默放行）、checker panic fail-closed。
//   应用层 = 判据与门宽——Checkers 内容（build/test 命令、审查简报）、
//     按会话概要开门与否（闭包入参 SessionBrief：模式/任务形态）、
//     MaxRetries 调参、对抗审查（E4）自包在 checker 内。实验场 = einox-pm。
//
// 门宽纪律：只卡收束一个点（挂起/中断/错误轮不触发）；纯问答会话也会过门
// ——闭包应按会话形态开门（例：仅编码模式）；重试预算随 Run/Resume 执行体
// （挂起续流后重新计数——用户决议本身是继续的明确信号）。

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
)

// gateDefaultRetries 回灌上限缺省（教程判据：同类失败 2-3 次转硬错）。
const gateDefaultRetries = 2

// GateChecker 确定性判据（ctx + 会话工作区根；nil error = 过）。判据归应用
// ——基座不内置任何 build/test 知识；要 LLM 对抗审查（E4 形态）在 checker
// 内自包（应用侧调模型 + 只读工具），门循环不关心判据形态。ctx 是运行取消
// 面（用户停止/断连才断）而非判据超时面——慢判据应自包 WithTimeout。
type GateChecker func(ctx context.Context, workspaceRoot string) error

// GateConfig 门配置（Options.FinalGate 闭包产物；nil = 该会话不开门）。
type GateConfig struct {
	Checkers   []GateChecker // 按序执行，首个失败即回灌（错误文案应含判据名——门卡展示用）
	MaxRetries int           // 回灌上限（负数 = 缺省 2；0 = 零回灌——首验失败即报错，codex Guardian cyber 档同型）
}

func gateRetries(g *GateConfig) int {
	if g.MaxRetries < 0 {
		return gateDefaultRetries
	}
	return g.MaxRetries
}

// drive 泵事件至收束 + 收束门回灌循环（Run/Resume 共用）：自然收束且门在
// 场→跑 checker→过则正常收束；不过→门卡通知 + 反馈消息入史（steering 同款
// user 消息形态）+ 重入循环；重试耗尽→error 收束（诚实报错不静默放行）。
// est 每轮重算（回灌后上下文已变）。
func (m *Manager) drive(runCtx context.Context, s *session.Session, fn emitFn,
	iter *adk.AsyncIterator[*adk.AgentEvent], behaviors map[string]string) (*runAccum, string) {
	est := m.estimateContext(s)
	m.checkContextBudget(s, fn, est) // B1 常驻面超限告警（会话内只发一次，不阻断）
	acc, endState := m.pump(s, iter, fn, est, behaviors)
	var gate *GateConfig
	if m.Opt.FinalGate != nil {
		gate = m.Opt.FinalGate(m.briefOf(s))
	}
	if gate == nil || len(gate.Checkers) == 0 {
		return acc, endState
	}
	for retries := 0; endState == session.StateEnded && !s.Stopped(); {
		gerr := m.runGateCheckers(runCtx, s, gate)
		if gerr == nil {
			return acc, endState // 门过：正常收束
		}
		if retries >= gateRetries(gate) {
			m.emit(s, fn, contract.EvError, contract.ErrorOut{Code: "SERVER",
				Message: fmt.Sprintf("质量门未过（已重试 %d 次）：%s", retries, truncateRunes(gerr.Error(), 180))})
			return acc, session.StateError
		}
		retries++
		// 门卡通知（回放可见——不冒充用户消息；反馈本体入史驱动修复）
		m.emit(s, fn, contract.EvHarnessNote, contract.HarnessNote{
			Kind: "gate", Title: "质量门未过，已退回修复",
			Detail: fmt.Sprintf("第 %d 次回灌：%s", retries, truncateRunes(gerr.Error(), 300)),
		})
		// 本轮产出入史（settleTurn 同款封账）→ 反馈消息入史 → 重入
		acc.endAssistantMsg()
		if len(acc.msgs) > 0 {
			s.AppendHistory(acc.msgs...)
			acc.msgs = nil // 清账：回灌失败路径本 acc 复用至 settleTurn，防同批二次入史
		}
		fb := schema.UserMessage("（质量门未过）" + gerr.Error() +
			"。请修复以上问题后重新完成任务，不要直接重复原回答。")
		hist := sanitizeHistory(s.CloneHistory())
		s.AppendHistory(fb)
		m.reg.Persist(s)
		iter2, behaviors2, rerr := m.runIter(runCtx, s, append(hist, fb))
		if rerr != nil {
			m.emit(s, fn, contract.EvError, errCard(rerr))
			return acc, session.StateError
		}
		behaviors = behaviors2
		acc, endState = m.pump(s, iter2, fn, m.estimateContext(s), behaviors)
	}
	return acc, endState
}

// runGateCheckers 判据执行（fail-closed：checker panic 转门失败，不拖垮运行）。
func (m *Manager) runGateCheckers(ctx context.Context, s *session.Session, gate *GateConfig) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("质量门 checker panic：%v", r)
		}
	}()
	root := m.Opt.WorkspaceRoot(s.Owner, s.SID)
	for _, c := range gate.Checkers {
		if c == nil {
			continue
		}
		if e := c(ctx, root); e != nil {
			return e
		}
	}
	return nil
}
