package llm

// 模型记录缝单测（批 B；设计件 findings/2026-11-22-model-io-recorder-design.md）：
// 流式聚合/attempt 指纹/失败分类/stalled/panic 隔离/nil 直通。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// fakeRecModel 剧本假模型（第 n 次调用按 script 写出；返回非 nil err = 流以
// 该错误收线）。
type fakeRecModel struct {
	mu     sync.Mutex
	calls  int
	gen    func() (*schema.Message, error)
	script func(n int, send func(*schema.Message)) error
}

func (f *fakeRecModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	if f.gen != nil {
		return f.gen()
	}
	return schema.AssistantMessage("非流式答复", nil), nil
}

func (f *fakeRecModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	sr, sw := schema.Pipe[*schema.Message](8)
	go func() {
		defer sw.Close()
		if f.script == nil {
			sw.Send(&schema.Message{Role: schema.Assistant, Content: "已处理。"}, nil)
			return
		}
		if err := f.script(n, func(m *schema.Message) { sw.Send(m, nil) }); err != nil {
			sw.Send(nil, err)
		}
	}()
	return sr, nil
}

// captureRec 记录捕获（并发安全）。
type captureRec struct {
	mu   sync.Mutex
	recs []ModelCallRecord
}

func (c *captureRec) Record(r ModelCallRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r)
}

func (c *captureRec) all() []ModelCallRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ModelCallRecord(nil), c.recs...)
}

func drain(sr *schema.StreamReader[*schema.Message]) (text string, err error) {
	defer sr.Close()
	for {
		m, e := sr.Recv()
		if e != nil {
			if errors.Is(e, io.EOF) {
				return text, nil
			}
			return text, e
		}
		if m != nil {
			text += m.Content
		}
	}
}

var testIdent = ModelIdentity{ProviderID: "p", ModelID: "m", Effort: "low", Kind: "openai", BaseURL: "http://x"}

// TestRecordingStreamAggregation 流式透传+终态聚合：chunk 全量照收；completed
// 记录含归并 toolCall（Index 分片）/usage 缓存双字段/finishReason/首内容时点。
func TestRecordingStreamAggregation(t *testing.T) {
	fake := &fakeRecModel{script: func(n int, send func(*schema.Message)) error {
		send(&schema.Message{Role: schema.Assistant, ReasoningContent: "思考"})
		send(&schema.Message{Role: schema.Assistant, Content: "你"})
		idx := 0
		send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			Index: &idx, ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "get_time", Arguments: `{"a"`},
		}}})
		send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
			Index: &idx, Function: schema.FunctionCall{Arguments: `:1}`},
		}}})
		send(&schema.Message{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{
			FinishReason: "tool_calls",
			Usage: &schema.TokenUsage{
				PromptTokens: 100, CompletionTokens: 7, TotalTokens: 107,
				PromptTokenDetails:      schema.PromptTokenDetails{CachedTokens: 60, CacheWriteTokens: 10},
				CompletionTokensDetails: schema.CompletionTokensDetails{ReasoningTokens: 3},
			},
		}})
		return nil
	}}
	rec := &captureRec{}
	rm := NewRecordingModel(fake, FaceMain, testIdent, rec)

	text, err := drain(mustStream(t, rm))
	if err != nil || text != "你" {
		t.Fatalf("透传应原样：text=%q err=%v", text, err)
	}
	rs := rec.all()
	if len(rs) != 2 {
		t.Fatalf("应两条记录(started+终态)，实得 %d", len(rs))
	}
	if rs[0].Status != RecordStarted || rs[0].Request != nil || rs[0].Attempt != 1 {
		t.Fatalf("started 形状不符：%+v", rs[0])
	}
	done := rs[1]
	if done.Status != RecordCompleted || done.Attempt != 1 || done.Err != nil {
		t.Fatalf("终态形状不符：%+v", done)
	}
	if done.Response == nil || done.Response.Text != "你" || done.Response.ReasoningText != "思考" {
		t.Fatalf("文本聚合不符：%+v", done.Response)
	}
	if len(done.Response.ToolCalls) != 1 || done.Response.ToolCalls[0].Function.Arguments != `{"a":1}` ||
		done.Response.ToolCalls[0].Function.Name != "get_time" {
		t.Fatalf("toolCall 归并不符：%+v", done.Response.ToolCalls)
	}
	u := done.Response.Usage
	if u == nil || u.InputTokens != 100 || u.OutputTokens != 7 || u.ReasoningTokens != 3 ||
		u.CacheReadTokens != 60 || u.CacheWriteTokens != 10 {
		t.Fatalf("usage 聚合不符：%+v", u)
	}
	if done.Response.FinishReason != "tool_calls" {
		t.Fatalf("finishReason 不符：%q", done.Response.FinishReason)
	}
	if done.Request == nil || len(done.Request.Messages) == 0 {
		t.Fatalf("终态应带请求快照：%+v", done.Request)
	}
}

