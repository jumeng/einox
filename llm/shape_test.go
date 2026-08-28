package llm

// shape 包装回归：两分剥离（已结算剥/在途带 tool_calls 保留）/ 非工具轮剥离 /
// reasoning-only 剥后整条剔除 / copy-on-write 源零变更 / 无 reasoning 透传 /
// Stream 同整形（recModel 复用 vision_test 定义）。

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// shapedRun 助手：Generate 走一次包装器返回内层实收输入。
func shapedRun(t *testing.T, in []*schema.Message) []*schema.Message {
	t.Helper()
	inner := &recModel{}
	if _, err := NewHistoryShapeModel(inner, "openai").Generate(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	return inner.inputs[0]
}

func TestShapeTwoPartRule(t *testing.T) {
	a1 := schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "f", Arguments: "{}"}}})
	a1.ReasoningContent = "已结算思考"
	a2 := schema.AssistantMessage("", []schema.ToolCall{{ID: "c2", Type: "function", Function: schema.FunctionCall{Name: "f", Arguments: "{}"}}})
	a2.ReasoningContent = "在途思考"
	in := []*schema.Message{
		schema.SystemMessage("s"),
		schema.UserMessage("u1"),
		a1,
		schema.ToolMessage("r1", "c1"),
		schema.UserMessage("u2"),
		a2,
		schema.ToolMessage("r2", "c2"),
	}
	got := shapedRun(t, in)
	if len(got) != 7 {
		t.Fatalf("消息数应不变（无空壳），实得 %d", len(got))
	}
	if got[2].ReasoningContent != "" || len(got[2].ToolCalls) != 1 {
		t.Fatalf("已结算轮应剥 reasoning 留 tool_calls：%+v", got[2])
	}
	if got[5].ReasoningContent != "在途思考" {
		t.Fatalf("在途轮（最后 user 后带 tool_calls）应保留 reasoning，实得 %q", got[5].ReasoningContent)
	}
}

func TestShapeNonToolRound(t *testing.T) {
	text := schema.AssistantMessage("正文", nil)
	text.ReasoningContent = "带正文的非工具轮思考"
	in := []*schema.Message{schema.UserMessage("u"), text}
	got := shapedRun(t, in)
	if len(got) != 2 || got[1].Content != "正文" || got[1].ReasoningContent != "" {
		t.Fatalf("非工具轮应剥 reasoning 留正文：%+v", got)
	}
}

func TestShapeReasoningOnlyDropped(t *testing.T) {
	ro := schema.AssistantMessage("", nil)
	ro.ReasoningContent = "纯思考轮"
	in := []*schema.Message{schema.UserMessage("u1"), ro, schema.UserMessage("u2")}
	got := shapedRun(t, in)
	if len(got) != 2 || got[0].Content != "u1" || got[1].Content != "u2" {
		t.Fatalf("reasoning-only 轮剥后应整条剔除（user 相邻合法）： %+v", got)
	}
}

func TestShapeCopyOnWrite(t *testing.T) {
	a1 := schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "f", Arguments: "{}"}}})
	a1.ReasoningContent = "原文"
	// 末位再置一条 user：a1 之后出现新 user 消息 → 已结算（仅 [user, a1, tool]
	// 是在途形态，a1 属保留例外——shape 两分规则）
	in := []*schema.Message{schema.UserMessage("u"), a1, schema.ToolMessage("r", "c1"), schema.UserMessage("u2")}
	got := shapedRun(t, in)
	if len(got) != 4 {
		t.Fatalf("消息数应不变（无空壳），实得 %d", len(got))
	}
	if a1.ReasoningContent != "原文" {
		t.Fatal("源消息（共享历史）不得被改写")
	}
	if got[1] == in[1] {
		t.Fatal("被剥消息应是克隆而非源指针")
	}
	if got[0] != in[0] || got[2] != in[2] {
		t.Fatal("未改动消息应保持源指针")
	}
}

func TestShapePassThrough(t *testing.T) {
	inner := &recModel{}
	w := NewHistoryShapeModel(inner, "anthropic")
	in := []*schema.Message{schema.SystemMessage("s"), schema.UserMessage("u"), schema.AssistantMessage("答", nil)}
	if _, err := w.Generate(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	got := inner.inputs[0]
	if len(got) != 3 || got[0] != in[0] || got[1] != in[1] || got[2] != in[2] {
		t.Fatalf("无 reasoning 历史应原样透传（含消息指针），实得 %+v", got)
	}
}

func TestShapeStream(t *testing.T) {
	inner := &recModel{}
	a1 := schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "f", Arguments: "{}"}}})
	a1.ReasoningContent = "已结算"
	// 尾置 user2 使 a1 成已结算轮（见 TestShapeCopyOnWrite 注）
	in := []*schema.Message{schema.UserMessage("u"), a1, schema.ToolMessage("r", "c1"), schema.UserMessage("u2")}
	if _, err := NewHistoryShapeModel(inner, "openai").Stream(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	got := inner.inputs[0]
	if len(got) != 4 || got[1].ReasoningContent != "" {
		t.Fatalf("Stream 路径应同样整形：%+v", got)
	}
}

