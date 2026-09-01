package engine

// channel 机制核回归（docs/04 渠道接入）：入站分流（空闲起轮/运行中排队/
// 挂起决议续流）、常驻订阅出站（挂起期可达——live 回调绑定 Run 生命周期的
// 既有缺口）、慢消费水位补投、绑定持久化（重启经 Reattach 找回）、双渠道
// 隔离、Push/Cancel、构造期校验。模型走本地剧本假模型，不碰真实端点。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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

// chanSink 测试渠道出站：全量收集（可 hold 模拟慢消费——投递阻塞，触发
// 订阅通道满即弃的既有语义，验证水位补投）。
type chanSink struct {
	mu   sync.Mutex
	got  []session.Event
	hold chan struct{} // nil = 直收；非 nil 未关 = 每次投递阻塞，close 放行
}

func (s *chanSink) Deliver(_ ChannelBrief, ev session.Event) {
	if s.hold != nil {
		<-s.hold
	}
	s.mu.Lock()
	s.got = append(s.got, ev)
	s.mu.Unlock()
}

func (s *chanSink) events() []session.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]session.Event(nil), s.got...)
}

func (s *chanSink) has(name string) bool {
	for _, ev := range s.events() {
		if ev.Event == name {
			return true
		}
	}
	return false
}

// chanWaitFor 轮询等待（渠道面全异步——起轮/投递/续流各自 goroutine）。
// desc 惰性求值：失败时刻的诊断快照。
func chanWaitFor(t *testing.T, cond func() bool, desc func() string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(desc())
}

// channelSetup 渠道测试装配：写审批引擎 + 剧本模型 + 渠道清单。
func channelSetup(t *testing.T, st session.Store, fm *scriptedModel, chans ...ChannelConfig) (*Manager, *ChannelGateway, *int32) {
	t.Helper()
	var calls int32
	wt, err := tools.InferTool("write_tool", "写", func(context.Context, struct{}) (map[string]any, error) {
		atomic.AddInt32(&calls, 1)
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("InferTool: %v", err)
	}
	m, err := NewManager(session.NewRegistry(st), Options{
		Providers: func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{{ID: "m", Input: []string{"text"}, Priority: 100}},
			}}
		},
		Instruction: func(SessionBrief) string { return "test" },
		Tools:       func(SessionBrief) []contract.Tool { return []contract.Tool{wt} },
		NewModel: func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		},
		CheckPoints: func(operator, sid string) CheckPointStore {
			return checkpoint.NewCheckPointStore(st, operator, sid)
		},
		Approval:      hitl.ApprovalConfig{WriteTools: map[string]bool{"write_tool": true}},
		WorkspaceRoot: func(owner, sid string) string { return st.TmpDir() + "/ws/" + owner + "/" + sid },
		Channels:      chans,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, m.Channels(), &calls
}

// chanSessionOf 绑定会话取回（收尾 join 标题在途写）。
func chanSessionOf(t *testing.T, gw *ChannelGateway, channel, chat string) *session.Session {
	t.Helper()
	brief, ok := gw.Lookup(channel, chat)
	if !ok {
		t.Fatalf("绑定应存在（%s/%s）", channel, chat)
	}
	s, ok := gw.m.reg.Get(brief.SID)
	if !ok {
		t.Fatalf("会话应可达：%s", brief.SID)
	}
	return s
}

