// Package fsutil 提供工作区文件工具五件：read_file / list_dir / search_files /
// delete_file / edit_file（读-搜为主 + 显式删除与精确替换——删除走审批卡直白
// 话，比拼 rm 命令可控；成块补丁归 applypatch / runcommand）。行为形态参照
// Claude Code Read/Glob/Grep/Edit 与 deepseek-harness packages/fs/tool-fs-search
// （MIT）；全部路径圈进工作区根，穿越显式拒绝（fail-closed）。
package fsutil

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
)

// Config 构造配置。Root = 会话工作区根——空值拒绝构造（不给全盘默认，
// P0 纪律）。Spill = 可选的 reduction 外置域（会话持久 sessions/<sid>/spill，
// 与历史同寿命——「spill/」前缀路径路由至此根取回，跨轮不失效；其余路径
// 仍圈工作区，穿越显式拒绝 fail-closed）。ProtectDirs = 写保护区顶层子目录
// 名——delete_file/edit_file 命中即拒（读面不受影响）；nil/空 = 不设区零变化。
// ReadState = 可选的读取状态锚（edit_file 的防盲改门与 read_file 的
// unchanged 短路共用；nil = 内建轮内实例——跨轮保持由挂接方注入，见
// ReadStamp）。
type Config struct {
	Root        string
	Spill       string
	ProtectDirs []string
	ReadState   ReadState
}

// NewTools 构造五件（读面：直过审批；写面：edit_file/delete_file 走审批）。
func NewTools(cfg Config) ([]contract.Tool, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("fsutil 需要工作区根（拒绝全盘默认）")
	}
	if err := tools.CheckTopDirs("ProtectDirs", cfg.ProtectDirs); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	h := &helper{root: root, protect: cfg.ProtectDirs, state: cfg.ReadState}
	if h.state == nil {
		h.state = NewReadState() // 内建轮内实例：单轮内 read→edit 门完整，跨轮由挂接方注入
	}
	if cfg.Spill != "" {
		if h.spill, err = filepath.Abs(cfg.Spill); err != nil {
			return nil, err
		}
	}
	rd, err := tools.InferTool("read_file",
		"读取会话工作区文件（带行号）。path 为工作区内相对路径；offset/limit 为 1 基行号区间（默认前 2000 行）。目录无法读——先 list_dir 看结构。输出含总行数，超限会截断并提示分段读。单行超 2000 字符截断——外置结果等单行长文可传 line_width 放宽（上限 50000）。编辑前先读目标文件（edit_file 要求读过才能改）；同参数重读未变化的文件会返回 unchanged 提示。",
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
		"在工作区搜文件：pattern 为 glob（如 **/*.go，支持 doublestar 语法）；query 为正则（在 pattern 命中的文件内逐行匹配，输出 path:行号:内容）。两者至少给一个；只给 pattern = 按名找文件，配合 query = 按内容找。query 模式可传 context_lines（命中行上下文行数）、output_mode（content|files|count，默认 content）、max_results（命中上限，默认 100）。二进制与超大文件自动跳过；常见依赖/产物目录与 .gitignore 命中项自动忽略。",
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
	ed, err := tools.InferTool("edit_file",
		"精确替换文件内容（str_replace 语义，写操作需用户审批）。file_path 为工作区内相对路径；old_string 必须与文件内容精确一致（含缩进）；new_string 为替换内容；多处命中时给 replace_all=true 或扩大 old_string 上下文消歧。old_string 留空且文件不存在 = 创建新文件。编辑前必须先 read_file 读过该文件（读到最新内容且未被外部修改——防盲改）；匹配按精确 → 忽略空白 → 引号/标点归一 → 行号前缀剥离 → 转义归一 → 首尾锚定逐级降级，但请尽量给精确内容。整文件重写或多处大改用 apply_patch。",
		h.editFile)
	if err != nil {
		return nil, err
	}
	return []contract.Tool{tools.WithBehavior(rd, contract.BehaviorRead), tools.WithBehavior(ls, contract.BehaviorRead), tools.WithBehavior(sr, contract.BehaviorRead), tools.WithBehavior(dl, contract.BehaviorWrite), tools.WithBehavior(ed, contract.BehaviorWrite)}, nil
}

