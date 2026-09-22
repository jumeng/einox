// edit_file（str_replace 精确替换）：zcode Edit 工具语义移植（2026-09-22
// 对比裁决吸收——packages/core/src/tool/handlers/edit.ts + edit-matchers.ts），
// 匹配降级链融合 einox applypatch 的 Unicode 标点归一与 einox read_file 的
// 行号前缀形态（%6d→）。防盲改门（readFileState）：必须先 read_file 读过、
// 读到的视野完整（非 partial）、且读取后文件未被外部修改（mtime 毫秒 + size
// 双指纹）——三关全过才放行编辑。
package fsutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/jumeng/einox/internal/strutil"
	"github.com/jumeng/einox/tools"
)

// ReadStamp 单文件读取指纹（unchanged 短路与防盲改门共用）。
// MtimeMS/Size 双指纹对位 zcode 整数毫秒防亚毫秒误报；Partial = 上次读取
// 视野不完整（默认窗口截断或行宽截断）；Offset/Limit = 上次实际窗口
// （unchanged 短路要求同参重读）。
type ReadStamp struct {
	MtimeMS   int64
	Size      int64
	Partial   bool
	Offset    int
	Limit     int
	LineWidth int
}

// ReadState 读取状态锚（键 = resolve 后的绝对路径）。跨轮保持由挂接方注入
// （engine 按会话持有）；nil 时 NewTools 内建轮内实例——单轮内 read→edit
// 门完整，跨轮由挂接方决定。
type ReadState interface {
	Record(path string, st ReadStamp)
	Stamp(path string) (ReadStamp, bool)
}

// ReadStateStore ReadState 默认实现（并发安全）。无上限对齐 zcode（内存
// Map，条目为定长小结构，量级 = 会话读过的文件数）。
type ReadStateStore struct {
	mu sync.Mutex
	m  map[string]ReadStamp
}

// NewReadState 构造读取状态锚。engine 侧跨轮注入同一实例；独立使用 fsutil
// 的应用默认轮内实例（NewTools 内建）。
func NewReadState() *ReadStateStore { return &ReadStateStore{m: map[string]ReadStamp{}} }

// Record 记录读取指纹（后写覆盖）。
func (s *ReadStateStore) Record(path string, st ReadStamp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[path] = st
}

// Stamp 取读取指纹（无记录 ok=false）。
func (s *ReadStateStore) Stamp(path string) (ReadStamp, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[path]
	return st, ok
}

// maxEditFileBytes edit_file 目标文件上限（对齐 read 面惯例 256KB——超限
// 文件用 apply_patch 分段或 run_command 处理）。
const maxEditFileBytes = 256 << 10

