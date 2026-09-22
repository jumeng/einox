package session

// goal 域（C4 最小核，设计件 findings/2026-09-21-goal-domain-design.md）：
// 每会话一个持久完成目标——dsh packages/goal 域核对位。目标态随 sessionRecord
// 持久化（fork/Side/Reattach 继承——与 participants/imgOffloads 同位），每次
// 变异经 goal_change 事件落流（全快照载荷——回放/审计真源）。
//
// 权威分界：本层是域层（CAS + 转移表 + 校验），不懂「谁在调」——调用者两类：
// 引擎 goal 工具（模型面，engine 侧做 direct-human 权威门控）与应用直调
// （人类通道——应用把 /goal 类命令/端点接到这些公开方法上，dsh command-goal
// 对位）。变异原子性：锁内校验+替换，锁外落流（Record 自持锁）；先替换后
// 落流间观察者可能读到新态未见事件，与既有 participant_update 同款顺序取舍。
//
// 显式不做（最小核裁剪，设计件 §4）：GoalActivation 进程本地二分、
// roundsStarted 计数、wrapup 注入——GoalDriver 自动续行批一并落。暂停语义
// 坑绕开：无自动续行即无暂停栅栏竞态，pause 只落持久 phase 不联动取消在跑轮。

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/shortid"
)

// goalDefaultMaxRounds create 未带轮上限时的缺省（dsh GoalService 同值）。
const goalDefaultMaxRounds = 256

// blockCodeRe blocked 分类码形态（lower-kebab——机器可路由，dsh 同款约束）。
var blockCodeRe = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

// GoalOf 当前目标快照（nil = 无目标；返回副本——调用方改写不污染域态）。
func (s *Session) GoalOf() *contract.Goal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return goalCopyOf(s.goal)
}

// goalCopyOf 快照副本（nil 透传；全调用点通用——仅解引用拷贝，不触锁）。
func goalCopyOf(g *contract.Goal) *contract.Goal {
	if g == nil {
		return nil
	}
	cp := *g
	return &cp
}

