package engine

// 记忆三通道回归（findings/2026-08-29-memory-three-channel-design.md）：
// 拉 recall——引擎级（检索命中/owner 隔离/排除自身/列表/装配开关）+ 单元级
// （投影边界/digest 渲染/截断续读/缺页）；写 TurnEpilogue——自然收束触
// 发、错误轮不触发、panic 兜底。推通道由 agentsmd_test.go 覆盖。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// seedPersisted 落一个持久会话（不跑轮——直接构造历史并 Persist）。
func seedPersisted(t *testing.T, m *Manager, owner, task, title string, msgs ...*schema.Message) *session.Session {
	t.Helper()
	s := m.Registry().Create(owner, task, "auto", contract.UserPrefs{Model: "p/m"})
	if title != "" {
		s.SetTitle(title)
	}
	for _, msg := range msgs {
		s.AppendHistory(msg)
	}
	m.Registry().Persist(s)
	return s
}

// recallTurn 起一轮：模型首调 recall（args）→ 收工具结果 → 文本收口；
// 返回模型第二调的 tool 消息内容（recall 信封）。
func recallTurn(t *testing.T, m *Manager, userMsg, args string) (string, *scriptedModel) {
	t.Helper()
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("rc1", "recall", args)}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m.Opt.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	}
	s := m.Registry().Create("张三", userMsg, "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, userMsg, nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if s.StateOf() != session.StateEnded {
		t.Fatalf("应正常收线：%s", s.StateOf())
	}
	return strings.Join(toolMsgOf(fm.inputs[len(fm.inputs)-1]), "\n"), fm
}

// TestRecallSearchHitIsolation 检索模式：消息投影命中、owner 隔离（李四
// 不可见）、未命中会话不出现。
func TestRecallSearchHitIsolation(t *testing.T) {
	m := newSeamManager(t, func(o *Options) { o.Recall = true })
	seedPersisted(t, m, "张三", "部署一个服务", "部署脚本复盘",
		schema.UserMessage("帮我写部署脚本 deploy.sh"),
		schema.AssistantMessage("已生成部署脚本", nil))
	seedPersisted(t, m, "张三", "别的主题", "周报整理", schema.UserMessage("写周报"))
	seedPersisted(t, m, "李四", "部署关键词持有者", "李四的部署会话MARKER_L4",
		schema.UserMessage("部署部署部署"))

	joined, _ := recallTurn(t, m, "查历史", `{"query":"deploy.sh"}`)
	if !strings.Contains(joined, "部署脚本复盘") {
		t.Fatalf("应命中部署会话（消息投影）：%s", joined)
	}
	if strings.Contains(joined, "MARKER_L4") {
		t.Fatalf("owner 隔离失效（李四会话可见）：%s", joined)
	}
	if strings.Contains(joined, "周报整理") {
		t.Fatalf("未命中会话不应出现在结果：%s", joined)
	}
}

// TestRecallListModeExcludeSelf 列表模式（query 空）：返回历史会话、排除自身。
func TestRecallListModeExcludeSelf(t *testing.T) {
	m := newSeamManager(t, func(o *Options) { o.Recall = true })
	seedPersisted(t, m, "张三", "旧任务A", "旧会话甲")
	seedPersisted(t, m, "张三", "旧任务B", "旧会话乙")

	joined, _ := recallTurn(t, m, "当前SELFMARK", `{}`)
	if !strings.Contains(joined, "旧会话甲") || !strings.Contains(joined, "旧会话乙") {
		t.Fatalf("列表应含两个历史会话：%s", joined)
	}
	if strings.Contains(joined, "SELFMARK") {
		t.Fatalf("当前会话应被排除：%s", joined)
	}
}

// TestRecallAssemblyToggle opt-in 开关：默认不装配——recall 调用走幻觉兜底
// 信封（「不存在」，双重验证 #1 兜底）；开后在场由检索/列表用例引擎级证明。
func TestRecallAssemblyToggle(t *testing.T) {
	m := newSeamManager(t, nil) // 默认关
	seedPersisted(t, m, "张三", "旧任务A", "旧会话甲")
	joined, _ := recallTurn(t, m, "问", `{"query":"旧会话"}`)
	if !strings.Contains(joined, "不存在") {
		t.Fatalf("默认不装配：recall 调用应走幻觉兜底信封：%s", joined)
	}
}

