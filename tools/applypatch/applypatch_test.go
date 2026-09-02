package applypatch

// 移植回归：格式解析、四档模糊匹配、EOF 空行重试、@@ 锚、事务性、路径安全。
// 用例改编自 codex apply-patch crate 测试（Apache-2.0）与格式规范示例。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func apply(t *testing.T, root, patch string) (map[string]any, error) {
	t.Helper()
	ops, err := Parse(patch)
	if err != nil {
		return nil, err
	}
	res, err := Apply(root, ops)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(res)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m, nil
}

func read(t *testing.T, root, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, p))
	if err != nil {
		t.Fatalf("读 %s 失败：%v", p, err)
	}
	return string(b)
}

func TestAddFile(t *testing.T) {
	root := t.TempDir()
	if _, err := apply(t, root, "*** Begin Patch\n*** Add File: hello.txt\n+Hello world\n+second line\n*** End Patch\n"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "hello.txt"); got != "Hello world\nsecond line\n" {
		t.Fatalf("Add 内容错：%q", got)
	}
	// 重复 Add 拒绝
	if _, err := apply(t, root, "*** Begin Patch\n*** Add File: hello.txt\n+x\n*** End Patch\n"); err == nil {
		t.Fatal("已存在 Add 应拒绝")
	}
}

func TestUpdateWithContext(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.py"), []byte("def greet():\n    print(\"Hi\")\n    return\n"), 0o644)

	patch := "*** Begin Patch\n*** Update File: a.py\n@@\n def greet():\n-    print(\"Hi\")\n+    print(\"Hello, world!\")\n*** End Patch\n"
	if _, err := apply(t, root, patch); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "a.py"); !strings.Contains(got, "Hello, world!") || strings.Contains(got, `"Hi"`) {
		t.Fatalf("Update 结果错：%q", got)
	}
	if !strings.Contains(read(t, root, "a.py"), "return") {
		t.Fatal("未改动行应保留")
	}
}

func TestUpdateMultiChunkSequential(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "m.txt"), []byte("one\ntwo\nthree\nfour\nfive\n"), 0o644)
	// 同文件两个块：顺序游标（第二块从第一块之后找）
	patch := "*** Begin Patch\n*** Update File: m.txt\n@@\n one\n-two\n+TWO\n@@\n four\n-five\n+FIVE\n*** End Patch\n"
	if _, err := apply(t, root, patch); err != nil {
		t.Fatal(err)
	}
	if got := read(t, root, "m.txt"); got != "one\nTWO\nthree\nfour\nFIVE\n" {
		t.Fatalf("多块顺序应用错：%q", got)
	}
}

func TestMoveTo(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "old.go"), []byte("package a\n"), 0o644)
	patch := "*** Begin Patch\n*** Update File: old.go\n*** Move to: new.go\n@@\n-package a\n+package b\n*** End Patch\n"
	if _, err := apply(t, root, patch); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "old.go")); !os.IsNotExist(err) {
		t.Fatal("旧路径应已移除")
	}
	if got := read(t, root, "new.go"); got != "package b\n" {
		t.Fatalf("改名结果错：%q", got)
	}
}

func TestDeleteFile(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "x.txt"), []byte("bye\n"), 0o644)
	if _, err := apply(t, root, "*** Begin Patch\n*** Delete File: x.txt\n*** End Patch\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("应已删除")
	}
	// 不存在删除拒绝
	if _, err := apply(t, root, "*** Begin Patch\n*** Delete File: ghost.txt\n*** End Patch\n"); err == nil {
		t.Fatal("不存在删除应拒绝")
	}
}

