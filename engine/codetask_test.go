package engine

// 编码闭环引擎级链路用例（2026-08-25 设计验收硬性要求）：驱动真实 Run 循环
// 而非直调工具——
// ① 任务收尾 wipe：WorkspaceKeep 声明的持久子区保留、其余临时产物照清
//    （StateEnded → settleTurn → wipeWorkspace 链路）；
// ② apply_patch ArgsForce 挂起：auto 档不豁免、审批卡携带补丁原文
//   （DiffToolOf → hitl 探测 ApprovalDiff → pump 透传 ApprovalReq.Diff 全链）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/checkpoint"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/internal/tstore"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// TestTaskEndWipeKeepsDeclaredDirs 任务收尾 wipe：WorkspaceKeep 声明的持久
// 子区保留、其余照清（scriptedModel 一轮纯文本收尾 → StateEnded →
// wipeWorkspace——v0.4.0 起豁免须显式声明，基座不预设目录名）。
func TestTaskEndWipeKeepsDeclaredDirs(t *testing.T) {
	m := newSeamManager(t, func(o *Options) { o.WorkspaceKeep = []string{"repos"} })
	s := m.Registry().Create("张三", "编码", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)

	// Run 前手工铺工作区（路径与 Options.WorkspaceRoot 同源拼接）：声明持久
	// 区占位 + 临时脚本 + spill 溢出产物
	ws := m.Opt.WorkspaceRoot("张三", s.SID)
	if err := os.MkdirAll(filepath.Join(ws, "repos", "base-app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "repos", "base-app", "a.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "tmp.sh"), []byte("echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "spill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "spill", "01.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.Run(context.Background(), s, "干活", nil, func(session.Event) {})
	waitTitleFlight(t, s) // genTitle 在途写收尾后再断言/清理

	if s.StateOf() != session.StateEnded {
		t.Fatalf("纯文本一轮应收尾：%s", s.StateOf())
	}
	if _, err := os.Stat(filepath.Join(ws, "tmp.sh")); err == nil {
		t.Fatal("临时脚本应随任务收尾清除")
	}
	if _, err := os.Stat(filepath.Join(ws, "spill")); err == nil {
		t.Fatal("spill 应随任务收尾清除")
	}
	if _, err := os.Stat(filepath.Join(ws, "repos", "base-app", "a.go")); err != nil {
		t.Fatal("声明的持久子区应跨任务保留：", err)
	}
}

// TestApplyPatchApprovalCardDiff apply_patch ArgsForce 端到端：auto 档写面
// 命中 ArgsForce 即挂起，审批卡携带补丁原文（applyCardDiff 透传——
// DiffToolOf → hitl 探测 → pump 透传链；挂起即收线，无需 Resume）。
func TestApplyPatchApprovalCardDiff(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: out/demo.txt\n+demo line\n*** End Patch"
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				ID: "c-patch-1", Type: "function",
				Function: schema.FunctionCall{Name: "apply_patch", Arguments: fmt.Sprintf(`{"patch":%q}`, patch)},
			}}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}

	st := tstore.New(t.TempDir())
	m, err := NewManager(session.NewRegistry(st), Options{
		Providers: func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{{ID: "m", Input: []string{"text"}, Priority: 100}},
			}}
		},
		Instruction: func(SessionBrief) string { return "test" },
		NewModel: func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		},
		CheckPoints: func(operator, sid string) CheckPointStore {
			return checkpoint.NewCheckPointStore(st, operator, sid)
		},
		// auto 档：apply_patch（写名单内）本可直落，ArgsForce 无视模式豁免
		// 一律挂起——对齐「整补丁人工确认」装配形态
		Approval: hitl.ApprovalConfig{
			WriteTools: map[string]bool{"apply_patch": true},
			ArgsForce:  map[string]func(args string) bool{"apply_patch": func(string) bool { return true }},
		},
		WorkspaceRoot: func(owner, sid string) string { return st.TmpDir() + "/ws/" + owner + "/" + sid },
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	s := m.Registry().Create("张三", "提交", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)

	var names []string
	var card *contract.ApprovalReq
	m.Run(context.Background(), s, "落一个文件", nil, func(ev session.Event) {
		names = append(names, ev.Event)
		if ev.Event == contract.EvApprovalRequest {
			req, ok := ev.Data.(contract.ApprovalReq)
			if !ok {
				t.Fatalf("approval_request 载荷应为 contract.ApprovalReq：%T", ev.Data)
			}
			card = &req
		}
	})
	t.Cleanup(func() { stopApprovalTimer(s.SID) }) // 挂起超时器不跨用例残留

	if !contains(names, contract.EvApprovalRequest) {
		t.Fatalf("apply_patch ArgsForce 应挂起（auto 档不豁免）：%v", names)
	}
	if card == nil || card.Tool != "apply_patch" {
		t.Fatalf("应捕获 apply_patch 审批卡：%+v", card)
	}
	// 断言面在 ApprovalReq.Diff 本体——applyCardDiff 透传补丁原文（含 + 行）
	if !strings.Contains(card.Diff, "+demo line") {
		t.Fatalf("审批卡应携带补丁原文（含 + 新行）：%q", card.Diff)
	}
	if s.StateOf() != session.StatePendingApproval {
		t.Fatalf("挂起即收线，应为 pending_approval：%s", s.StateOf())
	}
}