// TestChannelInboundStartsRun 入站起轮：空闲消息建会话跑完，出站全链到达
// （user_message → … → session_end），终态 ended。
func TestChannelInboundStartsRun(t *testing.T) {
	sink := &chanSink{}
	m, gw, _ := channelSetup(t, tstore.New(t.TempDir()), &scriptedModel{},
		ChannelConfig{ID: "c1", Sink: sink, Model: "p/m"})

	if err := gw.Handle(InboundMsg{Channel: "c1", Chat: "g1", Owner: "张三", Text: "你好"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	diag := func() string {
		names := make([]string, 0, len(sink.events()))
		for _, ev := range sink.events() {
			names = append(names, ev.Event)
		}
		if b, ok := gw.Lookup("c1", "g1"); ok {
			if s, ok2 := gw.m.reg.Get(b.SID); ok2 {
				for _, ev := range s.SnapshotEvents() {
					names = append(names, "真源:"+ev.Event)
					if ev.Event == contract.EvError {
						names = append(names, fmt.Sprintf("错误载荷:%+v", ev.Data))
					}
				}
			} else {
				names = append(names, "会话不可达")
			}
		}
		return "实收/真源事件：" + strings.Join(names, ",")
	}
	chanWaitFor(t, func() bool { return sink.has(contract.EvSessionEnd) }, diag)

	s := chanSessionOf(t, gw, "c1", "g1")
	waitTitleFlight(t, s)
	if s.StateOf() != session.StateEnded {
		t.Fatalf("应正常收束，终态 %s", s.StateOf())
	}
	names := map[string]bool{}
	for _, ev := range sink.events() {
		names[ev.Event] = true
	}
	for _, want := range []string{contract.EvUserMessage, contract.EvTextDelta, contract.EvSessionEnd} {
		if !names[want] {
			t.Fatalf("出站链缺失 %s：%v", want, sink.events())
		}
	}
	// 未注册渠道拒路由
	if err := gw.Handle(InboundMsg{Channel: "nope", Chat: "g1", Owner: "张三", Text: "x"}); err == nil {
		t.Fatal("未注册渠道应报错")
	}
	m.Channels().Close(2 * time.Second)
}

// TestChannelRunningSteerQueues 运行中入站排队：不打断执行体，steer_queued
// 出站到达；下一轮 Run 头部前置带入模型输入。
func TestChannelRunningSteerQueues(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			close(started)
			<-release // 第一轮长跑
			send(&schema.Message{Role: schema.Assistant, Content: "第一轮完成"})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "第二轮完成"})
	}}
	sink := &chanSink{}
	m, gw, _ := channelSetup(t, tstore.New(t.TempDir()), fm, ChannelConfig{ID: "c1", Sink: sink, Model: "p/m"})

	_ = gw.Handle(InboundMsg{Channel: "c1", Chat: "g1", Owner: "张三", Text: "第一问"})
	<-started
	if err := gw.Handle(InboundMsg{Channel: "c1", Chat: "g1", Owner: "张三", Text: "排队消息B"}); err != nil {
		t.Fatalf("运行中入站应排队：%v", err)
	}
	chanWaitFor(t, func() bool { return sink.has(contract.EvSteerQueued) }, func() string { return "排队回执应出站" })
	close(release)
	chanWaitFor(t, func() bool { return sink.has(contract.EvSessionEnd) }, func() string { return "第一轮应收束" })

	// 排队消息随下一轮前置注入（第二轮模型输入含 B 与新消息）
	_ = gw.Handle(InboundMsg{Channel: "c1", Chat: "g1", Owner: "张三", Text: "第二问"})
	chanWaitFor(t, func() bool {
		return len(fm.inputs) >= 2 && joinedInput(fm.inputs[1]) != ""
	}, func() string { return "第二轮模型调用应发生" })
	s := chanSessionOf(t, gw, "c1", "g1")
	chanWaitFor(t, func() bool { return s.StateOf() == session.StateEnded },
		func() string { return "第二轮应收束" })
	waitTitleFlight(t, s)
	if j := joinedInput(fm.inputs[len(fm.inputs)-1]); !strings.Contains(j, "排队消息B") || !strings.Contains(j, "第二问") {
		t.Fatalf("第二轮输入应含排队消息与新消息：%s", j)
	}
	m.Channels().Close(2 * time.Second)
}

