package hitl

// guarded 第五档回归（findings/2026-10-31-hitl-guarded-mode-design.md）：
// ①GuardedRisk 命中挂起+批准续行；②未命中直落零中断；③ArgsForce 优先于
// guarded 直落语义；④GuardedRisk nil 退化 auto；⑤guarded 批准不授任务期/
// 本轮写权（逐次语义——风险集每次询问，批准只代执行本次）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jumeng/einox/contract"
)

func guardedConfig() ApprovalConfig {
	cfg := testConfig()
	cfg.GuardedRisk = map[string]func(args string) bool{
		// 风险集样本：删除类命令命中（内容归应用；此处仅机制回归）。
		"run_command": func(args string) bool { return strings.Contains(args, "rm -rf") },
	}
	return cfg
}

func TestGuardedRiskHitSuspends(t *testing.T) {
	stub := &countingTool{name: "run_command"}
	src := &stubDecisions{}
	wrapped := WrapTools([]contract.Tool{stub}, src, "guarded", guardedConfig())
	_, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"command":"rm -rf /tmp/x"}`))
	su, ok := err.(*contract.Suspend)
	if !ok {
		t.Fatalf("guarded 风险命中应挂起，得到：%v", err)
	}
	card, ok := su.Info.(contract.ApprovalCard)
	if !ok || card.Tool != "run_command" {
		t.Fatalf("挂起载荷应为审批卡：%+v", su.Info)
	}
	if stub.calls != 0 {
		t.Fatal("挂起时不应执行内层")
	}
}

func TestGuardedNonRiskPassesThrough(t *testing.T) {
	stub := &countingTool{name: "create_issues"}
	src := &stubDecisions{}
	wrapped := WrapTools([]contract.Tool{stub}, src, "guarded", guardedConfig())
	out, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"issues":[]}`))
	if err != nil || stub.calls != 1 {
		t.Fatalf("guarded 非风险写应直落（calls=%d err=%v）", stub.calls, err)
	}
	if string(out) != "{}" {
		t.Fatalf("直落返回值异常：%s", out)
	}
	// 直落不消耗决议（挂起路径才领决议）。
	if src.decision != nil {
		t.Fatal("直落不应触碰决议源")
	}
}

func TestGuardedArgsForceStillSuspends(t *testing.T) {
	cfg := guardedConfig()
	cfg.ArgsForce = map[string]func(args string) bool{
		"create_issues": func(args string) bool { return strings.Contains(args, `"status":"done"`) },
	}
	stub := &countingTool{name: "create_issues"}
	src := &stubDecisions{}
	wrapped := WrapTools([]contract.Tool{stub}, src, "guarded", cfg)
	_, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"issues":[],"status":"done"}`))
	if _, ok := err.(*contract.Suspend); !ok {
		t.Fatalf("ArgsForce 判断权在人的写值应仍挂起（guarded 不豁免），得到：%v", err)
	}
}

func TestGuardedNilRiskDegradesToAuto(t *testing.T) {
	cfg := testConfig() // 无 GuardedRisk
	stub := &countingTool{name: "run_command"}
	src := &stubDecisions{}
	wrapped := WrapTools([]contract.Tool{stub}, src, "guarded", cfg)
	out, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"command":"rm -rf /tmp/x"}`))
	if err != nil || stub.calls != 1 {
		t.Fatalf("GuardedRisk nil 应退化 auto 直落（calls=%d err=%v）", stub.calls, err)
	}
	if string(out) != "{}" {
		t.Fatalf("直落返回值异常：%s", out)
	}
}

func TestGuardedApprovalExecutesOnceWithoutGrant(t *testing.T) {
	stub := &countingTool{name: "run_command"}
	src := &stubDecisions{decision: &contract.ApprovalDecision{Approve: true}}
	wrapped := WrapTools([]contract.Tool{stub}, src, "guarded", guardedConfig())
	// 第一跳：挂起（拿到保存态）。
	_, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"command":"rm -rf /tmp/x"}`))
	su, ok := err.(*contract.Suspend)
	if !ok {
		t.Fatalf("应挂起：%v", err)
	}
	if src.granted || src.taskGrant {
		t.Fatal("guarded 批准语义不应预授本轮/任务期写权")
	}
	// 第二跳：经 Resume 注回保存态——决议到达即代执行本次（同既有两跳测试
	// 形态：legacy 单卡挂起态注回，决议源单槽供决议）。
	_ = su
	ctx := contract.WithResumeState(context.Background(), approvalState{Args: `{"command":"rm -rf /tmp/x"}`})
	out, err := wrapped[0].Invoke(ctx, json.RawMessage(`{"command":"rm -rf /tmp/x"}`))
	if err != nil || stub.calls != 1 {
		t.Fatalf("决议到达应代执行（calls=%d err=%v）", stub.calls, err)
	}
	if string(out) != "{}" {
		t.Fatalf("代执行返回值异常：%s", out)
	}
	// 第三跳：再遇风险仍挂起（逐次语义，非授权缓存）。
	_, err = wrapped[0].Invoke(context.Background(), json.RawMessage(`{"command":"rm -rf /tmp/y"}`))
	if _, ok := err.(*contract.Suspend); !ok {
		t.Fatalf("guarded 风险集应逐次询问，得到：%v", err)
	}
}
