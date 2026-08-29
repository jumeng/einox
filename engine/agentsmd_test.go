package engine

// AGENTS.md 注入缝回归：清单文件内容注入模型输入（transient user 消息、
// 首个 user 消息之前）；空清单不挂零注入；字节预算超限跳过余下文件。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

func writeAgentsMD(t *testing.T, dir, marker string) string {
	t.Helper()
	p := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(p, []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runOnce(t *testing.T, m *Manager) *scriptedModel {
	t.Helper()
	s := m.Registry().Create("张三", "注", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "问", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if s.StateOf() != session.StateEnded {
		t.Fatalf("应正常收线：%s", s.StateOf())
	}
	return nil
}

func TestAgentsMDInjectedBeforeFirstUser(t *testing.T) {
	dir := t.TempDir()
	p := writeAgentsMD(t, dir, "MARKER_XYZ 用户级约定")
	fm := &scriptedModel{}
	m := newSeamManager(t, func(o *Options) {
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) { return fm, nil }
		o.AgentsMD = func(SessionBrief) []string { return []string{p} }
	})
	runOnce(t, m)
	if len(fm.inputs) == 0 {
		t.Fatal("应有模型调用")
	}
	in := fm.inputs[0]
	idx, injected := -1, false
	for i, msg := range in {
		if msg.Role == "user" && strings.Contains(msgTextOf(msg), "问") {
			idx = i
		}
		if strings.Contains(msgTextOf(msg), "MARKER_XYZ") {
			injected = true
			if idx >= 0 && i > idx {
				t.Fatalf("注入应在首个业务 user 消息之前（注入位 %d，user 位 %d）", i, idx)
			}
		}
	}
	if !injected {
		t.Fatal("AGENTS.md 内容应注入模型输入")
	}
}

func TestAgentsMDEmptyListNoInjection(t *testing.T) {
	fm := &scriptedModel{}
	m := newSeamManager(t, func(o *Options) {
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) { return fm, nil }
		o.AgentsMD = func(SessionBrief) []string { return nil }
	})
	runOnce(t, m)
	if len(fm.inputs) == 0 {
		t.Fatal("应有模型调用")
	}
	for _, msg := range fm.inputs[0] {
		if strings.Contains(msgTextOf(msg), "system-reminder") || strings.Contains(msgTextOf(msg), "AGENTS") {
			t.Fatalf("空清单不应注入：%s", msgTextOf(msg))
		}
	}
}

func TestAgentsMDMaxBytesSkipsOverflow(t *testing.T) {
	dir := t.TempDir()
	big := writeAgentsMD(t, dir, strings.Repeat("A", 64*1024))
	smallMarker := filepath.Join(dir, "SMALL.md")
	if err := os.WriteFile(smallMarker, []byte("MARKER_SMALL"), 0o644); err != nil {
		t.Fatal(err)
	}
	fm := &scriptedModel{}
	m := newSeamManager(t, func(o *Options) {
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) { return fm, nil }
		o.AgentsMD = func(SessionBrief) []string { return []string{big, smallMarker} } // 首文件即超 32KiB 预算
		o.AgentsMDMaxBytes = 32 * 1024
	})
	runOnce(t, m)
	for _, msg := range fm.inputs[0] {
		if strings.Contains(msgTextOf(msg), "MARKER_SMALL") {
			t.Fatal("预算耗尽后余下文件应被跳过（上游按序装载超限即止）")
		}
	}
}

// TestAgentsMDImportRecursion @import 递归：清单文件内 @sub.md 引用被装载
//（相对宿主目录解析、上游深度上限 5）。
func TestAgentsMDImportRecursion(t *testing.T) {
	dir := t.TempDir()
	main := writeAgentsMD(t, dir, "MARKER_MAIN 与递归引用：\n@sub.md\n")
	sub := filepath.Join(dir, "sub.md")
	if err := os.WriteFile(sub, []byte("MARKER_SUB_IMPORTED"), 0o644); err != nil {
		t.Fatal(err)
	}
	fm := &scriptedModel{}
	m := newSeamManager(t, func(o *Options) {
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) { return fm, nil }
		o.AgentsMD = func(SessionBrief) []string { return []string{main} }
	})
	runOnce(t, m)
	joined := ""
	for _, msg := range fm.inputs[0] {
		joined += msgTextOf(msg)
	}
	if !strings.Contains(joined, "MARKER_MAIN") || !strings.Contains(joined, "MARKER_SUB_IMPORTED") {
		t.Fatalf("主文件与 @import 目标都应注入：%s", head(joined, 200))
	}
}
