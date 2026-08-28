package engine

// engine 单元回归（自产品 agent_test 的纯函数段迁入）：sanitizeHistory 防御
// 修复。行为级端到端回归（会话/审批/steering/usage 全链）在产品装配层测试
// （internal/agent）经 llmtest 假模型驱动；超长工具结果截断/外置归
// reduction_test.go（2026-08-26 newSpiller 退役，截断面统一移交 reduction）。

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
	"github.com/jumeng/einox/session"
)

func TestSanitizeHistoryRepairsEmptyArgs(t *testing.T) {
	// ① 空 arguments 回灌 "{}"
	msgs := []*schema.Message{
		schema.UserMessage("问题"),
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "t", Arguments: ""},
		}}},
		schema.ToolMessage("ok", "c1"),
	}
	out := sanitizeHistory(msgs)
	if out[1].ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("空 arguments 应回灌：%.30s", out[1].ToolCalls[0].Function.Arguments)
	}

	// ② 悬空 tool_calls 剥离（无应答 tool 消息）
	msgs = []*schema.Message{
		schema.UserMessage("问题"),
		{Role: schema.Assistant, Content: "我先调工具", ToolCalls: []schema.ToolCall{{ID: "c9", Function: schema.FunctionCall{Name: "t"}}}},
		schema.UserMessage("继续"),
	}
	out = sanitizeHistory(msgs)
	if len(out) != 3 || len(out[1].ToolCalls) != 0 || out[1].Content != "我先调工具" {
		t.Fatalf("悬空 tool_calls 应剥离（文本保留）：n=%d calls=%d", len(out), len(out[1].ToolCalls))
	}

	// ③ 空 assistant 剔除
	msgs = []*schema.Message{
		schema.UserMessage("问题"),
		{Role: schema.Assistant},
		schema.AssistantMessage("答复", nil),
	}
	out = sanitizeHistory(msgs)
	if len(out) != 2 {
		t.Fatalf("空 assistant 应剔除：%d", len(out))
	}

	// ④ 孤儿 tool 消息剔除
	msgs = []*schema.Message{
		schema.UserMessage("问题"),
		schema.ToolMessage("孤儿", "c0"),
		schema.AssistantMessage("答复", nil),
	}
	out = sanitizeHistory(msgs)
	if len(out) != 2 {
		t.Fatalf("孤儿 tool 消息应剔除：%d", len(out))
	}
}

// TestSessionToolsRepoFamily repo 工具族装配可见性：注入 RepoMounts 即装配
// 五件，nil 不装配（engine.Options 注入面契约）。
func TestSessionToolsRepoFamily(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := session.NewRegistry(st)
	m := NewManager(reg, Options{
		RepoMounts: stubMounts{},
		// WorkspaceRoot 必填（sessionTools 组装即取工作区根，nil 会 panic）
		WorkspaceRoot: func(owner, sid string) string {
			return filepath.Join(st.TmpDir(), "workspaces", owner, sid)
		},
	})
	s := m.Registry().Create("张三", "编码", "manual", contract.UserPrefs{})
	var names []string
	for _, x := range m.sessionTools(s) {
		if info := x.Info(); info != nil {
			names = append(names, info.Name)
		}
	}
	for _, want := range []string{"open_repo", "repo_status", "repo_diff", "repo_commit", "export_patch"} {
		if !contains(names, want) {
			t.Fatalf("repo 族缺 %s：%v", want, names)
		}
	}
	// nil RepoMounts = 不装配
	m2 := NewManager(session.NewRegistry(tstore.New(t.TempDir())), Options{
		WorkspaceRoot: func(owner, sid string) string {
			return filepath.Join(t.TempDir(), "workspaces", owner, sid)
		},
	})
	for _, x := range m2.sessionTools(m2.Registry().Create("张三", "x", "manual", contract.UserPrefs{})) {
		if n := x.Info().Name; strings.HasPrefix(n, "repo_") || n == "open_repo" || n == "export_patch" {
			t.Fatal("未注入 RepoMounts 不应装配 repo 族：" + n)
		}
	}
}

// stubMounts 仓定位桩（Resolve 恒未登记——只验装配可见性，不触挂载路径）。
type stubMounts struct{}

func (stubMounts) Resolve(string) (string, string, bool) { return "", "", false }
