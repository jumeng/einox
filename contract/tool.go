package contract

import (
	"context"
	"encoding/json"
)

// Tool 业务与通用工具的统一契约。args/result 均为 JSON 字节——传输无关
// （SSE/WS/CLI 由应用层自定），参数 schema 由 ToolInfo 携带。
// 工具实现不落审批语义——写面审批由基座组装期包装（hitl）统一处理；
// 需要挂起交互的工具（ask_user 同构）以 *Suspend 哨兵上抛（见 suspend.go）。
type Tool interface {
	// Info 工具元数据（名称/描述/参数 schema）。
	Info() *ToolInfo
	// Invoke 执行一次调用。返回值为 JSON 字节；业务性失败以 JSON 信封
	// {"ok":false,"error":…} 回喂模型自纠（Go error 会终止整轮）。
	Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

// ToolInfo 工具元数据。
type ToolInfo struct {
	Name   string  `json:"name"`
	Desc   string  `json:"desc"`
	Params *Schema `json:"params,omitempty"` // JSON Schema（object 形）
	// Behavior 行为面标记（UI-B2：过程流「探索/更改/终端」分组的展示语义
	// 数据源；空 = 未标记——交互卡/待办类不入组，前端按工具名兜底）。
	// 与 hitl 写面名单无关：名单 = 审批语义（写才拦），本标记 = 展示语义。
	Behavior string `json:"behavior,omitempty"`
}

// 行为面标记值（ToolInfo.Behavior；工具注册期自declare——内容归业务）。
const (
	BehaviorRead  = "read"  // 探索组：读文件/列表/搜索/取网页
	BehaviorWrite = "write" // 更改组：写文件/改数据/落文档
	BehaviorExec  = "exec"  // 终端组：跑命令/跑脚本（非只读进程执行）
)

// Schema 参数 JSON Schema 的最小子集（object 形）。与 eino ParamsOneOf 经
// JSON 往返互转（einox/einoext 的 Adapt/Bridge 桥）。Minimum/Maximum 供
// mid.Validate 数值边界校验（数值类型约束，声明了才校验）。
type Schema struct {
	Type        string             `json:"type,omitempty"`
	Description string             `json:"description,omitempty"`
	Enum        []string           `json:"enum,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Minimum     *float64           `json:"minimum,omitempty"`
	Maximum     *float64           `json:"maximum,omitempty"`
}
