package contract

import "time"

// 事件族（名称常量）：事件流是会话记录与前端渲染的真源——text_delta /
// thinking_delta / tool_call / tool_result / approval_request /
// approval_decision / approval_timeout / ask_user_request / ask_decision /
// ask_timeout / ask_ignored / plan_request / plan_decision / plan_timeout /
// todo_update / steer_queued / steer_updated / steer_removed / steer_injected /
// steer_reordered / notify_queued / notify_injected / user_message / usage /
// session_end / error / interrupted / harness_note / subagent / model_change /
// transport_retry。
// 传输无关：应用层订阅 Session 事件后映射到自己的管线（SSE 编码归应用）。
const (
	EvTextDelta        = "text_delta"
	EvThinkingDelta    = "thinking_delta"
	EvToolCall         = "tool_call"
	EvToolResult       = "tool_result"
	EvApprovalRequest  = "approval_request"
	EvApprovalDecision = "approval_decision"
	EvApprovalTimeout  = "approval_timeout"
	EvAskRequest       = "ask_user_request"
	EvAskDecision      = "ask_decision"
	EvAskTimeout       = "ask_timeout"
	EvAskIgnored       = "ask_ignored"
	EvPlanRequest      = "plan_request"
	EvPlanDecision     = "plan_decision"
	EvPlanTimeout      = "plan_timeout"
	EvTodoUpdate       = "todo_update"
	EvSteerQueued      = "steer_queued"
	EvSteerUpdated     = "steer_updated"
	EvSteerRemoved     = "steer_removed"
	EvSteerInjected    = "steer_injected"
	EvNotifyQueued     = "notify_queued"
	EvNotifyInjected   = "notify_injected"
	EvUserMessage      = "user_message"
	EvUsage            = "usage"
	EvSessionEnd       = "session_end"
	EvError            = "error"
	EvInterrupted      = "interrupted"
	EvHarnessNote      = "harness_note"
	EvSubAgent         = "subagent"
	EvModelChange      = "model_change"
	EvSteerReordered   = "steer_reordered"
	EvTransportRetry   = "transport_retry"
)

// Event 已发事件（回放/订阅的统一载体；Data 为下方载荷类型之一或 map）。
type Event struct {
	ID    int    `json:"id"`
	Event string `json:"event"`
	Data  any    `json:"data"`
	Ts    int64  `json:"ts,omitempty"` // 记录时刻墙钟毫秒（UI-B1：历史回放的时长计算源；旧数据零值=前端降级不显示）
}

// ModelChange 会话内模型切换注记（UI-B5：居中分隔条数据源——界面注记即数据，
// 回放/live 一致）。语义（2026-09-03 定调）：选择器切换只记录不落事件；下一次
// 模型调用时本次与上次实际调用不同才落（effort 变更不发）。ZCode 对位 =
// timeline part（model_change）。
type ModelChange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// SteerReorder 排队消息重排回执（UI-B3：拖拽排序——IDs = 重排后的完整顺序，
// 前端按此重建排队条目顺序；回放/live 一致）。
type SteerReorder struct {
	IDs []string `json:"ids"`
}

// HarnessNote harness 通知卡（H8-3：summarization 压缩 / reduction 外置——
// 统一可折叠系统卡形态，非 error 非普通消息；呈现归应用）。
type HarnessNote struct {
	Kind   string `json:"kind"`             // offload（外置）| compaction（摘要压缩）
	Title  string `json:"title"`            // 单行标题（常显）
	Detail string `json:"detail,omitempty"` // 可展开内容（路径清单/全文指引）
}

// SubAgentEvent 子代理过程事件（H8-2 全量转发档：spawn 子代理的内部事件
// 经泵翻译转发——只读流数据面，子过程不进父主流；折叠卡展开态消费）。
// SpawnID = per-invocation 实例键（W-1）：后台派生事件必带（会话域单调
// "sp1/sp2/…"，注册表分配——勿用回合内自增，跨回合后台任务会撞键）；
// 同步调用事件不带（零值 = 前端回退「归最近」启发式，回放兼容）。
// Kind 扩展 done | failed 两态（后台派生终态）：Text = 结论全文 / 失败
// errFeed 信封；killed/cancelled 位预留（v1.1 kill/wait 引入时扩第三态）。
type SubAgentEvent struct {
	SpawnID string `json:"spawn_id,omitempty"` // 实例键（后台派生；会话域唯一）
	Agent   string `json:"agent"`              // 子代理名（spawn）
	Kind    string `json:"kind"`               // text | tool_call | tool_result | done | failed
	Text    string `json:"text,omitempty"`     // text 正文 / tool_result 摘要 / done = 结论全文（超长 spill 外置）
	Tool    string `json:"tool,omitempty"`     // 子代理调用的工具名
	Args    string `json:"args,omitempty"`     // tool_call 参数摘要
	OK      bool   `json:"ok,omitempty"`       // tool_result 成败
}

