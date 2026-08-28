// Seatbelt profile 纯字符串单测（跨平台可跑——真源 §6「profile 文本生成
// 断言」；darwin 运行时行为本环境不可验，诚实声明见 docs/09）。
package sandbox

import (
	"strings"
	"testing"
)

func TestSeatbeltProfileReadOnlyOffline(t *testing.T) {
	profile, params, err := buildSeatbeltProfile(ModeReadOnly, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(deny default)",       // 闭合起手
		"(allow file-read*)",   // 全盘读
		`(path "/dev/null")`,   // base 内 /dev/null 写
		"(allow process-exec)", // 子进程继承策略
		"(allow sysctl-read",   // sysctl 只读白名单
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("read-only profile 缺少 %q", want)
		}
	}
	if strings.Contains(profile, "(allow file-write*") {
		t.Error("read-only 档不得出现 file-write* allow 段")
	}
	if strings.Contains(profile, "(allow network-outbound)") {
		t.Error("断网档不得出现 network-outbound allow")
	}
	if len(params) != 0 {
		t.Errorf("read-only 档不应有 -D 参数， got %v", params)
	}
}

func TestSeatbeltProfileWorkspaceWrite(t *testing.T) {
	profile, params, err := buildSeatbeltProfile(ModeWorkspaceWrite, false, []seatbeltWriteRoot{
		{Path: "/Users/dev/ws"},
		{Path: "/Users/dev/.cache/einox", Protected: []string{"/Users/dev/.cache/einox/lock"}},
		{Path: "/Users/dev/out/file.bin", Literal: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, `(subpath (param "WRITABLE_ROOT_0"))`) {
		t.Error("工作区根应为 subpath 引用")
	}
	if !strings.Contains(profile, `(literal (param "WRITABLE_ROOT_2"))`) {
		t.Error("文件根应为 literal 引用")
	}
	if !strings.Contains(profile, `(require-not (literal (param "WRITABLE_ROOT_1_PROTECTED_0")))`) ||
		!strings.Contains(profile, `(require-not (subpath (param "WRITABLE_ROOT_1_PROTECTED_0")))`) {
		t.Error("保护子路径应有 literal+subpath 双 require-not 排除")
	}
	if !strings.Contains(profile, "(require-all (subpath (param \"WRITABLE_ROOT_1\"))") {
		t.Error("带排除的根应收进 require-all")
	}
	// 锚点 deny：子树根防替换，literal 文件根不产锚点
	if strings.Count(profile, "deny file-write-unlink") != 2 {
		t.Errorf("锚点 deny 应恰 2 处（两个 subpath 根），got %d", strings.Count(profile, "deny file-write-unlink"))
	}
	if !strings.Contains(profile, `(deny file-write-unlink (require-all (literal (param "WRITABLE_ROOT_0")) (vnode-type DIRECTORY)))`) {
		t.Error("锚点 deny 形态不符")
	}
	wantParams := [][2]string{
		{"WRITABLE_ROOT_0", "/Users/dev/ws"},
		{"WRITABLE_ROOT_1", "/Users/dev/.cache/einox"},
		{"WRITABLE_ROOT_1_PROTECTED_0", "/Users/dev/.cache/einox/lock"},
		{"WRITABLE_ROOT_2", "/Users/dev/out/file.bin"},
	}
	if len(params) != len(wantParams) {
		t.Fatalf("参数表长度 %d ≠ %d: %v", len(params), len(wantParams), params)
	}
	for i, kv := range wantParams {
		if params[i] != kv {
			t.Errorf("参数[%d] = %v, want %v", i, params[i], kv)
		}
	}
}

func TestSeatbeltProfileNetworkOn(t *testing.T) {
	profile, _, err := buildSeatbeltProfile(ModeWorkspaceWrite, true, []seatbeltWriteRoot{{Path: "/ws"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(allow network-outbound)", // 出站
		"(allow network-inbound)",  // 入站
		`com.apple.SecurityServer`, // TLS/DNS mach-lookup 白名单
		"(socket-domain AF_SYSTEM)",
	} {
		if !strings.Contains(profile, want) {
			t.Errorf("联网 profile 缺少 %q", want)
		}
	}
}

func TestSeatbeltProfileDanger(t *testing.T) {
	// 裸 danger：regex 全盘写，无参数无锚点
	profile, params, err := buildSeatbeltProfile(ModeDangerFullAccess, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, `(allow file-write* (regex #"^/"))`) {
		t.Error("danger 档应为 regex 全盘写")
	}
	if len(params) != 0 || strings.Contains(profile, "deny file-write-unlink") {
		t.Errorf("裸 danger 不应有参数/锚点: params=%v", params)
	}

	// danger + ProtectedReadOnly：退化为「/ 子树 + 排除」
	profile, params, err = buildSeatbeltProfile(ModeDangerFullAccess, false, []seatbeltWriteRoot{
		{Path: "/", Protected: []string{"/etc/hosts"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, `(subpath (param "WRITABLE_ROOT_0"))`) {
		t.Error("danger+保护应为 / 子树引用")
	}
	if !strings.Contains(profile, `WRITABLE_ROOT_0_PROTECTED_0`) {
		t.Error("保护子路径参数缺失")
	}
	if len(params) != 2 {
		t.Errorf("参数表应含根+保护两项, got %v", params)
	}
}

func TestSeatbeltProfileInvalidMode(t *testing.T) {
	if _, _, err := buildSeatbeltProfile(Mode("bogus"), false, nil); err == nil {
		t.Error("非法模式应报错")
	}
}

func TestProtectedAncestors(t *testing.T) {
	got := protectedAncestors("/ws", []string{"/ws/a/b/.git"})
	if len(got) != 2 || got[0] != "/ws/a/b" || got[1] != "/ws/a" {
		t.Errorf("应自深向浅收全中间祖先（不含根本体）：%v", got)
	}
	// 保护路径直接挂根下 = 无中间祖先（根本体锚点 deny 已覆盖）
	if got := protectedAncestors("/ws", []string{"/ws/.git"}); len(got) != 0 {
		t.Errorf("根直属保护路径不应有祖先：%v", got)
	}
	// danger 形根为 /：任意深层都算
	if got := protectedAncestors("/", []string{"/a/b"}); len(got) != 1 || got[0] != "/a" {
		t.Errorf("/ 根应收 /a：%v", got)
	}
	// 多变体共享祖先去重
	got = protectedAncestors("/ws", []string{"/ws/a/b/.git", "/ws/a/c/.git"})
	if len(got) != 3 { // /ws/a/b, /ws/a, /ws/a/c
		t.Errorf("共享祖先应去重：%v", got)
	}
}

func TestSeatbeltProfileProtectedAncestors(t *testing.T) {
	// 收官二轮审查 B-2：祖先 deny 须发射且收尾（无更宽 allow 能重开 rename 的 unlink）
	profile, params, err := buildSeatbeltProfile(ModeWorkspaceWrite, false, []seatbeltWriteRoot{
		{Path: "/ws", Protected: []string{"/ws/a/b/.git"}, ProtectedAncestors: []string{"/ws/a/b", "/ws/a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantDeny := `(deny file-write-unlink (require-all (vnode-type DIRECTORY) (literal (param "PROTECTED_ANCHOR_0"))))`
	if !strings.Contains(profile, wantDeny) {
		t.Errorf("保护祖先 deny 形态不符，profile:\n%s", profile)
	}
	// 根锚点 1 处 + 祖先 2 处
	if n := strings.Count(profile, "deny file-write-unlink"); n != 3 {
		t.Errorf("deny file-write-unlink 应恰 3 处（1 锚点 + 2 祖先），got %d", n)
	}
	// 祖先 deny 在最后一段（网络段与锚点之后）
	if strings.LastIndex(profile, "PROTECTED_ANCHOR_1") < strings.LastIndex(profile, "WRITABLE_ROOT_0") {
		t.Error("保护祖先 deny 应收尾")
	}
	wantParams := [][2]string{
		{"WRITABLE_ROOT_0", "/ws"},
		{"WRITABLE_ROOT_0_PROTECTED_0", "/ws/a/b/.git"},
		{"PROTECTED_ANCHOR_0", "/ws/a/b"},
		{"PROTECTED_ANCHOR_1", "/ws/a"},
	}
	if len(params) != len(wantParams) {
		t.Fatalf("参数表 %v ≠ %v", params, wantParams)
	}
	for i, kv := range wantParams {
		if params[i] != kv {
			t.Errorf("参数[%d] = %v, want %v", i, params[i], kv)
		}
	}
}
