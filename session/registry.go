package session

// Registry 会话注册表（内存 + 磁盘合并读）：会话生命周期（Create/Get/Delete）
// 与落盘（sessionRecord DTO + persist 写序锁）、进程生命周期（Reattach 盘面
// 续接 / Drain 优雅停机 / RecoverInterrupted 启动孤儿清理）、列表/详情/
// 搜索只读查询面。

import (
	"encoding/json"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/shortid"
	"github.com/jumeng/einox/internal/strutil"
)

// Registry 会话注册表（内存 + 磁盘合并读）。
type Registry struct {
	mu       sync.Mutex
	st       Store
	sessions map[string]*Session
}

// NewRegistry 构造。
func NewRegistry(st Store) *Registry {
	return &Registry{st: st, sessions: map[string]*Session{}}
}

// Store 会话域存储出口（引擎会话域件装配用——计划文档写入走用户域文件面）。
func (r *Registry) Store() Store { return r.st }

// newSID 会话 id（s 前缀——shortid 单点，区别 issue id 形态）。
func newSID() string { return shortid.Hex("s", 4) }

// Create 新会话（默认态 running 由 Run 侧设置；此处先 ended 占位防并发窗）。
// 顺带触发一轮过期清理（2026-08-23 定：新建即触检，替代小时级后台任务——
// 会话只在有人用时累积，新建时机天然自限；全量扫盘轻量）。
func (r *Registry) Create(owner, task, mode string, model contract.UserPrefs) *Session {
	s := &Session{
		SID: newSID(), Owner: owner, Task: strutil.Truncate(task, 16),
		State: StateEnded, Mode: mode, Model: model,
		StartedAt: time.Now(), UpdatedAt: time.Now(), stoppedCh: make(chan struct{}),
	}
	r.mu.Lock()
	r.sessions[s.SID] = s
	r.mu.Unlock()
	NewSweeper(r.st, r).RunOnce(time.Now())
	return s
}

// Get 取会话（仅内存——运行态必在内存；历史会话经 Detail 读盘）。
func (r *Registry) Get(sid string) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[sid]
	return s, ok
}

// Delete 删除契约：停执行 → 内存摘除 → users/<op>/sessions/<sid>/ 与会话
// 工作区（用户域 workspaces/<sid>，含持久子区）整目录移除（停执行在
// 前——运行中的 Run 步间检查即中止；磁盘零残留）。返回 owner 供
// api 层发起人校验（校验失败时调用方不得触达本方法——先 Detail 校验再删）。
func (r *Registry) Delete(owner, sid string) {
	r.mu.Lock()
	s, inMem := r.sessions[sid]
	if inMem {
		delete(r.sessions, sid)
	}
	r.mu.Unlock()
	if inMem && s.Owner != owner {
		// 归属不符：回滚注册表（防御——api 层应已校验）。槽位已被并发
		// Reattach 占用时不回写（旧对象覆盖新对象 = 同 sid 双实体各自落盘，
		// 审查 P3 修补）
		r.mu.Lock()
		if _, occupied := r.sessions[sid]; !occupied {
			r.sessions[sid] = s
		}
		r.mu.Unlock()
		return
	}
	if inMem {
		s.Stop()
	}
	_ = r.st.RemoveUserTree(owner, path.Join("sessions", sid))
	// 会话工作区一并清理（用户域 workspaces/<sid>，含持久子区整删；工作区
	// 外的挂载收口〔如缓存仓 worktree 元数据〕归应用自理）
	_ = os.RemoveAll(filepath.Join(r.st.UserTreeDir(owner), "workspaces", sid))
	_ = os.RemoveAll(filepath.Join(r.st.TmpDir(), "workspaces", owner, sid)) // 旧布局兜底
	// 级联删辅助对话（ZCode 语义：主任务删除即辅助对话永久关闭）。side 无
	// 自有工作区（共享父域），只清记录树。
	for _, sideID := range r.SidesOf(owner, sid) {
		r.deleteSide(owner, sideID)
	}
}

