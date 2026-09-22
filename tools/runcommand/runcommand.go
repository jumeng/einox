// Package runcommand 提供 run_command 工具族：run_command / task_output /
// task_stop（工作区内 shell 执行——超时/输出头尾截断或尾行取样/cwd 圈进
// 工作区/env 白名单注入/后台任务生命周期）。输出截断策略参照 openai/codex
// unified_exec/head_tail_buffer.rs（Apache-2.0，头尾保留中间省略——构建日志
// 的头尾才是定位关键）；默认超时 120s 与 tail 取样对齐 zcode Bash 实测默认
// （go build/test 常超 30s，2026-09-22 对比裁决）；命令安全分类
// （IsSafeReadCommand）供装配层做参数级审批豁免（白名单只读命令直过，
// 思路参照 codex exec_policy 的规则版）。
package runcommand

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/strutil"
	"github.com/jumeng/einox/sandbox"
	"github.com/jumeng/einox/tools"
	"github.com/jumeng/einox/tools/egress"
)

// Config 构造配置。Root = 工作区根（空 = 拒绝构造，P0 纪律）；Sandbox =
// 可选沙箱策略（nil = 不沙箱——默认零行为变化，2026-08-26 沙箱设计定案
// §5.2）；SandboxProvider = 可选沙箱后端（nil =
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
	Command    string   `json:"command"`
	TimeoutMS  int      `json:"timeout_ms"` // 0 = 默认 120s；上限 10min
	Background bool     `json:"background"` // true = 后台执行立即返回 task_id
	Cwd        string   `json:"cwd"`        // 工作区内相对路径（空 = 根；圈禁解析，越界拒）
	Env        []string `json:"env"`        // "K=V" 注入（键黑名单拒；上限 32 项）
	TailLines  int      `json:"tail_lines"` // >0 = 输出取尾 N 行（钳 2000；替代头尾截断）
}

type taskIn struct {
	TaskID string `json:"task_id"`
}

// bgTask 后台任务（输出环形累积 + 进程生命周期）。
type bgTask struct {
	id        string
	cmd       string
	root      string // 起任务时的工作区根（应用侧归属反解——taskquery 缝）
	mu        sync.Mutex
	buf       bytes.Buffer
	start     time.Time
	end       time.Time // 完成时刻（done 后时长冻结——查询缝设计件伴生修）
	state     *os.ProcessState
	done      bool
	proc      *os.Process
	stopped   bool
	sandboxed bool // 沙箱生效路径标记（DenialHint 标注仅沙箱形态——裸跑的普通失败不误标）
}

var (
	taskMu    sync.Mutex
	taskSeq   int
	taskTable = map[string]*bgTask{}
)

// maxBgTasks 后台任务表上限（防泄漏累积；超出拒绝新起）。
const maxBgTasks = 50

// startBackground 起后台进程，登记任务表。
func startBackground(root, cwd string, sb *sandbox.Policy, sp sandbox.Provider, cmdLine string, extraEnv []string) (string, error) {
	bgDir := cwd
	if bgDir == "" {
		bgDir = root
	}
	cmd, sandboxed, err := sandbox.BuildCommand(context.Background(), root, bgDir, sb, sp, []string{"sh", "-c", cmdLine}, extraEnv)
	if err != nil {
		return "", err // require 姿态 fail-closed（BuildCommand 文案已带指引）
	}
	bt := &bgTask{cmd: cmdLine, root: root, start: time.Now(), sandboxed: sandboxed} // id/proc 占位后回填
	cmd.Stdout = bt
	cmd.Stderr = bt
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("启动失败：%w", err)
	}
	bt.proc = cmd.Process
	go func() {
		err := cmd.Wait()
		bt.mu.Lock()
		bt.done = true
		bt.end = time.Now()
		bt.state = cmd.ProcessState
		if err != nil && cmd.ProcessState == nil {
			bt.stopped = true
		}
		bt.mu.Unlock()
	}()
	return adoptTask(bt)
}

