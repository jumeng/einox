// Package session 是会话域（自产品 internal/agent/session.go 迁入）：内存
// 注册表 + users/<op>/sessions/<sid>/session.json 落盘（用户隔离）。会话记录 =
// 发起人/状态/模式/模型快照/任务摘要 + 事件流（回看端点原样输出，应用层走
// 自有传输管线渲染）。存储经 Store 接口注入——数据面（唯一文件写入器）在应用。
//
// 状态：running | pending_approval | ended | error。
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
)

// 会话状态常量（契约形态）。
const (
	StateRunning         = contract.StateRunning
	StatePendingApproval = contract.StatePendingApproval
	StateEnded           = contract.StateEnded
	StateError           = contract.StateError
)

// Event 已发事件（回放端点原样输出；契约形态）。
type Event = contract.Event

// Store 会话域存储面（应用注入——产品 FileStore 结构性满足）。
type Store interface {
	ReadUserTreeFile(operator, rel string) ([]byte, bool)
	WriteUserTreeFile(operator, rel string, data []byte) error
	RemoveUserTree(operator, rel string) error
	// UserTreeDir 用户域根绝对路径（.agent/users/<op>；会话工作区
	// workspaces/<sid> 在其下——Delete/Sweep 清理链用）。契约：须为本地
	// 文件系统路径——Sweeper 与 recall 检索（persist_read.go）直用 os 操作，
	// 文件保存会话是本基座唯一支持的存储形态（无 DB 型实现需求）。
	UserTreeDir(operator string) string
	ListUserTreeSessions(operator string) []string
	ListUsers() []string
	TmpDir() string
	Dir() string
}

// Session 会话（Events 即消息流真源；mu 保护并发 emit/删除竞态）。
type Session struct {
	mu        sync.Mutex
	SID       string             `json:"sid"`
	Owner     string             `json:"owner"`
	Scope     string             `json:"scope"` // 关联 scope 展示（默认需求池；M4 采集场景细化）
	Task      string             `json:"task"`  // 会话列表任务摘要（首条用户消息截断）
	State     string             `json:"state"`
	Mode      string             `json:"mode"`
	Title     string             `json:"title"` // LLM 总结标题（首轮收尾异步生成；空 = 回退 Task 截断）
	Model     contract.UserPrefs `json:"model"` // {model 复合键, effort} 快照（会话粘住）
	StartedAt time.Time          `json:"started_at"`
	UpdatedAt time.Time          `json:"updated_at"`
	Events    []Event            `json:"events"`

	// History 跨轮消息历史（续聊回传模型——adk checkpoint 仅覆盖中断/取消恢复，
	// 正常续聊由调用方自持历史，M3-3 实测定案；不进 Events/回放载荷）
	history []*schema.Message

	seq         int
	subs        map[chan Event]bool   // 旁观订阅（M5-2 SSE 多订阅；record 持锁扇出）
	pendingMsgs []QueuedMsg           // steering 待注入（可编辑/删除——zcode 形态；下一轮 Run 前置带回）
	summary     string                // 列表摘要（最后一段 assistant 文本截断）
	fileChanges map[string]fileChange // 会话累计文件变更（工具层报备；session_end 载荷与落盘）
	stopped     bool                  // 删除置位：步间检查即中止，emit 屏蔽
	stoppedCh   chan struct{}         // close 一次；唤醒挂起等待
	cancel      context.CancelFunc
	runDone     chan struct{} // 执行体结束信号（BeginRun 创建；Run/Resume defer 关——FlushQueue 等待接管锚点）
	titleCh     chan struct{} // 标题异步生成在途信号（MarkTitleFlight 置；goroutine 收尾 close——Run 后在途写可 join 的唯一窗口）
	flushing    bool          // 立即处理排队标记（打断形态分叉：interrupted 事件而非 ABORTED 错误）
	writeMu     sync.Mutex    // persist 写序锁（持 mu 时获取——快照序即落盘提交序，见 Registry.persist）

	// 审批域（M3-5）+ ask_user 域（P1a——与审批共用挂起通道）
	// decisions 按 item_id 的决议表（H4-2 合并决议多槽；"" 键 = 旧单决议——
	// plan/超时兜底与升级前 checkpoint 重放共用）
	decisions    map[string]*ApprovalDecision
	curAppID     string       // 挂起中的审批/提问 ID（空 = 无）
	pendingKind  string       // 挂起类型（"approval"|"ask"——跨重启续表用）
	pendingDue   time.Time    // 挂起截止时刻（超时兜底跨重启——进程重启丢内存计时器）
	pendingItems []string     // 合并决议卡项标识清单（kind=approval；超时批量拒与端点覆盖校验依据）
	askDecision  *AskDecision // ask_user 作答（answer 端点写入；Resume 消费后清空）
	turnGrant    bool         // plan 档本轮写授权（首个批准置位；session_end 清零）
	taskGrant    bool         // plan 档任务期写授权（计划批准置位；任务成功收尾/换档/新计划提交清零）
	planSeq      int          // 计划文档序号（submit_plan 自增取号；修订递增留痕）
	turnUserMsg  string       // 轮次用户消息（跨审批中断保留）
	notifySpent  int          // 后台通知连续自续已花费预算（W-3 自激护栏；用户消息消费清零，通知自身不恢复）
}