// sessionRecord 落盘 DTO（显式字段——Session 含锁与运行态字段不整体序列化）。
type sessionRecord struct {
	SID           string                 `json:"sid"`
	Owner         string                 `json:"owner"`
	Task          string                 `json:"task"`
	Title         string                 `json:"title"`
	State         string                 `json:"state"`
	Mode          string                 `json:"mode"`
	Model         contract.UserPrefs     `json:"model"`
	LastUsedModel string                 `json:"last_used_model,omitempty"`
	ParentSID     string                 `json:"parent_sid,omitempty"` // 辅助对话父会话（空 = 普通会话）
	StartedAt     time.Time              `json:"started_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
	Events        []Event                `json:"events"`
	Summary       string                 `json:"summary"`
	FileChanges   map[string]fileChange  `json:"file_changes"`
	Messages      []*schema.Message      `json:"messages"`                 // 续聊历史（进程重启后恢复）
	Pending       []QueuedMsg            `json:"pending,omitempty"`        // 排队消息（重启续接——用户补充指令是交互内容，不随进程丢）
	PendingAppID  string                 `json:"pending_app_id,omitempty"` // 挂起审批/提问/计划 ID（重启续接——checkpoint 在盘，决议可续流）
	PendingKind   string                 `json:"pending_kind,omitempty"`   // 挂起类型（approval|ask|plan——续表/超时翻转动作分叉）
	PendingDue    time.Time              `json:"pending_due,omitempty"`    // 挂起截止（超时兜底跨重启——内存计时器随进程丢）
	PendingItems  []string               `json:"pending_items,omitempty"`  // 合并决议卡项标识清单（重启续接——超时批量拒/决议覆盖校验依据）
	TaskGranted   bool                   `json:"task_granted,omitempty"`   // plan 档任务期写授权（重启续接——批准的计划不因进程重启失效）
	PlanSeq       int                    `json:"plan_seq,omitempty"`       // 计划文档序号末号（新提交接续递增）
	Participants  []contract.Participant `json:"participants,omitempty"`   // T6 参与者名册（Fork/Side/Reattach 继承）
	PendingTarget string                 `json:"pending_target,omitempty"` // T6 挂起审批路由目标（重启续接——guard 校验依据）
}

// fileChangesCopy 变更表拷贝（持锁块内调用）。
func fileChangesCopy(s *Session) map[string]fileChange {
	if len(s.fileChanges) == 0 {
		return nil
	}
	out := make(map[string]fileChange, len(s.fileChanges))
	for k, v := range s.fileChanges {
		out[k] = v
	}
	return out
}

// historyForRecord 落盘消息形态：带 parts 的多模态 user 消息拍平为文本
// （text parts 拼接——含附件路径引用）。记录是回放/续聊真源，Content 留空的
// parts 形态对文本消费者不可见；进程内历史仍持 parts（当轮视觉路由直连），
// 重启续接降级为路径引用 + 读图工具消费。
func historyForRecord(msgs []*schema.Message) []*schema.Message {
	out := make([]*schema.Message, len(msgs))
	for i, m := range msgs {
		if len(m.UserInputMultiContent) == 0 {
			out[i] = m
			continue
		}
		cp := *m
		cp.UserInputMultiContent = nil
		var b strings.Builder
		for _, p := range m.UserInputMultiContent {
			if p.Type == schema.ChatMessagePartTypeText {
				b.WriteString(p.Text)
			}
		}
		cp.Content = b.String()
		out[i] = &cp
	}
	return out
}

// recordOf 会话快照 DTO（调用方持 writeMu+mu——与 persist 同锁序同锁段；
// Fork 复用同一构造面）。
func recordOf(s *Session) sessionRecord {
	return sessionRecord{
		SID: s.SID, Owner: s.Owner, Task: s.Task, Title: s.Title,
		State: s.State, Mode: s.Mode, Model: s.Model, LastUsedModel: s.lastUsedModel,
		ParentSID: s.parentSID,
		StartedAt: s.StartedAt, UpdatedAt: s.UpdatedAt,
		Events:       append([]Event(nil), s.Events...),
		Summary:      s.summary,
		FileChanges:  fileChangesCopy(s),
		Messages:     historyForRecord(s.history),
		Pending:      append([]QueuedMsg(nil), s.pendingMsgs...),
		PendingAppID: s.curAppID, PendingKind: s.pendingKind, PendingDue: s.pendingDue,
		PendingItems:  append([]string(nil), s.pendingItems...),
		PendingTarget: s.pendingTarget,
		Participants:  append([]contract.Participant(nil), s.participants...),
		TaskGranted:   s.taskGrant, PlanSeq: s.planSeq,
	}
}

// readSessionRecord 读单个会话 session.json 的全量记录（统一路径拼装与解析；
// 缺失/损坏 = false——各调用点的容错语义保留在调用点）。Reattach/
// LoadHistory/清扫迁移/Fork/检索五处曾各写同序（审查 P3-6）。
func readSessionRecord(st Store, owner, sid string) (sessionRecord, bool) {
	var rec sessionRecord
	data, ok := st.ReadUserTreeFile(owner, path.Join("sessions", sid, "session.json"))
	if !ok {
		return rec, false
	}
	if json.Unmarshal(data, &rec) != nil {
		return rec, false
	}
	return rec, true
}

// persist 落盘 session.json（状态迁移点调用；坏数据容错忽略）。
func (r *Registry) persist(s *Session) {
	if s.Stopped() {
		return // 已删除会话不再落盘（防删除后残留）
	}
	// 写序锁先于快照锁获取：并发 Persist 同一会话（应用端点的决议/排队编辑
	// 落盘 × 引擎泵状态迁移落盘——einox-pm 实测形态）时，快照序即提交序——
	// 旧快照若后提交会覆盖新快照（事件/决议/队列编辑在盘上丢失，重启
	// Reattach 才显形）。锁序 writeMu → mu 全仓唯一，无反转；文件写在 mu 外，
	// Record/订阅扇出不被文件 IO 阻塞。
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	rec := recordOf(s)
	// 序列化须持锁完成：Messages 是共享消息对象的浅拷贝切片，锁外 marshal
	// 会与下一轮 Run 的 sanitizeHistory 原地改写同批消息并发读写（-race 实报
	// 的存量竞态，2026-08-24 移植测试时捕获）。
	data, err := json.MarshalIndent(rec, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return
	}
	// 落盘失败不留静默黑洞（会话真源唯一持久化写——磁盘满/权限错时排障锚点）
	if err := r.st.WriteUserTreeFile(s.Owner, path.Join("sessions", s.SID, "session.json"), data); err != nil {
		log.Printf("session: 会话 %s 落盘失败：%v", s.SID, err)
	}
	// 删除竞态自愈：在途写（标题 goroutine）可能落在 Delete 的 RemoveUserTree
	// 之后重建目录（stopped 即已删除——Stop 唯一调用方是 Registry.Delete），
	// 写后复查已删即收回，窗口收口。
	if s.Stopped() {
		_ = r.st.RemoveUserTree(s.Owner, path.Join("sessions", s.SID))
	}
}

// Persist 导出落盘口（Run 状态迁移调用）。
func (r *Registry) Persist(s *Session) { r.persist(s) }

// Drain 优雅停机收尾（A6）：对全部 running 态会话发取消 → 有界等执行体收尾
// （取消走引擎 interruptUnlessStopped 既有链：终态事件 + 检查点 + 中断注记
// 全落）。到点仍未收尾的会话 SID 如实返回（调用方记日志，不阻塞停机——
// 与强制退出同，但多数场景已收干净）。挂起态（pending_approval）无执行体
// 不在列——跨重启由 RearmPendingTimer 续表；执行体必在内存（Get 语义）。
func (r *Registry) Drain(deadline time.Duration) []string {
	r.mu.Lock()
	var targets []*Session
	for _, s := range r.sessions {
		if s.StateOf() == StateRunning {
			targets = append(targets, s)
		}
	}
	r.mu.Unlock()
	if len(targets) == 0 {
		return nil
	}
	for _, s := range targets {
		s.CancelRun()
	}
	limit := time.Now().Add(deadline)
	for {
		still := drainPending(targets)
		if len(still) == 0 {
			return nil
		}
		if time.Now().After(limit) {
			left := make([]string, 0, len(still))
			for _, s := range still {
				left = append(left, s.SID)
			}
			return left
		}
		for _, s := range still {
			s.CancelRun() // 重发取消（执行体起跑竞态兜底，FlushQueue 同款）
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// drainPending 仍挂执行体的会话（runDone 在场且未关闭）。
func drainPending(ts []*Session) []*Session {
	var out []*Session
	for _, s := range ts {
		done := s.RunDone()
		if done == nil {
			continue
		}
		select {
		case <-done:
		default:
			out = append(out, s)
		}
	}
	return out
}

// SessionListItem 列表条目（活动时间倒序；字段契约 = M3-2 桩 + 2026-08-23 增 updated_at）。
type SessionListItem struct {
	SID       string `json:"sid"`
	Owner     string `json:"owner"`
	State     string `json:"state"`
	Title     string `json:"title"` // Title || Task（存量兼容回退）
	StartedAt string `json:"started_at"`
	UpdatedAt string `json:"updated_at"` // 最新活动时间（列表展示与排序键）
	Summary   string `json:"summary"`
}

// activityTs 活动时间（排序/展示键；零值回退发起时间）。
func activityTs(updated, started time.Time) time.Time {
	if updated.IsZero() {
		return started
	}
	return updated
}

// List 我的会话（mine：内存 + 磁盘合并，内存覆盖同 sid；活动时间倒序）。
func (r *Registry) List(owner string) []SessionListItem {
	return r.listOwner(owner, "")
}

// Search 我的会话关键词过滤（标题或任一用户消息文本包含 q，不区分大小写；
// 活动时间倒序——q 由 api 层 TrimSpace，空串 = 同 List）。
func (r *Registry) Search(owner, q string) []SessionListItem {
	return r.listOwner(owner, strings.ToLower(q))
}

func (r *Registry) listOwner(owner, ql string) []SessionListItem {
	type entry struct {
		item SessionListItem
		ts   time.Time
	}
	seen := map[string]bool{}
	var out []entry

	r.mu.Lock()
	for _, s := range r.sessions {
		if s.Owner != owner {
			continue
		}
		seen[s.SID] = true
		s.mu.Lock()
		if ql != "" && !sessionMatchQ(s, ql) {
			s.mu.Unlock()
			continue
		}
		ts := activityTs(s.UpdatedAt, s.StartedAt)
		out = append(out, entry{SessionListItem{
			SID: s.SID, Owner: s.Owner, State: s.State,
			Title:     titleOrTaskLocked(s),
			StartedAt: s.StartedAt.UTC().Format(time.RFC3339),
			UpdatedAt: ts.UTC().Format(time.RFC3339), Summary: s.summary,
		}, ts})
		s.mu.Unlock()
	}
	r.mu.Unlock()

	// 磁盘历史（进程重启后的存量会话）
	for _, sid := range r.st.ListUserTreeSessions(owner) {
		if seen[sid] {
			continue
		}
		data, ok := r.st.ReadUserTreeFile(owner, path.Join("sessions", sid, "session.json"))
		if !ok {
			continue
		}
		var rec diskItem
		if ql == "" {
			if json.Unmarshal(data, &rec) != nil {
				continue
			}
		} else {
			// 搜索路径：连 events 一起轻解析（只取 user_message 文本）
			var full diskSearchItem
			if json.Unmarshal(data, &full) != nil || !diskMatchQ(&full, ql) {
				continue
			}
			rec = full.diskItem
		}
		ts := activityTs(rec.UpdatedAt, rec.StartedAt)
		out = append(out, entry{SessionListItem{
			SID: rec.SID, Owner: rec.Owner, State: rec.State,
			Title:     titleOrTaskStr(rec.Title, rec.Task),
			StartedAt: rec.StartedAt.UTC().Format(time.RFC3339),
			UpdatedAt: ts.UTC().Format(time.RFC3339), Summary: rec.Summary,
		}, ts})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ts.After(out[j].ts) })
	items := make([]SessionListItem, 0, len(out))
	for _, e := range out {
		items = append(items, e.item)
	}
	return items
}

// SessionDetail 回看/软恢复载荷（字段契约 = M3-2 桩 detail）。
type SessionDetail struct {
	SID       string             `json:"sid"`
	Owner     string             `json:"owner"`
	State     string             `json:"state"`
	Title     string             `json:"title"`
	Mode      string             `json:"mode"`  // 会话当前档位（前端切回恢复 composer 显示）
	Model     contract.UserPrefs `json:"model"` // 会话模型快照（创建时粘住——前端恢复显示用）
	StartedAt string             `json:"started_at"`
	UpdatedAt string             `json:"updated_at"` // 最新活动时间（头部展示）
	Events    []Event            `json:"events"`
}

// Detail 单会话详情（内存优先，缺则读盘；owner 不符由 api 层先校验）。since>0
// 时 events 只含 id>since 的增量（按标签缓存切回的增量追赶——缩 wire 载荷，
// 2026-08-26 会话切换提速）。
func (r *Registry) Detail(sid string, since int) (SessionDetail, bool) {
	r.mu.Lock()
	s, inMem := r.sessions[sid]
	r.mu.Unlock()
	if inMem {
		s.mu.Lock()
		d := SessionDetail{
			SID: s.SID, Owner: s.Owner, State: s.State,
			Mode: s.Mode, Model: s.Model,
			Title:     titleOrTaskLocked(s),
			StartedAt: s.StartedAt.UTC().Format(time.RFC3339),
			UpdatedAt: activityTs(s.UpdatedAt, s.StartedAt).UTC().Format(time.RFC3339),
			Events:    eventsSince(append([]Event(nil), s.Events...), since),
		}
		s.mu.Unlock()
		return d, true
	}
	return SessionDetail{}, false
}

// eventsSince 增量裁剪（since<=0 全量）：事件 id 单调递增，定位首个 >since 的
// 切点返回尾段。
func eventsSince(events []Event, since int) []Event {
	if since <= 0 {
		return events
	}
	for i, ev := range events {
		if ev.ID > since {
			return events[i:]
		}
	}
	return nil
}

// Reattach 磁盘会话续接（进程重启后带 sid 再 POST：注册表重建会话 + 装载
// 历史；内存已有同 sid 直接回）。挂起审批态一并恢复（checkpoint 在盘，approve
// 端点可续流）——running 残留/旧格式挂起（无 pending_app_id，RecoverInterrupted
// 启动已翻，防御分支）落 ended。归属不符返回 nil。
func (r *Registry) Reattach(owner, sid string) *Session {
	r.mu.Lock()
	if s, ok := r.sessions[sid]; ok {
		r.mu.Unlock()
		if s.Owner != owner {
			return nil
		}
		return s
	}
	r.mu.Unlock()

	rec, ok := readSessionRecord(r.st, owner, sid)
	if !ok || rec.Owner != owner {
		return nil
	}
	rec.Model.Effort = contract.NormalizeEffort(rec.Model.Effort) // 存量快照旧值归一：进内存即四档值，detail/回发/引擎全链一致
	st := rec.State
	if st == StateRunning || (st == StatePendingApproval && rec.PendingAppID == "") {
		st = StateEnded // 无执行体残留不可续
	}
	s := &Session{
		SID: rec.SID, Owner: rec.Owner, Task: rec.Task, Title: rec.Title,
		State: st, Mode: rec.Mode, Model: rec.Model,
		StartedAt: rec.StartedAt, UpdatedAt: rec.UpdatedAt,
		Events:  append([]Event(nil), rec.Events...),
		summary: rec.Summary, fileChanges: rec.FileChanges, stoppedCh: make(chan struct{}),
		lastUsedModel: rec.LastUsedModel, parentSID: rec.ParentSID,
		participants: append([]contract.Participant(nil), rec.Participants...), // T6 名册续接
	}
	if st == StatePendingApproval {
		s.curAppID, s.pendingKind, s.pendingDue = rec.PendingAppID, rec.PendingKind, rec.PendingDue
		s.pendingItems = append([]string(nil), rec.PendingItems...) // 合并决议项清单续接（超时批量拒依据）
		s.pendingTarget = rec.PendingTarget                         // T6 路由目标续接
	}
	// seq 接续末位事件 ID——不接续则新事件 ID 从 1 重来，与恢复事件撞号
	if n := len(s.Events); n > 0 {
		s.seq = s.Events[n-1].ID
	}
	s.history = rec.Messages
	s.pendingMsgs = append([]QueuedMsg(nil), rec.Pending...) // 排队消息续接（下一轮前置带回）
	s.taskGrant, s.planSeq = rec.TaskGranted, rec.PlanSeq    // 任务期授权/计划序号续接（执行中重启不丢授权）
	r.mu.Lock()
	r.sessions[s.SID] = s
	r.mu.Unlock()
	return s
}

// LoadHistory 磁盘恢复续聊历史（进程重启后 Run 前调用；无历史 no-op）。
func (r *Registry) LoadHistory(s *Session) {
	rec, ok := readSessionRecord(r.st, s.Owner, s.SID)
	if !ok {
		return
	}
	s.mu.Lock()
	if len(s.history) == 0 {
		s.history = rec.Messages
	}
	s.mu.Unlock()
}

// DetailDisk 磁盘历史会话详情（软恢复/回看：进程重启后；since 语义同 Detail）。
func (r *Registry) DetailDisk(owner, sid string, since int) (SessionDetail, bool) {
	data, ok := r.st.ReadUserTreeFile(owner, path.Join("sessions", sid, "session.json"))
	if !ok {
		return SessionDetail{}, false
	}
	var s Session
	if json.Unmarshal(data, &s) != nil || s.Owner != owner {
		return SessionDetail{}, false
	}
	s.Model.Effort = contract.NormalizeEffort(s.Model.Effort) // 盘面直读回显同规则
	ts := activityTs(s.UpdatedAt, s.StartedAt)
	return SessionDetail{
		SID: s.SID, Owner: s.Owner, State: s.State,
		Mode: s.Mode, Model: s.Model,
		Title:     titleOrTaskStr(s.Title, s.Task),
		StartedAt: s.StartedAt.UTC().Format(time.RFC3339),
		UpdatedAt: ts.UTC().Format(time.RFC3339),
		Events:    eventsSince(s.Events, since),
	}, true
}

// diskItem 列表轻量解析 DTO（不驻留 events/messages）。
type diskItem struct {
	SID       string    `json:"sid"`
	Owner     string    `json:"owner"`
	Task      string    `json:"task"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Summary   string    `json:"summary"`
}