type editIn struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (h *helper) editFile(_ context.Context, in editIn) (map[string]any, error) {
	for _, d := range h.protect {
		if tools.PathBlocked(in.FilePath, []string{d}) {
			return fail("路径位于写保护区 " + d + " 内，拒绝编辑：" + in.FilePath)
		}
	}
	full, err := h.resolve(in.FilePath)
	if err != nil {
		return fail(err.Error())
	}
	if in.OldString == in.NewString {
		return fail("old_string 与 new_string 相同——无变更不需要编辑")
	}
	st, statErr := os.Stat(full)
	if statErr != nil {
		if in.OldString != "" {
			return fail("文件不存在：" + in.FilePath + suggestNearby(h.root, full))
		}
		// old 留空且文件不存在 = 创建新文件
		if err := writeAtomic(full, in.NewString); err != nil {
			return fail("创建失败：" + err.Error())
		}
		if nst, err := os.Stat(full); err == nil {
			h.state.Record(full, ReadStamp{MtimeMS: nst.ModTime().UnixMilli(), Size: nst.Size()})
		}
		return map[string]any{"ok": true, "path": in.FilePath, "created": true, "chars": len([]rune(in.NewString))}, nil
	}
	if st.IsDir() {
		return fail("是目录不是文件：" + in.FilePath)
	}
	if in.OldString == "" {
		return fail("文件已存在：" + in.FilePath + "——old_string 留空仅用于创建新文件")
	}
	// 防盲改门（zcode readFileState 三关：读过 / 视野完整 / 未被外部修改）
	if stp, ok := h.state.Stamp(full); !ok {
		return fail("文件尚未读取——先 read_file 读取 " + in.FilePath + " 再编辑（防盲改）")
	} else if stp.Partial {
		return fail("上次读取被截断（默认窗口或行宽截断）——用 offset/limit 分段读全后再编辑")
	} else if stp.MtimeMS != st.ModTime().UnixMilli() || stp.Size != st.Size() {
		return fail("文件在最近一次读取后被修改——重新 read_file 后再编辑")
	}
	if st.Size() > maxEditFileBytes {
		return fail(fmt.Sprintf("文件超 edit_file 上限（%d KB）——用 apply_patch 或 run_command 处理", maxEditFileBytes>>10))
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		return fail("读取失败：" + err.Error())
	}
	// CRLF 归一（模型给的 old/new 是 LF 形态；检测主导行尾风格，写回还原）
	crlf := strings.Contains(string(raw), "\r\n")
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")

	flines := splitLines(text)
	oldLines := splitLines(in.OldString)
	newLines := splitLines(in.NewString)

	wins, matcherName := findEditMatches(flines, oldLines)
	if len(wins) == 0 {
		return fail("old_string 在文件中未命中（已尝试全部匹配降级档）——请先 read_file 核对内容：注意缩进、空白与行号前缀，确保 old_string 与文件精确一致")
	}
	if len(wins) > 1 && !in.ReplaceAll {
		var at []string
		for _, w := range wins {
			at = append(at, fmt.Sprintf("%d", w.start+1))
		}
		return fail(fmt.Sprintf("old_string 命中 %d 处（行 %s）——扩大 old_string 上下文消歧，或确认全部替换时传 replace_all=true", len(wins), strings.Join(at, "、")))
	}
	// 有序窗口正向重建（窗口内整段替换为 new_string 原文——降级档匹配、原文替换）
	res := make([]string, 0, len(flines)+len(newLines)*len(wins))
	i := 0
	for _, w := range wins {
		res = append(res, flines[i:w.start]...)
		res = append(res, newLines...)
		i = w.end + 1
	}
	res = append(res, flines[i:]...)
	out := strings.Join(res, "\n")
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if err := writeAtomic(full, out); err != nil {
		return fail("写回失败：" + err.Error())
	}
	if nst, err := os.Stat(full); err == nil {
		h.state.Record(full, ReadStamp{MtimeMS: nst.ModTime().UnixMilli(), Size: nst.Size()})
	}
	return map[string]any{
		"ok": true, "path": in.FilePath,
		"replaced": len(wins), "match": matcherName, "lines": len(res),
	}, nil
}

// win 命中窗口（0 基行号，含端点）。
type win struct{ start, end int }

// matcher 降级档：norm 行变换（文件行与 old 行同变换后比对）。
type matcher struct {
	name string
	norm func(string) string
}

// editMatchers 匹配降级链（zcode 8 档融合 applypatch 标点归一 + einox 行号
// 前缀形态；从精确到宽松逐档，命中即停——宽松档只在精确档空手时兜底）。
var editMatchers = []matcher{
	{"exact", func(s string) string { return s }},
	{"尾空白忽略", func(s string) string { return strings.TrimRight(s, " \t") }},
	{"两侧空白忽略", strings.TrimSpace},
	{"引号归一", normQuotes},
	{"行号前缀剥离", stripLineNoPrefix},
	{"转义归一", unescapeLiteral},
	{"标点归一", normPunct},
}

func normQuotes(s string) string {
	r := strings.NewReplacer("“", `"`, "”", `"`, "‘", "'", "’", "'")
	return r.Replace(s)
}

// stripLineNoPrefix 剥模型夹带的行号前缀：einox read_file 输出形态「%6d→」
// 与 cat -n 形态「%6d:」/「%d\t」。
var lineNoRe = regexp.MustCompile(`^\s*\d+(?:→|:|\t)\s?`)

func stripLineNoPrefix(s string) string { return lineNoRe.ReplaceAllString(s, "") }

