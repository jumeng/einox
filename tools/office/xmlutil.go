// xmlutil.go office 族共享 XML/OOXML 小工具：文本转义、Excel 列字母与
// 单元格引用解析、通用节点树（docx/pptx 文本抽取用）。

package office

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// esc XML 文本与属性转义。
func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// colLetters 0 基列号 → Excel 字母（0→A、25→Z、26→AA）。
func colLetters(c int) string {
	s := ""
	for n := c + 1; n > 0; n = (n - 1) / 26 {
		s = string(rune('A'+(n-1)%26)) + s
	}
	return s
}

// parseRef 单元格引用 → 1 基行号 + 0 基列号（"AB12" → 12, 27）。
func parseRef(ref string) (row, col int, ok bool) {
	i := 0
	for i < len(ref) && ref[i] >= 'A' && ref[i] <= 'Z' {
		i++
	}
	if i == 0 || i == len(ref) {
		return 0, 0, false
	}
	c := 0
	for _, ch := range ref[:i] {
		c = c*26 + int(ch-'A'+1)
	}
	n, err := fmt.Sscanf(ref[i:], "%d", &row)
	if err != nil || n != 1 || row < 1 {
		return 0, 0, false
	}
	return row, c - 1, true // 列转 0 基
}

// zipFile 按 zip 内路径找条目（nil = 不存在）。
func zipFile(r *zip.Reader, name string) *zip.File {
	for _, f := range r.File {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// maxZipEntryBytes 单个 zip 条目解压读取上限（zip 炸弹防御——小压缩包可藏
// GB 级 XML，安全审查 2026-09-06：解压器无上限即内存放大面；超限报错不静默
// 截断——截断的 XML 会以「成功但缺内容」形态误导模型）。
const maxZipEntryBytes = 64 << 20

// openCapped 打开 zip 条目并施加解压字节上限（读超限返回错误）。
func openCapped(f *zip.File) (io.ReadCloser, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	return &cappedRC{rc: rc, left: maxZipEntryBytes}, nil
}

// cappedRC 解压字节限流读取（超限 Read 返回错误——xml 解码器向上传播）。
type cappedRC struct {
	rc   io.ReadCloser
	left int64
}

func (c *cappedRC) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, fmt.Errorf("zip 条目解压超上限 %d 字节（疑似压缩炸弹，读取已中止）", maxZipEntryBytes)
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.rc.Read(p)
	c.left -= int64(n)
	return n, err
}

func (c *cappedRC) Close() error { return c.rc.Close() }

// xmlNode 通用节点树（丢命名空间只留局部名；data = 元素内直接字符数据）。
type xmlNode struct {
	local    string
	data     []string
	children []*xmlNode
}

// buildTree 流式解码为节点树。
func buildTree(r io.Reader) (*xmlNode, error) {
	root := &xmlNode{}
	stack := []*xmlNode{root}
	dec := xml.NewDecoder(r)
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
			n := &xmlNode{local: t.Name.Local}
			p := stack[len(stack)-1]
			p.children = append(p.children, n)
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			n := stack[len(stack)-1]
			n.data = append(n.data, string(t))
		}
	}
	return root, nil
}

// text 子树内全部 t 元素文本拼接（docx 正文段/单元格抽取）。
func (n *xmlNode) text() string {
	if n.local == "t" {
		return strings.Join(n.data, "")
	}
	var b strings.Builder
	for _, c := range n.children {
		b.WriteString(c.text())
	}
	return b.String()
}

// collectT 子树内每个 t 元素文本各成一段（pptx 分文本框抽取）。
func collectT(n *xmlNode, out *[]string) {
	if n.local == "t" {
		*out = append(*out, strings.Join(n.data, ""))
		return
	}
	for _, c := range n.children {
		collectT(c, out)
	}
}