// Delta 文本/思考增量。
type Delta struct {
	Delta string `json:"delta"`
}

// ToolCall 工具调用卡片。
type ToolCall struct {
	CallID     string `json:"call_id"`
	Tool       string `json:"tool"`
	ArgsDigest string `json:"args_digest"`
	Behavior   string `json:"behavior,omitempty"` // 行为面标记（ToolInfo.Behavior 快照——过程流分组数据源；空=未标记）
}

// ToolResult 工具结果收口（Digest = 语义摘要；Preview = 原始内容头，展开用）。
type ToolResult struct {
	CallID  string `json:"call_id"`
	OK      bool   `json:"ok"`
	Digest  string `json:"digest"`
	Preview string `json:"preview,omitempty"`
	Counts  string `json:"counts,omitempty"` // B4 文件变更信封 "+A -D"（写类工具结果自带；编辑卡/汇总行数据源）
	Verb    string `json:"verb,omitempty"`   // B4 信封扩展键 create|edit（新建/修改写动词——工具名判别不可分时工具方自带）
}

// PlanItem 审批卡计划清单行。
type PlanItem struct {
	Action  string `json:"action"`
	Summary string `json:"summary"`
	Count   int    `json:"count"`
}

// ApprovalItem 合并决议卡单项（H4-2：一轮并行写调用聚合为一卡 N 项，
// 逐项审阅、一次提交携带全部决议）。
type ApprovalItem struct {
	ItemID   string     `json:"item_id"` // 项标识（工具挂起时生成、随保存态回放）
	Tool     string     `json:"tool"`
	Action   string     `json:"action,omitempty"`
	Plan     []PlanItem `json:"plan"`
	PlanMode bool       `json:"plan_mode,omitempty"`
	Note     string     `json:"note,omitempty"`
	Diff     string     `json:"diff,omitempty"`
}

// ApprovalReq 审批请求（事件载荷，前端审批卡数据源）。Items = 合并决议卡
// 的逐项清单（并行写调用聚合）；顶层旧字段 = 首项镜像——旧事件无 Items 时
// 前端按 N=1 单卡渲染（回放兼容），新事件两处都填。
type ApprovalReq struct {
	ApprovalID string         `json:"approval_id"`
	Tool       string         `json:"tool"`
	Action     string         `json:"action,omitempty"` // 动作名（直白话；缺省回退 tool）
	Plan       []PlanItem     `json:"plan"`
	TimeoutAt  time.Time      `json:"timeout_at"`
	PlanMode   bool           `json:"plan_mode,omitempty"` // plan 档计划卡语义（批准授权本轮全部写）
	Note       string         `json:"note,omitempty"`      // 卡面提示（如「批准后本轮后续写操作一并执行」）
	Diff       string         `json:"diff,omitempty"`      // 审批卡逐行 diff 载荷（apply_patch 补丁原文 / repo_commit git diff；超长截断）
	Items      []ApprovalItem `json:"items,omitempty"`     // 合并决议卡逐项（空 = 旧单卡形态）
}

// ItemDecisionOut 合并决议卡单项决议回执（切回/回放重建逐项终态的真源）。
type ItemDecisionOut struct {
	ItemID  string `json:"item_id"`
	Approve bool   `json:"approve"`
	Reason  string `json:"reason,omitempty"`
}

// DecisionOut 审批决议回执（事件流是切回/回放重建审批卡终态的真源）。
// 顶层 Approve/Reason = N=1 镜像或全项一致语义；N>1 分歧态以 Items 为准。
type DecisionOut struct {
	ApprovalID string            `json:"approval_id"`
	Approve    bool              `json:"approve"`
	Reason     string            `json:"reason,omitempty"`
	Items      []ItemDecisionOut `json:"items,omitempty"` // 合并决议逐项回执（空 = 旧单卡决议）
}

