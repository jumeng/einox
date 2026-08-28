// Package applypatch 提供 apply_patch 工具：codex `*** Begin Patch` 格式的
// 解析与应用（多文件 增/改/删/改名，事务性——任一失败全部不落盘）。
// 格式与匹配算法移植自 openai/codex codex-rs/apply-patch crate
// （parser.rs / file_update.rs / seek_sequence.rs，Apache-2.0；streaming 与
// CLI 部分不移植）。行尾策略取 NormalizeToLf 模式（统一 LF + 保留尾换行；
// PreserveLineEndings 混合行尾保留留后续）。
package applypatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
)

// Config 构造配置。Root = 工作区根（空 = 拒绝构造，P0 纪律）。
type Config struct {
	Root string
}

// ---- 补丁模型（parser 产物） ----

// Chunk 单个改动块（一个 @@ 区段）。OldLines/NewLines 含上下文行；
// ContextPairs 记录两侧同为上下文的行号对（PreserveLineEndings 模式用，
// 本模式仅调试展示）。
type Chunk struct {
	ChangeContext string // @@ 头文本（定位锚，单行 seek）
	OldLines      []string
	NewLines      []string
	IsEOF         bool // *** End of File 标记：优先在文件尾匹配
}

// Kind 文件操作类型。
type Kind int

// 文件操作三态。
const (
	KindAdd Kind = iota
	KindUpdate
	KindDelete
)

// FileOp 单文件操作。
type FileOp struct {
	Kind     Kind
	Path     string
	MoveTo   string   // Update 可选改名
	Chunks   []Chunk  // Update
	Contents []string // Add（+ 行内容）
}

// ---- 解析（规范：prompt_with_apply_patch_instructions.md 文法） ----

const (
	beginPatch = "*** Begin Patch"
	endPatch   = "*** End Patch"
	addFile    = "*** Add File: "
	deleteFile = "*** Delete File: "
	updateFile = "*** Update File: "
	moveTo     = "*** Move to: "
	endOfFile  = "*** End of File"
	hunkHeader = "@@"
)

// Parse 解析补丁文本 → 文件操作序列。
func Parse(raw string) ([]FileOp, error) {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != beginPatch {
		return nil, fmt.Errorf("补丁必须以 %s 开头", beginPatch)
	}
	var ops []FileOp
	i := 1
	for ; i < len(lines); i++ {
		line := lines[i]
		switch {
		case strings.TrimSpace(line) == endPatch:
			return ops, nil
		case strings.HasPrefix(line, addFile):
			path := strings.TrimSpace(line[len(addFile):])
			if path == "" {
				return nil, fmt.Errorf("Add File 缺路径（第 %d 行）", i+1)
			}
			op, ni, err := parseAdd(lines, i, path)
			if err != nil {
				return nil, err
			}
			ops = append(ops, op)
			i = ni
		case strings.HasPrefix(line, deleteFile):
			path := strings.TrimSpace(line[len(deleteFile):])
			if path == "" {
				return nil, fmt.Errorf("Delete File 缺路径（第 %d 行）", i+1)
			}
			ops = append(ops, FileOp{Kind: KindDelete, Path: path})
		case strings.HasPrefix(line, updateFile):
			path := strings.TrimSpace(line[len(updateFile):])
			if path == "" {
				return nil, fmt.Errorf("Update File 缺路径（第 %d 行）", i+1)
			}
			op, ni, err := parseUpdate(lines, i, path)
			if err != nil {
				return nil, err
			}
			ops = append(ops, op)
			i = ni
		case strings.TrimSpace(line) == "":
			continue // 段间空行容错
		default:
			return nil, fmt.Errorf("无法识别的补丁行（第 %d 行）：%s", i+1, truncate(line, 60))
		}
	}
	return nil, fmt.Errorf("补丁缺少 %s 结尾", endPatch)
}

