package runcommand

import (
	"strings"
	"testing"
	"time"
)

// TestTaskQuerySeam 应用侧查询缝(设计件 findings/2026-09-22-runcommand-
// taskquery-seam-design.md):后台任务可见(root 捕获/running)→ StopTask 出表
// → 完成态时长冻结。
func TestTaskQuerySeam(t *testing.T) {
	root := t.TempDir()

	// 1. 起一个长跑后台任务
	id, err := startBackground(root, "", nil, nil, "echo start; sleep 30", nil)
	if err != nil {
		t.Fatalf("起后台任务失败: %v", err)
	}
	// 输出就位(有缓冲期)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := TaskOutput(id); ok && strings.Contains(s.Output, "start") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 2. 全表可见:running + root 归属(全局表可能残留同包其他测试的任务
	// ——按本测试独有的 root 过滤断言)
	mine := mineOf(Tasks(), root)
	if len(mine) != 1 {
		t.Fatalf("本 root 应恰见 1 个任务,实际 %d", len(mine))
	}
	g := mine[0]
	if g.ID != id || !g.Running || g.Root != root {
		t.Fatalf("任务快照不符: %+v", g)
	}
	if !strings.Contains(g.Output, "start") {
		t.Fatalf("输出尾应含 start: %q", g.Output)
	}

	// 3. 应用侧终止 → 出表
	if !StopTask(id) {
		t.Fatal("StopTask 应返回 true")
	}
	if _, ok := TaskOutput(id); ok {
		t.Fatal("终止后应出表")
	}
	if mine := mineOf(Tasks(), root); len(mine) != 0 {
		t.Fatalf("终止后本 root 任务应空: %+v", mine)
	}
	if StopTask("t9999") {
		t.Fatal("不存在任务的 StopTask 应 false")
	}

	// 4. 完成态时长冻结:短任务自然结束后,两次轮询 duration 不变
	id2, err := startBackground(root, "", nil, nil, "sleep 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	var snap TaskSnapshot
	for time.Now().Before(deadline) {
		if mine := mineOf(Tasks(), root); len(mine) == 1 && !mine[0].Running {
			snap = mine[0]
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if snap.ID != id2 || snap.Running {
		t.Fatalf("任务应已自然结束: %+v", snap)
	}
	if snap.DurationMS < 900 {
		t.Fatalf("时长应约 1s: %d", snap.DurationMS)
	}
	first := snap.DurationMS
	time.Sleep(300 * time.Millisecond)
	if s2, ok := TaskOutput(id2); !ok || s2.DurationMS != first {
		t.Fatalf("完成态时长应冻结: first=%d second=%+v", first, s2)
	}
}

// mineOf 全表快照中归属 root 的子集(全局表跨测试共享——断言域收窄)。
func mineOf(all []TaskSnapshot, root string) []TaskSnapshot {
	var out []TaskSnapshot
	for _, s := range all {
		if s.Root == root {
			out = append(out, s)
		}
	}
	return out
}