// helper 工作区路径与实现（闭包共享 root）。
type helper struct {
	root    string
	spill   string    // reduction 外置域（空 = spill/ 前缀同样走工作区解析）
	protect []string  // 写保护区顶层目录名（空 = delete_file/edit_file 不设栏）
	state   ReadState // 读取状态锚（edit_file 防盲改门 + read_file unchanged 短路）
}

// resolve 路径解析：spill/ 前缀 → 外置域（会话持久，跨轮取回），其余 → 工作区。
// 圈禁判定与 office/applypatch/extwire 同源（tools.ResolveUnder 单点——审查
// P1-5 四份两算法收口）。
func (h *helper) resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if h.spill != "" && (p == "spill" || strings.HasPrefix(p, "spill/")) {
		return tools.ResolveUnder(h.spill, strings.TrimPrefix(strings.TrimPrefix(p, "spill"), "/"))
	}
	return tools.ResolveUnder(h.root, p)
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
	// 扩展名黑名单先行（免读直接拒，提示去向）
	if skipExt[strings.ToLower(filepath.Ext(full))] {
		return fail("二进制/富格式文件（" + filepath.Ext(full) + "）不支持文本读取——office 族用 read_xlsx/read_docx 工具，其他二进制经 run_command 处理")
	}
	offset := in.Offset
	if offset < 1 {
		offset = 1
	}
	limit := in.Limit
	if limit < 1 {
		limit = 2000
	}
	// unchanged 短路（zcode file_unchanged 同款：mtime 整数毫秒 + size 双指纹
	// 未变且请求窗口与上次相同 = 白读，信封提示省 token；要内容改 offset/limit/
	// line_width——三者任一不同即正常重读）
	lineWidthReq := in.LineWidth
	if lineWidthReq < 1 {
		lineWidthReq = 2000
	}
	if lineWidthReq > maxLineWidth {
		lineWidthReq = maxLineWidth
	}
	if h.state != nil {
		if stp, ok := h.state.Stamp(full); ok &&
			stp.MtimeMS == st.ModTime().UnixMilli() && stp.Size == st.Size() &&
			stp.Offset == offset && stp.Limit == limit && stp.LineWidth == lineWidthReq {
			return map[string]any{
				"ok": true, "path": in.Path, "unchanged": true,
				"note": "文件自上次读取未变化——无需重读（要看内容请调整 offset/limit/line_width 参数）",
			}, nil
		}
	}
	f, err := os.Open(full)
	if err != nil {
		return fail("打开失败：" + err.Error())
	}
	defer f.Close()
	// 二进制嗅探（NUL 前 8KB——与 grepFile 同源；read 面此前无此防线）
	head := make([]byte, 8192)
	n, _ := io.ReadFull(f, head)
	if idx := strings.IndexByte(string(head[:n]), 0); idx >= 0 {
		return fail("文件含 NUL 字节，判定为二进制——不支持文本读取；如需处理经 run_command")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail("读取失败：" + err.Error())
	}

	// 窗口式扫描（安全审查 2026-09-06：此前先整读全文件进内存再切片——
	// 数百 MB 日志会打爆进程；现只保留窗口内行，内存 O(offset 区间) 而非 O(文件)）
	var kept []string
	total := 0
	partial := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024) // 外置结果单行可达 MB 级
	for sc.Scan() {
		total++
		if total >= offset && total < offset+limit {
			kept = append(kept, sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		return fail("读取失败：" + err.Error())
	}
	if offset > total {
		return fail(fmt.Sprintf("offset=%d 超出总行数 %d", offset, total))
	}
	end := offset + limit - 1
	if end > total {
		end = total
	}
	lineWidth := lineWidthReq // 已归一（unchanged 比对同源）
	var b strings.Builder
	for i, line := range kept {
		if r := len([]rune(line)); r > lineWidth {
			line = string([]rune(line)[:lineWidth]) +
				fmt.Sprintf("…（单行截断：本行共 %d 字符——传 line_width 放宽，上限 %d）", r, maxLineWidth)
			partial = true
		}
		fmt.Fprintf(&b, "%6d→%s\n", offset+i, line)
	}
	// 视野不完整标记（edit_file 防盲改用）：默认窗口截断（非显式分段）或
	// 行宽截断都算 partial——zcode 语义对位 truncatedByTokenCap
	if end < total {
		partial = true
	}
	if h.state != nil {
		h.state.Record(full, ReadStamp{MtimeMS: st.ModTime().UnixMilli(), Size: st.Size(), Partial: partial, Offset: offset, Limit: limit, LineWidth: lineWidth})
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
	underSpill := h.spill != "" && full != h.root &&
		(full == h.spill || strings.HasPrefix(full, h.spill+string(filepath.Separator)))
	for _, d := range des {
		// 条目路径：工作区列表相对工作区根；spill 外置域列表带 spill/ 虚拟
		// 前缀（安全审查 2026-09-06：此前统一 Rel 工作区根，spill 条目输出
		// ../sessions/<sid>/… 形态——回喂路径不可解析，read_file 无法寻址）
		j := filepath.Join(full, d.Name())
		var rel string
		if underSpill {
			r, err := filepath.Rel(h.spill, j)
			if err != nil {
				r = d.Name()
			}
			rel = path.Join("spill", r)
		} else {
			r, err := filepath.Rel(h.root, j)
			if err != nil {
				r = d.Name()
			}
			rel = r
		}
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
	Pattern      string `json:"pattern"`       // glob（空 = **/*）
	Query        string `json:"query"`         // 正则
	ContextLines int    `json:"context_lines"` // content 模式上下文行数（0=无，钳 10）
	OutputMode   string `json:"output_mode"`   // content|files|count（缺省：无 query=files，有 query=content）
	MaxResults   int    `json:"max_results"`   // 命中/文件上限（默认 100，上限 1000）
}

// skipExt 二进制后缀（内容嗅探之外的第一道过滤）。
var skipExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
	".pdf": true, ".zip": true, ".gz": true, ".tar": true, ".exe": true,
	".woff": true, ".woff2": true, ".ttf": true, ".mp4": true, ".mp3": true,
	".xlsx": true, ".docx": true, ".pptx": true, ".bin": true, ".dat": true,
}

const (
	maxHits       = 100     // 默认命中/文件上限（max_results 可调，上限 1000）
	maxFileSize   = 2 << 20 // 2MB 以上文件不逐行搜
	searchWorkers = 8       // 并行 grep 工作数
)

// builtinIgnore 内置忽略目录名（段名匹配——任意层命中即整棵跳过；rg 默认
// 尊重 .gitignore/跳隐藏的对位，覆盖无 .gitignore 的裸目录形态）。
var builtinIgnore = map[string]bool{
	"node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"__pycache__": true, "dist": true, "build": true, "target": true,
	".idea": true, ".vscode": true, ".pytest_cache": true, ".mypy_cache": true,
	".next": true, "coverage": true, ".gradle": true, ".terraform": true,
}

// ignoreRule .gitignore 简版规则（常见形态：段名/锚定 glob/尾斜杠目录/
// ! 否定；** 与后优先级等边角语义不做——边角从宽：宁可漏忽略不多忽略，
// 漏忽略只是噪声、多忽略会漏结果）。
type ignoreRule struct {
	pat      string
	negate   bool
	dirOnly  bool
	anchored bool // pattern 含 / → 锚定根相对
}

// loadIgnores 解析 root/.gitignore（不存在 = 空规则集）。
func loadIgnores(root string) []ignoreRule {
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return nil
	}
	var rules []ignoreRule
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(strings.TrimSuffix(ln, "\r"))
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		r := ignoreRule{}
		if strings.HasPrefix(ln, "!") {
			r.negate = true
			ln = ln[1:]
		}
		if strings.HasSuffix(ln, "/") {
			r.dirOnly = true
			ln = strings.TrimSuffix(ln, "/")
		}
		if ln == "" {
			continue
		}
		r.anchored = strings.Contains(ln, "/")
		r.pat = strings.Trim(ln, "/")
		rules = append(rules, r)
	}
	return rules
}

