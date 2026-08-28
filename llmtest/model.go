// Package llmtest 提供剧本化假模型（应用侧测试注入——公开面零 eino 类型，
// 业务测试不 import eino 即可模拟模型行为）。Stream 每次调用按序消费一个
// Turn 的剧本；用尽后回退 Plain 文本。Generate 同剧本取文本。
package llmtest

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/llm"
)

// ToolCallSpec 工具调用剧本（ID/名称/参数 JSON 字符串）。
type ToolCallSpec struct {
	ID   string
	Name string
	Args string
}

// Turn 单次模型调用剧本：Thinking 与 Text 合成 assistant 消息（思考字段 +
// 内容字段），ToolCalls 随同消息；三者全空 = 纯文本「已处理。」。
type Turn struct {
	Thinking  string
	Text      string
	ToolCalls []ToolCallSpec
	// Usage 流末 usage chunk（Prompt/Completion/Total）；nil 不发。
	Usage *Usage
	// Fragments 非空时忽略 ToolCalls：ToolCalls 按分片形态发送（测流式
	// 参数增量归并——每项一次 chunk，首片带 ID/Name，续片仅 Args 增量）。
	Fragments []ToolCallSpec
	// Err 非 nil = 本轮以该错误收流（在发送 Thinking/Text 剧本内容之后注入
	// ——模拟流中途断连；Generate 路径同错误直接返回）。网络容错测试锚点：
	// 注 llm.ErrIdleTimeout 等可重试错误可驱动引擎重试层。
	Err error
}

// Usage token 用量剧本。
type Usage struct {
	Prompt     int
	Completion int
	Total      int
}

// Model 剧本假模型（实现 llm.ModelFactory 的返回类型；并发安全）。
type Model struct {
	mu     sync.Mutex
	script []Turn
	inputs [][]*schema.Message
}

// New 构造（script 为空 = 恒纯文本回话）。
func New(script ...Turn) *Model { return &Model{script: script} }

// Factory 包成 ModelFactory（直接注入 engine.Options.NewModel）。
func (m *Model) Factory() llm.ModelFactory {
	return func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return m, nil
	}
}

// Switch 按构造序分派多个 factory（应用层测试的父子模型装配用——引擎按
// 构造序先建主模型再建子代理/摘要模型，本函数让测试免直引 eino 类型即可
// 完成分派； factories 依次消费，超界恒回退末位）。
func Switch(factories ...llm.ModelFactory) llm.ModelFactory {
	n := 0
	return func(ctx context.Context, p llm.ProviderSpec, s llm.ModelSpec, e string) (model.BaseModel[*schema.Message], error) {
		idx := n
		n++
		if idx >= len(factories) {
			idx = len(factories) - 1
		}
		return factories[idx](ctx, p, s, e)
	}
}

// RecCall 一次模型构造记录（引擎 assemble 实际使用的模型 id + effort 档）。
type RecCall struct {
	Model  string
	Effort string
}

// Recorder 构造入参记录器（并发安全）——会话内切换生效链测试锚点：断言
// 「下一次 LLM 交互」的工厂入参即会话快照现值。
type Recorder struct {
	mu    sync.Mutex
	calls []RecCall
}

// Calls 快照返回已记录的构造序列。
func (r *Recorder) Calls() []RecCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// Last 末次构造记录（空序列返回零值）。
func (r *Recorder) Last() RecCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return RecCall{}
	}
	return r.calls[len(r.calls)-1]
}

// Record 包装 factory 透传并记录每次构造入参（模型 id + effort）。
func Record(f llm.ModelFactory) (llm.ModelFactory, *Recorder) {
	r := &Recorder{}
	return func(ctx context.Context, p llm.ProviderSpec, s llm.ModelSpec, e string) (model.BaseModel[*schema.Message], error) {
		r.mu.Lock()
		r.calls = append(r.calls, RecCall{Model: s.ID, Effort: e})
		r.mu.Unlock()
		return f(ctx, p, s, e)
	}, r
}

var _ model.BaseModel[*schema.Message] = (*Model)(nil)

// take 取第 n 次调用的剧本（n 从 1 起；超界回退纯文本）。
func (m *Model) take(n int) Turn {
	if len(m.script) == 0 || n > len(m.script) {
		return Turn{Text: "已处理。"}
	}
	return m.script[n-1]
}

func (m *Model) Generate(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	n := m.record(input)
	t := m.take(n)
	if t.Err != nil {
		return nil, t.Err
	}
	if len(t.ToolCalls) > 0 {
		calls := make([]schema.ToolCall, 0, len(t.ToolCalls))
		for _, c := range t.ToolCalls {
			calls = append(calls, schema.ToolCall{ID: c.ID, Type: "function",
				Function: schema.FunctionCall{Name: c.Name, Arguments: c.Args}})
		}
		return schema.AssistantMessage(t.Text, calls), nil
	}
	return schema.AssistantMessage(t.Text, nil), nil
}

