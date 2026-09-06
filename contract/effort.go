package contract

// 思考档（effort）归一的唯一权威（会话域契约语义——四档封闭词表）。
// 2026-09-06 自 llm 包迁入（审查 P3-3：session 为两行调用背一条包级依赖边；
// 语义本就写在 UserPrefs 注释里，函数体落契约面后 session→llm 整条边消失，
// llm 内部调用点同改）。调用方：会话读侧（恢复/回显）、API 校验、模型工厂。

// NormalizeEffort 思考档归一：off | low | high | max（2026-08-31 关档回归
// 四档——机制层能力，模型是否真支持关由端点定）。旧值兼容——升级前用户
// 偏好与存量会话快照存的是 on/off（旧「开」即 enabled+effort max）：on/
// max → max，off → off（关档回归后恢复本义），其余（""/未知）→ 默认 low。
// 全链路唯一权威：四档外的值任何一环都不外流。
func NormalizeEffort(effort string) string {
	switch effort {
	case "off":
		return "off"
	case "high":
		return "high"
	case "on", "max":
		return "max"
	default:
		return "low"
	}
}