// diskSearchItem 搜索路径盘上解析：diskItem + events 轻扫（只取 user_message 文本）。
type diskSearchItem struct {
	diskItem
	Events []struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	} `json:"events"`
}

// matchFold 不区分大小写子串匹配（ql 已小写）。
func matchFold(s, ql string) bool {
	return strings.Contains(strings.ToLower(s), ql)
}

// userMsgText user_message 事件文本（内存载荷 = UserMsg 结构；盘上重读 = map）。
func userMsgText(d any) string {
	switch v := d.(type) {
	case contract.UserMsg:
		return v.Text
	case map[string]any:
		t, _ := v["text"].(string)
		return t
	}
	return ""
}

// sessionMatchQ 会话搜索命中：标题（Title||Task 回退前）或任一用户消息文本
// 包含关键词（调用方持 s.mu）。
func sessionMatchQ(s *Session, ql string) bool {
	if matchFold(titleOrTaskLocked(s), ql) {
		return true
	}
	for _, ev := range s.Events {
		if ev.Event == contract.EvUserMessage && matchFold(userMsgText(ev.Data), ql) {
			return true
		}
	}
	return false
}

// diskMatchQ 盘上会话搜索命中（events 原始 JSON 只解 user_message.text）。
func diskMatchQ(rec *diskSearchItem, ql string) bool {
	if matchFold(titleOrTaskStr(rec.Title, rec.Task), ql) {
		return true
	}
	for _, ev := range rec.Events {
		if ev.Event != contract.EvUserMessage {
			continue
		}
		var m struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &m) == nil && matchFold(m.Text, ql) {
			return true
		}
	}
	return false
}

