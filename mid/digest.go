package mid

// 参数与结果摘要（审批卡/事件流的可读化，自产品 internal/agent interrupt.go
// 与 tooldigest.go 迁入——2026-08-24 定案：卡片与事件不倾倒原始 JSON 头）。

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// truncateRunes 截断加省略号。
func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ArgsDigest 审批卡参数摘要：JSON 展开为「k: v, …」人类可读——数组取首项
// title/name 与数量，对象省略。非 JSON 原样截断 120 runes。
func ArgsDigest(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil || len(m) == 0 {
		return truncateRunes(args, 120)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+ValueDigest(m[k]))
	}
	return truncateRunes(strings.Join(parts, ", "), 120)
}

// ValueDigest 值摘要：字符串/布尔/数字原样（截 40）；数组取首项 title/name
// 与数量；嵌套对象省略。
func ValueDigest(v any) string {
	switch x := v.(type) {
	case string:
		return truncateRunes(x, 40)
	case bool:
		return fmt.Sprintf("%v", x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []any:
		if n := len(x); n > 0 {
			if om, ok := x[0].(map[string]any); ok {
				for _, key := range []string{"title", "name", "id"} {
					if s, ok := om[key].(string); ok && s != "" {
						return fmt.Sprintf("「%s」等 %d 项", truncateRunes(s, 24), n)
					}
				}
			}
			return fmt.Sprintf("%d 项", n)
		}
		return "0 项"
	case map[string]any:
		return "{…}"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// argsPrimaryKeys 参数主识别字段优先序：单行摘要取一个最能定位调用的字段
// （read_document 的 path、list_issues 的 query、run_command 的 cmd…）。
var argsPrimaryKeys = []string{
	"path", "file", "dir", "query", "cmd", "command", "url",
	"name", "title", "question", "id", "ids", "text", "script", "action",
}

// ToolArgsDigest tool_call 参数摘要：单字段值即摘要（k 冗余）；多字段取主
// 识别字段；无命中回退 k: v 全展开（ArgsDigest——审批卡同源可读化）；
// 空/{} → ""（零参工具不显参数）。
func ToolArgsDigest(args string) string {
	args = strings.TrimSpace(args)
	if args == "" || args == "{}" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil || len(m) == 0 {
		return truncateRunes(args, 60) // 非 JSON（罕见）原样截断
	}
	if len(m) == 1 {
		for _, v := range m {
			return truncateRunes(ValueDigest(v), 60)
		}
	}
	for _, k := range argsPrimaryKeys {
		if v, ok := m[k]; ok {
			if s := truncateRunes(ValueDigest(v), 60); s != "" && s != "{…}" {
				return s
			}
		}
	}
	return truncateRunes(ArgsDigest(args), 60)
}

// resultLabels 结果数组字段 → 中文量词（计数语义「N 个需求」形态）。
var resultLabels = []struct{ key, label string }{
	{"issues", "个需求"}, {"documents", "篇文档"}, {"sessions", "个会话"},
	{"entries", "项"}, {"files", "个文件"}, {"results", "条"}, {"matches", "处"},
}

// ToolResultDigest tool_result 语义摘要 → (ok, digest, preview)：
//   - 失败信封 {"ok":false,"error"} 或 run_command 的非零 exit_code → ok=false，
//     摘要 = 错误消息（前端红显）
//   - 成功 → 计数语义（「首项标题」等 N 个需求）或行数语义（N 行）
//   - 提取不出 → 摘要 ""（只留状态点，零噪音）
//   - preview = 原始内容头 400 字（展开查看用；全文不进事件流——大结果会撑爆）
func ToolResultDigest(content string) (ok bool, digest, preview string) {
	content = strings.TrimSpace(content)
	preview = truncateRunes(content, 400)

	var m map[string]any
	if json.Unmarshal([]byte(content), &m) != nil {
		return true, lineCountLabel(content), preview // 纯文本：行数语义
	}
	if b, has := m["ok"]; has {
		if be, _ := b.(bool); be == false {
			msg := "失败"
			if e, has := m["error"]; has && e != nil {
				msg = fmt.Sprint(e)
			}
			return false, truncateRunes(msg, 120), preview
		}
	}
	// run_command 约定：ok=true + 非零 exit_code = 命令失败（信封不翻 false）
	if ec, has := m["exit_code"]; has {
		if n, _ := ec.(float64); n != 0 {
			d := fmt.Sprintf("exit %.0f", n)
			if s, ok := m["note"].(string); ok && s != "" {
				d += "：" + s
			}
			return false, truncateRunes(d, 120), preview
		}
	}
	var parts []string
	for _, l := range resultLabels {
		if arr, has := m[l.key]; has {
			if items, ok := arr.([]any); ok {
				parts = append(parts, countLabel(items, l.label))
			}
		}
	}
	if len(parts) > 0 {
		return true, strings.Join(parts, " · "), preview
	}
	// 文本载荷（read_document 族）：行数语义
	for _, k := range []string{"text", "content", "output", "description"} {
		if s, ok := m[k].(string); ok && s != "" {
			if d := lineCountLabel(s); d != "" {
				return true, d, preview
			}
		}
	}
	// 兜底计数（list_dir 的 count、list_issues 的 total）
	for _, k := range []string{"count", "total"} {
		if n, ok := m[k].(float64); ok {
			return true, fmt.Sprintf("%.0f 条", n), preview
		}
	}
	return true, "", preview // 无语义可提取：零摘要（状态点即结果）
}

// countLabel 数组计数标签：首项带 title/name/id/path 则「标识」前缀（单条即
// 摘要，多条「标识」等 N 量词），否则纯计数。
func countLabel(items []any, label string) string {
	n := len(items)
	if n == 0 {
		return "0 " + label
	}
	if om, ok := items[0].(map[string]any); ok {
		for _, key := range []string{"title", "name", "id", "path"} {
			if s, ok := om[key].(string); ok && strings.TrimSpace(s) != "" {
				s = truncateRunes(s, 24)
				if n == 1 {
					return "「" + s + "」"
				}
				return fmt.Sprintf("「%s」等 %d %s", s, n, label)
			}
		}
	}
	return fmt.Sprintf("%d %s", n, label)
}

// lineCountLabel 多行文本行数标签（单行/空 → ""，不留噪音）。
func lineCountLabel(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" || !strings.Contains(s, "\n") {
		return ""
	}
	return fmt.Sprintf("%d 行", strings.Count(s, "\n")+1)
}
