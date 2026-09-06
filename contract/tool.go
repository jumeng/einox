package contract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Tool 业务与通用工具的统一契约。args/result 均为 JSON 字节——传输无关
// （SSE/WS/CLI 由应用层自定），参数 schema 由 ToolInfo 携带。
// 工具实现不落审批语义——写面审批由基座组装期包装（hitl）统一处理；
// 需要挂起交互的工具（ask_user 同构）以 *Suspend 哨兵上抛（见 suspend.go）。
// OutputContract 可选输出契约（T8 吸收设计 §1.3-①——dsh ToolDefinition 的
// output.schema + render 形态）：实现了即被引擎在装配期识别（wrapFace 入口、
// mid 包装前——包装链类型不可穿透，hitl.go:216 DiffProvider 同款先例）。
// 值 / 校验 / 渲染 / UI 四件事正交：canonical 值仍走工具返回，成功路径按
// OutputSchema 校验（mid.ValidateJSON——required 在场校验），Render 产出模型
// 可见文本（回喂/reduction/transcript 共用同一投影——渲染只写一份）。
// 未实现 = 既有行为（自由 JSON 全量回喂）零变化；Render 返回空 = 退回原样。
type OutputContract interface {
	// OutputSchema 结果 JSON Schema（object 形；nil = 只声明渲染不校验）。
	OutputSchema() *Schema
	// Render canonical 值 → 模型可见文本；空返回 = 退回原样 JSON。
	Render(args, result json.RawMessage) string
}

type Tool interface {
	// Info 工具元数据（名称/描述/参数 schema）。
	Info() *ToolInfo
	// Invoke 执行一次调用。返回值为 JSON 字节；业务性失败以 JSON 信封
	// {"ok":false,"error":…} 回喂模型自纠（Go error 会终止整轮）。
	Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

// ToolInfo 工具元数据。
type ToolInfo struct {
	Name   string  `json:"name"`
	Desc   string  `json:"desc"`
	Params *Schema `json:"params,omitempty"` // JSON Schema（object 形）
	// Behavior 行为面标记（UI-B2：过程流「探索/更改/终端」分组的展示语义
	// 数据源；空 = 未标记——交互卡/待办类不入组，前端按工具名兜底）。
	// 与 hitl 写面名单无关：名单 = 审批语义（写才拦），本标记 = 展示语义。
	Behavior string `json:"behavior,omitempty"`
}

// 行为面标记值（ToolInfo.Behavior；工具注册期自declare——内容归业务）。
const (
	BehaviorRead  = "read"  // 探索组：读文件/列表/搜索/取网页
	BehaviorWrite = "write" // 更改组：写文件/改数据/落文档
	BehaviorExec  = "exec"  // 终端组：跑命令/跑脚本（非只读进程执行）
)

// Schema 参数 JSON Schema 的最小子集（object 形）。与 eino ParamsOneOf 经
// JSON 往返互转（einox/einoext 的 Adapt/Bridge 桥）。Minimum/Maximum 供
// mid.Validate 数值边界校验（数值类型约束，声明了才校验）。
type Schema struct {
	Type        string             `json:"type,omitempty"`
	Description string             `json:"description,omitempty"`
	Enum        []string           `json:"enum,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Minimum     *float64           `json:"minimum,omitempty"`
	Maximum     *float64           `json:"maximum,omitempty"`
}

// ModelArgError 把工具参数反序列化错误翻译为面向模型的可行动文案（六层
// 防御层 3′：错误必达模型由 mid.ErrFeed 承担，但 encoding/json 默认文本
// 含结构体名/偏移量等开发者视角信息，对模型是噪声——2026-08-27 盘点定案
// 的基座缺口）。说清三件事：哪个参数、实得什么、应为什么。非参数类错误
// 原样返回（业务自定义 UnmarshalJSON 的友好文案不被改写）。2026-09-06 自
// tools/infer.go 迁入契约面（审查 P3-3：错误也是契约——einoext 桥与工具
// 构造器（InferTool）共用；errors.As 穿透上游 %w 包装链）；owner 可选
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