func TestFuzzyMatching(t *testing.T) {
	// 行尾/两侧空白容错 + Unicode 弯引号归一
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("start  \n  mid\t\nend\n"), 0o644)
	patch := "*** Begin Patch\n*** Update File: f.txt\n@@\n start\n-  mid\n+MIDDLE\n end\n*** End Patch\n"
	if _, err := apply(t, root, patch); err != nil {
		t.Fatalf("空白容错匹配失败：%v", err)
	}
	if !strings.Contains(read(t, root, "f.txt"), "MIDDLE") {
		t.Fatal("模糊匹配未应用")
	}

	os.WriteFile(filepath.Join(root, "u.txt"), []byte("msg = “hello”\n"), 0o644)
	patch = "*** Begin Patch\n*** Update File: u.txt\n@@\n-msg = \"hello\"\n+msg = \"world\"\n*** End Patch\n"
	if _, err := apply(t, root, patch); err != nil {
		t.Fatalf("Unicode 归一匹配失败：%v", err)
	}
	if !strings.Contains(read(t, root, "u.txt"), "world") {
		t.Fatal("Unicode 匹配未应用")
	}
}

func TestEOFMarkerAndTrailingEmptyRetry(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "e.txt"), []byte("head\nlast\n"), 0o644)
	// 尾部空行哨兵（块尾含空上下文行代表文件尾换行）
	patch := "*** Begin Patch\n*** Update File: e.txt\n@@\n last\n-\n+extra\n*** End Patch\n"
	if _, err := apply(t, root, patch); err != nil {
		t.Fatalf("EOF 空行重试失败：%v", err)
	}
	// End of File 标记：同型块出现两次时锚定末次（收尾意图）
	os.WriteFile(filepath.Join(root, "e2.txt"), []byte("head\ntail\ntail\nend\nmid\ntail\ntail\nend\n"), 0o644)
	patch = "*** Begin Patch\n*** Update File: e2.txt\n@@\n tail\n tail\n-end\n+X\n*** End of File\n*** End Patch\n"
	if _, err := apply(t, root, patch); err != nil {
		t.Fatalf("End of File 锚定失败：%v", err)
	}
	got := read(t, root, "e2.txt")
	if !strings.Contains(got, "head\ntail\ntail\nend\nmid") {
		t.Fatalf("前段同型块应保持不变：%q", got)
	}
	if !strings.HasSuffix(got, "mid\ntail\ntail\nX\n") {
		t.Fatalf("EOF 锚定应改末次出现：%q", got)
	}
}

func TestConflictFailsWholePatch(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "c.txt"), []byte("aaa\n"), 0o644)
	// 第二文件找不到上下文 → 整体不落盘（含第一个 Add）
	patch := "*** Begin Patch\n*** Add File: n.txt\n+new\n*** Update File: c.txt\n@@\n-nope\n+zzz\n*** End Patch\n"
	if _, err := apply(t, root, patch); err == nil {
		t.Fatal("找不到上下文应整体失败")
	}
	if _, err := os.Stat(filepath.Join(root, "n.txt")); !os.IsNotExist(err) {
		t.Fatal("事务性：失败时 Add 也不应落盘")
	}
	if read(t, root, "c.txt") != "aaa\n" {
		t.Fatal("事务性：原文件不应被改动")
	}
}

func TestPathSafety(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"../esc.txt", "/abs.txt", "a/../../b", ""} {
		patch := "*** Begin Patch\n*** Add File: " + p + "\n+x\n*** End Patch\n"
		if _, err := apply(t, root, patch); err == nil {
			t.Errorf("路径 %q 应被拒绝", p)
		}
	}
}

func TestParseErrors(t *testing.T) {
	root := t.TempDir()
	for name, patch := range map[string]string{
		"无开头":   "not a patch",
		"无结尾":   "*** Begin Patch\n*** Add File: a\n+x\n",
		"Add空":  "*** Begin Patch\n*** Add File: a\n*** End Patch\n",
		"块外加减行": "*** Begin Patch\n*** Update File: a\n-x\n*** End Patch\n",
	} {
		if _, err := apply(t, root, patch); err == nil {
			t.Errorf("%s 应解析失败", name)
		}
	}
}