// ignoredBy 路径是否被忽略（rel 工作区相对 slash 路径，segs 逐段）。
// 内置集先判；gitignore 否定可反置内置外命中（后者先判赢——简版取
// 「最后一条命中生效」退化近似：negate 命中即放行）。
func ignoredBy(rel string, isDir bool, rules []ignoreRule) bool {
	segs := strings.Split(rel, "/")
	// gitignore 规则（先于内置——negate 不能救回内置命中：内置集语义是
	// 基座级卫生底线，不随仓库配置放宽）
	for _, r := range rules {
		if r.dirOnly && !isDir {
			// 目录 pattern 对文件的祖先段生效：任一祖先段命中即忽略
			if !r.anchored {
				for _, s := range segs[:len(segs)-1] {
					if ok, _ := doublestar.Match(r.pat, s); ok || s == r.pat {
						if r.negate {
							return false
						}
						return true
					}
				}
			}
			continue
		}
		var m bool
		if r.anchored {
			m, _ = doublestar.Match(r.pat, rel)
			if !m && !r.dirOnly {
				m = strings.HasPrefix(rel, r.pat+"/")
			}
		} else {
			for _, s := range segs {
				if ok, _ := doublestar.Match(r.pat, s); ok || s == r.pat {
					m = true
					break
				}
			}
		}
		if m {
			if r.negate {
				return false
			}
			return true
		}
	}
	// 内置集：目录段名命中（文件路径的祖先段或目录本身）
	for _, s := range segs {
		if builtinIgnore[s] {
			return true
		}
	}
	return false
}

