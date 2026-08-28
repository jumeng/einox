// Package hitl 是审批包装器（挂起-续流 HITL 机制；自产品 internal/agent
// interrupt.go 迁入，对齐官方 approval_wrapper 形态 + fail-closed 语义）。
//
// 模式 × 工具级矩阵（组装期包装组合，引擎层无分支）：
//
//	读工具         任何模式裸传（读无副作用）
//	写工具 manual   每次调用单独中断 → 审批卡（逐次确认）
//	写工具 plan     未授权时首个写调用中断（批准授本轮）；submit_plan 计划获
//	               批后任务期全免（taskGrant——批准计划 = 一口气执行）
//	写工具 auto     裸传直落（用户显式让渡；硬红线仍在工具实现内代码级拒绝）
//	ArgsForce 命中  无视模式与授权状态一律中断（判断权在人的写值，如 status=
//	               完成——人批准一代执行，任何模式/授权不豁免）
//
// fail-closed：中断恢复后无决议（审批通道异常/数据丢失）→ 一律拒绝执行，
// 绝不放行。名单与文案（写工具集/动作名/参数豁免）由应用注入——基座只持
// 机制，内容归业务。
package hitl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/mid"
)

// ApprovalConfig 审批配置（应用注入）。
type ApprovalConfig struct {
	// WriteTools 写工具名集（读工具不包装）。
	WriteTools map[string]bool
	// WritePrefix 写工具名前缀集（如 mcp_ 拉取工具语义未知，fail-closed
	// 一律按写审批）。
	WritePrefix []string
	// ArgsSkip 参数级只读豁免（同工具名下部分调用只读，如 run_command 的
	// 白名单只读命令）。
	ArgsSkip map[string]func(args string) bool
	// ArgsForce 参数级强制审批（同 ArgsSkip 对称的反向机制）：命中则无视
	// 模式与授权状态一律挂起——「判断权在人」的写值（如 status=完成），
	// auto 档/任务期授权均不豁免，人批准后本次代执行。
	ArgsForce map[string]func(args string) bool
	// ForceNotes 强制审批卡说明文案（按工具名；命中 ArgsForce 时覆盖缺省
	// Note——说明本确认不可被模式授权语义替代）。
	ForceNotes map[string]string
	// Actions 工具动作名（审批卡直白话；缺省回退工具名）。
	Actions map[string]string
}

// ActionNameOf 工具动作名（缺省回退工具名）。
func (c ApprovalConfig) ActionNameOf(tool string) string {
	if zh, ok := c.Actions[tool]; ok {
		return zh
	}
	return tool
}

