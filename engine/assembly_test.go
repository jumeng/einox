package engine

// 批次 A 装配缝回归（设计真源 findings/2026-08-29-assembly-seams-design.md
// §1/§2/§8.2/§8.8）：缝一 Tools 会话化（SessionBrief 携带 Owner/SID、按
// owner 分面）、缝二 sessionTools 裁剪（SessionToolsOff 族级裁剪 + 未知名
// NewManager fail-fast）、§8.8 构造错误上抛（族无声缺席 → CONFIG 错误卡）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/checkpoint"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/sandbox"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// newSeamManager 装配缝测试引擎（newRunManagerOn 同款底座 + Options 改写口）。
func newSeamManager(t *testing.T, mut func(*Options)) *Manager {
	t.Helper()
	st := tstore.New(t.TempDir())
	opt := Options{
		Providers: func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{{ID: "m", Input: []string{"text"}, Priority: 100}},
			}}
		},
		Instruction: func(SessionBrief) string { return "test" },
		NewModel: func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return &scriptedModel{}, nil
		},
		CheckPoints: func(operator, sid string) CheckPointStore {
			return checkpoint.NewCheckPointStore(st, operator, sid)
		},
		WorkspaceRoot: func(owner, sid string) string { return st.TmpDir() + "/ws/" + owner + "/" + sid },
	}
	if mut != nil {
		mut(&opt)
	}
	m, err := NewManager(session.NewRegistry(st), opt)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// toolNamesOf 工具名清单（白盒 sessionTools 面）。
func toolNamesOf(t *testing.T, m *Manager, s *session.Session) []string {
	t.Helper()
	ts, err := m.sessionTools(s)
	if err != nil {
		t.Fatalf("sessionTools: %v", err)
	}
	var names []string
	for _, x := range ts {
		if info := x.Info(); info != nil {
			names = append(names, info.Name)
		}
	}
	return names
}

// TestNewManagerRejectsUnknownSessionToolFamily 未知名构造期即拒（对齐
// DenyTools 纪律）；合法名单（含全六族）通过。
func TestNewManagerRejectsUnknownSessionToolFamily(t *testing.T) {
	_, err := NewManager(session.NewRegistry(tstore.New(t.TempDir())), Options{
		Providers:   func() []llm.ProviderSpec { return nil },
		Instruction: func(SessionBrief) string { return "test" },
		CheckPoints: func(operator, sid string) CheckPointStore { return nil },
		WorkspaceRoot: func(owner, sid string) string {
			return t.TempDir() + "/ws/" + owner + "/" + sid
		},
		SessionToolsOff: []string{"shell"},
	})
	if err == nil || !strings.Contains(err.Error(), "未知") {
		t.Fatalf("未知族名应构造期报错：%v", err)
	}
	// 缺必填项同样构造期即拒（NewManager 校验面：未知名 → 必填 nil，逐项报）
	if _, err := NewManager(session.NewRegistry(tstore.New(t.TempDir())), Options{}); err == nil ||
		!strings.Contains(err.Error(), "必填") {
		t.Fatalf("缺必填项应构造期报错：%v", err)
	}
	m, err := NewManager(session.NewRegistry(tstore.New(t.TempDir())), Options{
		SessionToolsOff: []string{FamilyTodo, FamilyAsk, FamilyPlan, FamilyFS, FamilyCmd, FamilyPatch},
		Providers:       func() []llm.ProviderSpec { return nil },
		Instruction:     func(SessionBrief) string { return "test" },
		CheckPoints:     func(operator, sid string) CheckPointStore { return nil },
		// WorkspaceRoot 必填（sessionTools 组装即取工作区根，nil 会 panic）
		WorkspaceRoot: func(owner, sid string) string {
			return t.TempDir() + "/ws/" + owner + "/" + sid
		},
	})
	if err != nil {
		t.Fatalf("合法族名不应报错：%v", err)
	}
	s := m.Registry().Create("张三", "问答", "manual", contract.UserPrefs{})
	if names := toolNamesOf(t, m, s); len(names) != 0 {
		t.Fatalf("全裁后会话域面应为空：%v", names)
	}
}

