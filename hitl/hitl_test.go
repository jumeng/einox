package hitl

// P0 收紧回归（自产品 interrupt_test 迁入）：写工具分类（名单 + mcp_ 前缀
// fail-closed）、run_command 参数级只读豁免（白名单命令直过，写命令全审批）、
// fail-closed（恢复无决议一律拒绝）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	runcommandtool "github.com/jumeng/einox/tools/runcommand"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
)

func testConfig() ApprovalConfig {
	return ApprovalConfig{
		WriteTools: map[string]bool{
			"create_issues": true, "data_sync": true,
			"python_execute": true,
			"requests_post":  true, "requests_put": true, "requests_delete": true,
			"run_command": true, "delete_file": true, "apply_patch": true,
			"write_spreadsheet": true,
		},
		WritePrefix: []string{"mcp_"},
		ArgsSkip:    map[string]func(args string) bool{"run_command": runcommandtool.IsSafeReadCommand},
		Actions:     map[string]string{"create_issues": "创建需求"},
	}
}

func TestWriteToolClassification(t *testing.T) {
	cfg := testConfig()
	for name, want := range map[string]bool{
		// 写工具（业务样本）
		"create_issues": true, "data_sync": true,
		// 执行面
		"python_execute": true,
		"requests_post":  true, "requests_put": true, "requests_delete": true,
		"run_command": true, "delete_file": true, "apply_patch": true,
		"write_spreadsheet": true,
		// mcp_ 前缀 fail-closed
		"mcp_anything": true, "mcp_query": true,
		// 读工具裸传
		"request_get": false, "list_issues": false, "read_document": false,
		"read_file": false, "search_files": false, "task_output": false,
		"get_current_time": false, "web_fetch": false, "search_all": false,
	} {
		if got := cfg.IsWrite(name); got != want {
			t.Errorf("IsWrite(%q)=%v，应为 %v", name, got, want)
		}
	}
}

func TestRunCommandSafeSkip(t *testing.T) {
	skip := testConfig().ArgsSkip["run_command"]
	if skip == nil {
		t.Fatal("run_command 缺参数级豁免")
	}
	for _, args := range []string{
		`{"command":"ls -la"}`,
		`{"command":"git status"}`,
		`{"command":"cat a.txt"}`,
	} {
		if !skip(args) {
			t.Errorf("只读命令应豁免：%s", args)
		}
	}
	for _, args := range []string{
		`{"command":"rm -rf /"}`,
		`{"command":"cat a | sh"}`,
		`{"command":"go test ./..."}`,
		`not json`,
	} {
		if skip(args) {
			t.Errorf("写命令/坏参数不应豁免（fail-closed）：%s", args)
		}
	}
}

// countingTool 计数桩（豁免路径直通验证）。
type countingTool struct {
	name  string
	calls int
}

func (c *countingTool) Info() *contract.ToolInfo {
	return &contract.ToolInfo{Name: c.name, Desc: "stub"}
}

func (c *countingTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	c.calls++
	return json.RawMessage("{}"), nil
}

