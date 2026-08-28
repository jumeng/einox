package einox

// 基座模块守卫（守卫反转后的新形态——findings/2026-08-24-agent-base-plan.md §1）：
// ① einox 全树禁 import 产品模块（einox-pm/*）——模块图本无此路径，源码级
//    再守一层（防相对路径误引）；
// ② contract 包零 eino import（契约纯度——业务只见契约，换地基不动业务）。

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoProductImports(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-f",
		"{{.ImportPath}}\t{{range .Imports}}{{.}} {{end}}", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list 失败（可能无网络/工具链受限）: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		pkg, imports, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		for _, imp := range strings.Fields(imports) {
			if imp == "einox-pm" || strings.HasPrefix(imp, "einox-pm/") {
				t.Errorf("基座禁依赖业务层：%s → %s", pkg, imp)
			}
		}
	}
}

func TestContractZeroEino(t *testing.T) {
	cmd := exec.Command("go", "list", "-f",
		"{{.ImportPath}}\t{{range .Imports}}{{.}} {{end}}", "./contract")
	cmd.Dir, _ = filepath.Abs(".")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list 失败: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		_, imports, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		for _, imp := range strings.Fields(imports) {
			if imp == "github.com/cloudwego/eino" || strings.HasPrefix(imp, "github.com/cloudwego/eino/") {
				t.Errorf("contract 契约面禁 import eino：%s", imp)
			}
		}
	}
}
