// docx.go 文档写读：写 = blocks（标题/段落/列表项/表格）→ 极小合法 docx
//（zip + document.xml + styles.xml，无样式定制面）；读 = 正文段落逐行 +
// 表格行「 | 」连接。list_item 以圆点段落呈现（语义编号列表不在极小面）。

package office

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	maxBlocks    = 5000
	readLineCap  = 20000
	docxPageSize = `<w:pgSz w:w="11906" w:h="16838"/><w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440"/>`
)

const docxStyles = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style><w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:basedOn w:val="Normal"/><w:pPr><w:outlineLvl w:val="0"/></w:pPr><w:rPr><w:b/><w:sz w:val="36"/></w:rPr></w:style><w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:basedOn w:val="Normal"/><w:pPr><w:outlineLvl w:val="1"/></w:pPr><w:rPr><w:b/><w:sz w:val="30"/></w:rPr></w:style><w:style w:type="paragraph" w:styleId="Heading3"><w:name w:val="heading 3"/><w:basedOn w:val="Normal"/><w:pPr><w:outlineLvl w:val="2"/></w:pPr><w:rPr><w:b/><w:sz w:val="26"/></w:rPr></w:style><w:style w:type="paragraph" w:styleId="Heading4"><w:name w:val="heading 4"/><w:basedOn w:val="Normal"/><w:pPr><w:outlineLvl w:val="3"/></w:pPr><w:rPr><w:b/><w:sz w:val="24"/></w:rPr></w:style></w:styles>`

// DocxBlock docx 内容块（GenDocx 与 write_docx 共享输入面）。
type DocxBlock struct {
	Type  string  `json:"type"`  // heading|paragraph|list_item|table
	Level int     `json:"level"` // heading 1-4
	Text  string  `json:"text"`
	Rows  [][]any `json:"rows"` // table
}

type writeDocxIn struct {
	Path   string      `json:"path"`
	Blocks []DocxBlock `json:"blocks"`
}

// GenDocx 内容块 → docx 字节（含全部块校验）。产品装配经 store 唯一写入器
// 落盘时复用的生成面——基座出机制，落盘域与审批归应用装配层。
func GenDocx(blocks []DocxBlock) ([]byte, error) {
	if err := validateDocxBlocks(blocks); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := buildDocx(&buf, blocks); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// validateDocxBlocks 块校验（空/超限/类型与必填字段）。
func validateDocxBlocks(blocks []DocxBlock) error {
	if len(blocks) == 0 {
		return errors.New("blocks 不能为空")
	}
	if len(blocks) > maxBlocks {
		return fmt.Errorf("内容块数 %d 超上限 %d", len(blocks), maxBlocks)
	}
	for i, b := range blocks {
		switch b.Type {
		case "heading":
			if strings.TrimSpace(b.Text) == "" {
				return fmt.Errorf("第 %d 块 heading 缺 text", i+1)
			}
			if b.Level < 1 || b.Level > 4 {
				return fmt.Errorf("第 %d 块 heading level %d 非法（1-4）", i+1, b.Level)
			}
		case "paragraph", "list_item":
			if strings.TrimSpace(b.Text) == "" {
				return fmt.Errorf("第 %d 块 %s 缺 text", i+1, b.Type)
			}
		case "table":
			if len(b.Rows) == 0 {
				return fmt.Errorf("第 %d 块 table 缺 rows", i+1)
			}
			if len(b.Rows) > maxRows {
				return fmt.Errorf("第 %d 块 table 行数超上限 %d", i+1, maxRows)
			}
			for _, r := range b.Rows {
				if len(r) > maxCols {
					return fmt.Errorf("第 %d 块 table 列数超上限 %d", i+1, maxCols)
				}
			}
		default:
			return fmt.Errorf("第 %d 块 type 非法（heading/paragraph/list_item/table）：%s", i+1, b.Type)
		}
	}
	return nil
}

func (h *helper) writeDocx(_ context.Context, in writeDocxIn) (map[string]any, error) {
	b, err := GenDocx(in.Blocks)
	if err != nil {
		return fail(err.Error())
	}
	full, err := h.resolve(in.Path)
	if err != nil {
		return fail(err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fail("建目录失败：" + err.Error())
	}
	if err := os.WriteFile(full, b, 0o644); err != nil {
		return fail("写入失败：" + err.Error())
	}
	return map[string]any{"ok": true, "path": in.Path, "blocks": len(in.Blocks), "bytes": len(b)}, nil
}

// docxP 段落 XML（style 空 = 正文；bold = 首行表头等加粗）。
func docxP(style, text string, bold bool) string {
	var pPr, rPr strings.Builder
	if style != "" {
		fmt.Fprintf(&pPr, `<w:pStyle w:val="%s"/>`, style)
	}
	if bold {
		rPr.WriteString(`<w:b/>`)
	}
	return fmt.Sprintf(`<w:p>%s<w:r>%s<w:t xml:space="preserve">%s</w:t></w:r></w:p>`,
		wrap("w:pPr", pPr.String()), wrap("w:rPr", rPr.String()), esc(text))
}

// wrap 有内容才包标签（空元素省略）。
func wrap(tag, inner string) string {
	if inner == "" {
		return ""
	}
	return "<" + tag + ">" + inner + "</" + tag + ">"
}

// docxCell 表格单元格（首行 bold 表头）。
func docxCell(text string, bold bool) string {
	return `<w:tc><w:tcPr><w:tcW w:w="0" w:type="auto"/></w:tcPr>` + docxP("", text, bold) + `</w:tc>`
}

// docxTable 单线边框表格，首行加粗表头。
func docxTable(rows [][]any) string {
	var b strings.Builder
	const border = `<w:tblBorders><w:top w:val="single" w:sz="4" w:color="auto"/><w:left w:val="single" w:sz="4" w:color="auto"/><w:bottom w:val="single" w:sz="4" w:color="auto"/><w:right w:val="single" w:sz="4" w:color="auto"/><w:insideH w:val="single" w:sz="4" w:color="auto"/><w:insideV w:val="single" w:sz="4" w:color="auto"/></w:tblBorders>`
	b.WriteString(`<w:tbl><w:tblPr><w:tblW w:w="0" w:type="auto"/>` + border + `</w:tblPr>`)
	for ri, row := range rows {
		b.WriteString(`<w:tr>`)
		for _, v := range row {
			b.WriteString(docxCell(cellText(v), ri == 0))
		}
		b.WriteString(`</w:tr>`)
	}
	b.WriteString(`</w:tbl>`)
	return b.String()
}

// cellText 单元格值 → 字符串（数字保精度形态）。
func cellText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "TRUE"
		}
		return "FALSE"
	default:
		return fmt.Sprint(x)
	}
}

// buildDocx blocks → docx zip。
func buildDocx(w io.Writer, blocks []DocxBlock) error {
	zw := zip.NewWriter(w)
	def := func(name, body string) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = f.Write([]byte(body))
		return err
	}
	var body strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "heading":
			body.WriteString(docxP("Heading"+strconv.Itoa(b.Level), b.Text, false))
		case "paragraph":
			body.WriteString(docxP("", b.Text, false))
		case "list_item":
			body.WriteString(docxP("", "• "+b.Text, false))
		case "table":
			body.WriteString(docxTable(b.Rows))
		}
	}
	document := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><w:body>` +
		body.String() + `<w:sectPr>` + docxPageSize + `</w:sectPr></w:body></w:document>`
	for _, e := range []struct{ name, body string }{
		{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/><Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/></Types>`},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`},
		{"word/document.xml", document},
		{"word/styles.xml", docxStyles},
	} {
		if err := def(e.name, e.body); err != nil {
			return err
		}
	}
	return zw.Close()
}

