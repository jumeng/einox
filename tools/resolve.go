package tools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ResolveUnder 工作区路径圈禁（防穿越）单点实现：fsutil（读删检索）、office
// （文档族写）、applypatch（补丁写）、einoext/extwire（本地算子）四家共用——
// 曾四份两算法（Join+Rel 三份逐字拷贝 + applypatch 子串拒绝误伤 a..b.txt 致
// 同一文件可读不可改，审查 P1-5）。语义：空白路径与 "." 归根；词法圈禁
// （Join 清洗后逃出 root 显式拒绝）+ 符号链接圈禁（目标现存祖先经
// EvalSymlinks 解析后仍在 root 内才放行——区内指向区外的 symlink 即拒，
// 写面沿链接写出工作区的逃逸面闭合）。fail-closed，未列明的形态不侥幸
// 放行。已知残留窗口：检查通过后到实际打开前并发创建的 symlink 不在此拦
// （并发对抗需 openat/O_NOFOLLOW 级改造，超出库基座形态——沙箱是第二层）。
func ResolveUnder(root, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." {
		return root, nil
	}
	a := filepath.Join(root, filepath.FromSlash(p))
	if !underRoot(root, a) {
		return "", fmt.Errorf("路径越界（仅限工作区 %s 内）：%s", root, p)
	}
	if !symlinkContained(root, a) {
		return "", fmt.Errorf("路径经符号链接越出工作区（%s 内含指向区外的链接）：%s", root, p)
	}
	return a, nil
}

// underRoot 词法圈禁：a 清洗后逃出 root（".." 前缀）即拒。
func underRoot(root, a string) bool {
	rel, err := filepath.Rel(root, a)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// symlinkContained 符号链接圈禁：root 自身先解析（根可能含链接组件——macOS
// /tmp → /private/tmp，不先解析则真实路径 Rel 恒败全拒），再解析目标的最近
// 现存祖先（新建文件的全路径不存在 = 无链接可解析，词法圈禁已足），真实
// 路径仍在 root 内才放行。
func symlinkContained(root, a string) bool {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return true // 根不存在：区内无任何东西（含链接），词法判定已足
	}
	probe := a
	for {
		if rp, err := filepath.EvalSymlinks(probe); err == nil {
			return underRoot(realRoot, rp)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return true // 一路上溯到文件系统根均不存在：纯新建路径
		}
		probe = parent
	}
}

// Fail 工具失败信封（{"ok":false,"error":…}——mid.ToolResultDigest 判红的
// 软契约形态）。七个工具包各自同名的本地 fail() 与手拼 JSON 的两处
// （mid 硬上限信封、engine 悬空调回执）曾各写（审查 P2-11——手拼一旦文案
// 带引号即产非法 JSON），此处单点。
func Fail(msg string) map[string]any {
	return map[string]any{"ok": false, "error": msg}
}
