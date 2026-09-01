package feishu

// 假边界回归：入站事件 → Gateway 分流起轮；出站事件流 → 卡片创建/节流更新/
// 审批卡按钮；卡片回调 → 决议回写续流。SDK 不进测试（client 接口假实现），
// 引擎走 llmtest 剧本模型。

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/checkpoint"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/engine"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/internal/tstore"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/llmtest"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// fakeClient SDK 边界假实现：记录卡片收发。
type fakeClient struct {
	mu      sync.Mutex
	sent    []string // SendCard 载荷（chatID｜card）
	updates []string // UpdateCard 载荷（msgID｜card）
	nextID  int
	runErr  chan struct{} // Run 阻塞锚点
}

func (f *fakeClient) SendCard(_ context.Context, chatID string, card []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := "om_" + itoa(f.nextID)
	f.sent = append(f.sent, chatID+"|"+string(card))
	return id, nil
}

func (f *fakeClient) UpdateCard(_ context.Context, msgID string, card []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, msgID+"|"+string(card))
	return nil
}

func (f *fakeClient) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeClient) sentCards() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *fakeClient) updatesOf() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.updates...)
}

// botSetup 装配：真引擎（llmtest 剧本）+ 假 client 的 Bot。
func botSetup(t *testing.T, script ...llmtest.Turn) (*Bot, *fakeClient, *engine.Manager, *int32) {
	t.Helper()
	var calls int32
	wt, err := tools.InferTool("write_tool", "写", func(context.Context, struct{}) (map[string]any, error) {
		atomic.AddInt32(&calls, 1)
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("InferTool: %v", err)
	}
	fc := &fakeClient{runErr: make(chan struct{})}
	bot := New(Config{AppID: "cli_test", AppSecret: "x"})
	bot.cli = fc
	bot.cards.cli = fc

	st := tstore.New(t.TempDir())
	fm := llmtest.New(script...).Factory()
	m, err := engine.NewManager(session.NewRegistry(st), engine.Options{
		Providers: func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{{ID: "m", Input: []string{"text"}, Priority: 100}},
			}}
		},
		Instruction: func(engine.SessionBrief) string { return "test" },
		Tools:       func(engine.SessionBrief) []contract.Tool { return []contract.Tool{wt} },
		NewModel: func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm(context.Background(), llm.ProviderSpec{}, llm.ModelSpec{}, "")
		},
		CheckPoints: func(operator, sid string) engine.CheckPointStore {
			return checkpoint.NewCheckPointStore(st, operator, sid)
		},
		Approval:      hitl.ApprovalConfig{WriteTools: map[string]bool{"write_tool": true}},
		WorkspaceRoot: func(owner, sid string) string { return st.TmpDir() + "/ws/" + owner + "/" + sid },
		Channels:      []engine.ChannelConfig{{ID: bot.ID(), Model: "p/m", Sink: bot}},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	bot.gw = m.Channels()
	return bot, fc, m, &calls
}

func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestInboundRunsAndRendersCards 入站起轮 + 出站卡片链：文本消息建卡、
// 增量节流更新、终稿定格。
func TestInboundRunsAndRendersCards(t *testing.T) {
	bot, fc, m, _ := botSetup(t, llmtest.Turn{Text: "答复内容"})
	defer bot.Close()

	bot.handleMsg(inboundMsg{chatID: "oc_1", openID: "ou_zhang", chatType: "p2p", text: "你好"})
	waitUntil(t, func() bool {
		for _, c := range fc.sentCards() {
			if strings.Contains(c, "答复内容") {
				return true
			}
		}
		return false
	}, "终稿卡应含答复文本")

	if brief, ok := bot.gw.Lookup(bot.ID(), "oc_1"); !ok || brief.Owner != "ou_zhang" {
		t.Fatalf("绑定应记录发送者归属：%+v ok=%v", brief, ok)
	}
	// 未注册渠道消息（路由键错配）→ 回执告警卡
	b2 := New(Config{ID: "other"})
	b2.cards.cli = fc
	b2.gw = m.Channels()
	b2.handleMsg(inboundMsg{chatID: "oc_1", openID: "ou_zhang", text: "x"})
	waitUntil(t, func() bool {
		for _, c := range fc.sentCards() {
			if strings.Contains(c, "未注册的渠道") {
				return true
			}
		}
		return false
	}, "分流失败应回执告警卡")
}

// TestApprovalCardFlow 审批闭环：挂起 → 审批卡（按钮 value 带路由键）→ 批准
// 回调 → 续流收束 + 挂起卡定格。
func TestApprovalCardFlow(t *testing.T) {
	bot, fc, _, calls := botSetup(t,
		llmtest.Turn{ToolCalls: []llmtest.ToolCallSpec{{ID: "t1", Name: "write_tool", Args: "{}"}}},
		llmtest.Turn{Text: "完成"},
	)
	defer bot.Close()

	bot.handleMsg(inboundMsg{chatID: "oc_1", openID: "ou_zhang", text: "写一下"})
	var approvalCard string
	waitUntil(t, func() bool {
		for _, c := range fc.sentCards() {
			if strings.Contains(c, "审批请求") && strings.Contains(c, "\"act\":\"approve\"") {
				approvalCard = c
				return true
			}
		}
		return false
	}, "挂起应发审批卡（带按钮）")
	if !strings.Contains(approvalCard, "\"sid\":\"s") {
		t.Fatalf("按钮 value 应带会话路由键：%s", approvalCard)
	}
	sid := extractJSONString(approvalCard, "sid")

	bot.handleAction(cardAction{value: map[string]any{"act": "approve", "sid": sid}})
	waitUntil(t, func() bool { return atomic.LoadInt32(calls) == 1 }, "批准后写工具应执行")
	// 挂起卡定格为已答复
	waitUntil(t, func() bool {
		for _, c := range fc.updatesOf() {
			if strings.Contains(c, "已收到答复") {
				return true
			}
		}
		return false
	}, "决议后挂起卡应定格")
}

// TestActionCardRender 卡片渲染函数级：按钮 value 约定（回调路由键）。
func TestActionCardRender(t *testing.T) {
	card := string(actionCard("标题", "说明", "s123", []actionBtn{
		{Label: "批准", Type: "primary", Act: "approve"},
		{Label: "选项A", Type: "default", Act: "answer", Value: "A"},
	}))
	for _, want := range []string{`"act":"approve"`, `"sid":"s123"`, `"val":"A"`, `"plain_text"`, `"button"`} {
		if !strings.Contains(card, want) {
			t.Fatalf("卡片缺 %s：%s", want, card)
		}
	}
}

// extractJSONString 从卡片 JSON 提取指定字符串键值（测试断言辅助）。
func extractJSONString(card, key string) string {
	idx := strings.Index(card, `"`+key+`":"`)
	if idx < 0 {
		return ""
	}
	rest := card[idx+len(key)+4:]
	if end := strings.Index(rest, `"`); end >= 0 {
		return rest[:end]
	}
	return ""
}
