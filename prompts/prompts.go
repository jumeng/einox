// Package prompts 自研通用提示词资产（业务无关——对位 ext/tools 的提示词
// 层；将来抽仓随 ext 同走）。文件头注释即 license 署名位。
package prompts

import (
	_ "embed"
	"strings"
)

//go:embed coding.md
var coding string

// Coding 编码工作模式提示词段（应用 Instruction 拼装注入——工作区工具面
// 在场时生效）。
func Coding() string { return coding }

//go:embed orchestration.md
var orchestration string

// Orchestration 子代理自主编排指导段（H5-2：何时拆分/派发/聚合——spawn
// 条件措辞，应用不装配子代理时忽略；应用 Instruction 拼装注入）。
func Orchestration() string { return orchestration }

//go:embed hitl.md
var hitl string

// Hitl 审批协作指导段（挂起交互工具面〔ask_user/submit_plan〕在场时生效；
// 应用 Instruction 拼装注入——模式措辞与 einox 三档对位）。
func Hitl() string { return hitl }

//go:embed subagents.md
var subagents string

// Subagents 子代理参考模板段（应用装配子代理面时供 spawn Instruction /
// Topology SubAgentSpec 引用；含侦察/实现/审查三档只读约束模板）。
func Subagents() string { return subagents }

// EnvironmentFacts 环境段素材（应用侧采集——基座零发现逻辑红线不动：
// git 状态/挂载区等素材由应用或 assemble 知识层喂入，einox 只提供拼装）。
type EnvironmentFacts struct {
	WorkspaceRoot string   // 工作区根（模型全部读写与命令执行的圈禁域）
	KeepDirs      []string // 持久子区（每行「路径——用途」更友好，由应用组织）
	ProtectDirs   []string // 写保护区（只读不可写）
	DateLine      string   // 当前日期行（推荐 engine.DayHeader(now) 的产出）
	GitStatusLine string   // 挂载业务仓时的分支与未提交摘要（空 = 省略本行）
	SessionMode   string   // manual | plan | auto（空 = 省略本行）
}

// Environment 环境段模板（拼装顺序约定见 assemble/rules.md：环境事实每轮
// 变化最大，放最后利于前缀缓存；无信息行省略，全空返回空串）。
func Environment(f EnvironmentFacts) string {
	var b strings.Builder
	b.WriteString("# 环境\n\n")
	b.WriteString("- 工作区根：" + f.WorkspaceRoot + " —— 你的全部文件读写与命令执行都圈禁在此。\n")
	for _, k := range f.KeepDirs {
		b.WriteString("- 挂载区：" + k + "\n")
	}
	if len(f.ProtectDirs) > 0 {
		b.WriteString("- 写保护区：" + strings.Join(f.ProtectDirs, "、") + "（只读不可写）\n")
	}
	if f.DateLine != "" {
		b.WriteString("- " + f.DateLine + "\n")
	}
	if f.GitStatusLine != "" {
		b.WriteString("- 仓库状态：" + f.GitStatusLine + "\n")
	}
	if f.SessionMode != "" {
		b.WriteString("- 会话档位：" + f.SessionMode + "（含义见「审批协作」段）。\n")
	}
	return b.String()
}
