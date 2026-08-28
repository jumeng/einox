// pptx.go 幻灯片读：ppt/slides/slideN.xml 按序号排序，a:t 文本框逐段拼接。
// 写面暂缺（无成熟纯 Go 方案，findings/2026-08-25 已记短板）。

package office

import (
	"archive/zip"
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const maxSlides = 200

type readPptxIn struct {
	Path string `json:"path"`
}

func (h *helper) readPptx(_ context.Context, in readPptxIn) (map[string]any, error) {
	full, err := h.resolve(in.Path)
	if err != nil {
		return fail(err.Error())
	}
	zr, err := zip.OpenReader(full)
	if err != nil {
		return fail("打开失败（不存在或非 pptx）：" + in.Path)
	}
	defer zr.Close()

	re := regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)
	type slide struct {
		no   int
		text string
	}
	var slides []slide
	for _, f := range zr.File {
		m := re.FindStringSubmatch(f.Name)
		if m == nil {
			continue
		}
		no, _ := strconv.Atoi(m[1])
		if len(slides) >= maxSlides {
			break
		}
		rc, err := f.Open()
		if err != nil {
			return fail("读取失败：" + f.Name + "：" + err.Error())
		}
		root, err := buildTree(rc)
		rc.Close()
		if err != nil {
			return fail("解析失败：" + f.Name)
		}
		var parts []string
		collectT(root, &parts)
		slides = append(slides, slide{no: no, text: strings.Join(parts, "\n")})
	}
	if len(slides) == 0 {
		return fail("无幻灯片（非 pptx 结构或无 ppt/slides/*.xml）")
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].no < slides[j].no })
	out := make([]map[string]any, 0, len(slides))
	for i, s := range slides {
		out = append(out, map[string]any{"slide": i + 1, "text": s.text})
	}
	return map[string]any{"ok": true, "path": in.Path, "slides": out, "count": len(out)}, nil
}
