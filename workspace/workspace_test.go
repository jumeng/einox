package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOfLazyAndWipe(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspaces", "张三", "s1")

	// 惰性创建：Of 调用即建根
	if got := Of(root); got != root {
		t.Fatalf("应原样返回根：%s", got)
	}
	if err := os.MkdirAll(filepath.Join(root, "spill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "spill", "01.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Wipe：工作区整删 + 空壳祖先（owner/、workspaces/）回收
	Wipe(root)
	if _, err := os.Stat(root); err == nil {
		t.Fatal("工作区应整删")
	}
	if _, err := os.Stat(filepath.Dir(root)); err == nil {
		t.Fatal("空壳 owner 目录应回收")
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("临时域基根保留（他组会话可能占用）：%v", err)
	}
}

func TestWipeKeepsRepoMounts(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspaces", "张三", "s1")
	_ = os.MkdirAll(filepath.Join(root, "repos", "base-app"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "repos", "base-app", "a.go"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "tmp.sh"), []byte("x"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "spill"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "spill", "01.md"), []byte("x"), 0o644)

	Wipe(root)

	// repos/ 挂载保留（会话级持久：worktree 所在）
	if _, err := os.Stat(filepath.Join(root, "repos", "base-app", "a.go")); err != nil {
		t.Fatal("repos/ 挂载应跨任务保留：", err)
	}
	// 其余临时产物照清
	if _, err := os.Stat(filepath.Join(root, "tmp.sh")); err == nil {
		t.Fatal("临时文件应清除")
	}
	if _, err := os.Stat(filepath.Join(root, "spill")); err == nil {
		t.Fatal("spill 应清除")
	}
	// repos/ 尚存时根与祖先不回收（Remove 非空自败，无损害）
	if _, err := os.Stat(root); err != nil {
		t.Fatal("根应保留（repos/ 尚存）")
	}
}
