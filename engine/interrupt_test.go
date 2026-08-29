package engine

// 打断语义告知回归：显式停止收尾时历史追加系统注记（部分执行三义——
// 续聊轮模型可见面）；FlushQueue 打断路径同注记。

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

func TestInterruptMarkerInHistory(t *testing.T) {
	m := newSeamManager(t, func(o *Options) {
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return &scriptedModel{}, nil
		}
	})
	s := m.Registry().Create("张三", "会停", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)

	m.interruptUnlessStopped(s, func(session.Event) {})

	hist := s.CloneHistory()
	if len(hist) == 0 {
		t.Fatal("打断应注入历史注记")
	}
	last := hist[len(hist)-1]
	if last.Role != schema.User {
		t.Fatalf("注记应为 user 消息：%s", last.Role)
	}
	for _, want := range []string{"被中断", "部分", "核对现场"} {
		if !strings.Contains(last.Content, want) {
			t.Fatalf("注记应含 %q：%s", want, last.Content)
		}
	}
}