// ApprovalDecision 审批决议（approve 端点 → 包装工具消费；契约形态）。
type ApprovalDecision = contract.ApprovalDecision

// AskDecision ask_user 作答（answer 端点 → askuser 工具消费；契约形态）。
type AskDecision = contract.AskDecision

// SetAskDecision 登记作答（answer 端点调用）。
func (s *Session) SetAskDecision(d AskDecision) {
	s.mu.Lock()
	s.askDecision = &d
	s.mu.Unlock()
}

// TakeAskDecision 取走作答（Resume 消费；取后清空防复用）。
func (s *Session) TakeAskDecision() *AskDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.askDecision
	s.askDecision = nil
	return d
}

// RecordAskDecision 作答回执落流（answer 端点在 SetAskDecision 后调用——
// 切回/回放重建提问卡终态的真源）。
func (s *Session) RecordAskDecision(askID string, d AskDecision) {
	s.Record("ask_decision", map[string]any{"ask_id": askID, "answers": d.Answers, "free_text": d.FreeText})
}

// SetDecision 登记决议（approve 端点调用；幂等覆盖前值无意义——单审批通道）。
// 旧单决议路径（plan 决议/超时兜底/升级前重放）：落 "" 槽。
func (s *Session) SetDecision(d ApprovalDecision) {
	s.mu.Lock()
	if s.decisions == nil {
		s.decisions = map[string]*ApprovalDecision{}
	}
	s.decisions[""] = &d
	s.mu.Unlock()
}

// SetDecisionFor 按项登记决议（合并决议多槽——approve 端点批量形态逐项写入）。
func (s *Session) SetDecisionFor(itemID string, d ApprovalDecision) {
	if itemID == "" {
		s.SetDecision(d)
		return
	}
	s.mu.Lock()
	if s.decisions == nil {
		s.decisions = map[string]*ApprovalDecision{}
	}
	s.decisions[itemID] = &d
	s.mu.Unlock()
}

// TakeDecisionFor 按项取走决议（Resume 重放时各审批工具按保存态 item_id
// 领各自的；取后清空防复用；无决议 = nil → fail-closed 拒绝）。
func (s *Session) TakeDecisionFor(itemID string) *ApprovalDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	if itemID == "" || s.decisions == nil {
		return s.takeDecisionLocked()
	}
	d := s.decisions[itemID]
	delete(s.decisions, itemID)
	return d
}

// HasPendingDecision 挂起决议/作答是否已登记。决议到达（SetDecision/
// SetDecisionFor/SetAskDecision）先于超时到点或 Resume 停表时，超时兜底
// 须让位——覆盖已到达的决议等于把用户的 approve 改写成超时拒绝。审批看
// "" 槽与挂起项槽（批量决议逐项写入的非原子窗口：任一到达即让位，未决项
// 由 Resume 重放 fail-closed 兜底）；提问看 askDecision。
func (s *Session) HasPendingDecision() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.askDecision != nil {
		return true
	}
	if s.decisions == nil {
		return false
	}
	if _, ok := s.decisions[""]; ok {
		return true
	}
	for _, id := range s.pendingItems {
		if _, ok := s.decisions[id]; ok {
			return true
		}
	}
	return false
}

// RecordDecision 决议回执落流（approve 端点在 SetDecision 后调用）。事件流是
// 切回/回放重建审批卡终态的真源——决议只 Set 不落流，卡片永远停在待审批态。
func (s *Session) RecordDecision(appID string, d ApprovalDecision) {
	s.Record("approval_decision", contract.DecisionOut{ApprovalID: appID, Approve: d.Approve, Reason: d.Reason})
}

// TakeDecision 取走决议（Resume 消费；取后清空防复用）。
func (s *Session) TakeDecision() *ApprovalDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takeDecisionLocked()
}

// takeDecisionLocked 取走 "" 槽决议（持锁块内调用）。
func (s *Session) takeDecisionLocked() *ApprovalDecision {
	if s.decisions == nil {
		return nil
	}
	d := s.decisions[""]
	delete(s.decisions, "")
	return d
}

// SetPendingApproval 挂起登记（approval_request 已发；空 id = 清挂起态）。
func (s *Session) SetPendingApproval(appID string) {
	s.mu.Lock()
	s.curAppID = appID
	if appID != "" {
		s.State = StatePendingApproval
	} else {
		s.curAppID = ""
		s.pendingKind = ""
		s.pendingDue = time.Time{}
		s.pendingItems = nil
	}
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// PendingAppID 当前挂起审批 ID。
func (s *Session) PendingAppID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.curAppID
}

// SetPendingDue 挂起计时元数据登记（startApprovalTimer 挂表时同步调用——
// 挂起态落盘在 finishOf 其后，元数据随之持久；跨重启续表依据）。
func (s *Session) SetPendingDue(kind string, due time.Time) {
	s.mu.Lock()
	if s.curAppID != "" {
		s.pendingKind, s.pendingDue = kind, due
	}
	s.mu.Unlock()
}

