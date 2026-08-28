// Package prompts 自研通用提示词资产（业务无关——对位 ext/tools 的提示词
// 层；将来抽仓随 ext 同走）。文件头注释即 license 署名位。
package prompts

import _ "embed"

//go:embed coding.md
var coding string

// Coding 编码工作模式提示词段（BuildInstruction 组装注入——工作区工具面
// 在场时生效）。
func Coding() string { return coding }

//go:embed orchestration.md
var orchestration string

// Orchestration 子代理自主编排指导段（H5-2：何时拆分/派发/聚合——spawn
// 条件措辞，应用不装配子代理时忽略；BuildInstruction 组装注入）。
func Orchestration() string { return orchestration }