// parseAdd 解析 Add File 段（全为 + 行）。
func parseAdd(lines []string, i int, path string) (FileOp, int, error) {
	op := FileOp{Kind: KindAdd, Path: path}
	j := i + 1
	for ; j < len(lines); j++ {
		line := lines[j]
		if strings.HasPrefix(line, "+") {
			op.Contents = append(op.Contents, line[1:])
			continue
		}
		break // 首个非 + 行：段结束（可能是下一文件头或 End Patch）
	}
	if len(op.Contents) == 0 {
		return op, j - 1, fmt.Errorf("Add File %s 无内容（至少一个 + 行；空文件也要 + 空行）", path)
	}
	return op, j - 1, nil
}

// parseUpdate 解析 Update File 段（可选 Move to + @@ 块序列）。
func parseUpdate(lines []string, i int, path string) (FileOp, int, error) {
	op := FileOp{Kind: KindUpdate, Path: path}
	j := i + 1
	if j < len(lines) && strings.HasPrefix(lines[j], moveTo) {
		op.MoveTo = strings.TrimSpace(lines[j][len(moveTo):])
		if op.MoveTo == "" {
			return op, j, fmt.Errorf("Move to 缺路径")
		}
		j++
	}
	var cur *Chunk
	flush := func() {
		if cur != nil {
			op.Chunks = append(op.Chunks, *cur)
			cur = nil
		}
	}
	for ; j < len(lines); j++ {
		line := lines[j]
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == endPatch || strings.HasPrefix(line, addFile) ||
			strings.HasPrefix(line, deleteFile) || strings.HasPrefix(line, updateFile):
			flush()
			return op, j - 1, nil
		case trimmed == endOfFile:
			if cur != nil {
				cur.IsEOF = true
				flush()
			}
		case line == hunkHeader || strings.HasPrefix(line, hunkHeader+" "):
			// @@ 可堆叠：每个 @@ 都是一个定位锚块（块体为空，仅推进行游标）
			flush()
			ctx := ""
			if strings.HasPrefix(line, hunkHeader+" ") {
				ctx = line[len(hunkHeader)+1:]
			}
			cur = &Chunk{ChangeContext: ctx}
		case strings.HasPrefix(line, " "):
			if cur == nil {
				return op, j, fmt.Errorf("%s 的上下文行出现在 @@ 之前（第 %d 行）", path, j+1)
			}
			cur.OldLines = append(cur.OldLines, line[1:])
			cur.NewLines = append(cur.NewLines, line[1:])
		case strings.HasPrefix(line, "-"):
			if cur == nil {
				return op, j, fmt.Errorf("%s 的删除行出现在 @@ 之前（第 %d 行）", path, j+1)
			}
			cur.OldLines = append(cur.OldLines, line[1:])
		case strings.HasPrefix(line, "+"):
			if cur == nil {
				return op, j, fmt.Errorf("%s 的新增行出现在 @@ 之前（第 %d 行）", path, j+1)
			}
			cur.NewLines = append(cur.NewLines, line[1:])
		case trimmed == "":
			if cur != nil { // 块内空行 = 上下文空行（容错：省略前导空格）
				cur.OldLines = append(cur.OldLines, "")
				cur.NewLines = append(cur.NewLines, "")
			}
		default:
			return op, j, fmt.Errorf("%s 块内无法识别的行（第 %d 行，须以 空格/-/+ 开头）：%s", path, j+1, truncate(line, 60))
		}
	}
	flush()
	return op, j - 1, nil
}

// ---- 匹配（seek_sequence.rs 四档模糊） ----

// seekSequence 在 lines 中自 start 起找 pattern 首个匹配位置；-1 = 未找到。
// 四档从严到宽：精确 → 忽略行尾空白 → 忽略两侧空白 → Unicode 标点归一化。
// eof=true 时优先尝试文件尾（收尾块意图锚定末尾）。
func seekSequence(lines, pattern []string, start int, eof bool) int {
	if len(pattern) == 0 {
		return start
	}
	if len(pattern) > len(lines) {
		return -1
	}
	searchStart := start
	if eof {
		eofStart := len(lines) - len(pattern)
		if eofStart > searchStart {
			searchStart = eofStart
		}
	}
	last := len(lines) - len(pattern)
	// ① 精确
	for i := searchStart; i <= last; i++ {
		if equalExact(lines[i:i+len(pattern)], pattern) {
			return i
		}
	}
	// ② 行尾空白
	for i := searchStart; i <= last; i++ {
		if equalTrimRight(lines[i:i+len(pattern)], pattern) {
			return i
		}
	}
	// ③ 两侧空白
	for i := searchStart; i <= last; i++ {
		if equalTrimSpace(lines[i:i+len(pattern)], pattern) {
			return i
		}
	}
	// ④ Unicode 标点归一化（弯引号/长划线/特殊空格 → ASCII）
	for i := searchStart; i <= last; i++ {
		if equalNormalized(lines[i:i+len(pattern)], pattern) {
			return i
		}
	}
	return -1
}

