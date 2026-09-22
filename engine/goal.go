package engine

// goal 模型工具面（C4 最小核，设计件 findings/2026-09-21-goal-domain-design.md）：
// get_goal / create_goal / update_goal 三工具（dsh tool-goal 对位）。权威门控
// 在工具执行层：状态变更操作需当轮 direct-human（Session.TurnHumanInput——
// 直接输入/用户排队/运行中补充三源置位）；resume 命中「当前目标恰 paused 且
// ref 精确匹配」硬拒（dsh 窗口修复核心 GOAL_TOOL_RESUME_PAUSED：模型不能解除
// 暂停——direct-human 在场也拒，人类经应用面 Session.ResumeGoal 解除）。
//
// 挂载位：assemble 内独立于 ts（spawn 白名单源）追加进 face——goal 是会话级
// 单目标、主面专属，子代理面结构性不可见（白名单筛不到即物理不可达）。不在
// hitl 审批名单（非业务数据写面，todo/ask 同款先例）。
//
// GoalDriver 最小驱动器已落（设计件 findings/2026-09-22-goal-driver-design.md，
// GoalConfig.AutoContinue 装配）：自然收束 → goal 轮自续（complete/blocked
// 第二权威源）；dsh 全量形态中的暂停栅栏/armed/finish-reason 续行/wrapup/
// blockedAfter 按 einox 重定界不采（收束后自然停 + 无状态判定，见设计件 §1）。

