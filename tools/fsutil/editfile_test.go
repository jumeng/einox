package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jumeng/einox/contract"
)

// mkTools 构造 + 落文件（测试惯用前缀）。
func mkTools(t *testing.T, root string) []contract.Tool {
	t.Helper()
	ts, err := NewTools(Config{Root: root, ProtectDirs: []string{"keep"}})
	if err != nil {
		t.Fatalf("构造失败：%v", err)
	}
	return ts
}

func edit(t *testing.T, ts []contract.Tool, args string) map[string]any {
	t.Helper()
	return invoke(t, ts, "edit_file", args)
}

// TestEditFileGate 防盲改门三关：未读拒 / partial 拒 / 外部修改拒；读过且
// 视野完整才放行。
func TestEditFileGate(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha\nbeta\ngamma\n"), 0o644)
	ts := mkTools(t, root)

	// 未读即编辑：拒
	m := edit(t, ts, `{"file_path":"a.txt","old_string":"alpha","new_string":"ALPHA"}`)
	if m["ok"] != false || !strings.Contains(m["error"].(string), "尚未读取") {
		t.Fatalf("未读应拒：%v", m)
	}
	// 读后放行
	invoke(t, ts, "read_file", `{"path":"a.txt"}`)
	if m = edit(t, ts, `{"file_path":"a.txt","old_string":"alpha","new_string":"ALPHA"}`); m["ok"] != true {
		t.Fatalf("读过应放行：%v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); !strings.Contains(string(b), "ALPHA") {
		t.Fatalf("替换未落盘：%s", b)
	}
	// 外部修改后再编辑：拒（mtime/size 指纹失配）
	invoke(t, ts, "read_file", `{"path":"a.txt"}`)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha\nbeta\ngamma\ndelta\n"), 0o644)
	m = edit(t, ts, `{"file_path":"a.txt","old_string":"alpha","new_string":"X"}`)
	if m["ok"] != false || !strings.Contains(m["error"].(string), "被修改") {
		t.Fatalf("外部修改应拒：%v", m)
	}
}

// TestEditFilePartialGate partial 视野拒编辑（默认窗口截断）。
func TestEditFilePartialGate(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	os.WriteFile(filepath.Join(root, "big.txt"), []byte(b.String()), 0o644)
	ts := mkTools(t, root)

	m := invoke(t, ts, "read_file", `{"path":"big.txt"}`) // 默认 2000 行 → partial
	if m["truncated"] != true {
		t.Fatalf("应截断：%v", m["truncated"])
	}
	m = edit(t, ts, `{"file_path":"big.txt","old_string":"line 2999","new_string":"X"}`)
	if m["ok"] != false || !strings.Contains(m["error"].(string), "截断") {
		t.Fatalf("partial 应拒编辑：%v", m)
	}
	// 分段读全（显式 offset/limit 的非默认窗口把文件读完）后放行
	invoke(t, ts, "read_file", `{"path":"big.txt","offset":2001,"limit":2000}`)
	if m = edit(t, ts, `{"file_path":"big.txt","old_string":"line 2999","new_string":"X"}`); m["ok"] != true {
		t.Fatalf("读全后应放行：%v", m)
	}
}

// TestEditFileCreateAndBasic 创建/基础拒绝面。
func TestEditFileCreateAndBasic(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "keep"), 0o755)
	os.WriteFile(filepath.Join(root, "keep", "k.txt"), []byte("k"), 0o644)
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub", "near.txt"), []byte("n"), 0o644)
	ts := mkTools(t, root)

	// old==new 拒
	m := edit(t, ts, `{"file_path":"a.txt","old_string":"x","new_string":"x"}`)
	if m["ok"] != false || !strings.Contains(m["error"].(string), "相同") {
		t.Fatalf("old==new 应拒：%v", m)
	}
	// old 空 + 不存在 = 创建
	if m = edit(t, ts, `{"file_path":"new.txt","old_string":"","new_string":"hello\n"}`); m["ok"] != true || m["created"] != true {
		t.Fatalf("创建应成功：%v", m)
	}
	// old 空 + 已存在 = 拒
	if m = edit(t, ts, `{"file_path":"new.txt","old_string":"","new_string":"y"}`); m["ok"] != false {
		t.Fatalf("已存在应拒创建：%v", m)
	}
	// old 非空 + 不存在 = 拒 + 相近名建议（sub/near.txt 距 nar.txt ≤3）
	m = edit(t, ts, `{"file_path":"sub/nar.txt","old_string":"n","new_string":"y"}`)
	if m["ok"] != false || !strings.Contains(m["error"].(string), "near.txt") {
		t.Fatalf("不存在应带相近名建议：%v", m)
	}
	// 写保护区拒
	invoke(t, ts, "read_file", `{"path":"keep/k.txt"}`)
	m = edit(t, ts, `{"file_path":"keep/k.txt","old_string":"k","new_string":"K"}`)
	if m["ok"] != false || !strings.Contains(m["error"].(string), "写保护区") {
		t.Fatalf("写保护区应拒：%v", m)
	}
}