func equalExact(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalTrimRight(a, b []string) bool {
	for i := range a {
		if strings.TrimRight(a[i], " \t\r") != strings.TrimRight(b[i], " \t\r") {
			return false
		}
	}
	return true
}

func equalTrimSpace(a, b []string) bool {
	for i := range a {
		if strings.TrimSpace(a[i]) != strings.TrimSpace(b[i]) {
			return false
		}
	}
	return true
}

func equalNormalized(a, b []string) bool {
	for i := range a {
		if normalizePunct(a[i]) != normalizePunct(b[i]) {
			return false
		}
	}
	return true
}

// normalizePunct 常见 Unicode 标点/空格 → ASCII 等价（git apply 式容错）。
func normalizePunct(s string) string {
	s = strings.TrimSpace(s)
	r := strings.Map(func(ch rune) rune {
		switch ch {
		case '‐', '‑', '‒', '–', '—', '―', '−':
			return '-'
		case '‘', '’', '‚', '‛':
			return '\''
		case '“', '”', '„', '‟':
			return '"'
		case ' ', ' ', ' ', ' ', ' ', ' ',
			' ', ' ', ' ', ' ', ' ', ' ', '　':
			return ' '
		}
		return ch
	}, s)
	return r
}

// ---- 应用（file_update.rs：替换计算 + 逆序套用） ----

// replacement (起始行, 旧长度, 新行段)。
type replacement struct {
	start int
	old   int
	new   []string
}

// computeReplacements 计算一组块的替换表（顺序游标 + EOF 空行重试）。
func computeReplacements(original []string, path string, chunks []Chunk) ([]replacement, error) {
	var reps []replacement
	lineIndex := 0
	for _, ch := range chunks {
		if ch.ChangeContext != "" {
			idx := seekSequence(original, []string{ch.ChangeContext}, lineIndex, false)
			if idx < 0 {
				return nil, fmt.Errorf("找不到定位锚 %q（%s）", truncate(ch.ChangeContext, 60), path)
			}
			lineIndex = idx + 1
		}
		if len(ch.OldLines) == 0 {
			// 纯新增块：追加文件尾（legacy 语义——中插须带上下文）
			reps = append(reps, replacement{len(original), 0, ch.NewLines})
			continue
		}
		pattern := ch.OldLines
		newSlice := ch.NewLines
		found := seekSequence(original, pattern, lineIndex, ch.IsEOF)
		if found < 0 && len(pattern) > 0 && pattern[len(pattern)-1] == "" {
			// 尾部空行 = 文件尾换行哨兵：去掉后重试（改动触及 EOF 的定位）
			pattern = pattern[:len(pattern)-1]
			if len(newSlice) > 0 && newSlice[len(newSlice)-1] == "" {
				newSlice = newSlice[:len(newSlice)-1]
			}
			found = seekSequence(original, pattern, lineIndex, ch.IsEOF)
		}
		if found < 0 {
			return nil, fmt.Errorf("在 %s 中找不到待改内容：\n%s", path, truncate(strings.Join(ch.OldLines, "\n"), 200))
		}
		reps = append(reps, replacement{found, len(pattern), newSlice})
		lineIndex = found + len(pattern)
	}
	sort.SliceStable(reps, func(a, b int) bool { return reps[a].start < reps[b].start })
	return reps, nil
}

// applyReplacements 逆序套用（后段先行，避免行号位移），返回成品内容。
// 行尾保留（text_file.rs 移植）：未改动行保留原行尾，插入行用首选行尾
// （原文首个行尾形态，无行尾默认 LF），末行无行尾时补齐（更新补尾换行
// 的历史语义）。
func applyReplacements(lines []string, reps []replacement, prefCRLF bool) string {
	type srcLine struct {
		text string
		crlf bool
	}
	src := make([]srcLine, len(lines))
	for i, l := range lines {
		src[i] = srcLine{text: l, crlf: prefCRLF}
	}
	nl := "\n"
	if prefCRLF {
		nl = "\r\n"
	}
	for i := len(reps) - 1; i >= 0; i-- {
		r := reps[i]
		if r.start > len(src) {
			continue // 防御（纯追加块 start=len 越界场景）
		}
		end := r.start + r.old
		if end > len(src) {
			end = len(src)
		}
		seg := make([]srcLine, 0, r.start+len(r.new)+len(src)-end)
		seg = append(seg, src[:r.start]...)
		for _, t := range r.new {
			seg = append(seg, srcLine{text: t, crlf: prefCRLF})
		}
		seg = append(seg, src[end:]...)
		src = seg
	}
	var b strings.Builder
	for _, l := range src {
		b.WriteString(l.text)
		if l.crlf {
			b.WriteString("\r\n")
		} else {
			b.WriteString(nl)
		}
	}
	return b.String()
}

// ---- 文件面（事务性：先全算后全写） ----

// FileResult 单文件结果。
type FileResult struct {
	Path    string `json:"path"`
	Action  string `json:"action"` // added | updated | deleted | renamed
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// Apply 应用补丁到 root 工作区（事务性：解析与计算全部成功才落盘）。
// 返回逐文件摘要。
func Apply(root string, ops []FileOp) ([]FileResult, error) {
	type staged struct {
		op      FileOp
		content string
	}
	var writes []staged
	var deletes []FileOp
	var results []FileResult

	for _, op := range ops {
		full, err := safeJoin(root, op.Path)
		if err != nil {
			return nil, err
		}
		switch op.Kind {
		case KindAdd:
			if _, err := os.Lstat(full); err == nil {
				return nil, fmt.Errorf("文件已存在（Add File 拒绝覆盖）：%s", op.Path)
			}
			writes = append(writes, staged{op, strings.Join(op.Contents, "\n") + "\n"})
			results = append(results, FileResult{Path: op.Path, Action: "added", Added: len(op.Contents)})
		case KindDelete:
			if _, err := os.Lstat(full); err != nil {
				return nil, fmt.Errorf("文件不存在（Delete File）：%s", op.Path)
			}
			deletes = append(deletes, op)
			results = append(results, FileResult{Path: op.Path, Action: "deleted"})
		case KindUpdate:
			b, err := os.ReadFile(full)
			if err != nil {
				return nil, fmt.Errorf("文件不存在（Update File）：%s", op.Path)
			}
			original := splitLines(string(b))
			reps, err := computeReplacements(original, op.Path, op.Chunks)
			if err != nil {
				return nil, err
			}
			content := applyReplacements(original, reps, preferredCRLF(string(b)))
			path := op.Path
			action := "updated"
			if op.MoveTo != "" {
				if _, err := safeJoin(root, op.MoveTo); err != nil {
					return nil, err
				}
				path = op.MoveTo
				action = "renamed"
				deletes = append(deletes, FileOp{Kind: KindDelete, Path: op.Path})
			}
			writes = append(writes, staged{FileOp{Kind: KindUpdate, Path: path}, content})
			added, removed := diffCount(original, splitLines(content))
			results = append(results, FileResult{Path: path, Action: action, Added: added, Removed: removed})
		}
	}
	// 落盘阶段（先写后删：改名场景写新删旧）
	for _, w := range writes {
		full, err := safeJoin(root, w.op.Path)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, fmt.Errorf("建目录失败（%s）：%w", w.op.Path, err)
		}
		if err := os.WriteFile(full, []byte(w.content), 0o644); err != nil {
			return nil, fmt.Errorf("写入失败（%s）：%w", w.op.Path, err)
		}
	}
	for _, d := range deletes {
		full, err := safeJoin(root, d.Path)
		if err != nil {
			return nil, err
		}
		if err := os.Remove(full); err != nil {
			return nil, fmt.Errorf("删除失败（%s）：%w", d.Path, err)
		}
	}
	return results, nil
}

// splitLines 按行拆（丢尾换行哨兵；CRLF 归一 LF）。
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// preferredCRLF 首个行尾是否 CRLF（无行尾默认 LF——插入行与补齐行尾用）。
func preferredCRLF(s string) bool {
	if i := strings.IndexByte(s, '\n'); i > 0 && s[i-1] == '\r' {
		return true
	}
	return false
}

// diffCount 粗粒度增删行统计（审批卡展示用）。
func diffCount(old, new []string) (added, removed int) {
	oldSet := map[string]int{}
	for _, l := range old {
		oldSet[l]++
	}
	newSet := map[string]int{}
	for _, l := range new {
		newSet[l]++
	}
	for l, n := range newSet {
		if o := oldSet[l]; n > o {
			added += n - o
		}
	}
	for l, n := range oldSet {
		if nw := newSet[l]; n > nw {
			removed += n - nw
		}
	}
	return added, removed
}

// safeJoin 工作区内路径（相对路径 only；穿越拒绝）。
func safeJoin(root, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("路径必须为工作区内相对路径：%s", p)
	}
	if strings.Contains(p, "..") {
		return "", fmt.Errorf("路径不允许 ..：%s", p)
	}
	return filepath.Join(root, filepath.FromSlash(p)), nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ---- 工具面 ----

type patchIn struct {
	Patch string `json:"patch"`
}

// NewTools 构造 apply_patch（写面：进审批名单，整补丁一批）。
func NewTools(cfg Config) ([]contract.Tool, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("applypatch 需要工作区根（拒绝全盘默认）")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	t, err := tools.InferTool("apply_patch",
		"以 codex apply_patch 格式批量修改工作区文件（一次调用多文件 增/改/删/改名，事务性——任一失败全部不生效）。格式：*** Begin Patch 开头、*** End Patch 结尾；*** Add File: <相对路径> + 全部 + 行；*** Delete File: <路径>；*** Update File: <路径>（可跟 *** Move to: <新路径>）+ @@ 可选定位锚 + 上下文行(前缀空格)/删除行(-)/新增行(+)。改动处上下各带 3 行上下文；不够唯一定位时用 @@ 锚（可堆叠）。文件不存在/已存在/找不到上下文会整体失败并说明原因。",
		func(_ context.Context, in patchIn) (map[string]any, error) {
			ops, err := Parse(in.Patch)
			if err != nil {
				return map[string]any{"ok": false, "error": "补丁解析失败：" + err.Error()}, nil
			}
			results, err := Apply(root, ops)
			if err != nil {
				return map[string]any{"ok": false, "error": "补丁未应用（整体回退）：" + err.Error()}, nil
			}
			add, del := 0, 0
			allAdded := len(results) > 0 // 全部为 Add File 才算「新建」——混入改/删/改名即「修改」（UI 审查 B-5 写动词）
			for _, r := range results {
				add += r.Added
				del += r.Removed
				if r.Action != "added" {
					allAdded = false
				}
			}
			verb := "edit"
			if allAdded {
				verb = "create"
			}
			return map[string]any{"ok": true, "files": results, "count": len(results),
				"counts": fmt.Sprintf("+%d -%d", add, del), "verb": verb}, nil
		})
	if err != nil {
		return nil, err
	}
	t = tools.DiffToolOf(t, applyCardDiff) // 审批卡 diff 载荷（hitl 组卡时探测 ApprovalDiff）
	return []contract.Tool{tools.WithBehavior(t, contract.BehaviorWrite)}, nil
}

// applyCardDiff 审批卡 diff 载荷：透传补丁原文（补丁本身即逐行 diff）。
func applyCardDiff(args string) string {
	var in struct {
		Patch string `json:"patch"`
	}
	if json.Unmarshal([]byte(args), &in) != nil {
		return ""
	}
	return truncateRunes(in.Patch, 12000) // 卡面防爆；超长走工具结果看全文
}

// truncateRunes 按 rune 截断（超长附截断标记）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…（截断）"
}
