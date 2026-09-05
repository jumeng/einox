package engine

// 录制会话快照测试（吸收设计 §4.2-2，dsh 门禁形态的 Go 对位）：关键场景的
// 事件流 golden 落盘（testdata/golden/*.jsonl），行为漂移即红——docs/03 事件
// 软契约的可执行证据，回放即回归。
//
// 归一化纪律：事件 ID 留序（序即行为）；Ts 不序列化（时刻非行为）；会话
// SID/审批与排队等随机 hex ID/临时目录占位替换；内容确定性由 scriptedModel
// 剧本保证。维护：go test ./engine -run TestGolden -update（行为有意变更后
// 重录，评审天然可见 diff）。

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

var goldenUpdate = flag.Bool("update", false, "重写 golden 事件流基线")

// goldenLine 归一后的单事件行。
type goldenLine struct {
	ID    int    `json:"id"`
	Event string `json:"event"`
	Data  any    `json:"data"`
}

var goldenHexID = regexp.MustCompile(`[0-9a-f]{16,}`)

// goldenKeyNorm 按键定向归一：随机短 ID（approval/item/ask/plan 各域构造器
// 是字母前缀 + 短 hex，形状太短不够通用 hex 规则捕获）与墙钟时刻（超时面
// 的 RFC3339）——键清单随场景增长在此扩，不猜值形状。
var goldenKeyNorm = regexp.MustCompile(`("(?:approval_id|item_id|ask_id|plan_id|timeout_at)"):("[^"]*")`)

// goldenNormalize 事件流 → 归一化 JSON Lines（SID→SID、随机 ID→RID、临时目录→TMP）。
func goldenNormalize(t *testing.T, s *session.Session, tmp string) string {
	t.Helper()
	var b strings.Builder
	for _, e := range s.SnapshotEvents() {
		j, err := json.Marshal(goldenLine{ID: e.ID, Event: e.Event, Data: e.Data})
		if err != nil {
			t.Fatalf("golden 序列化失败（%s 载荷含不可序列化面）：%v", e.Event, err)
		}
		line := string(j)
		line = strings.ReplaceAll(line, s.SID, "SID")
		if tmp != "" {
			line = strings.ReplaceAll(line, tmp, "TMP")
		}
		line = goldenKeyNorm.ReplaceAllString(line, `$1:"RID"`)
		line = goldenHexID.ReplaceAllString(line, "RID")
		b.WriteString(line + "\n")
	}
	return b.String()
}

// checkGolden 比对/重写基线（首处差异定位到行）。
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".jsonl")
	if *goldenUpdate {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden 缺失（先 go test ./engine -run TestGolden -update 生成）：%v", err)
	}
	if string(want) == got {
		return
	}
	wl, gl := strings.Split(string(want), "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			t.Fatalf("golden 漂移 [%s] 第 %d 行：\n  want: %s\n   got: %s\n（行为有意变更则 -update 重录，diff 进评审）", name, i+1, w, g)
		}
	}
}

// TestGoldenCompaction 压缩触发：阈上会话 Run → 摘要压缩 + 通知卡 + transcript
// 外置 + 摘要视图续跑（口径数字随内容确定性稳定）。
func TestGoldenCompaction(t *testing.T) {
	parent := &scriptedModel{}
	sub := &recGenModel{reply: "压缩摘要文本"}
	n := 0
	m, st := newReductionManager(t, 20000, nil, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 { // 构造序：主模型(1) → 摘要模型(2)
			return sub, nil
		}
		return parent, nil
	})
	s := m.Registry().Create("张三", "压缩", "plan", contract.UserPrefs{Model: "p/m"})
	s.AppendHistory(sumHist(6, 15000)...)
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "继续推进", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	checkGolden(t, "compaction", goldenNormalize(t, s, st.TmpDir()))
}

// TestGoldenApprovalSuspendResume 审批挂起-恢复：manual 档写工具中断 → 聚合
// 审批卡 → 批准 → Resume 续流执行 → 正常收束。
func TestGoldenApprovalSuspendResume(t *testing.T) {
	wt, _ := tools.InferTool("write_tool", "写桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("cw", "write_tool", `{}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m, _ := newRunManager(t, []contract.Tool{wt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	})
	s := m.Registry().Create("张三", "审批", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var card *contract.ApprovalReq
	m.Run(context.Background(), s, "写文件", nil, func(ev session.Event) {
		if ev.Event == contract.EvApprovalRequest {
			req := ev.Data.(contract.ApprovalReq)
			card = &req
		}
	})
	t.Cleanup(func() { stopApprovalTimer(s.SID) })
	if card == nil {
		t.Fatal("manual 档写工具应中断挂起发审批卡")
	}
	for _, it := range card.Items {
		s.SetDecisionFor(it.ItemID, contract.ApprovalDecision{Approve: true})
	}
	m.Resume(context.Background(), s, func(session.Event) {})
	waitTitleFlight(t, s)
	checkGolden(t, "approval", goldenNormalize(t, s, ""))
}

// TestGoldenSubagentSpawn 子代理派生：父派 spawn → 子独立上下文执行 → 结论经
// tool 结果内联父窗口 → 父收口。
func TestGoldenSubagentSpawn(t *testing.T) {
	rt, _ := tools.InferTool("read_tool", "读桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c1", "spawn", `{"task":"勘察并统计","tools":"read_tool","expect":"数量"}`)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	sub := &recGenModel{reply: "子代理结论：仓库共 3 文件"}
	n := 0
	m, _ := newReductionManager(t, 0, []contract.Tool{rt}, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		n++
		if n == 2 {
			return sub, nil
		}
		return fm, nil
	}, func(o *Options) {
		o.SubAgents = &SubAgentsConfig{Tools: []string{"read_tool"}}
	})
	s := m.Registry().Create("张三", "派子", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "帮我勘察", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	checkGolden(t, "subagent", goldenNormalize(t, s, ""))
}

// TestGoldenFinalGateRefeed 质量门回灌：首验失败 → 门卡 + 反馈入史重跑 → 次
// 验过 → 正常收束。
func TestGoldenFinalGateRefeed(t *testing.T) {
	calls := 0
	m := newSeamManager(t, func(o *Options) {
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return &scriptedModel{}, nil
		}
		o.FinalGate = func(SessionBrief) *GateConfig {
			return &GateConfig{MaxRetries: 1, Checkers: []GateChecker{
				func(context.Context, string) error {
					calls++
					if calls == 1 {
						return fmt.Errorf("lint 未过：残留调试打印")
					}
					return nil
				}}}
		}
	})
	s := m.Registry().Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "做", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	checkGolden(t, "gate", goldenNormalize(t, s, ""))
}
