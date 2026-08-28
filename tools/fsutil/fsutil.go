// Package fsutil 提供工作区文件工具四件：read_file / list_dir / search_files /
// delete_file（读-搜为主 + 显式删除——删除走审批卡直白话，比拼 rm 命令可控；
// 内容写面归 applypatch / str_replace_editor / runcommand）。行为形态参照
// Claude Code Read/Glob/Grep 与 deepseek-harness packages/fs/tool-fs-search
// （MIT）；全部路径圈进工作区根，穿越显式拒绝（fail-closed）。
package fsutil

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
)

// Config 构造配置。Root = 会话工作区根——空值拒绝构造（不给全盘默认，
// P0 纪律）。Spill = 可选的 reduction 外置域（会话持久 sessions/<sid>/spill，
// 与历史同寿命——「spill/」前缀路径路由至此根取回，跨轮不失效；其余路径
// 仍圈工作区，穿越显式拒绝 fail-closed）。
type Config struct {
	Root  string
	Spill string
}

// NewTools 构造三件（读面：直过审批）。
func NewTools(cfg Config) ([]contract.Tool, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("fsutil 需要工作区根（拒绝全盘默认）")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	h := &helper{root: root}
	if cfg.Spill != "" {
		if h.spill, err = filepath.Abs(cfg.Spill); err != nil {
			return nil, err
		}
	}
	rd, err := tools.InferTool("read_file",
		"读取会话工作区文件（带行号）。path 为工作区内相对路径；offset/limit 为 1 基行号区间（默认前 2000 行）。目录无法读——先 list_dir 看结构。输出含总行数，超限会截断并提示分段读。单行超 2000 字符截断——外置结果等单行长文可传 line_width 放宽（上限 50000）。",
		h.readFile)
	if err != nil {
		return nil, err
	}
	ls, err := tools.InferTool("list_dir",
		"列出会话工作区某目录的直接子项（目录在前文件在后，含大小）。path 为相对路径，空 = 工作区根。",
		h.listDir)
	if err != nil {
		return nil, err
	}
	sr, err := tools.InferTool("search_files",
		"在工作区搜文件：pattern 为 glob（如 **/*.go，支持 doublestar 语法）；query 为正则（在 pattern 命中的文件内逐行匹配，输出 path:行号:内容）。两者至少给一个；只给 pattern = 按名找文件，配合 query = 按内容找。二进制与超大文件自动跳过。",
		h.searchFiles)
	if err != nil {
		return nil, err
	}
	dl, err := tools.InferTool("delete_file",
		"删除会话工作区内的文件或目录（删除操作需用户审批确认）。path 为相对路径；目录须 recursive=true（递归整删，审批卡可见路径）。删除不可恢复——只清工作区临时产物，业务数据不在此工具管辖内。",
		h.deleteFile)
	if err != nil {
		return nil, err
	}
	return []contract.Tool{tools.WithBehavior(rd, contract.BehaviorRead), tools.WithBehavior(ls, contract.BehaviorRead), tools.WithBehavior(sr, contract.BehaviorRead), tools.WithBehavior(dl, contract.BehaviorWrite)}, nil
}

// helper 工作区路径与实现（闭包共享 root）。
type helper struct {
	root  string
	spill string // reduction 外置域（空 = spill/ 前缀同样走工作区解析）
}

// resolveUnder 指定根内路径解析（防穿越：Join 清洗 .. 后逃出 root 必须显式拒绝）。
func resolveUnder(root, p string) (string, error) {
	if p == "" || p == "." {
		return root, nil
	}
	a := filepath.Join(root, filepath.FromSlash(p))
	rel, err := filepath.Rel(root, a)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界（仅限工作区内）：%s", p)
	}
	return a, nil
}

// resolve 路径解析：spill/ 前缀 → 外置域（会话持久，跨轮取回），其余 → 工作区。
func (h *helper) resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if h.spill != "" && (p == "spill" || strings.HasPrefix(p, "spill/")) {
		return resolveUnder(h.spill, strings.TrimPrefix(strings.TrimPrefix(p, "spill"), "/"))
	}
	return resolveUnder(h.root, p)
}

