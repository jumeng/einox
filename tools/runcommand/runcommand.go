// Package runcommand 提供 run_command 工具：工作区内 shell 命令执行（P1b
// 简版——超时/输出头尾截断/cwd 圈进工作区；后台任务形态 P2 随编码子代理）。
// 输出截断策略参照 openai/codex unified_exec/head_tail_buffer.rs（Apache-2.0，
// 头尾保留中间省略——构建日志的头尾才是定位关键）；命令安全分类
// （IsSafeReadCommand）供装配层做参数级审批豁免（白名单只读命令直过，
// 思路参照 codex exec_policy 的规则版）。
package runcommand

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/sandbox"
	"github.com/jumeng/einox/tools"
	"github.com/jumeng/einox/tools/egress"
)

// Config 构造配置。Root = 工作区根（空 = 拒绝构造，P0 纪律）；Sandbox =
// 可选沙箱策略（nil = 不沙箱——默认零行为变化，真源 findings/2026-08-26-
// einox-sandbox-design.md §5.2）；SandboxProvider = 可选沙箱后端（nil =
// sandbox.OSProvider 平台内建；容器类后端经此注入——engine.Options 同名
// 字段透传）；Egress = 可选出口校验器（nil = 不预检，真源 §9——Network
// 开放形态下命令串 URL 预检是命令面的唯一网络治理层）。
type Config struct {
	Root            string
	Sandbox         *sandbox.Policy
	SandboxProvider sandbox.Provider
	Egress          *egress.Validator
}

type runIn struct {
	Command    string `json:"command"`
	TimeoutMS  int    `json:"timeout_ms"` // 0 = 默认 30s；上限 10min
	Background bool   `json:"background"` // true = 后台执行立即返回 task_id
}

type taskIn struct {
	TaskID string `json:"task_id"`
}

// bgTask 后台任务（输出环形累积 + 进程生命周期）。
type bgTask struct {
	id      string
	cmd     string
	mu      sync.Mutex
	buf     bytes.Buffer
	start   time.Time
	state   *os.ProcessState
	done    bool
	proc    *os.Process
	stopped bool
}

var (
	taskMu    sync.Mutex
	taskSeq   int
	taskTable = map[string]*bgTask{}
)

// maxBgTasks 后台任务表上限（防泄漏累积；超出拒绝新起）。
const maxBgTasks = 50

// startBackground 起后台进程，登记任务表。
func startBackground(root string, sb *sandbox.Policy, sp sandbox.Provider, cmdLine string) (string, error) {
	taskMu.Lock()
	if len(taskTable) >= maxBgTasks {
		taskMu.Unlock()
		return "", fmt.Errorf("后台任务已达上限 %d——先 task_stop 清理", maxBgTasks)
	}
	taskSeq++
	id := fmt.Sprintf("t%d", taskSeq)
	taskMu.Unlock()

	cmd, _ := buildCmd(context.Background(), root, sb, sp, cmdLine)
	bt := &bgTask{id: id, cmd: cmdLine, start: time.Now(), proc: cmd.Process}
	cmd.Stdout = bt
	cmd.Stderr = bt
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("启动失败：%w", err)
	}
	bt.proc = cmd.Process
	taskMu.Lock()
	taskTable[id] = bt
	taskMu.Unlock()
	go func() {
		err := cmd.Wait()
		bt.mu.Lock()
		bt.done = true
		bt.state = cmd.ProcessState
		if err != nil && cmd.ProcessState == nil {
			bt.stopped = true
		}
		bt.mu.Unlock()
	}()
	return id, nil
}

// Write io.Writer 接口（输出累积，锁保护）。
func (b *bgTask) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() > 1<<20 { // 输出上限 1MB：防失控进程吃内存
		return len(p), nil
	}
	return b.buf.Write(p)
}

// snapshot 任务状态快照。
func (b *bgTask) snapshot() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	exit := -1
	if b.state != nil {
		exit = b.state.ExitCode()
	}
	snap := map[string]any{
		"ok": true, "task_id": b.id, "command": b.cmd,
		"running": !b.done, "exit_code": exit,
		"duration_ms": time.Since(b.start).Milliseconds(),
		"output":      headTail(b.buf.Bytes()),
	}
	if hint := sandbox.DenialHint(string(b.buf.Bytes())); hint != "" { // 与前台 run() 同款标注（审查 C-4）
		snap["note"] = hint
	}
	return snap
}

func stopTask(id string) (map[string]any, error) {
	taskMu.Lock()
	bt, ok := taskTable[id]
	taskMu.Unlock()
	if !ok {
		return fail("任务不存在：" + id)
	}
	bt.mu.Lock()
	wasRunning := !bt.done
	proc := bt.proc
	bt.mu.Unlock()
	if wasRunning && proc != nil {
		sandbox.KillGroup(proc) // 进程组杀（沙箱形态整组终结；未组化回退单杀）
	}
	taskMu.Lock()
	delete(taskTable, id) // 停止即出表（快照由调用方先取）
	taskMu.Unlock()
	return map[string]any{"ok": true, "task_id": id, "stopped": wasRunning}, nil
}

