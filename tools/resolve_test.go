package tools

// ResolveUnder 圈禁表测：词法穿越拒绝、a..b.txt 不误伤、符号链接逃逸拒绝
// （文件链接/目录链接）、指向区内的链接放行、新建深路径放行。

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveUnderLexical(t *testing.T) {
	root := t.TempDir()
	if _, err := ResolveUnder(root, "../../etc/passwd"); err == nil {
		t.Fatal("词法穿越应拒绝")
	}
	if _, err := ResolveUnder(root, ".."); err == nil {
		t.Fatal(".. 应拒绝")
	}
	got, err := ResolveUnder(root, "a..b.txt") // 子串拒绝误伤的回归锚
	if err != nil {
		t.Fatalf("a..b.txt 不应误伤：%v", err)
	}
	if want := filepath.Join(root, "a..b.txt"); got != want {
		t.Fatalf("实得 %s，期望 %s", got, want)
	}
	if r, err := ResolveUnder(root, " . "); err != nil || r != root {
		t.Fatalf("空白归一应归根：%v %v", r, err)
	}
}

func TestResolveUnderSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink 需特权，Windows 跳过")
	}
	root := t.TempDir()
	outside := t.TempDir() // 区外真目录
	outFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 文件链接：区内链接指向区外文件
	if err := os.Symlink(outFile, filepath.Join(root, "link-file")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveUnder(root, "link-file"); err == nil {
		t.Fatal("指向区外的文件链接应拒绝")
	}
	// 目录链接：区内目录链接指向区外目录
	if err := os.Symlink(outside, filepath.Join(root, "link-dir")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveUnder(root, "link-dir/secret.txt"); err == nil {
		t.Fatal("指向区外的目录链接下寻址应拒绝")
	}
	if _, err := ResolveUnder(root, "link-dir"); err == nil {
		t.Fatal("指向区外的目录链接本身应拒绝")
	}
	// 指向区内的链接：放行
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "in-link")); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveUnder(root, "in-link/a.txt"); err != nil {
		t.Fatalf("指向区内的链接应放行：%v", err)
	} else if filepath.Base(got) != "a.txt" {
		t.Fatalf("实得 %s", got)
	}
	// 新建深路径（全不存在）：无链接可解析，词法圈禁已足
	if _, err := ResolveUnder(root, "new/deep/file.txt"); err != nil {
		t.Fatalf("新建深路径应放行：%v", err)
	}
}
