package engine

// summarization 接线（Phase H3-1；方案 = findings/2026-08-26-h3-summarization-plan.md）：
// 中间件本体 = adk middlewares/summarization（原生白拿——触发/重试/摘要/finalize），
// einox 自研仅三件补差：
//   ①防超窗修剪（GenModelInput 覆写——摘要请求输入裁进 80% 窗口预算，
//     自尾向前保留、起点回退 user 边界不切 tool 配对；codex trim 同语义）
//   ②清窗兜底（包装层：摘要降级链耗尽不外抛——state 改自最后 user 起的
//     尾段新窗，运行继续；codex token-budget compact 语义）
//   ③transcript 落域（触发即写 sessions/<sid>/spill/transcript.txt——成功
//     路径 Callback 写压缩前全文、清窗兜底写原文；溯源路径经 Finalize 包装
//     注入摘要信封，模型经 read_file spill/ 路由可取回，零新增机制）
// 触发 70% 窗口、挂 reduction 之后（计数看 clear 后状态——语义压缩只兜机械
// 压缩兜不住的文本膨胀）；TokenCounter 用 shapedTokenCounter（整形后出站
// 口径，与 reduction 同源）。存储保真：state.Messages 替换为新切片，session
// 历史原文不动（summarization.go :335-358 实核）；einox 全量重放 → 阈上会话
// 每 Run 重摘要一次（跨 Run 前缀稳定，KV-cache 高命中摊薄——已接受成本，
// 优化位=会话域摘要缓存+覆盖水位，本 Phase 不做）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

const (
	summarizeTriggerPct = 70 // 触发阈值：70% 窗口（reduction 30% 恒先行动）
	summarizeInputPct   = 80 // 摘要请求输入预算：防超窗
)

// transcriptNote 摘要信封溯源段（Finalize 包装注入——adk 的 TranscriptFilePath
// 与 Finalize 互斥，Finalize 设置时该字段被架空，路径只能经包装进信封）。
const transcriptNote = "\n\n完整对话记录已存档：spill/transcript.txt（需要被压缩内容的原文细节时，用 read_file 读取该路径）"

// summarizeInstruction 摘要指令（任务水位/todo/结论/全景/未决——断链防护）。
const summarizeInstruction = `请把以上完整对话历史压缩为一份结构化摘要，作为后续对话的上下文。必须保留：
- 任务目标与当前进度水位（进行到哪一步、下一步是什么）
- todo 清单当前状态（未完成项逐一保留）
- 关键结论与已做决策（各附一句理由）
- 涉及的 issue/需求/文档全景（编号与标题）
- 未决事项与等待中的问题
丢失以上任何一类都会导致任务断链。直接输出摘要正文，不要解释你在做什么。`