const (
	defaultTimeoutMS = 30_000
	maxTimeoutMS     = 600_000
	headKeep         = 8 << 10 // 头 8KB
	tailKeep         = 8 << 10 // 尾 8KB
)

// NewTools 构造 run_command / task_output / task_stop（run 写面进审批名单，
// 白名单只读命令经 IsSafeReadCommand 参数级豁免；task_output 读直过，
// task_stop 管自己起的后台任务不进审批）。
func NewTools(cfg Config) ([]contract.Tool, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("runcommand 需要工作区根（拒绝全盘默认）")
	}
	root, absErr := filepath.Abs(cfg.Root)
	if absErr != nil {
		return nil, absErr
	}
	if cfg.Sandbox != nil {
		if err := cfg.Sandbox.Validate(); err != nil {
			return nil, err
		}
	}
	run, err := tools.InferTool("run_command",
		"在会话工作区内执行 shell 命令（cwd = 工作区根）。command 为单条命令行；timeout_ms 可选（默认 30 秒，上限 10 分钟）；输出超长时头尾各保留 8KB 中间省略；退出码非 0 不算失败——输出里有全部信息。长任务（构建/测试/服务）传 background=true：立即返回 task_id，之后用 task_output 查输出、task_stop 终止。",
		func(ctx context.Context, in runIn) (map[string]any, error) {
			return run(ctx, root, cfg.Sandbox, cfg.SandboxProvider, cfg.Egress, in)
		})
	if err != nil {
		return nil, err
	}
	out, err := tools.InferTool("task_output",
		"查询后台任务输出与状态（run_command background=true 起的任务）。返回运行中/退出码/输出（头尾保留）。",
		func(_ context.Context, in taskIn) (map[string]any, error) {
			taskMu.Lock()
			bt, ok := taskTable[in.TaskID]
			taskMu.Unlock()
			if !ok {
				return fail("任务不存在（已结束出表或未起）：" + in.TaskID)
			}
			return bt.snapshot(), nil
		})
	if err != nil {
		return nil, err
	}
	stop, err := tools.InferTool("task_stop",
		"终止后台任务（run_command background=true 起的任务）。",
		func(_ context.Context, in taskIn) (map[string]any, error) {
			return stopTask(in.TaskID)
		})
	if err != nil {
		return nil, err
	}
	return []contract.Tool{tools.WithBehavior(run, contract.BehaviorExec), tools.WithBehavior(out, contract.BehaviorRead), stop}, nil
}

// dockerWrap 已退役（2026-08-29 批次 C，设计真源 findings/2026-08-29-
// assembly-seams-design.md §4）：EINO_RUN_DOCKER env 魔法开关与「绕过
// policy」优先级告警撤除，容器形态正规化为 sandbox.DockerProvider——
// 经 Config.SandboxProvider / engine.Options.SandboxProvider 注入，策略
// 翻译进容器参数（见 sandbox/docker.go）。

// tokenAttachWarn windows token 构造失败告警（进程一次——auto 档裸跑降级，
// 与 Probe unusable 告警同款节流；真源 §4）。
var tokenAttachWarn sync.Once

// providerOf 配置归一（nil = OSProvider 平台内建）。
func providerOf(sp sandbox.Provider) sandbox.Provider {
	if sp != nil {
		return sp
	}
	return sandbox.OSProvider
}

// buildCmd 组装执行命令。沙箱分支 = provider.Wrap（OS 后端：re-exec 哨兵
// argv〔linux/darwin〕或直执行+token 侧挂〔windows〕；容器后端：一次性
// 容器 argv）+ 进程组长（组杀锚点）+ cmd.Env（去重合并；EnvMode 分档在
// provider 内）；后端不可用（auto 档）裸跑（Probe 已告警）。windows token
// 侧挂仅对 OSProvider（容器后端的 CLI 进程不套 restricted token——它要
// 正常访问 daemon 通道，围栏在容器层）。第二返回值 = 是否走沙箱（拒绝
// 提示标注仅沙箱生效路径）。
func buildCmd(ctx context.Context, root string, sb *sandbox.Policy, sp sandbox.Provider, cmdLine string) (*exec.Cmd, bool) {
	if sb != nil {
		p := providerOf(sp)
		if argv, env := p.Wrap(sb, root, cmdLine); argv != nil {
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Dir = root
			cmd.Env = env
			sandbox.SetGroupLeader(cmd)
			if p == sandbox.OSProvider {
				if err := sandbox.AttachToken(cmd, sb, root); err != nil {
					// windows restricted token 构造失败：裸跑降级会失去围栏——
					// 静默 fail-open 不可接受，告警一次（auto 语义；require 档
					// 接线后此处应拒跑）
					tokenAttachWarn.Do(func() {
						log.Printf("run_command: 沙箱 token 构造失败（%v）——该命令裸跑（auto 档降级）", err)
					})
					fallback := exec.CommandContext(ctx, "sh", "-c", cmdLine)
					fallback.Dir = root
					fallback.Env = env // 已 cleanseEnv + Policy.Env 重定向——降级不回退到继承全量环境
					return fallback, false
				}
			}
			cmd.Cancel = func() error { // 超时/取消通道同款进程组杀
				sandbox.KillGroup(cmd.Process)
				return nil
			}
			return cmd, true
		}
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdLine)
	cmd.Dir = root
	return cmd, false
}

