package office

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jumeng/einox/contract"
)

// invoke 直接驱动工具（构造路径不绕组装）。
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

func argsOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func newTools(t *testing.T, root string) []contract.Tool {
	ts, err := NewTools(Config{Root: root})
	if err != nil || len(ts) != 5 {
		t.Fatalf("构造失败：%v（工具数 %d）", err, len(ts))
	}
	return ts
}

func TestOfficeXlsx(t *testing.T) {
	root := t.TempDir()
	ts := newTools(t, root)

	in := writeXlsxIn{Path: "sub/表.xlsx", Sheets: []xlsxSheetIn{
		{Name: "数据", Rows: [][]any{
			{"名称", "数量", "启用"},
			{"甲", 123, true},
			{"乙", 1.5, false},
			{"丙", "", nil},
		}},
		{Rows: [][]any{{"无名表"}}},
	}}
	if m := invoke(t, ts, "write_xlsx", argsOf(in)); m["ok"] != true {
		t.Fatalf("write_xlsx 失败：%v", m)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "表.xlsx")); err != nil {
		t.Fatalf("文件未落盘（嵌套目录未建）：%v", err)
	}

	// zip 结构完备性：五个必备 part
	zr, err := zip.OpenReader(filepath.Join(root, "sub", "表.xlsx"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	parts := map[string]bool{}
	for _, f := range zr.File {
		parts[f.Name] = true
	}
	for _, p := range []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels", "xl/styles.xml", "xl/worksheets/sheet1.xml", "xl/worksheets/sheet2.xml"} {
		if !parts[p] {
			t.Errorf("缺 zip part：%s", p)
		}
	}

	// 读回：默认表 + 类型序列化 + 空串补位
	m := invoke(t, ts, "read_xlsx", `{"path":"sub/表.xlsx"}`)
	if m["ok"] != true || m["sheet"] != "数据" || m["truncated"] != false {
		t.Fatalf("read_xlsx 失败：%v", m)
	}
	rows := m["rows"].([]any)
	if len(rows) != 4 {
		t.Fatalf("行数错：%d", len(rows))
	}
	r1 := rows[1].([]any)
	if r1[0] != "甲" || r1[1] != "123" || r1[2] != "TRUE" {
		t.Fatalf("类型序列化错：%v", r1)
	}
	r2 := rows[2].([]any)
	if r2[1] != "1.5" || r2[2] != "FALSE" {
		t.Fatalf("数值/布尔错：%v", r2)
	}
	if r3 := rows[3].([]any); r3[1] != "" || r3[2] != "" {
		t.Fatalf("空单元格应补空串：%v", r3)
	}

	// 指定表名（未命名默认 Sheet2）+ 全表名清单
	m = invoke(t, ts, "read_xlsx", `{"path":"sub/表.xlsx","sheet":"Sheet2"}`)
	if m["ok"] != true || m["sheet"] != "Sheet2" {
		t.Fatalf("指定表读失败：%v", m)
	}
	if s := strings.Join(stringify(m["sheets"]), ","); s != "数据,Sheet2" {
		t.Fatalf("表名清单错：%s", s)
	}
	if m := invoke(t, ts, "read_xlsx", `{"path":"sub/表.xlsx","sheet":"ghost"}`); m["ok"] != false {
		t.Errorf("无此表应拒绝：%v", m)
	}

	// 反例：不存在 / 非 xlsx（docx 是 zip 但无 workbook）/ 防穿越 / 非法输入
	if m := invoke(t, ts, "read_xlsx", `{"path":"nope.xlsx"}`); m["ok"] != false {
		t.Errorf("不存在应拒绝：%v", m)
	}
	if m := invoke(t, ts, "write_xlsx", `{"path":"../x.xlsx","sheets":[{"name":"a","rows":[[1]]}]}`); m["ok"] != false {
		t.Errorf("越界写应拒绝：%v", m)
	}
	if m := invoke(t, ts, "read_xlsx", `{"path":"../x"}`); m["ok"] != false {
		t.Errorf("越界读应拒绝：%v", m)
	}
	if m := invoke(t, ts, "write_xlsx", `{"path":"a.xlsx","sheets":[]}`); m["ok"] != false {
		t.Errorf("空 sheets 应拒绝：%v", m)
	}
	if m := invoke(t, ts, "write_xlsx", `{"path":"a.xlsx","sheets":[{"name":"x","rows":[[1]]},{"name":"x","rows":[[1]]}]}`); m["ok"] != false {
		t.Errorf("重名表应拒绝：%v", m)
	}
	if m := invoke(t, ts, "write_xlsx", `{"path":"a.xlsx","sheets":[{"name":"a/b","rows":[[1]]}]}`); m["ok"] != false {
		t.Errorf("非法表名应拒绝：%v", m)
	}
}

func TestXlsxTruncate(t *testing.T) {
	root := t.TempDir()
	ts := newTools(t, root)
	var rows [][]any
	for i := 0; i < readRowCap+100; i++ {
		rows = append(rows, []any{"r" + strconv.Itoa(i)})
	}
	in := writeXlsxIn{Path: "big.xlsx", Sheets: []xlsxSheetIn{{Name: "S", Rows: rows}}}
	if m := invoke(t, ts, "write_xlsx", argsOf(in)); m["ok"] != true {
		t.Fatalf("写入失败：%v", m)
	}
	m := invoke(t, ts, "read_xlsx", `{"path":"big.xlsx"}`)
	if m["ok"] != true || m["truncated"] != true || m["count"] != float64(readRowCap) {
		t.Fatalf("截断语义错：%v", m)
	}
	rowsOut := m["rows"].([]any)
	if last := rowsOut[len(rowsOut)-1].([]any); last[0] != "r"+strconv.Itoa(readRowCap-1) {
		t.Fatalf("截断应保留前 %d 行：%v", readRowCap, last)
	}
}