// IsWrite 写工具判定：名单命中或前缀命中。
func (c ApprovalConfig) IsWrite(name string) bool {
	if c.WriteTools[name] {
		return true
	}
	for _, p := range c.WritePrefix {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// DecisionSource 会话域决议源（approve 端点写入 → Resume 消费；plan 档
// 本轮/任务期写授权跟随会话）。
type DecisionSource interface {
	TakeDecision() *contract.ApprovalDecision
	// TakeDecisionFor 合并决议多槽取用（H4-2：按挂起时生成的 item_id 领
	// 各自项的决议；并行写调用重放时各工具各取各的，无决议到达 = fail-closed）。
	TakeDecisionFor(itemID string) *contract.ApprovalDecision
	TurnGranted() bool
	GrantTurn()
	TaskGranted() bool
	GrantTask()
}

// approvalState 中断保存态（恢复时带回：原始调用参数 + 项标识）。
type approvalState struct {
	Args   string
	ItemID string // 合并决议卡项标识（空 = 旧单卡挂起态——legacy 单决议路径）
}

// newItemID 合并决议卡项标识（i 前缀 + 6 hex；与审批 a/提问 q/计划 p 前缀区分）。
func newItemID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "i" + hex.EncodeToString(b)
}

// gob 序列化注册（checkpoint 持久化中断载荷——未注册跨进程恢复会失败）。
func init() {
	schema.Register[ApprovalConfig]() // 保持 gob 注册表覆盖（空结构无字段开销）
	schema.Register[approvalState]()
	schema.Register[contract.ApprovalCard]()
}

// approvalTool 写工具审批包装（委托 Info，覆写 Invoke）。
type approvalTool struct {
	t      contract.Tool
	src    DecisionSource
	mode   string
	opName string
	cfg    ApprovalConfig
	diff   func(args string) string // 卡 diff 载荷（原始工具实现 DiffProvider 时取入；nil = 无）
}

// needsApproval 模式 × 授权判定（plan 档：本轮授权或任务期授权任一在即免审；
// bg 档：后台子代理面——写工具 auto 语义直落，ArgsForce 不走本判定）。
func (a *approvalTool) needsApproval() bool {
	switch a.mode {
	case "auto", "bg":
		return false
	case "plan":
		return !a.src.TurnGranted() && !a.src.TaskGranted()
	default: // manual
		return true
	}
}

func (a *approvalTool) Info() *contract.ToolInfo { return a.t.Info() }

func (a *approvalTool) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	forced := false
	if f := a.cfg.ArgsForce[a.opName]; f != nil && f(string(args)) {
		forced = true // 参数级强制审批：豁免路径全数让位
		if a.mode == "bg" {
			// 后台子代理 fail-closed（W-2）：挂起等人在后台无人决议——一律拒绝
			// 回喂子代理改道（宁可子代理失败，绝不静默放行写操作）。
			return json.RawMessage(`{"ok":false,"error":"该操作需人工审批，后台子代理内禁用（fail-closed）——请在结论中说明，由父会话用户确认后执行"}`), nil
		}
	} else if skip := a.cfg.ArgsSkip[a.opName]; skip != nil && skip(string(args)) {
		return a.t.Invoke(ctx, args) // 参数级只读豁免（view 类）
	} else if !a.needsApproval() {
		return a.t.Invoke(ctx, args)
	}

	// 恢复流：此前在本工具处中断过（适配层经 ctx 注回保存态）
	if st, ok := contract.ResumeStateOf(ctx); ok {
		var saved approvalState
		if s, ok := st.(approvalState); ok {
			saved = s
		}
		var decision *contract.ApprovalDecision
		if saved.ItemID != "" {
			decision = a.src.TakeDecisionFor(saved.ItemID) // 合并决议多槽：按项领决议
		} else {
			decision = a.src.TakeDecision() // 旧单卡挂起态（升级前 checkpoint 重放）
		}
		if decision == nil {
			// fail-closed：恢复但无决议 → 拒绝（绝不放行）。以工具结果形态回喂
			// 模型（Go error 会终止整个 run——拒绝应让模型可见并可调整）。
			return json.RawMessage(`{"ok":false,"error":"审批通道不可用（无决议到达），本次操作默认拒绝（fail-closed）——请向用户确认后再试"}`), nil
		}
		if !decision.Approve {
			reason := decision.Reason
			if reason == "" {
				reason = "用户未提供原因"
			}
			return json.RawMessage(fmt.Sprintf(`{"ok":false,"error":"disapproved: %s——用户拒绝本次操作，请调整方案或向用户确认"}`, reason)), nil
		}
		// 批准：plan 档授权本轮后续写
		if a.mode == "plan" {
			a.src.GrantTurn()
		}
		runArgs := string(args)
		if saved.Args != "" {
			runArgs = saved.Args
		}
		return a.t.Invoke(ctx, json.RawMessage(runArgs))
	}

	// 新写调用 → 挂起等审批（适配层转引擎中断 → 存 checkpoint → 事件面卡片）
	itemID := newItemID() // 合并决议卡项标识：随卡与保存态双带（Resume 按项领决议）
	digest := mid.ArgsDigest(string(args))
	action := a.cfg.ActionNameOf(a.opName)
	card := contract.ApprovalCard{Tool: a.opName, Action: action, Args: string(args), ItemID: itemID,
		Plan: []contract.PlanItem{{Action: action, Summary: digest, Count: 1}}}
	if a.diff != nil {
		card.Diff = a.diff(string(args))
	}
	if a.mode == "plan" {
		card.PlanMode = true
		card.Note = "计划模式：批准 = 授权本轮写操作。建议先用 submit_plan 提交计划文档——计划获批后整个任务期免逐项确认"
	}
	if forced {
		card.Note = a.cfg.ForceNotes[a.opName] // 强制审批文案优先（本确认不可被模式授权语义替代）
	}
	return nil, &contract.Suspend{Info: card, State: approvalState{Args: string(args), ItemID: itemID}}
}

// WrapTools 组装期包装：全量工具内层套 errFeed（业务错误回喂模型）+ guard
// （防死循环/硬上限）；写工具按模式再套审批 wrapper，读工具裸传。
// 挂起哨兵（*contract.Suspend）经 errFeed 直通（ask_user 同构——包装链
// 不吞挂起信号，故无需裸传名单）。
func WrapTools(ts []contract.Tool, src DecisionSource, mode string, cfg ApprovalConfig) []contract.Tool {
	out := make([]contract.Tool, 0, len(ts))
	for _, t := range ts {
		fed := mid.Guard(mid.ErrFeed(t))
		name := t.Info().Name
		if !cfg.IsWrite(name) {
			out = append(out, fed)
			continue
		}
		at := &approvalTool{t: fed, src: src, mode: mode, opName: name, cfg: cfg}
		// diff 载荷探测在未包装的原始工具 t 上做——fed/Guard 包装会隐藏可选接口
		if dp, ok := t.(contract.DiffProvider); ok {
			at.diff = dp.ApprovalDiff
		}
		out = append(out, at)
	}
	return out
}
