package engine

// Run 级引擎回归（自产品 agent_test 的两个用例迁入——产品侧因 llmtest 无
// 对应旋钮/协议形态而无法直译者，在基座侧用本地假模型覆盖）：
// ① 审批挂起超时自动拒绝（approvalTimeout 基座内旋钮）
// ② 流式 tool call 分片按 Index 归并入史（OpenAI 协议——llmtest 分片修复
//   后同语义，此处直接验引擎归并）。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/checkpoint"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/internal/tstore"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// scriptedModel 本地剧本假模型（engine 包内测试可用 eino 类型）。
type scriptedModel struct {
	mu     sync.Mutex
	inputs [][]*schema.Message
	// onStream 第 n 次调用（n 从 1 起）的写出闭包（nil = 纯文本）。
	onStream func(n int, send func(*schema.Message))
}

func (f *scriptedModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	return schema.AssistantMessage("非流式答复", nil), nil
}

func (f *scriptedModel) Stream(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, append([]*schema.Message(nil), input...))
	n := len(f.inputs)
	f.mu.Unlock()
	sr, sw := schema.Pipe[*schema.Message](8)
	go func() {
		defer sw.Close()
		if f.onStream == nil {
			sw.Send(&schema.Message{Role: schema.Assistant, Content: "已处理。"}, nil)
			return
		}
		f.onStream(n, func(m *schema.Message) { sw.Send(m, nil) })
	}()
	return sr, nil
}

// newRunManager 构造带单工具面的测试引擎（写审批名单含 write_tool）。
func newRunManager(t *testing.T, ts []contract.Tool, fm llm.ModelFactory) (*Manager, *tstore.Store) {
	t.Helper()
	st := tstore.New(t.TempDir())
	return newRunManagerOn(t, ts, fm, st), st
}

// newRunManagerOn 自带 store 的引擎装配（ghost 回归用慢 store 包装注入写延迟）。
func newRunManagerOn(t *testing.T, ts []contract.Tool, fm llm.ModelFactory, st session.Store) *Manager {
	t.Helper()
	reg := session.NewRegistry(st)
	m := NewManager(reg, Options{
		Providers: func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{{ID: "m", Input: []string{"text"}, Priority: 100}},
			}}
		},
		Instruction: func(SessionBrief) string { return "test" },
		Tools:       func() []contract.Tool { return ts },
		NewModel:    fm,
		CheckPoints: func(operator, sid string) CheckPointStore {
			return checkpoint.NewCheckPointStore(st, operator, sid)
		},
		Approval:      hitl.ApprovalConfig{WriteTools: map[string]bool{"write_tool": true}},
		WorkspaceRoot: func(owner, sid string) string { return st.TmpDir() + "/ws/" + owner + "/" + sid },
	})
	return m
}

// waitTitleFlight 等标题在途写收尾（Run 返回 ≠ 写完——genTitle 是唯一逃逸
// Run 生命周期的写者，测试收尾前必须 join，否则与 TempDir 清理竞态）。
func waitTitleFlight(t *testing.T, s *session.Session) {
	t.Helper()
	ch := s.TitleFlight()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("标题在途写未收尾（TitleFlight 超时）")
	}
}

