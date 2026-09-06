package engine

// usage 估算与常驻上下文预算（B1/H8-1）：estTokens 无分词器启发式、消息/
// schema/上下文分类估算（usage 事件载荷与整形节省注记）、常驻面超限告警
// （checkContextBudget，会话内只发一次）。

import (
	"fmt"
	"log"

	"github.com/cloudwego/eino/schema"

	"encoding/json"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// ctxEstimates 上下文分类估算（usage 事件的分类三项来源；每轮 Run 计算一次）。
// messages = 整形后出站口径（H8-1）；saved = 原始口径与整形口径差额（整形
// 节省注记——reasoning 剥离 + 空壳剔除的量化，不含 reduction 外置/摘要）。
type ctxEstimates struct{ instruction, tools, messages, saved int }

// estTokens 无分词器的字符启发式：CJK ≈ 1 token/字，其余 ≈ 1/4。
func estTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2e80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + other/4
}

// msgTokenEst 单条消息 token 估算（文本 + reasoning + 角色开销 8 + tool_call
// 名与参数）。触发判定（shapedTokenCounter）/usage 上报（estimateContext）/
// 摘要修剪（trimToBudget）三面同源单点——曾三处各写且 trimToBudget 漏计
// ToolCalls，与「同一计数器，不引入第二口径」的注释承诺失实（审查 P1-4）。
func msgTokenEst(m *schema.Message) int {
	if m == nil {
		return 0
	}
	n := estTokens(msgTextOf(m)) + estTokens(m.ReasoningContent) + 8 // 8 ≈ 角色开销
	for _, tc := range m.ToolCalls {
		n += estTokens(tc.Function.Name) + estTokens(tc.Function.Arguments)
	}
	return n
}

// estimateContext 上下文分类估算（Run 在泵前已把本轮用户消息入史——中断保险，
// CloneHistory 天然含本轮，无须另计）。工具面口径（B1 补齐）：业务面 + 进程件
// + 会话域件 + recall + spawn，名+描述+参数 schema JSON 均计——会话域件恒
// 常驻故须计入（此前全漏）；toolsearch 名单内工具不计（动态装载正是瘦身
// 手段，只有常驻面计费；分流与 assemble 同源名单）。
func (m *Manager) estimateContext(s *session.Session) ctxEstimates {
	brief := m.briefOf(s)
	est := ctxEstimates{instruction: estTokens(m.Opt.Instruction(brief))}
	dyn := dynamicToolSet(m.Opt.ToolSearchPolicy)
	addFace := func(ts []contract.Tool) {
		for _, t := range ts {
			if info := t.Info(); info != nil && !dyn[info.Name] {
				est.tools += estTokens(info.Name) + estTokens(info.Desc) + schemaTokens(info.Params)
			}
		}
	}
	if m.Opt.Tools != nil {
		addFace(m.Opt.Tools(brief)) // 会话面：随 Owner/SID 裁剪后的真实业务面
	}
	if m.Opt.ProcessTools != nil {
		addFace(m.Opt.ProcessTools())
	}
	if sts, err := m.sessionTools(s); err == nil { // 会话域件实际面（族裁剪后）；构造失败随 assemble 报，此处不计
		addFace(sts)
	}
	if m.Opt.Recall {
		if rt, err := newRecallTool(m.reg, s); err == nil {
			addFace([]contract.Tool{rt})
		}
	}
	if m.Opt.SubAgents != nil { // spawn 面走静态估算（构造工具本体需建模板 agent——重）
		est.tools += estTokens(spawnToolName) + estTokens(spawnDesc) + spawnSchemaTokens()
	}
	// H8-1 口径：est_messages = 整形后出站视图（真实发送面——与 H1 TokenCounter
	// 同规则函数 llm.ShapeMessages）；saved = 原始口径差额（「整形节省」注记）。
	history := s.CloneHistory()
	shaped := llm.ShapeMessages(history)
	for _, msg := range shaped {
		est.messages += msgTokenEst(msg)
	}
	for _, msg := range history {
		est.saved += msgTokenEst(msg)
	}
	est.saved -= est.messages
	return est
}

// schemaTokens 参数 schema 的 JSON 形估算（nil = 0；marshal 失败容错 0——
// 估算是治理信号不是精确账）。
func schemaTokens(sc *contract.Schema) int {
	if sc == nil {
		return 0
	}
	if b, err := json.Marshal(sc); err == nil {
		return estTokens(string(b))
	}
	return 0
}

// emitUsage 流末 usage chunk 到达即发（每轮模型调用一次，react 多轮后值
// 覆盖——最后一条 = 最终上下文规模）。Record 落事件流 → 刷新回放可恢复。
// spawnID 非空 = 子代理面用量上卷（B2：估算四项传零——子面无 estimateContext；
// 消费侧按 SpawnID 归组聚合）。
func (m *Manager) emitUsage(s *session.Session, fn emitFn, u *schema.TokenUsage, est ctxEstimates, spawnID string) {
	if u == nil || u.PromptTokens <= 0 {
		return
	}
	m.emit(s, fn, contract.EvUsage, contract.UsageOut{
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
		TotalTokens:    u.TotalTokens,
		SpawnID:        spawnID,
		EstInstruction: est.instruction, EstTools: est.tools, EstMessages: est.messages,
		EstSaved: est.saved, // 整形节省注记（H8-1；原始-整形差额，>0 才有意义）
	})
}

// envContextBudget env 覆盖的常驻上下文预算（0 = 未设；显式 Options 值优先）。
var envContextBudget int

// contextBudgetOf 生效预算（显式 Options 值优先，次 env；0 = 关）。
func (m *Manager) contextBudgetOf() int {
	if m.Opt.ContextBudget > 0 {
		return m.Opt.ContextBudget
	}
	return envContextBudget
}

// checkContextBudget 常驻面超限告警（B1）：Instruction+常驻工具面合计超线即发
// harness_note（Kind: budget）+ 日志，不阻断运行（大工具面配 toolsearch 就是
// 合法超标场景）；会话内只发一次——判定扫 Events 既有同 Kind note（免持久化
// 标记位：Reattach 后 Events 恢复即含旧告警，跨重启天然不重发；盘面重建的
// Data 是 map 形态，两形态同判）。
func (m *Manager) checkContextBudget(s *session.Session, fn emitFn, est ctxEstimates) {
	budget := m.contextBudgetOf()
	if budget <= 0 {
		return
	}
	resident := est.instruction + est.tools
	if resident <= budget {
		return
	}
	for _, ev := range s.SnapshotEvents() {
		if ev.Event != contract.EvHarnessNote {
			continue
		}
		switch d := ev.Data.(type) {
		case contract.HarnessNote:
			if d.Kind == "budget" {
				return
			}
		case map[string]any:
			if d["kind"] == "budget" {
				return
			}
		}
	}
	m.emit(s, fn, contract.EvHarnessNote, contract.HarnessNote{
		Kind:  "budget",
		Title: "常驻上下文超预算",
		Detail: fmt.Sprintf("Instruction ≈%d + 常驻工具面 ≈%d = ≈%d token，超预算线 %d（estTokens 启发式口径；瘦身：精简工具描述 / SessionToolsOff 裁族 / ToolSearchPolicy 动态装载）",
			est.instruction, est.tools, resident, budget),
	})
	log.Printf("einox: 会话 %s 常驻上下文 ≈%d token 超预算 %d（instruction %d + tools %d）",
		s.SID, resident, budget, est.instruction, est.tools)
}
