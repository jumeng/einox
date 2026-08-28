package tools

// ModelArgError 翻译器测试（六层防御层 3′：unmarshal 错误的模型向文案——
// 说清哪个参数/实得什么/应为什么，可选参数给省略指路；语法错误给位置；
// 业务自定义 UnmarshalJSON 的错误直通不改写）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type argNestIn struct {
	Inner struct {
		Starred *bool `json:"starred"`
	} `json:"inner"`
	Flag *bool    `json:"flag"`
	Tags []string `json:"tags"`
}

func mkArgTool(t *testing.T) contractTool {
	t.Helper()
	tl, err := InferTool[argNestIn, map[string]any]("argtest", "测试工具",
		func(_ context.Context, _ argNestIn) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

// contractTool 测试内简写（避免引 contract 包别处重名）。
type contractTool = interface {
	Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

func TestModelArgErrorTranslation(t *testing.T) {
	tl := mkArgTool(t)
	ctx := context.Background()

	// 可选布尔收到字符串：路径 + 期望形态 + 实得形态 + 省略指路
	_, err := tl.Invoke(ctx, json.RawMessage(`{"flag":"yes"}`))
	if err == nil || !strings.Contains(err.Error(), "flag 应为布尔值") ||
		!strings.Contains(err.Error(), "实得字符串") || !strings.Contains(err.Error(), "省略此键") {
		t.Fatalf("布尔翻译不符：%v", err)
	}
	// 字符串数组收到字符串
	_, err = tl.Invoke(ctx, json.RawMessage(`{"tags":"a"}`))
	if err == nil || !strings.Contains(err.Error(), "tags 应为字符串数组") {
		t.Fatalf("数组翻译不符：%v", err)
	}
	// 嵌套路径直达叶子（不含 Go 结构体名）
	_, err = tl.Invoke(ctx, json.RawMessage(`{"inner":{"starred":123}}`))
	if err == nil || !strings.Contains(err.Error(), "inner.starred") ||
		!strings.Contains(err.Error(), "实得数字") || strings.Contains(err.Error(), "argNestIn") {
		t.Fatalf("嵌套路径翻译不符：%v", err)
	}
	// 语法错误给位置与检查方向
	_, err = tl.Invoke(ctx, json.RawMessage(`{bad`))
	if err == nil || !strings.Contains(err.Error(), "语法错误") {
		t.Fatalf("语法错误翻译不符：%v", err)
	}
	// 正常调用不受影响
	out, err := tl.Invoke(ctx, json.RawMessage(`{"flag":true}`))
	if err != nil || !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("正常路径受损：%v %s", err, out)
	}
}

func TestModelArgErrorPassthrough(t *testing.T) {
	// 业务错误原样直通（不改写非参数类错误）
	want := errors.New("业务自定义文案")
	tl, err := InferTool[argNestIn, map[string]any]("argbiz", "d",
		func(_ context.Context, _ argNestIn) (map[string]any, error) { return nil, want })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Invoke(context.Background(), json.RawMessage(`{}`)); err != want {
		t.Fatalf("业务错误应直通：%v", err)
	}
	// %w 包装链穿透（eino 上游组件会把 unmarshal 错误包一层再抛）
	var v struct {
		Code string `json:"code"`
	}
	raw := json.RawMessage(`{"code":123}`)
	wrapped := json.Unmarshal(raw, &v)
	tr := ModelArgError(fmt.Errorf("extract argument fail: %w", wrapped))
	if tr == nil || !strings.Contains(tr.Error(), "code 应为字符串") || !strings.Contains(tr.Error(), "实得数字") {
		t.Fatalf("包装链应穿透翻译：%v", tr)
	}
}