func TestStackedAnchors(t *testing.T) {
	// 多 @@ 堆叠定位（规范：类内重复代码块场景）
	root := t.TempDir()
	body := strings.Repeat("value = 0\n", 5) + "class A:\n    def m():\n        value = 1\n"
	os.WriteFile(filepath.Join(root, "s.py"), []byte(body), 0o644)
	patch := "*** Begin Patch\n*** Update File: s.py\n@@ class A:\n@@     def m():\n-\tvalue = 1\n+\tvalue = 2\n*** End Patch\n"
	if _, err := apply(t, root, patch); err != nil {
		t.Fatalf("堆叠锚失败：%v", err)
	}
	if !strings.Contains(read(t, root, "s.py"), "value = 2") || strings.Count(read(t, root, "s.py"), "value = 0") != 5 {
		t.Fatal("堆叠锚应只改目标块")
	}
}

func TestToolInvocation(t *testing.T) {
	root := t.TempDir()
	ts, err := NewTools(Config{Root: root})
	if err != nil || len(ts) != 1 {
		t.Fatalf("构造失败：%v", err)
	}
	info := ts[0].Info()
	if info.Name != "apply_patch" {
		t.Fatalf("工具名：%s", info.Name)
	}
	out, err := ts[0].Invoke(context.Background(),
		json.RawMessage(`{"patch":"*** Begin Patch\n*** Add File: t.txt\n+hi\n*** End Patch\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil || m["ok"] != true {
		t.Fatalf("工具执行错：%s", out)
	}
	// 失败信封（不抛 Go error）
	out, _ = ts[0].Invoke(context.Background(), json.RawMessage(`{"patch":"bad"}`))
	_ = json.Unmarshal([]byte(out), &m)
	if m["ok"] != false {
		t.Fatalf("坏补丁应回喂信封：%s", out)
	}
	if _, err := NewTools(Config{}); err == nil {
		t.Error("空 Root 应拒绝构造")
	}
}

func TestCRLFPreserve(t *testing.T) {
	root := t.TempDir()
	// CRLF 文件：改动后行尾保持 CRLF（含插入行）
	os.WriteFile(filepath.Join(root, "w.txt"), []byte("alpha\r\nbeta\r\ngamma\r\n"), 0o644)
	patch := "*** Begin Patch\n*** Update File: w.txt\n@@\n-alpha\n-beta\n+ALPHA\n+BETA\n*** End Patch\n"
	if _, err := apply(t, root, patch); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "w.txt"))
	if string(b) != "ALPHA\r\nBETA\r\ngamma\r\n" {
		t.Fatalf("CRLF 应保留：%q", string(b))
	}
	// LF 文件保持 LF
	os.WriteFile(filepath.Join(root, "l.txt"), []byte("a\nb\nc\n"), 0o644)
	patch = "*** Begin Patch\n*** Update File: l.txt\n@@\n-b\n+B2\n*** End Patch\n"
	if _, err := apply(t, root, patch); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(root, "l.txt"))
	if string(b) != "a\nB2\nc\n" {
		t.Fatalf("LF 应保持：%q", string(b))
	}
	// 无尾换行文件：更新后补尾换行（codex 历史语义）
	os.WriteFile(filepath.Join(root, "n.txt"), []byte("x\ny"), 0o644)
	patch = "*** Begin Patch\n*** Update File: n.txt\n@@\n-y\n+Y\n*** End Patch\n"
	if _, err := apply(t, root, patch); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(root, "n.txt"))
	if string(b) != "x\nY\n" {
		t.Fatalf("无尾换行更新应补齐：%q", string(b))
	}
}

// TestProtectDirs 写保护区：补丁任一目标（Add/Delete/Update 路径与 Move to
// 改名目标）命中即整单拒绝——事务性（混单中非保护区目标不部分应用）；归一
// 覆盖 ./ 前缀与平台分隔符；空清单零变化；非法条目构造期即拒。
func TestProtectDirs(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "repos"), 0o755)
	os.MkdirAll(filepath.Join(root, "notes"), 0o755)
	os.WriteFile(filepath.Join(root, "repos", "a.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "notes", "b.txt"), []byte("y"), 0o644)
	os.WriteFile(filepath.Join(root, "notes", "c.txt"), []byte("c\n"), 0o644)
	ts, err := NewTools(Config{Root: root, ProtectDirs: []string{"repos"}})
	if err != nil || len(ts) != 1 {
		t.Fatalf("构造失败：%v", err)
	}
	inv := func(t *testing.T, patch string) map[string]any {
		t.Helper()
		out, err := ts[0].Invoke(context.Background(), json.RawMessage(fmt.Sprintf(`{"patch":%q}`, patch)))
		if err != nil {
			t.Fatalf("Invoke 不应返回 Go error：%v", err)
		}
		var m map[string]any
		if json.Unmarshal(out, &m) != nil {
			t.Fatalf("非 JSON 信封：%s", out)
		}
		return m
	}

	// 混单整拒：Add repos/new.txt 命中 → Update notes/b.txt 不部分应用
	mixed := "*** Begin Patch\n*** Update File: notes/b.txt\n@@\n-y\n+z\n*** Add File: repos/new.txt\n+n\n*** End Patch"
	if m := inv(t, mixed); m["ok"] != false || !strings.Contains(m["error"].(string), "写保护区") {
		t.Fatalf("混单含保护区目标应整单拒：%v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "notes", "b.txt")); string(b) != "y" {
		t.Fatalf("混单拒绝应事务性（notes/b.txt 不得部分应用）：%q", b)
	}

	// Delete 命中、Move to 改名目标命中
	del := "*** Begin Patch\n*** Delete File: repos/a.txt\n*** End Patch"
	if m := inv(t, del); m["ok"] != false {
		t.Fatalf("Delete 保护区目标应拒：%v", m)
	}
	mv := "*** Begin Patch\n*** Update File: notes/c.txt\n*** Move to: repos/c.txt\n@@\n-c\n+z\n*** End Patch"
	if m := inv(t, mv); m["ok"] != false {
		t.Fatalf("Move to 保护区目标应拒：%v", m)
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "c.txt")); err != nil {
		t.Fatal("被拒改名不得移动原文件")
	}

	// 归一变体：./ 前缀、平台分隔符、与保护区同名的根下文件
	for _, p := range []string{"./repos/x.txt", filepath.Join("repos", "x.txt"), "repos"} {
		patch := fmt.Sprintf("*** Begin Patch\n*** Add File: %s\n+n\n*** End Patch", p)
		if m := inv(t, patch); m["ok"] != false {
			t.Fatalf("Add %s 应拒（写保护区变体）：%v", p, m)
		}
	}

	// 保护区外照常；空清单零变化
	if m := inv(t, "*** Begin Patch\n*** Add File: notes/ok.txt\n+n\n*** End Patch"); m["ok"] != true {
		t.Fatalf("保护区外应照常：%v", m)
	}
	ts2, err := NewTools(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := ts2[0].Invoke(context.Background(), json.RawMessage(`{"patch":"*** Begin Patch\n*** Add File: repos/free.txt\n+n\n*** End Patch\n"}`))
	var m2 map[string]any
	_ = json.Unmarshal(out, &m2)
	if m2["ok"] != true {
		t.Fatalf("空 ProtectDirs 应零变化：%v", m2)
	}

	// 非法条目构造期即拒
	for _, bad := range []string{"/abs", "..", "a/b", `a\b`, ""} {
		if _, err := NewTools(Config{Root: root, ProtectDirs: []string{bad}}); err == nil {
			t.Fatalf("非法条目 %q 应构造期报错", bad)
		}
	}
}
