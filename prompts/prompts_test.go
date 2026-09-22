package prompts

import (
	"strings"
	"testing"
)

// TestEnvironment 环境段拼装：有信息行保留、空素材行省略、全空仅根行。
func TestEnvironment(t *testing.T) {
	got := Environment(EnvironmentFacts{
		WorkspaceRoot: "/ws/s1", KeepDirs: []string{"repos/ —— 挂载业务仓"},
		ProtectDirs: []string{"docs"}, DateLine: "今天是 2026-09-22 周二。",
		GitStatusLine: "main 分支，2 文件未提交", SessionMode: "manual",
	})
	for _, want := range []string{"# 环境", "/ws/s1", "repos/", "docs", "2026-09-22", "main 分支", "manual"} {
		if !strings.Contains(got, want) {
			t.Fatalf("环境段缺 %q：%s", want, got)
		}
	}
	min := Environment(EnvironmentFacts{WorkspaceRoot: "/ws/s2"})
	if strings.Contains(min, "挂载区") || strings.Contains(min, "写保护区") || strings.Contains(min, "会话档位") {
		t.Fatalf("空素材行应省略：%s", min)
	}
	if !strings.Contains(min, "/ws/s2") {
		t.Fatalf("根行不可省：%s", min)
	}
}

// TestSections 资产段非空导出（embed 生效面）。
func TestSections(t *testing.T) {
	for name, s := range map[string]string{
		"Coding": Coding(), "Orchestration": Orchestration(), "Hitl": Hitl(), "Subagents": Subagents(),
	} {
		if len(s) < 100 {
			t.Fatalf("%s 段异常短：%d", name, len(s))
		}
	}
	if !strings.Contains(Coding(), "侦察纪律") || !strings.Contains(Subagents(), "侦察子代理") {
		t.Fatal("新增节缺失")
	}
}
