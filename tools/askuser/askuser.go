// Package askuser 提供 ask_user 工具：长任务中途向用户结构化提问（单选/
// 多选/自由文本），经挂起-续流通道（contract.Suspend 哨兵 → 适配层转引擎
// Interrupt×Resume）。工具本体不实现交互：发起 = Suspend 上抛（装配层转
// 事件卡），作答 = DecisionResolver 读取（会话域 TakeAskDecision）。
// 交互语义参照 openai/codex core/src/elicitation.rs（Apache-2.0）与
// deepseek-harness packages/interaction/tool-ask-user（MIT）。
package askuser

import (
	"context"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
)

// Decision 作答（answer 端点登记；Resume 消费，取后清空防复用）——契约形态
// contract.AskDecision，此处别名保持包面稳定。
type Decision = contract.AskDecision

// askState 中断保存态（恢复时带回——引擎重放可能不带原始 args，问题文本以
// 挂起时的已校验 AskCard 为准）。
type askState struct {
	Info contract.AskCard
}

// gob 序列化注册（checkpoint 持久化中断载荷——未注册跨进程恢复失败）。
func init() {
	schema.Register[askState]()
	schema.Register[contract.AskCard]()
}

// DecisionResolver 决议读取（装配层注入：会话域）。
type DecisionResolver interface {
	Decision() *Decision
}

// Config 构造配置。
type Config struct {
	Resolver DecisionResolver
}

type askIn struct {
	Question      string               `json:"question"`
	Options       []contract.AskOption `json:"options"`
	AllowMulti    bool                 `json:"allow_multi"`
	AllowFreeText bool                 `json:"allow_free_text"`
}

// NewTools 构造 ask_user（非写操作：不走审批名单，走挂起通道）。
func NewTools(cfg Config) ([]contract.Tool, error) {
	t, err := tools.InferTool("ask_user",
		"向用户提问并等待作答（任务挂起，用户在界面上选择或输入后继续执行）。question 一句话问清一件事；options 给出候选（label 直白；value 可省略缺省同 label）；allow_multi = 可多选；allow_free_text = 允许自由输入。拿不准的事实、需要用户取舍的分叉，先问再动，不要猜。",
		func(ctx context.Context, in askIn) (map[string]any, error) {
			return run(ctx, cfg, in)
		})
	if err != nil {
		return nil, err
	}
	return []contract.Tool{t}, nil
}

func run(ctx context.Context, cfg Config, in askIn) (map[string]any, error) {
	// 恢复流：此前在本提问处中断过（适配层经 ctx 注回保存态）
	if st, ok := contract.ResumeStateOf(ctx); ok {
		saved, _ := st.(askState)
		if cfg.Resolver == nil {
			return fail("ask_user 通道未装配，本分支取消")
		}
		d := cfg.Resolver.Decision()
		if d == nil {
			// fail-closed：恢复但无作答（超时/通道异常）→ 分支取消，模型可调整
			return fail("用户未作答（提问超时或通道关闭），本分支取消——基于已有信息继续，或向用户说明情况后重新提问")
		}
		return map[string]any{
			"ok": true, "question": saved.Info.Question,
			"answers": d.Answers, "free_text": d.FreeText,
		}, nil
	}

	// 新提问：校验后挂起
	q := strings.TrimSpace(in.Question)
	if q == "" {
		return fail("question 不能为空")
	}
	if len([]rune(q)) > 500 {
		return fail("question 过长（≤500 字）——一句话问清一件事，长背景放前面的对话里")
	}
	if len(in.Options) > 6 {
		return fail("options 最多 6 个——更多选项说明该拆成多个问题")
	}
	opts := make([]contract.AskOption, 0, len(in.Options))
	for i, o := range in.Options {
		label := strings.TrimSpace(o.Label)
		if label == "" {
			return fail("第 " + strconv.Itoa(i+1) + " 个 option 的 label 为空")
		}
		value := strings.TrimSpace(o.Value)
		if value == "" {
			value = label
		}
		opts = append(opts, contract.AskOption{Label: label, Value: value})
	}
	if len(opts) == 0 && !in.AllowFreeText {
		return fail("无选项时必须 allow_free_text=true（否则用户无法作答）")
	}
	if cfg.Resolver == nil {
		return fail("ask_user 通道未装配")
	}
	info := contract.AskCard{Question: q, Options: opts, AllowMulti: in.AllowMulti, AllowFreeText: in.AllowFreeText}
	return nil, &contract.Suspend{Info: info, State: askState{Info: info}}
}

func fail(msg string) (map[string]any, error) {
	return map[string]any{"ok": false, "error": msg}, nil // 回喂模型自纠（errFeed 语义）
}
