// Package engine 内文件：订阅式工具钩子（ToolHooks）。与 ToolWrap 的分工——
// ToolWrap 管包装改写（有状态包装、结果改写），本缝管零样板订阅：审计观察
// 与策略拦截不必再写包装类型。
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/jumeng/einox/contract"
)

// ToolHooks 订阅式工具钩子（审计/准入零样板订阅口）。挂 wrapFace 最外层
// （包装序 hitl → ToolWrap → Hooks → einoext）：Pre 先于审批决策触发、Post
// 收工具原始返回（reduction 外真值）与耗时——审批拒绝的调用也过钩子（Pre
// 触发 ≠ 实际执行，Post 以拒绝信封为结果——审计完整）。主面与子代理面同挂
// （wrapFace 共用）——子代理面（spawn/拓扑）的调用记在**父会话名下**
// （Sess = 父身份，会话粒度审计；子面调用与主面调用在钩子层不可区分）；
// spawn 派发本体不经钩子（同 ToolWrap 纪律）。
// 失败语义（只能收紧不能放宽——与 ToolWrap 同纪律）：
//   - Pre 返回 error：拒绝执行，{"ok":false,"error":"pre-hook: …"} 信封回喂
//     模型自纠（Go error 不上抛——轮不终止）；Post 以拒绝信封为结果补发；
//   - Pre panic：recover 转拒绝（fail-closed——钩子崩了不放行）；
//   - Post panic：recover 记日志（观察不破坏运行）。
//
// 双回调全 nil 的空结构 = 原样放行零变化。
type ToolHooks struct {
	Pre  func(c ToolHookCall) error
	Post func(c ToolHookCall, r ToolHookResult)
}

// ToolHookCall 单次工具调用钩子载荷（Args = Invoke 实参原文）。
type ToolHookCall struct {
	Sess SessionBrief // 含 ParentSID——side 面可区分
	Name string
	Args json.RawMessage
}

// ToolHookResult 工具结果观察载荷（Result 信封语义同 contract.Tool：
// ok:false = 业务失败；Err 非 nil = 框架级失败）。Result 为工具原始返回
// （reduction 截断/外置之前的真值——审计口径）。
type ToolHookResult struct {
	Result json.RawMessage
	Err    error
	Dur    time.Duration
}

// hookFace 钩子包装（nil Hooks 或双 nil 回调 = 原样返回零变化；包装随每次
// assemble 重建——与 ToolWrap 同生命周期契约）。
func (m *Manager) hookFace(ts []contract.Tool, sess SessionBrief) []contract.Tool {
	h := m.Opt.Hooks
	if h == nil || (h.Pre == nil && h.Post == nil) {
		return ts
	}
	out := make([]contract.Tool, len(ts))
	for i, t := range ts {
		out[i] = &hookedTool{Tool: t, hooks: h, sess: sess, name: t.Info().Name}
	}
	return out
}

// hookedTool 钩子包装实例（Info 透传——名字是审批名单/子代理白名单的寻址键）。
type hookedTool struct {
	contract.Tool
	hooks *ToolHooks
	sess  SessionBrief
	name  string
}

func (h *hookedTool) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	c := ToolHookCall{Sess: h.sess, Name: h.name, Args: args}
	if h.hooks.Pre != nil {
		if err := safePreHook(h.hooks.Pre, c); err != nil {
			res := denyEnvelope(err)
			h.firePost(c, ToolHookResult{Result: res})
			return res, nil
		}
	}
	start := time.Now()
	res, err := h.Tool.Invoke(ctx, args)
	h.firePost(c, ToolHookResult{Result: res, Err: err, Dur: time.Since(start)})
	return res, err
}

// firePost Post 触发（panic 记日志不破坏运行）。
func (h *hookedTool) firePost(c ToolHookCall, r ToolHookResult) {
	if h.hooks.Post == nil {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("engine: PostToolCall 钩子 panic（已忽略）：%v", rec)
		}
	}()
	h.hooks.Post(c, r)
}

// safePreHook Pre 触发（panic 收敛为拒绝——fail-closed）。
func safePreHook(pre func(ToolHookCall) error, c ToolHookCall) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("钩子 panic：%v", rec)
		}
	}()
	return pre(c)
}

// denyEnvelope 拒绝信封（回喂模型自纠——与 hitl 拒绝信封同形态）。经
// json.Marshal 构造：钩子错误文案是应用任意字符串，Sprintf 直拼遇引号/
// 换行即产非法 JSON。
func denyEnvelope(err error) json.RawMessage {
	type env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	b, mErr := json.Marshal(env{OK: false, Error: "pre-hook: " + err.Error() + "——订阅钩子策略拦截，请调整方案或向用户确认"})
	if mErr != nil {
		return json.RawMessage(`{"ok":false,"error":"pre-hook: 拒绝信封构造失败"}`)
	}
	return b
}
