package tools

import "github.com/jumeng/einox/contract"

// diffTool 给工具挂审批卡 diff 载荷（hitl 组卡时探测 ApprovalDiff——
// InferTool 通用件需包一层暴露可选接口）。
type diffTool struct {
	contract.Tool
	diff func(args string) string
}

// DiffToolOf 包装（diff = 参数 JSON → diff 文本；不改 Info/Invoke 行为）。
func DiffToolOf(t contract.Tool, diff func(args string) string) contract.Tool {
	return &diffTool{Tool: t, diff: diff}
}

// ApprovalDiff 审批卡逐行 diff 载荷（可选接口，hitl.WrapTools 探测）。
func (d *diffTool) ApprovalDiff(args string) string { return d.diff(args) }

// SetBehavior 行为标记透传（WithBehavior 就位点——向内层 typedTool 落
// Info，保 ApprovalDiff 可断言不被外包装剥离）。
func (d *diffTool) SetBehavior(b string) {
	if bs, ok := d.Tool.(behaviorSetter); ok {
		bs.SetBehavior(b)
	}
}
