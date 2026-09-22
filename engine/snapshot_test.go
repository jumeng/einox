package engine

// C4 工作区快照/恢复（设计件 findings/2026-09-22-snapshot-and-autobg-design.md）。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jumeng/einox/contract"
)

func TestWorkspaceSnapshotRestore(t *testing.T) {
	m := newTestManager(t, nil)
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{})
	ws := m.workspaceOf(s)
	os.MkdirAll(filepath.Join(ws, "repos"), 0o755) // keep 形态子区
	os.WriteFile(filepath.Join(ws, "a.txt"), []byte("v1"), 0o644)
	os.WriteFile(filepath.Join(ws, "repos", "r.txt"), []byte("r1"), 0o644)

	if err := m.SnapshotWorkspace(s, "s1"); err != nil {
		t.Fatalf("快照失败：%v", err)
	}
	// 快照后改工作区（含 keep 子区）+ 新增文件
	os.WriteFile(filepath.Join(ws, "a.txt"), []byte("v2"), 0o644)
	os.WriteFile(filepath.Join(ws, "repos", "r.txt"), []byte("r2"), 0o644)
	os.WriteFile(filepath.Join(ws, "new.txt"), []byte("n"), 0o644)

	if err := m.RestoreWorkspace(s, "s1"); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "a.txt")); string(b) != "v1" {
		t.Fatalf("a.txt 应回到快照态：%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "repos", "r.txt")); string(b) != "r1" {
		t.Fatalf("keep 子区应同被恢复：%s", b)
	}
	if _, err := os.Stat(filepath.Join(ws, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("快照后新增文件应随恢复移除")
	}
	// 快照域不受任务收尾 wipe 影响
	m.wipeWorkspace(s)
	if _, err := os.Stat(filepath.Join(m.snapshotsDirOf(s), "s1")); err != nil {
		t.Fatalf("快照域不应随任务收尾被清：%v", err)
	}
	// 清单与 Drop
	if tags := m.WorkspaceSnapshots(s); len(tags) != 1 || tags[0] != "s1" {
		t.Fatalf("清单应含 s1：%v", tags)
	}
	if err := m.DropWorkspaceSnapshot(s, "s1"); err != nil {
		t.Fatalf("Drop：%v", err)
	}
	if tags := m.WorkspaceSnapshots(s); len(tags) != 0 {
		t.Fatalf("Drop 后清单空：%v", tags)
	}
}

func TestWorkspaceSnapshotGuards(t *testing.T) {
	m := newTestManager(t, nil)
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{})
	// 非法 tag
	if err := m.SnapshotWorkspace(s, "a/b"); err == nil {
		t.Fatal("分隔符 tag 应拒")
	}
	// 运行中拒恢复
	s.SetState("running")
	if err := m.RestoreWorkspace(s, "x"); err == nil || filepath.IsAbs(err.Error()) {
		t.Fatalf("运行中应拒恢复：%v", err)
	}
	// 未知 tag
	s.SetState("ended")
	if err := m.RestoreWorkspace(s, "ghost"); err == nil {
		t.Fatal("未知 tag 应拒")
	}
}