// TestSessionToolsOffTrimsFamilies 族级裁剪：nil = 六族全挂（11 件基线）；
// 裁 cmd/patch 两族后恰减对应 7→4 件，其余族不受波及。
func TestSessionToolsOffTrimsFamilies(t *testing.T) {
	m := newSeamManager(t, nil)
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{})
	full := toolNamesOf(t, m, s)
	for _, want := range []string{
		"todo_write", "ask_user", "submit_plan",
		"read_file", "list_dir", "search_files", "delete_file",
		"run_command", "task_output", "task_stop", "apply_patch",
	} {
		if !contains(full, want) {
			t.Fatalf("默认全挂缺 %s：%v", want, full)
		}
	}
	if len(full) != 11 {
		t.Fatalf("基线应为 11 件：%v", full)
	}

	m2 := newSeamManager(t, func(o *Options) { o.SessionToolsOff = []string{FamilyCmd, FamilyPatch} })
	s2 := m2.Registry().Create("张三", "任务", "manual", contract.UserPrefs{})
	trimmed := toolNamesOf(t, m2, s2)
	for _, gone := range []string{"run_command", "task_output", "task_stop", "apply_patch"} {
		if contains(trimmed, gone) {
			t.Fatalf("裁族后不应在场 %s：%v", gone, trimmed)
		}
	}
	for _, keep := range []string{"todo_write", "ask_user", "submit_plan", "read_file", "list_dir", "search_files", "delete_file"} {
		if !contains(trimmed, keep) {
			t.Fatalf("未裁族不应受波及 %s：%v", keep, trimmed)
		}
	}
}