func (m *Model) Stream(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	n := m.record(input)
	t := m.take(n)
	sr, sw := schema.Pipe[*schema.Message](8)
	go func() {
		defer sw.Close()
		send := func(msg *schema.Message) { sw.Send(msg, nil) }
		if t.Usage != nil {
			// usage 随流末 chunk 携带（ResponseMeta）
			send(&schema.Message{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{
				Usage: &schema.TokenUsage{PromptTokens: t.Usage.Prompt, CompletionTokens: t.Usage.Completion, TotalTokens: t.Usage.Total},
			}})
		}
		if len(t.Fragments) > 0 {
			// 分片形态（OpenAI 协议）：同一调用的全部分片共享一个 Index 锚；
			// 首片原子（ID/Name + 首段 Args），续片仅 Args 增量——引擎按 Index
			// 归并参数。Fragments 全体属于同一次工具调用。
			idx := 0
			for i, f := range t.Fragments {
				tc := schema.ToolCall{Index: &idx, Type: "function",
					Function: schema.FunctionCall{Arguments: f.Args}}
				if i == 0 {
					tc.ID = f.ID
					tc.Function.Name = f.Name
				}
				send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{tc}})
			}
			return
		}
		if t.Thinking != "" {
			send(&schema.Message{Role: schema.Assistant, ReasoningContent: t.Thinking})
		}
		if len(t.ToolCalls) > 0 {
			calls := make([]schema.ToolCall, 0, len(t.ToolCalls))
			for _, c := range t.ToolCalls {
				calls = append(calls, schema.ToolCall{ID: c.ID, Type: "function",
					Function: schema.FunctionCall{Name: c.Name, Arguments: c.Args}})
			}
			send(&schema.Message{Role: schema.Assistant, ToolCalls: calls})
			return
		}
		if t.Text != "" {
			send(&schema.Message{Role: schema.Assistant, Content: t.Text})
		} else {
			send(&schema.Message{Role: schema.Assistant, Content: "已处理。"})
		}
		if t.Err != nil {
			sw.Send(nil, t.Err) // 流中途错误（defer Close 其后收线）
		}
	}()
	return sr, nil
}

// record 记录输入，返回本次是第几次调用。
func (m *Model) record(input []*schema.Message) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inputs = append(m.inputs, append([]*schema.Message(nil), input...))
	return len(m.inputs)
}

// CallCount 模型调用次数。
func (m *Model) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.inputs)
}

// LastInputLen 最近一次输入的消息数。
func (m *Model) LastInputLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.inputs) == 0 {
		return 0
	}
	return len(m.inputs[len(m.inputs)-1])
}

// Inputs 全部调用的输入消息快照（多模态 part 断言用；逐调用独立副本）。
func (m *Model) Inputs() [][]*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]*schema.Message, len(m.inputs))
	copy(out, m.inputs)
	return out
}

// LastInputToolResults 最近一次输入中 tool 消息内容（react 续答验证）。
func (m *Model) LastInputToolResults() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.inputs) == 0 {
		return nil
	}
	var out []string
	for _, msg := range m.inputs[len(m.inputs)-1] {
		if msg.Role == schema.Tool {
			out = append(out, msg.Content)
		}
	}
	return out
}

// ImagePartsOf 第 n 次调用（1 基，0 = 最近一次）全部消息中的 image_url part 数
// （多模态链路断言——应用层测试零 eino import 的替代面）。
func (m *Model) ImagePartsOf(n int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n == 0 && len(m.inputs) > 0 {
		n = len(m.inputs)
	}
	if n < 1 || n > len(m.inputs) {
		return 0
	}
	c := 0
	for _, msg := range m.inputs[n-1] {
		for _, p := range msg.UserInputMultiContent {
			if p.Type == schema.ChatMessagePartTypeImageURL {
				c++
			}
		}
	}
	return c
}

// Dump 调试输出（失败诊断用）。
func (m *Model) Dump() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := fmt.Sprintf("calls=%d", len(m.inputs))
	for i, in := range m.inputs {
		s += fmt.Sprintf("\n-- call %d: %d msgs", i+1, len(in))
		for _, msg := range in {
			body := msg.Content
			if len(body) > 80 {
				body = body[:80] + "…"
			}
			s += fmt.Sprintf("\n   [%s] %s", msg.Role, body)
		}
	}
	return s
}