// newSummarizationMiddleware 摘要中间件 + 清窗兜底包装（window>0 才装配）。
func (m *Manager) newSummarizationMiddleware(ctx context.Context, s *session.Session, window int) (adk.ChatModelAgentMiddleware, error) {
	p, spec, ok := llm.FindSpec(m.Opt.Providers(), s.Model.Model)
	if !ok {
		return nil, &configError{"摘要模型不在可用清单内：" + s.Model.Model}
	}
	cm, err := m.Opt.NewModel(ctx, p, spec, s.Model.Effort)
	if err != nil {
		return nil, &configError{"摘要模型构造失败：" + err.Error()}
	}
	cm = llm.NewVisionModel(cm, spec, m.Opt.ImageResolve)
	cm = llm.NewHistoryShapeModel(cm, p.Kind)

	// H9-3 PreserveSkills 预算按窗口钳制：min(25000, window×10%)——adk 默认
	// 25k 是窗口无关常数，与 70% 相对触发线零关联（预算与触发线无 post-check
	// 联动，实锚），小窗（≤40k）下「摘要后视图仍 > 触发线」逐调用重摘要；
	// 大窗 min 恒取 25k 行为不变。10% 系数为自定（上游均绝对值：adk 25k /
	// codex 20k）——小窗防重触发的相对化特化。
	budget := min(25000, window/10)
	finalizer, err := summarization.NewFinalizer().PreserveSkills(&summarization.PreserveSkillsConfig{SkillsTokenBudget: &budget}).Build()
	if err != nil {
		return nil, err
	}
	// 溯源路径注入包装：transcript 取回指引追加进摘要信封末尾（copy-on-write
	// 不动 finalizer 产物原消息——PreserveSkills 与溯源段两全）。
	finalizeWithTrace := func(ctx context.Context, orig []*schema.Message, summary *schema.Message) ([]*schema.Message, error) {
		out, err := finalizer(ctx, orig, summary)
		if err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1] != nil {
			last := *out[n-1]
			last.Content += transcriptNote
			out[n-1] = &last
		}
		return out, nil
	}
	one := 1
	failover, err := m.summarizerFailover(ctx, s, window)
	if err != nil {
		return nil, err
	}
	inner, err := summarization.New(ctx, &summarization.Config{
		Model:           cm,
		Trigger:         &summarization.TriggerCondition{ContextTokens: window * summarizeTriggerPct / 100},
		TokenCounter:    summarTokenCounter,
		UserInstruction: summarizeInstruction,
		Finalize:        finalizeWithTrace,
		Retry:           &summarization.RetryConfig{MaxRetries: &one},
		Failover:        failover,
		GenModelInput:   trimModelInput(window),
		// H8-3 压缩通知卡（dsh CompactionSummaryNode 形态：压缩前后口径 +
		// 全文指针；live 流经 session 订阅扇出，回放随事件流）。transcript
		// 在此落域（before = 压缩前全文——触发即写：未触发的常规调用零写，
		// 摘要成功后 state 已是压缩视图不会被覆写）。
		Callback: func(ctx context.Context, before, after adk.TypedChatModelAgentState[*schema.Message]) error {
			bTok, _ := shapedTokenCounter(ctx, before.Messages, nil)
			aTok, _ := shapedTokenCounter(ctx, after.Messages, nil)
			writeTranscript(m.reg.Store(), s.Owner, s.SID, before.Messages)
			s.Record(contract.EvHarnessNote, contract.HarnessNote{
				Kind:   "compaction",
				Title:  fmt.Sprintf("已压缩历史 %d → %d token（保留任务水位）", bTok, aTok),
				Detail: "被压缩内容以摘要形态注入后续上下文；全文 spill/transcript.txt（read_file 可溯源）",
			})
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	return &clearWindowFallback{
		TypedBaseChatModelAgentMiddleware: &adk.TypedBaseChatModelAgentMiddleware[*schema.Message]{},
		inner:                             inner, st: m.reg.Store(), owner: s.Owner, sid: s.SID, sess: s,
	}, nil
}

// summarTokenCounter 形变后出站口径计数（与 reduction 同一计数器）。
func summarTokenCounter(_ context.Context, in *summarization.TokenCounterInput) (int, error) {
	n, err := shapedTokenCounter(context.Background(), in.Messages, in.Tools)
	return int(n), err
}

// summarizerFailover H9-10 摘要降级链（adk Failover 白拿位）：主摘要失败
// 按序换链上模型（逐个 NewModel + vision/shape 同链包装）；GetFailoverModel
// 按 Attempt 取链、输入经 trimModelInput 与主摘要同规则（adk 约束回参 input
// 非空）；ShouldFailover 排除 ctx 取消/中断类（默认 err!=nil 过频——取消中
// 的会话也进降级循环空转，adk failover_chatmodel.needFailover 同款排除面）；
// MaxRetries=链长（adk 默认 3，链尽后空转）。链尽走既有 clearWindowFallback
// 清窗兜底不外抛；空清单 = 不配降级零变化。
func (m *Manager) summarizerFailover(ctx context.Context, s *session.Session, window int) (*summarization.FailoverConfig, error) {
	if len(m.Opt.SummarizerFallbackModels) == 0 {
		return nil, nil
	}
	chain, err := m.modelChain(ctx, s, m.Opt.SummarizerFallbackModels, "摘要")
	if err != nil {
		return nil, err
	}
	maxR := len(chain)
	mkInput := trimModelInput(window)
	return &summarization.FailoverConfig{
		MaxRetries: &maxR,
		ShouldFailover: func(ctx context.Context, _ *schema.Message, err error) bool {
			if err == nil || ctx.Err() != nil {
				return false // 成功/ctx 取消：不切
			}
			var sig *adk.InterruptSignal
			return !errors.As(err, &sig) // 中断穿透不降级
		},
		GetFailoverModel: func(ctx context.Context, fctx *summarization.FailoverContext) (model.BaseModel[*schema.Message], []*schema.Message, error) {
			if fctx.Attempt > len(chain) {
				return nil, nil, fmt.Errorf("摘要降级链尽（%d 级）", len(chain))
			}
			in, err := mkInput(ctx, fctx.SystemInstruction, fctx.UserInstruction, fctx.OriginalMessages)
			if err != nil {
				return nil, nil, err
			}
			return chain[fctx.Attempt-1], in, nil
		},
	}, nil
}

// trimModelInput 防超窗修剪：输入裁进预算（80% 窗口），超预算时自尾向前
// 保留、起点回退 user 消息边界（不切 assistant(tool_calls)↔tool 配对）。
func trimModelInput(window int) summarization.GenModelInputFunc {
	budget := window * summarizeInputPct / 100
	return func(_ context.Context, sys, user *schema.Message, orig []*schema.Message) ([]*schema.Message, error) {
		var msgs []*schema.Message // 去 system（摘要有自己的 sys 指令）
		for _, m := range orig {
			if m.Role != schema.System {
				msgs = append(msgs, m)
			}
		}
		if n, _ := shapedTokenCounter(context.Background(), msgs, nil); n > int64(budget) {
			msgs = trimToBudget(msgs, budget)
		}
		out := make([]*schema.Message, 0, len(msgs)+2)
		out = append(out, sys)
		out = append(out, msgs...)
		out = append(out, user)
		return out, nil
	}
}

// trimToBudget 自尾向前累计至预算，起点回退到 user 边界（至少保留末条）。
func trimToBudget(msgs []*schema.Message, budget int) []*schema.Message {
	n, start := 0, len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		c := estTokens(msgTextOf(msgs[i])) + estTokens(msgs[i].ReasoningContent) + 8
		if n+c > budget && i < len(msgs)-1 {
			break
		}
		n += c
		start = i
	}
	for start < len(msgs)-1 && msgs[start].Role != schema.User {
		start++
	}
	if start >= len(msgs) {
		start = len(msgs) - 1
	}
	return msgs[start:]
}

// clearWindowFallback 摘要降级链耗尽的清窗兜底：不外抛错误，state 改自最后
// 一条 user 起的尾段新窗（user 开轮结构合法），运行继续。兜底先落 transcript
// 原文（state 尚未被改写）+ 发清窗通知卡（最破坏性的压缩不得对用户不可见
// ——dsh/codex 清窗路径均有 marker）。
type clearWindowFallback struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.Message]
	inner adk.ChatModelAgentMiddleware
	st    session.Store
	owner string
	sid   string
	sess  *session.Session // 清窗通知卡 Record 面
}

