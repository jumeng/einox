package engine

// 批次 B 装配缝回归（设计真源 findings/2026-08-29-assembly-seams-design.md
// §3/§5）：ToolWrap 工具包装缝挂 hitl 审批外层——单调收紧（审批不可豁免、
// 重放时刻重算）、deny 走错误信封回喂模型自纠、覆盖会话域件与子代理面。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// wrapGate 计数/拒绝对（ToolWrap 测试件——闭包持有共享态，跨 assemble 存续：
// 包装随每次 Run/Resume 重建，计数必须在包装外）。
type wrapGate struct {
	mu    sync.Mutex
	seen  map[string]int // 进入包装计数（审批挂起也计——包装在审批外层）
	calls map[string]int // 透传到真实工具计数（过审批后）
	deny  atomic.Bool    // 拒绝开关（重放时刻判定测试在 Run/Resume 间隙翻）
}

func newWrapGate() *wrapGate {
	return &wrapGate{seen: map[string]int{}, calls: map[string]int{}}
}

func (g *wrapGate) wrap(t contract.Tool) contract.Tool { return &gateTool{g: g, t: t} }

func (g *wrapGate) seenOf(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.seen[name]
}

func (g *wrapGate) callsOf(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[name]
}

// gateTool 契约合规包装样板：Info 透传、deny 以错误信封返回、透传保留内层
// 审批语义。
type gateTool struct {
	g *wrapGate
	t contract.Tool
}

func (w *gateTool) Info() *contract.ToolInfo { return w.t.Info() }

func (w *gateTool) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	name := ""
	if info := w.t.Info(); info != nil {
		name = info.Name
	}
	w.g.mu.Lock()
	w.g.seen[name]++
	w.g.mu.Unlock()
	if w.g.deny.Load() {
		return json.RawMessage(`{"ok":false,"error":"策略拒绝：门禁测试拦截（ToolWrap deny 信封）"}`), nil
	}
	out, err := w.t.Invoke(ctx, args)
	if err != nil {
		return nil, err
	}
	w.g.mu.Lock()
	w.g.calls[name]++
	w.g.mu.Unlock()
	return out, nil
}

