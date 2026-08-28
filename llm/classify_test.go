package llm

// 分类表回归（网络容错 ③）：DeepSeek 官方错误码面（400/401/402/422/429/5xx）
// + anthropic 状态码面 + net 包裹链 + 哨兵 + 未知保守不重试。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
)

func TestClassifyTable(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantRe   bool
		wantCode string
		wantSub  string // 文案子串
	}{
		// DeepSeek 官方错误码（openai 协议组件 APIError 形态）
		{"DS400 格式错", &einoopenai.APIError{HTTPStatusCode: 400, Message: "bad body"}, false, "SERVER", "400"},
		{"DS401 认证失败", &einoopenai.APIError{HTTPStatusCode: 401, Message: "auth"}, false, "AUTH", "认证"},
		{"DS402 欠费", &einoopenai.APIError{HTTPStatusCode: 402, Message: "Insufficient Balance"}, false, "SERVER", "余额不足"},
		{"DS422 参数错", &einoopenai.APIError{HTTPStatusCode: 422, Message: "bad param"}, false, "SERVER", "422"},
		{"DS429 限速", &einoopenai.APIError{HTTPStatusCode: 429}, true, "RATE_LIMIT", "频率"},
		{"DS500 服务端故障", &einoopenai.APIError{HTTPStatusCode: 500}, true, "SERVER", "故障"},
		{"DS503 繁忙", &einoopenai.APIError{HTTPStatusCode: 503}, true, "SERVER", "503"},
		// 包裹链（组件/fmt %w 包裹下 errors.As 穿透）
		{"包裹链402", fmt.Errorf("调用失败: %w", &einoopenai.APIError{HTTPStatusCode: 402}), false, "SERVER", "余额不足"},
		// anthropic 协议（状态码面；type 字符串不可手工构造，状态码路径覆盖）
		{"anthropic429", &anthropic.Error{StatusCode: 429}, true, "RATE_LIMIT", "频率"},
		{"anthropic401", &anthropic.Error{StatusCode: 401}, false, "AUTH", "认证"},
		// 网络类
		{"空闲哨兵", ErrIdleTimeout, true, "TRANSPORT", "网络异常"},
		{"ctx超时", context.DeadlineExceeded, true, "TRANSPORT", "网络异常"},
		{"dial拒绝", &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}, true, "TRANSPORT", "网络异常"},
		{"url包裹reset", &url.Error{Op: "Post", URL: "https://api/x", Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}, true, "TRANSPORT", "网络异常"},
		{"EOF", io.EOF, true, "TRANSPORT", "连接提前关闭"},
		{"GOAWAY文本", errors.New("http2: server sent GOAWAY and closed the stream"), true, "TRANSPORT", "网络异常"},
		// 取消与未知
		{"ctx取消不重试", context.Canceled, false, "ABORTED", ""},
		{"未知保守不重试", errors.New("boom"), false, "SERVER", "boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.err)
			if got.Retryable != c.wantRe {
				t.Fatalf("Retryable = %v, 期望 %v（%+v）", got.Retryable, c.wantRe, got)
			}
			if got.Code != c.wantCode {
				t.Fatalf("Code = %q, 期望 %q", got.Code, c.wantCode)
			}
			if c.wantSub != "" && !strings.Contains(got.Message, c.wantSub) {
				t.Fatalf("Message = %q, 应含 %q", got.Message, c.wantSub)
			}
		})
	}
}