func TestWrapRunCommandPassthrough(t *testing.T) {
	stub := &countingTool{name: "run_command"}
	wrapped := WrapTools([]contract.Tool{stub}, nil, "manual", testConfig())
	if len(wrapped) != 1 {
		t.Fatalf("包装数量异常：%d", len(wrapped))
	}
	out, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"command":"git status"}`))
	if err != nil || stub.calls != 1 {
		t.Fatalf("只读命令应直通内层（calls=%d err=%v）", stub.calls, err)
	}
	if string(out) != "{}" {
		t.Fatalf("直通返回值异常：%s", out)
	}
}

// stubDecisions 决议源桩。
type stubDecisions struct {
	granted   bool
	taskGrant bool
	decision  *contract.ApprovalDecision
	byItem    map[string]*contract.ApprovalDecision // 合并决议多槽（item_id → 决议）
	resumeCtx bool
}

func (d *stubDecisions) TakeDecision() *contract.ApprovalDecision { return d.decision }

// TakeDecisionFor 按项取用（合并决议多槽）；取后清空防复用（对齐真源语义）。
func (d *stubDecisions) TakeDecisionFor(itemID string) *contract.ApprovalDecision {
	if dec, ok := d.byItem[itemID]; ok {
		delete(d.byItem, itemID)
		return dec
	}
	return nil
}

func (d *stubDecisions) TurnGranted() bool { return d.granted }
func (d *stubDecisions) GrantTurn()        { d.granted = true }
func (d *stubDecisions) TaskGranted() bool { return d.taskGrant }
func (d *stubDecisions) GrantTask()        { d.taskGrant = true }

func TestManualModeSuspends(t *testing.T) {
	stub := &countingTool{name: "create_issues"}
	src := &stubDecisions{}
	wrapped := WrapTools([]contract.Tool{stub}, src, "manual", testConfig())
	_, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{"issues":[]}`))
	su, ok := err.(*contract.Suspend)
	if !ok {
		t.Fatalf("manual 写调用应挂起，得到：%v", err)
	}
	card, ok := su.Info.(contract.ApprovalCard)
	if !ok || card.Tool != "create_issues" || card.Action != "创建需求" {
		t.Fatalf("挂起载荷应为审批卡：%+v", su.Info)
	}
	if stub.calls != 0 {
		t.Fatal("挂起时不应执行内层")
	}
}

func TestPlanModeGrantAndAutoPass(t *testing.T) {
	// auto：直通
	stub := &countingTool{name: "create_issues"}
	wrapped := WrapTools([]contract.Tool{stub}, &stubDecisions{}, "auto", testConfig())
	if _, err := wrapped[0].Invoke(context.Background(), json.RawMessage(`{}`)); err != nil || stub.calls != 1 {
		t.Fatalf("auto 应直通：%v calls=%d", err, stub.calls)
	}
	// plan 本轮已授权：直通
	stub2 := &countingTool{name: "create_issues"}
	wrapped2 := WrapTools([]contract.Tool{stub2}, &stubDecisions{granted: true}, "plan", testConfig())
	if _, err := wrapped2[0].Invoke(context.Background(), json.RawMessage(`{}`)); err != nil || stub2.calls != 1 {
		t.Fatalf("plan 已授权应直通：%v calls=%d", err, stub2.calls)
	}
	// plan 任务期已授权（计划获批，本轮未授权）：直通——「批准计划 = 一口气执行」
	stub3 := &countingTool{name: "create_issues"}
	wrapped3 := WrapTools([]contract.Tool{stub3}, &stubDecisions{taskGrant: true}, "plan", testConfig())
	if _, err := wrapped3[0].Invoke(context.Background(), json.RawMessage(`{}`)); err != nil || stub3.calls != 1 {
		t.Fatalf("plan 任务期授权应直通：%v calls=%d", err, stub3.calls)
	}
}

