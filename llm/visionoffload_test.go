package llm

// C3 图片省略粘性选择集回归：occurrence 铸造/解析、粘性命中恒占位（不解析
// 不占预算、字节与首次驱逐同文）、新驱逐上报、旧引用（无 #o= 片段）零变化、
// resolve 收净路径。

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
)

// fakeSink 测试承载面（记上报 + 可预置粘性集）。
type fakeSink struct {
	pre      map[string]bool
	noted    []contract.ImageOffloadItem
	resolved []string
}

func (f *fakeSink) OffloadedIDs() map[string]bool { return f.pre }

func (f *fakeSink) NoteImageOffload(items []contract.ImageOffloadItem) {
	f.noted = append(f.noted, items...)
}

// userParts 取假模型输入首条 user 消息的 parts。
func userParts(in []*schema.Message) []schema.MessageInputPart {
	for _, m := range in {
		if m.Role == schema.User {
			return m.UserInputMultiContent
		}
	}
	return nil
}

// TestMintAndSplitOcc 铸造往返：净路径与 occurrence 标识可拆、resolve 收净路径。
func TestMintAndSplitOcc(t *testing.T) {
	ref := MintAttRef("docs/img.png")
	if !strings.HasPrefix(ref, AttRefPrefix) {
		t.Fatalf("应带方案前缀：%s", ref)
	}
	path, occ := splitOcc(strings.TrimPrefix(ref, AttRefPrefix))
	if path != "docs/img.png" || occ == "" {
		t.Fatalf("拆分失真：path=%s occ=%s", path, occ)
	}
	ref2 := MintAttRef("docs/img.png")
	if ref == ref2 {
		t.Fatal("同路径两次铸造应得不同 occurrence")
	}
	if p, _ := splitOcc("docs/plain.png"); p != "docs/plain.png" {
		t.Fatalf("无片段引用应原样：%s", p)
	}
}

// TestVisionStickyOffloadPlaceholder 粘性命中：预置集内的 occurrence 恒占位
// （预算文案、不解析、不重上报），集外图照常保留。
func TestVisionStickyOffloadPlaceholder(t *testing.T) {
	old := maxRequestImageBytes
	maxRequestImageBytes = 1 << 20
	t.Cleanup(func() { maxRequestImageBytes = old })

	stickyRef := MintAttRef("imgs/old.png")
	freshRef := MintAttRef("imgs/new.png")
	_, occ := splitOcc(strings.TrimPrefix(stickyRef, AttRefPrefix))
	sink := &fakeSink{pre: map[string]bool{occ: true}}
	var resolved []string
	rm := &recModel{}
	vm := NewVisionModel(rm, ModelSpec{Input: []string{"text", "image"}},
		func(p string) ([]byte, string, error) {
			resolved = append(resolved, p)
			return []byte("imagedata"), "image/png", nil
		})
	msg := &schema.Message{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{URL: &stickyRef}}},
		{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{URL: &freshRef}}},
	}}
	if _, err := vm.Generate(WithImageOffloadSink(context.Background(), sink), []*schema.Message{msg}); err != nil {
		t.Fatal(err)
	}
	if len(rm.inputs) == 0 {
		t.Fatal("假模型未收到输入")
	}
	parts := userParts(rm.inputs[0])
	if len(parts) != 2 { // 原位替换：两个 part 各自变换
		t.Fatalf("parts 应两枚（原位替换）：%d", len(parts))
	}
	if parts[0].Type != schema.ChatMessagePartTypeText || !strings.Contains(parts[0].Text, "imgs/old.png 已省略") {
		t.Fatalf("粘性命中应预算文案占位：%+v", parts[0])
	}
	if parts[1].Type != schema.ChatMessagePartTypeImageURL || parts[1].Image == nil || parts[1].Image.Base64Data == nil {
		t.Fatalf("集外图应保留为 base64 part：%+v", parts[1])
	}
	for _, p := range resolved {
		if p == "imgs/old.png" {
			t.Fatal("粘性占位不应触发 resolve")
		}
	}
	if len(sink.noted) != 0 {
		t.Fatalf("粘性命中不重上报：%v", sink.noted)
	}
}

