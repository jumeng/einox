package engine

// T8 输出契约回归（吸收设计 §1.3-① 修订形态）：装配期探测 + 最内层包装——
// 成功路径按 schema 校验（required 在场）、Render 产出模型可见投影、失败
// 信封豁免、校验失败信封回喂自纠、未实现契约的工具逐字节零变化。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// bigReportTool 实现输出契约的大 JSON 工具：canonical 值大对象，Render 只
// 投影计数摘要（模型可见面瘦身——office/xlsx/报表类的典型形态）。
type bigReportTool struct{}

func (bigReportTool) Info() *contract.ToolInfo {
	return &contract.ToolInfo{Name: "big_report", Desc: "报表", Params: &contract.Schema{Type: "object"}}
}

func (bigReportTool) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"rows":[{"v":1},{"v":2},{"v":3}],"meta":{"gen":"x"}}`), nil
}

func (bigReportTool) OutputSchema() *contract.Schema {
	return &contract.Schema{Type: "object", Required: []string{"rows", "meta"}}
}

func (bigReportTool) Render(args, result json.RawMessage) string {
	return "报表已生成：共 3 行（详情见事件流）"
}

func TestOutputContractRenderAndValidate(t *testing.T) {
	var toolSeen string // 工具返回的模型可见结果（父模型输入里的 tool 消息）
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("cr", "big_report", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m := newTestManager(t, func(o *Options) {
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{bigReportTool{}} }
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("u1", "报表", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "出报表", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	// 模型第二轮输入里的 tool 结果 = Render 投影（非 canonical 大 JSON）
	if len(fm.inputsOf()) < 2 {
		t.Fatalf("应有两轮模型调用，实得 %d", len(fm.inputsOf()))
	}
	for _, msg := range fm.inputsOf()[1] {
		if msg.Role == schema.Tool && strings.Contains(msg.Content, "报表已生成") {
			toolSeen = msg.Content
		}
	}
	if toolSeen == "" {
		t.Fatal("模型可见面应为 Render 投影文本")
	}
	if strings.Contains(toolSeen, `"rows"`) {
		t.Fatal("canonical 大 JSON 不应直接回喂模型")
	}
}

func TestOutputContractValidationFailFeedsBack(t *testing.T) {
	// 工具返回缺 meta（required 违规）→ 校验失败信封回喂，模型可自纠
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("cb", "bad_report", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m := newTestManager(t, func(o *Options) {
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{badReportTool{}} }
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("u1", "报表", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "出报表", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	found := false
	for _, msg := range fm.inputsOf()[1] {
		if msg.Role == schema.Tool && strings.Contains(msg.Content, "不符合声明的 schema") && strings.Contains(msg.Content, "meta") {
			found = true
		}
	}
	if !found {
		t.Fatal("required 违规应以信封回喂（含字段名可自纠）")
	}
}

type badReportTool struct{}

func (badReportTool) Info() *contract.ToolInfo {
	return &contract.ToolInfo{Name: "bad_report", Desc: "坏报表", Params: &contract.Schema{Type: "object"}}
}

func (badReportTool) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"rows":[]}`), nil // 缺 meta
}

func (badReportTool) OutputSchema() *contract.Schema {
	return &contract.Schema{Type: "object", Required: []string{"rows", "meta"}}
}

func (badReportTool) Render(args, result json.RawMessage) string { return "不应到达" }
