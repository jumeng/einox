package llm

// vision 包装回归：透传快路径 / 引用解析 / 能力门禁 / 超限驱逐（最老占位）/
// 工具 images 标记升级（合成 user 携图）/ 解析失败降级。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// recModel 记录输入的最小假模型（llmtest 会反向依赖本包，测试内自备）。
type recModel struct{ inputs [][]*schema.Message }

func (r *recModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	r.inputs = append(r.inputs, input)
	return schema.AssistantMessage("ok", nil), nil
}

func (r *recModel) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	r.inputs = append(r.inputs, input)
	sr, sw := schema.Pipe[*schema.Message](2)
	sw.Send(&schema.Message{Role: schema.Assistant, Content: "ok"}, nil)
	sw.Close()
	return sr, nil
}

// attMsg 构造带图片引用 part 的 user 消息。
func attMsg(text, p string) *schema.Message {
	u := AttRefPrefix + p
	return &schema.Message{Role: schema.User, Content: text, UserInputMultiContent: []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeText, Text: text},
		{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{URL: &u},
		}},
	}}
}

func visionSpec(image bool) ModelSpec {
	in := []string{"text"}
	if image {
		in = append(in, "image")
	}
	return ModelSpec{ID: "m", Input: in}
}

func TestVisionPassThrough(t *testing.T) {
	inner := &recModel{}
	v := NewVisionModel(inner, visionSpec(true), func(string) ([]byte, string, error) {
		t.Fatal("无图请求不应触发解析")
		return nil, "", nil
	})
	in := []*schema.Message{schema.UserMessage("纯文本"), schema.AssistantMessage("答", nil)}
	if _, err := v.Generate(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	got := inner.inputs[0]
	if len(got) != 2 || got[0] != in[0] || got[1] != in[1] {
		t.Fatalf("无图请求应原样透传（含消息指针），实得 %+v", got)
	}
}

func TestVisionResolveRef(t *testing.T) {
	inner := &recModel{}
	v := NewVisionModel(inner, visionSpec(true), func(p string) ([]byte, string, error) {
		if p != "docs/a.png" {
			t.Fatalf("路径应解引用：%s", p)
		}
		return []byte("pngbytes"), "image/png", nil
	})
	in := []*schema.Message{attMsg("看这张图", "docs/a.png")}
	if _, err := v.Generate(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	got := inner.inputs[0][0]
	if got.Content != "" {
		t.Fatalf("带 parts 的消息 Content 必须为空——openai 适配层 Content 与 MultiContent 并存即 MarshalJSON 拒绝，实得 %q", got.Content)
	}
	parts := got.UserInputMultiContent
	if len(parts) != 2 || parts[0].Type != schema.ChatMessagePartTypeText || parts[0].Text != "看这张图" {
		t.Fatalf("文本 part 应原样保留：%+v", parts)
	}
	img := parts[1]
	if img.Type != schema.ChatMessagePartTypeImageURL || img.Image == nil || img.Image.Base64Data == nil {
		t.Fatalf("引用应解析为 base64 图片 part：%+v", img)
	}
	if img.Image.MIMEType != "image/png" || *img.Image.Base64Data != "cG5nYnl0ZXM=" { // std base64("pngbytes")
		t.Fatalf("解析产物失真：mime=%s b64=%s", img.Image.MIMEType, *img.Image.Base64Data)
	}
	if in[0].UserInputMultiContent[1].Image.URL == nil || *in[0].UserInputMultiContent[1].Image.URL != AttRefPrefix+"docs/a.png" {
		t.Fatal("原消息（共享历史）不得被改写")
	}
}

func TestVisionGate(t *testing.T) {
	v := NewVisionModel(&recModel{}, visionSpec(false), func(string) ([]byte, string, error) {
		return []byte("x"), "image/png", nil
	})
	_, err := v.Stream(context.Background(), []*schema.Message{attMsg("图", "docs/a.png")})
	if err == nil || !strings.Contains(err.Error(), "不支持图片输入") {
		t.Fatalf("纯文本模型含图请求应硬拒，实得 %v", err)
	}
	// 工具标记图同受门禁（read_image 在文本模型下已被 ctx 门禁拦，此处防直灌）
	v2 := NewVisionModel(&recModel{}, visionSpec(false), nil)
	tool := schema.ToolMessage(`{"ok":true,"images":["docs/a.png"]}`, "c1")
	if _, err := v2.Generate(context.Background(), []*schema.Message{tool}); err == nil || !strings.Contains(err.Error(), "不支持图片输入") {
		t.Fatalf("标记图也应门禁，实得 %v", err)
	}
}

func TestVisionEvict(t *testing.T) {
	old := maxRequestImageBytes
	maxRequestImageBytes = 4 // 仅容一张小图
	t.Cleanup(func() { maxRequestImageBytes = old })
	inner := &recModel{}
	v := NewVisionModel(inner, visionSpec(true), func(p string) ([]byte, string, error) {
		return []byte("1234"), "image/png", nil // base64 后 8 字节 > 预算
	})
	in := []*schema.Message{
		attMsg("旧图", "docs/old.png"),
		schema.AssistantMessage("中间轮", nil),
		attMsg("新图", "docs/new.png"),
	}
	if _, err := v.Generate(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	msgs := inner.inputs[0]
	oldParts := msgs[0].UserInputMultiContent
	if oldParts[1].Type != schema.ChatMessagePartTypeText || !strings.Contains(oldParts[1].Text, "已省略") {
		t.Fatalf("最老的图应被驱逐为文本占位：%+v", oldParts[1])
	}
	newParts := msgs[2].UserInputMultiContent
	if newParts[1].Type != schema.ChatMessagePartTypeImageURL || newParts[1].Image == nil || newParts[1].Image.Base64Data == nil {
		t.Fatalf("最新的图应保留：%+v", newParts[1])
	}
}

func TestVisionToolMarker(t *testing.T) {
	inner := &recModel{}
	v := NewVisionModel(inner, visionSpec(true), func(p string) ([]byte, string, error) {
		return []byte("img"), "image/png", nil
	})
	tool := schema.ToolMessage(`{"path":"docs/a.png","mime":"image/png","images":["docs/a.png"]}`, "c1")
	if _, err := v.Generate(context.Background(), []*schema.Message{tool}); err != nil {
		t.Fatal(err)
	}
	msgs := inner.inputs[0]
	if len(msgs) != 2 || msgs[0] != tool {
		t.Fatalf("工具消息应原位保留并追加合成消息：%+v", msgs)
	}
	syn := msgs[1]
	if syn.Role != schema.User || len(syn.UserInputMultiContent) != 2 {
		t.Fatalf("合成 user 消息应含标注文本 + 图片 part：%+v", syn)
	}
	if syn.Content != "" {
		t.Fatalf("合成消息 Content 必须为空（并存即 MarshalJSON 拒绝），实得 %q", syn.Content)
	}
	img := syn.UserInputMultiContent[1]
	if img.Type != schema.ChatMessagePartTypeImageURL || img.Image == nil || img.Image.Base64Data == nil {
		t.Fatalf("标记图应升级为 base64 part：%+v", img)
	}
	if tool.Content != `{"path":"docs/a.png","mime":"image/png","images":["docs/a.png"]}` {
		t.Fatal("工具消息内容不得被改写")
	}
}

func TestVisionResolveFailDegrades(t *testing.T) {
	inner := &recModel{}
	v := NewVisionModel(inner, visionSpec(true), func(string) ([]byte, string, error) {
		return nil, "", errors.New("文件不存在")
	})
	if _, err := v.Generate(context.Background(), []*schema.Message{attMsg("图", "docs/gone.png")}); err != nil {
		t.Fatalf("解析失败应降级占位而非毁请求：%v", err)
	}
	part := inner.inputs[0][0].UserInputMultiContent[1]
	if part.Type != schema.ChatMessagePartTypeText || !strings.Contains(part.Text, "读取失败") {
		t.Fatalf("解析失败应为文本占位：%+v", part)
	}
}
