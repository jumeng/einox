package mid

// 入参 schema 校验单元回归：子集约束逐类（type/enum/数值边界/items/嵌套）、
// null 宽容、nil Params 透传、合法调用直达工具；ErrFeed 组合（校验失败转
// 信封回喂）一并覆盖。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jumeng/einox/contract"
)

// schemaOf 构造声明面工具（fn 记录触达）。
func schemaOf(sc *contract.Schema) (contract.Tool, *bool) {
	hit := new(bool)
	return &vstub{info: &contract.ToolInfo{Name: "t", Params: sc},
		fn: func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
			*hit = true
			return json.RawMessage(`{"ok":true}`), nil
		}}, hit
}

type vstub struct {
	info *contract.ToolInfo
	fn   func(context.Context, json.RawMessage) (json.RawMessage, error)
}

func (s *vstub) Info() *contract.ToolInfo { return s.info }
func (s *vstub) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	return s.fn(ctx, args)
}

func f64(v float64) *float64 { return &v }

func TestValidateTypeMismatch(t *testing.T) {
	sc := &contract.Schema{Type: "object", Properties: map[string]*contract.Schema{
		"name": {Type: "string"},
		"n":    {Type: "integer"},
	}}
	w, hit := schemaOf(sc)
	v := Validate(w)
	_, err := v.Invoke(context.Background(), json.RawMessage(`{"name":123}`))
	if err == nil || !strings.Contains(err.Error(), "params.name") {
		t.Fatalf("类型错应带路径报错：%v", err)
	}
	if *hit {
		t.Fatal("校验失败不得触达工具")
	}
	if _, err := v.Invoke(context.Background(), json.RawMessage(`{"n":1.5}`)); err == nil || !strings.Contains(err.Error(), "integer") {
		t.Fatalf("非整数值应报 integer 违规：%v", err)
	}
	if _, err := v.Invoke(context.Background(), json.RawMessage(`{"name":"x","n":3}`)); err != nil {
		t.Fatalf("合法调用应放行：%v", err)
	}
	if !*hit {
		t.Fatal("合法调用应触达工具")
	}
}

func TestValidateEnumAndBounds(t *testing.T) {
	sc := &contract.Schema{Type: "object", Properties: map[string]*contract.Schema{
		"mode": {Enum: []string{"fast", "slow"}},
		"page": {Minimum: f64(1), Maximum: f64(100)},
	}}
	v := Validate(mustTool(t, sc))
	if _, err := v.Invoke(context.Background(), json.RawMessage(`{"mode":"mid"}`)); err == nil || !strings.Contains(err.Error(), "枚举") {
		t.Fatalf("枚举外应报错：%v", err)
	}
	if _, err := v.Invoke(context.Background(), json.RawMessage(`{"page":0}`)); err == nil || !strings.Contains(err.Error(), "下限") {
		t.Fatalf("低于下限应报错：%v", err)
	}
	if _, err := v.Invoke(context.Background(), json.RawMessage(`{"page":101}`)); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超上限应报错：%v", err)
	}
}

func TestValidateItemsAndNested(t *testing.T) {
	sc := &contract.Schema{Type: "object", Properties: map[string]*contract.Schema{
		"tags": {Type: "array", Items: &contract.Schema{Type: "string"}},
		"filt": {Type: "object", Properties: map[string]*contract.Schema{
			"limit": {Type: "integer", Minimum: f64(1)},
		}},
	}}
	v := Validate(mustTool(t, sc))
	if _, err := v.Invoke(context.Background(), json.RawMessage(`{"tags":["a",5]}`)); err == nil || !strings.Contains(err.Error(), "params.tags[1]") {
		t.Fatalf("items 违规应带下标路径：%v", err)
	}
	if _, err := v.Invoke(context.Background(), json.RawMessage(`{"filt":{"limit":0}}`)); err == nil || !strings.Contains(err.Error(), "params.filt.limit") {
		t.Fatalf("嵌套违规应带全路径：%v", err)
	}
}

func TestValidateNullAndNilParams(t *testing.T) {
	// null 宽容（宁漏勿误伤——schema 未声明 nullable 语义）
	sc := &contract.Schema{Type: "object", Properties: map[string]*contract.Schema{
		"name": {Type: "string"},
	}}
	v := Validate(mustTool(t, sc))
	if _, err := v.Invoke(context.Background(), json.RawMessage(`{"name":null}`)); err != nil {
		t.Fatalf("null 应放行：%v", err)
	}
	// nil Params 零开销透传
	plain, hit := schemaOf(nil)
	if _, err := Validate(plain).Invoke(context.Background(), json.RawMessage(`{"任意":1}`)); err != nil {
		t.Fatalf("nil Params 应透传：%v", err)
	}
	if !*hit {
		t.Fatal("透传应触达工具")
	}
}

func TestValidateErrFeedEnvelope(t *testing.T) {
	// 装配序组合：Validate 在 ErrFeed 内层——校验失败转 {"ok":false} 信封
	sc := &contract.Schema{Type: "object", Properties: map[string]*contract.Schema{
		"mode": {Enum: []string{"a"}},
	}}
	raw, _ := schemaOf(sc)
	fed := ErrFeed(Validate(raw))
	out, err := fed.Invoke(context.Background(), json.RawMessage(`{"mode":"b"}`))
	if err != nil {
		t.Fatalf("ErrFeed 应转信封不上抛：%v", err)
	}
	if !strings.Contains(string(out), `"ok":false`) || !strings.Contains(string(out), "枚举") {
		t.Fatalf("信封应含校验错误：%s", out)
	}
}

func mustTool(t *testing.T, sc *contract.Schema) contract.Tool {
	t.Helper()
	w, _ := schemaOf(sc)
	return w
}
