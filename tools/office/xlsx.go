// xlsx.go 工作簿写读：写 = 极小合法 OOXML（zip + inlineStr 单元格，无样式面）；
// 读 = workbook/rels 定位表 + sharedStrings/inlineStr/数值原始值，稀疏补齐成
// 规整二维字符串网格。

package office

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	maxSheets  = 32
	maxRows    = 5000
	maxCols    = 256
	readRowCap = 2000
	readColCap = 256
)

type xlsxSheetIn struct {
	Name string  `json:"name"`
	Rows [][]any `json:"rows"`
}

type writeXlsxIn struct {
	Path   string        `json:"path"`
	Sheets []xlsxSheetIn `json:"sheets"`
}

// helper 工作区路径与实现（闭包共享 root）。resolve 防穿越：Join 清洗 .. 后
// 逃出 root 必须显式拒绝（与 fsutil 同纪律）。
type helper struct {
	root string
}

func (h *helper) resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." {
		return h.root, nil
	}
	a := filepath.Join(h.root, filepath.FromSlash(p))
	rel, err := filepath.Rel(h.root, a)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界（仅限工作区内）：%s", p)
	}
	return a, nil
}

func (h *helper) writeXlsx(_ context.Context, in writeXlsxIn) (map[string]any, error) {
	if len(in.Sheets) == 0 {
		return fail("sheets 不能为空（至少一张工作表）")
	}
	if len(in.Sheets) > maxSheets {
		return fail(fmt.Sprintf("工作表数 %d 超上限 %d", len(in.Sheets), maxSheets))
	}
	names := map[string]bool{}
	for i, sh := range in.Sheets {
		name := strings.TrimSpace(sh.Name)
		if name == "" {
			name = fmt.Sprintf("Sheet%d", i+1)
		}
		if len([]rune(name)) > 31 || strings.ContainsAny(name, "[]:*?/\\") {
			return fail("工作表名非法（≤31 字符且不含 []:*?/\\）：" + name)
		}
		if names[name] {
			return fail("工作表名重复：" + name)
		}
		names[name] = true
		if len(sh.Rows) > maxRows {
			return fail(fmt.Sprintf("表「%s」行数 %d 超上限 %d", name, len(sh.Rows), maxRows))
		}
		for _, row := range sh.Rows {
			if len(row) > maxCols {
				return fail(fmt.Sprintf("表「%s」列数 %d 超上限 %d", name, len(row), maxCols))
			}
		}
	}
	full, err := h.resolve(in.Path)
	if err != nil {
		return fail(err.Error())
	}
	var buf bytes.Buffer
	if err := buildXlsx(&buf, in.Sheets); err != nil {
		return fail("生成失败：" + err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fail("建目录失败：" + err.Error())
	}
	if err := os.WriteFile(full, buf.Bytes(), 0o644); err != nil {
		return fail("写入失败：" + err.Error())
	}
	return map[string]any{"ok": true, "path": in.Path, "sheets": len(in.Sheets), "bytes": buf.Len()}, nil
}

const xlsxStyles = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts><fills count="1"><fill><patternFill patternType="none"/></fill></fills><borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/></cellXfs></styleSheet>`

// buildXlsx 生成极小合法工作簿 zip。
func buildXlsx(w io.Writer, sheets []xlsxSheetIn) error {
	zw := zip.NewWriter(w)
	def := func(name, body string) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = f.Write([]byte(body))
		return err
	}
	var ct, wb, wbr strings.Builder
	ct.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`)
	wb.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	wbr.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	relNS := "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	for i, sh := range sheets {
		name := strings.TrimSpace(sh.Name)
		if name == "" {
			name = fmt.Sprintf("Sheet%d", i+1)
		}
		fmt.Fprintf(&ct, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i+1)
		fmt.Fprintf(&wb, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, esc(name), i+1, i+1)
		fmt.Fprintf(&wbr, `<Relationship Id="rId%d" Type="%s/worksheet" Target="worksheets/sheet%d.xml"/>`, i+1, relNS, i+1)
	}
	fmt.Fprintf(&wbr, `<Relationship Id="rId%d" Type="%s/styles" Target="styles.xml"/>`, len(sheets)+1, relNS)
	ct.WriteString(`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/></Types>`)
	wb.WriteString(`</sheets></workbook>`)
	wbr.WriteString(`</Relationships>`)

	for _, e := range []struct{ name, body string }{
		{"[Content_Types].xml", ct.String()},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", wb.String()},
		{"xl/_rels/workbook.xml.rels", wbr.String()},
		{"xl/styles.xml", xlsxStyles},
	} {
		if err := def(e.name, e.body); err != nil {
			return err
		}
	}
	for i, sh := range sheets {
		if err := def(fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1), sheetXML(sh.Rows)); err != nil {
			return err
		}
	}
	return zw.Close()
}