import (
	"context"
	"fmt"
	"strings"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// goalDefaultMaxRounds create 未带轮上限的缺省（dsh GoalService 同值）。
const goalDefaultMaxRounds = 256

// GoalConfig goal 工具面配置（Options.Goal；nil = 不挂零变化）。
type GoalConfig struct {
	// DefaultMaxGoalRounds create_goal 未带 max_goal_rounds 的缺省轮上限
	// （<=0 = 256）。
	DefaultMaxGoalRounds int
	// AutoContinue 自动续行（GoalDriver 最小驱动器，设计件 findings/
	// 2026-09-22-goal-driver-design.md）：自然收束时目标仍 active 即注入
	// goal 轮自续（Kind="goal" 非 human 轮——complete/blocked 的第二权威源，
	// 应用面续行会置 direct-human 稀释权威，引擎通道是唯一不稀释形态）；
	// 轮预算耗尽原子转 blocked（round-limit）。false/nil = 零变化。
	AutoContinue bool
}

// goalRoundMsg goal 轮注入文案（机制常量——驱动器心跳，非业务内容）。
func goalRoundMsg(g *contract.Goal) string {
	return fmt.Sprintf("[goal 轮 %d/%d] 继续推进当前目标：%s。目标达成即用 update_goal 标记 complete；受阻即 blocked（附具体受阻条件）；确认无需继续时向用户说明并停止。",
		g.RoundsStarted, g.MaxGoalRounds, g.Objective)
}

// goalDrive goal 自动续行驱动点（settleTurn 尾部、StateEnded 分支——自然
// 收束唯一续行面；挂起/error 终态 settleTurn 早退不达此处）。四步全查：
// 配置关/无目标/非 active → 返回（暂停·完成·受阻即停——收束后自然停，
// 无栅栏）；AdmitGoalRound 预约（CAS+预算，超限已原子转 blocked 事件已落）
// → err 放弃（并发变异下轮再判）；注入（ContinueOrNotifyKind 复用单锁
// 原子原语——与用户并发 Run 同锁序列化：goal 先抢到则用户消息走 steering
// 排队成混合轮，用户先抢到则 goal 条目排下轮）→ began 即起自续轮
// （NotifyOwner 同款 goroutine 形态）。
func (m *Manager) goalDrive(s *session.Session) {
	if m.Opt.Goal == nil || !m.Opt.Goal.AutoContinue || s.Stopped() {
		return
	}
	g := s.GoalOf()
	if g == nil || g.Phase != contract.GoalActive {
		return
	}
	admitted, g2, err := s.AdmitGoalRound(g.ID, g.Revision)
	if err != nil || !admitted {
		return // 预算耗尽（已转 blocked）或并发变异——续行终止/下轮再判
	}
	began, q := s.ContinueOrNotifyKind(goalRoundMsg(g2), true, "goal")
	s.Record(contract.EvNotifyQueued, contract.SteerEvent{ID: q.ID, Text: q.Text, Kind: "goal"})
	if began {
		s.SetTurnActor(nil)                                  // 系统自续轮：清陈旧说话人（NotifyOwner 同律）
		go m.Run(context.Background(), s, "", nil, noopEmit) // goal 轮：预约经队列注入（输入路径唯一）
	}
}

// newGoalTools 构造三工具（无错误面——纯结构构造，域校验在执行层）。
func newGoalTools(s *session.Session, cfg *GoalConfig) []contract.Tool {
	defRounds := cfg.DefaultMaxGoalRounds
	if defRounds < 1 {
		defRounds = goalDefaultMaxRounds
	}
	gt, _ := tools.InferTool("get_goal",
		"读取当前会话的持久完成目标（无目标返回 goal:null；有目标含精确 id/revision/目标/相位/轮上限/受阻原因）。更新目标前先调用本工具取精确 goal_id 与 revision。",
		func(_ context.Context, _ struct{}) (map[string]any, error) {
			return map[string]any{"ok": true, "goal": goalValue(s.GoalOf())}, nil
		})
	ct, _ := tools.InferTool("create_goal",
		"为当前用户的直接请求建立一个持久完成目标（跨轮次/跨重启存活的长期目标——用户以任意表述表达「持续做到完成」意图时使用；普通单轮任务勿建）。仅在用户直接指令的轮次可用。",
		func(_ context.Context, in createGoalIn) (map[string]any, error) {
			if !s.TurnHumanInputOf() {
				return tools.Fail("create_goal 需用户直接指令（本轮无直接用户输入）——请等待用户的下一条消息"), nil
			}
			rounds := in.MaxGoalRounds
			if rounds == 0 {
				rounds = defRounds
			}
			g, err := s.CreateGoal(in.Objective, rounds)
			if err != nil {
				return tools.Fail(err.Error()), nil
			}
			return map[string]any{"ok": true, "goal": goalValue(g)}, nil
		})
	ut, _ := tools.InferTool("update_goal",
		"更新当前持久目标（goal_id 与 revision 须精确取自 get_goal）。action 取值：edit（改目标/轮上限——需用户直接指令）| pause（暂停——需用户直接指令）| resume（恢复——需用户直接指令；已暂停目标模型不可恢复，须由用户解除）| complete（标记完成——需用户直接指令，确已达成才标）| blocked（报告受阻——需用户直接指令，附 blocked_reason 说明具体受阻条件）。",
		func(_ context.Context, in updateGoalIn) (map[string]any, error) {
			return updateGoal(s, in)
		})
	return []contract.Tool{gt, ct, ut}
}

type createGoalIn struct {
	Objective     string `json:"objective"`
	MaxGoalRounds int    `json:"max_goal_rounds,omitempty"`
}

type updateGoalIn struct {
	GoalID        string `json:"goal_id"`
	Revision      int    `json:"revision"`
	Action        string `json:"action"`
	Objective     string `json:"objective,omitempty"`
	MaxGoalRounds int    `json:"max_goal_rounds,omitempty"`
	BlockedReason string `json:"blocked_reason,omitempty"`
}

// updateGoal update_goal 执行（判序照 dsh：参数互斥 → 权威 → 域变异）。
func updateGoal(s *session.Session, in updateGoalIn) (map[string]any, error) {
	if strings.TrimSpace(in.GoalID) == "" || in.GoalID != strings.TrimSpace(in.GoalID) ||
		in.Revision < 1 {
		return tools.Fail("goal_id 须非空且 revision 须为正整数（取自 get_goal）"), nil
	}
	in.GoalID = strings.TrimSpace(in.GoalID)
	needHuman := func() (map[string]any, error) {
		if !s.TurnHumanInputOf() {
			return tools.Fail("update_goal " + in.Action + " 需用户直接指令（本轮无直接用户输入）——请转告用户在其下一条消息中下达"), nil
		}
		return nil, nil
	}
	switch in.Action {
	case "edit", "pause", "resume", "complete", "blocked":
	default:
		return tools.Fail("action 取值非法：" + in.Action + "（edit|pause|resume|complete|blocked）"), nil
	}
	// 互斥校验（dsh 同款全表）
	switch in.Action {
	case "edit":
		if in.BlockedReason != "" {
			return tools.Fail("blocked_reason 仅 action=blocked 可用"), nil
		}
	case "pause", "resume":
		if in.Objective != "" || in.MaxGoalRounds != 0 || in.BlockedReason != "" {
			return tools.Fail("objective 与 max_goal_rounds 仅 action=edit 可用；blocked_reason 仅 action=blocked 可用"), nil
		}
	case "complete":
		if in.Objective != "" || in.MaxGoalRounds != 0 {
			return tools.Fail("objective 与 max_goal_rounds 仅 action=edit 可用"), nil
		}
		if in.BlockedReason != "" {
			return tools.Fail("blocked_reason 仅 action=blocked 可用"), nil
		}
	case "blocked":
		if in.Objective != "" || in.MaxGoalRounds != 0 {
			return tools.Fail("objective 与 max_goal_rounds 仅 action=edit 可用"), nil
		}
		if strings.TrimSpace(in.BlockedReason) == "" {
			return tools.Fail("action=blocked 须携带 blocked_reason（具体受阻条件）"), nil
		}
	}
	// resume-paused 硬拒（先于域变异——模型不能解除暂停，direct-human 在场也拒）
	if in.Action == "resume" {
		if cur := s.GoalOf(); cur != nil && cur.ID == in.GoalID && cur.Revision == in.Revision &&
			cur.Phase == contract.GoalPaused {
			return tools.Fail("目标已暂停——模型不能解除暂停，须由用户经应用通道恢复（请向用户说明恢复方式后停止重试）"), nil
		}
	}
	// complete/blocked 第二权威源：goal 轮（引擎注入的非 human 轮——dsh
	// goal-round authority 对位；应用面续行必置 direct-human 会稀释权威，
	// goal 轮是唯一不稀释的自评通道）；edit/pause/resume 仍仅 direct-human。
	if fail, err := needHuman(); fail != nil || err != nil {
		if !s.TurnGoalRoundOf() || (in.Action != "complete" && in.Action != "blocked") {
			return fail, err
		}
	}
	var (
		g   *contract.Goal
		err error
	)
	switch in.Action {
	case "edit":
		g, err = s.EditGoal(in.GoalID, in.Revision, in.Objective, in.MaxGoalRounds)
	case "pause":
		g, err = s.PauseGoal(in.GoalID, in.Revision)
	case "resume":
		g, err = s.ResumeGoal(in.GoalID, in.Revision)
	case "complete":
		g, err = s.CompleteGoal(in.GoalID, in.Revision)
	case "blocked":
		g, err = s.BlockGoal(in.GoalID, in.Revision, "model-reported", strings.TrimSpace(in.BlockedReason))
	}
	if err != nil {
		return tools.Fail(err.Error()), nil
	}
	return map[string]any{"ok": true, "goal": goalValue(g)}, nil
}

// goalValue 契约快照 → 工具输出形态（nil = JSON null——无目标双态）。
func goalValue(g *contract.Goal) any {
	if g == nil {
		return nil
	}
	return g
}
