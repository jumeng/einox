// Package contract 是 einox 对应用层暴露的最小契约面：工具/事件/审批/
// 挂起/上下文注入协议（event.go/tool.go/hitl.go/suspend.go）+ 会话域载荷
// （session.go：UserPrefs/Attachment/Participant/QueuedMsg）+ 横切常量与
// 小函数（错误码封闭词表、effort 归一、参数错误翻译）。零 eino 类型——
// 业务系统只见本包与基座各机制包的公开面，换地基只动基座内部
// （2026-08-24 agent 基座定案 §2：从现状提炼，不发明）。
package contract
