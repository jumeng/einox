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
