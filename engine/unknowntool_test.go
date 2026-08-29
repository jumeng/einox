package engine

// 幻觉工具兜底单元回归：信封构造分流（真幻觉/动态装载 miss/拼写建议唯一
// 命中才提示）。引擎级行为（信封回喂自纠、不杀轮）由 assembly/subagent 的
// 面外调用用例覆盖。

import (
	"strings"
	"testing"
)

func TestUnknownToolEnvelopePlainMiss(t *testing.T) {
	got := unknownToolEnvelope("nope", []string{"read_file", "todo_write"}, nil)
	for _, want := range []string{"nope", "不存在", "read_file", "todo_write"} {
		if !strings.Contains(got, want) {
			t.Fatalf("信封应含 %q：%s", want, got)
		}
	}
	if strings.Contains(got, "tool_search") {
		t.Fatal("名单外 miss 不应含动态装载指引")
	}
}

func TestUnknownToolEnvelopeDynamicMiss(t *testing.T) {
	dyn := map[string]bool{"web_fetch": true}
	got := unknownToolEnvelope("web_fetch", []string{"read_file", "web_fetch"}, dyn)
	if !strings.Contains(got, "tool_search") {
		t.Fatalf("动态装载名单内 miss 应指引先检索：%s", got)
	}
}

func TestUnknownToolSuggest(t *testing.T) {
	// 大小写/标点变体：归一化后唯一命中 → 建议
	if got := didYouMean("Read.File", []string{"read_file", "todo_write"}); got != "read_file" {
		t.Fatalf("变体唯一命中应建议：实得 %q", got)
	}
	if got := unknownToolEnvelope("READ.FILE", []string{"read_file"}, nil); !strings.Contains(got, "read_file") {
		t.Fatalf("信封应含拼写建议：%s", got)
	}
	// 多候选不猜（归一化后 readfile 同时命中两个）
	if got := didYouMean("read_file", []string{"ReadFile", "read-file"}); got != "" {
		t.Fatalf("多候选不应建议：实得 %q", got)
	}
	// 真拼写错误不猜（readfil ≠ readfile）
	if got := didYouMean("readfil", []string{"read_file"}); got != "" {
		t.Fatalf("拼写错误不应建议：实得 %q", got)
	}
	if got := didYouMean("", []string{"read_file"}); got != "" {
		t.Fatalf("空名不应建议：实得 %q", got)
	}
}
