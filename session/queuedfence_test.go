package session

// C6 队列变异所有权围栏回归（设计件 findings/2026-09-21-subagent-inbox-control-design.md
// §2.1）：署名排队条目（T6 SpeakerID）仅本人或 Owner 可编辑/删除；匿名
// editor = 单用户零变化；匿名条目不设栏；notify 只读照旧。dsh updateQueue
// 「队列变异需所有权」本质的 einox T6 形态。

import (
	"testing"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
)

// queueBy 署名排队便捷面（running 态入队；id 经 steer_queued 回执寻址）。
func queueBy(t *testing.T, s *Session, speaker *contract.Participant, text string) string {
	t.Helper()
	n := 0
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvSteerQueued {
			n++
		}
	}
	if !s.SteerBy(speaker, text, nil, "") {
		t.Fatal("running 态入队应成功")
	}
	ids := steerIDsOf(t, s, n+1)
	return ids[len(ids)-1]
}

// TestQueuedOwnershipFence 围栏矩阵：署名条目 × 四类 editor。
func TestQueuedOwnershipFence(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	s := reg.Create("张三", "群任务", "manual", contract.UserPrefs{})
	if !s.BeginRun("manual") {
		t.Fatal("抢占失败")
	}
	li := &contract.Participant{ID: "u_li", Name: "李四"}
	wang := &contract.Participant{ID: "u_wang", Name: "王五"}
	mine := queueBy(t, s, li, "李四的补充")
	anon := queueBy(t, s, nil, "匿名补充")

	// 他人拒：王五不能改/删李四的排队
	if s.EditQueued("u_wang", mine, "篡改") {
		t.Fatal("他人编辑署名条目应拒")
	}
	if s.RemoveQueued("u_wang", mine) {
		t.Fatal("他人删除署名条目应拒")
	}
	// 本人过
	if !s.EditQueued("u_li", mine, "李四改口") {
		t.Fatal("本人编辑应过")
	}
	if !s.RemoveQueued("u_li", mine) {
		t.Fatal("本人删除应过")
	}
	// Owner 过（会话归属者权威）
	ownerEntry := queueBy(t, s, wang, "王五的补充")
	if !s.EditQueued("张三", ownerEntry, "归属者代改") {
		t.Fatal("Owner 编辑署名条目应过")
	}
	if !s.RemoveQueued("张三", ownerEntry) {
		t.Fatal("Owner 删除署名条目应过")
	}
	// 匿名 editor 零变化：单用户/系统路径不设栏
	anonEdit := queueBy(t, s, wang, "再排一条")
	if !s.EditQueued("", anonEdit, "匿名控制面改") {
		t.Fatal("匿名 editor（单用户零变化）应过")
	}
	if !s.RemoveQueued("", anonEdit) {
		t.Fatal("匿名 editor 删除应过")
	}
	// 匿名条目（无署名）不设栏
	if !s.EditQueued("u_wang", anon, "谁都能改匿名条") {
		t.Fatal("匿名条目不设栏")
	}
}
