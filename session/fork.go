package session

// 会话派生族（fork_test/side_test 对位）：sourceRecord 源记录寻址（Fork/
// ForkAt/Side 共用）→ 全量/锚定分叉（Fork/ForkAt/forkOf + spill 外置域整目录
// 复制）与锚定解析小件；Side 辅助对话（继承父历史快照、工作区共享父域）与
// 级联删除寻址（SidesOf/deleteSide）。

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/jumeng/einox/contract"
)

// sourceRecord 源会话记录寻址（Fork/ForkAt/Side 共用）：内存优先（writeMu→mu
// 快照序，与 persist 同串行化点），不在内存读盘重建。allowRunning：Side 允许
// running 源（历史快照持锁安全——主任务执行中问快速问题是辅助对话核心场景）；
// Fork 族拒绝（spill 目录复制与源 reduction 外置写并发无锁覆盖，V1 禁令）。
// 归属不符/未知 sid 返回 nil（Reattach 同款纪律）。
//
// 锁外 marshal 的安全性依赖两层既有不变量（若被打破须改为持锁 marshal）：
// ① s.history 只追加不改写既有元素（recordOf 浅拷贝切片后元素不被原地变更）；
// ② 组装输入经 sanitizeHistory 已是深拷贝改写产物（在途消息对象与 history
// 元素零共享）。persist 的「序列化须持锁」教训针对的是共享指针并发改写，
// 此处两条不变量使锁外 marshal 与锁内等价。
func (r *Registry) sourceRecord(owner, sid string, allowRunning bool) *sessionRecord {
	if s, ok := r.Get(sid); ok {
		if s.Owner != owner || (!allowRunning && s.StateOf() == StateRunning) {
			return nil
		}
		s.writeMu.Lock()
		s.mu.Lock()
		rec := recordOf(s)
		s.mu.Unlock()
		s.writeMu.Unlock()
		return &rec
	}
	rec, ok := readSessionRecord(r.st, owner, sid)
	if !ok || rec.Owner != owner || (!allowRunning && rec.State == StateRunning) {
		return nil
	}
	return &rec
}

// Fork 全量快照分叉（B8）：从既有会话复制当前完整态（事件流+历史+摘要+外置
// 域），生成新 SID 的新会话（= ForkAt 锚 0）。V1 限定源非 running（源 running
// 即返回 nil——升级位须先给 spill 写入与复制立同步点）。分叉体 State=ended
// 占位（Create 同款），Run 由应用发起。寻址同 Reattach 语义：内存优先，不在
// 内存则磁盘重建后再克隆（历史会话分叉是主场景）。挂起域/taskGrant/planSeq/
// 排队消息不带（分叉体无执行体残留，与 Reattach 的不可续降级同裁决；任务期
// 写授权回审批流是安全侧）；spill 外置域整目录复制（history 含外置指针，不
// 复制即 read_file 取回落空）。
func (r *Registry) Fork(owner, sid string) *Session {
	rec := r.sourceRecord(owner, sid, false)
	if rec == nil {
		return nil
	}
	return r.forkOf(rec, 0)
}

// ForkAt 锚定分叉（ZCode「从已完成消息岔出」对位）：anchor = 源会话某条
// session_end 事件 ID（载荷 HistLen>0 才可锚——旧存量零值事件不可锚）；
// anchor=0 即全量快照（Fork 同义）；负数一律 nil（fail-closed——非 0 即
// 要求合法锚）。截断语义：事件截至锚（含）、历史截至锚 HistLen、
// fileChanges/summary 取锚事件载荷重建（锚后账目不随行）、事件
// seq 接锚 ID。锚后 spill 余量照旧整目录复制（无害——callID 寻址不冲突）。
// side 会话不可分叉；源 running/未知锚/数据不一致（HistLen 越界）一律 nil。
func (r *Registry) ForkAt(owner, sid string, anchor int) *Session {
	if anchor == 0 {
		return r.Fork(owner, sid)
	}
	if anchor < 0 {
		return nil
	}
	rec := r.sourceRecord(owner, sid, false)
	if rec == nil {
		return nil
	}
	return r.forkOf(rec, anchor)
}

