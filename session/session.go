// Package session 是会话域（自产品 internal/agent/session.go 迁入）：内存
// 注册表 + users/<op>/sessions/<sid>/session.json 落盘（用户隔离）。会话记录 =
// 发起人/状态/模式/模型快照/任务摘要 + 事件流（回看端点原样输出，应用层走
// 自有传输管线渲染）。存储经 Store 接口注入——数据面（唯一文件写入器）在应用。
//
// 状态：running | pending_approval | ended | error。
package session

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/strutil"
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
// 契约：operator 是用户标识符——实现方构造用户域路径时必须圈禁（结构规则
// 见 ValidOwner：无路径分隔符、非 ./.. ——否则 operator 可携带 ../ 穿越
// users/<op>/ 树读写区外，安全审查 2026-09-06）。
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

// ValidOwner operator（用户标识符）的结构围栏：非空、非 "." / ".."、不含
// 路径分隔符与 NUL——可含任意 Unicode（owner 是「张三」这类标识不是文件名
// 白名单）。operator 进 Store 前的入口面（ui 查询参数、渠道 InboundMsg）
// 应先过此栏：拦截的是「标识符携带路径形态穿越 users 树」的注入面。
func ValidOwner(op string) bool {
	return op != "" && op != "." && op != ".." &&
		!strings.ContainsAny(op, `/\`) && !strings.ContainsRune(op, 0)
}

// Session 会话（Events 即消息流真源；mu 保护并发 emit/删除竞态）。
type Session struct {
	mu            sync.Mutex
	SID           string             `json:"sid"`
	Owner         string             `json:"owner"`
	Task          string             `json:"task"` // 会话列表任务摘要（首条用户消息截断）
	State         string             `json:"state"`
	Mode          string             `json:"mode"`
	Title         string             `json:"title"` // LLM 总结标题（首轮收尾异步生成；空 = 回退 Task 截断）
	Model         contract.UserPrefs `json:"model"` // {model 复合键, effort} 快照（会话粘住）
	lastUsedModel string             // 上次实际参与模型调用的模型复合键（NoteModelCall 维护，不导出）
	parentSID     string             // 辅助对话父会话（空 = 普通会话；构造后不变——工作区/spill 共享父域的寻址键）
	StartedAt     time.Time          `json:"started_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
	Events        []Event            `json:"events"`

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
	decisions     map[string]*ApprovalDecision
	curAppID      string                 // 挂起中的审批/提问 ID（空 = 无）
	pendingKind   string                 // 挂起类型（"approval"|"ask"——跨重启续表用）
	pendingDue    time.Time              // 挂起截止时刻（超时兜底跨重启——进程重启丢内存计时器）
	pendingItems  []string               // 合并决议卡项标识清单（kind=approval；超时批量拒与端点覆盖校验依据）
	pendingTarget string                 // T6 挂起审批路由目标（ApprovalRouter 裁决；guard 校验依据——随挂起清空）
	askDecision   *AskDecision           // ask_user 作答（answer 端点写入；Resume 消费后清空）
	turnGrant     bool                   // plan 档本轮写授权（首个批准置位；session_end 清零）
	taskGrant     bool                   // plan 档任务期写授权（计划批准置位；任务成功收尾/换档/新计划提交清零）
	planSeq       int                    // 计划文档序号（submit_plan 自增取号；修订递增留痕）
	turnUserMsg   string                 // 轮次用户消息（跨审批中断保留）
	notifySpent   int                    // 后台通知连续自续已花费预算（W-3 自激护栏；用户消息消费清零，通知自身不恢复）
	participants  []contract.Participant // T6 参与者名册（多人群聊/坐席协同；落盘随 session.json，变更经 participant_update 事件落流）
	turnActor     *contract.Participant  // T6 当轮说话人（渠道/应用 Run 前设置；跨审批中断保留保 Requester/Operator 连续；不落盘——轮次事实）
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
	m := map[string]any{"ask_id": askID, "answers": d.Answers, "free_text": d.FreeText}
	if d.DeciderID != "" { // T6：谁作答的（空 = 零变化）
		m["decider_id"], m["decider_name"] = d.DeciderID, d.DeciderName
	}
	s.Record("ask_decision", m)
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

// UpsertParticipant 名册登记（T6）：首见落 joined、在册变更落 updated
// （participant_update 事件——回放重建 roster 的真源，不依赖 session.json
// 快照）。Name/Role 空值不覆盖在册值（部分更新）。返回变更 Kind（无变更
// 空串）；Kind 封闭集含 left（移除面待第一个消费者，本批不开）。
func (s *Session) UpsertParticipant(p contract.Participant) string {
	if p.ID == "" {
		return ""
	}
	s.mu.Lock()
	for i := range s.participants {
		if s.participants[i].ID != p.ID {
			continue
		}
		kind := ""
		if p.Name != "" && p.Name != s.participants[i].Name {
			s.participants[i].Name = p.Name
			kind = "updated"
		}
		if p.Role != "" && p.Role != s.participants[i].Role {
			s.participants[i].Role = p.Role
			kind = "updated"
		}
		cur := s.participants[i]
		s.mu.Unlock()
		if kind != "" {
			s.Record(contract.EvParticipantUpdate, contract.ParticipantEvent{Participant: cur, Kind: kind})
		}
		return kind
	}
	s.participants = append(s.participants, p)
	s.mu.Unlock()
	s.Record(contract.EvParticipantUpdate, contract.ParticipantEvent{Participant: p, Kind: "joined"})
	return "joined"
}

// ParticipantsOf 名册快照。
func (s *Session) ParticipantsOf() []contract.Participant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]contract.Participant(nil), s.participants...)
}

