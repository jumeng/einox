// re-exec 哨兵协议（真源 §2.1）：run_command 侧以 [<exe>, "__einox-sandbox",
// "--", "sh", "-c", <cmd>] re-exec 产品自身，产品 main() 顶部挂 RunHelper
// 拦截哨兵子命令——helper 路径内施加策略（LockOSThread → NO_NEW_PRIVS →
// seccomp → Landlock → rlimit）后 syscall.Exec 真实命令。策略 JSON 经 env
// 传递（不污染 ps 输出）；exe 路径 init 期 os.Executable() 固化（审查 C2：
// os.Args[0] 相对形态在子进程 cwd 下解析落空）。依赖产品 main 挂钩是本机制
// 唯一侵入点（一行）——未挂钩时探测报 unusable 而非静默裸跑（审查 C1）。
package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// 哨兵协议常量。
const (
	sentinelCmd    = "__einox-sandbox" // 子命令位标记（不与产品子命令冲突）
	flagProbe      = "--probe"         // 哨兵握手（不施加任何限制，C1）
	flagProbeEnf   = "--probe-enforce" // 后端实测（子进程自施加后报告三态）
	flagProbeWrite = "--probe-write"   // 围栏写探针（darwin/windows probe-enforce 的被测替身）
	policyEnvKey   = "EINOX_SANDBOX_POLICY"
	probeMarker    = "einox-sandbox-helper-ready"
	helperFail     = 125 // helper 自身失败退出码（dsh 同款：与命令自身退出码可区分）
)

// selfExe 本体路径（init 期固化）。
var selfExe = func() string {
	if p, err := os.Executable(); err == nil && p != "" {
		return p
	}
	return os.Args[0]
}()

// policyPayload env 载荷：Policy 全量 + 会话工作区（helper 侧 Landlock 规则
// 生成需要；read-only 档工作区仍只读）。
type policyPayload struct {
	Policy
	Workspace string `json:"workspace,omitempty"`
}

// RunHelper 产品 main() 顶部挂接：命中哨兵子命令进入 helper 路径永不返回，
// 未命中原样返回（正常产品流程）。
func RunHelper(args []string) {
	if len(args) < 2 || args[1] != sentinelCmd {
		return
	}
	switch {
	case len(args) >= 3 && args[2] == flagProbe:
		fmt.Println(probeMarker) // 纯握手：证明 main 已挂钩
		os.Exit(0)
	case len(args) >= 3 && args[2] == flagProbeEnf:
		os.Exit(probeEnforceChild())
	case len(args) >= 4 && args[2] == flagProbeWrite:
		os.Exit(probeWriteChild(args[3]))
	case len(args) >= 4 && args[2] == "--":
		helperExec(args[3:])
	}
	fmt.Fprintln(os.Stderr, "einox-sandbox: 参数形态不符")
	os.Exit(helperFail)
}

// helperExec 施加序（真源 §2.1：LockOSThread → NO_NEW_PRIVS → seccomp →
// Landlock → rlimit → Exec）后 exec 真实命令——成功不返回。
func helperExec(argv []string) {
	runtime.LockOSThread()
	raw := os.Getenv(policyEnvKey)
	if raw == "" {
		helperFailf("env %s 缺失（载荷由 ArgvEnv/Wrap 注入）", policyEnvKey)
	}
	var p policyPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		helperFailf("策略载荷解码失败: %v", err)
	}
	if err := p.Validate(); err != nil {
		helperFailf("%v", err)
	}
	if err := applySandbox(&p); err != nil {
		helperFailf("施加失败: %v", err)
	}
	if err := syscallExec(argv, cleanseEnv(os.Environ())); err != nil {
		helperFailf("exec 失败: %v", err)
	}
}

func helperFailf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "einox-sandbox: "+format+"\n", args...)
	os.Exit(helperFail)
}

// 探测缓存（进程级一次）。
var (
	probeOnce sync.Once
	probeStat Status
)

// Probe 见下方 OSProvider 出口（进程级缓存，装配期调用一次即可让告警进
// 启动日志；序 = 哨兵握手 → 后端实测，真源 §1.3）。