// forkOf 分叉体构造：record JSON 往返深拷贝（Event.Data/消息对象全量拷贝、
// 零共享指针——与 Reattach 的盘面重建同语义，Event.Data 降为 map 形态亦同；
// 锚定解析在往返后统一以 map 形态进行）→ 可选锚截断 → 新 SID 装配（Reattach
// 构造路径同款，挂起域不装载）→ 血缘 note（分叉体自身首个事件——回放可见
// 血缘）→ spill 复制 → 注册 → 落盘（Reattach 可恢复的完整态）。源会话零增量。
// anchor=0 全量；anchor>0 截断（锚不合法返回 nil）。side 会话不可分叉。
func (r *Registry) forkOf(rec *sessionRecord, anchor int) *Session {
	if rec.ParentSID != "" {
		return nil // side 会话不可分叉
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return nil
	}
	var cp sessionRecord
	if json.Unmarshal(data, &cp) != nil {
		return nil
	}
	detail := "全量快照分叉自会话 " + cp.SID + "（分叉点即源会话当时完整态；工作区不随行，文件成果携带归应用决策）"
	if anchor > 0 {
		se, ok := sessionEndAt(cp.Events, anchor)
		if !ok || se.HistLen <= 0 || se.HistLen > len(cp.Messages) {
			return nil // 未知锚/零值锚（旧存量）/数据不一致——fail-closed
		}
		cp.Events = eventsUpTo(cp.Events, anchor)
		cp.Messages = cp.Messages[:se.HistLen]
		cp.Summary = se.Summary
		cp.FileChanges = fileChangesOf(se.Files)
		detail = "锚定分叉自会话 " + cp.SID + "（锚点事件 #" + strconv.Itoa(anchor) + "；工作区不随行，文件成果携带归应用决策）"
	}
	ns := &Session{
		SID: newSID(), Owner: cp.Owner, Task: cp.Task, Title: cp.Title,
		State: StateEnded, Mode: cp.Mode, Model: cp.Model,
		StartedAt: time.Now(), UpdatedAt: time.Now(),
		Events:  append([]Event(nil), cp.Events...),
		summary: cp.Summary, fileChanges: cp.FileChanges, stoppedCh: make(chan struct{}),
		lastUsedModel: cp.LastUsedModel,
		participants:  append([]contract.Participant(nil), cp.Participants...), // T6 名册继承（快照后各自演化）
	}
	if n := len(ns.Events); n > 0 {
		ns.seq = ns.Events[n-1].ID // 接续末位事件 ID——不接续则新事件撞号（截断后末位 = 锚）
	}
	ns.history = cp.Messages
	ns.Record(contract.EvHarnessNote, contract.HarnessNote{
		Kind: "fork", Title: "会话分叉", Detail: detail,
	})
	r.copySpill(cp.Owner, cp.SID, ns.SID)
	r.mu.Lock()
	r.sessions[ns.SID] = ns
	r.mu.Unlock()
	r.persist(ns)
	return ns
}

// copySpill 外置域整目录复制（sessions/<src>/spill/** → sessions/<dst>/
// spill/**）。读走 os 直扫（Store 无子树枚举面——Sweeper/recall 检索同先
// 例，文件保存是本基座唯一支持的存储形态），写走 Store 唯一写入器（与
// persist 同串行化点）。
func (r *Registry) copySpill(owner, srcSID, dstSID string) {
	root := filepath.Join(r.st.UserTreeDir(owner), "sessions", srcSID, "spill")
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			crel := path.Join(rel, e.Name())
			if e.IsDir() {
				walk(filepath.Join(dir, e.Name()), crel)
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			_ = r.st.WriteUserTreeFile(owner, path.Join("sessions", dstSID, "spill", crel), data)
		}
	}
	walk(root, "")
}

// sessionEndAt 事件流中定位 session_end 事件并还原载荷（内存态 typed 直取；
// JSON 往返后的 map 形态经 marshal 还原）。
func sessionEndAt(events []Event, id int) (contract.SessionEnd, bool) {
	for _, ev := range events {
		if ev.ID != id || ev.Event != contract.EvSessionEnd {
			continue
		}
		return EventAs[contract.SessionEnd](ev) // 双形态统一取值（typed/map）
	}
	return contract.SessionEnd{}, false
}

