// Package plan 提供 submit_plan 工具：计划文档提交（用户域 plans/<sid>/
// <seq>.md 持久落盘）+ 挂起送审（Suspend → 引擎转 plan_request 事件卡 →
// approve 端点决议 → Resume 消费）。决议语义按提交时档位分叉：plan 批准 =
// 授权任务期全部写（GrantTask，只审这一次）；manual 批准 = 仅确认方向不授权
// （写操作仍逐写挂起）；auto 不挂起（文档落档即走，供事后审计）。
// 写计划文档本身免审批（用户域过程态非产品数据；否则 manual 档死锁——批的
// 对象还没写出来）。修订 = 新序号文档，不改写旧稿（审计留痕）。
package plan

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
)

// Decision 决议（复用审批决议形态：Approve/Reason——挂起单通道，决议消费同源）。
type Decision = contract.ApprovalDecision

// planState 中断保存态（恢复时带回；文档内容可由 PlanCard 完整重建，恢复流
// 按决议状态重写整文件，不做部分编辑）。
type planState struct {
	Info contract.PlanCard
}

// gob 序列化注册（checkpoint 持久化中断载荷——未注册跨进程恢复失败）。
func init() {
	schema.Register[planState]()
	schema.Register[contract.PlanCard]()
	schema.Register[contract.PlanStep]()
}

// Session 会话域依赖（引擎装配注入：决议消费/任务授权/当前档位/序号取号）。
type Session interface {
	TakeDecision() *Decision
	GrantTask()
	ModePublic() string
	NextPlanSeq() int
}

// Config 构造配置。
type Config struct {
	S      Session
	SID    string                              // 会话 ID（文档路径 plans/<sid>/<seq>.md）
	Writer func(rel string, data []byte) error // 用户域文件写入（Registry.Store().WriteUserTreeFile）
}

type stepIn struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

type planIn struct {
	Task    string   `json:"task"`
	Summary string   `json:"summary"`
	Steps   []stepIn `json:"steps"`
	Risks   string   `json:"risks"`
}

// NewTools 构造 submit_plan（会话域件：不走审批名单，走挂起通道）。
func NewTools(cfg Config) ([]contract.Tool, error) {
	t, err := tools.InferTool("submit_plan",
		"提交任务计划文档（复杂任务先分析再计划）。task 一句话任务名；summary 概述目标与整体思路；steps 按执行顺序列步骤（title 简短，detail 写这步做什么、动哪些数据/文件）；risks 写风险与不确定点。计划模式（plan）：涉及写操作的任务必须先提交计划供用户审批——批准后整个任务期免逐项确认，被拒按反馈修订重提。变更前确认（manual）：可提交计划供用户过目，批准不等于授权，写操作仍逐一确认。完全访问（auto）：计划仅落档不送审，写完按计划推进。简单任务（无写操作或单步小改）不必提交计划。",
		func(ctx context.Context, in planIn) (map[string]any, error) {
			return run(ctx, cfg, in)
		})
	if err != nil {
		return nil, err
	}
	return []contract.Tool{t}, nil
}

func run(ctx context.Context, cfg Config, in planIn) (map[string]any, error) {
	// 恢复流：此前在本计划提交处中断过（适配层经 ctx 注回保存态）
	if st, ok := contract.ResumeStateOf(ctx); ok {
		saved, _ := st.(planState)
		return resume(cfg, saved)
	}

	// 新提交：校验 → 定号 → 文档先落盘 → 按档分叉（auto 即走 / 其余挂起）
	card, err := validate(cfg, in)
	if err != nil {
		return fail(err.Error())
	}
	if err := cfg.Writer(card.Path, docOf(cfg, card, docStatus(card.Mode))); err != nil {
		return fail("计划文档写入失败：" + err.Error())
	}
	if card.Mode == contract.ModeAuto {
		return map[string]any{
			"ok": true, "path": card.Path, "seq": card.Seq,
			"note": "计划已落档（完全访问模式不送审）——按计划推进执行",
		}, nil
	}
	return nil, &contract.Suspend{Info: card, State: planState{Info: card}}
}