func (c *clearWindowFallback) BeforeModelRewriteState(
	ctx context.Context, state *adk.TypedChatModelAgentState[*schema.Message], mc *adk.TypedModelContext[*schema.Message],
) (context.Context, *adk.TypedChatModelAgentState[*schema.Message], error) {
	nctx, nstate, err := c.inner.BeforeModelRewriteState(ctx, state, mc)
	if err == nil {
		return nctx, nstate, nil
	}
	writeTranscript(c.st, c.owner, c.sid, state.Messages)
	c.sess.Record(contract.EvHarnessNote, contract.HarnessNote{
		Kind:   "compaction",
		Title:  "摘要失败，已清窗续跑（保留最近上下文）",
		Detail: fmt.Sprintf("压缩降级链耗尽：%v。上下文改用最近消息续跑；全文 spill/transcript.txt（read_file 可溯源）", err),
	})
	after := *state
	tail := tailFromLastUser(state.Messages)
	// 任务锚（H9-9）：尾段之后注入 user 消息（保 user 开轮结构）；显式新
	// 切片不动原数组（保真）。锚为单轮注入不落 session 真源——合成消息不
	// 进 events，模型当轮消化后经回复/todo_write 延续。
	out := make([]*schema.Message, len(tail), len(tail)+1)
	copy(out, tail)
	out = append(out, taskAnchor(c.sess.TitleOf(), lastTodoState(state.Messages)))
	after.Messages = out
	return ctx, &after, nil
}

