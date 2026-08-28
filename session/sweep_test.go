package session

// 会话工作区清扫回归（自产品 tmpworkspace_test 迁入）：启动孤儿清扫（无会话
// 残留删/活会话保留/已结束残留清）、旧布局一次性迁移（DATA_DIR/workspaces →
// 用户域）、更早布局 .tmp/workspaces 整树移除、删除级联清工作区。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
)

// mkDiskSession 造磁盘会话记录（state 供 SweepTmpWorkspaces 的 sessionEnded
// 判定）。
func mkDiskSession(t *testing.T, st *tstore.Store, owner, sid, state string) {
	t.Helper()
	if err := st.WriteUserTreeFile(owner, filepath.Join("sessions", sid, "session.json"),
		[]byte(`{"state":"`+state+`","updated_at":"2026-08-24T00:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}
}

// userWs 用户域会话工作区路径（users/<op>/workspaces/<sid>）。
func userWs(st *tstore.Store, owner, sid string) string {
	return filepath.Join(st.UserTreeDir(owner), "workspaces", sid)
}

// mkWorkspace 造一个带内容的工作区目录（spill 文件证内容随迁）——ws 即工作区
// 根（用户域用 userWs 拼，旧布局自拼 <base>/workspaces/<owner>/<sid>）。
func mkWorkspace(t *testing.T, ws string) string {
	t.Helper()
	dir := filepath.Join(ws, "spill")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "01.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestSweepTmpWorkspaces(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)

	// 活会话 s111：用户域已有工作区 → 保留
	mkDiskSession(t, st, "张三", "s111", "running")
	keep := mkWorkspace(t, userWs(st, "张三", "s111"))
	// 孤儿 s222：有工作区、无会话 → 清
	orphan := mkWorkspace(t, userWs(st, "张三", "s222"))
	// 已结束 s555：会话记录 state=ended → 工作区属漏网残留 → 清
	mkDiskSession(t, st, "张三", "s555", "ended")
	ended := mkWorkspace(t, userWs(st, "张三", "s555"))
	// 旧布局待续会话 s444：DATA_DIR/workspaces 有 → rename 迁入用户域（内容随迁）
	mkDiskSession(t, st, "张三", "s444", "pending_approval")
	mkWorkspace(t, filepath.Join(st.Dir(), "workspaces", "张三", "s444"))
	// 旧布局孤儿 s333 → 清
	legacyDead := mkWorkspace(t, filepath.Join(st.Dir(), "workspaces", "张三", "s333"))

	if n := reg.SweepTmpWorkspaces(); n != 4 {
		t.Fatalf("应处理 4 个目录（孤儿/已结束/旧待续/旧孤儿）：%d", n)
	}
	if _, err := os.Stat(filepath.Join(keep, "spill", "01.md")); err != nil {
		t.Fatalf("活会话工作区应保留：%v", err)
	}
	if _, err := os.Stat(orphan); err == nil {
		t.Fatal("孤儿工作区应清理")
	}
	if _, err := os.Stat(ended); err == nil {
		t.Fatal("已结束会话的工作区残留应清理")
	}
	if _, err := os.Stat(filepath.Join(userWs(st, "张三", "s444"), "spill", "01.md")); err != nil {
		t.Fatalf("旧布局活会话应迁入用户域（内容随迁）：%v", err)
	}
	if _, err := os.Stat(legacyDead); err == nil {
		t.Fatal("旧布局孤儿应清理")
	}
	if _, err := os.Stat(filepath.Join(st.Dir(), "workspaces")); err == nil {
		t.Fatal("旧布局根应移除")
	}

	// 幂等：第二轮应 0
	if n := reg.SweepTmpWorkspaces(); n != 0 {
		t.Fatalf("第二轮应无处理：%d", n)
	}
}

func TestSweepTmpWorkspacesEmptyRoot(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)

	// 全孤儿：清扫后用户域空壳连根回收（workspaces/ 与 users/<op>）
	mkWorkspace(t, userWs(st, "李四", "s999"))
	if n := reg.SweepTmpWorkspaces(); n != 1 {
		t.Fatalf("应清 1 个孤儿：%d", n)
	}
	if _, err := os.Stat(filepath.Join(st.UserTreeDir("李四"), "workspaces")); err == nil {
		t.Fatal("空壳 workspaces/ 应回收")
	}
	if _, err := os.Stat(st.UserTreeDir("李四")); err == nil {
		t.Fatal("空壳用户目录应连根回收")
	}
}

// TestSweepRemovesOldTmpTree 更早布局 .tmp/workspaces 整树移除：迁移目标已是
// 用户域，旧树全是孤儿——即使对应会话仍在续（挂起态）也不保留。
func TestSweepRemovesOldTmpTree(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)

	mkDiskSession(t, st, "张三", "s111", "pending_approval")
	mkWorkspace(t, filepath.Join(st.TmpDir(), "workspaces", "张三", "s111"))
	if n := reg.SweepTmpWorkspaces(); n != 1 {
		t.Fatalf("应清 1 个旧位工作区：%d", n)
	}
	if _, err := os.Stat(filepath.Join(st.TmpDir(), "workspaces")); err == nil {
		t.Fatal("旧 .tmp/workspaces 应整树移除")
	}
}

// TestDeleteCascadesWorkspaces 删除级联清会话工作区（含 repos/ 挂载——
// 会话级持久随会话删除整清）。
func TestDeleteCascadesWorkspaces(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	s := reg.Create("张三", "任务", "manual", contract.UserPrefs{})
	ws := userWs(st, "张三", s.SID)
	_ = os.MkdirAll(filepath.Join(ws, "repos", "base-app"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "repos", "base-app", "a"), []byte("x"), 0o644)
	reg.Delete("张三", s.SID)
	if _, err := os.Stat(ws); err == nil {
		t.Fatal("会话删除应级联清工作区（含 repos/ 挂载）")
	}
}
