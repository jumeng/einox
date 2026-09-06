package session

// 排队消息编辑/移除/重排回归（queued 三件——此前零测试）：排队 id 经
// steer_queued 回执可回放寻址；Edit 改文本落 steer_updated（未知 id 拒、
// 系统通知只读）；Remove 删中间落 steer_removed（重复删拒、系统通知不可删）；
// Reorder 全量集合校验（丢/多/未知 id 整体拒、原序不动）+ steer_reordered
// 回执即重排后顺序真源。

import (
	"testing"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
)

// newQueuedSession running 态会话（Steer 只在 running/pending 入队）。
func newQueuedSession(t *testing.T) *Session {
	t.Helper()
	s := NewRegistry(tstore.New(t.TempDir())).Create("张三", "任务", "manual", contract.UserPrefs{})
	s.SetState(StateRunning)
	return s
}

// steerIDsOf 排队消息 id 清单（事件流回放寻址——id 只在 steer_queued 回执里）。
func steerIDsOf(t *testing.T, s *Session, n int) []string {
	t.Helper()
	var ids []string
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvSteerQueued {
			if d, ok := ev.Data.(contract.SteerEvent); ok {
				ids = append(ids, d.ID)
			}
		}
	}
	if len(ids) != n {
		t.Fatalf("应落 %d 笔 steer_queued 回执，实得 %v", n, ids)
	}
	return ids
}

// eventHas 条件式事件载荷断言（回执落流与载荷字段的合并检查面）。
func eventHas(s *Session, name string, match func(contract.SteerEvent) bool) bool {
	for _, ev := range s.SnapshotEvents() {
		if ev.Event != name {
			continue
		}
		if d, ok := ev.Data.(contract.SteerEvent); ok && match(d) {
			return true
		}
	}
	return false
}

func TestEditQueuedRewritesText(t *testing.T) {
	s := newQueuedSession(t)
	if !s.Steer("第一条", nil, "") || !s.Steer("第二条", nil, "") {
		t.Fatal("running 态应可排队")
	}
	ids := steerIDsOf(t, s, 2)
	if !s.EditQueued(ids[1], "第二条（改）") {
		t.Fatal("在队消息应可编辑")
	}
	if n := s.QueueLen(); n != 2 {
		t.Fatalf("编辑不动队列长度，实得 %d", n)
	}
	if !eventHas(s, contract.EvSteerUpdated, func(d contract.SteerEvent) bool {
		return d.ID == ids[1] && d.Text == "第二条（改）"
	}) {
		t.Fatal("编辑应落 steer_updated 回执（携带新文本）")
	}
	if s.EditQueued("q_unknown", "x") {
		t.Fatal("未知 id 不应可编辑")
	}
	msgs := s.TakePending()
	if msgs[1].Text != "第二条（改）" {
		t.Fatalf("消费面应见改写后文本，实得 %+v", msgs)
	}
}

func TestEditQueuedNotifyReadonly(t *testing.T) {
	s := newQueuedSession(t)
	_, q := s.ContinueOrNotify("后台结论", false)
	if n := s.QueueLen(); n != 1 {
		t.Fatalf("running 态通知应入队，实得 %d", n)
	}
	if s.EditQueued(q.ID, "改") {
		t.Fatal("系统通知应只读（后台子代理结论不可编辑）")
	}
	if eventHas(s, contract.EvSteerUpdated, func(contract.SteerEvent) bool { return true }) {
		t.Fatal("被拒编辑不应落回执")
	}
}

func TestRemoveQueuedDropsMiddle(t *testing.T) {
	s := newQueuedSession(t)
	s.Steer("甲", nil, "")
	s.Steer("乙", nil, "")
	s.Steer("丙", nil, "")
	ids := steerIDsOf(t, s, 3)
	if !s.RemoveQueued(ids[1]) {
		t.Fatal("在队消息应可移除")
	}
	if !eventHas(s, contract.EvSteerRemoved, func(d contract.SteerEvent) bool { return d.ID == ids[1] }) {
		t.Fatal("移除应落 steer_removed 回执")
	}
	if s.RemoveQueued(ids[1]) {
		t.Fatal("重复移除应拒（条目已不存在）")
	}
	msgs := s.TakePending()
	if len(msgs) != 2 || msgs[0].Text != "甲" || msgs[1].Text != "丙" {
		t.Fatalf("移除中间条后应余甲/丙，实得 %+v", msgs)
	}
}

func TestRemoveQueuedNotifyKept(t *testing.T) {
	s := newQueuedSession(t)
	_, q := s.ContinueOrNotify("后台结论", false)
	if s.RemoveQueued(q.ID) {
		t.Fatal("系统通知不可删（结论注入是模型面承诺）")
	}
	if n := s.QueueLen(); n != 1 {
		t.Fatalf("被拒移除不应动队列，实得 %d", n)
	}
}

func TestReorderQueuedAppliesNewOrder(t *testing.T) {
	s := newQueuedSession(t)
	s.Steer("甲", nil, "")
	s.Steer("乙", nil, "")
	s.Steer("丙", nil, "")
	ids := steerIDsOf(t, s, 3)
	want := []string{ids[2], ids[0], ids[1]} // 丙/甲/乙
	if !s.ReorderQueued(want) {
		t.Fatal("全量重排应成功")
	}
	reordered := false
	for _, ev := range s.SnapshotEvents() {
		if ev.Event != contract.EvSteerReordered {
			continue
		}
		if d, ok := ev.Data.(contract.SteerReorder); ok && len(d.IDs) == 3 &&
			d.IDs[0] == want[0] && d.IDs[1] == want[1] && d.IDs[2] == want[2] {
			reordered = true
		}
	}
	if !reordered {
		t.Fatal("重排应落 steer_reordered 回执（新序真源）")
	}
	msgs := s.TakePending()
	if msgs[0].Text != "丙" || msgs[1].Text != "甲" || msgs[2].Text != "乙" {
		t.Fatalf("消费顺序应按重排，实得 %+v", msgs)
	}
}

func TestReorderQueuedRejectsBrokenSet(t *testing.T) {
	s := newQueuedSession(t)
	s.Steer("甲", nil, "")
	s.Steer("乙", nil, "")
	s.Steer("丙", nil, "")
	ids := steerIDsOf(t, s, 3)
	if s.ReorderQueued(ids[:2]) { // 丢一项
		t.Fatal("丢项应整体拒")
	}
	if s.ReorderQueued([]string{ids[0], ids[1], "q_unknown"}) { // 未知 id 顶替
		t.Fatal("未知 id 应整体拒")
	}
	if s.ReorderQueued([]string{ids[0], ids[1], ids[2], ids[0]}) { // 多一项
		t.Fatal("多项应整体拒")
	}
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvSteerReordered {
			t.Fatal("被拒重排不应落回执")
		}
	}
	msgs := s.TakePending()
	if msgs[0].Text != "甲" || msgs[1].Text != "乙" || msgs[2].Text != "丙" {
		t.Fatalf("整体拒应保原序不动，实得 %+v", msgs)
	}
}
