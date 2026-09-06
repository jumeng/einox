package applypatch

// 落盘事务性回归（安全审查 2026-09-06）：Move to 目标存在拒覆盖、非空目录
// 删除预检期拒绝（前序写入不落盘）、写入段失败回滚（已写文件恢复/新建回收）。

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMoveToExistingTargetRejected(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "old.go"), []byte("package a\n"), 0o644)
	os.WriteFile(filepath.Join(root, "exists.go"), []byte("occupied\n"), 0o644)
	patch := "*** Begin Patch\n*** Update File: old.go\n*** Move to: exists.go\n@@\n-package a\n+package b\n*** End Patch\n"
	if _, err := apply(t, root, patch); err == nil {
		t.Fatal("Move to 已存在目标应拒绝")
	}
	if read(t, root, "old.go") != "package a\n" {
		t.Fatal("源文件应保持原状")
	}
	if read(t, root, "exists.go") != "occupied\n" {
		t.Fatal("目标文件不应被覆盖")
	}
}

func TestDeleteNonEmptyDirPreflight(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "keep")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "sub", "x.txt"), []byte("x\n"), 0o644)
	// 第一项 Add 会成功、第二项 Delete 非空目录须在预检期拒绝——Add 不落盘
	patch := "*** Begin Patch\n*** Add File: n.txt\n+new\n*** Delete File: keep\n*** End Patch\n"
	if _, err := apply(t, root, patch); err == nil {
		t.Fatal("删除非空目录应失败")
	}
	if _, err := os.Stat(filepath.Join(root, "n.txt")); !os.IsNotExist(err) {
		t.Fatal("预检期失败：前序 Add 不应落盘")
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "x.txt")); err != nil {
		t.Fatal("目录内容应原样保留")
	}
	// 空目录删除照常成功
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := apply(t, root, "*** Begin Patch\n*** Delete File: empty\n*** End Patch\n"); err != nil {
		t.Fatalf("空目录删除应成功：%v", err)
	}
}

func TestWritePhaseRollback(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("只读目录对 root/Windows 不构成写失败，跳过")
	}
	root := t.TempDir()
	ro := filepath.Join(root, "ro")
	if err := os.MkdirAll(ro, 0o555); err != nil { // 属主只读（无写位）——WriteFile 必败
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "orig.txt"), []byte("old\n"), 0o644)
	// 三项：改既有文件 + 新建文件（会成功）+ 写入只读目录（失败）→ 前两项回滚
	patch := "*** Begin Patch\n" +
		"*** Update File: orig.txt\n@@\n-old\n+new\n" +
		"*** Add File: fresh.txt\n+created\n" +
		"*** Add File: ro/x.txt\n+blocked\n" +
		"*** End Patch\n"
	if _, err := apply(t, root, patch); err == nil {
		t.Fatal("写入只读目录应失败")
	}
	if read(t, root, "orig.txt") != "old\n" {
		t.Fatal("回滚：已改文件应恢复原内容")
	}
	if _, err := os.Stat(filepath.Join(root, "fresh.txt")); !os.IsNotExist(err) {
		t.Fatal("回滚：已新建文件应被回收")
	}
	if _, err := os.Stat(filepath.Join(ro, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("失败项不应存在")
	}
}