// TestRecordingAttemptFingerprint 同指纹连续失败=同 RequestID 续 attempt；
// 异指纹=新请求；失败记录带 WillRetry（分类+预算推断）。
func TestRecordingAttemptFingerprint(t *testing.T) {
	fake := &fakeRecModel{script: func(n int, send func(*schema.Message)) error {
		if n == 1 {
			return fmt.Errorf("post http://x: connection reset by peer") // transportTexts 命中=可重试
		}
		send(&schema.Message{Role: schema.Assistant, Content: "恢复"})
		return nil
	}}
	rec := &captureRec{}
	rm := NewRecordingModel(fake, FaceMain, testIdent, rec)
	input := []*schema.Message{schema.UserMessage("同一问")}

	if _, err := drain(mustStream(t, rm, input...)); err == nil {
		t.Fatal("第一次应失败")
	}
	if _, err := drain(mustStream(t, rm, input...)); err != nil {
		t.Fatalf("第二次应成功：%v", err)
	}
	rs := rec.all()
	if len(rs) != 4 {
		t.Fatalf("应 4 条（2×started+终态），实得 %d", len(rs))
	}
	id := rs[0].RequestID
	for i, want := range []string{RecordStarted, RecordFailed, RecordStarted, RecordCompleted} {
		if rs[i].Status != want {
			t.Fatalf("第 %d 条状态 %s ≠ %s", i, rs[i].Status, want)
		}
		if rs[i].RequestID != id {
			t.Fatalf("重试应同 RequestID：%d 号 %s", i, rs[i].RequestID)
		}
	}
	if rs[1].Attempt != 1 || rs[2].Attempt != 2 || rs[3].Attempt != 2 {
		t.Fatalf("attempt 序列不符：%d %d %d", rs[1].Attempt, rs[2].Attempt, rs[3].Attempt)
	}
	if !rs[1].WillRetry || rs[1].Err == nil || rs[1].Err.Code == "" {
		t.Fatalf("失败终态应带 WillRetry+分类：%+v", rs[1])
	}

	// 异指纹：新请求新 RequestID、attempt 归 1
	if _, err := drain(mustStream(t, rm, schema.UserMessage("另一问"))); err != nil {
		t.Fatalf("第三次应成功：%v", err)
	}
	rs = rec.all()
	last := rs[len(rs)-1]
	if last.Attempt != 1 || last.RequestID == id {
		t.Fatalf("异指纹应新请求：attempt=%d id=%s", last.Attempt, last.RequestID)
	}
}

// TestRecordingStalled 空闲超时哨兵 → stalled 终态。
func TestRecordingStalled(t *testing.T) {
	fake := &fakeRecModel{script: func(n int, send func(*schema.Message)) error {
		send(&schema.Message{Role: schema.Assistant, Content: "半截"})
		return ErrIdleTimeout
	}}
	rec := &captureRec{}
	rm := NewRecordingModel(fake, FaceMain, testIdent, rec)
	if _, err := drain(mustStream(t, rm)); err == nil {
		t.Fatal("应错误收线")
	}
	rs := rec.all()
	if len(rs) != 2 || rs[1].Status != RecordStalled {
		t.Fatalf("应 stalled 终态：%+v", rs)
	}
}

// TestRecordingGenerate 非流式路径：成功 completed / 失败 failed 带分类。
func TestRecordingGenerate(t *testing.T) {
	rec := &captureRec{}
	rm := NewRecordingModel(&fakeRecModel{}, FaceTitle, testIdent, rec)
	if _, err := rm.Generate(context.Background(), []*schema.Message{schema.UserMessage("标题")}); err != nil {
		t.Fatal(err)
	}
	failing := &fakeRecModel{gen: func() (*schema.Message, error) { return nil, errors.New("boom") }}
	rec2 := &captureRec{}
	rm2 := NewRecordingModel(failing, FaceTitle, testIdent, rec2)
	if _, err := rm2.Generate(context.Background(), []*schema.Message{schema.UserMessage("标题")}); err == nil {
		t.Fatal("应失败")
	}
	rs, rs2 := rec.all(), rec2.all()
	if len(rs) != 2 || rs[1].Status != RecordCompleted || rs[0].Face != FaceTitle {
		t.Fatalf("Generate 成功形状不符：%+v", rs)
	}
	if len(rs2) != 2 || rs2[1].Status != RecordFailed || rs2[1].Err.Code == "" {
		t.Fatalf("Generate 失败形状不符：%+v", rs2)
	}
}

// TestRecordingPanicIsolated 记录器 panic 不影响模型路径（铁律）。
func TestRecordingPanicIsolated(t *testing.T) {
	boom := recFunc(func(ModelCallRecord) { panic("recorder boom") })
	rm := NewRecordingModel(&fakeRecModel{}, FaceMain, testIdent, boom)
	text, err := drain(mustStream(t, rm))
	if err != nil || text != "已处理。" {
		t.Fatalf("panic 应被隔离：text=%q err=%v", text, err)
	}
}

// TestRecordingNilRecorder nil recorder 直通零 panic。
func TestRecordingNilRecorder(t *testing.T) {
	rm := NewRecordingModel(&fakeRecModel{}, FaceMain, testIdent, nil)
	if _, err := drain(mustStream(t, rm)); err != nil {
		t.Fatalf("nil recorder 直通：%v", err)
	}
}

// recFunc 函数适配器。
type recFunc func(ModelCallRecord)

func (f recFunc) Record(c ModelCallRecord) { f(c) }

func mustStream(t *testing.T, m model.BaseModel[*schema.Message], input ...*schema.Message) *schema.StreamReader[*schema.Message] {
	t.Helper()
	if len(input) == 0 {
		input = []*schema.Message{schema.UserMessage("问")}
	}
	sr, err := m.Stream(context.Background(), input)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	return sr
}