// PendingDueOf 挂起计时元数据读取（RearmPendingTimer 续表用）。
func (s *Session) PendingDueOf() (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingKind, s.pendingDue
}

// SetPendingItems 合并决议卡项标识清单登记（pump 聚合发卡后调用；与
// SetPendingApproval 同生命周期——清挂起态时一并清空）。
func (s *Session) SetPendingItems(ids []string) {
	s.mu.Lock()
	s.pendingItems = ids
	s.mu.Unlock()
}

// PendingItems 项标识清单快照（超时批量拒 / approve 端点覆盖校验）。
func (s *Session) PendingItems() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.pendingItems...)
}

// GrantTurn plan 档本轮写授权置位 / 查询 / 清零。
func (s *Session) GrantTurn()        { s.mu.Lock(); s.turnGrant = true; s.mu.Unlock() }
func (s *Session) TurnGranted() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.turnGrant }
func (s *Session) ClearTurnGrant()   { s.mu.Lock(); s.turnGrant = false; s.mu.Unlock() }

// GrantTask plan 档任务期写授权置位 / 查询 / 清零（计划批准置位；任务成功
// 收尾、档位变更、新计划提交时清零。不在轮末无条件清——轮次预算耗尽后
// 「继续」仍在授权期内，兑现「批准计划后一口气完成」）。
func (s *Session) GrantTask()        { s.mu.Lock(); s.taskGrant = true; s.mu.Unlock() }
func (s *Session) TaskGranted() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.taskGrant }
func (s *Session) ClearTaskGrant()   { s.mu.Lock(); s.taskGrant = false; s.mu.Unlock() }

// NextPlanSeq 计划文档序号自增取号（提交即定号——文档先落盘再挂起，跨重启
// 由 sessionRecord.PlanSeq 接续）。
func (s *Session) NextPlanSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.planSeq++
	return s.planSeq
}

// RecordPlanDecision 计划决议回执落流（approve 端点按 pending_kind=plan 分叉
// 调用；切回/回放重建计划卡终态的真源）。
func (s *Session) RecordPlanDecision(planID string, d ApprovalDecision) {
	s.Record(contract.EvPlanDecision, contract.PlanDecisionOut{PlanID: planID, Approve: d.Approve, Reason: d.Reason})
}

// SetTurnUserMsg / TakeTurnUserMsg 轮次用户消息（跨审批中断保留——Resume 完成
// 时随 assistant 终态一并入历史）。
func (s *Session) SetTurnUserMsg(msg string) {
	s.mu.Lock()
	s.turnUserMsg = msg
	s.mu.Unlock()
}

func (s *Session) TakeTurnUserMsg() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := s.turnUserMsg
	s.turnUserMsg = ""
	return msg
}

// TurnUserMsgOf 轮次用户消息读取（标题生成输入）。
func (s *Session) TurnUserMsgOf() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnUserMsg
}

// SetTitle / TitleOf 标题读写（genTitle 异步写）。
func (s *Session) SetTitle(title string) {
	s.mu.Lock()
	s.Title = title
	s.mu.Unlock()
}

func (s *Session) TitleOf() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Title
}

// TaskOf 会话任务摘要读（首条用户消息截断——与落盘 sessionRecord.Task 同源；
// recall 检索与 TurnEpilogue 交接载荷用）。
func (s *Session) TaskOf() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Task
}

// Subscribe 旁观订阅（返回通道与订阅时最新事件 ID——追赶回放去重基准）。
// 慢消费者不阻塞运行：通道满即丢弃（旁观是尽力而为视图，回看端点兜底完整流）。
func (s *Session) Subscribe() (chan Event, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan Event, 64)
	if s.subs == nil {
		s.subs = map[chan Event]bool{}
	}
	s.subs[ch] = true
	return ch, s.seq
}

// Unsubscribe 摘除订阅（断连/收尾）。
func (s *Session) Unsubscribe(ch chan Event) {
	s.mu.Lock()
	delete(s.subs, ch)
	s.mu.Unlock()
}

// SnapshotEvents 事件快照（追赶回放源）。
func (s *Session) SnapshotEvents() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.Events...)
}

// Record 追加事件（seq 自增；已停止会话不再记录——删除后磁盘零残留）。
// 记录即扇出到旁观订阅（M5-2）。
func (s *Session) Record(name string, data any) Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return Event{}
	}
	s.seq++
	now := time.Now()
	s.UpdatedAt = now
	ev := Event{ID: s.seq, Event: name, Data: data, Ts: now.UnixMilli()}
	s.Events = append(s.Events, ev)
	for ch := range s.subs {
		select {
		case ch <- ev:
		default: // 满即弃（旁观尽力而为）
		}
	}
	if name == "text_delta" {
		if d, ok := data.(contract.Delta); ok && d.Delta != "" {
			s.summary = truncateRunes(appendSummary(s.summary, d.Delta), 60)
		}
	}
	return ev
}

