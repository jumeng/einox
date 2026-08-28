package mid

import (
	"strings"
	"testing"
)

// TestToolArgsDigest 参数摘要：单字段值即摘要 / 主识别字段优先 / 回退 k: v
// 全展开 / 空与残缺零摘要。
func TestToolArgsDigest(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"空", "", ""},
		{"空对象", "{}", ""},
		{"单字段path", `{"path":"weekly/summary/W34产品周报.md"}`, "weekly/summary/W34产品周报.md"},
		{"单字段cmd", `{"cmd":"ls -la"}`, "ls -la"},
		{"主识别query", `{"query":"进行中的P0","states":["进行中"]}`, "进行中的P0"},
		{"主识别url", `{"url":"https://a.com/x","method":"POST"}`, "https://a.com/x"},
		{"数组识别ids", `{"ids":["i1","i2","i3"],"fields":["title"]}`, "3 项"},
		{"回退k:v", `{"filters":{"state":"进行中"},"limit":10}`, "filters: {…}, limit: 10"},
		{"非JSON截断", `{broken`, "{broken"},
	}
	for _, c := range cases {
		if got := ToolArgsDigest(c.args); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestToolResultDigest 结果语义摘要：失败信封（ok:false / exit_code 非 0）→
// 红显错误；计数（「首项标题」等 N 个需求）；行数（read_document 族）；提取
// 不出零摘要——原始头随 preview 供展开，全文不进事件流。
func TestToolResultDigest(t *testing.T) {
	// 失败信封
	ok, digest, _ := ToolResultDigest(`{"ok":false,"error":"query 不能为空"}`)
	if ok || digest != "query 不能为空" {
		t.Fatalf("失败信封：ok=%v digest=%q", ok, digest)
	}
	// run_command 约定：ok=true + exit_code 非 0 = 命令失败
	ok, digest, _ = ToolResultDigest(`{"ok":true,"command":"ls","exit_code":2,"output":""}`)
	if ok || digest != "exit 2" {
		t.Fatalf("命令失败应红显：ok=%v digest=%q", ok, digest)
	}
	// 计数语义：单条带标题 → 「标题」
	ok, digest, _ = ToolResultDigest(`{"total":1,"truncated":false,"issues":[{"id":"i1","title":"P0 紧急缺陷"}]}`)
	if !ok || digest != "「P0 紧急缺陷」" {
		t.Fatalf("单条计数：ok=%v digest=%q", ok, digest)
	}
	// 多条：「首项」等 N 个需求
	ok, digest, _ = ToolResultDigest(`{"total":2,"issues":[{"title":"A"},{"title":"B"}]}`)
	if !ok || digest != "「A」等 2 个需求" {
		t.Fatalf("多条计数：ok=%v digest=%q", ok, digest)
	}
	// search_all 多域：域名间 · 连接；documents 首项无 title 回退 path 标识
	ok, digest, _ = ToolResultDigest(`{"ok":true,"query":"q","issues":[{"title":"X"}],"documents":[{"path":"docs/a.md","snippet":"s"}]}`)
	if !ok || digest != "「X」 · 「docs/a.md」" {
		t.Fatalf("多域计数：ok=%v digest=%q", ok, digest)
	}
	// 行数语义（read_document：path + text，无 ok 字段）
	text := "# 标题\n正文一\n正文二"
	ok, digest, _ = ToolResultDigest(`{"path":"docs/a.md","text":"` + text + `","truncated":false}`)
	if !ok || digest != "3 行" {
		t.Fatalf("行数语义：ok=%v digest=%q", ok, digest)
	}
	// 兜底计数（list_dir：entries 空数组命中计数语义）
	ok, digest, _ = ToolResultDigest(`{"dir":"docs","entries":[],"count":0}`)
	if !ok || digest != "0 项" {
		t.Fatalf("兜底计数：ok=%v digest=%q", ok, digest)
	}
	// 零摘要：写工具 {"ok":true} —— 只留状态点
	ok, digest, preview := ToolResultDigest(`{"ok":true,"id":"i9"}`)
	if !ok || digest != "" {
		t.Fatalf("无语义应零摘要：ok=%v digest=%q", ok, digest)
	}
	// preview = 原始头（截 400 + 省略号——truncateRunes 仓库约定）
	long := strings.Repeat("x", 500)
	_, _, preview = ToolResultDigest(long)
	if !strings.HasSuffix(preview, "…") || len([]rune(preview)) != 401 {
		t.Fatalf("preview 应截 400+省略号，实得 %d", len([]rune(preview)))
	}
}