// titleOrTaskLocked 标题回退（调用方持 s.mu）。
func titleOrTaskLocked(s *Session) string {
	if s.Title != "" {
		return s.Title
	}
	return s.Task
}

func titleOrTaskStr(title, task string) string {
	if title != "" {
		return title
	}
	return task
}

// listAllCap admin 全量列表上限（倒序截断，防失控）。
const listAllCap = 200

// ListAll 全部用户全部会话（admin 视图）：内存全量 + 各用户磁盘合并（内存覆盖
// 同 sid；含已移除成员的历史目录——ListUsers 直扫语义），活动时间倒序截断。
func (r *Registry) ListAll() []SessionListItem {
	return r.listAll("")
}

// SearchAll admin 全量关键词过滤（语义同 Search，跨全部用户）。
func (r *Registry) SearchAll(q string) []SessionListItem {
	return r.listAll(strings.ToLower(q))
}

func (r *Registry) listAll(ql string) []SessionListItem {
	type entry struct {
		item SessionListItem
		ts   time.Time
	}
	seen := map[string]bool{}
	var out []entry

	r.mu.Lock()
	for _, s := range r.sessions {
		seen[s.SID] = true
		s.mu.Lock()
		if ql != "" && !sessionMatchQ(s, ql) {
			s.mu.Unlock()
			continue
		}
		ts := activityTs(s.UpdatedAt, s.StartedAt)
		out = append(out, entry{SessionListItem{
			SID: s.SID, Owner: s.Owner, State: s.State,
			Title:     titleOrTaskLocked(s),
			StartedAt: s.StartedAt.UTC().Format(time.RFC3339),
			UpdatedAt: ts.UTC().Format(time.RFC3339), Summary: s.summary,
		}, ts})
		s.mu.Unlock()
	}
	r.mu.Unlock()

	for _, op := range r.st.ListUsers() {
		for _, sid := range r.st.ListUserTreeSessions(op) {
			if seen[sid] {
				continue
			}
			data, ok := r.st.ReadUserTreeFile(op, path.Join("sessions", sid, "session.json"))
			if !ok {
				continue
			}
			var rec diskItem
			if ql == "" {
				if json.Unmarshal(data, &rec) != nil || rec.SID == "" {
					continue
				}
			} else {
				var full diskSearchItem
				if json.Unmarshal(data, &full) != nil || full.SID == "" || !diskMatchQ(&full, ql) {
					continue
				}
				rec = full.diskItem
			}
			ts := activityTs(rec.UpdatedAt, rec.StartedAt)
			out = append(out, entry{SessionListItem{
				SID: rec.SID, Owner: rec.Owner, State: rec.State,
				Title:     titleOrTaskStr(rec.Title, rec.Task),
				StartedAt: rec.StartedAt.UTC().Format(time.RFC3339),
				UpdatedAt: ts.UTC().Format(time.RFC3339), Summary: rec.Summary,
			}, ts})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ts.After(out[j].ts) })
	if len(out) > listAllCap {
		out = out[:listAllCap]
	}
	items := make([]SessionListItem, 0, len(out))
	for _, e := range out {
		items = append(items, e.item)
	}
	return items
}