// maxLineWidth 单行截断放宽上限（外置工具结果多为单行 JSON，3 万字符量级
// 常态——50000 runes 足够整读；防误传百万级把上下文打爆）。
const maxLineWidth = 50000

type readFileIn struct {
	Path      string `json:"path"`
	Offset    int    `json:"offset"`     // 1 基；0 = 从头
	Limit     int    `json:"limit"`      // 0 = 默认 2000
	LineWidth int    `json:"line_width"` // 0 = 默认 2000；单行截断上限
}

func (h *helper) readFile(_ context.Context, in readFileIn) (map[string]any, error) {
	full, err := h.resolve(in.Path)
	if err != nil {
		return fail(err.Error())
	}
	st, err := os.Stat(full)
	if err != nil {
		return fail("文件不存在：" + in.Path + "——先 list_dir 或 search_files 确认路径")
	}
	if st.IsDir() {
		return fail("是目录不是文件：" + in.Path + "——用 list_dir 看子项")
	}
	f, err := os.Open(full)
	if err != nil {
		return fail("打开失败：" + err.Error())
	}
	defer f.Close()

	offset := in.Offset
	if offset < 1 {
		offset = 1
	}
	limit := in.Limit
	if limit < 1 {
		limit = 2000
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024) // 外置结果单行可达 MB 级
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return fail("读取失败：" + err.Error())
	}
	total := len(lines)
	if offset > total {
		return fail(fmt.Sprintf("offset=%d 超出总行数 %d", offset, total))
	}
	end := offset + limit - 1
	if end > total {
		end = total
	}
	lineWidth := in.LineWidth
	if lineWidth < 1 {
		lineWidth = 2000
	}
	if lineWidth > maxLineWidth {
		lineWidth = maxLineWidth
	}
	var b strings.Builder
	for i := offset; i <= end; i++ {
		line := lines[i-1]
		if r := len([]rune(line)); r > lineWidth {
			line = string([]rune(line)[:lineWidth]) +
				fmt.Sprintf("…（单行截断：本行共 %d 字符——传 line_width 放宽，上限 %d）", r, maxLineWidth)
		}
		fmt.Fprintf(&b, "%6d→%s\n", i, line)
	}
	out := map[string]any{
		"ok": true, "path": in.Path,
		"content": b.String(),
		"lines":   total, "offset": offset, "end": end,
	}
	if end < total {
		out["truncated"] = true
		out["hint"] = fmt.Sprintf("共 %d 行，已读 %d~%d——续读传 offset=%d", total, offset, end, end+1)
	}
	return out, nil
}

type listDirIn struct {
	Path string `json:"path"`
}

func (h *helper) listDir(_ context.Context, in listDirIn) (map[string]any, error) {
	full, err := h.resolve(in.Path)
	if err != nil {
		return fail(err.Error())
	}
	des, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return fail("目录不存在：" + in.Path)
		}
		return fail(err.Error())
	}
	type entry struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Dir  bool   `json:"dir"`
		Size int64  `json:"size"`
	}
	var dirs, files []entry
	for _, d := range des {
		rel, _ := filepath.Rel(h.root, filepath.Join(full, d.Name()))
		e := entry{Name: d.Name(), Path: filepath.ToSlash(rel), Dir: d.IsDir()}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				e.Size = info.Size()
			}
		}
		if d.IsDir() {
			dirs = append(dirs, e)
		} else {
			files = append(files, e)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	all := append(dirs, files...)
	truncated := false
	if len(all) > 500 {
		all = all[:500]
		truncated = true
	}
	return map[string]any{
		"ok": true, "path": in.Path, "entries": all,
		"count": len(all), "truncated": truncated,
	}, nil
}