func TestOfficeDocx(t *testing.T) {
	root := t.TempDir()
	ts := newTools(t, root)

	in := writeDocxIn{Path: "报告.docx", Blocks: []DocxBlock{
		{Type: "heading", Level: 1, Text: "周报标题"},
		{Type: "paragraph", Text: "第一段 <含特殊字符 & \"引号\">"},
		{Type: "list_item", Text: "要点一"},
		{Type: "list_item", Text: "要点二"},
		{Type: "table", Rows: [][]any{{"列A", "列B"}, {"值1", 2}}},
	}}
	if m := invoke(t, ts, "write_docx", argsOf(in)); m["ok"] != true {
		t.Fatalf("write_docx 失败：%v", m)
	}

	m := invoke(t, ts, "read_docx", `{"path":"报告.docx"}`)
	if m["ok"] != true {
		t.Fatalf("read_docx 失败：%v", m)
	}
	all := strings.Join(stringify(m["lines"]), "\n")
	for _, want := range []string{"周报标题", `第一段 <含特殊字符 & "引号">`, "• 要点一", "• 要点二", "列A | 列B", "值1 | 2"} {
		if !strings.Contains(all, want) {
			t.Errorf("正文缺「%s」：%s", want, all)
		}
	}

	// 反例：块校验 + 防穿越
	if m := invoke(t, ts, "write_docx", `{"path":"a.docx","blocks":[]}`); m["ok"] != false {
		t.Errorf("空 blocks 应拒绝：%v", m)
	}
	if m := invoke(t, ts, "write_docx", `{"path":"a.docx","blocks":[{"type":"quote","text":"x"}]}`); m["ok"] != false {
		t.Errorf("非法 type 应拒绝：%v", m)
	}
	if m := invoke(t, ts, "write_docx", `{"path":"a.docx","blocks":[{"type":"heading","level":5,"text":"x"}]}`); m["ok"] != false {
		t.Errorf("level 5 应拒绝：%v", m)
	}
	if m := invoke(t, ts, "write_docx", `{"path":"a.docx","blocks":[{"type":"table"}]}`); m["ok"] != false {
		t.Errorf("无 rows 表格应拒绝：%v", m)
	}
	if m := invoke(t, ts, "read_docx", `{"path":"../x.docx"}`); m["ok"] != false {
		t.Errorf("越界读应拒绝：%v", m)
	}
	if m := invoke(t, ts, "read_docx", `{"path":"ghost.docx"}`); m["ok"] != false {
		t.Errorf("不存在应拒绝：%v", m)
	}
	// docx 读 xlsx（zip 但无 document.xml）应拒绝
	_ = invoke(t, ts, "write_xlsx", `{"path":"x.xlsx","sheets":[{"name":"a","rows":[[1]]}]}`)
	if m := invoke(t, ts, "read_docx", `{"path":"x.xlsx"}`); m["ok"] != false {
		t.Errorf("xlsx 当 docx 读应拒绝：%v", m)
	}
}

func TestOfficePptx(t *testing.T) {
	root := t.TempDir()
	ts := newTools(t, root)

	// 手工极小 pptx：三页乱序写入，验证按序号排序与文本框分段
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entry := func(name, body string) {
		f, _ := zw.Create(name)
		_, _ = f.Write([]byte(body))
	}
	entry("ppt/slides/slide10.xml", `<p:sld><a:t>第十页</a:t></p:sld>`)
	entry("ppt/slides/slide2.xml", `<p:sld><a:t>第二页</a:t><a:t>换行段</a:t></p:sld>`)
	entry("ppt/slides/slide1.xml", `<p:sld><a:t>第一页</a:t></p:sld>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deck.pptx"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	m := invoke(t, ts, "read_pptx", `{"path":"deck.pptx"}`)
	if m["ok"] != true || m["count"] != float64(3) {
		t.Fatalf("read_pptx 失败：%v", m)
	}
	slides := m["slides"].([]any)
	s1 := slides[0].(map[string]any)
	s2 := slides[1].(map[string]any)
	s3 := slides[2].(map[string]any)
	if s1["text"] != "第一页" || s2["text"] != "第二页\n换行段" || s3["text"] != "第十页" {
		t.Fatalf("幻灯片顺序/文本错：%v %v %v", s1, s2, s3)
	}
	if m := invoke(t, ts, "read_pptx", `{"path":"../x.pptx"}`); m["ok"] != false {
		t.Errorf("越界读应拒绝：%v", m)
	}
	if m := invoke(t, ts, "read_pptx", `{"path":"报告不存在.pptx"}`); m["ok"] != false {
		t.Errorf("不存在应拒绝：%v", m)
	}
}

func TestOfficeRefHelpers(t *testing.T) {
	cases := []struct {
		ref  string
		r, c int
		ok   bool
	}{
		{"A1", 1, 0, true},
		{"AB12", 12, 27, true},
		{"Z99", 99, 25, true},
		{"1A", 0, 0, false},
		{"A", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, x := range cases {
		r, c, ok := parseRef(x.ref)
		if ok != x.ok || r != x.r || c != x.c {
			t.Errorf("parseRef(%q) = %d,%d,%v", x.ref, r, c, ok)
		}
	}
	for c, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 27: "AB", 701: "ZZ"} {
		if got := colLetters(c); got != want {
			t.Errorf("colLetters(%d) = %s want %s", c, got, want)
		}
	}
}

// TestOfficeConfig 空 Root 拒绝构造（P0 纪律）。
func TestOfficeConfig(t *testing.T) {
	if _, err := NewTools(Config{}); err == nil {
		t.Error("空 Root 应拒绝构造（不给全盘默认）")
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
