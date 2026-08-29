// Package einoext 是 eino 组件双向桥：Adapt 把契约工具（contract.Tool）适配
// 为 eino 工具供引擎组装消费；Bridge 把 eino/eino-ext 组件接入契约面（第三方
// 件依赖与适配归基座）。Suspend 哨兵在此翻译为引擎中断（StatefulInterrupt ×
// GetInterruptState），恢复态经 ctx 注回（contract.WithResumeState）。
package einoext

import (
	"context"
	"encoding/json"
	"fmt"

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

// RegisterSuspendState 注册业务挂起态类型（*contract.Suspend 的 Info/State
// 过 checkpoint 的 gob 序列化要求——接口字段持有的具体类型必须注册，eino
// schema/serialization.go 明文）。等价转发 eino schema.Register——业务勿
// 为此直接 import eino（业务 0 import eino 承诺）。业务包 init() 中对每个
// 用作 Suspend 载荷的具体类型调用一次；未注册的挂起在 checkpoint 落盘时
// 即报 gob 未注册错误（非跨进程才暴露）。
func RegisterSuspendState[T any]() { schema.Register[T]() }

// adaptTool contract.Tool → eino 工具。
type adaptTool struct {
	t    contract.Tool
	name string // 构造期预存（panic 信封用，见 invoke 注释）
}

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
	out, err := a.invoke(ctx, args)
	if err != nil {
		if su, ok := err.(*contract.Suspend); ok {
			return "", compose.StatefulInterrupt(ctx, su.Info, suspendState{State: su.State})
		}
		return "", err
	}
	return string(out), nil
}

// invoke 带 panic 隔离的执行体：包装链上任一层（业务工具/审批包装/ToolWrap）
// panic 都在此外层单点收敛——转 {"ok":false} 信封回喂模型自纠（codex 对位 =
// panic 升级 Fatal 丢自纠机会；dsh/nanobot 均为单工具 isError 结果）。Go error
// 原样上抛（ErrFeed 在内层已消费过可恢复类，走到这里的 error 就是应上抛的）。
// 工具自起 goroutine 的 panic 兜不住（语言边界）。信封用的工具名构造期预存
// ——不在 recover 内调 Info()：工具 panic 时可能持有与 Info 共用的锁，重入
// 即死锁（名字是装配寻址键，构造后不变）。
func (a *adaptTool) invoke(ctx context.Context, args string) (out json.RawMessage, err error) {
	defer func() {
		if r := recover(); r != nil {
			b, _ := json.Marshal(map[string]any{"ok": false, "error": fmt.Sprintf("工具 %q 执行 panic：%v（换参数重试或改用其他方式）", a.name, r)})
			out, err = b, nil
		}
	}()
	return a.t.Invoke(ctx, json.RawMessage(args))
}

// Adapt 契约工具批量适配为 eino 工具（引擎组装入口）。
func Adapt(ts []contract.Tool) []tool.BaseTool {
	out := make([]tool.BaseTool, 0, len(ts))
	for _, t := range ts {
		name := ""
		if info := t.Info(); info != nil {
			name = info.Name
		}
		out = append(out, &adaptTool{t: t, name: name})
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
