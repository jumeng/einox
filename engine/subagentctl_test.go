package engine

// C6 子代理 inbox 控制回归（设计件 findings/2026-09-21-subagent-inbox-control-design.md）：
// 重定界后真增量——渠道排队控制面（EditQueue/RemoveQueue，sid 寻址 + 所有权
// 围栏）与后台任务个体控制面（CancelSpawn 父会话域寻址 / LiveSpawns 活态发现）。
// dsh cancel 走父权威（interruptByParent 校验 parentSession）的 einox 同构 =
// 注册表按会话域隔离，寻址即权威。

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// TestGatewayQueueControlFence 渠道排队控制面：sid 寻址透传 + 围栏生效
// （署名条目他人拒）+ 无会话 false。
func TestGatewayQueueControlFence(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	m := newTestManager(t, func(o *Options) {
		o.Tools = func(SessionBrief) []contract.Tool { return []contract.Tool{rt} }
		o.Channels = []ChannelConfig{{ID: "ch", Model: "p/m", Sink: sinkNop{}}}
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	if !s.BeginRun("manual") {
		t.Fatal("抢占失败")
	}
	li := &contract.Participant{ID: "u_li", Name: "李四"}
	if !s.SteerBy(li, "李四的补充", nil, "") {
		t.Fatal("入队失败")
	}
	var id string
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvSteerQueued {
			if d, ok := ev.Data.(contract.SteerEvent); ok {
				id = d.ID
			}
		}
	}
	if id == "" {
		t.Fatal("应落 steer_queued 回执")
	}

	gw := m.Channels()
	// 他人经渠道控制面改：围栏拒
	if gw.EditQueue(s.SID, id, "u_wang", "篡改") {
		t.Fatal("渠道面他人编辑应拒（围栏在 Session 层）")
	}
	// 本人过 + 回执落流
	if !gw.EditQueue(s.SID, id, "u_li", "李四改口") {
		t.Fatal("渠道面本人编辑应过")
	}
	updated := false
	for _, ev := range s.SnapshotEvents() {
		if ev.Event == contract.EvSteerUpdated {
			if d, ok := ev.Data.(contract.SteerEvent); ok && d.ID == id && d.Text == "李四改口" {
				updated = true
			}
		}
	}
	if !updated {
		t.Fatal("编辑回执应落流")
	}
	// Owner 删除过；再删未知条目 false；未知会话 false
	if !gw.RemoveQueue(s.SID, id, "张三") {
		t.Fatal("渠道面 Owner 删除应过")
	}
	if gw.RemoveQueue(s.SID, id, "张三") {
		t.Fatal("已删条目应 false")
	}
	if gw.EditQueue("no_such_session", "q1", "", "x") || gw.RemoveQueue("no_such_session", "q1", "") {
		t.Fatal("未知会话应 false")
	}
}

// sinkNop 空投递面（控制面测试不起消费泵依赖）。
type sinkNop struct{}

func (sinkNop) Deliver(ChannelBrief, session.Event) {}

// TestCancelSpawnAndLiveSpawns 后台任务个体控制：双任务在途——取消其一，
// 另一无扰完成；LiveSpawns 在册/出册同步。
func TestCancelSpawnAndLiveSpawns(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		switch n {
		case 1:
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "spawn", `{"task":"后台一","background":true}`),
				tcOf("c2", "spawn", `{"task":"后台二","background":true}`)}})
		default:
			send(&schema.Message{Role: schema.Assistant, Content: "已派双后台"})
		}
	}}
	g := &gateGenModel{release: make(chan struct{}), reply: "结论"} // 全部子代理共享同一 subCM（构造一次）
	n := 0
	m, _ := newReductionManager(t, 0, []contract.Tool{rt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 {
			return g, nil
		}
		return fm, nil
	}, func(o *Options) {
		o.SubAgents = &SubAgentsConfig{Tools: []string{"read_tool"}, EmitEvents: true}
	})
	s := m.Registry().Create("张三", "双后台", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "派双后台", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	// 双任务在途：清单恰两枚
	waitFor(t, "双后台在册", func() bool { return len(m.LiveSpawns(s.SID)) == 2 })
	spawns := m.LiveSpawns(s.SID)
	if spawns[0] != "sp1" || spawns[1] != "sp2" {
		t.Fatalf("清单应稳定排序 sp1,sp2，实得 %v", spawns)
	}
	// 个体取消：sp1 停、sp2 无扰完成
	if !m.CancelSpawn(s.SID, "sp1") {
		t.Fatal("取消在途任务应 true")
	}
	if m.CancelSpawn(s.SID, "sp9") {
		t.Fatal("未知任务应 false")
	}
	close(g.release) // sp2 自然完成（共享闸）
	waitFor(t, "sp2 done", func() bool {
		for _, e := range subeventsOf(s) {
			if e.SpawnID == "sp2" && e.Kind == "done" && strings.Contains(e.Text, "结论") {
				return true
			}
		}
		return false
	})
	waitFor(t, "sp1 失败收尾", func() bool {
		for _, e := range subeventsOf(s) {
			if e.SpawnID == "sp1" && e.Kind == "failed" {
				return true
			}
		}
		return false
	})
	// 收尾后清单出册
	waitFor(t, "清单出册", func() bool { return len(m.LiveSpawns(s.SID)) == 0 })
	waitBgQuiescent(t, s)
}
