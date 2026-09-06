package llm

import (
	"testing"

	"github.com/jumeng/einox/contract"
)

// TestNormalizeEffort 档位归一表测：四档原样 + 旧值/空值/未知归一。
// 旧值来自升级前用户偏好与存量会话快照（on/off 二值时代）。
func TestNormalizeEffort(t *testing.T) {
	// 归一权威已迁 contract（审查 P3-3）——经 llm 测试仍在 llm 包锚定语义。
	cases := map[string]string{
		"off": "off", "low": "low", "high": "high", "max": "max", // 四档原样（off 2026-08-31 回归）
		"on": "max",                                  // 旧「开」= enabled+effort max
		"":   "low", "medium": "low", "xhigh": "low", // 缺省/未知 → 默认低档
	}
	for in, want := range cases {
		if got := contract.NormalizeEffort(in); got != want {
			t.Errorf("contract.NormalizeEffort(%q) = %q，应为 %q", in, got, want)
		}
	}
}

// TestDeepseekThinkingFields 思考扩展字段映射：开档闸门 enabled + 三档直传
// 档位名；关档闸门 disabled 且不发 effort（effort → HTTP 请求体的最后一跳，
// 本测锚定防回归）。
func TestDeepseekThinkingFields(t *testing.T) {
	for _, e := range []string{"low", "high", "max"} {
		f := deepseekThinkingFields(e)
		think, ok := f["thinking"].(map[string]any)
		if !ok || think["type"] != "enabled" {
			t.Errorf("%s 档 thinking 闸门应为 enabled，实得 %v", e, f["thinking"])
		}
		if f["reasoning_effort"] != e {
			t.Errorf("%s 档 reasoning_effort 应直传档位名，实得 %v", e, f["reasoning_effort"])
		}
	}
	off := deepseekThinkingFields("off")
	think, ok := off["thinking"].(map[string]any)
	if !ok || think["type"] != "disabled" {
		t.Errorf("off 档 thinking 闸门应为 disabled，实得 %v", off["thinking"])
	}
	if _, has := off["reasoning_effort"]; has {
		t.Errorf("off 档不应发 reasoning_effort（DeepSeek 文档 effort 属思考模式）：%v", off)
	}
}

// TestThinkingConfigOf anthropic 协议关/开分支：off = nil（协议零思考字段
// 即关），开档 = OfEnabled 预算档（预算数值由 TestThinkingBudget 锚定，此处
// 只锚分支与预算联动）。
func TestThinkingConfigOf(t *testing.T) {
	m := ModelSpec{Limit: &Limit{Output: 128_000}}
	if c := thinkingConfigOf("off", m); c != nil {
		t.Fatalf("off 档应不发思考块（nil），实得 %+v", c)
	}
	for _, e := range []string{"low", "high", "max"} {
		c := thinkingConfigOf(e, m)
		if c == nil || c.OfEnabled == nil || c.OfEnabled.BudgetTokens != thinkingBudget(e, m) {
			t.Errorf("%s 档应为 OfEnabled 预算档且与 thinkingBudget 同源，实得 %+v", e, c)
		}
	}
}

// TestThinkingBudget anthropic 协议三档预算：低 8192 / 高 32768 / 最高 =
// limit.output-1024（顶满会被协议拒），小窗钳制与下限兜底。
func TestThinkingBudget(t *testing.T) {
	// 常规窗（输出 128K）：低 8192 / 高 32768 / 最高 = output-1024
	big := ModelSpec{Limit: &Limit{Output: 128_000}}
	for e, want := range map[string]int64{"low": 8192, "high": 32768, "max": 126_976} {
		if got := thinkingBudget(e, big); got != want {
			t.Errorf("128K 窗 %s 档预算 = %d，应为 %d", e, got, want)
		}
	}
	// 小窗（输出 16K）：高档被钳到 output-1024；最高同；低下限兜底不受影响
	small := ModelSpec{Limit: &Limit{Output: 16_000}}
	for e, want := range map[string]int64{"high": 14_976, "max": 14_976} {
		if got := thinkingBudget(e, small); got != want {
			t.Errorf("16K 窗 %s 档预算 = %d，应为 %d", e, got, want)
		}
	}
	// 极小窗（输出 2048）：全档钳到 1024 下限
	tiny := ModelSpec{Limit: &Limit{Output: 2048}}
	if got := thinkingBudget("max", tiny); got != 1024 {
		t.Errorf("2K 窗最高档预算 = %d，应为下限 1024", got)
	}
	// 未配 Limit：兜自定义模板默认 8192（MaxTokens 同兜——anthropic 必填），
	// 全档钳到 8192-1024=7168（预算须严格小于 max_tokens，宁保守不越协议）
	for _, e := range []string{"low", "high", "max"} {
		if got := thinkingBudget(e, ModelSpec{}); got != 7168 {
			t.Errorf("无窗 %s 档预算 = %d，应为默认输出钳制 7168", e, got)
		}
	}
}

// TestSamplingOf 采样参数映射：未声明不发字段（nil=端点默认）、显式声明经
// f32 窄化下发、单项声明互不影响（两协议组件共用此单点）。
func TestSamplingOf(t *testing.T) {
	if temp, topP := samplingOf(ModelSpec{}); temp != nil || topP != nil {
		t.Fatalf("未声明应不发字段：%v %v", temp, topP)
	}
	tp, pp := 0.7, 0.9
	temp, topP := samplingOf(ModelSpec{Temperature: &tp, TopP: &pp})
	if temp == nil || *temp != float32(0.7) || topP == nil || *topP != float32(0.9) {
		t.Fatalf("显式声明应下发（f32 窄化）：%v %v", temp, topP)
	}
	if temp, topP = samplingOf(ModelSpec{Temperature: &tp}); temp == nil || topP != nil {
		t.Fatalf("单项声明应只发该项：%v %v", temp, topP)
	}
}

// TestFlattenModelsPriorityOrder（安全审查 2026-09-06）：平铺按 priority
// 降序、同优先级稳定保清单序——「priority 序」注释此前零排序兑现，
// ResolveCurrent 的「回退第一模型」拿到的是配置切片序。
func TestFlattenModelsPriorityOrder(t *testing.T) {
	ps := []ProviderSpec{
		{ID: "a", Kind: "openai", Enabled: true, Models: []ModelSpec{
			{ID: "low1", Priority: 10},
			{ID: "same1", Priority: 50},
		}},
		{ID: "b", Kind: "openai", Enabled: true, Models: []ModelSpec{
			{ID: "top", Priority: 100},
			{ID: "same2", Priority: 50},
		}},
		{ID: "off", Kind: "openai", Models: []ModelSpec{{ID: "gone", Priority: 999}}},
	}
	got := FlattenModels(ps)
	keys := make([]string, len(got))
	for i, m := range got {
		keys[i] = m.Key
	}
	want := []string{"b/top", "a/same1", "b/same2", "a/low1"}
	if len(keys) != len(want) {
		t.Fatalf("禁用 provider 应排除：实得 %v", keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("应按 priority 降序（同优先级稳定保清单序）：实得 %v 期望 %v", keys, want)
		}
	}
}
