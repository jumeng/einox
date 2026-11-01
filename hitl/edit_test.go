package hitl

// edit 第四档回归（findings/2026-11-01-hitl-batch-a-prime-design.md，对位
// 上游 checkEditMode：permissionName=edit 且 sideEffectScope=workspace →
// 放行，其余 build 档逐审）：①EditPass 命中直落零中断；②未命中回落 manual
// 挂起+批准续行；③EditPass nil 退化 manual；④edit+ArgsForce 仍挂起（判断
// 权在人不豁免）；⑤edit 批准不授本轮/任务期写权（逐次语义）+ 会话域 Always
// 授权豁免回落分支。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jumeng/einox/contract"
)

func editConfig() ApprovalConfig {
	cfg := testConfig()
	cfg.EditPass = map[string]bool{
		// 编辑免审集样本：文件编辑类（内容归应用；此处仅机制回归）。
		"apply_patch": true,
		"edit_file":   true,
	}
	return cfg
}

func TestEditPassToolPassesThrough(t *testing.T) {
	stub := &countingTool{name: "apply_patch"}
	src := &stubDecisions{}
	wrapped := WrapTools([]contract.Tool{stub}, src, "edit", editConfig())
	out, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"patch":"***"}`))
	if err != nil || stub.calls != 1 {
		t.Fatalf("edit 档 EditPass 命中应直落（calls=%d err=%v）", stub.calls, err)
	}
	if string(out) != "{}" {
		t.Fatalf("直落返回值异常：%s", out)
	}
	if src.decision != nil {
		t.Fatal("直落不应触碰决议源")
	}
}

func TestEditNonPassFallsBackToManual(t *testing.T) {
	stub := &countingTool{name: "run_command"}
	src := &stubDecisions{}
	wrapped := WrapTools([]contract.Tool{stub}, src, "edit", editConfig())
	_, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"command":"rm -rf /tmp/x"}`))
	su, ok := err.(*contract.Suspend)
	if !ok {
		t.Fatalf("edit 档名单外写工具应回落 manual 挂起，得到：%v", err)
	}
	card, ok := su.Info.(contract.ApprovalCard)
	if !ok || card.Tool != "run_command" {
		t.Fatalf("挂起载荷应为审批卡：%+v", su.Info)
	}
	if stub.calls != 0 {
		t.Fatal("挂起时不应执行内层")
	}
	// 批准续行（legacy 单卡挂起态注回，同 guarded 两跳形态）。
	ctx := contract.WithResumeState(context.Background(), approvalState{Args: `{"command":"rm -rf /tmp/x"}`})
	src.decision = &contract.ApprovalDecision{Approve: true}
	if _, err := wrapped[0].Invoke(ctx, json.RawMessage(`{"command":"rm -rf /tmp/x"}`)); err != nil || stub.calls != 1 {
		t.Fatalf("决议到达应代执行（calls=%d err=%v）", stub.calls, err)
	}
}

func TestEditNilPassDegradesToManual(t *testing.T) {
	cfg := testConfig() // 无 EditPass
	stub := &countingTool{name: "apply_patch"}
	src := &stubDecisions{}
	wrapped := WrapTools([]contract.Tool{stub}, src, "edit", cfg)
	_, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"patch":"***"}`))
	if _, ok := err.(*contract.Suspend); !ok {
		t.Fatalf("EditPass nil 应退化 manual 挂起，得到：%v", err)
	}
}

func TestEditArgsForceStillSuspends(t *testing.T) {
	cfg := editConfig()
	cfg.ArgsForce = map[string]func(args string) bool{
		"apply_patch": func(args string) bool { return true },
	}
	stub := &countingTool{name: "apply_patch"}
	src := &stubDecisions{}
	wrapped := WrapTools([]contract.Tool{stub}, src, "edit", cfg)
	_, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"patch":"***"}`))
	if _, ok := err.(*contract.Suspend); !ok {
		t.Fatalf("ArgsForce 判断权在人的写值应仍挂起（edit 不豁免），得到：%v", err)
	}
}

func TestEditApprovalNoGrantAndAlwaysExempt(t *testing.T) {
	stub := &countingTool{name: "run_command"}
	src := &stubDecisions{decision: &contract.ApprovalDecision{Approve: true}}
	wrapped := WrapTools([]contract.Tool{stub}, src, "edit", editConfig())
	// 第一跳：挂起。
	_, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"command":"rm -rf /tmp/x"}`))
	if _, ok := err.(*contract.Suspend); !ok {
		t.Fatalf("应挂起：%v", err)
	}
	if src.granted || src.taskGrant {
		t.Fatal("edit 批准语义不应预授本轮/任务期写权")
	}
	// 第二跳：批准代执行本次。
	ctx := contract.WithResumeState(context.Background(), approvalState{Args: `{"command":"rm -rf /tmp/x"}`})
	if _, err := wrapped[0].Invoke(ctx, json.RawMessage(`{"command":"rm -rf /tmp/x"}`)); err != nil || stub.calls != 1 {
		t.Fatalf("决议到达应代执行（calls=%d err=%v）", stub.calls, err)
	}
	// 第三跳：再遇名单外写仍挂起（逐次语义，非授权缓存）。
	_, err = wrapped[0].Invoke(context.Background(), json.RawMessage(`{"command":"rm -rf /tmp/y"}`))
	if _, ok := err.(*contract.Suspend); !ok {
		t.Fatalf("edit 回落分支应逐次询问，得到：%v", err)
	}
	// 会话域 Always 授权（1198fa3）对 edit 回落分支同样豁免。
	src.grants = map[string]bool{"run_command": true}
	if _, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"command":"rm -rf /tmp/z"}`)); err != nil || stub.calls != 2 {
		t.Fatalf("会话域写授权应豁免 edit 回落分支（calls=%d err=%v）", stub.calls, err)
	}
}
