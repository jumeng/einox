package session

// C4 goal 域最小核回归（设计件 findings/2026-09-21-goal-domain-design.md）：
// 持久完成目标的域层——CAS 变异、转移表、校验、goal_change 事件全快照载荷、
// 跨重启（Reattach）与 fork/Side 继承。权威门控在 engine 工具面（goal_test.go
// engine 侧），本层只管域不变量。

import (
	"strings"
	"testing"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
)

func newGoalSession(t *testing.T) (*Registry, *Session) {
	t.Helper()
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	return reg, reg.Create("张三", "任务", "manual", contract.UserPrefs{})
}

// TestGoalLifecycleTransitions 转移表全路径：create→pause→resume→block→
// complete；complete 后可重建；active 期不可重建；clear 任意相可清。
func TestGoalLifecycleTransitions(t *testing.T) {
	_, s := newGoalSession(t)

	if _, err := s.CreateGoal("  ", 5); err == nil {
		t.Fatal("空 objective 应拒")
	}
	if _, err := s.CreateGoal("目标", 0); err == nil {
		t.Fatal("非正 maxRounds 应拒")
	}
	g, err := s.CreateGoal("完成重构", 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if g.Phase != contract.GoalActive || g.Revision != 1 || g.MaxGoalRounds != 5 || g.ID == "" {
		t.Fatalf("create 形态不符：%+v", g)
	}
	if _, err := s.CreateGoal("另一个", 5); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("active 期不可重建：%v", err)
	}

	// pause：仅 active
	if _, err := s.PauseGoal(g.ID, g.Revision); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if cur := s.GoalOf(); cur.Phase != contract.GoalPaused || cur.Revision != 2 {
		t.Fatalf("pause 形态不符：%+v", cur)
	}
	if _, err := s.PauseGoal(g.ID, 2); err == nil {
		t.Fatal("paused 不可再 pause")
	}

	// resume：paused/blocked → active；active 来源拒
	if _, err := s.ResumeGoal(g.ID, 2); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := s.ResumeGoal(g.ID, 3); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatalf("active 来源 resume 应拒：%v", err)
	}

	// block：仅 active，带 code/msg
	if _, err := s.BlockGoal(g.ID, 3, "BadCode", "x"); err == nil {
		t.Fatal("block code 应 lower-kebab")
	}
	if _, err := s.BlockGoal(g.ID, 3, "missing-perm", " "); err == nil {
		t.Fatal("block msg 应非空")
	}
	if _, err := s.BlockGoal(g.ID, 3, "missing-perm", "缺少权限"); err != nil {
		t.Fatalf("block: %v", err)
	}
	if cur := s.GoalOf(); cur.Phase != contract.GoalBlocked || cur.BlockedCode != "missing-perm" || cur.BlockedMsg != "缺少权限" {
		t.Fatalf("block 形态不符：%+v", cur)
	}

	// complete：active/paused/blocked 均可
	if _, err := s.CompleteGoal(g.ID, 4); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if cur := s.GoalOf(); cur.Phase != contract.GoalComplete {
		t.Fatalf("complete 形态不符：%+v", cur)
	}
	if _, err := s.PauseGoal(g.ID, 5); err == nil {
		t.Fatal("complete 不可 pause")
	}
	if _, err := s.BlockGoal(g.ID, 5, "x-y", "z"); err == nil {
		t.Fatal("complete 不可 block")
	}
	if _, err := s.CompleteGoal(g.ID, 5); err == nil {
		t.Fatal("complete 不可再 complete")
	}

	// complete 后可重建（新目标新身份）
	g2, err := s.CreateGoal("新任务", 3)
	if err != nil {
		t.Fatalf("complete 后应可重建：%v", err)
	}
	if g2.ID == g.ID || g2.Revision != 1 {
		t.Fatalf("重建应是全新目标：%+v", g2)
	}

	// clear：任意相可清（墓碑 rev+1）；清后无目标、可再建
	if err := s.ClearGoal(g2.ID, g2.Revision); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if s.GoalOf() != nil {
		t.Fatal("clear 后应无当前目标")
	}
	if _, err := s.CreateGoal("再来", 2); err != nil {
		t.Fatalf("clear 后应可重建：%v", err)
	}
}

// TestGoalCASRevisions CAS：id 不符 / revision 陈旧 / 无目标 全拒。
func TestGoalCASRevisions(t *testing.T) {
	_, s := newGoalSession(t)
	if _, err := s.PauseGoal("g9", 1); err == nil || !strings.Contains(err.Error(), "无当前目标") {
		t.Fatalf("无目标操作应拒：%v", err)
	}
	g, _ := s.CreateGoal("目标", 5)
	if _, err := s.PauseGoal("别的ID", g.Revision); err == nil || !strings.Contains(err.Error(), "不符") {
		t.Fatalf("id 不符应拒：%v", err)
	}
	if _, err := s.PauseGoal(g.ID, 99); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("陈旧 revision 应拒：%v", err)
	}
	// 陈旧拒绝不产生副作用：当前 revision 仍 1、无事件
	if cur := s.GoalOf(); cur.Revision != 1 {
		t.Fatalf("拒绝不应有副作用：%+v", cur)
	}
	if err := s.ClearGoal(g.ID, 99); err == nil {
		t.Fatal("clear 陈旧 revision 应拒")
	}
}

