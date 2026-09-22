package runcommand

import (
	"sort"
	"time"
)

// TaskSnapshot 应用侧任务视图（查询缝——桌面/TUI 任务面板数据源；设计件
// findings/2026-09-22-runcommand-taskquery-seam-design.md）。与模型面的
// map 信封（snapshot()）并存：map 形态是工具结果回喂的既有约定，应用面
// 用结构体。Root 供应用反解任务归属会话（模型不消费）。
type TaskSnapshot struct {
	ID         string `json:"id"`
	Command    string `json:"command"`
	Root       string `json:"root"` // 起任务时的工作区根
	Running    bool   `json:"running"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Output     string `json:"output"` // 头尾保留（同 task_output）
}

// Tasks 全部后台任务快照（新→旧）。只读聚合，锁序：taskMu → bgTask.mu
// （与 task_output 同序）。
func Tasks() []TaskSnapshot {
	taskMu.Lock()
	ids := make([]string, 0, len(taskTable))
	for id := range taskTable {
		ids = append(ids, id)
	}
	snapOf := make(map[string]*bgTask, len(ids))
	for _, id := range ids {
		snapOf[id] = taskTable[id]
	}
	taskMu.Unlock()
	// 新→旧：id 序号即创建序（t1、t2…）
	sort.Slice(ids, func(i, j int) bool { return taskSeqOf(ids[i]) > taskSeqOf(ids[j]) })
	out := make([]TaskSnapshot, 0, len(ids))
	for _, id := range ids {
		if s, ok := taskView(snapOf[id]); ok {
			out = append(out, s)
		}
	}
	return out
}

// TaskOutput 单任务快照（不存在 = false）。
func TaskOutput(id string) (TaskSnapshot, bool) {
	taskMu.Lock()
	bt, ok := taskTable[id]
	taskMu.Unlock()
	if !ok {
		return TaskSnapshot{}, false
	}
	return taskView(bt)
}

// StopTask 应用侧终止（与 task_stop 同语义：KillGroup + 出表）。
// false = 任务不存在（已结束出表或未起）。
func StopTask(id string) bool {
	res, err := stopTask(id)
	return err == nil && res["ok"] == true
}

func taskView(b *bgTask) (TaskSnapshot, bool) {
	if b == nil {
		return TaskSnapshot{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	exit := -1
	if b.state != nil {
		exit = b.state.ExitCode()
	}
	dur := time.Since(b.start).Milliseconds()
	if b.done && !b.end.IsZero() {
		dur = b.end.Sub(b.start).Milliseconds()
	}
	return TaskSnapshot{
		ID: b.id, Command: b.cmd, Root: b.root,
		Running: !b.done, ExitCode: exit, DurationMS: dur,
		Output: headTail(b.buf.Bytes()),
	}, true
}

// taskSeqOf id（tN 形态）→ 创建序号；解析失败按 0（排序稳定性兜底）。
func taskSeqOf(id string) int {
	n := 0
	for _, c := range id {
		if c < '0' || c > '9' {
			if c == 't' && n == 0 {
				continue
			}
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
