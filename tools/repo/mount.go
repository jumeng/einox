package repo

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// gitOut exec git（参数走 argv 不经 shell；stderr 并入输出——错误信息对模型可读）。
func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if err != nil {
		return out, fmt.Errorf("git %s 失败: %w: %s", args[0], err, truncateRunes(out, 300))
	}
	return out, nil
}

// truncateRunes 按 rune 截断（超长附截断标记）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…（截断）"
}

// mountDir 挂载点绝对路径（<Root>/repos/<短名>）。
func (c Config) mountDir(name string) string {
	return filepath.Join(c.Root, "repos", name)
}

// mountExists 挂载点是否已存在（幂等判定锚点）。
func (c Config) mountExists(name string) bool {
	_, err := os.Stat(c.mountDir(name))
	return err == nil
}

// mergeBase 取基线合并点（基线 ref 由 Resolver 提供，origin/ 前缀保留原样
// ——rev-parse/merge-base 直接吃）。返回 (mergeBase, baseRef, err)。
func mergeBase(mount, base string) (string, string, error) {
	if out, err := gitOut(mount, "rev-parse", "--verify", "--quiet", base); err != nil || strings.TrimSpace(out) == "" {
		return "", "", fmt.Errorf("基线 ref 不存在：%s（可能未采集，先确认仓已 fetch）", base)
	}
	out, err := gitOut(mount, "merge-base", "HEAD", base)
	if err != nil {
		return "", "", fmt.Errorf("merge-base 失败：%w", err)
	}
	return strings.TrimSpace(out), base, nil
}

// diffOf 基线 → 工作区 diff（repo_diff 核心）：merge-base 前置，path 非空时
// 限定路径。含未提交改动，untracked 除外（git diff 语义）。
func diffOf(mount, base, path string) (string, error) {
	mb, _, err := mergeBase(mount, base)
	if err != nil {
		return "", err
	}
	args := []string{"diff", mb}
	if path != "" {
		args = append(args, "--", path)
	}
	return gitOut(mount, args...)
}

// untrackedList 未跟踪新文件清单段（ls-files --others --exclude-standard，
// 只读不动 index——禁用 add -N：会改 index 且拒批后残留状态）。repo_commit
// 走 add -A 会纳入这些文件而 diff 载荷（git diff 语义）不含其内容，卡面须
// 单列补全。空清单返回 ""；最多 50 条，超出附「还有 N 个未列出」；单路径超
// 200 rune 截断。
func untrackedList(mount string) string {
	out, err := gitOut(mount, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return ""
	}
	var files []string
	for _, ln := range strings.Split(out, "\n") { // 不整体 TrimSpace：路径首列无状态位，逐行判空即可
		if ln != "" {
			files = append(files, truncateRunes(ln, 200))
		}
	}
	if len(files) == 0 {
		return ""
	}
	entries := files
	if len(entries) > 50 {
		entries = entries[:50]
	}
	var b strings.Builder
	b.WriteString("\n\n未跟踪新文件（将随本次提交一并纳入，上方 diff 未含其内容）：\n- ")
	b.WriteString(strings.Join(entries, "\n- "))
	if len(files) > 50 {
		fmt.Fprintf(&b, "\n…（还有 %d 个未列出）", len(files)-50)
	}
	return b.String()
}

// aheadBehind 相对基线领先/落后提交数（rev-list --left-right --count）。
func aheadBehind(mount, base string) (int, int, error) {
	out, err := gitOut(mount, "rev-list", "--left-right", "--count", "HEAD..."+base)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("rev-list 输出异常：%s", truncateRunes(out, 100))
	}
	ahead, err1 := strconv.Atoi(f[0])
	behind, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("rev-list 计数非数字：%s", truncateRunes(out, 100))
	}
	return ahead, behind, nil
}
