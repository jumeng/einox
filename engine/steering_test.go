package engine

// steering 运行中注入回归（BeforeModelRewriteState——此前零测试）：轮内到达的
// 排队消息在**下一次模型调用前**注入（当前调用链完整走完后模型自然看到，
// 不打断已执行操作）——注入位置在工具结果之后（输入末尾）、带「用户运行中
// 补充」/说话人标签，回执落流（steer_injected/notify_injected——回放不停在
// queued 态）。轮次跨 3 次模型调用（工具→工具→收口），注入窗口由工具执行体
// 承担（模型调用之间）。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// TestSteeringMidRunInjectsQueuedBeforeNextModelCall 第 1 次模型调用发起工具
// 调用后 Steer/SteerBy 排队两条（匿名 + 署名）→ 第 2 次调用输入末尾出现
// 「（用户运行中补充）」与「（<名字> 运行中补充）」消息；工具二执行期
// NotifyOwner 排入系统通知 → 第 3 次调用出现「（系统通知）」消息；三类回执
// 均落事件流。
func TestSteeringMidRunInjectsQueuedBeforeNextModelCall(t *testing.T) {
	var m *Manager
	var s *session.Session
	calls := 0
	rt, err := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		calls++
		if calls == 1 { // 第 1/2 次模型调用之间：匿名 + 署名各排一条
			if !s.Steer("优先看中文资料", nil, "") {
				t.Error("running 态 Steer 应入队")
			}
			s.SteerBy(&contract.Participant{ID: "u_wang", Name: "王五"}, "顺带核对引文", nil, "")
		} else { // 第 2/3 次模型调用之间：系统通知入队
			m.NotifyOwner(s, "[后台子代理完成] 勘察\n结论：\n共 3 文件")
		}
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("InferTool: %v", err)
	}
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n <= 2 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf(fmt.Sprintf("c%d", n), "read_tool", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, _ = newRunManager(t, []contract.Tool{rt}, factoryOf(fm))
	s = m.Registry().Create("张三", "勘察", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "查一下", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	ins := fm.inputsOf()
	if len(ins) != 3 {
		t.Fatalf("应 3 次模型调用（工具→工具→收口），实得 %d", len(ins))
	}
	// 第 2 次输入末尾：注入的运行中补充排在工具结果之后（不打断已执行操作）；
	// 两条按入队序各自成条（FIFO——匿名在前、署名在后）
	prev := ins[1][len(ins[1])-2]
	if prev.Role != schema.User || !strings.Contains(prev.Content, "（用户运行中补充）优先看中文资料") {
		t.Fatalf("第二次模型调用应含匿名注入消息（倒数第二），实得 %+v", prev)
	}
	tail := ins[1][len(ins[1])-1]
	if tail.Role != schema.User || !strings.Contains(tail.Content, "（王五 运行中补充）顺带核对引文") {
		t.Fatalf("第二次模型调用末尾应含署名注入消息（说话人标签），实得 %+v", tail)
	}
	// 第 3 次输入末尾：系统通知独立标签
	tail3 := ins[2][len(ins[2])-1]
	if tail3.Role != schema.User || !strings.Contains(tail3.Content, "（系统通知）[后台子代理完成]") {
		t.Fatalf("第三次模型调用末尾应含系统通知，实得 %+v", tail3)
	}
	// 回执落流：注入即翻态（回放不停在 queued/notify_queued 态）
	steerInj, notifyInj := 0, 0
	for _, ev := range s.SnapshotEvents() {
		switch ev.Event {
		case contract.EvSteerInjected:
			steerInj++
		case contract.EvNotifyInjected:
			notifyInj++
		}
	}
	if steerInj != 2 {
		t.Fatalf("两条排队消息均应落 steer_injected 回执，实得 %d", steerInj)
	}
	if notifyInj != 1 {
		t.Fatalf("系统通知应落 notify_injected 回执，实得 %d", notifyInj)
	}
	// 注入消费后队列清空（不滞留下一轮重复注入）
	if n := s.QueueLen(); n != 0 {
		t.Fatalf("注入后队列应清空，实得 %d", n)
	}
	// 跨轮保真（安全审查 2026-09-06）：注入消息入会话历史——下一轮
	// CloneHistory 含注入原文（此前只进 state.Messages，轮结束即失忆）
	hist := s.CloneHistory()
	var injInHist int
	for _, m := range hist {
		if m.Role == schema.User && (strings.Contains(m.Content, "（用户运行中补充）") ||
			strings.Contains(m.Content, "（王五 运行中补充）") ||
			strings.Contains(m.Content, "（系统通知）[后台子代理完成]")) {
			injInHist++
		}
	}
	if injInHist != 3 {
		t.Fatalf("三条注入消息均应入会话历史，实得 %d（hist %d 条）", injInHist, len(hist))
	}
}