// TestShapeAnthropicExtraKeys H9-2 anthropic 协议分叉：已结算轮删 Extra
// thinking 双键（连带 signature）+ 剥 ReasoningContent，其他 Extra 键保留；
// 在途 tool_calls 轮双键与 ReasoningContent 原样（官方协议：历史可省/在途
// 必带 complete unmodified）。kind 空（openai 计数口径）不动 Extra。
func TestShapeAnthropicExtraKeys(t *testing.T) {
	settled := schema.AssistantMessage("答1", nil)
	settled.ReasoningContent = "已结算思考"
	settled.Extra = map[string]any{keyClaudeThinking: "t1", keyClaudeThinkingSignature: "s1", "other": "keep"}
	inFlight := schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "f", Arguments: "{}"}}})
	inFlight.ReasoningContent = "在途思考"
	inFlight.Extra = map[string]any{keyClaudeThinking: "t2", keyClaudeThinkingSignature: "s2"}
	in := []*schema.Message{schema.UserMessage("u1"), settled, schema.UserMessage("u2"), inFlight}

	got := ShapeMessagesForKind(in, "anthropic")
	if len(got) != 4 {
		t.Fatalf("应保四条，实得 %d", len(got))
	}
	s := got[1]
	if s.ReasoningContent != "" {
		t.Fatalf("已结算轮 ReasoningContent 应剥：%q", s.ReasoningContent)
	}
	if _, ok := s.Extra[keyClaudeThinking]; ok {
		t.Fatal("已结算轮 Extra thinking 键应删")
	}
	if _, ok := s.Extra[keyClaudeThinkingSignature]; ok {
		t.Fatal("已结算轮 Extra signature 键应删（独立键非嵌套）")
	}
	if s.Extra["other"] != "keep" {
		t.Fatal("无关 Extra 键不应误删")
	}
	f := got[3]
	if f.ReasoningContent != "在途思考" || f.Extra[keyClaudeThinking] != "t2" || f.Extra[keyClaudeThinkingSignature] != "s2" {
		t.Fatalf("在途 tool_calls 轮 thinking 应原样保留（协议必带）：%+v", f)
	}

	// openai 口径（kind 空）：Extra 不动（计数同口径说明见 shape.go 包注释）
	got2 := ShapeMessagesForKind(in, "")
	if _, ok := got2[1].Extra[keyClaudeThinking]; !ok {
		t.Fatal("kind 空（openai 规则）不应动 Extra 键")
	}
}

// TestShapeAnthropicReasoningOnlyDropped H9-2：anthropic reasoning-only 轮
// （content 空、thinking 只在 Extra）剥后整条剔除——与 openai 空壳剔除同构。
func TestShapeAnthropicReasoningOnlyDropped(t *testing.T) {
	only := schema.AssistantMessage("", nil)
	only.Extra = map[string]any{keyClaudeThinking: "纯思考", keyClaudeThinkingSignature: "sig"}
	in := []*schema.Message{schema.UserMessage("u"), only, schema.UserMessage("u2")}
	got := ShapeMessagesForKind(in, "anthropic")
	if len(got) != 2 {
		t.Fatalf("Extra-only 零负载轮应整条剔除，实得 %d 条", len(got))
	}
}

// TestShapeAnthropicKeyAnchor 键名防退化锚：Extra 双键字符串是 eino-ext
// claude 包内私有常量（message_extra.go keyOfThinking/keyOfThinkingSignature）
// 的硬编码镜像——上游改键名即静默失效（剥不到 = 历史照传），本锚防本仓误改；
// 升级 eino-ext 时须对照上游常量复核（真端点验证留部署机）。
func TestShapeAnthropicKeyAnchor(t *testing.T) {
	if keyClaudeThinking != "_eino_claude_thinking" || keyClaudeThinkingSignature != "_eino_claude_thinking_signature" {
		t.Fatalf("Extra 键名被改动——须与 eino-ext claude message_extra.go 常量严格一致：%q/%q", keyClaudeThinking, keyClaudeThinkingSignature)
	}
}