// TestSessionToolsOffPatchCallErrors 裁 patch 族后面上无 apply_patch——模型
// 调用经幻觉兜底信封回喂自纠（不执行、不杀轮；物理移除执行面仍成立——信封
// 即「不在可用清单」，非审批语义可绕）。
func TestSessionToolsOffPatchCallErrors(t *testing.T) {
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "apply_patch", Arguments: `{"patch":"x"}`},
			}}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}
	m := newSeamManager(t, func(o *Options) {
		o.SessionToolsOff = []string{FamilyPatch}
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "改文件", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var names []string
	m.Run(context.Background(), s, "改一下", nil, func(ev session.Event) { names = append(names, ev.Event) })
	if contains(names, contract.EvError) {
		t.Fatalf("面外工具调用应信封回喂自纠（非错误收线）：%v", names)
	}
	if !contains(names, contract.EvSessionEnd) {
		t.Fatalf("自纠后应正常收线：%v", names)
	}
	// 模型第二调必须看到「不存在」信封（apply_patch 未被执行的直接证据）
	fed := false
	for _, c := range toolMsgOf(fm.inputs[len(fm.inputs)-1]) {
		if strings.Contains(c, "apply_patch") && strings.Contains(c, "不存在") {
			fed = true
		}
	}
	if !fed {
		t.Fatalf("面外调用应回喂不存在信封，实得 %+v", toolMsgOf(fm.inputs[len(fm.inputs)-1]))
	}
	for _, x := range toolNamesOf(t, m, s) {
		if x == "apply_patch" {
			t.Fatal("裁族后 apply_patch 不应回场")
		}
	}
}

// TestSessionToolsOffAskDegradesGracefully 裁 ask 族：纯文本轮正常收尾、
// 全程无 ask 类事件（引擎 ask 分支被动——优雅退化）。
func TestSessionToolsOffAskDegradesGracefully(t *testing.T) {
	m := newSeamManager(t, func(o *Options) { o.SessionToolsOff = []string{FamilyAsk} })
	s := m.Registry().Create("张三", "问答", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var names []string
	m.Run(context.Background(), s, "介绍一下", nil, func(ev session.Event) { names = append(names, ev.Event) })
	waitTitleFlight(t, s)
	if !contains(names, contract.EvSessionEnd) {
		t.Fatalf("裁 ask 族不影响正常收尾：%v", names)
	}
	for _, n := range names {
		if strings.HasPrefix(n, "ask") {
			t.Fatalf("裁族后不应出现 ask 类事件：%v", names)
		}
	}
}

// TestToolsBriefCarriesIdentity Tools 闭包入参携带会话身份（每轮求值与
// Instruction 同源的 SessionBrief——Owner/SID/Mode/Model 全量可寻址）。
func TestToolsBriefCarriesIdentity(t *testing.T) {
	var briefs []SessionBrief
	m := newSeamManager(t, func(o *Options) {
		o.Tools = func(b SessionBrief) []contract.Tool {
			briefs = append(briefs, b)
			return nil
		}
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	m.Run(context.Background(), s, "查一下", nil, func(session.Event) {})
	waitTitleFlight(t, s)
	if len(briefs) == 0 {
		t.Fatal("Tools 闭包应被求值")
	}
	for _, b := range briefs {
		if b.Owner != "张三" || b.SID != s.SID || b.Mode != "manual" || b.Model != "p/m" {
			t.Fatalf("brief 身份失真：%+v", b)
		}
	}
}

// ownerGatedTool ownerB 专属工具桩（Behavior 使 assemble 的 behaviors 快照
// 可寻址——面成员判定的白盒锚）。
func ownerGatedTool(t *testing.T) contract.Tool {
	t.Helper()
	g, err := tools.InferTool("ownerb_tool", "ownerB 专属", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tools.WithBehavior(g, contract.BehaviorRead)
}

// TestToolsPerOwnerFace 按 owner 分面：ownerB 的会话面含专属件、ownerA 不含
// ——多租户工具面经 Options.Tools 单点达成（无需拆 Manager 实例）。
func TestToolsPerOwnerFace(t *testing.T) {
	gated := ownerGatedTool(t)
	m := newSeamManager(t, func(o *Options) {
		o.Tools = func(b SessionBrief) []contract.Tool {
			if b.Owner != "ownerB" {
				return nil
			}
			return []contract.Tool{gated}
		}
	})
	sA := m.Registry().Create("ownerA", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	_, _, behA, err := m.assemble(context.Background(), sA)
	if err != nil {
		t.Fatalf("ownerA assemble: %v", err)
	}
	if _, ok := behA["ownerb_tool"]; ok {
		t.Fatal("ownerA 面不应含 ownerb_tool")
	}
	sB := m.Registry().Create("ownerB", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	_, _, behB, err := m.assemble(context.Background(), sB)
	if err != nil {
		t.Fatalf("ownerB assemble: %v", err)
	}
	if _, ok := behB["ownerb_tool"]; !ok {
		t.Fatalf("ownerB 面应含 ownerb_tool：%v", behB)
	}
}

// TestDenyToolsOwnerGatedConfigError 子代理 DenyTools 引用被 owner 门禁掉的
// 工具名：该会话 Run 以 CONFIG 错误卡暴露（loud 优于静默空面——白名单源随
// 会话面收缩的既有 fail-fast 语义保持）。
func TestDenyToolsOwnerGatedConfigError(t *testing.T) {
	gated := ownerGatedTool(t)
	m := newSeamManager(t, func(o *Options) {
		o.Tools = func(b SessionBrief) []contract.Tool {
			if b.Owner != "ownerB" {
				return nil
			}
			return []contract.Tool{gated}
		}
		o.SubAgents = &SubAgentsConfig{
			Tools:     []string{"read_file"},
			DenyTools: []string{"ownerb_tool"},
		}
	})
	s := m.Registry().Create("ownerA", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var errs []contract.ErrorOut
	m.Run(context.Background(), s, "做点事", nil, func(ev session.Event) {
		if ev.Event == contract.EvError {
			if eo, ok := ev.Data.(contract.ErrorOut); ok {
				errs = append(errs, eo)
			}
		}
	})
	found := false
	for _, eo := range errs {
		if eo.Code == "CONFIG" && strings.Contains(eo.Message, "DenyTools") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应 CONFIG 错误卡暴露 DenyTools 未同名：%+v", errs)
	}
}

// TestSessionToolsErrorSurfacesAsConfig 构造错误上抛（§8.8）：WorkspaceRoot
// 返回空 → 文件面族构造失败 → CONFIG 错误卡（此前为族无声缺席）。
func TestSessionToolsErrorSurfacesAsConfig(t *testing.T) {
	m := newSeamManager(t, func(o *Options) {
		o.WorkspaceRoot = func(owner, sid string) string { return "" }
	})
	s := m.Registry().Create("张三", "任务", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var errs []contract.ErrorOut
	m.Run(context.Background(), s, "做点事", nil, func(ev session.Event) {
		if ev.Event == contract.EvError {
			if eo, ok := ev.Data.(contract.ErrorOut); ok {
				errs = append(errs, eo)
			}
		}
	})
	found := false
	for _, eo := range errs {
		if eo.Code == "CONFIG" && strings.Contains(eo.Message, "工作区根") {
			found = true
		}
	}
	if !found {
		t.Fatalf("应以 CONFIG 错误卡暴露构造失败：%+v", errs)
	}
}

// sandboxFakeProvider 引擎端注入桩（Options.SandboxProvider → sessiontools →
// runcommand 全链接线验证）。
type sandboxFakeProvider struct {
	calls int32
}

func (f *sandboxFakeProvider) Wrap(*sandbox.Policy, string, string) ([]string, []string) {
	atomic.AddInt32(&f.calls, 1)
	return []string{"echo", "SANDBOX_PROVIDER_OK"}, nil
}

func (f *sandboxFakeProvider) Probe() sandbox.Status {
	return sandbox.Status{Enforcement: sandbox.EnforcementFull}
}

// TestSandboxProviderPlumbing 沙箱后端注入链（批次 C）：Options.SandboxProvider
// 经 sessiontools 透传 runcommand——run_command 执行面即注入后端的 argv。
func TestSandboxProviderPlumbing(t *testing.T) {
	fp := &sandboxFakeProvider{}
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
		o.Sandbox = &sandbox.Policy{Mode: sandbox.ModeWorkspaceWrite}
		o.SandboxProvider = fp
		o.NewModel = func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		}
	})
	s := m.Registry().Create("张三", "跑命令", "auto", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var outputs []string
	m.Run(context.Background(), s, "跑一下", nil, func(ev session.Event) {
		if ev.Event == contract.EvToolResult {
			if tr, ok := ev.Data.(contract.ToolResult); ok {
				outputs = append(outputs, tr.Preview)
			}
		}
	})
	waitTitleFlight(t, s)
	if atomic.LoadInt32(&fp.calls) != 1 {
		t.Fatalf("注入后端 Wrap 应被调用一次，实得 %d", fp.calls)
	}
	joined := strings.Join(outputs, "\n")
	if !strings.Contains(joined, "SANDBOX_PROVIDER_OK") {
		t.Fatalf("执行面应为注入后端的 argv（echo SANDBOX_PROVIDER_OK）：%q", joined)
	}
}

// TestWorkspaceGuardSeams 工作区持久/写保护区装配缝：① 非法条目（嵌套
// 路径/绝对路径/../分隔符/空串）NewManager 即拒——对齐 SessionToolsOff
// fail-fast 纪律；② 合法 WorkspaceProtect 注入 fsutil/applypatch 写面——
// delete_file 与补丁目标（含混单的非保护区目标）命中即拒信封、整单不部分
// 应用，保护区外照常、读面不受波及。
func TestWorkspaceGuardSeams(t *testing.T) {
	// ① 构造期校验
	for _, tc := range []struct {
		field string
		mut   func(*Options)
	}{
		{"WorkspaceKeep", func(o *Options) { o.WorkspaceKeep = []string{"repos/x"} }},
		{"WorkspaceKeep", func(o *Options) { o.WorkspaceKeep = []string{".."} }},
		{"WorkspaceProtect", func(o *Options) { o.WorkspaceProtect = []string{"/abs"} }},
		{"WorkspaceProtect", func(o *Options) { o.WorkspaceProtect = []string{`a\b`} }},
		{"WorkspaceProtect", func(o *Options) { o.WorkspaceProtect = []string{""} }},
	} {
		opt := Options{
			Providers:   func() []llm.ProviderSpec { return nil },
			Instruction: func(SessionBrief) string { return "test" },
			CheckPoints: func(operator, sid string) CheckPointStore { return nil },
			WorkspaceRoot: func(owner, sid string) string {
				return t.TempDir() + "/ws/" + owner + "/" + sid
			},
		}
		tc.mut(&opt)
		_, err := NewManager(session.NewRegistry(tstore.New(t.TempDir())), opt)
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Fatalf("%s 非法条目应构造期报错（含字段名）：%v", tc.field, err)
		}
	}

	// ② 注入链：WorkspaceProtect → fsutil/applypatch 写面
	m := newSeamManager(t, func(o *Options) { o.WorkspaceProtect = []string{"repos"} })
	s := m.Registry().Create("张三", "勘察", "manual", contract.UserPrefs{})
	var del, rd, ap contract.Tool
	for _, x := range func() []contract.Tool {
		ts, err := m.sessionTools(s)
		if err != nil {
			t.Fatalf("sessionTools: %v", err)
		}
		return ts
	}() {
		switch x.Info().Name {
		case "delete_file":
			del = x
		case "read_file":
			rd = x
		case "apply_patch":
			ap = x
		}
	}
	if del == nil || rd == nil || ap == nil {
		t.Fatal("会话域面应含 delete_file/read_file/apply_patch")
	}

	// 工作区铺：保护区内文件 + 保护区外文件
	ws := m.Opt.WorkspaceRoot("张三", s.SID)
	if err := os.MkdirAll(filepath.Join(ws, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "repos", "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "notes", "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	invoke := func(t *testing.T, tool contract.Tool, args string) string {
		t.Helper()
		out, err := tool.Invoke(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s Invoke 不应返回 Go error：%v", tool.Info().Name, err)
		}
		return string(out)
	}

	// apply_patch 混单整拒：含保护区目标（Add repos/new.txt）即整体不应用，
	// 非保护区目标（Update notes/b.txt）不部分落盘
	mixed := "*** Begin Patch\n*** Update File: notes/b.txt\n@@\n-y\n+z\n*** Add File: repos/new.txt\n+n\n*** End Patch"
	if out := invoke(t, ap, fmt.Sprintf(`{"patch":%q}`, mixed)); !strings.Contains(out, "写保护区") {
		t.Fatalf("补丁含保护区目标应整单拒绝：%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "notes", "b.txt")); string(b) != "y" {
		t.Fatalf("混单拒绝应事务性（notes/b.txt 不得部分应用）：%q", b)
	}

	// delete_file 命中即拒：直接命中、./ 前缀变体、平台分隔符变体
	for _, p := range []string{"repos/a.txt", "./repos/a.txt", filepath.Join("repos", "a.txt")} {
		if out := invoke(t, del, fmt.Sprintf(`{"path":%q}`, p)); !strings.Contains(out, "写保护区") {
			t.Fatalf("delete_file(%s) 应拒（写保护区）：%s", p, out)
		}
	}

	// 读面不受波及；保护区外照常
	if out := invoke(t, rd, `{"path":"repos/a.txt"}`); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("read_file 不受写保护区影响：%s", out)
	}
	if out := invoke(t, del, `{"path":"notes/b.txt"}`); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("保护区外 delete_file 应照常：%s", out)
	}
}