func (h *helper) searchFiles(_ context.Context, in searchIn) (map[string]any, error) {
	if strings.TrimSpace(in.Pattern) == "" && strings.TrimSpace(in.Query) == "" {
		return fail("pattern 与 query 至少给一个")
	}
	pattern := strings.TrimSpace(in.Pattern)
	if pattern == "" {
		pattern = "**/*"
	}
	mode := strings.TrimSpace(in.OutputMode)
	var re *regexp.Regexp
	if q := strings.TrimSpace(in.Query); q != "" {
		var err error
		re, err = regexp.Compile(q)
		if err != nil {
			return fail("query 正则非法：" + err.Error())
		}
		if mode == "" {
			mode = "content"
		}
	} else if mode == "" {
		mode = "files"
	}
	switch mode {
	case "content", "files", "count":
	default:
		return fail("output_mode 取值 content|files|count（收到 " + mode + "）")
	}
	if re != nil && mode == "files" {
		mode = "content" // 有 query 时 files 退化为 content（files 语义 = 按名找）
	}
	maxR := in.MaxResults
	if maxR < 1 {
		maxR = maxHits
	}
	maxR = min(maxR, 1000)
	ctxN := min(max(in.ContextLines, 0), 10)

	// 收集阶段：过滤 ignore（.git 隐藏目录天然在内置跳过面）+ pattern
	rules := loadIgnores(h.root)
	var files []string
	_ = filepath.WalkDir(h.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || len(files) >= 10000 {
			return nil
		}
		rel, rerr := filepath.Rel(h.root, p)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// 隐藏目录 + ignore 目录整棵跳过（.git 含于隐藏面）
			if strings.HasPrefix(d.Name(), ".") || ignoredBy(rel, true, rules) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") { // 隐藏文件（.env 等——rg 默认同款）
			return nil
		}
		if ignoredBy(rel, false, rules) {
			return nil
		}
		if re != nil {
			if skipExt[strings.ToLower(filepath.Ext(p))] {
				return nil
			}
			if info, ierr := d.Info(); ierr == nil && info.Size() > maxFileSize {
				return nil
			}
		}
		if ok, _ := doublestar.Match(pattern, rel); ok {
			files = append(files, rel)
		}
		return nil
	})

	// pattern-only / files 模式：文件名清单即结果
	if re == nil {
		trunc := false
		if len(files) > maxR {
			files = files[:maxR]
			trunc = true
		}
		return map[string]any{"ok": true, "files": files, "count": len(files), "truncated": trunc}, nil
	}

	// 并行 grep（结果排序去重上下文行——并发完成序不稳定，统一按 path/line 排）
	type job struct{ rel, full string }
	sem := make(chan struct{}, searchWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var hits []hit
	var counts []map[string]any
	countByFile := map[string]int{}
	for _, rel := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func(rel string) {
			defer wg.Done()
			defer func() { <-sem }()
			full := filepath.Join(h.root, filepath.FromSlash(rel))
			if mode == "content" {
				hs := grepCtx(full, rel, re, ctxN)
				mu.Lock()
				hits = append(hits, hs...)
				mu.Unlock()
				return
			}
			n := grepCount(full, re)
			if n > 0 {
				mu.Lock()
				countByFile[rel] = n
				mu.Unlock()
			}
		}(rel)
	}
	wg.Wait()

	out := map[string]any{"ok": true}
	switch mode {
	case "count":
		for rel, n := range countByFile {
			counts = append(counts, map[string]any{"path": rel, "count": n})
		}
		sort.Slice(counts, func(i, j int) bool {
			return counts[i]["path"].(string) < counts[j]["path"].(string)
		})
		trunc := false
		if len(counts) > maxR {
			counts = counts[:maxR]
			trunc = true
		}
		total := 0
		for _, c := range counts {
			total += c["count"].(int)
		}
		out["counts"], out["files_matched"], out["total"], out["truncated"] = counts, len(counts), total, trunc
	default: // content
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].Path != hits[j].Path {
				return hits[i].Path < hits[j].Path
			}
			return hits[i].Line < hits[j].Line
		})
		trunc := false
		if len(hits) > maxR {
			hits = hits[:maxR]
			trunc = true
		}
		out["matches"], out["count"], out["truncated"] = hits, len(hits), trunc
	}
	return out, nil
}

