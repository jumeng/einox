package llm

// N1-b system 消息边界归一单测：非能力模型收到 mid-history system → 最后
// 一条归位 position-0、其余剔除（最新生效语义的归一化）；无 mid-system
// 透传零拷贝；原切片不改写（视图纪律）。

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestHoistLatestSystemPassthrough 无 mid-system：原切片透传（指针同一）。
func TestHoistLatestSystemPassthrough(t *testing.T) {
	msgs := []*schema.Message{
		schema.SystemMessage("锚"),
		schema.UserMessage("问"),
		schema.AssistantMessage("答", nil),
	}
	out := hoistLatestSystem(msgs)
	if len(out) != 3 || &out[0] != &msgs[0] || &out[2] != &msgs[2] {
		t.Fatal("无 mid-system 应透传零拷贝（指针同一）")
	}
}

// TestHoistLatestSystemNoSystem 全程无 system 消息：透传。
func TestHoistLatestSystemNoSystem(t *testing.T) {
	msgs := []*schema.Message{schema.UserMessage("问"), schema.AssistantMessage("答", nil)}
	if out := hoistLatestSystem(msgs); len(out) != 2 || &out[0] != &msgs[0] {
		t.Fatal("无 system 应透传零拷贝")
	}
}

// TestHoistLatestSystemHoistsLast 末位 system 归位首位、其余 system 剔除、
// 消息保序；原切片不动。
func TestHoistLatestSystemHoistsLast(t *testing.T) {
	msgs := []*schema.Message{
		schema.SystemMessage("锚"),
		schema.UserMessage("问1"),
		schema.AssistantMessage("答1", nil),
		schema.SystemMessage("V2"),
		schema.UserMessage("问2"),
		schema.SystemMessage("V3"),
		schema.UserMessage("问3"),
	}
	out := hoistLatestSystem(msgs)
	if len(out) != 5 { // V3 + 问1 答1 问2 问3
		t.Fatalf("归一后长度应 5：%d", len(out))
	}
	if out[0].Role != schema.System || out[0].Content != "V3" {
		t.Fatalf("最后一条 system 应归位 position-0：%s", out[0].Content)
	}
	for _, m := range out[1:] {
		if m.Role == schema.System {
			t.Fatal("mid-system 应全部剔除")
		}
	}
	if out[1].Content != "问1" || out[3].Content != "问2" || out[4].Content != "问3" {
		t.Fatal("非 system 消息应保序")
	}
	if len(msgs) != 7 || msgs[0].Content != "锚" { // 原切片不改写
		t.Fatal("原切片不应被改写")
	}
}

// TestHoistLatestSystemOnlyPositionZero system 仅在首位：透传（首位即最新）。
func TestHoistLatestSystemOnlyPositionZero(t *testing.T) {
	msgs := []*schema.Message{schema.SystemMessage("唯一"), schema.UserMessage("问")}
	if out := hoistLatestSystem(msgs); len(out) != 2 || &out[0] != &msgs[0] {
		t.Fatal("仅在首位的 system 应透传")
	}
}
