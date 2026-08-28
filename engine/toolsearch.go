package engine

// H7 动态工具装载策略（toolsearch 接线的装配面定义；中间件本体 = adk
// `middlewares/dynamictool/toolsearch` 官方件，v0.9.13 已含——实现序铁律
// 零自研）。装配序（manager.assemble）：contract.Tool → hitl.WrapTools →
// einoext.Adapt → 静态/动态分流——审批包装在分流上游，ArgsForce 与模式
// 审批对动态工具不豁免（红线拓扑无关）。

// ToolSearchPolicy 动态工具装载策略（engine.Options.ToolSearchPolicy；nil =
// 全量常驻，产品零变化）。名单内工具初始对模型不可见，经 tool_search 元工具
// 检索命中后加载（Run 内 sticky；跨 Run 靠扫描历史 tool_search 结果恢复——
// 故其结果消息在 reduction Exclude 名单禁外置，见 H1-3）。
type ToolSearchPolicy struct {
	// DynamicTools 动态面工具名（检索后可见）；名单外全量常驻
	// （ask_user/todo_write/submit_plan 等会话域件与高频件留常驻是装配纪律）。
	DynamicTools []string
}
