// Package tools 是 einox 通用工具面基设：类型化工具构造（InferTool——结构体
// 反射出参数 JSON Schema，schema 推断复用 eino 反射器保证与既有工具面完全
// 同构）与通用工具族（fs/todo/web/时间/问答/命令/补丁，自原 ext/tools 迁入）。
// 统一形态：NewXxxTools(cfg) ([]contract.Tool, error)；失败容忍策略由应用
// 装配层决定；写面审批由基座 hitl 组装期包装，工具内不落审批语义。
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/cloudwego/eino/components/tool/utils"

	"github.com/jumeng/einox/contract"
)

// InferTool 类型化工具构造：In 结构体反射出参数 schema（json/jsonschema tag
// 语义与 eino utils.InferTool 完全一致——内部即其反射器），fn 返回值 JSON
// 序列化为工具结果。挂起 = fn 返回 *contract.Suspend（适配层转引擎中断）。
func InferTool[T, D any](name, desc string, fn func(ctx context.Context, in T) (D, error)) (contract.Tool, error) {
	po, err := utils.GoStruct2ParamsOneOf[T]()
	if err != nil {
		return nil, err
	}
	var params *contract.Schema
	if js, err := po.ToJSONSchema(); err == nil && js != nil {
		if b, err := json.Marshal(js); err == nil {
			var sc contract.Schema
			if json.Unmarshal(b, &sc) == nil {
				params = &sc
			}
		}
	}
	return &typedTool[T, D]{
		info: &contract.ToolInfo{Name: name, Desc: desc, Params: params},
		fn:   fn,
	}, nil
}

// typedTool InferTool 产物。
type typedTool[T, D any] struct {
	info *contract.ToolInfo
	fn   func(ctx context.Context, in T) (D, error)
}

func (t *typedTool[T, D]) Info() *contract.ToolInfo { return t.info }

// WithBehavior 行为面标记（UI-B2：展示分组语义——值用 contract.Behavior*
// 常量；不改写已标记的 Info，空 behavior 原样返回。与 hitl 写面名单无关：
// 名单 = 审批语义，本标记 = 展示语义）。优先就地写具体工具的 Info——
// 接口嵌入包装会剥掉 ApprovalDiff 这类可选接口（hitl 组卡探测依赖），
// 故仅无 SetBehavior 能力的工具退回包装形态。须在 hitl.WrapTools 之前用。
type behaviorSetter interface{ SetBehavior(string) }

func WithBehavior(t contract.Tool, behavior string) contract.Tool {
	if behavior == "" {
		return t
	}
	if bs, ok := t.(behaviorSetter); ok {
		bs.SetBehavior(behavior)
		return t
	}
	return behaviorTool{Tool: t, behavior: behavior}
}

// SetBehavior 就地标记（typedTool 专属——Info 指针共享，审批/diff 包装层
// 的 Info 透传天然可见）。
func (t *typedTool[T, D]) SetBehavior(b string) {
	if t.info != nil && t.info.Behavior == "" {
		t.info.Behavior = b
	}
}

// behaviorTool 无 SetBehavior 能力工具的兜底包装（Info 透传+标记覆写）。
type behaviorTool struct {
	contract.Tool
	behavior string
}

func (b behaviorTool) Info() *contract.ToolInfo {
	info := b.Tool.Info()
	if info == nil || info.Behavior != "" {
		return info
	}
	c := *info
	c.Behavior = b.behavior
	return &c
}

func (t *typedTool[T, D]) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var in T
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, ModelArgError(err, reflect.TypeFor[T]())
		}
	}
	out, err := t.fn(ctx, in)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ModelArgError 把工具参数反序列化错误翻译为面向模型的可行动文案（六层
// 防御层 3′：错误必达模型由 mid.ErrFeed 承担，但 encoding/json 默认文本
// 含结构体名/偏移量等开发者视角信息，对模型是噪声——2026-08-27 盘点定案
// 的基座缺口）。说清三件事：哪个参数、实得什么、应为什么。非参数类错误
// 原样返回（业务自定义 UnmarshalJSON 的友好文案不被改写）。导出供 einoext
// Bridge 等其他参数错误源头复用（errors.As 穿透上游 %w 包装链）；owner 可选
// 传入参数结构体类型，用于核对可选性（encoding/json 报错的 Type 已解引用
// 指针，元素类型无法自判可选）。
func ModelArgError(err error, owner ...reflect.Type) error {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		path := te.Field
		if te.Struct != "" && strings.HasPrefix(path, te.Struct+".") {
			path = strings.TrimPrefix(path, te.Struct+".") // 防御：Field 偶带结构体名前缀
		}
		if path == "" {
			path = "参数整体"
		}
		msg := fmt.Sprintf("参数类型不符：%s 应为%s，实得%s——请按参数 schema 修正后重试",
			path, typeHint(te.Type), valueHint(te.Value))
		optional := te.Type.Kind() == reflect.Ptr
		if !optional && len(owner) == 1 {
			optional = fieldOptional(owner[0], path)
		}
		if optional {
			msg += "（该参数可选，不需要时省略此键，勿传空值）"
		}
		return errors.New(msg)
	}
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Errorf("参数 JSON 语法错误（字节位置 %d）——检查引号/逗号/括号配对后重试", se.Offset)
	}
	return err
}

// fieldOptional 沿点分路径回结构体声明核对叶子字段是否指针（可选参数）。
// te.Field 报的是 json 名（非 Go 字段名），须按标签匹配；中段遇到非结构体
// （数组/映射等）或字段不存在即判非可选。
func fieldOptional(owner reflect.Type, path string) bool {
	cur := owner
	for _, seg := range strings.Split(path, ".") {
		for cur.Kind() == reflect.Ptr {
			cur = cur.Elem()
		}
		if cur.Kind() != reflect.Struct {
			return false
		}
		f, ok := fieldByJSONName(cur, seg)
		if !ok {
			return false
		}
		cur = f.Type
	}
	return cur.Kind() == reflect.Ptr
}

// fieldByJSONName 按 json 标签名找结构体字段（无标签退回字段名）。
func fieldByJSONName(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag == name || (tag == "" && f.Name == name) {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// typeHint Go 类型 → 模型可读的期望形态（指针解引用后按 Kind 判）。
func typeHint(t reflect.Type) string {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return "布尔值（true/false）"
	case reflect.String:
		return "字符串"
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.String {
			return `字符串数组（如 ["a","b"]）`
		}
		return "数组"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "数字"
	case reflect.Map, reflect.Struct:
		return "对象"
	default:
		return t.String()
	}
}

// valueHint UnmarshalTypeError.Value（JSON 实得值描述）→ 中文。
func valueHint(v string) string {
	if strings.HasPrefix(v, "number") { // "number"、带值的 "number -5"
		return "数字"
	}
	switch v {
	case "string":
		return "字符串"
	case "bool":
		return "布尔值"
	case "array":
		return "数组"
	case "object":
		return "对象"
	default:
		return v
	}
}
