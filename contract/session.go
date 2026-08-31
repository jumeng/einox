package contract

// 会话状态常量（会话域状态机公开值）。
const (
	StateRunning         = "running"
	StatePendingApproval = "pending_approval"
	StateEnded           = "ended"
	StateError           = "error"
)

// UserPrefs 用户模型偏好（会话模型快照与用户默认；字段语义见 einox/llm）。
type UserPrefs struct {
	Model  string `json:"model"`  // provider/model 复合键
	Effort string `json:"effort"` // 思考档位 off | low | high | max（旧值 on 由读侧归一 max、未知归一 low；能否真关由端点定）
	Mode   string `json:"mode"`   // 会话模式 manual | plan | auto
}

// Attachment 用户消息附件（应用层注入文本以路径引用带给模型；IsImage 供
// 前端渲染图片标）。
type Attachment struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsImage bool   `json:"is_image"`
}

// QueuedMsg 排队消息（steering 入队；可编辑/删除。ID 是事件定位锚）。
// Kind 标记条目来源：空 = 用户输入（可编辑/删除）；notify = 系统通知
// （后台子代理完成回传注入——只读，编辑/删除面拒绝）。
type QueuedMsg struct {
	ID          string       `json:"id"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Kind        string       `json:"kind,omitempty"`
}
