package contract

import "time"

// HITL 契约：审批决议与提问作答（应用端点写入 → 基座 Resume 消费），以及
// 审批包装的中断载荷（Suspend.Info 的三种形态——引擎按类型分叉事件卡）。

// 会话模式三档（应用可自定义集合；这三档是审批矩阵的既有语义）。
const (
	ModeManual = "manual" // 写工具逐次审批
	ModePlan   = "plan"   // 计划模式：submit_plan 出计划文档供审，批准授权任务期全部写（taskGrant）
	ModeAuto   = "auto"   // 裸传直落（硬红线仍在工具实现内代码级拒绝）
)

// ApprovalDecision 审批决议（应用 approve 端点 → 审批包装工具消费）。
type ApprovalDecision struct {
	Approve bool
	Reason  string
}

// AskDecision ask_user 作答（应用 answer 端点 → ask_user 工具消费）。
type AskDecision struct {
	Answers  []string
	FreeText string
}

// ApprovalCard 审批中断载荷（Suspend.Info；approval_request 事件数据源）。
// ItemID = 合并决议卡的项标识（H4-2：挂起时生成，随卡与保存态双带——
// Resume 重放时工具按保存态领各自项的决议）。
type ApprovalCard struct {
	Tool     string     `json:"tool"`
	Action   string     `json:"action,omitempty"` // 动作名（缺省回退 Tool）
	Args     string     `json:"-"`
	ItemID   string     `json:"item_id,omitempty"`
	PlanMode bool       `json:"plan_mode"` // plan 档计划卡（批准授权本轮全部写）
	Plan     []PlanItem `json:"plan"`
	Note     string     `json:"note,omitempty"`
	Diff     string     `json:"diff,omitempty"` // 审批卡逐行 diff 载荷——apply_patch 透传补丁原文 / repo_commit 现算 git diff；超长截断
}

// DiffProvider 工具可选接口：审批卡 diff 载荷（hitl.WrapTools 组卡时探测；
// 参数为工具入参 JSON，返回 diff 文本——人审逐行 diff 的数据源）。
type DiffProvider interface {
	ApprovalDiff(args string) string
}

// AskCard 提问中断载荷（Suspend.Info；ask_user_request 事件数据源）。
type AskCard struct {
	Question      string      `json:"question"`
	Options       []AskOption `json:"options"`
	AllowMulti    bool        `json:"allow_multi"`
	AllowFreeText bool        `json:"allow_free_text"`
}

// PlanStep 计划文档步骤。
type PlanStep struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// PlanCard 计划中断载荷（Suspend.Info 第三形态；plan_request 事件数据源）。
// 文档内容可由本卡完整重建（恢复流按决议状态重写整文件）。
type PlanCard struct {
	Task        string     `json:"task"` // 任务一句话
	Summary     string     `json:"summary"`
	Steps       []PlanStep `json:"steps"`
	Risks       string     `json:"risks,omitempty"`
	Seq         int        `json:"seq"`  // 文档序号（修订递增留痕）
	Path        string     `json:"path"` // 用户域相对路径 plans/<sid>/<seq>.md
	Mode        string     `json:"mode"` // 提交时档位（决议语义分叉：plan 批准=任务授权，manual 批准=仅确认方向）
	SubmittedAt time.Time  `json:"submitted_at"`
}