// sheetXML 行列 → inlineStr/数值单元格（空串跳过，ref 保稀疏位置）。
func sheetXML(rows [][]any) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for ri, row := range rows {
		fmt.Fprintf(&b, `<row r="%d">`, ri+1)
		for ci, v := range row {
			switch x := v.(type) {
			case nil, string:
				s, _ := x.(string)
				if s == "" {
					continue
				}
				fmt.Fprintf(&b, `<c r="%s%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, colLetters(ci), ri+1, esc(s))
			case bool:
				n := 0
				if x {
					n = 1
				}
				fmt.Fprintf(&b, `<c r="%s%d" t="b"><v>%d</v></c>`, colLetters(ci), ri+1, n)
			case float64:
				fmt.Fprintf(&b, `<c r="%s%d"><v>%s</v></c>`, colLetters(ci), ri+1, strconv.FormatFloat(x, 'f', -1, 64))
			default:
				fmt.Fprintf(&b, `<c r="%s%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, colLetters(ci), ri+1, esc(fmt.Sprint(x)))
			}
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

// ── 读面 ──

type readXlsxIn struct {
	Path  string `json:"path"`
	Sheet string `json:"sheet"` // 空 = 第一张
}

// sheetRef workbook 里的工作表（name + zip 内 part 路径）。
type sheetRef struct {
	name string
	part string
}

func (h *helper) readXlsx(_ context.Context, in readXlsxIn) (map[string]any, error) {
	full, err := h.resolve(in.Path)
	if err != nil {
		return fail(err.Error())
	}
	zr, err := zip.OpenReader(full)
	if err != nil {
		return fail("打开失败（不存在或非 xlsx）：" + in.Path)
	}
	defer zr.Close()

	sheets, shared, err := xlsxParts(&zr.Reader)
	if err != nil {
		return fail("解析失败：" + err.Error())
	}
	if len(sheets) == 0 {
		return fail("工作簿无工作表")
	}
	target := sheets[0]
	if s := strings.TrimSpace(in.Sheet); s != "" {
		found := false
		for _, x := range sheets {
			if x.name == s {
				target, found = x, true
				break
			}
		}
		if !found {
			return fail("无此工作表：" + s + "（现有：" + strings.Join(sheetNames(sheets), "、") + "）")
		}
	}
	f := zipFile(&zr.Reader, target.part)
	if f == nil {
		return fail("工作表 part 缺失：" + target.part)
	}
	rows, truncated, err := parseSheetXML(f, shared)
	if err != nil {
		return fail("解析失败：" + err.Error())
	}
	return map[string]any{
		"ok": true, "path": in.Path, "sheet": target.name,
		"sheets": sheetNames(sheets), "rows": rows,
		"count": len(rows), "truncated": truncated,
	}, nil
}

func sheetNames(sheets []sheetRef) []string {
	out := make([]string, 0, len(sheets))
	for _, s := range sheets {
		out = append(out, s.name)
	}
	return out
}

// xlsxParts workbook/rels 解析：表名顺序 + rId→part + sharedStrings 表。
func xlsxParts(r *zip.Reader) ([]sheetRef, []string, error) {
	wb := zipFile(r, "xl/workbook.xml")
	if wb == nil {
		return nil, nil, fmt.Errorf("非 xlsx 结构（缺 xl/workbook.xml）")
	}
	relNS := "http://schemas.openxmlformats.org/package/2006/relationships"
	odNS := "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	id2target := map[string]string{}
	if rels := zipFile(r, "xl/_rels/workbook.xml.rels"); rels != nil {
		rc, err := rels.Open()
		if err != nil {
			return nil, nil, err
		}
		defer rc.Close()
		dec := xml.NewDecoder(rc)
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, nil, err
			}
			if se, ok := tok.(xml.StartElement); ok && se.Name.Space == relNS && se.Name.Local == "Relationship" {
				var id, tgt string
				for _, a := range se.Attr {
					switch a.Name.Local {
					case "Id":
						id = a.Value
					case "Target":
						tgt = a.Value
					}
				}
				id2target[id] = tgt
			}
		}
	}
	var out []sheetRef
	rc, err := wb.Open()
	if err != nil {
		return nil, nil, err
	}
	defer rc.Close()
	dec := xml.NewDecoder(rc)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "sheet" {
			var name, rid string
			for _, a := range se.Attr {
				if a.Name.Local == "name" {
					name = a.Value
				}
				if a.Name.Local == "id" && a.Name.Space == odNS {
					rid = a.Value
				}
			}
			tgt := id2target[rid]
			if strings.HasPrefix(tgt, "/") {
				tgt = strings.TrimPrefix(tgt, "/")
			} else {
				tgt = path.Join("xl", tgt)
			}
			out = append(out, sheetRef{name: name, part: tgt})
		}
	}
	var shared []string
	if ss := zipFile(r, "xl/sharedStrings.xml"); ss != nil {
		if shared, err = readSharedStrings(ss); err != nil {
			return nil, nil, err
		}
	}
	return out, shared, nil
}

