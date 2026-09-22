// Package strutil 仓内横切字符串小件单点。2026-09-06 收口（审查 P3-1）：
// 截断家族曾八处各写、边界约定不一（"…" 后缀五份逐字、定制后缀两份、TUI
// 总长恰 n 一份反向）。内部包不进契约面；带定制展示后缀的变体（applypatch
// 的「…（截断）」、webfetch 的「\n…（已截断）」）属各工具的输出文案选择，
// 留在本地。
package strutil

// Truncate 按 rune 截断：超长取前 n 个 rune 加省略号（总长 n+1）。
func Truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// TruncateTotal 总长恰 n 的截断（n-1 个 rune + 省略号）——固定宽度显示面
// （TUI 列宽）语义，与 Truncate 的 n+1 边界刻意不同。
func TruncateTotal(s string, n int) string {
	r := []rune(s)
	if len(r) <= n || n <= 0 {
		return s
	}
	return string(r[:max(0, n-1)]) + "…"
}

// Levenshtein 标准编辑距离（rune 级；行级短串量级 O(n·m) 可接受——匹配
// 降级链的容差判定用，fsutil edit_file 与 applypatch 相似度提示同源单点）。
func Levenshtein(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// Similarity 归一相似度（1 - dist/max(len)，双空 = 1）。
func Similarity(x, y string) float64 {
	a, b := []rune(x), []rune(y)
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	maxL := max(len(a), len(b))
	return 1 - float64(Levenshtein(a, b))/float64(maxL)
}