// SetTurnActor 当轮说话人（T6：渠道/应用在 Run 前设置）。
func (s *Session) SetTurnActor(p *contract.Participant) {
	s.mu.Lock()
	s.turnActor = p
	s.mu.Unlock()
}

// TurnActorOf 当轮说话人（nil = 单用户零变化——引擎回退 Owner 语义）。
func (s *Session) TurnActorOf() *contract.Participant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnActor
}

// RecordDecision 决议回执落流（approve 端点在 SetDecision 后调用）。事件流是
// 切回/回放重建审批卡终态的真源——决议只 Set 不落流，卡片永远停在待审批态。
// items = 合并决议卡的逐项回执（分歧态以 Items 为准——契约声明的真源面，
// 审查 P2-16 落码：此前从不填充，批一拒一的回放无法重建逐项终态）；单卡
// 决议空参零变化。
func (s *Session) RecordDecision(appID string, d ApprovalDecision, items ...contract.ItemDecisionOut) {
	s.Record("approval_decision", contract.DecisionOut{ApprovalID: appID, Approve: d.Approve, Reason: d.Reason,
		DeciderID: d.DeciderID, DeciderName: d.DeciderName, Items: items}) // T6：谁决议的——回放可审计
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
		s.pendingTarget = "" // T6 挂起清空同步清目标（防陈旧目标误拒下一卡）
	}
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// SetPendingTarget 挂起审批的路由目标（T6：Options.ApprovalRouter 裁决后
// 泵面写入；空 = 不路由）。
func (s *Session) SetPendingTarget(id string) {
	s.mu.Lock()
	s.pendingTarget = id
	s.mu.Unlock()
}

// PendingTarget 挂起审批的路由目标（空 = 无目标不校验）。
func (s *Session) PendingTarget() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingTarget
}

// PendingAppID 当前挂起审批 ID。
func (s *Session) PendingAppID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.curAppID
}

