// Package tools 是 einox 通用工具面基设：类型化工具构造（InferTool——结构体
// 反射出参数 JSON Schema，schema 推断复用 eino 反射器保证与既有工具面完全
// 同构）与通用工具族（fs/todo/web/时间/问答/命令/补丁，自原 ext/tools 迁入）。
// 统一形态：NewXxxTools(cfg) ([]contract.Tool, error)；失败容忍策略由应用
// 装配层决定；写面审批由基座 hitl 组装期包装，工具内不落审批语义。
package tools

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

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
	params := SchemaOf(po)
	return &typedTool[T, D]{
		info: &contract.ToolInfo{Name: name, Desc: desc, Params: params},
		fn:   fn,
	}, nil
}

// SchemaOf eino ParamsOneOf → contract.Schema（JSON 往返最小子集投影；失败
// nil——schema 是描述面，工具仍可用）。与 einoext 反向转换（adapt.go）成对
// 三处收敛的单点（审查 P2-9：曾三份各写）。
func SchemaOf(po *schema.ParamsOneOf) *contract.Schema {
	if po == nil {
		return nil
	}
	js, err := po.ToJSONSchema()
	if err != nil || js == nil {
		return nil
	}
	b, err := json.Marshal(js)
	if err != nil {
		return nil
	}
	var sc contract.Schema
	if json.Unmarshal(b, &sc) != nil {
		return nil
	}
	return &sc
}

// ParamsOf contract.Schema → eino ParamsOneOf（反向转换，与 SchemaOf 成对；
// nil 入参 = nil 返回）。失败 nil——schema 是描述面，工具仍可用。
func ParamsOf(sc *contract.Schema) *schema.ParamsOneOf {
	if sc == nil {
		return nil
	}
	b, err := json.Marshal(sc)
	if err != nil {
		return nil
	}
	var js jsonschema.Schema
	if json.Unmarshal(b, &js) != nil {
		return nil
	}
	return schema.NewParamsOneOfByJSONSchema(&js)
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
			return nil, contract.ModelArgError(err, reflect.TypeFor[T]())
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