// appendSummary 摘要只取最后一段文本（截断到 60 runes 由调用方）。
func appendSummary(cur, delta string) string {
	if cur == "" {
		return delta
	}
	merged := cur + delta
	if n := len([]rune(merged)); n > 120 { // 超长即重开（近似「最后一段」）
		return string([]rune(merged)[n-60:])
	}
	return merged
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// AppendHistory 追加消息（user 输入与每轮 assistant 终态）。
func (s *Session) AppendHistory(msgs ...*schema.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append(s.history, msgs...)
}

// CloneHistory 取历史快照（Run 组装输入用）。
func (s *Session) CloneHistory() []*schema.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*schema.Message(nil), s.history...)
}

// BeginRun 执行抢占（原子：态检查 + 翻 running + 模式置换一步完成）——
// 任务后台化后同一会话防并发双执行体的闸门（api 层 POST /api/chat 启动前
// 调用；false = 活跃中，调用方走 steering 入队）。顺带挂 runDone（本轮执行体
// 结束信号——FlushQueue 打断当前执行后等其收尾再接管，防双执行体竞态）。
func (s *Session) BeginRun(mode string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.State == StateRunning || s.State == StatePendingApproval {
		return false
	}
	s.State = StateRunning
	s.setModeLocked(mode)
	s.runDone = make(chan struct{})
	s.UpdatedAt = time.Now()
	return true
}

// setModeLocked 换档收口（BeginRun / Steer / SetRunMode 三入口同规则）：
// 写 s.Mode 并同步快照字段 s.Model.Mode（detail/设置回执回显读 s.Model——
// 不同步则创建后换档回显旧值）；换档作废任务期授权（收紧或放松都以新档
// 为准）。调用方持锁。
func (s *Session) setModeLocked(mode string) {
	if mode == "" || mode == s.Mode {
		return
	}
	s.Mode = mode
	s.Model.Mode = mode
	s.taskGrant = false
}

// BeginResume 决议续流执行体抢占（单锁原子：查挂起 + 清挂起域 + 翻 running +
// 挂 runDone——BeginRun 同型三步）。判据用 curAppID 而非 State：挂起后 State
// 仍为 pending_approval，态检查挡不住并发双 Resume；curAppID 的查清在同一
// 锁段完成即闭合 check→Resume 的 TOCTOU 窗口。翻 running 同时修复续流执行
// 期的可见性（此前恒显 pending：FlushQueue 误报不可打断、Drain 枚举漏执行
// 体）。返回 false = 无挂起（重复/迟到 Resume 在此即拒——checkpoint 不随
// Resume 消费，迟到调用放行是脏重放）。
func (s *Session) BeginResume() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.curAppID == "" {
		return false
	}
	s.curAppID = ""
	s.pendingKind = ""
	s.pendingDue = time.Time{}
	s.pendingItems = nil
	s.State = StateRunning
	s.runDone = make(chan struct{})
	s.UpdatedAt = time.Now()
	return true
}

// SetRunModel 会话内切换模型/effort（BeginRun 抢占成功后由 API 层调用——
// 运行边界生效：当前执行体不被打断，新值从本轮起用；空值保持现值。
// 换模型不作废任务期授权——授权跟任务不跟模型）。模型实际变更时落
// model_change 注记事件（UI-B5：前端居中分隔条；effort 变更不发）。
func (s *Session) SetRunModel(model, effort string) {
	s.mu.Lock()
	var from string
	if model != "" && model != s.Model.Model {
		from = s.Model.Model
		s.Model.Model = model
	}
	if effort != "" && effort != s.Model.Effort {
		s.Model.Effort = effort
	}
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
	if from != "" { // Record 自取锁（不可重入，锁外发）
		s.Record(contract.EvModelChange, contract.ModelChange{From: from, To: model})
	}
}

// RunDone 执行体结束信号快照（nil = 无执行体）。
func (s *Session) RunDone() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runDone
}

// RunFinished 执行体收尾（Run/Resume defer 调用；摘下并关闭 runDone——
// 等待者以 close 为同步点，此后 BeginRun 才可能创建新通道，无别名竞态）。
func (s *Session) RunFinished() {
	s.mu.Lock()
	ch := s.runDone
	s.runDone = nil
	s.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// MarkFlush / TakeFlushMark 立即处理排队标记（FlushQueue 打断前置位；
// pump 中断收尾消费分叉事件形态）。
func (s *Session) MarkFlush() { s.mu.Lock(); s.flushing = true; s.mu.Unlock() }
func (s *Session) TakeFlushMark() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.flushing
	s.flushing = false
	return f
}

// QueueLen 排队消息数（flush 端点空队列守卫）。
func (s *Session) QueueLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingMsgs)
}

// HasQueuedImages 排队消息是否含图片附件（设置通道切模型的守卫：落回轮
// 无图片门禁兜底，纯文本模型 + 含图排队消息会在执行时炸——写入时拒）。
func (s *Session) HasQueuedImages() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, q := range s.pendingMsgs {
		for _, a := range q.Attachments {
			if a.IsImage {
				return true
			}
		}
	}
	return false
}

