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

func TestWipeKeepsDeclared(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspaces", "张三", "s1")
	_ = os.MkdirAll(filepath.Join(root, "repos", "base-app"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "repos", "base-app", "a.go"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "tmp.sh"), []byte("x"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "spill"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "spill", "01.md"), []byte("x"), 0o644)

	// 声明 repos 为持久子区：挂载保留、其余照清（v0.4.0 起豁免须显式声明，
	// 基座不再预设名字）
	Wipe(root, "repos")

	if _, err := os.Stat(filepath.Join(root, "repos", "base-app", "a.go")); err != nil {
		t.Fatal("声明的持久子区应跨任务保留：", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tmp.sh")); err == nil {
		t.Fatal("临时文件应清除")
	}
	if _, err := os.Stat(filepath.Join(root, "spill")); err == nil {
		t.Fatal("spill 应清除")
	}
	// 持久子区尚存时根不回收（Remove 非空自败，无损害）
	if _, err := os.Stat(root); err != nil {
		t.Fatal("根应保留（持久子区尚存）")
	}
}

func TestWipeAllWithoutKeep(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspaces", "张三", "s1")
	_ = os.MkdirAll(filepath.Join(root, "repos", "base-app"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "repos", "base-app", "a.go"), []byte("x"), 0o644)

	Wipe(root)

	// 无声明 = 全清（此前 repos/ 的隐式豁免已移除）
	if _, err := os.Stat(root); err == nil {
		t.Fatal("无 keep 声明时应整清（含 repos/）")
	}
}
