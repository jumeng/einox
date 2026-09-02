package fsutil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jumeng/einox/contract"
)

// invoke 直接驱动 helper（构造路径不绕组装）。
func invoke(t *testing.T, ts []contract.Tool, name, args string) map[string]any {
	t.Helper()
	for _, x := range ts {
		info := x.Info()
		if info == nil || info.Name != name {
			continue
		}
		out, err := x.Invoke(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s 执行失败：%v", name, err)
		}
		var m map[string]any
		if json.Unmarshal([]byte(out), &m) != nil {
			t.Fatalf("%s 非 JSON 输出：%s", name, out)
		}
		return m
	}
	t.Fatalf("缺工具 %s", name)
	return nil
}

func TestFSUtil(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("l1 alpha\nl2 beta\nl3 gamma\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "sub", "b.md"), []byte("# beta head\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "sub", "c.bin"), []byte{'x', 0, 'y'}, 0o644)
	ts, err := NewTools(Config{Root: root})
	if err != nil || len(ts) != 4 {
		t.Fatalf("构造失败：%v（工具数 %d）", err, len(ts))
	}

	// read_file：行号 + 区间 + 越界拒绝
	m := invoke(t, ts, "read_file", `{"path":"a.txt"}`)
	if m["ok"] != true || !strings.Contains(m["content"].(string), "     2→l2 beta") {
		t.Fatalf("read_file 行号输出错：%v", m)
	}
	m = invoke(t, ts, "read_file", `{"path":"a.txt","offset":2,"limit":1}`)
	if m["ok"] != true || strings.Contains(m["content"].(string), "l1") {
		t.Fatalf("offset/limit 区间错：%v", m)
	}
	if m := invoke(t, ts, "read_file", `{"path":"a.txt","offset":9}`); m["ok"] != false {
		t.Errorf("offset 越界应拒绝：%v", m)
	}
	if m := invoke(t, ts, "read_file", `{"path":"nope.txt"}`); m["ok"] != false {
		t.Errorf("不存在应拒绝：%v", m)
	}
	if m := invoke(t, ts, "read_file", `{"path":"sub"}`); m["ok"] != false {
		t.Errorf("目录应拒绝并提示 list_dir：%v", m)
	}
	// 防穿越
	for _, p := range []string{"../x", "sub/../../y", ".."} {
		if m := invoke(t, ts, "read_file", `{"path":"`+p+`"}`); m["ok"] != false {
			t.Errorf("越界应拒绝：%s", p)
		}
	}

	// list_dir：目录在前
	m = invoke(t, ts, "list_dir", `{}`)
	if m["ok"] != true {
		t.Fatalf("list_dir 失败：%v", m)
	}
	s, _ := json.Marshal(m["entries"])
	if !strings.Contains(string(s), `"name":"sub"`) || !strings.Contains(string(s), `"dir":true`) ||
		!strings.Contains(string(s), `"name":"a.txt"`) {
		t.Fatalf("list_dir 条目错：%s", s)
	}

	// search_files：只 glob / glob+正则 / 二进制跳过
	m = invoke(t, ts, "search_files", `{"pattern":"**/*.md"}`)
	if m["ok"] != true || !strings.Contains(strings.Join(stringify(m["files"]), " "), "sub/b.md") {
		t.Fatalf("glob 搜错：%v", m)
	}
	m = invoke(t, ts, "search_files", `{"pattern":"**/*","query":"beta"}`)
	if m["ok"] != true {
		t.Fatalf("内容搜失败：%v", m)
	}
	hits, _ := json.Marshal(m["matches"])
	if !strings.Contains(string(hits), "a.txt") || !strings.Contains(string(hits), "sub/b.md") {
		t.Fatalf("beta 应命中两文件：%s", hits)
	}
	if strings.Contains(string(hits), "c.bin") {
		t.Fatalf("二进制不应命中：%s", hits)
	}
	if m := invoke(t, ts, "search_files", `{}`); m["ok"] != false {
		t.Errorf("空参数应拒绝：%v", m)
	}
	if m := invoke(t, ts, "search_files", `{"query":"("}`); m["ok"] != false {
		t.Errorf("坏正则应拒绝：%v", m)
	}

	// delete_file：文件直删 / 目录须 recursive / 不存在拒绝 / 防穿越
	if m := invoke(t, ts, "delete_file", `{"path":"a.txt"}`); m["ok"] != true {
		t.Fatalf("文件删除失败：%v", m)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Error("文件应已删除")
	}
	if m := invoke(t, ts, "delete_file", `{"path":"sub"}`); m["ok"] != false {
		t.Errorf("目录无 recursive 应拒绝：%v", m)
	}
	if m := invoke(t, ts, "delete_file", `{"path":"sub","recursive":true}`); m["ok"] != true {
		t.Fatalf("目录递归删失败：%v", m)
	}
	if m := invoke(t, ts, "delete_file", `{"path":"ghost"}`); m["ok"] != false {
		t.Errorf("不存在应拒绝：%v", m)
	}
	if m := invoke(t, ts, "delete_file", `{"path":"../x"}`); m["ok"] != false {
		t.Errorf("越界删除应拒绝：%v", m)
	}

	// 空 Root 拒绝构造
	if _, err := NewTools(Config{}); err == nil {
		t.Error("空 Root 应拒绝构造（P0 纪律：不给全盘默认）")
	}
}

func stringify(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, x.(string))
	}
	return out
}