// CancelRun 取消运行中的执行体（停止按钮端点；无运行则 no-op）。执行体收
// 到取消后由 pump 中断收尾（中断事件 + error 终态落盘）。
func (s *Session) CancelRun() {
	s.mu.Lock()
	c := s.cancel
	s.mu.Unlock()
	if c != nil {
		c()
	}
}

// Stop 删除会话的停止语义：置位 + close 通道 + 取消运行上下文（幂等）。
func (s *Session) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	if s.cancel != nil {
		s.cancel()
	}
	if s.stoppedCh != nil {
		close(s.stoppedCh)
	}
}

// Stopped 删除置位检查。
func (s *Session) Stopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// MarkTitleFlight 标记标题异步生成在途（返回收尾闭包；引擎 settleTurn 起
// genTitle goroutine 前调用）。genTitle 是唯一逃逸 Run 生命周期的写者——
// Run 返回 ≠ 写完，此信号让测试收尾/删除方可确定性等待在途写落地。
func (s *Session) MarkTitleFlight() func() {
	ch := make(chan struct{})
	s.mu.Lock()
	s.titleCh = ch
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// TitleFlight 标题生成在途信号：nil = 无在途；关闭 = 写收尾完成。
func (s *Session) TitleFlight() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.titleCh
}

// Attachment 用户消息附件（契约形态）。
type Attachment = contract.Attachment

// QueuedMsg 排队消息（契约形态；steering 入队，可编辑/删除）。
type QueuedMsg = contract.QueuedMsg

// newQueueID 排队消息 id（q 前缀 + 4 hex）。
func newQueueID() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return "q" + hex.EncodeToString(b)
}

// Steer running/pending 中再输入：入队 + 模式即时切换（docs/05）。
// 返回 false = 会话空闲（走正常 Run 而非 steering）。入队落 steer_queued
// 事件——排队态可回放重建（切回/刷新不丢，对齐审批决议回执定案）。
func (s *Session) Steer(msg string, atts []Attachment, mode string) bool {
	s.mu.Lock()
	if s.State != StateRunning && s.State != StatePendingApproval {
		s.mu.Unlock()
		return false
	}
	s.setModeLocked(mode)
	var q QueuedMsg
	if msg != "" || len(atts) > 0 {
		q = QueuedMsg{ID: newQueueID(), Text: msg, Attachments: atts}
		s.pendingMsgs = append(s.pendingMsgs, q)
	}
	s.mu.Unlock()
	if q.ID != "" {
		s.Record("steer_queued", contract.SteerEvent{ID: q.ID, Text: q.Text, Attachments: q.Attachments})
	}
	return true
}

// ContinueOrNotify 后台完成通知的单锁原子注入原语（W-3）：
// 锁内一步裁定——running/pending/error → 通知入队（began=false，当前/
// 下一轮 TakePending 消费）；ended（自然收口）+ wake → 翻 running + 挂
// runDone + 通知入队（began=true，调用方锁外 Record notify_queued 后以
// Run("") 起自续轮——通知经队列注入，输入路径与 steering 唯一）；ended +
// !wake → 只入队（自激护栏预算耗尽：不自动开轮，下轮用户交互消费）。
// **error 终态绝不自续**（对照审查 A-3：用户停止把中断洗成模型请求是被
// 明确拒绝的形态；运行错误同理保守——通知只入队，下轮用户交互消费）。
// 竞态封闭依据：与 BeginRun/Steer 同锁互斥，TOCTOU 丢通知/丢启动窗口不
// 存在（单锁原语义，对照审查定案）。返回的 q 供锁外 Record（Record 自持
// 锁，锁内不可调）。
func (s *Session) ContinueOrNotify(msg string, wake bool) (began bool, q QueuedMsg) {
	s.mu.Lock()
	q = QueuedMsg{ID: newQueueID(), Text: msg, Kind: "notify"}
	if s.State != StateEnded {
		s.pendingMsgs = append(s.pendingMsgs, q)
		s.mu.Unlock()
		return false, q
	}
	if !wake {
		s.pendingMsgs = append(s.pendingMsgs, q)
		s.mu.Unlock()
		return false, q
	}
	// ended：抢占执行体（与 BeginRun 同型三步——查态+翻 running+挂 runDone）
	s.State = StateRunning
	s.runDone = make(chan struct{})
	s.UpdatedAt = time.Now()
	s.pendingMsgs = append(s.pendingMsgs, q)
	s.mu.Unlock()
	return true, q
}

// NotifyBudget 自激护栏预算（W-3）：budget = 连续自续上限。true = 还有预算
// （消费一格）；false = 已耗尽（调用方降级仅记事件）。用户消息消费恢复预算
// （RestoreNotifyBudget——通知自身不恢复，dsh spentWakes 同款）。
func (s *Session) NotifyBudget(budget int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notifySpent >= budget {
		return false
	}
	s.notifySpent++
	return true
}

// RestoreNotifyBudget 用户输入到达本轮输入（Run/Resume 头部消费到非 notify
// 条目）时恢复全部自续预算。
func (s *Session) RestoreNotifyBudget() {
	s.mu.Lock()
	s.notifySpent = 0
	s.mu.Unlock()
}