// writeTranscript 全文落会话持久域（触发即写；read_file spill/transcript.txt 读回）。
func writeTranscript(st session.Store, owner, sid string, msgs []*schema.Message) {
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "## %s\n", m.Role)
		if m.ReasoningContent != "" {
			b.WriteString(m.ReasoningContent + "\n")
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "[tool_call %s %s]\n", tc.Function.Name, tc.Function.Arguments)
		}
		b.WriteString(msgTextOf(m) + "\n\n")
	}
	_ = st.WriteUserTreeFile(owner, filepath.Join("sessions", sid, "spill", "transcript.txt"), []byte(b.String()))
}

// tailFromLastUser 自最后一条 user 起的尾段（无 user 退化保末两条）。
func tailFromLastUser(msgs []*schema.Message) []*schema.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == schema.User {
			return msgs[i:]
		}
	}
	if len(msgs) > 2 {
		return msgs[len(msgs)-2:]
	}
	return msgs
}

// todoWriteTool 任务清单工具名（einox/tools/todo 注册名；引擎按名配对，
// 不引工具包——与 spawn 载荷解析同风格）。
const todoWriteTool = "todo_write"

// todoState todo_write 结果清单条目（todo 包 run 返回契约的子集）。
type todoState struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed
}

// lastTodoState 被清历史中最近一次 todo_write 的清单状态（结果 JSON 带
// todos 全量——全量覆盖写语义）。结果可能已被 reduction 外置成指针文本：
// 解析失败即降级返回 nil（锚退化为指引+transcript 路径，不解析 spill）。
func lastTodoState(msgs []*schema.Message) []todoState {
	calls := map[string]bool{}
	for _, m := range msgs {
		if m.Role == schema.Assistant {
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == todoWriteTool {
					calls[tc.ID] = true
				}
			}
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.Role == schema.Tool && calls[m.ToolCallID] {
			var tr struct {
				OK    bool        `json:"ok"`
				Todos []todoState `json:"todos"`
			}
			if json.Unmarshal([]byte(m.Content), &tr) == nil && tr.OK {
				return tr.Todos
			}
			return nil
		}
	}
	return nil
}

// taskAnchor 清窗兜底任务锚（H9-9）：todo 状态 + 会话标题 + 溯源指引。
// 压缩保任务状态是主流共识（Claude Code 摘要 Pending Tasks 章节/plan file
// 重注入、codex notes 跨窗持久）；成功路径摘要指令已要求保 todo，fallback
// 路径此前裸奔——此锚补差。
func taskAnchor(title string, todos []todoState) *schema.Message {
	var b strings.Builder
	b.WriteString("[任务状态锚] 此前对话已因压缩失败清窗，以上为清窗保留的最近消息。")
	if len(todos) > 0 {
		b.WriteString("当前任务清单状态：\n")
		for _, it := range todos {
			fmt.Fprintf(&b, "- [%s] %s\n", it.Status, it.Content)
		}
	}
	if title != "" {
		fmt.Fprintf(&b, "会话主题：%s\n", title)
	}
	b.WriteString("完整对话记录已存档 spill/transcript.txt（需要原文细节时用 read_file 读取）。请基于以上锚点继续任务，不要从头重做。")
	return schema.UserMessage(b.String())
}
