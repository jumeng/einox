package engine

// Run 级图片链路回归（官方 harness 视觉路线）：含图附件 → 用户消息携带
// 引用 part → vision 包装解析为 base64 part（模型实际收到图）；纯文本模型
// 会话含图 → 轮前门禁错误事件。

import (
	"context"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/checkpoint"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// newVisionRunManager 图片链路测试引擎（模型声明 image 输入 + 引用解析注入）。
func newVisionRunManager(t *testing.T, imageInput bool, resolve llm.ImageResolver, fm llm.ModelFactory) *Manager {
	t.Helper()
	st := tstore.New(t.TempDir())
	input := []string{"text"}
	if imageInput {
		input = append(input, "image")
	}
	reg := session.NewRegistry(st)
	return NewManager(reg, Options{
		Providers: func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{{ID: "m", Input: input, Priority: 100}},
			}}
		},
		Instruction: func(SessionBrief) string { return "test" },
		NewModel:    fm,
		CheckPoints: func(operator, sid string) CheckPointStore {
			return checkpoint.NewCheckPointStore(st, operator, sid)
		},
		WorkspaceRoot: func(owner, sid string) string { return st.TmpDir() + "/ws/" + owner + "/" + sid },
		ImageResolve:  resolve,
	})
}

// TestRunImageAttachmentResolved 含图附件经引擎与包装两层：Run 构造引用 part，
// 模型调用前解析为 base64——模型真实收到图片。
func TestRunImageAttachmentResolved(t *testing.T) {
	inner := &scriptedModel{}
	m := newVisionRunManager(t, true, func(p string) ([]byte, string, error) {
		if p != "docs/shot.png" {
			t.Fatalf("引用路径失真：%s", p)
		}
		return []byte("PNGDATA"), "image/png", nil
	}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return inner, nil
	})
	s := m.Registry().Create("张三", "看图", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "这图什么内容", []session.Attachment{
		{Name: "shot.png", Path: "docs/shot.png", IsImage: true},
	}, func(session.Event) {})
	waitTitleFlight(t, s)
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.inputs) == 0 {
		t.Fatal("模型未被调用")
	}
	last := inner.inputs[0][len(inner.inputs[0])-1]
	if last.Role != schema.User || len(last.UserInputMultiContent) != 2 {
		t.Fatalf("末条应为双 part 用户消息（文本 + 图）：%+v", last)
	}
	if last.Content != "" {
		t.Fatalf("多模态消息 Content 必须为空（openai 适配 Content 与 parts 并存即 MarshalJSON 拒绝），实得 %q", last.Content)
	}
	img := last.UserInputMultiContent[1]
	if img.Type != schema.ChatMessagePartTypeImageURL || img.Image == nil || img.Image.Base64Data == nil ||
		img.Image.MIMEType != "image/png" {
		t.Fatalf("模型应收到解析后的 base64 图片 part：%+v", img)
	}
}

// TestRunImageGateTextModel 纯文本模型 + 图片附件：轮前门禁——错误事件 +
// error 终态，模型零调用。
func TestRunImageGateTextModel(t *testing.T) {
	inner := &scriptedModel{}
	var events []string
	var mu sync.Mutex
	m := newVisionRunManager(t, false, func(string) ([]byte, string, error) {
		return []byte("x"), "image/png", nil
	}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return inner, nil
	})
	s := m.Registry().Create("张三", "看图", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "这图什么内容", []session.Attachment{
		{Name: "shot.png", Path: "docs/shot.png", IsImage: true},
	}, func(ev session.Event) {
		mu.Lock()
		events = append(events, ev.Event)
		mu.Unlock()
	})
	waitTitleFlight(t, s)
	inner.mu.Lock()
	calls := len(inner.inputs)
	inner.mu.Unlock()
	if calls != 0 {
		t.Fatalf("门禁拒绝后模型不应被调用，实得 %d 次", calls)
	}
	mu.Lock()
	defer mu.Unlock()
	gated := false
	for _, n := range events {
		if n == contract.EvError {
			gated = true
		}
	}
	if !gated {
		t.Fatalf("应发错误事件：%v", events)
	}
	if st := s.StateOf(); st != session.StateError {
		t.Fatalf("终态应为 error：%s", st)
	}
}