// TestEditFileAmbiguous 多命中消歧：报全部行号；replace_all 全换。
func TestEditFileAmbiguous(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "dup.txt"), []byte("same\nmid\nsame\nmid\nsame\n"), 0o644)
	ts := mkTools(t, root)
	invoke(t, ts, "read_file", `{"path":"dup.txt"}`)

	m := edit(t, ts, `{"file_path":"dup.txt","old_string":"same","new_string":"X"}`)
	if m["ok"] != false || !strings.Contains(m["error"].(string), "3 处") || !strings.Contains(m["error"].(string), "1") {
		t.Fatalf("多命中应报行号：%v", m)
	}
	if m = edit(t, ts, `{"file_path":"dup.txt","old_string":"same","new_string":"X","replace_all":true}`); m["ok"] != true || m["replaced"].(float64) != 3 {
		t.Fatalf("replace_all 应全换：%v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "dup.txt")); strings.Count(string(b), "X") != 3 {
		t.Fatalf("落盘应 3 处：%s", b)
	}
}

// TestEditMatchers 降级链各档：exact 未命中时逐档兜底，宽松档匹配、原文替换。
func TestEditMatchers(t *testing.T) {
	root := t.TempDir()
	ts := mkTools(t, root)

	cases := []struct {
		name, file, content, old, new string
		wantMatch                     string
	}{
		{"尾空白", "t1.txt", "alpha\n  beta  \ngamma\n", "  beta", "BETA", "尾空白忽略"},
		{"行号前缀-einox形态", "t2.txt", "alpha\nbeta\ngamma\n", "     2→beta", "BETA", "行号前缀剥离"},
		{"行号前缀-cat形态", "t3.txt", "alpha\nbeta\ngamma\n", "2: beta", "BETA", "行号前缀剥离"},
		{"反转义", "t4.txt", "alpha\n\tbeta\ngamma\n", "\\tbeta", "BETA", "转义归一"},
		{"引号归一", "t5.txt", "alpha\n\"beta\"\ngamma\n", "“beta”", "BETA", "引号归一"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			os.WriteFile(filepath.Join(root, c.file), []byte(c.content), 0o644)
			invoke(t, ts, "read_file", fmt.Sprintf(`{"path":%q}`, c.file))
			m := edit(t, ts, fmt.Sprintf(`{"file_path":%q,"old_string":%q,"new_string":%q}`, c.file, c.old, c.new))
			if m["ok"] != true {
				t.Fatalf("应经降级档命中：%v", m)
			}
			if m["match"] != c.wantMatch {
				t.Fatalf("档名 = %v，期望 %v", m["match"], c.wantMatch)
			}
			b, _ := os.ReadFile(filepath.Join(root, c.file))
			if !strings.Contains(string(b), c.new) {
				t.Fatalf("替换应落盘：%s", b)
			}
		})
	}
}

// TestEditBlockAnchor 首尾锚定档：中间行细微出入容差命中。
func TestEditBlockAnchor(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "blk.txt"), []byte("head\nm1 line\nm2 line\nm3 line\ntail\n"), 0o644)
	ts := mkTools(t, root)
	invoke(t, ts, "read_file", `{"path":"blk.txt"}`)
	// 中间行 m2 → m-2（1 字符出入，相似度 0.75……取 2/3=0.67？低于 0.8 阈值；
	// 用更细微的出入：m2 line → m2 ln（4/6≈0.67）不行——换行内一个字符差异于更长行）
	os.WriteFile(filepath.Join(root, "blk.txt"), []byte("head\nm1 line one\nm2 line two\nm3 line three\ntail\n"), 0o644)
	invoke(t, ts, "read_file", `{"path":"blk.txt"}`)
	m := edit(t, ts, `{"file_path":"blk.txt","old_string":"head\nm1 line one\nm2 line tw0\nm3 line three\ntail\n","new_string":"REPLACED"}`)
	// old 中 m2 line tw0（o→0 一字符差，行相似度 11/12≈0.92 ≥ 0.8）
	if m["ok"] != true || m["match"] != "首尾锚定" {
		t.Fatalf("首尾锚定应命中：%v", m)
	}
	b, _ := os.ReadFile(filepath.Join(root, "blk.txt"))
	if string(b) != "REPLACED" {
		t.Fatalf("整段替换应落盘：%q", b)
	}
}