// EditQueued 编辑排队消息（false = 不存在或系统通知只读）；落 steer_updated 回执。
func (s *Session) EditQueued(id, text string) bool {
	s.mu.Lock()
	ok := false
	for i := range s.pendingMsgs {
		if s.pendingMsgs[i].ID == id {
			if s.pendingMsgs[i].Kind == "notify" {
				s.mu.Unlock()
				return false // 系统通知只读（后台子代理结论不可编辑）
			}
			s.pendingMsgs[i].Text = text
			ok = true
			break
		}
	}
	s.mu.Unlock()
	if ok {
		s.Record("steer_updated", contract.SteerEvent{ID: id, Text: text})
	}
	return ok
}

// RemoveQueued 移除排队消息（false = 不存在或系统通知不可删）；落 steer_removed 回执。
func (s *Session) RemoveQueued(id string) bool {
	s.mu.Lock()
	ok := false
	for i := range s.pendingMsgs {
		if s.pendingMsgs[i].ID == id {
			if s.pendingMsgs[i].Kind == "notify" {
				s.mu.Unlock()
				return false // 系统通知不可删（结论注入是模型面承诺）
			}
			s.pendingMsgs = append(s.pendingMsgs[:i], s.pendingMsgs[i+1:]...)
			ok = true
			break
		}
	}
	s.mu.Unlock()
	if ok {
		s.Record("steer_removed", contract.SteerEvent{ID: id})
	}
	return ok
}

// ReorderQueued 排队消息重排（UI-B3 拖拽排序——ids = 期望顺序的完整清单；
// 集合与现存不一致整体拒（多/少/未知 id，防并发丢消息）；成功落
// steer_reordered 回执——回放重建顺序的真源）。
func (s *Session) ReorderQueued(ids []string) bool {
	s.mu.Lock()
	if len(ids) != len(s.pendingMsgs) {
		s.mu.Unlock()
		return false
	}
	pos := make(map[string]int, len(s.pendingMsgs))
	for i, q := range s.pendingMsgs {
		pos[q.ID] = i
	}
	ordered := make([]QueuedMsg, 0, len(ids))
	for _, id := range ids {
		p, ok := pos[id]
		if !ok {
			s.mu.Unlock()
			return false
		}
		ordered = append(ordered, s.pendingMsgs[p])
	}
	s.pendingMsgs = ordered
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
	s.Record(contract.EvSteerReordered, contract.SteerReorder{IDs: ids})
	return true
}

// TakePending 取走排队消息（Run 前置带回——排队兜底路径）。
func (s *Session) TakePending() []QueuedMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.pendingMsgs
	s.pendingMsgs = nil
	return msgs
}

// StateOf 当前状态读取（与 State 字段区分）。
func (s *Session) StateOf() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.State
}

// ModePublic 模式读取（包装器组装用）。
func (s *Session) ModePublic() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Mode
}