// TestRecallMatchProjection 投影边界：大小写不敏感命中；thinking 不入投影；
// 工具名入投影。
func TestRecallMatchProjection(t *testing.T) {
	d := &session.PersistedDigest{
		Title: "会话X", Task: "任务", Summary: "总结",
		Messages: []*schema.Message{
			schema.UserMessage("帮我处理 Deploy 事宜"),
			{Role: schema.Assistant, Content: "", ReasoningContent: "deploy 思考中",
				ToolCalls: []schema.ToolCall{tcOf("t1", "run_command", `{}`)}},
			{Role: schema.Assistant, Content: "已完成 deploy"},
		},
	}
	if f := recallMatch(d, "deploy"); len(f) == 0 {
		t.Fatal("大小写不敏感应命中")
	}
	only := &session.PersistedDigest{Title: "T", Messages: []*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "zzq_secret_thinking"},
	}}
	if f := recallMatch(only, "zzq_secret"); f != nil {
		t.Fatalf("thinking 不入投影：%v", f)
	}
	if f := recallMatch(d, "run_command"); len(f) != 1 || f[0] != "tools" {
		t.Fatalf("工具名应命中 tools 投影：%v", f)
	}
	// 空白折叠：查询与文本内的空白串（含换行/多空格）折叠为单空格后匹配
	fold := &session.PersistedDigest{Title: "T", Messages: []*schema.Message{
		schema.UserMessage("Deploy\n  script\n已生成"),
	}}
	if f := recallMatch(fold, "deploy script"); len(f) == 0 {
		t.Fatal("空白折叠应命中")
	}
}