func run(ctx context.Context, root string, sb *sandbox.Policy, sp sandbox.Provider, eg *egress.Validator, in runIn) (map[string]any, error) {
	cmdLine := strings.TrimSpace(in.Command)
	if cmdLine == "" {
		return fail("command 不能为空")
	}
	// 出口预检（真源 §9：接在审批放行与执行之间、覆盖前台与后台；fail-closed
	// ——命令串含阻断段 URL 即拒执行，沙箱 Network 开放形态下这是命令面的
	// 唯一网络治理层）
	if eg != nil {
		if err := eg.CheckCommand(cmdLine); err != nil {
			return fail(egress.BoundaryNote + "\n" + err.Error())
		}
	}
	if in.Background {
		id, err := startBackground(root, sb, sp, cmdLine)
		if err != nil {
			return fail(err.Error())
		}
		return map[string]any{
			"ok": true, "task_id": id, "command": cmdLine,
			"note": "已在后台启动——task_output 查输出与状态，task_stop 终止",
		}, nil
	}
	timeout := in.TimeoutMS
	if timeout <= 0 {
		timeout = defaultTimeoutMS
	}
	if timeout > maxTimeoutMS {
		return fail(fmt.Sprintf("timeout_ms 上限 %d", maxTimeoutMS))
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	cmd, sandboxed := buildCmd(runCtx, root, sb, sp, cmdLine)
	start := time.Now()
	out, _ := cmd.CombinedOutput() // 退出码/超时经 ProcessState 判定，err 不另用
	timedOut := runCtx.Err() == context.DeadlineExceeded
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	res := map[string]any{
		"ok": true, "command": cmdLine,
		"exit_code": exitCode, "timed_out": timedOut,
		"duration_ms": time.Since(start).Milliseconds(),
		"output":      headTail(out),
	}
	if timedOut {
		res["note"] = fmt.Sprintf("执行超时（%dms）已终止——加大 timeout_ms 或拆分任务", timeout)
	}
	if sandboxed {
		if hint := sandbox.DenialHint(string(out)); hint != "" {
			if n, ok := res["note"].(string); ok && n != "" {
				res["note"] = n + "\n" + hint
			} else {
				res["note"] = hint
			}
		}
	}
	return res, nil
}

// headTail 头尾保留截断（中间省略标记）。
func headTail(b []byte) string {
	if len(b) <= headKeep+tailKeep {
		return string(b)
	}
	return string(b[:headKeep]) +
		fmt.Sprintf("\n…（中间省略 %d 字节）…\n", len(b)-headKeep-tailKeep) +
		string(b[len(b)-tailKeep:])
}

// safeReadOnly 只读白名单（无 shell 元字符前提下直过审批——部署可按需扩）。
var safeReadOnly = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "find": true, "file": true, "du": true, "stat": true,
	"tree": true, "echo": true, "which": true, "pwd": true, "date": true,
	"whoami": true, "rg": true, "diff": true, "sort": true, "uniq": true,
}

// safeGitSub git 只读子命令。
var safeGitSub = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true, "branch": true,
	"blame": true, "remote": true, "tag": true, "rev-parse": true,
}

// IsSafeReadCommand 参数级审批豁免判定：纯只读命令（无元字符组合、无重定向、
// 无命令替换）直过；其余（rm/mvn/sudo/管道/重定向/任何白名单外）必审批。
// 判定从宽于「无害」从严于「白名单」：白名单程序 + 零元字符才豁免。
func IsSafeReadCommand(args string) bool {
	var in runIn
	if json.Unmarshal([]byte(args), &in) != nil {
		return false // 坏参数 fail-closed
	}
	cmdLine := strings.TrimSpace(in.Command)
	if cmdLine == "" {
		return false
	}
	// 元字符一票否决：组合/重定向/替换/管道均不可豁免
	for _, ch := range []string{";", "|", "&", ">", "<", "`", "$(", "(", ")"} {
		if strings.Contains(cmdLine, ch) {
			return false
		}
	}
	fields := strings.Fields(cmdLine)
	if len(fields) == 0 {
		return false
	}
	prog := fields[0]
	if prog == "git" {
		if len(fields) < 2 {
			return false
		}
		return safeGitSub[fields[1]]
	}
	if prog == "go" {
		return len(fields) == 2 && fields[1] == "version" // go version 只读；build/test 走审批
	}
	if prog == "python3" || prog == "python" {
		return len(fields) == 2 && fields[1] == "--version"
	}
	return safeReadOnly[prog]
}

func fail(msg string) (map[string]any, error) {
	return map[string]any{"ok": false, "error": msg}, nil // 回喂模型自纠（errFeed 语义）
}