func TestApprovalTimeoutAutoReject(t *testing.T) {
	old := approvalTimeout
	approvalTimeout = 100 * time.Millisecond
	t.Cleanup(func() { approvalTimeout = old })

	// write_tool：记录执行次数
	calls := 0
	wt, _ := tools.InferTool("write_tool", "写桩", func(context.Context, struct{}) (map[string]any, error) {
		calls++
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "write_tool", Arguments: `{}`},
			}}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, _ := newRunManager(t, []contract.Tool{wt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	})

	s := m.Registry().Create("张三", "创建", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var names []string
	m.Run(context.Background(), s, "写一个", nil, func(ev session.Event) { names = append(names, ev.Event) })
	if !contains(names, contract.EvApprovalRequest) {
		t.Fatalf("应中断：%v", names)
	}
	// 等超时触发
	deadline := time.Now().Add(2 * time.Second)
	for s.StateOf() == session.StatePendingApproval && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("超时应自动拒绝终态：%s", s.StateOf())
	}
	if calls != 0 {
		t.Fatalf("超时不执行写工具：%d", calls)
	}
	waitTitleFlight(t, s) // 超时路径同样走 settleTurn——在途标题写收尾后再清理
	d, ok := m.Registry().Detail(s.SID, 0)
	if !ok {
		t.Fatal("详情应可读")
	}
	for _, ev := range d.Events {
		if ev.Event == contract.EvApprovalTimeout {
			return // 通过
		}
	}
	t.Fatalf("回放应含 approval_timeout：%+v", d.Events)
}

func TestToolCallFragmentsMergedIntoHistory(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			idx := 0
			frag := func(args string, head bool) schema.ToolCall {
				tc := schema.ToolCall{Index: &idx, Type: "function", Function: schema.FunctionCall{Arguments: args}}
				if head {
					tc.ID = "c-frag-1"
					tc.Function.Name = "read_tool"
				}
				return tc
			}
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{frag(`{"filters":{"prior`, true)}})
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{frag(`ity":"P0"}}`, false)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, _ := newRunManager(t, []contract.Tool{rt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	})

	s := m.Registry().Create("张三", "查询", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "查 P0", nil, func(session.Event) {})
	waitTitleFlight(t, s) // Run 后仍有标题在途写——join 后 TempDir 清理才无竞态

	var tc *schema.ToolCall
	for _, msg := range s.CloneHistory() {
		for i := range msg.ToolCalls {
			tc = &msg.ToolCalls[i]
		}
	}
	if tc == nil {
		t.Fatal("历史应含 tool call")
	}
	if tc.ID != "c-frag-1" || tc.Function.Name != "read_tool" {
		t.Fatalf("tool call 标识不符：%+v", tc)
	}
	if want := `{"filters":{"priority":"P0"}}`; tc.Function.Arguments != want {
		t.Fatalf("arguments 应分片拼装完整，实得 %q（期望 %q）", tc.Function.Arguments, want)
	}
}

// slowStore 写路径注入延迟的测试存储（确定性制造在途写窗口）。
type slowStore struct {
	*tstore.Store
	d time.Duration
}

func (s slowStore) WriteUserTreeFile(op, rel string, data []byte) error {
	time.Sleep(s.d)
	return s.Store.WriteUserTreeFile(op, rel, data)
}

// TestTitleFlightDeleteNoGhost 标题在途写与删除竞态不复活目录：Delete 落在
// genTitle 的 persist 写中途（延迟放大窗口），写落地后 persist 写后复查
// stopped 自愈收回——目录零残留（2026-08-25 引擎侧修复回归）。
func TestTitleFlightDeleteNoGhost(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	st := slowStore{Store: tstore.New(t.TempDir()), d: 100 * time.Millisecond}
	m := newRunManagerOn(t, []contract.Tool{rt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	}, st)
	s := m.Registry().Create("张三", "查询", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "查", nil, func(session.Event) {})
	ch := s.TitleFlight()
	if ch == nil {
		t.Fatal("首轮收尾应有标题在途信号")
	}
	time.Sleep(20 * time.Millisecond) // 让 genTitle 越过 persist 入口检查、进入延迟写
	m.Registry().Delete("张三", s.SID)  // 删除落在写在途——无自愈则此写复活目录
	waitTitleFlight(t, s)
	if s.TitleOf() == "" {
		t.Fatal("标题应已生成（验证路径非静默跳过）")
	}
	for _, sid := range st.ListUserTreeSessions("张三") {
		if sid == s.SID {
			t.Fatal("删除后被在途写复活（ghost 目录残留）")
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