// gateWriteTool 计数写工具（进审批名单用）。
func gateWriteTool(t *testing.T, name string, calls *int32) contract.Tool {
	t.Helper()
	wt, err := tools.InferTool(name, "写桩", func(context.Context, struct{}) (map[string]any, error) {
		atomic.AddInt32(calls, 1)
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return wt
}

// TestToolWrapKeepsApprovalMandatory 单调收紧（结构层）：passthrough 包装下
// manual 档写工具仍必须走审批——包装在挂起点之前被进入（seen），但真实执行
// 只发生在决议批准之后（calls）；批准后执行、正常收口。
func TestToolWrapKeepsApprovalMandatory(t *testing.T) {
	gate := newWrapGate()
	var calls int32
	wt := gateWriteTool(t, "write_tool", &calls)
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{tcOf("c1", "write_tool", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{wt} }
		o.Approval = hitl.ApprovalConfig{WriteTools: map[string]bool{"write_tool": true}}
		o.ToolWrap = gate.wrap
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "写", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var card *contract.ApprovalReq
	m.Run(context.Background(), s, "写一个", nil, func(ev session.Event) {
		if ev.Event == contract.EvApprovalRequest {
			req := ev.Data.(contract.ApprovalReq)
			card = &req
		}
	})
	t.Cleanup(func() { stopApprovalTimer(s.SID) })
	if card == nil || len(card.Items) == 0 {
		t.Fatal("passthrough 包装下 manual 档仍应挂起审批")
	}
	if gate.seenOf("write_tool") == 0 {
		t.Fatal("包装应在审批外层（挂起前已被进入）")
	}
	if atomic.LoadInt32(&calls) != 0 || gate.callsOf("write_tool") != 0 {
		t.Fatal("挂起期不得执行")
	}
	for _, it := range card.Items {
		s.SetDecisionFor(it.ItemID, contract.ApprovalDecision{Approve: true})
	}
	m.Resume(context.Background(), s, func(session.Event) {})
	waitTitleFlight(t, s)
	if atomic.LoadInt32(&calls) != 1 || gate.callsOf("write_tool") != 1 {
		t.Fatalf("批准后应执行一次，实得 tool=%d gate=%d", calls, gate.callsOf("write_tool"))
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("批准后应正常收口，终态 %s", s.StateOf())
	}
}

// TestToolWrapDenyEnvelope deny 语义：拒绝以 {"ok":false} 信封回喂——
// tool_result（非 error 事件）可辨、历史可见（模型可自纠）、轮次正常收尾。
func TestToolWrapDenyEnvelope(t *testing.T) {
	gate := newWrapGate()
	gate.deny.Store(true)
	var calls int32
	wt := gateWriteTool(t, "write_tool", &calls)
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{tcOf("c1", "write_tool", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{wt} }
		o.ToolWrap = gate.wrap
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "写", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var names []string
	var denied []bool
	m.Run(context.Background(), s, "写一个", nil, func(ev session.Event) {
		names = append(names, ev.Event)
		if ev.Event == contract.EvToolResult {
			if tr, ok := ev.Data.(contract.ToolResult); ok {
				denied = append(denied, !tr.OK)
			}
		}
	})
	waitTitleFlight(t, s)
	if contains(names, contract.EvError) {
		t.Fatalf("deny 应走信封回喂而非终止轮次：%v", names)
	}
	if len(denied) != 1 || !denied[0] {
		t.Fatalf("应有一条失败 tool_result 信封：%v", names)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("deny 不得透传执行")
	}
	if !contains(names, contract.EvSessionEnd) {
		t.Fatalf("deny 后轮次应正常收尾：%v", names)
	}
	seenHistory := false
	for _, msg := range s.CloneHistory() {
		if msg.Role == schema.Tool && strings.Contains(msg.Content, "策略拒绝") {
			seenHistory = true
		}
	}
	if !seenHistory {
		t.Fatal("deny 信封应入史（模型可见可自纠）")
	}
}

// TestToolWrapCoversSessionTools 会话域件覆盖：包装对基座件（run_command）
// 同样生效——ToolWrap 面与 hitl 面同源（全量 ts）。
func TestToolWrapCoversSessionTools(t *testing.T) {
	gate := newWrapGate()
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "run_command", `{"command":"echo hi"}`),
			}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.ToolWrap = gate.wrap
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "跑命令", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "跑一下", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if gate.seenOf("run_command") != 1 || gate.callsOf("run_command") != 1 {
		t.Fatalf("会话域件应过包装：seen=%d calls=%d",
			gate.seenOf("run_command"), gate.callsOf("run_command"))
	}
}

// TestToolWrapCoversSubAgentFace 子代理面覆盖：spawn 子面经 wrapFace 同挂——
// 子代理调用的白名单件同样过包装（审计/准入对委派面生效）。
func TestToolWrapCoversSubAgentFace(t *testing.T) {
	gate := newWrapGate()
	var subCalls int32
	probe, err := tools.InferTool("sub_probe", "子面探针", func(context.Context, struct{}) (map[string]any, error) {
		atomic.AddInt32(&subCalls, 1)
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var seq atomic.Int32
	factory := func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		who := int(seq.Add(1)) // 1=父（assemble）2=子（newSpawnTool）3+=标题等
		return &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
			if n > 1 || who > 2 {
				send(&schema.Message{Role: schema.Assistant, Content: "完成"})
				return
			}
			if who == 1 { // 父：派发子代理
				send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
					tcOf("sp1", "spawn", `{"task":"探测","tools":"sub_probe"}`),
				}})
				return
			}
			// 子：调白名单件
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("csub", "sub_probe", `{}`),
			}})
		}}, nil
	}
	m := newSeamManager(t, func(o *Options) {
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{probe} }
		o.SubAgents = &SubAgentsConfig{Tools: []string{"sub_probe"}}
		o.ToolWrap = gate.wrap
		o.NewModel = factory
	})
	s := m.Registry().Create("张三", "委派", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "派一个", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if atomic.LoadInt32(&subCalls) != 1 {
		t.Fatalf("子代理应执行探针一次，实得 %d", subCalls)
	}
	if gate.seenOf("sub_probe") != 1 || gate.callsOf("sub_probe") != 1 {
		t.Fatalf("子面应过包装：seen=%d calls=%d",
			gate.seenOf("sub_probe"), gate.callsOf("sub_probe"))
	}
}