// ClearPendingIf 条件原子清挂起（超时路径专用）：锁内校验「仍挂起该 appID 且
// 无决议到达」才清并返回项清单——闭合守卫检查与清挂起之间的 TOCTOU（用户
// approve 与超时并发时抢到锁者赢，另一方让位）。项清单随清返回（清域后不可
// 再取——合并卡逐项落拒用）。askDecision 归提问路径（kind=ask 超时同样经此）。
func (s *Session) ClearPendingIf(appID string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.curAppID == "" || s.curAppID != appID || s.askDecision != nil || len(s.decisions) > 0 {
		return nil, false
	}
	items := append([]string(nil), s.pendingItems...)
	s.curAppID = ""
	s.pendingKind = ""
	s.pendingDue = time.Time{}
	s.pendingItems = nil
	s.UpdatedAt = time.Now()
	return items, true
}

// ModelSnapshot 模型复合键持锁快照（PUT settings 类随时写路径与引擎读并发，
// 裸读 s.Model.Model 构成 data race）。
func (s *Session) ModelSnapshot() contract.UserPrefs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Model
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
	s.Record(contract.EvPlanDecision, contract.PlanDecisionOut{PlanID: planID, Approve: d.Approve, Reason: d.Reason,
		DeciderID: d.DeciderID, DeciderName: d.DeciderName}) // T6：谁决议的
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

// EventsSince 快照中 ID 大于 from 的事件（订阅通道满即弃的补投源——渠道
// 消费泵的间隙补投/节拍追赶与 ui SSE 慢消费兜底同源共用，见 engine/channel.go）。
func (s *Session) EventsSince(from int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, ev := range s.Events {
		if ev.ID > from {
			out = append(out, ev)
		}
	}
	return out
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
			s.summary = strutil.Truncate(appendSummary(s.summary, d.Delta), 60)
		}
	}
	return ev
}

// EventAs 事件载荷的双形态统一取值器（Record 落 typed 原样、Reattach 盘面
// 回读是 map——data any 的固有双形态）。typed 直断言，map 经 JSON 往返还原，
// 失败 false。消费面统一走此取值器：此前各消费者自做归一（ui 手写往返、
// sessionEndAt 手写双分支、feishu 仅断言 typed——盘面回放消费即静默空白，
// 审查 P2-8 收口）。
func EventAs[T any](ev Event) (T, bool) {
	var zero T
	if ev.Data == nil {
		return zero, false
	}
	if v, ok := ev.Data.(T); ok {
		return v, true
	}
	b, err := json.Marshal(ev.Data)
	if err != nil {
		return zero, false
	}
	var out T
	if json.Unmarshal(b, &out) != nil {
		return zero, false
	}
	return out, true
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

// HistoryLen 历史长度（settleTurn 记 SessionEnd.HistLen 的取值面）。
func (s *Session) HistoryLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.history)
}

// ParentOf 父会话 SID（辅助对话；空 = 普通会话）。构造后不变，无锁直读。
func (s *Session) ParentOf() string { return s.parentSID }

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
// 换模型不作废任务期授权——授权跟任务不跟模型）。切换只记录不落事件——
// 切换标志注记在调用边界（NoteModelCall：本次调用与上次实际调用比对，
// 2026-09-03 用户定调——选择器切换与显示标志是两回事）。
func (s *Session) SetRunModel(model, effort string) {
	s.mu.Lock()
	if model != "" && model != s.Model.Model {
		s.Model.Model = model
	}
	if effort != "" && effort != s.Model.Effort {
		s.Model.Effort = effort
	}
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
}

// NoteModelCall 模型调用边界登记（engine.assemble 每次模型调用前调）：本次
// 将用模型与上次实际调用不同 → 落 model_change 注记（UI 切换标志）。首调/
// 跨重启无前序只登记不落。lastUsedModel 随记录持久化（跨重启比对不断）。
func (s *Session) NoteModelCall(model string) {
	s.mu.Lock()
	last := s.lastUsedModel
	s.lastUsedModel = model
	s.mu.Unlock()
	if last != "" && last != model {
		s.Record(contract.EvModelChange, contract.ModelChange{From: last, To: model})
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

// SummaryOf 取列表摘要（公开读取——引擎 session_end 载荷用）。
func (s *Session) SummaryOf() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summary
}
