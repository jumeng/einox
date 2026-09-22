// 工作区快照/恢复（C4 中性件，设计件 findings/2026-09-22-snapshot-and-
// autobg-design.md §1）：undo 语义的用户通道——机制归基座（拷贝/守卫/事件），
// 时机归应用（桌面应用撤销按钮接 Manager 公开方法；zcode rewind 的事件溯源/
// artifact/fork 重映射体系归应用生态面）。快照域 = 用户域 sessions/<wsSID>/
// snapshots/<tag>/（spill 同级——不受任务收尾 wipe 影响，随会话删除/过期/孤儿
// 清扫整清）。不给模型工具面：undo 是用户操作，模型自作主张恢复工作区是
// 危险面（裁决记档）。
package engine

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// snapshotsDirOf 快照域（spill 同级；side 共享父工作区 → 快照域同寻址）。
func (m *Manager) snapshotsDirOf(s *session.Session) string {
	return filepath.Join(m.reg.Store().UserTreeDir(s.Owner), "sessions", m.wsSID(s), "snapshots")
}

// SnapshotWorkspace 全量快照会话工作区（含 WorkspaceKeep 持久子区——恢复完整
// 语义）到 snapshots/<tag>/。tag = 单段路径名。原子落位（tmp + rename）；
// symlink 跳过（工作区产物不应有 symlink，防逃逸面）。落 harness_note
// （Kind=snapshot）供前端观察。
func (m *Manager) SnapshotWorkspace(s *session.Session, tag string) error {
	if !tools.ValidTopDir(tag) {
		return fmt.Errorf("快照 tag 须为单段路径名（非空、不含分隔符、非 . / ..）：%q", tag)
	}
	ws := m.workspaceOf(s)
	dst := filepath.Join(m.snapshotsDirOf(s), tag)
	tmp := dst + ".tmp-snap"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	n, skipped, err := copyTree(ws, tmp)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("快照拷贝失败：%w", err)
	}
	if err := os.RemoveAll(dst); err != nil { // 同名快照覆盖
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	title := fmt.Sprintf("已快照工作区（%s，%d 个文件）", tag, n)
	if skipped > 0 {
		title += fmt.Sprintf("，跳过 %d 个符号链接", skipped)
	}
	s.Record(contract.EvHarnessNote, contract.HarnessNote{Kind: "snapshot", Title: title})
	return nil
}

// RestoreWorkspace 恢复工作区到快照时点。守卫：仅 idle 态（StateEnded/StateError
// 外拒——运行中恢复撕裂在途写面）；恢复 = Wipe 全工作区（keep 子区整体替换
// 回快照态）后全量拷回。非原子（清+拷，中途失败如实报错——应用可重试或换
// 快照）。落 harness_note（Kind=restore）。
func (m *Manager) RestoreWorkspace(s *session.Session, tag string) error {
	if !tools.ValidTopDir(tag) {
		return fmt.Errorf("快照 tag 须为单段路径名：%q", tag)
	}
	switch s.StateOf() {
	case session.StateEnded, session.StateError:
	default:
		return fmt.Errorf("会话运行中（%s）拒绝恢复工作区——撕裂在途写面；请在空闲时恢复", s.StateOf())
	}
	src := filepath.Join(m.snapshotsDirOf(s), tag)
	if st, err := os.Lstat(src); err != nil || !st.IsDir() {
		return fmt.Errorf("快照不存在：%s（WorkspaceSnapshots 查可用清单）", tag)
	}
	ws := m.workspaceOf(s)
	// 全清（不保留 keep——keep 子区在快照内，整体替换回快照态）
	entries, err := os.ReadDir(ws)
	if err == nil {
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(ws, e.Name()))
		}
	}
	n, _, err := copyTree(src, ws)
	if err != nil {
		return fmt.Errorf("恢复拷贝失败（工作区可能处于半恢复态，可重试或换快照）：%w", err)
	}
	s.Record(contract.EvHarnessNote, contract.HarnessNote{
		Kind:  "restore",
		Title: fmt.Sprintf("已恢复工作区到快照 %s（%d 个文件）", tag, n),
	})
	return nil
}

// WorkspaceSnapshots 可用快照 tag 清单（字典序；无 = 空）。
func (m *Manager) WorkspaceSnapshots(s *session.Session) []string {
	des, err := os.ReadDir(m.snapshotsDirOf(s))
	if err != nil {
		return nil
	}
	var tags []string
	for _, d := range des {
		if d.IsDir() && tools.ValidTopDir(d.Name()) {
			tags = append(tags, d.Name())
		}
	}
	sort.Strings(tags)
	return tags
}

// DropWorkspaceSnapshot 删除单快照（应用清理面）。
func (m *Manager) DropWorkspaceSnapshot(s *session.Session, tag string) error {
	if !tools.ValidTopDir(tag) {
		return fmt.Errorf("快照 tag 须为单段路径名：%q", tag)
	}
	dst := filepath.Join(m.snapshotsDirOf(s), tag)
	if st, err := os.Lstat(dst); err != nil || !st.IsDir() {
		return fmt.Errorf("快照不存在：%s", tag)
	}
	return os.RemoveAll(dst)
}

// copyTree 目录树全量拷贝（保留文件 mode；symlink 跳过计数——ResolveUnder
// 的圈禁面含 symlink 防穿越，快照不跟随）。返回（文件数，跳过 symlink 数）。
func copyTree(src, dst string) (int, int, error) {
	n, skipped := 0, 0
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil || rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			skipped++
			return nil
		case d.Type().IsRegular():
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			if err := copyFile(p, target, info.Mode().Perm()); err != nil {
				return err
			}
			n++
			return nil
		default:
			return nil // 套接字/管道等特殊文件跳过
		}
	})
	return n, skipped, err
}

// copyFile 单文件拷贝（mode 保留）。
func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