// TestMergedDecisionMultiSlotDispatch 合并决议多槽分发（H4-2）：挂起时卡与
// 保存态双带 item_id；Resume 重放时各工具按保存态领各自项的决议——A 批 B 拒
// 各自生效（一卡两项的正反例在单元面的最小复现）。
func TestMergedDecisionMultiSlotDispatch(t *testing.T) {
	stubA := &countingTool{name: "create_issues"}
	stubB := &countingTool{name: "update_issue_fields"}
	src := &stubDecisions{byItem: map[string]*contract.ApprovalDecision{}}
	ts := WrapTools([]contract.Tool{stubA, stubB}, src, "manual", forceConfig())

	// 挂起：收卡与保存态，按 item_id 登记决议（A 批准 / B 拒绝）
	states := map[string]approvalState{}
	for _, tt := range ts {
		_, err := tt.Invoke(context.Background(), json.RawMessage(`{}`))
		su, ok := err.(*contract.Suspend)
		if !ok {
			t.Fatalf("manual 写调用应挂起，得到：%v", err)
		}
		card := su.Info.(contract.ApprovalCard)
		if card.ItemID == "" {
			t.Fatal("挂起卡应携带 item_id（合并决议多槽依据）")
		}
		st := su.State.(approvalState)
		if st.ItemID != card.ItemID {
			t.Fatalf("保存态与卡 item_id 应一致：%q vs %q", st.ItemID, card.ItemID)
		}
		states[card.Tool] = st
		approve := card.Tool == "create_issues"
		src.byItem[card.ItemID] = &contract.ApprovalDecision{Approve: approve, Reason: "测"}
	}
	// 重放：各工具经保存态 item_id 领各自决议
	outA, err := ts[0].Invoke(contract.WithResumeState(context.Background(), states["create_issues"]), json.RawMessage(`{}`))
	if err != nil || stubA.calls != 1 {
		t.Fatalf("A 项批准应执行：%v calls=%d", err, stubA.calls)
	}
	_ = outA
	outB, err := ts[1].Invoke(contract.WithResumeState(context.Background(), states["update_issue_fields"]), json.RawMessage(`{}`))
	if err != nil || stubB.calls != 0 {
		t.Fatalf("B 项拒绝不得执行：%v calls=%d", err, stubB.calls)
	}
	if !strings.Contains(string(outB), "disapproved") {
		t.Fatalf("B 项拒绝应以工具结果回喂（disapproved），实得 %s", outB)
	}
}

// TestMergedDecisionFailClosedPerItem 逐项 fail-closed：恢复流带 item_id 但
// 决议表无该项（无决议到达）→ 该项拒绝、绝不放行（粒度从整批细化到项）。
func TestMergedDecisionFailClosedPerItem(t *testing.T) {
	stub := &countingTool{name: "create_issues"}
	src := &stubDecisions{byItem: map[string]*contract.ApprovalDecision{}}
	ts := WrapTools([]contract.Tool{stub}, src, "manual", testConfig())
	_, err := ts[0].Invoke(context.Background(), json.RawMessage(`{}`))
	su := err.(*contract.Suspend)
	st := su.State.(approvalState)
	if st.ItemID == "" {
		t.Fatal("保存态应携带 item_id")
	}
	// byItem 空：无决议到达 → fail-closed 拒绝
	out, err := ts[0].Invoke(contract.WithResumeState(context.Background(), st), json.RawMessage(`{}`))
	if err != nil || stub.calls != 0 {
		t.Fatalf("无决议项应拒绝不执行：%v calls=%d", err, stub.calls)
	}
	if !strings.Contains(string(out), "fail-closed") {
		t.Fatalf("拒绝反馈应含 fail-closed 说明，实得 %s", out)
	}
}

// TestLegacyResumeWithoutItemID 旧挂起态兼容（升级前 checkpoint 重放）：保存态
// 无 item_id → 走 "" 槽单决议（既有语义零变化）。
func TestLegacyResumeWithoutItemID(t *testing.T) {
	stub := &countingTool{name: "create_issues"}
	src := &stubDecisions{decision: &contract.ApprovalDecision{Approve: true}}
	ts := WrapTools([]contract.Tool{stub}, src, "manual", testConfig())
	out, err := ts[0].Invoke(contract.WithResumeState(context.Background(), approvalState{Args: `{}`}), json.RawMessage(`{}`))
	if err != nil || stub.calls != 1 {
		t.Fatalf("旧态恢复应按单决议执行：%v calls=%d", err, stub.calls)
	}
	_ = out
}