// readSharedStrings 共享字符串表（si 内全部 t 拼接，覆盖富文本段）。
func readSharedStrings(f *zip.File) ([]string, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	dec := xml.NewDecoder(rc)
	var list []string
	var cur strings.Builder
	inSi, inT := false, false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSi, inT = true, false
				cur.Reset()
			case "t":
				inT = inSi
			}
		case xml.CharData:
			if inSi && inT {
				cur.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				list = append(list, cur.String())
				inSi = false
			case "t":
				inT = false
			}
		}
	}
	return list, nil
}

// parseSheetXML sheet → 规整二维字符串网格（稀疏行/列补空；超 readRowCap/
// readColCap 截断标记）。值形态：s→共享串、inlineStr→原文本、b→TRUE/FALSE、
// 其余（n/str/日期序列）→原始 v。
func parseSheetXML(f *zip.File, shared []string) ([][]string, bool, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, false, err
	}
	defer rc.Close()
	dec := xml.NewDecoder(rc)
	rows := map[int][]string{}
	var cell strings.Builder
	cellRow, cellCol, lastCol, curRow := 0, 0, -1, 0
	cellType := ""
	inV, inT := false, false
	place := func(r, c int, val string) {
		row := rows[r]
		for len(row) <= c {
			row = append(row, "")
		}
		row[c] = val
		rows[r] = row
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				curRow++
				if r, _, ok := parseRef(attr(t, "r")); ok {
					curRow = r
				}
				lastCol = -1
			case "c":
				cell.Reset()
				cellType = attr(t, "t")
				inV, inT = false, false
				cellRow, cellCol = curRow, lastCol+1
				if r, c, ok := parseRef(attr(t, "r")); ok {
					cellRow, cellCol = r, c
				}
			case "v":
				inV = true
			case "t":
				inT = true
			}
		case xml.CharData:
			if inV || inT {
				cell.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "c":
				var val string
				switch cellType {
				case "s":
					if idx, err := strconv.Atoi(strings.TrimSpace(cell.String())); err == nil && idx >= 0 && idx < len(shared) {
						val = shared[idx]
					}
				case "inlineStr":
					val = cell.String()
				case "b":
					val = "FALSE"
					if strings.TrimSpace(cell.String()) == "1" {
						val = "TRUE"
					}
				default:
					val = strings.TrimSpace(cell.String())
				}
				place(cellRow, cellCol, val)
				lastCol = cellCol
			case "v":
				inV = false
			case "t":
				inT = false
			}
		}
	}
	if len(rows) == 0 {
		return [][]string{}, false, nil
	}
	maxRow, maxCol := 0, 0
	for r, row := range rows {
		if r > maxRow {
			maxRow = r
		}
		if len(row) > maxCol {
			maxCol = len(row)
		}
	}
	truncated := maxRow > readRowCap || maxCol > readColCap
	endRow, width := min(maxRow, readRowCap), min(maxCol, readColCap)
	out := make([][]string, 0, endRow)
	for r := 1; r <= endRow; r++ {
		row := make([]string, width)
		copy(row, rows[r])
		out = append(out, row)
	}
	return out, truncated, nil
}

// attr 起始元素属性取值（空 = 无）。
func attr(se xml.StartElement, name string) string {
	for _, a := range se.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