// TestSpillRouting spill/ 前缀路由（reduction 外置域取回面）：配置 Spill 后
// spill/ 路径进会话外置域、防穿越同工作区语义；未配置时 spill/ 无特判。
func TestSpillRouting(t *testing.T) {
	root, spill := t.TempDir(), t.TempDir()
	for _, d := range []string{filepath.Join(spill, "trunc"), filepath.Join(root, "spill")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(spill, "trunc", "c1"), []byte("l1 full\nl2 output\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "spill", "w.txt"), []byte("workspace spill dir\n"), 0o644)

	ts, err := NewTools(Config{Root: root, Spill: filepath.Join(spill)})
	if err != nil {
		t.Fatal(err)
	}
	m := invoke(t, ts, "read_file", `{"path":"spill/trunc/c1"}`)
	if m["ok"] != true || !strings.Contains(m["content"].(string), "l2 output") {
		t.Fatalf("spill/ 前缀应路由至外置域：%v", m)
	}
	if m := invoke(t, ts, "read_file", `{"path":"spill/../../x"}`); m["ok"] != false {
		t.Errorf("外置域越界应拒绝：%v", m)
	}
	// 未配置 Spill：spill/ 是普通工作区相对路径
	ts2, _ := NewTools(Config{Root: root})
	m = invoke(t, ts2, "read_file", `{"path":"spill/w.txt"}`)
	if m["ok"] != true {
		t.Fatalf("未配置 Spill 时 spill/ 应按工作区解析：%v", m)
	}
}

func TestReadFileLineWidth(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "long.txt"), []byte(strings.Repeat("甲", 3000)), 0o644)
	_ = os.WriteFile(filepath.Join(root, "huge.txt"), []byte(strings.Repeat("乙", 50001)), 0o644)
	ts, err := NewTools(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	// 默认 2000 截断：标记自解释（总字符数 + line_width 指路）
	m := invoke(t, ts, "read_file", `{"path":"long.txt"}`)
	c := m["content"].(string)
	if !strings.Contains(c, "单行截断：本行共 3000 字符——传 line_width 放宽，上限 50000") {
		t.Fatalf("截断标记应含总长与指路：%v", c)
	}
	if strings.Count(c, "甲") != 2000 {
		t.Fatalf("默认应截 2000 字符：%d", strings.Count(c, "甲"))
	}
	// line_width 放宽至整读
	m = invoke(t, ts, "read_file", `{"path":"long.txt","line_width":3000}`)
	c = m["content"].(string)
	if strings.Contains(c, "单行截断") || strings.Count(c, "甲") != 3000 {
		t.Fatalf("line_width 放宽后应整读：%d", strings.Count(c, "甲"))
	}
	// 钳上限：50001 行传 999999 只给 50000
	m = invoke(t, ts, "read_file", `{"path":"huge.txt","line_width":999999}`)
	c = m["content"].(string)
	if !strings.Contains(c, "单行截断：本行共 50001 字符") || strings.Count(c, "乙") != 50000 {
		t.Fatalf("line_width 应钳到 50000：%d", strings.Count(c, "乙"))
	}
}

// TestDeleteFileProtectDirs 写保护区：delete_file 命中即拒（目录本身/子
// 路径/./ 变体/平台分隔符变体），读面与保护区外不受影响；非法条目构造期即拒。
func TestDeleteFileProtectDirs(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "repos"), 0o755)
	os.MkdirAll(filepath.Join(root, "notes"), 0o755)
	os.WriteFile(filepath.Join(root, "repos", "a.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "notes", "b.txt"), []byte("y"), 0o644)
	ts, err := NewTools(Config{Root: root, ProtectDirs: []string{"repos"}})
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}

	// 命中即拒（信封回喂，错误文案指明写保护区）
	for _, p := range []string{"repos", "repos/a.txt", "./repos/a.txt", filepath.Join("repos", "a.txt")} {
		m := invoke(t, ts, "delete_file", fmt.Sprintf(`{"path":%q}`, p))
		if m["ok"] != false || !strings.Contains(m["error"].(string), "写保护区") {
			t.Fatalf("delete_file(%s) 应拒（写保护区）：%v", p, m)
		}
	}

	// 读面不受波及；保护区外照常
	if m := invoke(t, ts, "read_file", `{"path":"repos/a.txt"}`); m["ok"] != true {
		t.Fatalf("read_file 不受写保护区影响：%v", m)
	}
	if m := invoke(t, ts, "delete_file", `{"path":"notes/b.txt"}`); m["ok"] != true {
		t.Fatalf("保护区外 delete_file 应照常：%v", m)
	}

	// 空清单零变化（保护区路径可删——装配未设栏）
	ts2, err := NewTools(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if m := invoke(t, ts2, "delete_file", `{"path":"repos/a.txt","recursive":true}`); m["ok"] != true {
		t.Fatalf("空 ProtectDirs 应零变化：%v", m)
	}

	// 非法条目构造期即拒
	for _, bad := range []string{"/abs", "..", "a/b", `a\b`, ""} {
		if _, err := NewTools(Config{Root: root, ProtectDirs: []string{bad}}); err == nil {
			t.Fatalf("非法条目 %q 应构造期报错", bad)
		}
	}
}
