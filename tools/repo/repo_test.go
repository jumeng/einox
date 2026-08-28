package repo

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jumeng/einox/contract"
)

// newFixture 建一个非 bare 缓存仓（含 1 个提交 + 文件 a.txt）+ Resolver 桩。
func newFixture(t *testing.T) (cfg Config, write func(rel, content string)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}
	base := t.TempDir()
	cache := filepath.Join(base, "repos-cache", "base-app-x1")
	ws := filepath.Join(base, "ws")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", cache},
		{"-C", cache, "config", "user.name", "t"}, {"-C", cache, "config", "user.email", "t@t"},
	} {
		if out, err := gitOut("", args...); err != nil {
			t.Fatalf("fixture: %v: %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(cache, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := gitOut(cache, "add", "-A"); err != nil {
		t.Fatal(out)
	}
	if out, err := gitOut(cache, "commit", "-q", "-m", "init"); err != nil {
		t.Fatal(out)
	}
	cfg = Config{
		Resolver: stubResolver{dir: cache, ref: "main"},
		Root:     ws, Owner: "张三", SID: "s1",
		PatchWriter: func(name string, content []byte, operator string) error {
			return os.WriteFile(filepath.Join(base, "exports", name), content, 0o644)
		},
	}
	os.MkdirAll(filepath.Join(base, "exports"), 0o755)
	return cfg, func(rel, content string) {
		_ = os.WriteFile(filepath.Join(ws, "repos", "base-app", rel), []byte(content), 0o644)
	}
}

type stubResolver struct{ dir, ref string }

func (r stubResolver) Resolve(string) (string, string, bool) { return r.dir, r.ref, true }

// noneResolver 未登记任何仓（未登记名用例）。
type noneResolver struct{}

func (noneResolver) Resolve(string) (string, string, bool) { return "", "", false }

// invoke 直接驱动 helper（构造路径不绕组装）。
func invoke(t *testing.T, ts []contract.Tool, name, args string) map[string]any {
	t.Helper()
	for _, x := range ts {
		if x.Info() == nil || x.Info().Name != name {
			continue
		}
		out, err := x.Invoke(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s 执行失败：%v", name, err)
		}
		var m map[string]any
		if json.Unmarshal([]byte(out), &m) != nil {
			t.Fatalf("%s 非 JSON：%s", name, out)
		}
		return m
	}
	t.Fatalf("缺工具 %s", name)
	return nil
}

func TestOpenRepoMountAndIdempotent(t *testing.T) {
	cfg, _ := newFixture(t)
	ts, err := NewTools(cfg)
	if err != nil || len(ts) != 5 {
		t.Fatalf("构造失败：%v（工具数 %d）", err, len(ts))
	}
	m := invoke(t, ts, "open_repo", `{"name":"base-app"}`)
	if m["ok"] != true || m["path"] != "repos/base-app" || m["branch"] != "agent/s1-1" {
		t.Fatalf("open_repo 首挂结果错：%v", m)
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, "repos", "base-app", "a.txt")); err != nil {
		t.Fatal("挂载点应含仓文件 a.txt：", err)
	}
	m2 := invoke(t, ts, "open_repo", `{"name":"base-app"}`)
	if m2["ok"] != true || m2["branch"] != "agent/s1-1" || !strings.Contains(m2["note"].(string), "幂等") {
		t.Fatalf("open_repo 二次挂载应幂等返回同分支：%v", m2)
	}
}

func TestRepoStatusDiffCommit(t *testing.T) {
	cfg, write := newFixture(t)
	ts, _ := NewTools(cfg)
	invoke(t, ts, "open_repo", `{"name":"base-app"}`)
	write("a.txt", "hello v2\n")

	m := invoke(t, ts, "repo_status", `{"name":"base-app"}`)
	s, _ := json.Marshal(m["changes"])
	if m["ok"] != true || !strings.Contains(string(s), `"status":"M"`) || !strings.Contains(string(s), `"path":"a.txt"`) {
		t.Fatalf("repo_status 应含 M 条目：%v", m)
	}
	m = invoke(t, ts, "repo_commit", `{"name":"base-app","message":"t"}`)
	c, _ := m["commit"].(string)
	if m["ok"] != true || len(c) != 7 {
		t.Fatalf("repo_commit 应返回 7 位短 hash：%v", m)
	}
	m = invoke(t, ts, "repo_status", `{"name":"base-app"}`)
	cs, _ := m["changes"].([]any)
	if m["ok"] != true || len(cs) != 0 || m["ahead"] != float64(1) {
		t.Fatalf("提交后 changes 应空且 ahead=1：%v", m)
	}
	m = invoke(t, ts, "repo_diff", `{"name":"base-app"}`)
	if m["ok"] != true || !strings.Contains(m["diff"].(string), "+hello v2") {
		t.Fatalf("repo_diff 应含 + 新行：%v", m)
	}
}

func TestExportPatch(t *testing.T) {
	cfg, write := newFixture(t)
	var saved []byte
	cfg.PatchWriter = func(name string, content []byte, operator string) error { // 捕获导出内容
		saved = content
		return nil
	}
	ts, _ := NewTools(cfg)
	invoke(t, ts, "open_repo", `{"name":"base-app"}`)
	write("a.txt", "hello v2\n")
	invoke(t, ts, "repo_commit", `{"name":"base-app","message":"t"}`)

	m := invoke(t, ts, "export_patch", `{"name":"base-app"}`)
	file, _ := m["file"].(string)
	if m["ok"] != true || !strings.HasPrefix(file, "docs/exports/") || !strings.Contains(file, ".patch") {
		t.Fatalf("export_patch 应返回 docs/exports/ 下 .patch 文件：%v", m)
	}
	if !strings.Contains(file, "base-app-agent-s1-1-") {
		t.Fatalf("补丁文件名应含 name-branch-date：%s", file)
	}
	if !strings.Contains(string(saved), "Subject:") {
		t.Fatalf("导出内容应含 format-patch Subject 头：%q", string(saved))
	}
}

