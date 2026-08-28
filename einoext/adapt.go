// Package einoext 是 eino 组件双向桥：Adapt 把契约工具（contract.Tool）适配
// 为 eino 工具供引擎组装消费；Bridge 把 eino/eino-ext 组件接入契约面（第三方
// 件依赖与适配归基座）。Suspend 哨兵在此翻译为引擎中断（StatefulInterrupt ×
// GetInterruptState），恢复态经 ctx 注回（contract.WithResumeState）。
package einoext

import (
	"context"
	"encoding/json"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
)

// suspendState 中断保存态外包装（适配层持有唯一注册类型；内层 State 是工具
// 自定形态，gob 注册归各工具）。
type suspendState struct{ State any }

func init() { schema.Register[suspendState]() }

// adaptTool contract.Tool → eino 工具。
type adaptTool struct{ t contract.Tool }

var _ tool.InvokableTool = (*adaptTool)(nil)

func (a *adaptTool) Info(context.Context) (*schema.ToolInfo, error) {
	ci := a.t.Info()
	ti := &schema.ToolInfo{Name: ci.Name, Desc: ci.Desc}
	if ci.Params != nil {
		if b, err := json.Marshal(ci.Params); err == nil {
			var js jsonschema.Schema
			if json.Unmarshal(b, &js) == nil {
				ti.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(&js)
			}
		}
	}
	return ti, nil
}

func (a *adaptTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	// 恢复流：引擎重放本工具时读回中断保存态，经 ctx 注回（工具据此走续跑分支）
	if was, _, st := compose.GetInterruptState[suspendState](ctx); was {
		ctx = contract.WithResumeState(ctx, st.State)
	}
	out, err := a.t.Invoke(ctx, json.RawMessage(args))
	if err != nil {
		if su, ok := err.(*contract.Suspend); ok {
			return "", compose.StatefulInterrupt(ctx, su.Info, suspendState{State: su.State})
		}
		return "", err
	}
	return string(out), nil
}

// Adapt 契约工具批量适配为 eino 工具（引擎组装入口）。
func Adapt(ts []contract.Tool) []tool.BaseTool {
	out := make([]tool.BaseTool, 0, len(ts))
	for _, t := range ts {
		out = append(out, &adaptTool{t: t})
	}
	return out
}

// bridgedTool eino 工具 → contract.Tool（eino-ext 第三方件入面）。
type bridgedTool struct{ t tool.InvokableTool }

var _ contract.Tool = (*bridgedTool)(nil)

func (b *bridgedTool) Info() *contract.ToolInfo {
	info, err := b.t.Info(context.Background())
	if err != nil || info == nil {
		return &contract.ToolInfo{}
	}
	ci := &contract.ToolInfo{Name: info.Name, Desc: info.Desc}
	if js, err := info.ParamsOneOf.ToJSONSchema(); err == nil && js != nil {
		if raw, err := json.Marshal(js); err == nil {
			var sc contract.Schema
			if json.Unmarshal(raw, &sc) == nil {
				ci.Params = &sc
			}
		}
	}
	return ci
}

func (b *bridgedTool) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	out, err := b.t.InvokableRun(ctx, string(args))
	if err != nil {
		// 上游组件的参数反序列化错误常裹多层 Go 噪声（结构体名/包装链），
		// 统一经 ModelArgError 翻译为模型可行动文案（errors.As 穿透 %w）
		return nil, tools.ModelArgError(err)
	}
	return json.RawMessage(out), nil
}

// Bridge eino 工具批量接入契约面（非 Invokable 形态跳过——现有面均为 Invokable）。
func Bridge(ts []tool.BaseTool) []contract.Tool {
	out := make([]contract.Tool, 0, len(ts))
	for _, t := range ts {
		if it, ok := t.(tool.InvokableTool); ok {
			out = append(out, &bridgedTool{t: it})
		}
	}
	return out
}