// TestToolWrapDenyAtReplayTime 重放时刻重算（单调收紧的时间语义）：批准发生
// 在允许窗内、重放落在拒绝窗——决议不得豁免包装门禁，deny 信封回喂、轮次
// 正常收口。
func TestToolWrapDenyAtReplayTime(t *testing.T) {
	gate := newWrapGate()
	var calls int32
	wt := gateWriteTool(t, "write_tool", &calls)
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{tcOf("c1", "write_tool", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{wt} }
		o.Approval = hitl.ApprovalConfig{WriteTools: map[string]bool{"write_tool": true}}
		o.ToolWrap = gate.wrap
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "写", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var card *contract.ApprovalReq
	m.Run(context.Background(), s, "写一个", nil, func(ev session.Event) {
		if ev.Event == contract.EvApprovalRequest {
			req := ev.Data.(contract.ApprovalReq)
			card = &req
		}
	})
	t.Cleanup(func() { stopApprovalTimer(s.SID) })
	if card == nil || len(card.Items) == 0 {
		t.Fatal("应先挂起审批")
	}
	gate.deny.Store(true) // 批准前翻进门禁拒绝窗（重放时刻重算）
	for _, it := range card.Items {
		s.SetDecisionFor(it.ItemID, contract.ApprovalDecision{Approve: true})
	}
	m.Resume(context.Background(), s, func(session.Event) {})
	waitTitleFlight(t, s)
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("已批准但落在拒绝窗：不得执行（单调收紧）")
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("deny 回喂后应正常收口，终态 %s", s.StateOf())
	}
	seenDeny := false
	for _, msg := range s.CloneHistory() {
		if msg.Role == schema.Tool && strings.Contains(msg.Content, "策略拒绝") {
			seenDeny = true
		}
	}
	if !seenDeny {
		t.Fatal("重放拒绝应信封入史")
	}
}

// goErrTool 违约包装样板：Invoke 返回 Go error（义务 2 的反面）。
type goErrTool struct{ t contract.Tool }

func (w *goErrTool) Info() *contract.ToolInfo { return w.t.Info() }

func (w *goErrTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("包装违约：Go error 而非信封")
}

// TestToolWrapGoErrorTerminatesRound 义务 2 反例钉板：本缝在 errFeed 外层，
// 包装返回 Go error 不再被转信封——整轮以 error 事件终止（模型不可见、不
// 可自纠）。这是「拒绝必须走 {"ok":false} 信封」契约的行为依据。
func TestToolWrapGoErrorTerminatesRound(t *testing.T) {
	var calls int32
	wt := gateWriteTool(t, "write_tool", &calls)
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{tcOf("c1", "write_tool", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{wt} }
		o.ToolWrap = func(t contract.Tool) contract.Tool { return &goErrTool{t: t} }
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "写", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var names []string
	m.Run(context.Background(), s, "写一个", nil, func(ev session.Event) { names = append(names, ev.Event) })
	waitTitleFlight(t, s)
	if !contains(names, contract.EvError) {
		t.Fatalf("包装 Go error 应终止整轮（error 事件）：%v", names)
	}
	if s.StateOf() != session.StateError {
		t.Fatalf("Go error 终止后应为 error 终态，实得 %s", s.StateOf())
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("违约包装不得透传执行")
	}
	for _, msg := range s.CloneHistory() { // 无信封回喂——模型侧不可见
		if msg.Role == schema.Tool && strings.Contains(msg.Content, "Go error 而非信封") {
			t.Fatal("Go error 不应转信封入史（errFeed 在内层管不到本缝）")
		}
	}
}
