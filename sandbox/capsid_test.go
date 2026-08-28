// capability SID 派生纯函数单测（跨平台可跑——真源 §6「Windows SID 派生
// 纯函数断言」；token/ACL 系统调用面本环境 Application Control 拦 go test，
// 运行时未验，诚实声明见 docs/09）。
package sandbox

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestWorkspaceWriteSIDForm(t *testing.T) {
	re := regexp.MustCompile(`^S-1-4-(\d+)-(\d+)$`)
	m := re.FindStringSubmatch(WorkspaceWriteSID(`C:\Users\dev\ws`))
	if m == nil {
		t.Fatalf("SID 形态不符: %s", WorkspaceWriteSID(`C:\Users\dev\ws`))
	}
	for _, sub := range m[1:] {
		n, err := strconv.ParseUint(sub, 10, 32)
		if err != nil || n == 0 || n >= 1<<30 {
			t.Errorf("subauthority %s 超出 30-bit 非零范围", sub)
		}
	}
}

func TestWorkspaceWriteSIDStable(t *testing.T) {
	a := WorkspaceWriteSID(`C:\Users\dev\ws`)
	// 拼写变体（大小写/分隔符/尾缀/空白）派生同一 SID——standing ACE 不分叉
	for _, v := range []string{
		`c:\users\dev\ws`, `C:/Users/dev/ws`, `C:\\Users\\dev\\ws`,
		`C:\Users\dev\ws\`, ` C:\Users\dev\ws `,
	} {
		if got := WorkspaceWriteSID(v); got != a {
			t.Errorf("拼写 %q 应派生同 SID：%s ≠ %s", v, got, a)
		}
	}
	// 不同路径不同 SID
	if WorkspaceWriteSID(`C:\Users\dev\ws2`) == a {
		t.Error("不同路径不得派生同一 SID")
	}
}

func TestCanonicalPathKey(t *testing.T) {
	cases := [][2]string{
		{`C:\A\B`, `c:\a\b`},
		{`C:/A//B/`, `c:\a\b`},
		{`  D:\X  `, `d:\x`},
	}
	for _, c := range cases {
		if got := canonicalPathKey(c[0]); got != c[1] {
			t.Errorf("canonicalPathKey(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	if strings.Contains(canonicalPathKey(`C:\a\\b\\\c`), `\\`) {
		t.Error("重复分隔符应被折叠")
	}
}