// hit 搜索单行命中（Ctx=true 为上下文行——context_lines 展开的邻近行，
// 非命中本身；模型据此区分命中与语境）。
type hit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
	Ctx  bool   `json:"ctx,omitempty"`
}

// grepCtx 单文件逐行匹配 + 上下文行展开（同文件重叠区间自然去重——
// 行号集合化；NUL 嗅探跳二进制）。
func grepCtx(p, rel string, re *regexp.Regexp, ctxN int) []hit {
	b, err := os.ReadFile(p)
	if err != nil || len(b) > maxFileSize {
		return nil
	}
	if idx := strings.IndexByte(string(b[:min(len(b), 8192)]), 0); idx >= 0 {
		return nil // 二进制
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	isMatch := make([]bool, len(lines))
	nMatch := 0
	for i, line := range lines {
		if re.MatchString(line) {
			isMatch[i] = true
			nMatch++
		}
	}
	if nMatch == 0 {
		return nil
	}
	want := make([]bool, len(lines))
	for i, m := range isMatch {
		if !m {
			continue
		}
		for j := max(0, i-ctxN); j <= min(len(lines)-1, i+ctxN); j++ {
			want[j] = true
		}
	}
	var hits []hit
	for i, line := range lines {
		if !want[i] {
			continue
		}
		line = strings.TrimRight(line, "\r")
		if len([]rune(line)) > 200 {
			line = string([]rune(line)[:200]) + "…"
		}
		hits = append(hits, hit{Path: rel, Line: i + 1, Text: line, Ctx: !isMatch[i]})
	}
	return hits
}

// grepCount 单文件命中计数（count 模式）。
func grepCount(p string, re *regexp.Regexp) int {
	b, err := os.ReadFile(p)
	if err != nil || len(b) > maxFileSize {
		return 0
	}
	if idx := strings.IndexByte(string(b[:min(len(b), 8192)]), 0); idx >= 0 {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if re.MatchString(line) {
			n++
		}
	}
	return n
}

type deleteIn struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"` // 目录整删须显式
}

func (h *helper) deleteFile(_ context.Context, in deleteIn) (map[string]any, error) {
	for _, d := range h.protect {
		if tools.PathBlocked(in.Path, []string{d}) {
			return fail("路径位于写保护区 " + d + " 内，拒绝删除：" + in.Path)
		}
	}
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

func fail(msg string) (map[string]any, error) { return tools.Fail(msg), nil } // 信封单点（tools.Fail——审查 P2-11）
