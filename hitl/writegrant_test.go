package hitl

// D11 外部持久授权咨询回归（findings/2026-11-01-hitl-batch-a-prime-design.md；
// fail-closed）：①nil 零变化（manual 照常挂起）；②manual 命中放行；③返回
// false 照常挂起（fail-closed）；④plan 档在会话域授权（turn/task/Always）
// 之后咨询——已有授权时不调用缝；⑤ArgsForce 不被外部授权豁免。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jumeng/einox/contract"
)

func TestWriteGrantSourceNilNoChange(t *testing.T) {
	stub := &countingTool{name: "create_issues"}
	wrapped := WrapTools([]contract.Tool{stub}, &stubDecisions{}, "manual", testConfig())
	if _, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"issues":[]}`)); err == nil {
		t.Fatal("nil 缝应零变化（manual 照常挂起）")
	}
}

func TestWriteGrantSourceManualPasses(t *testing.T) {
	cfg := testConfig()
	cfg.WriteGrantSource = func(tool string) bool { return tool == "create_issues" }
	stub := &countingTool{name: "create_issues"}
	src := &stubDecisions{}
	wrapped := WrapTools([]contract.Tool{stub}, src, "manual", cfg)
	if _, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"issues":[]}`)); err != nil || stub.calls != 1 {
		t.Fatalf("外部授权命中应放行（calls=%d err=%v）", stub.calls, err)
	}
	// 名单外工具不受同缝放行（咨询入参=工具名，粒度隔离）。
	other := &countingTool{name: "data_sync"}
	wrapped2 := WrapTools([]contract.Tool{other}, src, "manual", cfg)
	if _, err := wrapped2[0].Invoke(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("缝返回 false 的工具应照常挂起（fail-closed）")
	}
}

func TestWriteGrantSourceFailClosedFalse(t *testing.T) {
	cfg := testConfig()
	called := false
	cfg.WriteGrantSource = func(string) bool { called = true; return false }
	stub := &countingTool{name: "create_issues"}
	wrapped := WrapTools([]contract.Tool{stub}, &stubDecisions{}, "manual", cfg)
	if _, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"issues":[]}`)); err == nil {
		t.Fatal("返回 false 应照常挂起（fail-closed）")
	}
	if !called {
		t.Fatal("缝应被咨询")
	}
}

func TestWriteGrantSourceAfterSessionGrants(t *testing.T) {
	cfg := testConfig()
	called := 0
	cfg.WriteGrantSource = func(string) bool { called++; return true }
	// plan 档本轮授权已授 → 不咨询（无信息泄露面，省一次回调）。
	src := &stubDecisions{granted: true}
	stub := &countingTool{name: "create_issues"}
	wrapped := WrapTools([]contract.Tool{stub}, src, "plan", cfg)
	if _, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"issues":[]}`)); err != nil || stub.calls != 1 {
		t.Fatalf("plan 本轮授权应直落（calls=%d err=%v）", stub.calls, err)
	}
	if called != 0 {
		t.Fatal("会话域授权已覆盖时不应咨询外部缝")
	}
	// 无会话授权 → 咨询且命中放行。
	src2 := &stubDecisions{}
	stub2 := &countingTool{name: "create_issues"}
	wrapped2 := WrapTools([]contract.Tool{stub2}, src2, "plan", cfg)
	if _, err := wrapped2[0].Invoke(context.Background(), json.RawMessage(`{"issues":[]}`)); err != nil || stub2.calls != 1 {
		t.Fatalf("plan 无会话授权时应经外部缝放行（calls=%d err=%v）", stub2.calls, err)
	}
	if called != 1 {
		t.Fatalf("外部缝应被咨询一次，实得 %d", called)
	}
}

func TestWriteGrantSourceNotExemptArgsForce(t *testing.T) {
	cfg := testConfig()
	cfg.WriteGrantSource = func(string) bool { return true } // 全放行
	cfg.ArgsForce = map[string]func(args string) bool{
		"create_issues": func(args string) bool { return true },
	}
	stub := &countingTool{name: "create_issues"}
	wrapped := WrapTools([]contract.Tool{stub}, &stubDecisions{}, "manual", cfg)
	_, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"issues":[]}`))
	su, ok := err.(*contract.Suspend)
	if !ok {
		t.Fatalf("ArgsForce 判断权在人的写值不被外部授权豁免，得到：%v", err)
	}
	if card, ok := su.Info.(contract.ApprovalCard); !ok || card.Note == "" {
		t.Fatalf("强制卡应带说明文案：%+v", su.Info)
	}
}