// TestVisionOffloadNotifyAndStickyByteStability 新驱逐上报；下一请求粘性占位
// 与首次驱逐逐字节同文。
func TestVisionOffloadNotifyAndStickyByteStability(t *testing.T) {
	old := maxRequestImageBytes
	maxRequestImageBytes = 8 // 两张小图即超（base64 后 > 8）
	t.Cleanup(func() { maxRequestImageBytes = old })

	refOld := MintAttRef("imgs/a.png")
	refNew := MintAttRef("imgs/b.png")
	sink := &fakeSink{pre: map[string]bool{}}
	rm := &recModel{}
	vm := NewVisionModel(rm, ModelSpec{Input: []string{"text", "image"}},
		func(p string) ([]byte, string, error) { return []byte("12345678"), "image/png", nil })
	mk := func(a, b string) []*schema.Message {
		return []*schema.Message{{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{URL: &a}}},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{URL: &b}}},
		}}}
	}
	ctx := WithImageOffloadSink(context.Background(), sink)
	if _, err := vm.Generate(ctx, mk(refOld, refNew)); err != nil {
		t.Fatal(err)
	}
	if len(sink.noted) != 1 || sink.noted[0].Path != "imgs/a.png" || sink.noted[0].ID == "" {
		t.Fatalf("最老图应上报驱逐：%v", sink.noted)
	}
	p1 := userParts(rm.inputs[0])
	if p1[0].Type != schema.ChatMessagePartTypeText || !strings.Contains(p1[0].Text, "imgs/a.png 已省略") {
		t.Fatalf("最老图应占位：%+v", p1[0])
	}
	// 第二请求：sink 已含该 occurrence（模拟 session 集更新）→ 粘性占位同文、不重上报
	sink.pre[sink.noted[0].ID] = true
	if _, err := vm.Generate(ctx, mk(refOld, refNew)); err != nil {
		t.Fatal(err)
	}
	p2 := userParts(rm.inputs[1])
	if p1[0].Text != p2[0].Text {
		t.Fatalf("粘性占位应逐字节同文：\n一=%s\n二=%s", p1[0].Text, p2[0].Text)
	}
	if len(sink.noted) != 1 {
		t.Fatalf("粘性命中不应重上报：%v", sink.noted)
	}
}

// TestVisionLegacyRefUntracked 旧引用（无 occurrence 片段）：预算现算、不进
// 上报面（零变化——存量会话行为不受 C3 影响）。
func TestVisionLegacyRefUntracked(t *testing.T) {
	old := maxRequestImageBytes
	maxRequestImageBytes = 8
	t.Cleanup(func() { maxRequestImageBytes = old })

	legacyA, legacyB := AttRefPrefix+"imgs/la.png", AttRefPrefix+"imgs/lb.png"
	sink := &fakeSink{pre: map[string]bool{}}
	rm := &recModel{}
	vm := NewVisionModel(rm, ModelSpec{Input: []string{"text", "image"}},
		func(p string) ([]byte, string, error) { return []byte("12345678"), "image/png", nil })
	ctx := WithImageOffloadSink(context.Background(), sink)
	if _, err := vm.Generate(ctx, []*schema.Message{{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{URL: &legacyA}}},
		{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{URL: &legacyB}}},
	}}}); err != nil {
		t.Fatal(err)
	}
	p := userParts(rm.inputs[0])
	if p[0].Type != schema.ChatMessagePartTypeText || !strings.Contains(p[0].Text, "imgs/la.png 已省略") {
		t.Fatalf("旧引用超预算仍应现算占位（既有行为）：%+v", p[0])
	}
	if len(sink.noted) != 0 {
		t.Fatalf("旧引用不进持久化面：%v", sink.noted)
	}
}