// TestChannelSuspendDeliveredAndApprove 核心缺口验收：审批挂起、Run 已返回
// （fn 生命周期已结束）后 approval_request 仍经常驻订阅到达渠道；Approve
// 决议回写续流至收束，回执与终态事件齐达。
func TestChannelSuspendDeliveredAndApprove(t *testing.T) {
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("t1", "write_tool", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	sink := &chanSink{}
	m, gw, calls := channelSetup(t, tstore.New(t.TempDir()), fm, ChannelConfig{ID: "c1", Sink: sink, Model: "p/m"})

	_ = gw.Handle(InboundMsg{Channel: "c1", Chat: "g1", Owner: "张三", Text: "写一下"})
	s := chanSessionOf(t, gw, "c1", "g1")
	t.Cleanup(func() { stopApprovalTimer(s.SID) })
	chanWaitFor(t, func() bool { return sink.has(contract.EvApprovalRequest) }, func() string { return "挂起卡应经订阅到达渠道" })
	if s.StateOf() != session.StatePendingApproval {
		t.Fatalf("应挂起，实得 %s", s.StateOf())
	}
	var itemID string
	for _, ev := range sink.events() {
		if ev.Event == contract.EvApprovalRequest {
			req := ev.Data.(contract.ApprovalReq)
			itemID = req.Items[0].ItemID
		}
	}
	if !gw.Approve(s.SID, itemID, contract.ApprovalDecision{Approve: true}) {
		t.Fatal("决议回写应成功")
	}
	chanWaitFor(t, func() bool { return sink.has(contract.EvSessionEnd) }, func() string {
		names := make([]string, 0, len(sink.events()))
		for _, ev := range sink.events() {
			names = append(names, fmt.Sprintf("%s#%d", ev.Event, ev.ID))
			if ev.Event == contract.EvError {
				names = append(names, fmt.Sprintf("错误:%+v", ev.Data))
			}
		}
		return fmt.Sprintf("续流后应收束（态 %s），实收：%s", s.StateOf(), strings.Join(names, ","))
	})
	waitTitleFlight(t, s)
	if *calls != 1 {
		t.Fatalf("批准后写工具应执行一次，实得 %d", *calls)
	}
	for _, want := range []string{contract.EvApprovalDecision, contract.EvSessionEnd} {
		if !sink.has(want) {
			t.Fatalf("续流出站缺失 %s", want)
		}
	}
	if s.StateOf() != session.StateEnded {
		t.Fatalf("续流应收束 ended，实得 %s", s.StateOf())
	}
	// 迟到决议幂等拒绝（挂起已消费）
	if gw.Approve(s.SID, itemID, contract.ApprovalDecision{Approve: true}) {
		t.Fatal("迟到决议应拒绝")
	}
	m.Channels().Close(2 * time.Second)
}

// TestChannelSlowSinkGapCatchUp 慢消费补投：投递阻塞触发订阅通道满即弃，
// 放行后按事件 ID 间隙从快照补投——终态链无缺失。
func TestChannelSlowSinkGapCatchUp(t *testing.T) {
	const chunks = 120 // > 订阅缓冲 64：必有丢弃
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		for i := 0; i < chunks; i++ {
			send(&schema.Message{Role: schema.Assistant, Content: fmt.Sprintf("段%03d；", i)})
		}
	}}
	sink := &chanSink{hold: make(chan struct{})}
	m, gw, _ := channelSetup(t, tstore.New(t.TempDir()), fm, ChannelConfig{ID: "c1", Sink: sink, Model: "p/m"})

	_ = gw.Handle(InboundMsg{Channel: "c1", Chat: "g1", Owner: "张三", Text: "长文本"})
	s := chanSessionOf(t, gw, "c1", "g1")
	chanWaitFor(t, func() bool { return s.StateOf() == session.StateEnded }, func() string { return "轮应跑完（事件全落真源）" })
	waitTitleFlight(t, s)
	close(sink.hold) // 放行：泵走间隙补投
	chanWaitFor(t, func() bool { return sink.has(contract.EvSessionEnd) }, func() string {
		evs := sink.events()
		names := make([]string, 0, len(evs))
		for i, ev := range evs {
			if i >= 8 {
				names = append(names, fmt.Sprintf("…共%d条", len(evs)))
				break
			}
			names = append(names, fmt.Sprintf("%s#%d", ev.Event, ev.ID))
		}
		return "补投后终态应到达，实收首段：" + strings.Join(names, ",")
	})

	got := sink.events()
	if len(got) == 0 {
		t.Fatal("放行后应有事件到达")
	}
	// ID 连续无缺（从首个到达事件到末事件）
	ids := make(map[int]bool, len(got))
	for _, ev := range got {
		ids[ev.ID] = true
	}
	first, last := got[0].ID, got[len(got)-1].ID
	for id := first; id <= last; id++ {
		if !ids[id] {
			t.Fatalf("事件 %d 缺失（应从快照补投）：首 %d 末 %d 共收 %d 条", id, first, last, len(got))
		}
	}
	m.Channels().Close(2 * time.Second)
}