// SetMode 模式设置（设置通道与随轮次写入共用，docs/05；换档收口见
// setModeLocked——同步快照字段 s.Model.Mode，detail/设置回执回显用）。
func (s *Session) SetMode(mode string) {
	s.mu.Lock()
	s.setModeLocked(mode)
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetState 状态迁移 + 更新时间。
func (s *Session) SetState(state string) {
	s.mu.Lock()
	s.State = state
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetCancel 挂运行取消口（Stop 时取消）。
func (s *Session) SetCancel(c context.CancelFunc) {
	s.mu.Lock()
	s.cancel = c
	s.mu.Unlock()
}

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

// newSID 会话 id（s 前缀 + 8 hex，区别 issue id 形态）。
func newSID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "s" + hex.EncodeToString(b)
}

// Create 新会话（默认态 running 由 Run 侧设置；此处先 ended 占位防并发窗）。
// 顺带触发一轮过期清理（2026-08-23 定：新建即触检，替代小时级后台任务——
// 会话只在有人用时累积，新建时机天然自限；全量扫盘轻量）。
func (r *Registry) Create(owner, task, mode string, model contract.UserPrefs) *Session {
	s := &Session{
		SID: newSID(), Owner: owner, Scope: "需求池", Task: truncateRunes(task, 16),
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
		// 归属不符：回滚注册表（防御——api 层应已校验）
		r.mu.Lock()
		r.sessions[sid] = s
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
}

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

// sessionRecord 落盘 DTO（显式字段——Session 含锁与运行态字段不整体序列化）。
type sessionRecord struct {
	SID          string                `json:"sid"`
	Owner        string                `json:"owner"`
	Scope        string                `json:"scope"`
	Task         string                `json:"task"`
	Title        string                `json:"title"`
	State        string                `json:"state"`
	Mode         string                `json:"mode"`
	Model        contract.UserPrefs    `json:"model"`
	StartedAt    time.Time             `json:"started_at"`
	UpdatedAt    time.Time             `json:"updated_at"`
	Events       []Event               `json:"events"`
	Summary      string                `json:"summary"`
	FileChanges  map[string]fileChange `json:"file_changes"`
	Messages     []*schema.Message     `json:"messages"`                 // 续聊历史（进程重启后恢复）
	Pending      []QueuedMsg           `json:"pending,omitempty"`        // 排队消息（重启续接——用户补充指令是交互内容，不随进程丢）
	PendingAppID string                `json:"pending_app_id,omitempty"` // 挂起审批/提问/计划 ID（重启续接——checkpoint 在盘，决议可续流）
	PendingKind  string                `json:"pending_kind,omitempty"`   // 挂起类型（approval|ask|plan——续表/超时翻转动作分叉）
	PendingDue   time.Time             `json:"pending_due,omitempty"`    // 挂起截止（超时兜底跨重启——内存计时器随进程丢）
	PendingItems []string              `json:"pending_items,omitempty"`  // 合并决议卡项标识清单（重启续接——超时批量拒/决议覆盖校验依据）
	TaskGranted  bool                  `json:"task_granted,omitempty"`   // plan 档任务期写授权（重启续接——批准的计划不因进程重启失效）
	PlanSeq      int                   `json:"plan_seq,omitempty"`       // 计划文档序号末号（新提交接续递增）
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
		SID: s.SID, Owner: s.Owner, Scope: s.Scope, Task: s.Task, Title: s.Title,
		State: s.State, Mode: s.Mode, Model: s.Model, StartedAt: s.StartedAt, UpdatedAt: s.UpdatedAt,
		Events:       append([]Event(nil), s.Events...),
		Summary:      s.summary,
		FileChanges:  fileChangesCopy(s),
		Messages:     historyForRecord(s.history),
		Pending:      append([]QueuedMsg(nil), s.pendingMsgs...),
		PendingAppID: s.curAppID, PendingKind: s.pendingKind, PendingDue: s.pendingDue,
		PendingItems: append([]string(nil), s.pendingItems...),
		TaskGranted:  s.taskGrant, PlanSeq: s.planSeq,
	}
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
	_ = r.st.WriteUserTreeFile(s.Owner, path.Join("sessions", s.SID, "session.json"), data)
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

// Fork 全量快照分叉（B8）：从既有会话复制当前完整态（事件流+历史+摘要+外置
// 域），生成新 SID 的新会话。V1 限定源非 running（源 running 即返回 nil——
// spill 目录复制与源 reduction 外置写并发无锁覆盖，撕裂拷贝风险；主场景
// FinalGate 耗尽重跑/历史会话岔出均发生在源空闲态；源 running 快照分叉为
// 升级位，须先给 spill 写入与复制立同步点）。分叉体 State=ended 占位
// （Create 同款），Run 由应用发起。寻址同 Reattach 语义：内存优先，不在内存
// 则磁盘重建后再克隆（历史会话分叉是主场景——Pi 即从历史会话岔出）；归属
// 不符/未知 sid/源 running 返回 nil（Reattach 同款纪律）。挂起域/taskGrant/
// planSeq/排队消息不带（分叉体无执行体残留，与 Reattach 的不可续降级同裁
// 决；任务期写授权回审批流是安全侧）；spill 外置域整目录复制（history 含
// 外置指针，不复制即 read_file 取回落空）。
func (r *Registry) Fork(owner, sid string) *Session {
	if s, ok := r.Get(sid); ok {
		if s.Owner != owner || s.StateOf() == StateRunning {
			return nil
		}
		s.writeMu.Lock()
		s.mu.Lock()
		rec := recordOf(s)
		s.mu.Unlock()
		s.writeMu.Unlock()
		return r.forkOf(&rec)
	}
	data, ok := r.st.ReadUserTreeFile(owner, path.Join("sessions", sid, "session.json"))
	if !ok {
		return nil
	}
	var rec sessionRecord
	if json.Unmarshal(data, &rec) != nil || rec.Owner != owner || rec.State == StateRunning {
		return nil
	}
	return r.forkOf(&rec)
}

// forkOf 分叉体构造：record JSON 往返深拷贝（Event.Data/消息对象全量拷贝、
// 零共享指针——与 Reattach 的盘面重建同语义，Event.Data 降为 map 形态亦同）
// → 新 SID 装配（Reattach 构造路径同款，挂起域不装载）→ 血缘 note（分叉体
// 自身首个事件——回放可见血缘）→ spill 复制 → 注册 → 落盘（Reattach 可恢
// 复的完整态）。源会话零增量。
func (r *Registry) forkOf(rec *sessionRecord) *Session {
	data, err := json.Marshal(rec)
	if err != nil {
		return nil
	}
	var cp sessionRecord
	if json.Unmarshal(data, &cp) != nil {
		return nil
	}
	ns := &Session{
		SID: newSID(), Owner: cp.Owner, Scope: cp.Scope, Task: cp.Task, Title: cp.Title,
		State: StateEnded, Mode: cp.Mode, Model: cp.Model,
		StartedAt: time.Now(), UpdatedAt: time.Now(),
		Events:  append([]Event(nil), cp.Events...),
		summary: cp.Summary, fileChanges: cp.FileChanges, stoppedCh: make(chan struct{}),
	}
	if n := len(ns.Events); n > 0 {
		ns.seq = ns.Events[n-1].ID // 接续末位事件 ID——不接续则新事件撞号
	}
	ns.history = cp.Messages
	ns.Record(contract.EvHarnessNote, contract.HarnessNote{
		Kind: "fork", Title: "会话分叉",
		Detail: "全量快照分叉自会话 " + cp.SID + "（分叉点即源会话当时完整态；工作区不随行，文件成果携带归应用决策）",
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

// SessionListItem 列表条目（活动时间倒序；字段契约 = M3-2 桩 + 2026-08-23 增 updated_at）。
type SessionListItem struct {
	SID       string `json:"sid"`
	Scope     string `json:"scope"`
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
			SID: s.SID, Scope: s.Scope, Owner: s.Owner, State: s.State,
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
			SID: rec.SID, Scope: rec.Scope, Owner: rec.Owner, State: rec.State,
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
	Scope     string             `json:"scope"`
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
			SID: s.SID, Owner: s.Owner, Scope: s.Scope, State: s.State,
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

	data, ok := r.st.ReadUserTreeFile(owner, path.Join("sessions", sid, "session.json"))
	if !ok {
		return nil
	}
	var rec sessionRecord
	if json.Unmarshal(data, &rec) != nil || rec.Owner != owner {
		return nil
	}
	rec.Model.Effort = llm.NormalizeEffort(rec.Model.Effort) // 存量快照旧值归一：进内存即四档值，detail/回发/引擎全链一致
	st := rec.State
	if st == StateRunning || (st == StatePendingApproval && rec.PendingAppID == "") {
		st = StateEnded // 无执行体残留不可续
	}
	s := &Session{
		SID: rec.SID, Owner: rec.Owner, Scope: rec.Scope, Task: rec.Task, Title: rec.Title,
		State: st, Mode: rec.Mode, Model: rec.Model,
		StartedAt: rec.StartedAt, UpdatedAt: rec.UpdatedAt,
		Events:  append([]Event(nil), rec.Events...),
		summary: rec.Summary, fileChanges: rec.FileChanges, stoppedCh: make(chan struct{}),
	}
	if st == StatePendingApproval {
		s.curAppID, s.pendingKind, s.pendingDue = rec.PendingAppID, rec.PendingKind, rec.PendingDue
		s.pendingItems = append([]string(nil), rec.PendingItems...) // 合并决议项清单续接（超时批量拒依据）
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
	data, ok := r.st.ReadUserTreeFile(s.Owner, path.Join("sessions", s.SID, "session.json"))
	if !ok {
		return
	}
	var rec sessionRecord
	if json.Unmarshal(data, &rec) != nil {
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
	s.Model.Effort = llm.NormalizeEffort(s.Model.Effort) // 盘面直读回显同规则
	ts := activityTs(s.UpdatedAt, s.StartedAt)
	return SessionDetail{
		SID: s.SID, Owner: s.Owner, Scope: s.Scope, State: s.State,
		Mode: s.Mode, Model: s.Model,
		Title:     titleOrTaskStr(s.Title, s.Task),
		StartedAt: s.StartedAt.UTC().Format(time.RFC3339),
		UpdatedAt: ts.UTC().Format(time.RFC3339),
		Events:    eventsSince(s.Events, since),
	}, true
}

// fileChange 单文件累计（同文件多次写合并计数，动作后写覆盖）。
type fileChange struct {
	Action string `json:"action"`
	Count  int    `json:"count"`
}

// RecordFileChange 工具层报备入口（ctx 记录器直连；并发安全）。
func (s *Session) RecordFileChange(path, action string) {
	if path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fileChanges == nil {
		s.fileChanges = map[string]fileChange{}
	}
	e := s.fileChanges[path]
	e.Action = action
	e.Count++
	s.fileChanges[path] = e
}

// FileChangesSnapshot 会话累计变更清单（path 排序稳定输出）。
func (s *Session) FileChangesSnapshot() []contract.FileChange {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]contract.FileChange, 0, len(s.fileChanges))
	for p, e := range s.fileChanges {
		out = append(out, contract.FileChange{Path: p, Action: e.Action, Count: e.Count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// diskItem 列表轻量解析 DTO（不驻留 events/messages）。
type diskItem struct {
	SID       string    `json:"sid"`
	Owner     string    `json:"owner"`
	Scope     string    `json:"scope"`
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
			SID: s.SID, Scope: s.Scope, Owner: s.Owner, State: s.State,
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
				SID: rec.SID, Scope: rec.Scope, Owner: rec.Owner, State: rec.State,
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
			rel := path.Join("sessions", sid, "session.json")
			data, ok := r.st.ReadUserTreeFile(op, rel)
			if !ok {
				continue
			}
			var rec sessionRecord
			if json.Unmarshal(data, &rec) != nil || rec.SID == "" {
				continue
			}
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
				Data: contract.ErrorOut{Code: "SERVER", Message: "进程重启，会话中断"},
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

// SummaryOf 取列表摘要（公开读取——引擎 session_end 载荷用）。
func (s *Session) SummaryOf() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summary
}
