package engine

// 编码闭环引擎级链路用例（2026-08-25 设计验收硬性要求）：驱动真实 Run 循环
// 而非直调工具——
// ① 任务收尾 wipe：非 repos/ 临时产物照清、repos/ 挂载保留（应用层「工作区
//    临时脚本」用法零回归——StateEnded → settleTurn → wipeWorkspace 链路）；
// ② repo_commit ArgsForce 挂起：auto 档不豁免、审批卡携带逐行 diff
//   （DiffToolOf → hitl 探测 ApprovalDiff → pump 透传 ApprovalReq.Diff 全链）。

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/checkpoint"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/hitl"
	"github.com/jumeng/einox/internal/tstore"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// TestTaskEndWipeKeepsRepoMounts 任务收尾 wipe：非 repos 临时产物照清、
// repos/ 挂载保留（scriptedModel 一轮纯文本收尾 → StateEnded → wipeWorkspace）。
func TestTaskEndWipeKeepsRepoMounts(t *testing.T) {
	// onStream 缺省 = 每轮纯文本「已处理。」——首轮即收尾（StateEnded）
	fm := &scriptedModel{}
	st := tstore.New(t.TempDir())
	m := newRunManagerOn(t, nil, func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
		return fm, nil
	}, st)
	s := m.Registry().Create("张三", "编码", "manual", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)

	// Run 前手工铺工作区（路径与 Options.WorkspaceRoot 同源拼接）：repos 挂载
	// 占位 + 临时脚本 + spill 溢出产物
	ws := st.TmpDir() + "/ws/张三/" + s.SID
	if err := os.MkdirAll(filepath.Join(ws, "repos", "base-app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "repos", "base-app", "a.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "tmp.sh"), []byte("echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "spill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "spill", "01.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.Run(context.Background(), s, "干活", nil, func(session.Event) {})
	waitTitleFlight(t, s) // genTitle 在途写收尾后再断言/清理

	if s.StateOf() != session.StateEnded {
		t.Fatalf("纯文本一轮应收尾：%s", s.StateOf())
	}
	if _, err := os.Stat(filepath.Join(ws, "tmp.sh")); err == nil {
		t.Fatal("临时脚本应随任务收尾清除")
	}
	if _, err := os.Stat(filepath.Join(ws, "spill")); err == nil {
		t.Fatal("spill 应随任务收尾清除")
	}
	if _, err := os.Stat(filepath.Join(ws, "repos", "base-app", "a.go")); err != nil {
		t.Fatal("repos/ 挂载应跨任务保留：", err)
	}
}

// TestRepoCommitApprovalCardDiff repo_commit ArgsForce 端到端：auto 档挂载
// 直落后提交挂起，审批卡携带逐行 diff（挂起即收线，无需 Resume）。
func TestRepoCommitApprovalCardDiff(t *testing.T) {
	base, cache := newGitFixture(t)

	// mountFile 会话创建后回填（挂载点随 SID 定位）；onStream 第 2 轮前手工
	// 落盘改动——react 轮次间隙 = 上轮工具已执行、下轮调用未发的窗口
	var mountFile string
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		if n == 1 {
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				ID: "c-repo-1", Type: "function",
				Function: schema.FunctionCall{Name: "open_repo", Arguments: `{"name":"base-app"}`},
			}}})
			return
		}
		if n == 2 {
			// open_repo 已挂载完毕（结果回喂本轮流）——改挂载内文件制造待提交 diff
			if err := os.WriteFile(mountFile, []byte("hello v2\n"), 0o644); err != nil {
				t.Errorf("改挂载文件失败：%v", err)
			}
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				ID: "c-repo-2", Type: "function",
				Function: schema.FunctionCall{Name: "repo_commit", Arguments: `{"name":"base-app","message":"t"}`},
			}}})
			return
		}
		send(&schema.Message{Role: schema.Assistant, Content: "完成"})
	}}

	st := tstore.New(t.TempDir())
	m := NewManager(session.NewRegistry(st), Options{
		Providers: func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID: "p", Kind: "openai", Enabled: true,
				Models: []llm.ModelSpec{{ID: "m", Input: []string{"text"}, Priority: 100}},
			}}
		},
		Instruction: func(SessionBrief) string { return "test" },
		NewModel: func(context.Context, llm.ProviderSpec, llm.ModelSpec, string) (model.BaseModel[*schema.Message], error) {
			return fm, nil
		},
		CheckPoints: func(operator, sid string) CheckPointStore {
			return checkpoint.NewCheckPointStore(st, operator, sid)
		},
		// auto 档：open_repo（写名单内）直落挂载；repo_commit 命中 ArgsForce
		// 无视模式豁免一律挂起——对齐产品「commit 属人工确认」红线装配形态
		Approval: hitl.ApprovalConfig{
			WriteTools: map[string]bool{"open_repo": true, "repo_commit": true},
			ArgsForce:  map[string]func(args string) bool{"repo_commit": func(string) bool { return true }},
		},
		WorkspaceRoot: func(owner, sid string) string { return filepath.Join(base, "ws", owner, sid) },
		RepoMounts:    dirMounts{dir: cache},
	})

	s := m.Registry().Create("张三", "提交", "auto", contract.UserPrefs{Model: "p/m"})
	mountFile = filepath.Join(base, "ws", "张三", s.SID, "repos", "base-app", "a.txt")
	s.SetState(session.StateRunning)

	var names []string
	var card *contract.ApprovalReq
	m.Run(context.Background(), s, "提交改动", nil, func(ev session.Event) {
		names = append(names, ev.Event)
		if ev.Event == contract.EvApprovalRequest {
			req, ok := ev.Data.(contract.ApprovalReq)
			if !ok {
				t.Fatalf("approval_request 载荷应为 contract.ApprovalReq：%T", ev.Data)
			}
			card = &req
		}
	})
	t.Cleanup(func() { stopApprovalTimer(s.SID) }) // 挂起超时器不跨用例残留

	if !contains(names, contract.EvApprovalRequest) {
		t.Fatalf("repo_commit ArgsForce 应挂起（auto 档不豁免）：%v", names)
	}
	if card == nil || card.Tool != "repo_commit" {
		t.Fatalf("应捕获 repo_commit 审批卡：%+v", card)
	}
	// 断言面在 ApprovalReq.Diff 本体——DiffProvider → hitl 探测 → pump 透传链
	if !strings.Contains(card.Diff, "+hello v2") {
		t.Fatalf("审批卡应携带逐行 diff（含 + 新行）：%q", card.Diff)
	}
	if s.StateOf() != session.StatePendingApproval {
		t.Fatalf("挂起即收线，应为 pending_approval：%s", s.StateOf())
	}
}

// newGitFixture 建 非 bare 缓存仓 fixture（init + user 配置 + 1 个提交），
// 形态照 einox/tools/repo/repo_test.go 的 newFixture 本地小复刻（该包私有
// 不可引）。返回 (base 临时根, cache 仓目录)。
func newGitFixture(t *testing.T) (base, cache string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}
	base = t.TempDir()
	cache = filepath.Join(base, "repos-cache", "base-app-x1")
	fixtureGit(t, "", "init", "-q", "-b", "main", cache)
	fixtureGit(t, cache, "config", "user.name", "t")
	fixtureGit(t, cache, "config", "user.email", "t@t")
	if err := os.WriteFile(filepath.Join(cache, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, cache, "add", "-A")
	fixtureGit(t, cache, "commit", "-q", "-m", "init")
	return base, cache
}

// fixtureGit fixture 专用 git exec（stderr 并入报错信息）。
func fixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		t.Fatalf("fixture git %v 失败：%v: %s", args, err, buf.String())
	}
}

// dirMounts 指向 fixture 缓存仓的桩 resolver（短名恒命中，基线 main）。
type dirMounts struct{ dir string }

func (d dirMounts) Resolve(string) (string, string, bool) { return d.dir, "main", true }