func TestCommitCardDiff(t *testing.T) {
	cfg, write := newFixture(t)
	ts, _ := NewTools(cfg)
	invoke(t, ts, "open_repo", `{"name":"base-app"}`)
	write("a.txt", "hello v2\n")

	var ad interface{ ApprovalDiff(args string) string }
	for _, x := range ts {
		if x.Info() != nil && x.Info().Name == "repo_commit" {
			ad, _ = x.(interface{ ApprovalDiff(args string) string })
		}
	}
	if ad == nil {
		t.Fatal("repo_commit 应包 DiffToolOf 暴露 ApprovalDiff")
	}
	if d := ad.ApprovalDiff(`{"name":"base-app"}`); d == "" || !strings.Contains(d, "+") {
		t.Fatalf("ApprovalDiff 应返回非空含 + 的 diff：%q", d)
	}
	// 无 untracked：不含清单段
	if d := ad.ApprovalDiff(`{"name":"base-app"}`); strings.Contains(d, "未跟踪新文件") {
		t.Fatalf("无 untracked 时不应含未跟踪清单段：%q", d)
	}
	// 新建 untracked 文件：add -A 会纳入而 diff 不含，卡面须单列补全
	write("new.go", "package x\n")
	d := ad.ApprovalDiff(`{"name":"base-app"}`)
	if !strings.Contains(d, "未跟踪新文件（将随本次提交一并纳入，上方 diff 未含其内容）：") || !strings.Contains(d, "- new.go") {
		t.Fatalf("ApprovalDiff 应含未跟踪新文件段与文件名：%q", d)
	}
}

// TestCommitCardDiffHugeDiffKeepsUntracked diff 本体超预算（>12000 rune）时
// untracked 清单段须存活——add -A 会提交这些文件，总量统一截断会吃掉清单
// 造成人审盲区；diff 本体按余量截断让位。
func TestCommitCardDiffHugeDiffKeepsUntracked(t *testing.T) {
	cfg, write := newFixture(t)
	ts, _ := NewTools(cfg)
	invoke(t, ts, "open_repo", `{"name":"base-app"}`)
	write("a.txt", strings.Repeat("超长改动行内容padding\n", 1200)) // >12000 rune 的 diff 本体
	write("new.go", "package x\n")

	var ad interface{ ApprovalDiff(args string) string }
	for _, x := range ts {
		if x.Info() != nil && x.Info().Name == "repo_commit" {
			ad, _ = x.(interface{ ApprovalDiff(args string) string })
		}
	}
	if ad == nil {
		t.Fatal("repo_commit 应包 DiffToolOf 暴露 ApprovalDiff")
	}
	d := ad.ApprovalDiff(`{"name":"base-app"}`)
	if !strings.Contains(d, "未跟踪新文件（将随本次提交一并纳入，上方 diff 未含其内容）：") || !strings.Contains(d, "- new.go") {
		t.Fatalf("超长 diff 下 ApprovalDiff 仍应含未跟踪清单段与文件名（尾段）：%q", truncateRunes(d[len(d)-min(len(d), 400):], 400))
	}
	if r := len([]rune(d)); r > 12100 {
		t.Fatalf("总输出应受 12000 rune 兜底截断：%d", r)
	}
}

// TestMountDisablesPush 挂载后 worktree 级禁 push：pushurl 指向不可用协议
// （「不 push」硬约束；fetch 走 origin.url 不受影响）。
func TestMountDisablesPush(t *testing.T) {
	cfg, _ := newFixture(t)
	ts, _ := NewTools(cfg)
	invoke(t, ts, "open_repo", `{"name":"base-app"}`)
	out, err := gitOut(cfg.mountDir("base-app"), "config", "--worktree", "remote.origin.pushurl")
	if err != nil || strings.TrimSpace(out) != "no-push://disabled" {
		t.Fatalf("挂载后 worktree 级 pushurl 应为 no-push://disabled：%v %q", err, out)
	}
}

func TestFailures(t *testing.T) {
	cfg, _ := newFixture(t)
	ts, _ := NewTools(cfg)
	// 未挂载仓：repo_status 拒绝
	if m := invoke(t, ts, "repo_status", `{"name":"base-app"}`); m["ok"] != false {
		t.Fatalf("未挂载应拒绝：%v", m)
	}
	// 未登记名：open_repo 拒绝
	cfg.Resolver = noneResolver{}
	ts2, _ := NewTools(cfg)
	if m := invoke(t, ts2, "open_repo", `{"name":"nope"}`); m["ok"] != false {
		t.Fatalf("未登记名应拒绝：%v", m)
	}
	// 路径围栏：name 仅作 repos/<name> 目录段，含分隔符或 .. 一律拒绝（错误文本钉死，删围栏必红）
	for _, n := range []string{"../evil", "a/b", "..", "a..b", ""} {
		m := invoke(t, ts2, "open_repo", `{"name":"`+n+`"}`)
		if m["ok"] != false || !strings.Contains(m["error"].(string), "非法仓短名") {
			t.Errorf("越界 name 应拒且错误含「非法仓短名」：%s → %v", n, m)
		}
	}
	// validName 直接单元断言（含 Windows 分隔符）
	for _, n := range []string{"", ".", "..", "a/b", `a\b`, "a..b", "../evil"} {
		if validName(n) {
			t.Errorf("validName 应拒绝：%q", n)
		}
	}
	if !validName("base-app") || !validName("a.b") {
		t.Fatal("validName 应放行正常短名")
	}
}
