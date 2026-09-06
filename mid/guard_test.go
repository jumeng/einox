package mid

// guard 防死循环回归（repeat-tool-reminder + timeout-policy）。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jumeng/einox/contract"
)

// stubTool 计数桩工具（契约形态）。
type stubTool struct {
	name  string
	calls int
}

func (c *stubTool) Info() *contract.ToolInfo { return &contract.ToolInfo{Name: c.name, Desc: "stub"} }

func (c *stubTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	c.calls++
	return json.RawMessage("{}"), nil
}

// errTool 恒错工具（errFeed 信封验证）。
type errTool struct{ msg string }

func (e *errTool) Info() *contract.ToolInfo { return &contract.ToolInfo{Name: "err_tool"} }

func (e *errTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New(e.msg)
}

// blockingTool 阻塞直到 ctx 取消（模拟长工具）。
type blockingTool struct{}

func (b *blockingTool) Info() *contract.ToolInfo { return &contract.ToolInfo{Name: "blocking"} }

func (b *blockingTool) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// repeatNoteOf 结果里的防重提醒（JSON 信封注入 repeat_note 字段——安全审查
// 2026-09-06 改信封化后 ⚠ 前缀仅存于非 JSON 回退形态）。
func repeatNoteOf(t *testing.T, out json.RawMessage) bool {
	t.Helper()
	var m map[string]any
	if json.Unmarshal(out, &m) != nil {
		return false
	}
	_, ok := m["repeat_note"]
	return ok
}

func TestGuardRepeatReminder(t *testing.T) {
	g := Guard(&stubTool{name: "stub_tool"})
	for i := 1; i <= 3; i++ {
		out, err := g.Invoke(context.Background(), json.RawMessage(`{"a":1}`))
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 && repeatNoteOf(t, out) {
			t.Errorf("第 %d 次不应提醒", i)
		}
		if i == 3 && !repeatNoteOf(t, out) {
			t.Error("第 3 次同参调用应提醒")
		}
	}
	// 信封完整性：提醒注入后仍是合法 JSON（ok 语义不被破坏——修的是 ⚠ 前缀
	// 把失败信封拼成纯文本、Digest 回退判 ok=true 的缺陷）
	var m map[string]any
	if json.Unmarshal(json.RawMessage(g2Out(t, g)), &m) != nil {
		t.Fatal("提醒信封应为合法 JSON")
	}
	// 换参数重置计数
	if out, _ := g.Invoke(context.Background(), json.RawMessage(`{"a":2}`)); repeatNoteOf(t, out) {
		t.Error("换参应重置计数")
	}
}

func g2Out(t *testing.T, g contract.Tool) json.RawMessage {
	t.Helper()
	out, err := g.Invoke(context.Background(), json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestGuardRepeatNoteKeepsFailureRed 提醒注入不吞失败语义（安全审查
// 2026-09-06 的靶场景：反复失败的死循环里 Digest 须仍判红）。
func TestGuardRepeatNoteKeepsFailureRed(t *testing.T) {
	g := Guard(&errResultTool{})
	for i := 1; i <= 3; i++ {
		if _, err := g.Invoke(context.Background(), json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := g.Invoke(context.Background(), json.RawMessage(`{}`))
	var m map[string]any
	if json.Unmarshal(out, &m) != nil {
		t.Fatalf("提醒信封应为合法 JSON：%s", out)
	}
	if ok, _, _ := ToolResultDigest(string(out)); ok {
		t.Fatal("失败信封带提醒后 Digest 仍应判红（此前 ⚠ 前缀击穿判红）")
	}
}

// errResultTool 恒回失败信封的工具（零 error——业务失败形态）。
type errResultTool struct{}

func (e *errResultTool) Info() *contract.ToolInfo { return &contract.ToolInfo{Name: "err_result"} }

func (e *errResultTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"ok":false,"error":"业务失败"}`), nil
}

func TestGuardDeadline(t *testing.T) {
	old := ToolDeadline
	ToolDeadline = 50 * time.Millisecond
	t.Cleanup(func() { ToolDeadline = old })
	out, err := Guard(&blockingTool{}).Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("超时应转结果信封不上抛：%v", err)
	}
	if !strings.Contains(string(out), "硬上限") {
		t.Fatalf("应提示硬上限：%s", out)
	}
}

func TestErrFeedEnvelope(t *testing.T) {
	out, err := ErrFeed(&errTool{msg: "参数非法"}).Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("业务错误应转信封：%v", err)
	}
	if !strings.Contains(string(out), "参数非法") || !strings.Contains(string(out), `"ok":false`) {
		t.Fatalf("信封形态异常：%s", out)
	}
	// 挂起哨兵直通（不吞）
	suspend := &contract.Suspend{Info: "card"}
	suspending := &suspendTool{s: suspend}
	if _, err := ErrFeed(suspending).Invoke(context.Background(), json.RawMessage(`{}`)); err != suspend {
		t.Fatalf("Suspend 应原样上抛：%v", err)
	}
	// 取消直通
	canceling := &cancelTool{}
	if _, err := ErrFeed(canceling).Invoke(context.Background(), json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消应直通：%v", err)
	}
}

// suspendTool 返回挂起哨兵。
type suspendTool struct{ s *contract.Suspend }

func (s *suspendTool) Info() *contract.ToolInfo { return &contract.ToolInfo{Name: "ask_user"} }

func (s *suspendTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, s.s
}

// cancelTool 返回取消错误。
type cancelTool struct{}

func (c *cancelTool) Info() *contract.ToolInfo { return &contract.ToolInfo{Name: "canceling"} }

func (c *cancelTool) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	<-cancelCtx.Done()
	return nil, cancelCtx.Err()
}
