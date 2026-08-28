package llm

// 超时包装器回归（网络容错 ①）：正常流不误杀 / 静默死链确定性转哨兵 /
// Generate 总超时。测试内直改包级旋钮（缩至百毫秒级）。

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// fakeStreamModel 假模型：Stream 建管道即回（sw 留给测试驱动写——模拟
// 网络侧chunk 时序）；started 每次 Stream 调用发信号（测试同步锚）。
type fakeStreamModel struct {
	started chan struct{}
	sw      *schema.StreamWriter[*schema.Message]
}

func (f *fakeStreamModel) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	<-ctx.Done() // 阻塞至超时/取消（Generate 超时用例）
	return nil, ctx.Err()
}

func (f *fakeStreamModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	sr, sw := schema.Pipe[*schema.Message](1)
	f.sw = sw
	close(f.started)
	return sr, nil
}

func setTimeouts(t *testing.T, idle, gen time.Duration) {
	t.Helper()
	oi, og := idleTimeout, generateTimeout
	idleTimeout, generateTimeout = idle, gen
	t.Cleanup(func() { idleTimeout, generateTimeout = oi, og })
}

func TestTimeoutStreamNormalFlow(t *testing.T) {
	setTimeouts(t, 400*time.Millisecond, 5*time.Second)
	f := &fakeStreamModel{started: make(chan struct{})}
	wrapped := NewTimeoutModel(f)
	sr, err := wrapped.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	<-f.started
	go func() { // 网络侧：间隔远小于空闲阈值写 2 chunk 后正常收线
		f.sw.Send(&schema.Message{Role: schema.Assistant, Content: "a"}, nil)
		f.sw.Send(&schema.Message{Role: schema.Assistant, Content: "b"}, nil)
		f.sw.Close()
	}()
	var got []string
	for {
		chunk, rerr := sr.Recv()
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				t.Fatalf("正常流不应报错：%v", rerr)
			}
			break
		}
		if chunk != nil && chunk.Content != "" {
			got = append(got, chunk.Content)
		}
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("应原样收到 2 个 chunk：%v", got)
	}
}

func TestTimeoutStreamIdleSentinel(t *testing.T) {
	setTimeouts(t, 150*time.Millisecond, 5*time.Second)
	f := &fakeStreamModel{started: make(chan struct{})}
	wrapped := NewTimeoutModel(f)
	sr, err := wrapped.Stream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	<-f.started
	// 静默死链：网络侧零字节——看门狗应送出哨兵（检测粒度 idle/4）
	start := time.Now()
	_, rerr := sr.Recv()
	if !errors.Is(rerr, ErrIdleTimeout) {
		t.Fatalf("静默死链应得空闲哨兵，实得 %v", rerr)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("哨兵过迟：%v", el)
	}
}

func TestTimeoutGenerateDeadline(t *testing.T) {
	setTimeouts(t, 5*time.Second, 120*time.Millisecond)
	f := &fakeStreamModel{}
	wrapped := NewTimeoutModel(f)
	start := time.Now()
	_, err := wrapped.Generate(context.Background(), nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Generate 应超时：%v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("超时过迟：%v", el)
	}
	if !Classify(err).Retryable {
		t.Fatalf("超时应归可重试：%+v", Classify(err))
	}
}
