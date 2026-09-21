package engine

// C3 图片省略决策事件化集成回归：真实预算触发驱逐 → image_offload 事件落流
//（含 occurrence 身份）；跨轮粘滞（不重解析、占位逐字节同文、不重落事件）；
// 重读（工具 images 标记带新 occurrence）不受旧决策牵连——恢复通道。

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// imgPartsOf 取指定文本前缀 user 消息的 parts（nil = 未找到）。
func imgPartsOf(msgs []*schema.Message, text string) []schema.MessageInputPart {
	for _, m := range msgs {
		if m.Role == schema.User && len(m.UserInputMultiContent) > 0 && m.UserInputMultiContent[0].Type == schema.ChatMessagePartTypeText &&
			strings.Contains(m.UserInputMultiContent[0].Text, text) {
			return m.UserInputMultiContent
		}
	}
	return nil
}

// offloadEvents 数流中 image_offload 事件条数（返回累计 items 数）。
func offloadEventCount(s *session.Session) (int, []contract.ImageOffloadItem) {
	n := 0
	var items []contract.ImageOffloadItem
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvImageOffload {
			n++
			if io, ok := ev.Data.(contract.ImageOffload); ok {
				items = append(items, io.Items...)
			}
		}
	}
	return n, items
}

// TestImageOffloadStickyAndRecovery 端到端：驱逐决策持久化（事件+粘滞集）、
// 跨请求粘滞、重读新 occurrence 恢复。
func TestImageOffloadStickyAndRecovery(t *testing.T) {
	var resolvesA, resolvesB atomic.Int32
	big := make([]byte, 11<<20) // 单张 base64 ≈14.7MiB——两张累计 ≈29MiB 超 20MiB
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	rr, _ := tools.InferTool("re_read", "重读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"images": []string{"imgs/a.png#o=reread-1"}}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "看到了"})
	}}
	m := newTestManager(t, func(o *Options) {
		o.Providers = func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{{ID: "m", Input: []string{"text", "image"}, Priority: 100}},
			}}
		}
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{rr} }
		o.NewModel = factoryOf(fm)
		o.ImageResolve = func(p string) ([]byte, string, error) {
			if p == "imgs/a.png" {
				resolvesA.Add(1)
			} else {
				resolvesB.Add(1)
			}
			return big, "image/png", nil
		}
	})
	s := m.Registry().Create("张三", "看图", "manual", contract.UserPrefs{Model: "p/m"})
	atts := []session.Attachment{
		{Name: "a", Path: "imgs/a.png", IsImage: true},
		{Name: "b", Path: "imgs/b.png", IsImage: true},
	}

	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "看图", atts, func(session.Event) {})
	waitTitleFlight(t, s)
	waitBgQuiescent(t, s)

	in := lastInput(fm)
	parts := imgPartsOf(in, "看图")
	if parts == nil || len(parts) != 3 {
		t.Fatalf("应含文本+两图 part：%d", len(parts))
	}
	if parts[1].Type != schema.ChatMessagePartTypeText || !strings.Contains(parts[1].Text, "imgs/a.png 已省略") {
		t.Fatalf("最老图应占位：%+v", parts[1])
	}
	if parts[2].Type != schema.ChatMessagePartTypeImageURL || parts[2].Image == nil || parts[2].Image.Base64Data == nil {
		t.Fatalf("最新图应保留 base64：%+v", parts[2])
	}
	placeholder1 := parts[1].Text
	n1, items1 := offloadEventCount(s)
	if n1 != 1 || len(items1) != 1 || items1[0].Path != "imgs/a.png" || items1[0].ID == "" {
		t.Fatalf("应恰一条驱逐决策事件（含 occurrence 身份）：%d %v", n1, items1)
	}
	if resolvesA.Load() != 1 || resolvesB.Load() != 1 { // 首请求：粘性集空，a/b 各解析一次（a 解析后才判驱逐）
		t.Fatalf("首请求应解析两图：a=%d b=%d", resolvesA.Load(), resolvesB.Load())
	}

	runNote(m, s, "再问") // 第二轮：粘滞——不重解析 a、占位同文、不重落事件
	waitTitleFlight(t, s)
	waitBgQuiescent(t, s)
	in = lastInput(fm)
	parts = imgPartsOf(in, "看图")
	if parts == nil || parts[1].Text != placeholder1 {
		t.Fatalf("粘性占位应逐字节同文：%q vs %q", parts[1].Text, placeholder1)
	}
	if resolvesA.Load() != 1 { // 粘滞：a 不重解析（b 保留图每请求重建 base64——历史只存轻引用，属既有行为）
		t.Fatalf("粘滞命中不应重解析 a：%d", resolvesA.Load())
	}
	if n2, _ := offloadEventCount(s); n2 != 1 {
		t.Fatalf("粘滞命中不应重落事件：%d", n2)
	}

	// 第三轮：模型重读（新 occurrence）——不受旧决策牵连，图片回到请求面
	fm.onStream = func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("cr", "re_read", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "恢复看到了"})
	}
	runNote(m, s, "重读看看")
	waitTitleFlight(t, s)
	waitBgQuiescent(t, s)
	in = lastInput(fm)
	var recovered bool
	for _, msg := range in {
		if msg.Role != schema.User {
			continue
		}
		for _, p := range msg.UserInputMultiContent {
			if p.Type == schema.ChatMessagePartTypeImageURL && p.Image != nil && p.Image.Base64Data != nil {
				recovered = true
			}
		}
	}
	if !recovered {
		t.Fatalf("重读新 occurrence 应恢复到请求面：%s", fmt.Sprint(len(in)))
	}
}
