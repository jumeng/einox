package engine

// spawn 结构化回传（T8 吸收设计 §1.3-② 修订 + §2.3-③）：SpawnOutput.Schema
// 声明期望结论 → 子代理面注入 spawn_submit 工具（校验失败信封回喂自纠、
// 有界于 maxIterations）；合法提交置位收束信号——concludeModel 在下一次
// 模型调用前拦截、返回无工具调用合成消息让 adk 循环自然终结（零空跑模型
// 轮，dsh structured_output 两阶段提交的 einox 形态）。
//
// 信号经 ctx 携带（每次 spawn 调用一个）：外层包装注入 → 穿透
// throttledSpawn/spawnFailFeed/agent_tool → 子 run 的模型包装与 submit 工具
// 同源可见——共享 subCM（装配级复用）无需每调用重建模型包装。

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/mid"
)

// SpawnOutput 结构化回传配置（nil = 自由文本结论零变化）。
type SpawnOutput struct {
	// Schema 期望结论 JSON Schema（根形必须 object——构造期校验拒收）。
	Schema *contract.Schema
}

// concludeSignal 单次子代理运行的结构化收束信号（ctx 携带）。
type concludeSignal struct {
	mu    sync.Mutex
	done  bool
	value json.RawMessage
}

type concludeCtxKey struct{}

func withConcludeSignal(ctx context.Context, sig *concludeSignal) context.Context {
	return context.WithValue(ctx, concludeCtxKey{}, sig)
}

func concludeSignalOf(ctx context.Context) *concludeSignal {
	sig, _ := ctx.Value(concludeCtxKey{}).(*concludeSignal)
	return sig
}

func (c *concludeSignal) submit(v json.RawMessage) {
	c.mu.Lock()
	c.done, c.value = true, v
	c.mu.Unlock()
}

func (c *concludeSignal) result() (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value, c.done
}

// concludeModel 收束感知模型包装（装配级套在 subCM 外，状态全在 ctx 信号——
// 共享实例并发安全）：已提交后的一切模型调用被合成无工具调用 assistant
// 截断——adk ReAct 见无工具调用即自然终结。
type concludeModel struct {
	model.BaseModel[*schema.Message]
}

func (c *concludeModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if concludeModelIntercept(ctx) {
		return syntheticAssistantStream(), nil
	}
	return c.BaseModel.Stream(ctx, in, opts...)
}

func (c *concludeModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	if concludeModelIntercept(ctx) {
		return &schema.Message{Role: schema.Assistant}, nil
	}
	return c.BaseModel.Generate(ctx, in, opts...)
}

func concludeModelIntercept(ctx context.Context) bool {
	sig := concludeSignalOf(ctx)
	if sig == nil {
		return false
	}
	_, done := sig.result()
	return done
}

func syntheticAssistantStream() *schema.StreamReader[*schema.Message] {
	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		sw.Send(&schema.Message{Role: schema.Assistant}, nil)
	}()
	return sr
}

// spawnSubmitTool 结构化结论提交工具（注入子代理面——不在白名单筛内，
// Output 配置时专用）：校验失败信封回喂自纠（有界于子代理轮次预算）；
// 合法提交置位收束信号，canonical 值经信号回传外层包装进父上下文。
type spawnSubmitTool struct {
	schema *contract.Schema
}

func (t *spawnSubmitTool) Info() *contract.ToolInfo {
	return &contract.ToolInfo{
		Name:   "spawn_submit",
		Desc:   "提交最终结论（JSON 参数须符合给定 schema）——一经采纳本轮即收束，不要以文本形式另行回答结论",
		Params: t.schema,
	}
}

func (t *spawnSubmitTool) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	sig := concludeSignalOf(ctx)
	if sig == nil {
		return json.RawMessage(`{"ok":false,"error":"收束信号未装配（结构化回传配置异常）"}`), nil
	}
	if verr := mid.ValidateJSON(t.schema, args); verr != nil {
		b, _ := json.Marshal(map[string]any{
			"ok": false, "error": "结论不符合 schema：" + verr.Error() + "——请修正后重新 spawn_submit 提交"})
		return b, nil // 回喂自纠（dsh 形态：不合 schema 不落地）
	}
	sig.submit(args)
	return json.RawMessage(`{"ok":true,"message":"结论已采纳，本轮收束"}`), nil
}

// spawnOutputInstruction Output 配置时的子代理提示词追加段。
const spawnOutputInstruction = `

【结构化结论纪律】任务完成时必须调用 spawn_submit 工具提交最终结论：参数是符合给定 schema 的 JSON 对象（字段名与类型严格对齐，必填字段不可缺）。结论只能经 spawn_submit 提交——不要以文本形式直接回答结论；schema 校验失败会收到修正提示，修正后重新提交。`
