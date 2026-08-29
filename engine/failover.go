package engine

// 主对话模型 Failover 降级链（H 系列补差）：Options.FallbackModels 复合键
// 清单非空时，adk ChatModelAgentConfig.ModelFailoverConfig 挂接——重试
//（ModelRetryConfig，有界重连）先耗尽、RetryExhaustedError 触发换链上模型
// 重进重试（每档各享完整重连预算——nanobot「先耗尽再降级」同款）；lastSuccess
// 粘滞归 adk（先试上次成功的模型）。切换时发 model_change 事件（From=上次
// 尝试的模型，To=降级目标；经 ctx 携带的 emitFn live 转发，无消费者时仅落
// 会话记录）。ShouldFailover 排除面：ctx 取消/中断信号（审批挂起续流不降级）
// 与 llm.Classify 致命类（401/403/402 配置错换模型无意义）；只对可重试类
//（429/5xx/网络）降级。子代理/拓扑子面不挂（子模型各异，链表按主模型语境
// 配置；维持 retry-only）。空清单 = 零变化。

import (
	"context"
	"errors"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// modelChain 复合键清单 → 同链包装模型链（NewModel + vision + shape——与主
// 模型/摘要模型同一包装序，出站整形口径不因降级漂移）。摘要侧（summarize.go）
// 与主模型侧共用。
func (m *Manager) modelChain(ctx context.Context, s *session.Session, keys []string, what string) ([]model.BaseModel[*schema.Message], error) {
	chain := make([]model.BaseModel[*schema.Message], 0, len(keys))
	for _, key := range keys {
		p, spec, ok := llm.FindSpec(m.Opt.Providers(), key)
		if !ok {
			return nil, &configError{what + "降级模型不在可用清单内：" + key}
		}
		cm, err := m.Opt.NewModel(ctx, p, spec, s.Model.Effort)
		if err != nil {
			return nil, &configError{what + "降级模型构造失败：" + err.Error()}
		}
		cm = llm.NewVisionModel(cm, spec, m.Opt.ImageResolve)
		cm = llm.NewHistoryShapeModel(cm, p.Kind)
		chain = append(chain, cm)
	}
	return chain, nil
}

// modelFailoverConfig 主模型降级配置（空清单 = nil 零变化）。
func (m *Manager) modelFailoverConfig(ctx context.Context, s *session.Session) *adk.ModelFailoverConfig[*schema.Message] {
	if len(m.Opt.FallbackModels) == 0 {
		return nil
	}
	chain, err := m.modelChain(ctx, s, m.Opt.FallbackModels, "主模型")
	if err != nil {
		// 装配期探测失败不阻断运行（清单错配降级即失效，主模型照常重试兜底）
		// ——harness_note 通知卡留痕；与「单端点装配配了也白配」同一宽容语义。
		s.Record(contract.EvHarnessNote, contract.HarnessNote{
			Kind: "failover", Title: "主模型降级链装配失败，本次运行不降级", Detail: err.Error(),
		})
		return nil
	}
	keys := append([]string(nil), m.Opt.FallbackModels...)
	last := s.Model.Model // 上次尝试的模型（From 基准；粘滞后可能是链上模型）
	maxR := uint(len(chain))
	return &adk.ModelFailoverConfig[*schema.Message]{
		MaxRetries: maxR,
		ShouldFailover: func(ctx context.Context, _ *schema.Message, err error) bool {
			if err == nil || ctx.Err() != nil {
				return false // 成功 / ctx 取消：不切
			}
			var sig *adk.InterruptSignal
			if errors.As(err, &sig) {
				return false // 中断穿透不降级（审批挂起续流）
			}
			return llm.Classify(unwrapRetryExhausted(err)).Retryable // 致命类（401/403/402）不切
		},
		GetFailoverModel: func(ctx context.Context, fctx *adk.FailoverContext[*schema.Message]) (model.BaseModel[*schema.Message], []*schema.Message, error) {
			if int(fctx.FailoverAttempt) > len(chain) {
				return nil, nil, errors.New("主模型降级链尽")
			}
			i := int(fctx.FailoverAttempt) - 1
			mc := contract.ModelChange{From: last, To: keys[i]}
			if fn, ok := emitFnFrom(ctx); ok { // 切换可见性（live + 回放同源）
				m.emit(s, fn, contract.EvModelChange, mc)
			} else {
				s.Record(contract.EvModelChange, mc)
			}
			last = keys[i]
			return chain[i], nil, nil // 输入原样（nil = 原始输入；窗口差异归 reduction 兜底）
		},
	}
}

// ---- model_change 事件构造与 ctx 携带（避免 failover.go 直引 contract 的
// 载荷命名散落；emitFn 经 ctx 传递是 Run/Resume → 模型闭包的既有信息缺口） ----

type emitFnCtxKey struct{}

// withEmitFn 把本轮事件回调塞进 runCtx（failover 切换时 live 转发 model_change）。
func withEmitFn(ctx context.Context, fn emitFn) context.Context {
	return context.WithValue(ctx, emitFnCtxKey{}, fn)
}

// emitFnFrom 取出（无则 false）。
func emitFnFrom(ctx context.Context) (emitFn, bool) {
	fn, ok := ctx.Value(emitFnCtxKey{}).(emitFn)
	return fn, ok
}