// resume 决议三分叉：批准（plan 档授权任务期）/ 拒绝（原因回喂修订）/ 无决议
// （fail-closed 取消）。
func resume(cfg Config, saved planState) (map[string]any, error) {
	d := cfg.S.TakeDecision()
	if d == nil {
		// fail-closed：恢复但无决议（通道异常）→ 提交作废，模型可重提
		return fail("计划决议未到达（通道异常），本次提交作废——请与用户确认后重新 submit_plan")
	}
	if !d.Approve {
		reason := d.Reason
		if reason == "" {
			reason = "用户未提供原因"
		}
		if err := cfg.Writer(saved.Info.Path, docOf(cfg, saved.Info, "rejected（"+reason+"）")); err != nil {
			log.Printf("plan: 决议回写失败（%s rejected）：%v", saved.Info.Path, err)
		}
		return fail("计划被拒：" + reason + "——请按用户反馈修订后重新 submit_plan（新序号留痕，旧稿不动）")
	}
	if err := cfg.Writer(saved.Info.Path, docOf(cfg, saved.Info, "approved")); err != nil {
		log.Printf("plan: 决议回写失败（%s approved）：%v", saved.Info.Path, err)
	}
	if saved.Info.Mode == contract.ModePlan {
		cfg.S.GrantTask() // 批准计划 = 授权任务期全部写（只审这一次）
		return map[string]any{
			"ok": true, "approved": true, "path": saved.Info.Path,
			"note": "计划已批准——任务期内写操作不再逐项确认，按计划一口气执行，完成后汇报",
		}, nil
	}
	return map[string]any{
		"ok": true, "approved": true, "path": saved.Info.Path,
		"note": "计划已获用户确认（变更前确认模式：写操作仍会逐一请求确认）",
	}, nil
}

// validate 校验 + 组卡（取号、定路径、锁档位）。
func validate(cfg Config, in planIn) (contract.PlanCard, error) {
	if cfg.S == nil || cfg.Writer == nil || cfg.SID == "" {
		return contract.PlanCard{}, fmt.Errorf("计划通道未装配")
	}
	task := strings.TrimSpace(in.Task)
	summary := strings.TrimSpace(in.Summary)
	risks := strings.TrimSpace(in.Risks)
	if task == "" || summary == "" {
		return contract.PlanCard{}, fmt.Errorf("task 与 summary 不能为空")
	}
	if len([]rune(task)) > 120 {
		return contract.PlanCard{}, fmt.Errorf("task 过长（≤120 字）——一句话任务名")
	}
	if len([]rune(summary)) > 1500 {
		return contract.PlanCard{}, fmt.Errorf("summary 过长（≤1500 字）")
	}
	if len(in.Steps) == 0 || len(in.Steps) > 20 {
		return contract.PlanCard{}, fmt.Errorf("steps 需 1~20 步")
	}
	steps := make([]contract.PlanStep, 0, len(in.Steps))
	for i, s := range in.Steps {
		title := strings.TrimSpace(s.Title)
		detail := strings.TrimSpace(s.Detail)
		if title == "" {
			return contract.PlanCard{}, fmt.Errorf("第 %d 步 title 为空", i+1)
		}
		if len([]rune(title)) > 120 || len([]rune(detail)) > 1500 {
			return contract.PlanCard{}, fmt.Errorf("第 %d 步过长（title ≤120 / detail ≤1500 字）", i+1)
		}
		steps = append(steps, contract.PlanStep{Title: title, Detail: detail})
	}
	if len([]rune(risks)) > 1000 {
		return contract.PlanCard{}, fmt.Errorf("risks 过长（≤1000 字）")
	}
	seq := cfg.S.NextPlanSeq()
	return contract.PlanCard{
		Task: task, Summary: summary, Steps: steps, Risks: risks,
		Seq: seq, Path: fmt.Sprintf("plans/%s/%d.md", cfg.SID, seq),
		Mode: cfg.S.ModePublic(), SubmittedAt: time.Now(),
	}, nil
}

// docStatus 提交时文档状态（决议后由恢复流重写）。
func docStatus(mode string) string {
	if mode == contract.ModeAuto {
		return "auto（不送审）"
	}
	return "submitted（待审）"
}

// docOf 计划文档（内容可由 PlanCard 完整重建）。
func docOf(cfg Config, c contract.PlanCard, status string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# 计划：%s\n\n", c.Task)
	fmt.Fprintf(&b, "- 会话：%s\n- 序号：v%d\n- 模式：%s\n- 状态：%s\n- 提交时间：%s\n\n",
		cfg.SID, c.Seq, c.Mode, status, c.SubmittedAt.Format("2006-01-02 15:04:05"))
	b.WriteString("## 概述\n\n" + c.Summary + "\n\n## 步骤\n\n")
	for i, s := range c.Steps {
		if s.Detail != "" {
			fmt.Fprintf(&b, "%d. **%s**：%s\n", i+1, s.Title, s.Detail)
		} else {
			fmt.Fprintf(&b, "%d. **%s**\n", i+1, s.Title)
		}
	}
	if c.Risks != "" {
		b.WriteString("\n## 风险与备注\n\n" + c.Risks + "\n")
	}
	return []byte(b.String())
}

func fail(msg string) (map[string]any, error) {
	return map[string]any{"ok": false, "error": msg}, nil // 回喂模型自纠（errFeed 语义）
}
