package mid

// Validate 工具入参 schema 校验（契约层包装）：按 ToolInfo.Params 声明的
// 约束子集校验模型给的 JSON 参数——type / enum / minimum / maximum /
// items / properties 递归，违规聚合为带字段路径的单条错误。挂在 ErrFeed
// 内层（hitl.WrapTools 装配序 Guard(ErrFeed(Validate(t)))）：校验失败是
// Go error → ErrFeed 转 {"ok":false} 信封回喂模型自纠（校验错误必须达模型，
// 上抛终止整轮即不可调整）。
//
// 子集纪律（dsh/nanobot 同款，不引标准校验库）：只校验声明了的约束，未声明
// 不设防；null 视为满足任何 type（宽松——schema 未声明 nullable 语义，宁
// 漏勿误伤）。
//
// 不校验 required（2026-08-29 实锚裁决）：eino 反射生成的 schema 把全部非
// 指针字段标 required（指针+omitempty 被排除），与「零值可接受」的工具语义
// 普遍不符（ask_user options.value 缺省即用 label——既有测试即此形态，
// required 校验必误伤）；缺必填由 typed 反序列化类型面与工具自身语义兜底。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/jumeng/einox/contract"
)

// Validate 参数校验包装（Params 为 nil 的工具零开销透传）。
func Validate(t contract.Tool) contract.Tool { return &validateTool{t: t} }

type validateTool struct{ t contract.Tool }

func (w *validateTool) Info() *contract.ToolInfo { return w.t.Info() }

// Invoke 先校验后执行；校验失败直接返回错误（不经工具——参数不合法时
// 工具不该被触达）。
func (w *validateTool) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if sc := w.t.Info(); sc != nil && sc.Params != nil {
		var v any
		if err := json.Unmarshal(args, &v); err != nil {
			return nil, fmt.Errorf("参数不是合法 JSON：%w", err)
		}
		if errs := validateValue("params", v, sc.Params); len(errs) > 0 {
			return nil, fmt.Errorf("参数校验未过：%s", strings.Join(errs, "；"))
		}
	}
	return w.t.Invoke(ctx, args)
}

// validateValue 单值递归校验（path = 字段路径，如 params.foo / params.foos[0].bar）。
func validateValue(path string, v any, sc *contract.Schema) []string {
	if sc == nil {
		return nil
	}
	var errs []string
	if v != nil && sc.Type != "" && !typeMatches(v, sc.Type) {
		errs = append(errs, fmt.Sprintf("%s 必须是 %s（实际 %s）", path, sc.Type, jsonTypeOf(v)))
	}
	if len(sc.Enum) > 0 {
		if s, ok := v.(string); ok && !enumHas(sc.Enum, s) {
			errs = append(errs, fmt.Sprintf("%s 取值 %q 不在枚举内（可选：%s）", path, s, strings.Join(sc.Enum, "/")))
		}
	}
	if n, ok := v.(float64); ok { // JSON 数值解码恒为 float64
		if sc.Minimum != nil && n < *sc.Minimum {
			errs = append(errs, fmt.Sprintf("%s = %v 低于下限 %v", path, n, *sc.Minimum))
		}
		if sc.Maximum != nil && n > *sc.Maximum {
			errs = append(errs, fmt.Sprintf("%s = %v 超过上限 %v", path, n, *sc.Maximum))
		}
	}
	switch val := v.(type) {
	case []any:
		if sc.Items != nil {
			for i, item := range val {
				errs = append(errs, validateValue(fmt.Sprintf("%s[%d]", path, i), item, sc.Items)...)
			}
		}
	case map[string]any:
		if len(sc.Properties) > 0 {
			names := make([]string, 0, len(sc.Properties))
			for k := range sc.Properties {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				if item, ok := val[k]; ok {
					errs = append(errs, validateValue(path+"."+k, item, sc.Properties[k])...) // 递归校验
				}
			}
		}
	}
	return errs
}

// typeMatches JSON 值与声明类型匹配（integer 要整数值；null 已在调用方放行）。
func typeMatches(v any, typ string) bool {
	switch typ {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		n, ok := v.(float64)
		return ok && n == math.Trunc(n)
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "null":
		return v == nil
	default:
		return true // 未知类型声明不设防（子集纪律）
	}
}

// jsonTypeOf 实际类型的 JSON 名（错误文案用）。
func jsonTypeOf(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

func enumHas(enum []string, s string) bool {
	for _, e := range enum {
		if e == s {
			return true
		}
	}
	return false
}