// TestRecallDeepReadTruncateOffset 深读：digest 逐轮结构与工具名、截断标记、
// offset 续读、缺页报错、owner 隔离。
func TestRecallDeepReadTruncateOffset(t *testing.T) {
	m := newSeamManager(t, func(o *Options) { o.Recall = true })
	s := m.Registry().Create("张三", "深读任务", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetTitle("深读标题")
	// 多条 500 字消息（单条截 500）：总 digest 撑过 4000 字截断阈值
	for i := 0; i < 12; i++ {
		s.AppendHistory(schema.UserMessage(strings.Repeat("长内容。", 100)))
		s.AppendHistory(schema.AssistantMessage("第一段回答", []schema.ToolCall{
			tcOf("t1", "read_file", `{}`), tcOf("t2", "run_command", `{}`)}))
	}
	s.AppendHistory(schema.UserMessage("第二问"))
	s.AppendHistory(schema.AssistantMessage("第二答", nil))
	m.Registry().Persist(s)
	st := m.Registry().Store()

	out, err := recallDeepRead(st, "张三", s.SID, 0)
	if err != nil {
		t.Fatalf("深读应成功：%v", err)
	}
	sess := out.(map[string]any)["session"].(map[string]any)
	digest := sess["digest"].(string)
	if !strings.Contains(digest, "[用户]") || !strings.Contains(digest, "[助手]") ||
		!strings.Contains(digest, "[工具] read_file,run_command") {
		t.Fatalf("digest 应含逐轮结构与工具名：%s", head(digest, 200))
	}
	if sess["truncated"] != true {
		t.Fatal("超长 digest 应标记 truncated")
	}
	out2, _ := recallDeepRead(st, "张三", s.SID, 4000)
	if d2 := out2.(map[string]any)["session"].(map[string]any)["digest"].(string); d2 == digest {
		t.Fatal("offset 应推进内容")
	}
	if _, err := recallDeepRead(st, "张三", "不存在", 0); err == nil {
		t.Fatal("缺页应报错")
	}
	if _, err := recallDeepRead(st, "李四", s.SID, 0); err == nil {
		t.Fatal("跨 owner 深读应不可达")
	}
}

func head(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// TestRecallLimitBounds limit 越界拒绝（>20 / 负数），边界值 20 放行。
func TestRecallLimitBounds(t *testing.T) {
	m := newSeamManager(t, func(o *Options) { o.Recall = true })
	s := m.Registry().Create("张三", "当前", "auto", contract.UserPrefs{Model: "p/m"})
	rt, err := newRecallTool(m.Registry(), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"limit":21}`, `{"limit":-1}`} {
		if _, err := rt.Invoke(context.Background(), json.RawMessage(bad)); err == nil {
			t.Fatalf("limit %s 应拒绝", bad)
		}
	}
	if _, err := rt.Invoke(context.Background(), json.RawMessage(`{"limit":20}`)); err != nil {
		t.Fatalf("limit 20 应放行：%v", err)
	}
}

// TestRecallHitCountSortPriority 命中度优先于新近：先落盘（较老）的双字段
// 命中会话排在后落盘（较新）的单字段命中会话之前。
func TestRecallHitCountSortPriority(t *testing.T) {
	m := newSeamManager(t, func(o *Options) { o.Recall = true })
	seedPersisted(t, m, "张三", "部署KEY老任务", "老双KEY") // Task+Title 双命中（较老）
	seedPersisted(t, m, "张三", "别的主题", "新单KEY")      // 仅 Title 单命中（较新）
	joined, _ := recallTurn(t, m, "查", `{"query":"KEY"}`)
	o, n := strings.Index(joined, "老双KEY"), strings.Index(joined, "新单KEY")
	if o < 0 || n < 0 {
		t.Fatalf("两会话都应命中：%s", joined)
	}
	if o > n {
		t.Fatalf("双命中应排在单命中前（命中度优先于新近）：%s", joined)
	}
}

// TestTurnEpilogueFiresOnNaturalEnd 写通道：自然收束触发且载荷同源。
func TestTurnEpilogueFiresOnNaturalEnd(t *testing.T) {
	var mu sync.Mutex
	var got []TurnEndSummary
	m := newSeamManager(t, func(o *Options) {
		o.TurnEpilogue = func(sum TurnEndSummary) {
			mu.Lock()
			got = append(got, sum)
			mu.Unlock()
		}
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
				send(&schema.Message{Role: schema.Assistant, Content: "本轮结论"})
			}}, nil
		}
	})
	s := m.Registry().Create("张三", "交接任务", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "问", nil, func(session.Event) {})
	waitTitleFlight(t, s)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("自然收束应恰触发一次，实得 %d", len(got))
	}
	g := got[0]
	if g.Owner != "张三" || g.SID != s.SID || g.Task != "交接任务" || g.Summary != "本轮结论" {
		t.Fatalf("载荷应与 session_end 同源：%+v", g)
	}
}

// TestTurnEpilogueNotOnErrorAndPanicSafe 错误轮不触发；钩子 panic 不拖垮收尾。
func TestTurnEpilogueNotOnErrorAndPanicSafe(t *testing.T) {
	var fired int
	var mu sync.Mutex
	newM := func(hook func(TurnEndSummary), fm llm.ModelFactory) *Manager {
		return newSeamManager(t, func(o *Options) {
			o.TurnEpilogue = func(sum TurnEndSummary) {
				mu.Lock()
				fired++
				mu.Unlock()
				if hook != nil {
					hook(sum)
				}
			}
			o.NewModel = fm
		})
	}
	m1 := newM(nil, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return &brokenModel{err: errors.New("硬错")}, nil
	})
	s1 := m1.Registry().Create("张三", "会失败", "auto", contract.UserPrefs{Model: "p/m"})
	s1.SetState(session.StateRunning)
	m1.Run(context.Background(), s1, "问", nil, func(session.Event) {})
	waitTitleFlight(t, s1)
	mu.Lock()
	if fired != 0 {
		mu.Unlock()
		t.Fatalf("错误轮不应触发 epilogue（实得 %d 次）", fired)
	}
	mu.Unlock()

	m2 := newM(func(TurnEndSummary) { panic("钩子崩了") },
		func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return &scriptedModel{}, nil
		})
	s2 := m2.Registry().Create("张三", "会崩钩子", "auto", contract.UserPrefs{Model: "p/m"})
	s2.SetState(session.StateRunning)
	m2.Run(context.Background(), s2, "问", nil, func(session.Event) {})
	waitTitleFlight(t, s2)
	if s2.StateOf() != session.StateEnded {
		t.Fatalf("钩子 panic 不应拖垮收尾，终态 %s", s2.StateOf())
	}
}
