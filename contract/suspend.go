package contract

import "context"

// Suspend 工具挂起哨兵（ask_user 同构）：Info = 事件面载荷（审批卡/提问卡，
// 应用层据此渲染交互卡），State = 恢复读回态（经 checkpoint 持久化）。
// 适配层（einox/einoext.Adapt）将其转引擎中断；恢复时经 WithResumeState
// 注回 ctx，工具据此走续跑分支（读取决议并继续）。
type Suspend struct {
	Info  any
	State any
}

func (s *Suspend) Error() string { return "einox: tool suspend" }

type ctxKey int

const (
	keyOperator ctxKey = iota
	keyChangeRecorder
	keyResumeState
	keyImageInput
)

// WithOperator 注入操作者（审计主体——会话发起人；业务工具经 OperatorOf 消费）。
func WithOperator(ctx context.Context, op string) context.Context {
	return context.WithValue(ctx, keyOperator, op)
}

// OperatorOf 读取操作者（空 = 无审计主体上下文）。
func OperatorOf(ctx context.Context) string {
	if v, ok := ctx.Value(keyOperator).(string); ok {
		return v
	}
	return ""
}

// ChangeRecorder 文件变更报备（会话域注入；session_end 载荷数据源）。
type ChangeRecorder func(path, action string)

// WithChangeRecorder 注入变更记录器（业务工具写文件时报备）。
func WithChangeRecorder(ctx context.Context, rec ChangeRecorder) context.Context {
	return context.WithValue(ctx, keyChangeRecorder, rec)
}

// ChangeRecorderOf 读取变更记录器（nil = 无记录上下文）。
func ChangeRecorderOf(ctx context.Context) ChangeRecorder {
	if v, ok := ctx.Value(keyChangeRecorder).(ChangeRecorder); ok {
		return v
	}
	return nil
}

// WithImageInput 注入会话模型的图片输入能力（引擎按模型声明写入；读图工具
// 经 ImageInputOf 消费——当前路由明示能力才放行，对齐官方 harness 的路由
// 能力断言）。
func WithImageInput(ctx context.Context, ok bool) context.Context {
	return context.WithValue(ctx, keyImageInput, ok)
}

// ImageInputOf 读取图片输入能力（false = 未声明或不支持）。
func ImageInputOf(ctx context.Context) bool {
	v, _ := ctx.Value(keyImageInput).(bool)
	return v
}

// WithResumeState 注入挂起恢复态（适配层在引擎恢复重放时调用）。
func WithResumeState(ctx context.Context, state any) context.Context {
	return context.WithValue(ctx, keyResumeState, state)
}

// ResumeStateOf 读取恢复态（挂起重放时工具消费；ok = 本次调用是恢复流）。
func ResumeStateOf(ctx context.Context) (state any, ok bool) {
	state, ok = ctx.Value(keyResumeState).(any)
	return state, ok
}