// TestGoalEditPartially edit 部分栏替换（空串/0 = 不改该栏）；至少一栏。
func TestGoalEditPartially(t *testing.T) {
	_, s := newGoalSession(t)
	g, _ := s.CreateGoal("原目标", 5)
	if _, err := s.EditGoal(g.ID, g.Revision, "", 0); err == nil || !strings.Contains(err.Error(), "至少") {
		t.Fatalf("edit 零替换栏应拒：%v", err)
	}
	e, err := s.EditGoal(g.ID, g.Revision, "新目标", 0)
	if err != nil || e.Objective != "新目标" || e.MaxGoalRounds != 5 || e.Revision != 2 {
		t.Fatalf("edit objective 形态不符：%+v err=%v", e, err)
	}
	e, err = s.EditGoal(g.ID, e.Revision, "", 9)
	if err != nil || e.Objective != "新目标" || e.MaxGoalRounds != 9 {
		t.Fatalf("edit maxRounds 形态不符：%+v err=%v", e, err)
	}
	if _, err := s.EditGoal(g.ID, e.Revision, "  ", 0); err == nil {
		t.Fatal("空白 objective 应拒")
	}
	if _, err := s.EditGoal(g.ID, e.Revision, "", -1); err == nil {
		t.Fatal("非正 maxRounds 应拒")
	}
}

// TestGoalEventsRecorded 事件真源：每次变异落 goal_change 全快照载荷。
func TestGoalEventsRecorded(t *testing.T) {
	_, s := newGoalSession(t)
	g, _ := s.CreateGoal("目标", 5)
	s.PauseGoal(g.ID, 1)
	s.ClearGoal(g.ID, 2)

	var ops []string
	for _, ev := range s.SnapshotEvents() {
		if ev.Event != contract.EvGoalChange {
			continue
		}
		gc, ok := EventAs[contract.GoalChange](ev)
		if !ok {
			t.Fatalf("goal_change 载荷应可还原：%+v", ev.Data)
		}
		ops = append(ops, gc.Operation)
		switch gc.Operation {
		case "create", "pause":
			if gc.Goal == nil || gc.Goal.ID != g.ID {
				t.Fatalf("%s 应带全快照：%+v", gc.Operation, gc)
			}
			if gc.Operation == "pause" && gc.Goal.Phase != contract.GoalPaused {
				t.Fatalf("pause 快照相不符：%+v", gc.Goal)
			}
		case "clear":
			if gc.Goal != nil {
				t.Fatalf("clear 应无快照：%+v", gc)
			}
			if gc.ClearedID != g.ID || gc.ClearedRev != 3 {
				t.Fatalf("clear 墓碑不符：%+v", gc)
			}
		}
	}
	if strings.Join(ops, ",") != "create,pause,clear" {
		t.Fatalf("事件序不符：%v", ops)
	}
}

// TestGoalPersistenceAndInheritance 跨重启 Reattach 续接 + Fork/ForkAt/Side
// 继承（与 participants/imgOffloads 同位四路），派生可独立演化。
func TestGoalPersistenceAndInheritance(t *testing.T) {
	reg, s := newGoalSession(t)
	g, _ := s.CreateGoal("持久目标", 8)
	s.PauseGoal(g.ID, 1)
	reg.Persist(s)

	reg2 := NewRegistry(reg.Store())
	s2 := reg2.Reattach("张三", s.SID)
	if s2 == nil {
		t.Fatal("Reattach 应续接")
	}
	if cur := s2.GoalOf(); cur == nil || cur.Objective != "持久目标" || cur.Phase != contract.GoalPaused || cur.Revision != 2 {
		t.Fatalf("重启后目标应续接：%+v", cur)
	}

	fork := reg.Fork("张三", s.SID)
	side := reg.Side("张三", s.SID)
	for name, d := range map[string]*Session{"fork": fork, "side": side} {
		if d == nil {
			t.Fatalf("%s 应成功", name)
		}
		if cur := d.GoalOf(); cur == nil || cur.Phase != contract.GoalPaused {
			t.Fatalf("%s 应继承目标态：%+v", name, cur)
		}
	}
	// 派生独立演化：fork 恢复不影响父与 side
	if _, err := fork.ResumeGoal(fork.GoalOf().ID, fork.GoalOf().Revision); err != nil {
		t.Fatalf("fork 演化失败：%v", err)
	}
	if fork.GoalOf().Phase != contract.GoalActive || s.GoalOf().Phase != contract.GoalPaused || side.GoalOf().Phase != contract.GoalPaused {
		t.Fatal("派生演化不应影响父/兄弟")
	}
}