// forceConfig 带 ArgsForce 的测试配置：update_issue_fields 且 fields.status=完成
// 命中强制审批（产品侧判定逻辑的基座侧镜像样本）。
func forceConfig() ApprovalConfig {
	cfg := testConfig()
	cfg.WriteTools["update_issue_fields"] = true
	cfg.Actions["update_issue_fields"] = "更新需求字段"
	cfg.ArgsForce = map[string]func(args string) bool{
		"update_issue_fields": func(args string) bool {
			var in struct {
				Updates []struct {
					Fields struct {
						Status *string `json:"status"`
					} `json:"fields"`
				} `json:"updates"`
			}
			if json.Unmarshal([]byte(args), &in) != nil {
				return true // 坏参数 fail-closed 按需审批
			}
			for _, u := range in.Updates {
				if u.Fields.Status != nil && *u.Fields.Status == "完成" {
					return true
				}
			}
			return false
		},
	}
	cfg.ForceNotes = map[string]string{
		"update_issue_fields": "置「完成」属人工判断：本确认不可被模式/计划授权替代",
	}
	return cfg
}

const forceArgs = `{"updates":[{"id":"a1","fields":{"status":"完成"}}]}`
const plainArgs = `{"updates":[{"id":"a1","fields":{"title":"改名"}}]}`

func TestArgsForceSuspendsDespiteModeAndGrant(t *testing.T) {
	// auto 档（原直通）命中强制审批 → 挂起，卡文案为 ForceNotes
	stub := &countingTool{name: "update_issue_fields"}
	wrapped := WrapTools([]contract.Tool{stub}, &stubDecisions{}, "auto", forceConfig())
	_, err := wrapped[0].Invoke(context.Background(), json.RawMessage(forceArgs))
	su, ok := err.(*contract.Suspend)
	if !ok {
		t.Fatalf("auto 档强制参数应挂起，得到：%v", err)
	}
	card, ok := su.Info.(contract.ApprovalCard)
	if !ok {
		t.Fatalf("挂起载荷应为审批卡：%+v", su.Info)
	}
	if card.Note != "置「完成」属人工判断：本确认不可被模式/计划授权替代" || stub.calls != 0 {
		t.Fatalf("强制卡文案应覆盖且不执行：%+v calls=%d", card, stub.calls)
	}
	// plan 档任务期已授权（原直通）仍挂起
	stub2 := &countingTool{name: "update_issue_fields"}
	wrapped2 := WrapTools([]contract.Tool{stub2}, &stubDecisions{taskGrant: true}, "plan", forceConfig())
	if _, err := wrapped2[0].Invoke(context.Background(), json.RawMessage(forceArgs)); err == nil {
		t.Fatal("plan 任务期授权下强制参数仍应挂起")
	} else if _, ok := err.(*contract.Suspend); !ok {
		t.Fatalf("应为挂起信号：%v", err)
	}
	// 同工具非强制参数不受影响：auto 直通
	stub3 := &countingTool{name: "update_issue_fields"}
	wrapped3 := WrapTools([]contract.Tool{stub3}, &stubDecisions{}, "auto", forceConfig())
	if _, err := wrapped3[0].Invoke(context.Background(), json.RawMessage(plainArgs)); err != nil || stub3.calls != 1 {
		t.Fatalf("auto 非强制参数应直通：%v calls=%d", err, stub3.calls)
	}
	// 强制挂起批准 → 以保存参数执行（恢复流复用）
	ctx := contract.WithResumeState(context.Background(), approvalState{Args: forceArgs})
	src := &stubDecisions{decision: &contract.ApprovalDecision{Approve: true}}
	stub4 := &countingTool{name: "update_issue_fields"}
	wrapped4 := WrapTools([]contract.Tool{stub4}, src, "auto", forceConfig())
	if _, err := wrapped4[0].Invoke(ctx, json.RawMessage(forceArgs)); err != nil || stub4.calls != 1 {
		t.Fatalf("强制审批批准后应执行：%v calls=%d", err, stub4.calls)
	}
}

