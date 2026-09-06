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

// TestValidateJSONNestedRequired 输出面 required 递归（第三轮审查 P3 补齐）：
// 嵌套 object 缺 required 与数组元素内缺 required 都必须报——输出 schema 是
// 手写显式声明，嵌套必填是自然形态；入参面豁免裁决不动（Validate 不校验
// required，含嵌套）。
func TestValidateJSONNestedRequired(t *testing.T) {
	sc := &contract.Schema{Type: "object", Required: []string{"rows", "meta"},
		Properties: map[string]*contract.Schema{
			"rows": {Type: "array", Items: &contract.Schema{Type: "object", Required: []string{"v"}}},
			"meta": {Type: "object", Required: []string{"gen"},
				Properties: map[string]*contract.Schema{"gen": {Type: "string"}}},
		}}
	err := ValidateJSON(sc, json.RawMessage(`{"rows":[{"v":1},{}],"meta":{}}`))
	if err == nil || !strings.Contains(err.Error(), "meta.gen") || !strings.Contains(err.Error(), "rows[1].v") {
		t.Fatalf("嵌套 required 缺失应逐路径报（meta.gen、rows[1].v），实得 %v", err)
	}
	if err := ValidateJSON(sc, json.RawMessage(`{"rows":[{"v":1}],"meta":{"gen":"x"}}`)); err != nil {
		t.Fatalf("全在场应过：%v", err)
	}
	// required 深藏于未列 required 的子对象（外层只查 rows 在场，rows 元内查 v）
	deep := &contract.Schema{Type: "object",
		Properties: map[string]*contract.Schema{
			"rows": {Type: "array", Items: &contract.Schema{Type: "object", Required: []string{"v"}}}}}
	if err := ValidateJSON(deep, json.RawMessage(`{"rows":[{}]}`)); err == nil || !strings.Contains(err.Error(), "rows[0].v") {
		t.Fatalf("深藏 required 应报：%v", err)
	}
	// 未显式标 type:"object" 的子结构同样深查（第四轮裁定：递归门槛与
	// validateValue 对齐——map 值即查 required，不要求子 schema 标型）
	untyped := &contract.Schema{Type: "object",
		Properties: map[string]*contract.Schema{
			"meta": {Required: []string{"gen"}, Properties: map[string]*contract.Schema{"gen": {Type: "string"}}}}}
	if err := ValidateJSON(untyped, json.RawMessage(`{"meta":{}}`)); err == nil || !strings.Contains(err.Error(), "meta.gen") {
		t.Fatalf("未标型子对象的 required 应深查：%v", err)
	}
	// 入参豁免不动：同 schema 作 Params、缺全部 required 仍直通工具
	tool, hit := schemaOf(sc)
	if _, err := Validate(tool).Invoke(context.Background(), json.RawMessage(`{"rows":[{}]}`)); err != nil {
		t.Fatalf("入参面 required 豁免裁决不得被输出面改动破坏：%v", err)
	}
	if !*hit {
		t.Fatal("豁免面合法调用应直达工具")
	}
}