// ── 读面 ──

type readDocxIn struct {
	Path string `json:"path"`
}

func (h *helper) readDocx(_ context.Context, in readDocxIn) (map[string]any, error) {
	full, err := h.resolve(in.Path)
	if err != nil {
		return fail(err.Error())
	}
	zr, err := zip.OpenReader(full)
	if err != nil {
		return fail("打开失败（不存在或非 docx）：" + in.Path)
	}
	defer zr.Close()
	f := zipFile(&zr.Reader, "word/document.xml")
	if f == nil {
		return fail("非 docx 结构（缺 word/document.xml）")
	}
	rc, err := f.Open()
	if err != nil {
		return fail("读取失败：" + err.Error())
	}
	defer rc.Close()
	root, err := buildTree(rc)
	if err != nil {
		return fail("解析失败：" + err.Error())
	}
	var lines []string
	truncated := false
	addLine := func(s string) {
		if len(lines) >= readLineCap {
			truncated = true
			return
		}
		lines = append(lines, s)
	}
	// body 直接子元素：p → 一行；tbl → 每 tr 一行（单元格「 | 」连接）
	var walkBody func(n *xmlNode)
	walkBody = func(n *xmlNode) {
		for _, c := range n.children {
			switch c.local {
			case "p":
				addLine(c.text())
			case "tbl":
				for _, tr := range c.children {
					if tr.local != "tr" {
						continue
					}
					var cells []string
					for _, tc := range tr.children {
						if tc.local == "tc" {
							cells = append(cells, tc.text())
						}
					}
					addLine(strings.Join(cells, " | "))
				}
			case "sdt", "sdtContent":
				walkBody(c) // 结构化文档标签透传其内容
			}
		}
	}
	var body *xmlNode
	var find func(n *xmlNode) *xmlNode
	find = func(n *xmlNode) *xmlNode {
		for _, c := range n.children {
			if c.local == "body" {
				return c
			}
			if r := find(c); r != nil {
				return r
			}
		}
		return nil
	}
	body = find(root)
	if body == nil {
		return fail("document.xml 无 body")
	}
	walkBody(body)
	return map[string]any{
		"ok": true, "path": in.Path, "lines": lines,
		"count": len(lines), "truncated": truncated,
	}, nil
}