// eventsUpTo 事件截至 id（含）——事件 ID 单调递增，首条越界即止。
func eventsUpTo(events []Event, id int) []Event {
	out := make([]Event, 0, len(events))
	for _, ev := range events {
		if ev.ID > id {
			break
		}
		out = append(out, ev)
	}
	return out
}

// fileChangesOf 锚事件 Files 载荷 → 变更账目重建（Action/Count 原样）。
func fileChangesOf(files []contract.FileChange) map[string]fileChange {
	if len(files) == 0 {
		return nil
	}
	m := make(map[string]fileChange, len(files))
	for _, f := range files {
		m[f.Path] = fileChange{Action: f.Action, Count: f.Count}
	}
	return m
}

// Side 辅助对话：继承主会话当前历史快照的轻量派生会话（ZCode side chat
// 对位）。快照语义——创建后各自演化，父后续轮不流入；Model/Effort/Mode/
// lastUsedModel 继承；事件流置空（界面从空白开始）+ 血缘 note（Kind: side）
// ；Summary/FileChanges/挂起域/排队消息不带。允许父 running（历史快照持锁
// 安全——主任务执行中问快速问题是核心场景）；spill/工作区由引擎侧父感知
// 共享（engine.wsSID）。side of side/归属不符/未知 sid 返回 nil。
func (r *Registry) Side(owner, sid string) *Session {
	rec := r.sourceRecord(owner, sid, true)
	if rec == nil || rec.ParentSID != "" {
		return nil
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return nil
	}
	var cp sessionRecord
	if json.Unmarshal(data, &cp) != nil {
		return nil
	}
	ns := &Session{
		SID: newSID(), Owner: cp.Owner, State: StateEnded,
		Mode: cp.Mode, Model: cp.Model,
		StartedAt: time.Now(), UpdatedAt: time.Now(),
		history: cp.Messages, parentSID: cp.SID,
		stoppedCh: make(chan struct{}), lastUsedModel: cp.LastUsedModel,
		participants: append([]contract.Participant(nil), cp.Participants...), // T6 名册继承
	}
	ns.Record(contract.EvHarnessNote, contract.HarnessNote{
		Kind: "side", Title: "辅助对话",
		Detail: "继承主会话 " + cp.SID + " 历史快照（创建后各自演化）；工作区与外置域共享主会话",
	})
	r.mu.Lock()
	r.sessions[ns.SID] = ns
	r.mu.Unlock()
	r.persist(ns)
	return ns
}

// SidesOf 主会话的辅助对话清单（内存 + 盘面扫——UI 寻址与级联删除共用；
// Store 无子树枚举面，os 直扫是 copySpill/Sweep 同先例）。
func (r *Registry) SidesOf(owner, sid string) []string {
	var out []string
	seen := map[string]bool{}
	r.mu.Lock()
	for id, s := range r.sessions {
		if s.Owner == owner && s.parentSID == sid && !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	r.mu.Unlock()
	if entries, err := os.ReadDir(filepath.Join(r.st.UserTreeDir(owner), "sessions")); err == nil {
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			data, ok := r.st.ReadUserTreeFile(owner, path.Join("sessions", e.Name(), "session.json"))
			if !ok {
				continue
			}
			var probe struct {
				ParentSID string `json:"parent_sid"`
			}
			if json.Unmarshal(data, &probe) == nil && probe.ParentSID == sid {
				out = append(out, e.Name())
			}
		}
	}
	sort.Strings(out)
	return out
}

// deleteSide 单个辅助对话删除（级联专用；不触工作区——side 无自有工作区，
// 共享父域随父删除整清）。
func (r *Registry) deleteSide(owner, sid string) {
	r.mu.Lock()
	s, ok := r.sessions[sid]
	if ok {
		delete(r.sessions, sid)
	}
	r.mu.Unlock()
	if ok {
		s.Stop()
	}
	_ = r.st.RemoveUserTree(owner, path.Join("sessions", sid))
}