// 卡 diff 载荷：实现 ApprovalDiff 的写工具，挂起卡带 Diff；未实现的没有。
func TestApprovalCardDiff(t *testing.T) {
	// 带 ApprovalDiff 的写工具：InferTool 件经 DiffToolOf 包装暴露可选接口
	const patchText = "*** Begin Patch\n*** Update File: a.txt\n@@\n-x\n+y\n*** End Patch"
	base, err := tools.InferTool("apply_patch", "stub",
		func(_ context.Context, _ struct {
			Patch string `json:"patch"`
		}) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	dt := tools.DiffToolOf(base, func(args string) string {
		var in struct {
			Patch string `json:"patch"`
		}
		if json.Unmarshal([]byte(args), &in) != nil {
			return ""
		}
		return in.Patch
	})
	wrapped := WrapTools([]contract.Tool{dt}, &stubDecisions{}, "manual", testConfig())
	argsJSON, err := json.Marshal(struct {
		Patch string `json:"patch"`
	}{Patch: patchText})
	if err != nil {
		t.Fatal(err)
	}
	_, err = wrapped[0].Invoke(context.Background(), argsJSON)
	su, ok := err.(*contract.Suspend)
	if !ok {
		t.Fatalf("manual 写调用应挂起，得到：%v", err)
	}
	card, ok := su.Info.(contract.ApprovalCard)
	if !ok {
		t.Fatalf("挂起载荷应为审批卡：%+v", su.Info)
	}
	if card.Diff != patchText {
		t.Fatalf("挂起卡应带 diff 载荷：%q", card.Diff)
	}
	// 未实现 ApprovalDiff 的写工具：卡不带 diff
	stub := &countingTool{name: "create_issues"}
	wrapped2 := WrapTools([]contract.Tool{stub}, &stubDecisions{}, "manual", testConfig())
	_, err = wrapped2[0].Invoke(context.Background(), json.RawMessage(`{}`))
	su2, ok := err.(*contract.Suspend)
	if !ok {
		t.Fatalf("manual 写调用应挂起，得到：%v", err)
	}
	if card2, ok := su2.Info.(contract.ApprovalCard); !ok || card2.Diff != "" {
		t.Fatalf("未实现 ApprovalDiff 的工具卡不应带 diff：%+v", su2.Info)
	}
}

func TestResumeFailClosed(t *testing.T) {
	stub := &countingTool{name: "create_issues"}
	src := &stubDecisions{}
	wrapped := WrapTools([]contract.Tool{stub}, src, "manual", testConfig())
	// 恢复流（ctx 注入恢复态）但无决议 → fail-closed 拒绝信封
	ctx := contract.WithResumeState(context.Background(), approvalState{Args: `{}`})
	out, err := wrapped[0].Invoke(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("fail-closed 应转信封不上抛：%v", err)
	}
	if !strings.Contains(string(out), "fail-closed") || stub.calls != 0 {
		t.Fatalf("fail-closed 应拒绝且不执行：%s calls=%d", out, stub.calls)
	}
	// 拒绝决议 → 拒绝信封
	src.decision = &contract.ApprovalDecision{Approve: false, Reason: "范围不对"}
	out, err = wrapped[0].Invoke(ctx, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(out), "disapproved: 范围不对") {
		t.Fatalf("拒绝应回喂原因：%s err=%v", out, err)
	}
	// 批准 → 以挂起时参数执行
	src.decision = &contract.ApprovalDecision{Approve: true}
	planSrc := &stubDecisions{decision: src.decision}
	planWrapped := WrapTools([]contract.Tool{stub}, planSrc, "plan", testConfig())
	if _, err := planWrapped[0].Invoke(ctx, json.RawMessage(`{"orig":false}`)); err != nil || stub.calls != 1 {
		t.Fatalf("批准应按保存参数执行：%v calls=%d", err, stub.calls)
	}
	if !planSrc.granted {
		t.Fatal("plan 批准应授权本轮")
	}
}
