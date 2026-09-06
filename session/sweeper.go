package session

// 会话清扫域（sweep_test 对位）：TTL 过期清理器（Sweeper.RunOnce，触发 =
// Registry.Create 顺带全量扫）与启动工作区清扫（SweepTmpWorkspaces——孤儿
// 工作区整删 + 两代旧布局迁移，含 sessionEnded 磁盘终态判定的共享小件）。

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"time"
)

// SweepTmpWorkspaces 启动清扫会话工作区（用户域 workspaces/<sid>）：对照
// 磁盘会话（.agent/users/<op>/sessions/<sid>/），无对应会话**或会话已正常
// 结束**（任务收尾即清的主锚点漏网的兜底——崩溃前未及清的 ended 残留）
// 整删；挂起/异常态保留待续（跨重启可恢复）。顺带两代旧布局清场：
// DATA_DIR/workspaces 活会话 rename 搬入用户域（迁移后旧根移除，下次自然
// 空跑）；.tmp/workspaces 整树移除（旧位已弃用，树里全是孤儿）。返回清
// 理/迁移的目录数（serve 启动日志观测用）。
func (r *Registry) SweepTmpWorkspaces() int {
	n := 0
	// 旧布局迁移：DATA_DIR/workspaces/<owner>/<sid> → 用户域（活会话随迁）
	if legacy, err := os.ReadDir(filepath.Join(r.st.Dir(), "workspaces")); err == nil {
		for _, o := range legacy {
			if o.IsDir() {
				n += r.migrateLegacyOwner(o.Name())
			}
		}
		_ = os.RemoveAll(filepath.Join(r.st.Dir(), "workspaces"))
	}
	// 更早布局 .tmp/workspaces 整树移除（计数按会话目录粒度，与主扫口径一致）
	tmpRoot := filepath.Join(r.st.TmpDir(), "workspaces")
	if owners, err := os.ReadDir(tmpRoot); err == nil {
		for _, o := range owners {
			if !o.IsDir() {
				continue
			}
			if wss, err := os.ReadDir(filepath.Join(tmpRoot, o.Name())); err == nil {
				for _, w := range wss {
					if w.IsDir() {
						n++
					}
				}
			}
		}
		_ = os.RemoveAll(tmpRoot)
	}
	// 主扫：用户域非活（无会话/已结束）工作区整删；空壳目录顺手回收
	for _, op := range r.st.ListUsers() {
		root := filepath.Join(r.st.UserTreeDir(op), "workspaces")
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		live := map[string]bool{}
		for _, sid := range r.st.ListUserTreeSessions(op) {
			if !sessionEnded(r.st, op, sid) {
				live[sid] = true
			}
		}
		for _, e := range entries {
			if !e.IsDir() || live[e.Name()] {
				continue
			}
			if os.RemoveAll(filepath.Join(root, e.Name())) == nil {
				n++
			}
		}
		_ = os.Remove(root)                 // 空壳回收（非空自败——活工作区仍在）
		_ = os.Remove(r.st.UserTreeDir(op)) // 同上（sessions/ 等仍在则不动）
	}
	return n
}

// migrateLegacyOwner 旧布局（DATA_DIR/workspaces/<owner>/<sid>）单 owner
// 迁移：待续会话搬用户域 workspaces 同位（新位已有则旧位弃），孤儿/已结束
// 删；返回处理目录数。
func (r *Registry) migrateLegacyOwner(owner string) int {
	src := filepath.Join(r.st.Dir(), "workspaces", owner)
	entries, err := os.ReadDir(src)
	if err != nil {
		return 0
	}
	live := map[string]bool{}
	for _, sid := range r.st.ListUserTreeSessions(owner) {
		if !sessionEnded(r.st, owner, sid) {
			live[sid] = true
		}
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		old := filepath.Join(src, e.Name())
		if !live[e.Name()] {
			_ = os.RemoveAll(old)
			n++
			continue
		}
		dst := filepath.Join(r.st.UserTreeDir(owner), "workspaces", e.Name())
		if _, err := os.Stat(dst); err == nil {
			_ = os.RemoveAll(old) // 新位已有，旧位弃
			n++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			continue // 搬不动留原位（下次再试），不强删
		}
		if err := os.Rename(old, dst); err != nil {
			continue
		}
		n++
	}
	return n
}

// sessionEnded 读磁盘会话终态（session.json state == ended；读不出/解析失败
// 按未结束保守保留——ended 会话的工作区随任务收尾已清，此处是漏网兜底）。
func sessionEnded(st Store, owner, sid string) bool {
	data, ok := st.ReadUserTreeFile(owner, path.Join("sessions", sid, "session.json"))
	if !ok {
		return false
	}
	var rec struct {
		State string `json:"state"`
	}
	return json.Unmarshal(data, &rec) == nil && rec.State == StateEnded
}

// TTL 清理（docs/04：sessions 与 checkpoints 过期自动清理，7 天无活动——按
// UpdatedAt，2026-08-23 从 30 天收紧）。触发 = 新建会话顺带扫（见 Create）。
// 挂起审批的会话保留至超时拒绝后（超时器已把状态翻为 ended——按 ended 清理）。

// SessionTTLVar 会话保留期（测试可缩短）。
var SessionTTLVar = 7 * 24 * time.Hour

// Sweeper 过期清理器（触发 = Registry.Create 新建会话顺带全量扫一轮，
// 2026-08-23 定——无后台定时任务）。
type Sweeper struct {
	st  Store
	reg *Registry
}

// NewSweeper 构造。
func NewSweeper(st Store, reg *Registry) *Sweeper {
	return &Sweeper{st: st, reg: reg}
}

// RunOnce 扫一轮：各用户 sessions/ 下超期（按活动时间 UpdatedAt，7 天无活动）
// 目录整删 + 内存幽灵摘除（内存活跃会话跳过——7 天 TTL 不可能与运行中会话
// 相交，防御性判断）。
func (s *Sweeper) RunOnce(now time.Time) int {
	removed := 0
	for _, op := range s.st.ListUsers() { // 直扫用户域（含已移除成员的历史目录）
		for _, sid := range s.st.ListUserTreeSessions(op) {
			if sess, ok := s.reg.Get(sid); ok {
				state := sess.StateOf()
				if state == StateRunning || state == StatePendingApproval {
					continue // 活跃/挂起保留
				}
			}
			data, ok := s.st.ReadUserTreeFile(op, "sessions/"+sid+"/session.json")
			if !ok {
				continue
			}
			var rec struct {
				UpdatedAt time.Time `json:"updated_at"`
			}
			if json.Unmarshal(data, &rec) != nil {
				continue
			}
			ts := rec.UpdatedAt
			if ts.IsZero() || now.Sub(ts) > SessionTTLVar {
				_ = s.st.RemoveUserTree(op, "sessions/"+sid)
				// 过期会话的工作区一并清（同 Registry.Delete 清理链）
				_ = os.RemoveAll(filepath.Join(s.st.UserTreeDir(op), "workspaces", sid))
				_ = os.RemoveAll(filepath.Join(s.st.TmpDir(), "workspaces", op, sid)) // 旧布局兜底
				// 内存幽灵一并摘除（非活跃态——活跃的已在上方 continue 保留）
				s.reg.mu.Lock()
				delete(s.reg.sessions, sid)
				s.reg.mu.Unlock()
				removed++
			}
		}
	}
	return removed
}
