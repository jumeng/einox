// WorkspaceMounts 装配缝测试(findings/2026-11-01-workspace-mounts-design.md):
// 挂载区(宿主自建越区 symlink)写放行;区内再越区链接仍拒;词法穿越仍拒;
// nil mounts 与 ResolveUnder 零变化。
package tools_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jumeng/einox/tools"
	"github.com/jumeng/einox/tools/applypatch"
	"github.com/jumeng/einox/tools/fsutil"
)

func TestResolveUnderMounts(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir() // 挂载目标(区外)
	mount := filepath.Join(root, "visualize")
	if err := os.Symlink(out, mount); err != nil {
		t.Fatal(err)
	}

	// ①挂载区路径放行(返回词法路径)。
	got, err := tools.ResolveUnderMounts(root, "visualize/chart.html", []string{"visualize"})
	if err != nil {
		t.Fatalf("挂载区应放行: %v", err)
	}
	if want := filepath.Join(root, "visualize", "chart.html"); got != want {
		t.Fatalf("词法路径不符: %s ≠ %s", got, want)
	}
	// 挂载点本身放行。
	if _, err := tools.ResolveUnderMounts(root, "visualize", []string{"visualize"}); err != nil {
		t.Fatalf("挂载点应放行: %v", err)
	}
	// ②未列入 mounts 的同名路径仍拒(symlink 圈禁不变)。
	if _, err := tools.ResolveUnder(root, "visualize/chart.html"); err == nil {
		t.Fatal("未声明挂载应拒")
	}
	if _, err := tools.ResolveUnderMounts(root, "visualize/chart.html", []string{"other"}); err == nil {
		t.Fatal("清单外挂载名应拒")
	}
	// ③挂载区内再越区链接仍拒。
	inner := filepath.Join(out, "escape")
	if err := os.Symlink(t.TempDir(), inner); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.ResolveUnderMounts(root, "visualize/escape/x", []string{"visualize"}); err == nil {
		t.Fatal("区内越区链接应拒")
	}
	// ④穿越仍拒(剩余路径词法圈禁)。
	if _, err := tools.ResolveUnderMounts(root, "visualize/../chart.html", []string{"visualize"}); err == nil {
		t.Fatal("穿越应拒")
	}
}

func TestApplyPatchThroughMount(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	if err := os.Symlink(out, filepath.Join(root, "visualize")); err != nil {
		t.Fatal(err)
	}
	ops := []applypatch.FileOp{{Kind: applypatch.KindAdd, Path: "visualize/chart.html", Contents: []string{"hi"}}}
	// 未声明挂载:拒(既有语义)。
	if _, err := applypatch.Apply(root, ops); err == nil {
		t.Fatal("未声明挂载应拒")
	}
	// 声明挂载:放行且落真源。
	if _, err := applypatch.ApplyMounts(root, ops, []string{"visualize"}); err != nil {
		t.Fatalf("挂载写应放行: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "chart.html")); err != nil {
		t.Fatalf("应落挂载真源: %v", err)
	}
}

func TestFsutilMountsConfig(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	if err := os.Symlink(out, filepath.Join(root, "project")); err != nil {
		t.Fatal(err)
	}
	// 构造期 Mounts 校验(CheckTopDirs 同款)。
	if _, err := fsutil.NewTools(fsutil.Config{Root: root, Mounts: []string{".."}}); err == nil {
		t.Fatal("非法挂载名应拒")
	}
	if _, err := fsutil.NewTools(fsutil.Config{Root: root, Mounts: []string{"project"}}); err != nil {
		t.Fatalf("合法挂载名应过: %v", err)
	}
}