// TestEditKeepTrailingNewline 未参与替换的文件尾换行不丢。
func TestEditKeepTrailingNewline(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "nl.txt"), []byte("a\nb\n"), 0o644)
	ts := mkTools(t, root)
	invoke(t, ts, "read_file", `{"path":"nl.txt"}`)
	if m := edit(t, ts, `{"file_path":"nl.txt","old_string":"b","new_string":"B"}`); m["ok"] != true {
		t.Fatalf("替换失败：%v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "nl.txt")); string(b) != "a\nB\n" {
		t.Fatalf("尾换行应保持：%q", b)
	}
}

// TestEditCRLF CRLF 行尾风格保持。
func TestEditCRLF(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "crlf.txt"), []byte("alpha\r\nbeta\r\ngamma\r\n"), 0o644)
	ts := mkTools(t, root)
	invoke(t, ts, "read_file", `{"path":"crlf.txt"}`)
	if m := edit(t, ts, `{"file_path":"crlf.txt","old_string":"beta","new_string":"BETA"}`); m["ok"] != true {
		t.Fatalf("CRLF 文件应可编辑：%v", m)
	}
	b, _ := os.ReadFile(filepath.Join(root, "crlf.txt"))
	if !strings.Contains(string(b), "BETA\r\n") || strings.Contains(string(b), "\nBETA\n") {
		t.Fatalf("CRLF 风格应保持：%q", b)
	}
}

// TestReadUnchanged 同参重读短路；参数变化正常重读。
func TestReadUnchanged(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "u.txt"), []byte("u1\nu2\n"), 0o644)
	ts := mkTools(t, root)

	m := invoke(t, ts, "read_file", `{"path":"u.txt"}`)
	if m["ok"] != true || m["content"] == nil {
		t.Fatalf("首读应给内容：%v", m)
	}
	m = invoke(t, ts, "read_file", `{"path":"u.txt"}`)
	if m["unchanged"] != true || m["content"] != nil {
		t.Fatalf("同参重读应短路：%v", m)
	}
	m = invoke(t, ts, "read_file", `{"path":"u.txt","offset":2}`)
	if m["ok"] != true || m["content"] == nil {
		t.Fatalf("改参重读应正常：%v", m)
	}
}

// TestEditAtomicNoResidue 原子写不留 tmp 残留。
func TestEditAtomicNoResidue(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "r.txt"), []byte("x\n"), 0o644)
	ts := mkTools(t, root)
	invoke(t, ts, "read_file", `{"path":"r.txt"}`)
	edit(t, ts, `{"file_path":"r.txt","old_string":"x","new_string":"y"}`)
	des, _ := os.ReadDir(root)
	for _, d := range des {
		if strings.Contains(d.Name(), ".einox-edit-") {
			t.Fatalf("tmp 残留：%s", d.Name())
		}
	}
}

// TestReadStateCrossInstance 外部锚注入后跨工具实例保持（engine 跨轮语义）。
func TestReadStateCrossInstance(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "x.txt"), []byte("xx\n"), 0o644)
	state := NewReadState()
	ts1, _ := NewTools(Config{Root: root, ReadState: state})
	ts2, _ := NewTools(Config{Root: root, ReadState: state})
	// ts1 读、ts2 编辑（新工具面 + 同锚 = 跨轮门有效）
	invoke(t, ts1, "read_file", `{"path":"x.txt"}`)
	m := edit(t, ts2, `{"file_path":"x.txt","old_string":"xx","new_string":"yy"}`)
	if m["ok"] != true {
		t.Fatalf("同锚跨实例应放行：%v", m)
	}
	// ts3 无锚新内建：未读应拒（独立实例门从零开始）
	ts3, _ := NewTools(Config{Root: root})
	m = edit(t, ts3, `{"file_path":"x.txt","old_string":"yy","new_string":"zz"}`)
	if m["ok"] != false || !strings.Contains(m["error"].(string), "尚未读取") {
		t.Fatalf("独立实例应从未读拒开始：%v", m)
	}
}