// probeBackend 探测序（不缓存——测试可复调）：①哨兵握手自检（失败 = 产品
// main 未挂 RunHelper 钩子；内核 ABI 探测只证内核支持，不证哨兵在位）→
// ②后端实测（re-exec 自施加）。auto 序止于 OS 级后端——Docker daemon 探测
// 随 Docker 后端落地再接（真源 §5.3 出界项）。
func probeBackend() Status {
	out, err := exec.Command(selfExe, sentinelCmd, flagProbe).Output()
	if err != nil || !bytes.Contains(out, []byte(probeMarker)) {
		detail := fmt.Sprintf("%v", err)
		if err == nil {
			detail = "应答不含握手标记"
		}
		return Status{
			Enforcement: EnforcementUnusable,
			Detail:      "产品 main 未挂 sandbox.RunHelper 钩子（哨兵握手失败: " + detail + "）",
		}
	}
	return probeOSBackend()
}

// probeOSBackend 后端实测：re-exec --probe-enforce 子进程自施加后报告
// （「--version 式检查会漏有 syscall 但拒执行的内核」，dsh probe 哲学）。
// 约定行：einox-sandbox-enforce full|partial abi=N [uncovered=a,b]——
// 第 4 段可选（windows 后端网络禁断不支持，自报未覆盖项；Linux 侧维持
// 三段式由 ABI 推导）。
func probeOSBackend() Status {
	cmd := exec.Command(selfExe, sentinelCmd, flagProbeEnf)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Status{
			Enforcement: EnforcementUnusable,
			Detail:      fmt.Sprintf("后端实测失败（exit %v）: %s", err, strings.TrimSpace(stderr.String())),
		}
	}
	fields := strings.Fields(strings.TrimSpace(stdout.String()))
	if len(fields) < 3 || fields[0] != "einox-sandbox-enforce" {
		return Status{Enforcement: EnforcementUnusable, Detail: "后端实测应答不符: " + strings.TrimSpace(stdout.String())}
	}
	var abi int
	if _, err := fmt.Sscanf(fields[2], "abi=%d", &abi); err != nil {
		return Status{Enforcement: EnforcementUnusable, Detail: "后端实测应答 ABI 解析失败: " + fields[2]}
	}
	switch fields[1] {
	case "full":
		return Status{Enforcement: EnforcementFull, Detail: strings.Join(fields, " ")}
	case "partial":
		uncovered := abiUncovered(abi)
		if len(fields) >= 4 && strings.HasPrefix(fields[3], "uncovered=") {
			uncovered = strings.Split(strings.TrimPrefix(fields[3], "uncovered="), ",")
		}
		return Status{Enforcement: EnforcementPartial, Uncovered: uncovered, Detail: strings.Join(fields, " ")}
	}
	return Status{Enforcement: EnforcementUnusable, Detail: "后端实测状态未知: " + fields[1]}
}

// probeWriteChild 围栏写探针替身（darwin/windows 的 probe-enforce 在围栏内
// 运行本替身判定围栏是否真实拒绝写）：退出码 0=写成功 / 1=写失败。
func probeWriteChild(path string) int {
	if err := os.WriteFile(path, []byte("einox-sandbox-probe"), 0o644); err != nil {
		return 1
	}
	return 0
}

// abiUncovered 旧 ABI 未治理位（真源 §2.2：不在 handled 集的访问即不受围栏）。
// refer 例外：REFER 是唯一「未 handle 也默认拒」的位（ABI1 规则集永远拒跨目录
// reparent）——上报口径偏保守（过度报告无安全影响，审查 C-6 注记）。
func abiUncovered(abi int) []string {
	var out []string
	if abi < 2 {
		out = append(out, "refer") // 跨目录 rename/link
	}
	if abi < 3 {
		out = append(out, "truncate")
	}
	if abi < 5 {
		out = append(out, "ioctl-dev")
	}
	return out
}

// Wrap 探测后构造沙箱化执行参数（OSProvider 的包级出口——应用直用
// runcommand/sandbox 不经 engine 时的既有入口，行为不变）。
func Wrap(pol *Policy, workspace, cmdLine string) (argv []string, env []string) {
	return OSProvider.Wrap(pol, workspace, cmdLine)
}

// Probe OSProvider 的包级出口（既有入口，行为不变）。
func Probe() Status { return OSProvider.Probe() }

