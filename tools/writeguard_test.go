package tools

// PathBlocked 写区命中判定表测（写保护区路径判定——安全相关）：顶层粒度
// 命中（目录本身/子路径）、同前缀目录名陷阱（段边界）、归一（./ 前缀、尾
// 分隔符、首尾空白、.. 折叠、根路径）、大小写折叠（不敏感 FS 旁路防御）、
// 平台分隔符（反斜杠仅在 Windows 宿主视作分隔符）。

import (
	"runtime"
	"testing"
)

func TestPathBlocked(t *testing.T) {
	dirs := []string{"docs", "repos"}
	cases := []struct {
		rel  string
		want bool
		desc string
	}{
		{"docs", true, "保护区目录本身"},
		{"docs/", true, "尾分隔符归一为目录本身"},
		{"docs/x.md", true, "保护区子路径"},
		{"docs/sub/y.md", true, "保护区深层子路径"},
		{"./docs/x.md", true, "./ 前缀归一"},
		{"  docs/x.md  ", true, "首尾空白归一"},
		{"notes/../docs/x.md", true, ".. 折叠后命中"},
		{".", true, "工作区根（非空清单即命中——根是所有顶层区祖先）"},
		{"", true, "空路径 Clean 归一为根"},
		{" ./ ", true, "空白包裹的根"},
		{"docs2/x.md", false, "同前缀目录名不拦（段边界）"},
		{"doc/x.md", false, "短前缀不拦"},
		{"mydocs/x.md", false, "后缀包含不拦"},
		{"Docs/x.md", true, "大小写折叠命中（macOS/Windows 默认 FS 不区分大小写——字节级比较即旁路）"},
		{"DOCS", true, "保护区目录大小写变体命中"},
		{"/docs/x.md", false, "绝对前缀不视作相对命中"},
		{"notes/x.md", false, "保护区外"},
		{"notes/docs/x.md", false, "非顶层同名段不拦"},
	}
	for _, c := range cases {
		if got := PathBlocked(c.rel, dirs); got != c.want {
			t.Fatalf("%s：PathBlocked(%q) 实得 %v（期望 %v）", c.desc, c.rel, got, c.want)
		}
	}
	// 空清单 = 无保护区，恒不拦
	for _, rel := range []string{".", "docs/x.md", ""} {
		if PathBlocked(rel, nil) {
			t.Fatalf("空清单不应拦截 %q", rel)
		}
	}
}

// TestPathBlockedPlatformSeparator 反斜杠形态的平台语义：Linux/macOS 下反斜杠
// 是合法文件名字符（不视作分隔符），Windows 宿主上归一后命中——与
// fsutil/applypatch 既有路径解析语义一致。
func TestPathBlockedPlatformSeparator(t *testing.T) {
	rel := `docs\x.md`
	if got, want := PathBlocked(rel, []string{"docs"}), runtime.GOOS == "windows"; got != want {
		t.Fatalf("反斜杠形态平台语义：GOOS=%s 实得 %v（期望 %v）", runtime.GOOS, got, want)
	}
}