// unescapeLiteral 反转义模型字面量形态（\n \t \" \\ → 真字符）。
func unescapeLiteral(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case 't':
				b.WriteByte('\t')
				i++
				continue
			case '"':
				b.WriteByte('"')
				i++
				continue
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// normPunct Unicode 标点/空白变体归一（引号 + 长划线 + 特殊空格——applypatch
// seekSequence 第 4 档同款词汇）。
func normPunct(s string) string {
	s = normQuotes(s)
	r := strings.NewReplacer(
		"–", "-", "—", "-", "−", "-",
		" ", " ", "　", " ",
	)
	s = r.Replace(s)
	// 智能引号外的 Unicode 空白类（NBSP 等）
	return strings.Map(func(rr rune) rune {
		if unicode.IsSpace(rr) {
			return ' '
		}
		return rr
	}, s)
}

// findEditMatches 匹配链主入口：逐档滑窗找全部命中窗口；全档空手再试
// block_anchor（首尾行锚 + 中间行 Levenshtein 相似度容差）。
func findEditMatches(flines, oldLines []string) ([]win, string) {
	for _, m := range editMatchers {
		wins := matchWindow(flines, oldLines, m.norm)
		if len(wins) > 0 {
			return wins, m.name
		}
	}
	wins := matchBlockAnchor(flines, oldLines)
	if len(wins) > 0 {
		return wins, "首尾锚定"
	}
	return nil, ""
}

// matchWindow 滑窗匹配：norm 变换后逐行相等。
func matchWindow(flines, oldLines []string, norm func(string) string) []win {
	if len(oldLines) == 0 || len(oldLines) > len(flines) {
		return nil
	}
	normOld := make([]string, len(oldLines))
	for i, l := range oldLines {
		normOld[i] = norm(l)
	}
	var wins []win
	for i := 0; i+len(oldLines) <= len(flines); i++ {
		hit := true
		for j, l := range flines[i : i+len(oldLines)] {
			if norm(l) != normOld[j] {
				hit = false
				break
			}
		}
		if hit {
			wins = append(wins, win{start: i, end: i + len(oldLines) - 1})
		}
	}
	return wins
}

// blockAnchorMinSim 中间行相似度阈值（zcode BLOCK_ANCHOR 同思路：首尾行
// 精确锚定、中间行容差——模型复述长块时中段常有细微出入）。
const blockAnchorMinSim = 0.8

// matchBlockAnchor 首尾锚定匹配：锚行取首/末非空白行（old_string 尾换行产生
// 的空尾行不作锚、留在窗口内），中间行归一后 Levenshtein 相似度 ≥ 0.8 判中。
// 行长超 512 直接要求相等（防 O(n²) 大数）。
func matchBlockAnchor(flines, oldLines []string) []win {
	n := len(oldLines)
	// 锚索引：跳过首尾空白行（边界噪声）
	fi := 0
	for fi < n && strings.TrimSpace(oldLines[fi]) == "" {
		fi++
	}
	li := n - 1
	for li > fi && strings.TrimSpace(oldLines[li]) == "" {
		li--
	}
	if li-fi < 2 { // 有效锚跨度不足 3 行——短块应已被前档命中
		return nil
	}
	trim := strings.TrimSpace
	first, last := trim(oldLines[fi]), trim(oldLines[li])
	if first == "" || last == "" {
		return nil
	}
	var wins []win
	for i := 0; i+n <= len(flines); i++ {
		if trim(flines[i+fi]) != first || trim(flines[i+li]) != last {
			continue
		}
		ok := true
		for j := fi + 1; j < li; j++ {
			a, b := trim(flines[i+j]), trim(oldLines[j])
			if a == b {
				continue
			}
			if len(a) > 512 || len(b) > 512 || strutil.Similarity(a, b) < blockAnchorMinSim {
				ok = false
				break
			}
		}
		if ok {
			wins = append(wins, win{start: i, end: i + n - 1})
		}
	}
	return wins
}

// splitLines 按 LF 切行（尾换行产出末尾空行——join 时原样还原，保证未参与
// 替换的文件尾换行不丢；"" → 单空行，与 Join 往返一致）。
func splitLines(s string) []string { return strings.Split(s, "\n") }

// writeAtomic 原子写（tmp + rename；Windows 侧 Go 的 os.Rename 走
// MoveFileEx REPLACE_EXISTING，覆盖语义成立）。
func writeAtomic(full, content string) error {
	dir := filepath.Dir(full)
	tmp, err := os.CreateTemp(dir, ".einox-edit-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, full)
}

// suggestNearby 文件不存在时的相近名建议（同目录 stem 相同优先，其次
// Levenshtein ≤ 3；zcode edit.ts Did-you-mean 同款）。
func suggestNearby(root, full string) string {
	dir := filepath.Dir(full)
	base := filepath.Base(full)
	des, err := os.ReadDir(dir)
	if err != nil || len(des) == 0 {
		return ""
	}
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	var same []string
	for _, d := range des {
		if d.IsDir() {
			continue
		}
		n := d.Name()
		if n == base {
			continue
		}
		ns := strings.TrimSuffix(n, filepath.Ext(n))
		if ns == stem || strutil.Levenshtein([]rune(n), []rune(base)) <= 3 {
			same = append(same, n)
			if len(same) >= 5 {
				break
			}
		}
	}
	if len(same) == 0 {
		return ""
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		rel = dir
	}
	return "——是否想找：" + filepath.ToSlash(filepath.Join(rel, same[0])) +
		fmt.Sprintf("（同目录相近名 %d 个：%s）", len(same), strings.Join(same, "、"))
}
