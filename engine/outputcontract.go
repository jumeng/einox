package engine

// 输出契约包装（T8 吸收设计 §1.3-① 修订形态）：探测在装配期（wrapFace
// 入口、mid 包装前——包装链私有字段委托不可穿透，hitl.go:216 DiffProvider
// 同款先例），本包装成为最内层：成功路径按 OutputSchema 校验（失败信封
// {"ok":false,…} 豁免——失败不是 canonical 值）、Render 产出模型可见文本。
// 未实现契约的工具不经此包装（逐字节零变化）。

import (
	"context"
	"encoding/json"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/mid"
)

// outputContractTool 输出契约最内层包装（嵌入接口只提升 Tool 方法集——
// DiffProvider 显式转发，防嵌入隐藏可选接口）。
type outputContractTool struct {
	contract.Tool
	oc contract.OutputContract
}

func (w *outputContractTool) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	res, err := w.Tool.Invoke(ctx, args)
	if err != nil {
		return res, err // 生命周期错误原样上抛（两轨语义不变）
	}
	if isFailureEnvelope(res) {
		return res, nil // 业务失败豁免校验——失败不是 canonical 值
	}
	if verr := mid.ValidateJSON(w.oc.OutputSchema(), res); verr != nil {
		b, _ := json.Marshal(map[string]any{
			"ok": false, "error": "输出不符合声明的 schema：" + verr.Error() + "——请修正输出结构后重试"})
		return b, nil // fail-loud 且模型可自纠（dsh ToolOutputError 形态）
	}
	if r := w.oc.Render(args, res); r != "" {
		return json.RawMessage(r), nil // 模型可见面 = 声明式投影（渲染只写一份）
	}
	return res, nil // Render 空 = 退回原样 JSON（缺省不变）
}

// ApprovalDiff DiffProvider 转发（嵌入隐藏可选接口的防线——审批 diff 面
// 不因契约包装丢失）。
func (w *outputContractTool) ApprovalDiff(args string) string {
	if dp, ok := w.Tool.(contract.DiffProvider); ok {
		return dp.ApprovalDiff(args)
	}
	return ""
}

// isFailureEnvelope 业务失败信封判别（{"ok":false} 形态——errFeed 同款）。
func isFailureEnvelope(res json.RawMessage) bool {
	var probe struct {
		OK *bool `json:"ok"`
	}
	if json.Unmarshal(res, &probe) != nil || probe.OK == nil {
		return false
	}
	return !*probe.OK
}