// TestChannelBindingPersistsAcrossRestart 绑定持久化：同 store 重建引擎
// （模拟重启）后同渠道会话键经 Reattach 找回同一会话；Unbind 后再入站
// 新建会话。
func TestChannelBindingPersistsAcrossRestart(t *testing.T) {
	st := tstore.New(t.TempDir())
	fm := &scriptedModel{}
	m1, gw1, _ := channelSetup(t, st, fm, ChannelConfig{ID: "c1", Sink: &chanSink{}, Model: "p/m"})
	_ = gw1.Handle(InboundMsg{Channel: "c1", Chat: "g1", Owner: "张三", Text: "第一轮"})
	s1 := chanSessionOf(t, gw1, "c1", "g1")
	chanWaitFor(t, func() bool { return s1.StateOf() == session.StateEnded }, func() string { return "首轮应收束" })
	waitTitleFlight(t, s1)
	m1.Channels().Close(2 * time.Second)

	// 重启：同 store 新引擎（注册表/绑定表全部从盘恢复）
	fm2 := &scriptedModel{}
	m2, gw2, _ := channelSetup(t, st, fm2, ChannelConfig{ID: "c1", Sink: &chanSink{}, Model: "p/m"})
	defer m2.Channels().Close(2 * time.Second)
	if brief, ok := gw2.Lookup("c1", "g1"); !ok || brief.SID != s1.SID {
		t.Fatalf("重启后绑定应找回同会话：%+v ok=%v（want sid=%s）", brief, ok, s1.SID)
	}
	if err := gw2.Handle(InboundMsg{Channel: "c1", Chat: "g1", Owner: "张三", Text: "第二轮"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	s2 := chanSessionOf(t, gw2, "c1", "g1")
	if s2.SID != s1.SID {
		t.Fatalf("重启后同键应续接原会话：%s vs %s", s2.SID, s1.SID)
	}
	chanWaitFor(t, func() bool { return s2.StateOf() == session.StateEnded }, func() string { return "续聊应收束" })
	waitTitleFlight(t, s2)

	// 解绑后再入站：新建会话
	if !gw2.Unbind("c1", "g1") {
		t.Fatal("解绑应返回 true")
	}
	_ = gw2.Handle(InboundMsg{Channel: "c1", Chat: "g1", Owner: "张三", Text: "新会话"})
	s3 := chanSessionOf(t, gw2, "c1", "g1")
	if s3.SID == s1.SID {
		t.Fatal("解绑后再入站应新建会话")
	}
	chanWaitFor(t, func() bool { return s3.StateOf() == session.StateEnded },
		func() string { return "解绑后新会话应收束（Windows 清理竞态——先等收束再清 TempDir）" })
	waitTitleFlight(t, s3)
}

// TestChannelDualIsolation 双渠道隔离：同渠道会话键互不串扰（绑定复合键），
// 各自出站只投自家 Sink。
func TestChannelDualIsolation(t *testing.T) {
	sinkA, sinkB := &chanSink{}, &chanSink{}
	m, gw, _ := channelSetup(t, tstore.New(t.TempDir()), &scriptedModel{},
		ChannelConfig{ID: "ca", Sink: sinkA, Model: "p/m"}, ChannelConfig{ID: "cb", Sink: sinkB, Model: "p/m"})

	_ = gw.Handle(InboundMsg{Channel: "ca", Chat: "room", Owner: "张三", Text: "问甲"})
	_ = gw.Handle(InboundMsg{Channel: "cb", Chat: "room", Owner: "李四", Text: "问乙"})
	chanWaitFor(t, func() bool { return sinkA.has(contract.EvSessionEnd) && sinkB.has(contract.EvSessionEnd) },
		func() string { return "双渠道都应收束出站" })

	ba, _ := gw.Lookup("ca", "room")
	bb, _ := gw.Lookup("cb", "room")
	if ba.SID == bb.SID {
		t.Fatal("同键不同渠道应是不同会话（复合键隔离）")
	}
	sa := chanSessionOf(t, gw, "ca", "room")
	sb := chanSessionOf(t, gw, "cb", "room")
	waitTitleFlight(t, sa)
	waitTitleFlight(t, sb)
	for _, ev := range sinkA.events() {
		if ev.Event == contract.EvUserMessage && strings.Contains(fmt.Sprint(ev.Data), "问乙") {
			t.Fatal("渠道甲出站不应收到渠道乙的消息")
		}
	}
	m.Channels().Close(2 * time.Second)
}

// TestChannelPushAndCancel 主动推送与停止：Push 落 harness_note 经订阅出站
// （未绑定即拒）；Cancel 停运行中会话（挂起态无执行体不打断）。
func TestChannelPushAndCancel(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			close(started)
			<-release
		}
		send(&schema.Message{Role: schema.Assistant, Content: "答"})
	}}
	sink := &chanSink{}
	m, gw, _ := channelSetup(t, tstore.New(t.TempDir()), fm, ChannelConfig{ID: "c1", Sink: sink, Model: "p/m"})

	if err := gw.Push("c1", "none", "通知"); err == nil {
		t.Fatal("未绑定渠道会话应拒绝推送")
	}
	_ = gw.Handle(InboundMsg{Channel: "c1", Chat: "g1", Owner: "张三", Text: "跑"})
	<-started
	if err := gw.Push("c1", "g1", "任务提醒：正在处理"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	chanWaitFor(t, func() bool {
		for _, ev := range sink.events() {
			if ev.Event == contract.EvHarnessNote {
				if note, ok := ev.Data.(contract.HarnessNote); ok && note.Kind == "channel_push" {
					return true
				}
			}
		}
		return false
	}, func() string { return "推送应经订阅出站" })

	// Cancel 停运行轮（模型在途）→ 执行体中断收尾离开 running
	if !gw.Cancel("c1", "g1") {
		t.Fatal("运行中会话应可停止")
	}
	close(release)
	s := chanSessionOf(t, gw, "c1", "g1")
	chanWaitFor(t, func() bool { return s.StateOf() != session.StateRunning }, func() string { return "停止后应离开 running" })
	waitTitleFlight(t, s)
	// 挂起/空闲态取消不打断（false）
	if gw.Cancel("c1", "none") {
		t.Fatal("无绑定应返回 false")
	}
	m.Channels().Close(2 * time.Second)
}

