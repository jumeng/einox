package llm

import "testing"

// TestNormalizeEffort 档位归一表测：三档原样 + 旧值/空值/未知归一。
// 旧值来自升级前用户偏好与存量会话快照（on/off 二值时代）。
func TestNormalizeEffort(t *testing.T) {
	cases := map[string]string{
		"low": "low", "high": "high", "max": "max", // 三档原样
		"on":  "max",                                             // 旧「开」= enabled+effort max
		"off": "low", "": "low", "medium": "low", "xhigh": "low", // 旧「关」/缺省/未知 → 默认低档
	}
	for in, want := range cases {
		if got := NormalizeEffort(in); got != want {
			t.Errorf("NormalizeEffort(%q) = %q，应为 %q", in, got, want)
		}
	}
}

// TestDeepseekThinkingFields 思考扩展字段映射：闸门恒开 + 三档直传档位名
// （effort → HTTP 请求体的最后一跳，直赋值无分支——本测锚定防回归）。
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