// adoptTask 收编进任务表（容量/淘汰律单点：满时先淘汰已自然结束的表项，
// 仍满即拒——此前完成项永不出表，50 上限会被耗尽且 task_output 的「已结束
// 出表」文案与实现矛盾——安全审查 2026-09-06）。
func adoptTask(bt *bgTask) (string, error) {
	taskMu.Lock()
	if len(taskTable) >= maxBgTasks {
		for id, t := range taskTable {
			if len(taskTable) < maxBgTasks {
				break
			}
			t.mu.Lock()
			done := t.done
			t.mu.Unlock()
			if done {
				delete(taskTable, id)
			}
		}
	}
	if len(taskTable) >= maxBgTasks {
		taskMu.Unlock()
		return "", fmt.Errorf("后台任务已达上限 %d——先 task_stop 清理（已自然结束的任务会自动出表）", maxBgTasks)
	}
	taskSeq++
	bt.id = fmt.Sprintf("t%d", taskSeq)
	taskTable[bt.id] = bt // 容量检查与登记同锁：并发起任务不超上限
	taskMu.Unlock()
	return bt.id, nil
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
	dur := time.Since(b.start).Milliseconds()
	if b.done && !b.end.IsZero() { // 完成态冻结（此前随墙钟续涨——缺陷修，非语义变化）
		dur = b.end.Sub(b.start).Milliseconds()
	}
	snap := map[string]any{
		"ok": true, "task_id": b.id, "command": b.cmd,
		"running": !b.done, "exit_code": exit,
		"duration_ms": dur,
		"output":      headTail(b.buf.Bytes()),
	}
	if b.sandboxed { // 与前台 run() 同款门控（裸跑任务的普通 permission denied 不误标「沙箱拒绝」）
		if hint := sandbox.DenialHint(string(b.buf.Bytes())); hint != "" { // 审查 C-4
			snap["note"] = hint
		}
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
	defaultTimeoutMS = 120_000 // zcode Bash 同款实测默认（2026-09-22 对比裁决：30s 常不够 build/test）
	maxTimeoutMS     = 600_000
	headKeep         = 8 << 10  // 头 8KB
	tailKeep         = 8 << 10  // 尾 8KB
	maxCmdLineBytes  = 64 << 10 // 命令行长度上限（输入加固——失控载荷面）
	maxEnvPairs      = 32       // env 注入项数上限
	maxEnvValBytes   = 8 << 10  // env 单值上限
	maxTailLines     = 2000     // tail_lines 钳制上限
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
		"在会话工作区内执行 shell 命令（默认 cwd = 工作区根）。command 为单条命令行；timeout_ms 可选（默认 120 秒，上限 10 分钟）——超时不终止而是自动转后台（返回 task_id，用 task_output 收割、task_stop 终止）；cwd 可选（工作区内相对路径，越界拒绝）；env 可选（\"K=V\" 注入环境变量，PATH/HOME 等关键变量拒绝，至多 32 项）；输出超长时头尾各保留 8KB 中间省略——只要尾部时传 tail_lines（取尾 N 行，长测试日志定位失败摘要用）；退出码非 0 不算失败——输出里有全部信息。长任务（构建/测试/服务）传 background=true：立即返回 task_id，之后用 task_output 查输出、task_stop 终止。",
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

// dockerWrap 已退役（2026-08-29 批次 C，装配缝设计 §4 定案）：EINO_RUN_DOCKER env 魔法开关与「绕过
// policy」优先级告警撤除，容器形态正规化为 sandbox.DockerProvider——
// 经 Config.SandboxProvider / engine.Options.SandboxProvider 注入，策略
// 翻译进容器参数（见 sandbox/docker.go）。

func run(ctx context.Context, root string, sb *sandbox.Policy, sp sandbox.Provider, eg *egress.Validator, in runIn) (map[string]any, error) {
	cmdLine := strings.TrimSpace(in.Command)
	if cmdLine == "" {
		return fail("command 不能为空")
	}
	// cwd 圈禁解析（zcode Bash cwd 策略的 einox 形态：工作区内相对路径，
	// 越界 fail-closed——不做「越界 reset 回根」的宽放形态，围栏语义优先）
	cwd := ""
	if c := strings.TrimSpace(in.Cwd); c != "" {
		under, err := tools.ResolveUnder(root, c)
		if err != nil {
			return fail("cwd 越界（须为工作区内相对路径）：" + c)
		}
		cwd = under
	}
	// env 注入校验（fail-closed：黑名单键/坏形态/超限整批拒）
	extraEnv, err := parseEnvPairs(in.Env)
	if err != nil {
		return fail(err.Error())
	}
	// 输入加固（安全审查 2026-09-06）：NUL 使 argv 传递截断失真、超长命令行
	// 是失控载荷面——入口即拒（执行语义不变：合法命令行照常经审批/沙箱执行）
	if strings.ContainsRune(cmdLine, 0) {
		return fail("command 含 NUL 字节（非法命令行）")
	}
	if len(cmdLine) > maxCmdLineBytes {
		return fail(fmt.Sprintf("command 超长（上限 %d 字节）——拆分任务", maxCmdLineBytes))
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
		id, err := startBackground(root, cwd, sb, sp, cmdLine, extraEnv)
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
	dir := cwd
	if dir == "" {
		dir = root // 原空 = 工作区根语义（BuildCommand 的 dir 空 = 继承 cwd，归一在此）
	}
	// 超时自动转后台（2026-09-22 对比裁决，zcode auto_on_timeout 对位）：到点
	// 进程未结束 → 收编任务表（进程不杀，输出续灌同一 bt 缓冲），返回 task_id；
	// 取消链保留——parent ctx 取消 / guard 截止仍经 cmd.Cancel=KillGroup 整组
	// 终结（故不以 WithTimeout 驱动杀，改 select 计时）。
	cmd, sandboxed, err := sandbox.BuildCommand(ctx, root, dir, sb, sp, []string{"sh", "-c", cmdLine}, extraEnv)
	if err != nil {
		return fail(err.Error()) // require 姿态 fail-closed——不降级不裸跑
	}
	start := time.Now()
	bt := &bgTask{cmd: cmdLine, start: start, sandboxed: sandboxed}
	cmd.Stdout = bt
	cmd.Stderr = bt
	if err := cmd.Start(); err != nil {
		return fail("启动失败：" + err.Error())
	}
	bt.proc = cmd.Process
	done := make(chan struct{})
	go func() { // 状态机单点：前台完成与收编后台共用（done/state 只此处置）
		_ = cmd.Wait()
		bt.mu.Lock()
		bt.done = true
		bt.state = cmd.ProcessState
		if cmd.ProcessState == nil {
			bt.stopped = true // Cancel 链终结（非自然退出）
		}
		bt.mu.Unlock()
		close(done)
	}()
	timer := time.NewTimer(time.Duration(timeout) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-done:
		bt.mu.Lock()
		out := append([]byte(nil), bt.buf.Bytes()...)
		state := bt.state
		bt.mu.Unlock()
		exitCode := -1
		if state != nil {
			exitCode = state.ExitCode()
		}
		res := map[string]any{
			"ok": true, "command": cmdLine,
			"exit_code":   exitCode,
			"duration_ms": time.Since(start).Milliseconds(),
			"output":      outputOf(out, in.TailLines),
		}
		if cwd != "" {
			if rel, rerr := filepath.Rel(root, cwd); rerr == nil {
				res["cwd"] = filepath.ToSlash(rel)
			}
		}
		if sandboxed {
			if hint := sandbox.DenialHint(string(out)); hint != "" {
				res["note"] = hint
			}
		}
		return res, nil
	case <-timer.C:
		// 自动转后台：收编任务表（容量/淘汰同 startBackground 既有律）
		id, aerr := adoptTask(bt)
		if aerr != nil {
			// 任务表满：收编失败——退回硬超时语义（杀进程，已产出输出照常返回）
			sandbox.KillGroup(cmd.Process)
			<-done
			bt.mu.Lock()
			out := append([]byte(nil), bt.buf.Bytes()...)
			bt.mu.Unlock()
			return map[string]any{
				"ok": true, "command": cmdLine, "exit_code": -1, "timed_out": true,
				"duration_ms": time.Since(start).Milliseconds(),
				"output":      outputOf(out, in.TailLines),
				"note":        fmt.Sprintf("执行超时（%dms）已终止（后台任务表满无法转后台）——task_stop 清理后重试", timeout),
			}, nil
		}
		return map[string]any{
			"ok": true, "command": cmdLine, "task_id": id,
			"duration_ms": time.Since(start).Milliseconds(),
			"note":        fmt.Sprintf("执行超时（%dms）——已自动转后台继续执行，task_output 查输出、task_stop 终止", timeout),
		}, nil
	}
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

// outputOf 输出面策略：tail_lines > 0 = 尾 N 行取样（钳 maxTailLines，超长
// 日志定位失败摘要用——头尾 8KB 对长测试日志不够，zcode Bash preview tail
// 同款取向）；否则头尾截断。
func outputOf(b []byte, tailLines int) string {
	if tailLines <= 0 {
		return headTail(b)
	}
	n := min(tailLines, maxTailLines)
	lines := strings.Split(string(b), "\n")
	if len(lines) > n {
		omitted := len(lines) - n
		lines = lines[len(lines)-n:]
		return fmt.Sprintf("…（前面省略 %d 行）…\n", omitted) + strings.Join(lines, "\n")
	}
	return strings.Join(lines, "\n")
}

// envKeyBlocked env 注入键黑名单：进程定位类关键变量被改写 = 沙箱/工具链
// 语义旁路（PATH 劫持 exec、HOME 改写凭证寻址）——fail-closed 整批拒。
// Windows 环境变量名大小写不敏感，比对折叠。
var envKeyBlocked = map[string]bool{
	"PATH": true, "HOME": true, "PWD": true, "SHELL": true,
	"USER": true, "USERNAME": true, "TMP": true, "TEMP": true,
	"LD_PRELOAD": true, "DYLD_INSERT_LIBRARIES": true, // 动态库注入面
}

// parseEnvPairs 校验并归一 env 注入（[]string "K=V"）。键名形态、黑名单、
// 项数与值长全查；任一非法整批拒（fail-closed——不带病注入）。
func parseEnvPairs(pairs []string) ([]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	if len(pairs) > maxEnvPairs {
		return nil, fmt.Errorf("env 至多 %d 项（收到 %d）", maxEnvPairs, len(pairs))
	}
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("env 项须为 K=V 形态（收到 %q）", strutil.Truncate(kv, 40))
		}
		if envKeyBlocked[strings.ToUpper(k)] {
			return nil, fmt.Errorf("env 键 %s 在黑名单内（进程定位类关键变量不可注入）", k)
		}
		if !validEnvKey(k) {
			return nil, fmt.Errorf("env 键名非法（须字母/下划线开头，字母数字下划线组成）：%s", strutil.Truncate(k, 40))
		}
		if len(v) > maxEnvValBytes {
			return nil, fmt.Errorf("env 值超长（上限 %d 字节）：键 %s", maxEnvValBytes, k)
		}
	}
	return pairs, nil
}

// validEnvKey 键名字符集（shell 合法 env 名子集）。
func validEnvKey(k string) bool {
	for i, r := range k {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return k != ""
}

// safeReadOnly 只读白名单（无 shell 元字符前提下直过审批——部署可按需扩）。
// tree 不在列（-o 可写文件——安全审查 2026-09-06）；find 在列但写型 flag
// 拒绝（见 findWriteFlag）。
var safeReadOnly = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "find": true, "file": true, "du": true, "stat": true,
	"echo": true, "which": true, "pwd": true, "date": true,
	"whoami": true, "rg": true, "diff": true, "sort": true, "uniq": true,
}

// findWriteFlag find 的写型/执行型 flag：-delete/-exec*/-ok* 删除与执行、
// -fls/-fprint*（含 -fprint=f 形态）写文件——存在任一即不豁免。
func findWriteFlag(tok string) bool {
	switch tok {
	case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fls":
		return true
	}
	return strings.HasPrefix(tok, "-fprint")
}

// gitListFlags branch/tag/remote 的纯列表 flag 集：三个子命令的列表形态之外
// 都带变更语义（git branch x 即建分支、git tag v1 即打 tag、git remote add
// 即写配置，含 -d/-D/-m/-a 等建改 flag）——位置参数与列表外 flag 一律不豁免
// （安全审查 2026-09-06：白名单曾按子命令名放行，branch -D/tag -d/remote add
// 免审批直过）。
var gitListFlags = map[string]bool{
	"-v": true, "-vv": true, "--verbose": true, "-a": true, "--all": true,
	"-r": true, "--remotes": true, "-l": true, "--list": true, "-n": true,
	"--show-current": true,
}

// safeGitSub git 只读子命令（branch/tag/remote 的参数形态另经 gitListFlags
// 收紧，其余子命令本身只读）。
var safeGitSub = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true, "branch": true,
	"blame": true, "remote": true, "tag": true, "rev-parse": true,
}

// IsSafeReadCommand 参数级审批豁免判定：纯只读命令直过；其余必审批。
// 2026-09-22 增量增强（zcode bash-readonly-policy 对位取两档）：管道分段
// 判定（每段各自过白名单——`grep x | head` 高频形态免审批；空段 = `||`/
// 尾管道等病态形态整体拒）与 env 赋值前缀剥离（`VAR=x git status`——赋值
// 词形态严格 K=V）。其余元字符（组合/重定向/替换/后台）仍一票否决。
// 判定从宽于「无害」从严于「白名单」：白名单程序 + 参数形态只读才豁免
// （写型形态显式拒：find 写型 flag、git branch/tag/remote 非纯列表形态、
// tree 整体）。
func IsSafeReadCommand(args string) bool {
	var in runIn
	if json.Unmarshal([]byte(args), &in) != nil {
		return false // 坏参数 fail-closed
	}
	cmdLine := strings.TrimSpace(in.Command)
	if cmdLine == "" {
		return false
	}
	// 元字符一票否决（| 除外——管道走分段判定）：组合/后台/重定向/替换均不可豁免
	for _, ch := range []string{";", "&", ">", "<", "`", "$(", "(", ")"} {
		if strings.Contains(cmdLine, ch) {
			return false
		}
	}
	for _, seg := range strings.Split(cmdLine, "|") {
		if !readOnlySegment(seg) {
			return false
		}
	}
	return true
}

// isEnvAssign env 赋值词形态（K=V，K 合法 env 名——`VAR=x cmd` 前缀剥离用）。
func isEnvAssign(f string) bool {
	k, _, ok := strings.Cut(f, "=")
	return ok && k != "" && validEnvKey(k)
}

// readOnlySegment 单段纯判定（无元字符前提由调用方保证）：env 赋值前缀
// 剥离后过白名单 + 参数形态校验。空段（`||`/尾管道）拒。
func readOnlySegment(seg string) bool {
	fields := strings.Fields(seg)
	n := 0
	for n < len(fields) && isEnvAssign(fields[n]) {
		n++
	}
	fields = fields[n:]
	if len(fields) == 0 {
		return false // 纯赋值无命令 / 空段——拒绝豁免
	}
	prog := fields[0]
	if prog == "git" {
		if len(fields) < 2 {
			return false
		}
		if !safeGitSub[fields[1]] {
			return false
		}
		switch fields[1] {
		case "branch", "tag", "remote": // 列表形态之外全部收紧
			for _, f := range fields[2:] {
				if !gitListFlags[f] {
					return false
				}
			}
		}
		return true
	}
	if prog == "go" {
		return len(fields) == 2 && fields[1] == "version" // go version 只读；build/test 走审批
	}
	if prog == "python3" || prog == "python" {
		return len(fields) == 2 && fields[1] == "--version"
	}
	if !safeReadOnly[prog] {
		return false
	}
	if prog == "find" {
		for _, f := range fields[1:] {
			if findWriteFlag(f) {
				return false
			}
		}
	}
	return true
}

func fail(msg string) (map[string]any, error) { return tools.Fail(msg), nil } // 信封单点（tools.Fail——审查 P2-11）