// TestNewManagerRejectsBadChannels 渠道装配构造期校验：空 ID / 缺 Sink /
// 重复 ID 即拒（对齐 SessionToolsOff fail-fast 纪律）。
func TestNewManagerRejectsBadChannels(t *testing.T) {
	base := func(chans ...ChannelConfig) Options {
		return Options{
			Providers:     func() []llm.ProviderSpec { return nil },
			Instruction:   func(SessionBrief) string { return "test" },
			CheckPoints:   func(operator, sid string) CheckPointStore { return nil },
			WorkspaceRoot: func(owner, sid string) string { return t.TempDir() + "/" + sid },
			Channels:      chans,
		}
	}
	sink := &chanSink{}
	if _, err := NewManager(session.NewRegistry(tstore.New(t.TempDir())), base(ChannelConfig{ID: "", Sink: sink})); err == nil {
		t.Fatal("空 ID 应构造期报错")
	}
	if _, err := NewManager(session.NewRegistry(tstore.New(t.TempDir())), base(ChannelConfig{ID: "c1"})); err == nil {
		t.Fatal("缺 Sink 应构造期报错")
	}
	if _, err := NewManager(session.NewRegistry(tstore.New(t.TempDir())),
		base(ChannelConfig{ID: "c1", Sink: sink, Model: "p/m"}, ChannelConfig{ID: "c1", Sink: sink, Model: "p/m"})); err == nil {
		t.Fatal("重复 ID 应构造期报错")
	}
}

// joinedInput 模型输入拼串（断言用）。
func joinedInput(msgs []*schema.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}