// CreateGoal 建立目标（无目标或当前已 complete 才可建——complete 可替换、
// 其余相须先 clear/resume，dsh 同款）。maxRounds 须为正整数（缺省轮数归调用
// 方——引擎工具面按 GoalConfig 补缺省后传入，dsh 缺省应用在请求层同款）。
func (s *Session) CreateGoal(objective string, maxRounds int) (*contract.Goal, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return nil, errors.New("goal objective 必须非空")
	}
	if maxRounds < 1 {
		return nil, errors.New("max_goal_rounds 必须为正整数")
	}
	next := &contract.Goal{ID: shortid.Hex("g", 8), Revision: 1, Objective: objective,
		Phase: contract.GoalActive, MaxGoalRounds: maxRounds}
	out, err := s.mutateGoal("create", func(cur *contract.Goal) (*contract.Goal, error) {
		if cur != nil && cur.Phase != contract.GoalComplete {
			return nil, fmt.Errorf("goal %s 已存在（phase=%s）——complete 可替换，其余须先 clear/resume", cur.ID, cur.Phase)
		}
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EditGoal 替换 objective/maxGoalRounds（空串/0 = 不改该栏；两者皆空 = 拒）。
// 相位不变（dsh edit 无相限）。CAS：id+revision 与当前不符即拒。
func (s *Session) EditGoal(id string, rev int, objective string, maxRounds int) (*contract.Goal, error) {
	return s.mutateGoal("edit", func(cur *contract.Goal) (*contract.Goal, error) {
		if err := expectGoalRef(cur, id, rev); err != nil {
			return nil, err
		}
		if objective == "" && maxRounds == 0 {
			return nil, errors.New("goal edit 至少携带 objective / max_goal_rounds 之一")
		}
		next := *cur
		if objective != "" {
			o := strings.TrimSpace(objective)
			if o == "" {
				return nil, errors.New("goal objective 必须非空")
			}
			next.Objective = o
		}
		if maxRounds != 0 {
			if maxRounds < 1 {
				return nil, errors.New("max_goal_rounds 必须为正整数")
			}
			next.MaxGoalRounds = maxRounds
		}
		next.Revision = cur.Revision + 1
		return &next, nil
	})
}

// PauseGoal active → paused（执行期强制持久暂停——dsh 窗口修复语义的最小核
// 落点：暂停是持久相，无进程本地软暂停）。
func (s *Session) PauseGoal(id string, rev int) (*contract.Goal, error) {
	return s.transitionGoal("pause", id, rev, []string{contract.GoalActive}, contract.GoalPaused)
}

// ResumeGoal paused/blocked → active（人类通道：模型面 resume-paused 硬拒，
// 解除暂停只能走这里——应用接 /goal 命令/端点）。active 来源拒（dsh 允许
// active-disarmed 重挂，无 activation 概念不可区分——最小核收窄，驱动器批
// 恢复全语义，设计件 §2.1）。
func (s *Session) ResumeGoal(id string, rev int) (*contract.Goal, error) {
	return s.transitionGoal("resume", id, rev,
		[]string{contract.GoalPaused, contract.GoalBlocked}, contract.GoalActive)
}

// CompleteGoal active/paused/blocked → complete。
func (s *Session) CompleteGoal(id string, rev int) (*contract.Goal, error) {
	return s.transitionGoal("complete", id, rev,
		[]string{contract.GoalActive, contract.GoalPaused, contract.GoalBlocked}, contract.GoalComplete)
}

// BlockGoal active → blocked + 分类码/解释（lower-kebab code + 非空 msg）。
func (s *Session) BlockGoal(id string, rev int, code, msg string) (*contract.Goal, error) {
	if !blockCodeRe.MatchString(code) {
		return nil, errors.New("blocked 分类码须为 lower-kebab 形态（如 missing-perm）")
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return nil, errors.New("blocked 解释必须非空")
	}
	return s.mutateGoal("block", func(cur *contract.Goal) (*contract.Goal, error) {
		if err := expectGoalRef(cur, id, rev); err != nil {
			return nil, err
		}
		if cur.Phase != contract.GoalActive {
			return nil, fmt.Errorf("goal %s 不可从 phase=%s 执行 block（允许 active）", cur.ID, cur.Phase)
		}
		next := *cur
		next.Revision, next.Phase = cur.Revision+1, contract.GoalBlocked
		next.BlockedCode, next.BlockedMsg = code, msg
		return &next, nil
	})
}

// ClearGoal 清当前目标（墓碑 rev+1；任意相可清）。人类通道专用（模型面不
// 暴露 clear——dsh 同款，host 命令域）。
func (s *Session) ClearGoal(id string, rev int) error {
	s.mu.Lock()
	if s.goal == nil {
		s.mu.Unlock()
		return errors.New("无当前目标")
	}
	if s.goal.ID != id || s.goal.Revision != rev {
		err := fmt.Errorf("goal revision 不符：当前 %s@%d，请求 %s@%d", s.goal.ID, s.goal.Revision, id, rev)
		s.mu.Unlock()
		return err
	}
	change := contract.GoalChange{Operation: "clear", ClearedID: s.goal.ID, ClearedRev: s.goal.Revision + 1}
	s.goal = nil
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
	s.Record(contract.EvGoalChange, change)
	return nil
}

// AdmitGoalRound goal 轮预约（GoalDriver 驱动器批，设计件 findings/
// 2026-09-22-goal-driver-design.md §2.1）：锁内原子「CAS + active 校验 +
// 预算裁定」——预算内 RoundsStarted++（引擎簿记：不动 Revision——模型侧
// revision 不因轮推进漂移；不落 goal_change——record 全量快照携带计数即回放
// 可见）；超限同锁原子转 blocked（code=round-limit，rev+1 落 goal_change
// 事件——不追并发变异：CAS 失败即放弃，goal 仍 active 下轮再判）。
// 返回：admitted=true 附新快照；false 附 blocked 快照（预算耗尽）；err 非 nil
// = 本轮不注入（CAS 失败/无目标/非 active）。
func (s *Session) AdmitGoalRound(id string, rev int) (bool, *contract.Goal, error) {
	s.mu.Lock()
	if err := expectGoalRef(s.goal, id, rev); err != nil {
		s.mu.Unlock()
		return false, nil, err
	}
	if s.goal.Phase != contract.GoalActive {
		err := fmt.Errorf("goal %s 不可从 phase=%s 预约 goal 轮（允许 active）", s.goal.ID, s.goal.Phase)
		s.mu.Unlock()
		return false, nil, err
	}
	if s.goal.RoundsStarted >= s.goal.MaxGoalRounds {
		next := *s.goal
		next.Revision = s.goal.Revision + 1
		next.Phase = contract.GoalBlocked
		next.BlockedCode = "round-limit"
		next.BlockedMsg = fmt.Sprintf("goal 轮预算已耗尽（%d/%d）——续行终止，请用户经应用通道恢复或调整目标", s.goal.RoundsStarted, s.goal.MaxGoalRounds)
		s.goal = &next
		s.UpdatedAt = time.Now()
		s.mu.Unlock()
		cp := goalCopyOf(&next)
		s.Record(contract.EvGoalChange, contract.GoalChange{Operation: "block", Goal: cp})
		return false, cp, nil
	}
	s.goal.RoundsStarted++
	cp := goalCopyOf(s.goal)
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
	return true, cp, nil
}

// transitionGoal 共用相位转移（锁内 CAS + 来源相校验；blocked 相清理）。
func (s *Session) transitionGoal(op, id string, rev int, allowed []string, to string) (*contract.Goal, error) {
	return s.mutateGoal(op, func(cur *contract.Goal) (*contract.Goal, error) {
		if err := expectGoalRef(cur, id, rev); err != nil {
			return nil, err
		}
		if !phaseIn(cur.Phase, allowed) {
			return nil, fmt.Errorf("goal %s 不可从 phase=%s 执行 %s（允许 %s）", cur.ID, cur.Phase, op, strings.Join(allowed, "|"))
		}
		next := *cur
		next.Revision, next.Phase = cur.Revision+1, to
		next.BlockedCode, next.BlockedMsg = "", ""
		return &next, nil
	})
}

// mutateGoal 变异骨架：锁内「校验 + 快照替换」，锁外落流（Record 自持锁
// ——锁内调用即死锁）。fn 返回 error 非 nil = 拒绝（无副作用）。返回采纳的
// 快照副本（调用方拿到恰是自己这次变异的产物，不受并发后续变异影响）。
func (s *Session) mutateGoal(op string, fn func(cur *contract.Goal) (*contract.Goal, error)) (*contract.Goal, error) {
	s.mu.Lock()
	next, err := fn(s.goal)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.goal = next
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
	cp := goalCopyOf(next)
	s.Record(contract.EvGoalChange, contract.GoalChange{Operation: op, Goal: cp})
	return cp, nil
}

// expectGoalRef CAS 前置：目标在场且 id/revision 精确匹配。
func expectGoalRef(cur *contract.Goal, id string, rev int) error {
	if cur == nil {
		return errors.New("无当前目标")
	}
	if cur.ID != id || cur.Revision != rev {
		return fmt.Errorf("goal revision 不符：当前 %s@%d，请求 %s@%d（先 get_goal 取新值）", cur.ID, cur.Revision, id, rev)
	}
	return nil
}

func phaseIn(p string, allowed []string) bool {
	for _, a := range allowed {
		if p == a {
			return true
		}
	}
	return false
}