// AskOption 提问候选项（value 缺省 = label）。
type AskOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// AskReq 提问请求（事件载荷，前端提问卡数据源）。
type AskReq struct {
	AskID         string      `json:"ask_id"`
	Question      string      `json:"question"`
	Options       []AskOption `json:"options"`
	AllowMulti    bool        `json:"allow_multi"`
	AllowFreeText bool        `json:"allow_free_text"`
	TimeoutAt     time.Time   `json:"timeout_at"`
}

// PlanReq 计划请求（事件载荷，前端计划卡数据源）。Mode 分叉决议语义：
// plan 批准 = 授权任务期全部写（taskGrant）；manual 批准 = 仅确认方向不授权。
type PlanReq struct {
	PlanID    string     `json:"plan_id"`
	Task      string     `json:"task"`
	Summary   string     `json:"summary"`
	Steps     []PlanStep `json:"steps"`
	Risks     string     `json:"risks,omitempty"`
	Path      string     `json:"path"` // 用户域相对路径 plans/<sid>/<seq>.md
	Seq       int        `json:"seq"`
	Mode      string     `json:"mode,omitempty"`
	TimeoutAt time.Time  `json:"timeout_at"`
}

// PlanDecisionOut 计划决议回执（事件流是切回/回放重建计划卡终态的真源）。
type PlanDecisionOut struct {
	PlanID  string `json:"plan_id"`
	Approve bool   `json:"approve"`
	Reason  string `json:"reason,omitempty"`
}

// SteerEvent 排队消息事件载荷（steer_queued/updated/removed/injected）。
// Kind 标记条目来源：空 = 用户输入；notify = 系统通知（后台子代理完成回传，
// notify_queued/notify_injected 专用——排队区渲染为只读系统通知）。
type SteerEvent struct {
	ID          string       `json:"id"`
	Text        string       `json:"text,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Kind        string       `json:"kind,omitempty"`
}

// InterruptOut 打断行载荷（interrupted 事件——立即处理排队的取消收尾，
// 非故障形态；区别于 error ABORTED 的停止）。
type InterruptOut struct {
	Message string `json:"message"`
}

// UserMsg 用户消息记录（回放补全；附件结构化载荷）。
type UserMsg struct {
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// FileChange session_end 文件变更清单条目（会话累计，path 排序）。
type FileChange struct {
	Path   string `json:"path"`
	Action string `json:"action"` // created | edited | deleted
	Count  int    `json:"count"`
}

// SessionEnd 收尾。
type SessionEnd struct {
	Summary string       `json:"summary"`
	Files   []FileChange `json:"files,omitempty"` // 有改动才带（纯问答省缺）
}

// TransportRetry 传输重连通知（网络容错 ②：模型调用重试在途——事件层已实时
// 转发失败尝试的半截增量，eino 重试协议要求客户端自行 reset）。前端收此事件
// 回卷当前段半截显示 + 重连提示卡；历史不受影响（引擎侧已丢弃半截段）。
type TransportRetry struct {
	Attempt int `json:"attempt"` // 第几次重试（1 起）
	Max     int `json:"max"`     // 重试上限（llm.MaxRetries）
}

// ErrorOut 错误卡片（Code：CONFIG | SERVER | TRANSPORT | ABORTED | AUTH |
// RATE_LIMIT——后两者为网络容错 ③ 分类器新增：认证失败 / 限速重试耗尽）。
type ErrorOut struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// UsageOut usage 事件载荷（上下文按钮数据源）。分类三项为启发式估算——
// est_messages 已是整形后出站口径（H8-1：reasoning 剥离/空壳剔除后的真实
// 发送面）；est_saved = 原始口径差额（「整形节省」注记）。usage 真值注意
// DeepSeek prompt_tokens 含缓存命中（= hit+miss，双口径展示归应用）。
// SpawnID 非空 = 子代理面用量上卷（B2：子面无估算口径、四项为零；消费侧
// 按 SpawnID 归组聚合，空 = 主面）。
type UsageOut struct {
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	SpawnID          string `json:"spawn_id,omitempty"` // 子代理来源（空 = 主面）
	EstInstruction   int    `json:"est_instruction"`
	EstTools         int    `json:"est_tools"`
	EstMessages      int    `json:"est_messages"`
	EstSaved         int    `json:"est_saved,omitempty"` // 整形节省（原始-整形；0/负值省略）
}