// OSProvider 平台内建后端（默认 Provider：Linux Landlock+seccomp+rlimit /
// darwin Seatbelt / windows restricted token，构建标签分流——wrapOSBackend
// 平台实现）。ProtectedReadOnly 告警在 Linux 分支发——Landlock 做不到可写区
// 内回盖（审查 B-2），darwin/windows 后端可真回盖（require-not 排除 /
// deny ACE），不发该告警。
var OSProvider Provider = osProvider{}

// osProvider 平台内建后端实现（Wrap/Probe 主体 = 既有自由函数收拢）。
type osProvider struct{}

func (osProvider) Wrap(pol *Policy, workspace, cmdLine string) ([]string, []string) {
	if Probe().Enforcement == EnforcementUnusable {
		return nil, nil // auto 档裸跑降级（探测已告警一次——拒跑须显式 require，真源 §10.4 接线留待）
	}
	return wrapOSBackend(pol, workspace, cmdLine)
}

func (osProvider) Probe() Status { return probeOS() }

// probeOS 进程级探测缓存（既有 Probe 主体，收拢不改语义）。
func probeOS() Status {
	probeOnce.Do(func() {
		probeStat = probeBackend()
		if probeStat.Enforcement == EnforcementUnusable {
			log.Printf("einox-sandbox: 后端不可用（%s）——auto 档命令将裸跑；拒跑须显式 require（真源 §1.3）", probeStat.Detail)
		}
	})
	return probeStat
}

// ArgvEnv 纯构造（不探测——argv/env 形态测试与特殊装配用）。
func ArgvEnv(pol *Policy, workspace, cmdLine string) (argv []string, env []string) {
	argv = []string{selfExe, sentinelCmd, "--", "sh", "-c", cmdLine}
	return argv, payloadEnv(pol, workspace)
}

// payloadEnv 策略载荷 + Policy.Env 合并环境（哨兵协议统一 env 形态；
// darwin 的 sandbox-exec 包裹形同样经哨兵，共用本载体）。基础环境经
// baseEnv 分档（EnvMode：inherit 全继承 / minimal 白名单）。
func payloadEnv(pol *Policy, workspace string) []string {
	payload, _ := json.Marshal(policyPayload{Policy: *pol, Workspace: workspace})
	extra := append([]string{policyEnvKey + "=" + string(payload)}, pol.Env...)
	return mergeEnv(baseEnv(pol), extra)
}

// minimalEnvKeys 围栏内环境白名单（EnvMinimal 档；键名大小写不敏感——
// windows 环境键名惯例异形）。PATH/HOME/TMPDIR 覆盖 unix 主场景；TEMP/TMP/
// SystemRoot/USERPROFILE/HOMEDRIVE/HOMEPATH 覆盖 windows（SystemRoot 是 Go
// 程序与大量 syscall 的硬前提）。其余环境（凭据面在内）默认不进围栏——
// 业务所需经 Policy.Env 显式注入。
var minimalEnvKeys = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true,
	"TEMP": true, "TMP": true, "SYSTEMROOT": true,
	"USERPROFILE": true, "HOMEDRIVE": true, "HOMEPATH": true,
}

// baseEnv 围栏内基础环境（EnvMode 分档；windows 直执行分支同用）。
func baseEnv(pol *Policy) []string {
	if pol.EnvMode != EnvMinimal {
		return os.Environ() // 缺省 inherit：全继承（cleanseEnv 剥 LLM_*/载荷）
	}
	var out []string
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && minimalEnvKeys[strings.ToUpper(k)] {
			out = append(out, kv)
		}
	}
	return out
}

// mergeEnv 环境合并——同名键后者覆盖（env 数组重复键首见生效，必须去重：
// 父进程已设 GOCACHE 时 Policy.Env 的重定向值才真正生效）。
func mergeEnv(base, extra []string) []string {
	keys := make(map[string]bool, len(extra))
	for _, kv := range extra {
		if k, _, ok := strings.Cut(kv, "="); ok {
			keys[k] = true
		}
	}
	var out []string
	for _, kv := range base {
		if k, _, ok := strings.Cut(kv, "="); ok && keys[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// cleanseEnv 剔除继承环境中的敏感载荷（审查 C-3）：LLM_* 凭证面（llm env
// 逃生门族，密钥不随沙箱命令下传）与哨兵策略载荷（施加使命已完成）。
// Policy.Env 的显式注入不受影响（应用自担）。
func cleanseEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok &&
			(k == policyEnvKey || strings.HasPrefix(k, "LLM_")) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