type searchIn struct {
	Pattern string `json:"pattern"` // glob（空 = **/*）
	Query   string `json:"query"`   // 正则
}

// skipExt 二进制后缀（内容嗅探之外的第一道过滤）。
var skipExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
	".pdf": true, ".zip": true, ".gz": true, ".tar": true, ".exe": true,
	".woff": true, ".woff2": true, ".ttf": true, ".mp4": true, ".mp3": true,
	".xlsx": true, ".docx": true, ".pptx": true, ".bin": true, ".dat": true,
}

const (
	maxHits     = 100
	maxPathHits = 200
	maxFileSize = 2 << 20 // 2MB 以上文件不逐行搜
)

func (h *helper) searchFiles(_ context.Context, in searchIn) (map[string]any, error) {
	if strings.TrimSpace(in.Pattern) == "" && strings.TrimSpace(in.Query) == "" {
		return fail("pattern 与 query 至少给一个")
	}
	pattern := strings.TrimSpace(in.Pattern)
	if pattern == "" {
		pattern = "**/*"
	}
	var re *regexp.Regexp
	if q := strings.TrimSpace(in.Query); q != "" {
		var err error
		re, err = regexp.Compile(q)
		if err != nil {
			return fail("query 正则非法：" + err.Error())
		}
	}
	var hits []hit
	var paths []string
	_ = filepath.WalkDir(h.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || hits != nil && len(hits) >= maxHits || len(paths) >= maxPathHits {
			return nil
		}
		rel, rerr := filepath.Rel(h.root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		ok, _ := doublestar.Match(pattern, rel)
		if !ok {
			return nil
		}
		if re == nil {
			paths = append(paths, rel)
			return nil
		}
		if skipExt[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Size() > maxFileSize {
			return nil
		}
		hs := grepFile(p, rel, re)
		hits = append(hits, hs...)
		return nil
	})
	out := map[string]any{"ok": true}
	if re == nil {
		out["files"] = paths
		out["count"] = len(paths)
		out["truncated"] = len(paths) >= maxPathHits
		return out, nil
	}
	out["matches"] = hits
	out["count"] = len(hits)
	out["truncated"] = len(hits) >= maxHits
	return out, nil
}

// hit 搜索单行命中。
type hit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// grepFile 单文件逐行匹配（NUL 嗅探跳二进制）。
func grepFile(p, rel string, re *regexp.Regexp) []hit {
	b, err := os.ReadFile(p)
	if err != nil || len(b) > maxFileSize {
		return nil
	}
	if idx := strings.IndexByte(string(b[:min(len(b), 8192)]), 0); idx >= 0 {
		return nil // 二进制
	}
	var hits []hit
	for i, line := range strings.Split(string(b), "\n") {
		if re.MatchString(line) {
			line = strings.TrimRight(line, "\r")
			if len([]rune(line)) > 200 {
				line = string([]rune(line)[:200]) + "…"
			}
			hits = append(hits, hit{Path: rel, Line: i + 1, Text: line})
			if len(hits) >= maxHits {
				break
			}
		}
	}
	return hits
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type deleteIn struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"` // 目录整删须显式
}

func (h *helper) deleteFile(_ context.Context, in deleteIn) (map[string]any, error) {
	full, err := h.resolve(in.Path)
	if err != nil {
		return fail(err.Error())
	}
	st, err := os.Lstat(full)
	if err != nil {
		return fail("不存在：" + in.Path)
	}
	if st.IsDir() && !in.Recursive {
		return fail("是目录：" + in.Path + "——目录删除须 recursive=true（整目录移除，审批可见）")
	}
	if err := os.RemoveAll(full); err != nil {
		return fail("删除失败：" + err.Error())
	}
	return map[string]any{"ok": true, "deleted": in.Path, "was_dir": st.IsDir()}, nil
}

func fail(msg string) (map[string]any, error) {
	return map[string]any{"ok": false, "error": msg}, nil // 回喂模型自纠（errFeed 语义）
}