// RecoverInterrupted 进程启动孤儿清理：落盘仍处 running 的会话已无执行体
// （进程重启丢内存态，watch/续聊皆死胡同），统一翻 error 终态并补一条中断
// error 事件——列表显示「出错」，回放可见中断原因，点击可正常回看/接续。
// 挂起审批（pending_approval）例外——checkpoint 在盘、决议可续流（2026-08-24
// 定案：重启前挂起的审批卡跨重启可批准）：带 pending_app_id 且未到截止的保留
// 挂起（等 owner 决议）；停机期间已到截止的镜像超时器动作（approval_timeout/
// ask_timeout 事件 + ended）；旧格式（无 pending_app_id）按 running 同样翻 error。
// 返回修复数（serve 启动调用一次）。
func (r *Registry) RecoverInterrupted() int {
	fixed := 0
	now := time.Now()
	for _, op := range r.st.ListUsers() {
		for _, sid := range r.st.ListUserTreeSessions(op) {
			rec, ok := readSessionRecord(r.st, op, sid)
			if !ok || rec.SID == "" {
				continue
			}
			rel := path.Join("sessions", sid, "session.json")
			if rec.State == StatePendingApproval && rec.PendingAppID != "" {
				if rec.PendingDue.IsZero() || rec.PendingDue.After(now) {
					continue // 可续挂起：未到截止（或无截止信息）——保留待决议
				}
				// 停机期间到点：镜像超时器（事件形态与 startApprovalTimer 到点一致，
				// 回放重建卡片终态的真源）
				next := 1
				if n := len(rec.Events); n > 0 {
					next = rec.Events[n-1].ID + 1
				}
				ev, payload := "approval_timeout", map[string]string{
					"approval_id": rec.PendingAppID, "reason": "审批超时，自动拒绝",
				}
				if rec.PendingKind == "ask" {
					ev, payload = "ask_timeout", map[string]string{
						"ask_id": rec.PendingAppID, "reason": "提问超时未作答",
					}
				}
				if rec.PendingKind == "plan" {
					// 与在线超时器同形态（timers.go plan 分支）——此前落默认
					// approval_timeout，plan 卡回放终态错标
					ev, payload = "plan_timeout", map[string]string{
						"plan_id": rec.PendingAppID, "reason": "计划审批超时，自动拒绝",
					}
				}
				rec.Events = append(rec.Events, Event{ID: next, Event: ev, Data: payload})
				rec.State = StateEnded
				rec.UpdatedAt = now
				out, err := json.MarshalIndent(rec, "", "  ")
				if err != nil {
					continue
				}
				if r.st.WriteUserTreeFile(op, rel, out) == nil {
					fixed++
				}
				continue
			}
			if rec.State != StateRunning && rec.State != StatePendingApproval {
				continue
			}
			next := 1 // 事件 ID 接续末位（回放去重依赖连续性）
			if n := len(rec.Events); n > 0 {
				next = rec.Events[n-1].ID + 1
			}
			rec.Events = append(rec.Events, Event{
				ID: next, Event: "error",
				Data: contract.ErrorOut{Code: contract.ErrCodeServer, Message: "进程重启，会话中断"},
			})
			rec.State = StateError
			rec.UpdatedAt = time.Now()
			out, err := json.MarshalIndent(rec, "", "  ")
			if err != nil {
				continue
			}
			if r.st.WriteUserTreeFile(op, rel, out) == nil {
				fixed++
			}
		}
	}
	return fixed
}
