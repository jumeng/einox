//go:build linux && (amd64 || arm64)

// Landlock/seccomp/rlimit 真实测（WSL2 一次性容器——Windows 宿主 go test 被
// Application Control 拦，真源 §6）。围栏语义与工具链冒烟（围栏内合法工作流
// 不死亡与围栏外写被拒同等重要，审查 D2）都在内。
package sandbox

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// sbxDirs 测试目录：工作区与围栏外目标都在 /var/tmp 下（不在 /tmp——tmp 默认
// 可写会让「工作区内写」假绿；非 root 或 /var/tmp 不可用则跳过）。
func sbxDirs(t *testing.T) (ws, outsideFile string) {
	t.Helper()
	base := "/var/tmp/einox-sbx-test"
	if fi, err := os.Lstat("/var/tmp"); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Skipf("/var/tmp 不可用或为符号链接（会误入 /tmp 可写域）: %v", err)
	}
	if err := os.MkdirAll(base, 0o777); err != nil {
		t.Skipf("无法创建测试基目录（非 root）: %v", err)
	}
	ws, err := os.MkdirTemp(base, "ws-")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(base, "out-")
	if err != nil {
		t.Fatal(err)
	}
	outsideFile = f.Name()
	f.Close()
	t.Cleanup(func() {
		os.RemoveAll(ws)
		os.Remove(outsideFile)
	})
	return ws, outsideFile
}

// sbxExec 在沙箱内执行命令行（re-exec 测试二进制自身——TestMain 已挂
// RunHelper），返回合并输出与退出码。
func sbxExec(t *testing.T, pol *Policy, workspace, cmdLine string) (string, int) {
	t.Helper()
	argv, env := ArgvEnv(pol, workspace, cmdLine)
	return sbxRunRaw(t, argv, env, workspace)
}

// sbxChildExec 在沙箱内跑本测试二进制的一个子测试（网络/ptrace 等无 shell
// 工具依赖的验证通道）。
func sbxChildExec(t *testing.T, pol *Policy, workspace, run string) (string, int) {
	t.Helper()
	payload, _ := json.Marshal(policyPayload{Policy: *pol, Workspace: workspace})
	argv := []string{selfExe, sentinelCmd, "--", selfExe, "-test.run=^" + run + "$"}
	env := mergeEnv(os.Environ(), []string{policyEnvKey + "=" + string(payload)})
	return sbxRunRaw(t, argv, env, workspace)
}

func sbxRunRaw(t *testing.T, argv, env []string, dir string) (string, int) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	exit := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("沙箱 re-exec 启动失败: %v\n%s", err, out)
		}
		exit = ee.ExitCode()
	}
	return string(out), exit
}

// TestWorkspaceWriteFence workspace-write 围栏：工作区内写成功、工作区外写拒。
func TestWorkspaceWriteFence(t *testing.T) {
	ws, outside := sbxDirs(t)
	pol := &Policy{Mode: ModeWorkspaceWrite}
	out, exit := sbxExec(t, pol, ws, "echo data > in.txt")
	if exit != 0 {
		t.Fatalf("工作区内写应成功: exit=%d\n%s", exit, out)
	}
	if _, err := os.Stat(filepath.Join(ws, "in.txt")); err != nil {
		t.Fatalf("产物应落工作区: %v", err)
	}
	out, exit = sbxExec(t, pol, ws, "echo x > "+outside)
	if exit == 0 {
		t.Fatal("工作区外写应被拒")
	}
	if DenialHint(out) == "" {
		t.Fatalf("拒绝输出应命中签名（提示标注前提）：%s", out)
	}
}

// TestReadOnlyFence read-only 档全盘只读（工作区内也拒）。
func TestReadOnlyFence(t *testing.T) {
	ws, _ := sbxDirs(t)
	out, exit := sbxExec(t, &Policy{Mode: ModeReadOnly}, ws, "echo x > "+strconv.Quote(filepath.Join(ws, "f.txt")))
	if exit == 0 {
		t.Fatal("read-only 档工作区内写应被拒")
	}
	if DenialHint(out) == "" {
		t.Fatalf("拒绝输出应命中签名：%s", out)
	}
}

// TestTmpExcluded 临时目录两开关：ExcludeSlashTmp=true 时 /tmp 不在可写根。
func TestTmpExcluded(t *testing.T) {
	ws, _ := sbxDirs(t)
	pol := &Policy{Mode: ModeWorkspaceWrite, ExcludeSlashTmp: true}
	out, exit := sbxExec(t, pol, ws, "echo x > /tmp/einox-sbx-exclude-check")
	if exit == 0 {
		t.Fatal("ExcludeSlashTmp 后 /tmp 写应被拒")
	}
	_ = out
	os.Remove("/tmp/einox-sbx-exclude-check")
	// 对照：默认（false）可写
	out2, exit2 := sbxExec(t, &Policy{Mode: ModeWorkspaceWrite}, ws, "echo x > /tmp/einox-sbx-default-check")
	if exit2 != 0 {
		t.Fatalf("/tmp 默认应可写（审查 A1）: exit=%d\n%s", exit2, out2)
	}
	os.Remove("/tmp/einox-sbx-default-check")
}

// TestTmpdirExcluded ExcludeTmpdir=true 时 $TMPDIR 不在可写根（默认计入）。
func TestTmpdirExcluded(t *testing.T) {
	ws, _ := sbxDirs(t)
	td, err := os.MkdirTemp("/var/tmp", "sbx-td-")
	if err != nil {
		t.Skipf("无法建 TMPDIR 目录: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(td) })
	t.Setenv("TMPDIR", td)
	target := filepath.Join(td, "probe")
	if out, exit := sbxExec(t, &Policy{Mode: ModeWorkspaceWrite, ExcludeTmpdir: true}, ws, "echo x > "+target); exit == 0 {
		t.Fatalf("ExcludeTmpdir 后 $TMPDIR 写应被拒: exit=0\n%s", out)
	}
	if out, exit := sbxExec(t, &Policy{Mode: ModeWorkspaceWrite}, ws, "echo x > "+target); exit != 0 {
		t.Fatalf("$TMPDIR 默认应可写: exit=%d\n%s", exit, out)
	}
	os.Remove(target)
}

// TestDangerFullAccessFence danger 档不加 fs 围栏——围栏外写应成功（正向锚，
// 防过度围栏回归；真源 §1.1「仅资源限额与可选断网生效」）。
func TestDangerFullAccessFence(t *testing.T) {
	ws, outside := sbxDirs(t)
	out, exit := sbxExec(t, &Policy{Mode: ModeDangerFullAccess}, ws, "echo x > "+outside)
	if exit != 0 {
		t.Fatalf("danger 档围栏外写应成功: exit=%d\n%s", exit, out)
	}
	if b, err := os.ReadFile(outside); err != nil || strings.TrimSpace(string(b)) != "x" {
		t.Fatalf("围栏外产物应落位: %v %q", err, string(b))
	}
}

// TestWritableRootUnopenable 规则路径不可开 fail-closed（非 optional 路径
// O_PATH 失败 → helper 125 拒 exec，静默收窄授予面不可接受）。
func TestWritableRootUnopenable(t *testing.T) {
	ws, _ := sbxDirs(t)
	pol := &Policy{Mode: ModeWorkspaceWrite, WritableRoots: []string{"/no/such/einox-root"}}
	out, exit := sbxExec(t, pol, ws, "echo hi")
	if exit != helperFail || !strings.Contains(out, "规则路径不可开") {
		t.Fatalf("不可开可写根应 fail-closed（125+报错）: exit=%d\n%s", exit, out)
	}
}

// TestSandboxEnvCleansed 沙箱命令不见敏感 env（审查 C-3：LLM_* 凭证面与
// 策略载荷剔除，普通 env 保留）。
func TestSandboxEnvCleansed(t *testing.T) {
	ws, _ := sbxDirs(t)
	t.Setenv("LLM_API_KEY", "secret-should-not-leak")
	t.Setenv("SBX_MARKER", "kept")
	out, exit := sbxExec(t, &Policy{Mode: ModeWorkspaceWrite}, ws,
		`test -z "$LLM_API_KEY" && test "$SBX_MARKER" = kept && echo CLEANSE-OK`)
	if exit != 0 || !strings.Contains(out, "CLEANSE-OK") {
		t.Fatalf("LLM_* 应剔除、普通 env 应保留: exit=%d\n%s", exit, out)
	}
}

// TestSandboxSocketUnixAllowed 断网档 socket(AF_UNIX) 创建放行（AF_UNIX 条件
// 规则白名单侧的正向锚——socketpair/本地 IPC 工具兼容性）。
func TestSandboxSocketUnixAllowed(t *testing.T) {
	ws, _ := sbxDirs(t)
	out, exit := sbxChildExec(t, &Policy{Mode: ModeWorkspaceWrite}, ws, "TestChildSocketUnixAllowed")
	if exit != 0 {
		t.Fatalf("子测试应通过（断言 AF_UNIX socket 放行）: exit=%d\n%s", exit, out)
	}
}

// TestChildSocketUnixAllowed 子模式：socket(AF_UNIX) 不被 seccomp 拒。
func TestChildSocketUnixAllowed(t *testing.T) {
	if os.Getenv(policyEnvKey) == "" {
		t.Skip("沙箱子模式专用")
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket(AF_UNIX) 应放行: %v", err)
	}
	unix.Close(fd)
}

// TestSandboxConnectUnixDenied 断网档 AF_UNIX connect 也断（D3 披露：docker
// CLI / psql -h /sock 类本地 socket 客户端同断——防 docker.sock 逃逸的故意设计）。
func TestSandboxConnectUnixDenied(t *testing.T) {
	ws, _ := sbxDirs(t)
	out, exit := sbxChildExec(t, &Policy{Mode: ModeWorkspaceWrite}, ws, "TestChildConnectUnixDenied")
	if exit != 0 {
		t.Fatalf("子测试应通过（断言 AF_UNIX connect 被拒）: exit=%d\n%s", exit, out)
	}
}

// TestChildConnectUnixDenied 子模式：socket(AF_UNIX) 放行但 connect EPERM。
func TestChildConnectUnixDenied(t *testing.T) {
	if os.Getenv(policyEnvKey) == "" {
		t.Skip("沙箱子模式专用")
	}
	c, err := net.Dial("unix", "/tmp/einox-no-such.sock")
	if c != nil {
		c.Close()
		t.Fatal("connect 不应成功")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not permitted") {
		t.Fatalf("connect 应为 EPERM 拒绝，实际: %v", err)
	}
}

// TestSandboxNetworkDenied 断网：子测试进程内 connect/socket(AF_INET) 全拒
// （seccomp EPERM）。
func TestSandboxNetworkDenied(t *testing.T) {
	ws, _ := sbxDirs(t)
	out, exit := sbxChildExec(t, &Policy{Mode: ModeWorkspaceWrite}, ws, "TestChildNetworkDenied")
	if exit != 0 {
		t.Fatalf("子测试应通过（断言网络被拒）: exit=%d\n%s", exit, out)
	}
}

// TestChildNetworkDenied 子模式（policyEnv 在位时才有意义——沙箱内 exec 的
// 本二进制）。
func TestChildNetworkDenied(t *testing.T) {
	if os.Getenv(policyEnvKey) == "" {
		t.Skip("沙箱子模式专用")
	}
	_, err := net.DialTimeout("tcp", "127.0.0.1:1", 2*time.Second)
	if err == nil {
		t.Fatal("socket/connect 应被 seccomp 拒绝")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not permitted") {
		t.Fatalf("应为 EPERM 拒绝，实际: %v", err)
	}
}

// TestSandboxPtraceDenied 无条件 deny：ptrace 拒（子进程模式；断网档 filter
// 内——Network=true 时整个 filter 不装，codex 同款语义）。
func TestSandboxPtraceDenied(t *testing.T) {
	ws, _ := sbxDirs(t)
	out, exit := sbxChildExec(t, &Policy{Mode: ModeWorkspaceWrite}, ws, "TestChildPtrace")
	if exit != 0 {
		t.Fatalf("子测试应通过（断言 ptrace 被拒）: exit=%d\n%s", exit, out)
	}
}

// TestChildPtrace 子模式：ptrace 在无条件 deny 表（filter 装设时）。
func TestChildPtrace(t *testing.T) {
	if os.Getenv(policyEnvKey) == "" {
		t.Skip("沙箱子模式专用")
	}
	sleep := exec.Command("sh", "-c", "sleep 10")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	defer sleep.Process.Kill()
	err := unix.PtraceAttach(sleep.Process.Pid)
	if err == nil {
		t.Fatal("ptrace 应被无条件 deny")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not permitted") {
		t.Fatalf("应为 EPERM 拒绝，实际: %v", err)
	}
}

// TestSandboxFileSize RLIMIT_FSIZE 触发（1MB 上限写 3MB）。
func TestSandboxFileSize(t *testing.T) {
	ws, _ := sbxDirs(t)
	pol := &Policy{Mode: ModeWorkspaceWrite, Limit: Limit{FileSizeMB: 1}}
	out, exit := sbxExec(t, pol, ws, "head -c 3000000 /dev/zero > big.bin")
	if exit == 0 {
		t.Fatal("超 FSIZE 写应失败")
	}
	// 产物被钳在限额附近（防写满语义成立）
	fi, err := os.Stat(filepath.Join(ws, "big.bin"))
	if err == nil && fi.Size() > 2<<20 {
		t.Fatalf("文件应被钳在 ~1MB：%d", fi.Size())
	}
	_ = out
}

// TestSandboxTmpSmoke 临时目录工具链冒烟：mktemp + tar -C（/tmp 默认可写
// 后 tar/mktemp 族不死，审查 A1/D2）。
func TestSandboxTmpSmoke(t *testing.T) {
	ws, _ := sbxDirs(t)
	cmd := `P=$(mktemp -p /tmp einox-tar-XXXX) && T=$(mktemp) && echo hi > "$T" && tar -cf "$P" -C "$(dirname "$T")" "$(basename "$T")" && echo SMOKE-OK`
	out, exit := sbxExec(t, &Policy{Mode: ModeWorkspaceWrite}, ws, cmd)
	if exit != 0 || !strings.Contains(out, "SMOKE-OK") {
		t.Fatalf("mktemp+tar 冒烟应通过: exit=%d\n%s", exit, out)
	}
}

// TestSandboxGoBuildSmoke go build 冒烟（缓存重定向配方 = 审查 A2 保命件）：
// GOCACHE 指向围栏内可写根（WritableRoots），围栏内零依赖构建跑通。
func TestSandboxGoBuildSmoke(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("无 go 工具链")
	}
	ws, _ := sbxDirs(t)
	cache, err := os.MkdirTemp("/var/tmp", "sbx-cache-")
	if err != nil {
		t.Skipf("无法建围栏内缓存目录: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(cache) })
	os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module smoke\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nfunc main() { println(\"smoke-ok\") }\n"), 0o644)
	pol := &Policy{
		Mode:          ModeWorkspaceWrite,
		WritableRoots: []string{cache},
		Env: []string{
			"GOCACHE=" + filepath.Join(cache, "go-build"),
			"GOMODCACHE=" + filepath.Join(cache, "go-mod"),
			"GOPATH=" + filepath.Join(cache, "gopath"),
			"GOTOOLCHAIN=local",
		},
	}
	out, exit := sbxExec(t, pol, ws, "go build .")
	if exit != 0 {
		t.Fatalf("围栏内 go build 应跑通（缓存已重定向）: exit=%d\n%s", exit, out)
	}
	if _, err := os.Stat(filepath.Join(ws, "smoke")); err != nil {
		t.Fatalf("构建产物应存在: %v", err)
	}
}

// TestSandboxNpmSmoke npm install 冒烟（容器有 node 则跑，无则记遗留——真源
// §6 工具链冒烟）。
func TestSandboxNpmSmoke(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("容器无 node/npm——记遗留（真源 §6：容器有 node 则跑）")
	}
	ws, _ := sbxDirs(t)
	cache, err := os.MkdirTemp("/var/tmp", "sbx-npm-")
	if err != nil {
		t.Skipf("无法建围栏内缓存目录: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(cache) })
	os.WriteFile(filepath.Join(ws, "package.json"), []byte(`{"name":"smoke","version":"1.0.0","dependencies":{"is-odd":"1.0.0"}}`), 0o644)
	pol := &Policy{
		Mode:          ModeWorkspaceWrite,
		Network:       true, // 依赖安装需网络（真源 §5.2 D3：内网形态启用须 PM_SANDBOX_NET=on）
		WritableRoots: []string{cache},
		Env:           []string{"npm_config_cache=" + filepath.Join(cache, "npm")},
	}
	out, exit := sbxExec(t, pol, ws, "npm install --no-audit --no-fund --registry=https://registry.npmmirror.com")
	if exit != 0 {
		// 分流（审查 C-2 防静默空转）：容器网络不可达 → Skip 记遗留；非网络
		// 因素失败 → Fatal（疑似沙箱回归——冒烟测试的意义所在）。
		if npmNetUnreachable(out) {
			t.Skipf("npm install 网络不可达（记遗留）: exit=%d\n%s", exit, out)
		}
		t.Fatalf("npm install 围栏内失败（非网络因素——疑似沙箱回归）: exit=%d\n%s", exit, out)
	}
	if _, err := os.Stat(filepath.Join(ws, "node_modules")); err != nil {
		t.Fatalf("node_modules 应存在: %v（%s）", err, out)
	}
}

// npmNetUnreachable npm 网络不可达特征（与围栏内失败区分——后者是回归信号）。
func npmNetUnreachable(out string) bool {
	l := strings.ToLower(out)
	for _, s := range []string{"enotfound", "etimedout", "econnrefused", "econnreset",
		"eai_again", "getaddrinfo", "network"} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// TestProbeStatusLinux probe 三态：内核可用时 full/partial 且 partial 必带
// 未覆盖项；不可用如实报（skip 并给出部署前提指路）。unusable→skip 的静默面
// 由 fence 测试族兜底（探测不可用时 helper 125 → 围栏测试 Fatal——真回归仍有
// 报警面，审查 C-7 注记）。
func TestProbeStatusLinux(t *testing.T) {
	st := probeBackend()
	t.Logf("探测结果: %+v", st)
	if st.Enforcement == EnforcementUnusable {
		t.Skipf("内核 Landlock 不可用: %s（部署前提见 docs/09 §7：内核 ≥5.13 + 容器 seccomp 放行 landlock_*）", st.Detail)
	}
	if st.Enforcement == EnforcementPartial && len(st.Uncovered) == 0 {
		t.Fatal("partial 必须带未覆盖项清单")
	}
}

// TestProbeUnhooked 未挂钩形态：selfExe 换成未挂 RunHelper 的正常程序——
// 哨兵握手应失败且报「产品 main 未挂 sandbox.RunHelper 钩子」（C1）。
func TestProbeUnhooked(t *testing.T) {
	old := selfExe
	t.Cleanup(func() { selfExe = old })
	if _, err := os.Stat("/bin/echo"); err == nil {
		selfExe = "/bin/echo" // 正常 main 吞掉子命令退出 0——靠握手标记甄别
	} else {
		selfExe = "/einox-no-such-bin" // exec 失败形态
	}
	st := probeBackend()
	if st.Enforcement != EnforcementUnusable || !strings.Contains(st.Detail, "RunHelper") {
		t.Fatalf("未挂钩应 unusable 且指明钩子缺失：%+v", st)
	}
}

// TestKillGroup 进程组杀：组长+孙进程整组终结（/proc 扫描无残留同 pgid 进程）。
func TestKillGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & sleep 30")
	SetGroupLeader(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	KillGroup(cmd.Process)
	_, _ = cmd.Process.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !procGroupAlive(pgid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("进程组未整组终结（仍有 pgid=%d 残留）", pgid)
}

// procGroupAlive /proc 扫描是否存在指定进程组的存活进程。
func procGroupAlive(pgid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(data)
		i := strings.LastIndex(s, ")")
		if i < 0 {
			continue
		}
		fields := strings.Fields(s[i+1:]) // state ppid pgrp ...
		if len(fields) >= 3 {
			if p, err := strconv.Atoi(fields[2]); err == nil && p == pgid {
				return true
			}
		}
	}
	return false
}

// TestSeccompProgSemantics BPF 过滤程序语义回归（审查 D-7 人工展开的测试固化
// ——mini 解释器遍历指令序列，锚定各类输入的裁决落点，jt/jf 回填错位即抓）。
func TestSeccompProgSemantics(t *testing.T) {
	arch, err := seccompArch()
	if err != nil {
		t.Skipf("非 seccomp 支持架构: %v", err)
	}
	prog, err := buildSeccompProg(arch)
	if err != nil {
		t.Fatal(err)
	}
	// runBpf 单输入裁决（archVal 可注入不符值测架构门）。
	runBpf := func(archVal, nr, arg0 uint32) uint32 {
		pc, acc := 0, uint32(0)
		for steps := 0; steps < len(prog); steps++ {
			ins := prog[pc]
			switch ins.Code {
			case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
				switch ins.K {
				case sdArch:
					acc = archVal
				case sdNr:
					acc = nr
				case sdArg0:
					acc = arg0
				default:
					t.Fatalf("未预期加载偏移 %#x", ins.K)
				}
			case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
				if acc == ins.K {
					pc += int(ins.Jt)
				} else {
					pc += int(ins.Jf)
				}
			case unix.BPF_RET | unix.BPF_K:
				return ins.K
			default:
				t.Fatalf("未实现指令形态 %#x @%d", ins.Code, pc)
			}
			pc++
		}
		t.Fatal("BPF 程序未终止（控制流缺兜底）")
		return 0
	}
	ep := uint32(unix.SECCOMP_RET_ERRNO | unix.EPERM)
	cases := []struct {
		name     string
		nr, arg0 uint32
		want     uint32
	}{
		{"读类放行", uint32(unix.SYS_READ), 0, unix.SECCOMP_RET_ALLOW},
		{"ptrace 无条件拒", uint32(unix.SYS_PTRACE), 0, ep},
		{"connect 断网拒", uint32(unix.SYS_CONNECT), 0, ep},
		{"recvfrom 故意放行", uint32(unix.SYS_RECVFROM), 0, unix.SECCOMP_RET_ALLOW},
		{"socket AF_UNIX 放行", uint32(unix.SYS_SOCKET), uint32(unix.AF_UNIX), unix.SECCOMP_RET_ALLOW},
		{"socket AF_INET 拒", uint32(unix.SYS_SOCKET), uint32(unix.AF_INET), ep},
		{"socketpair 非 AF_UNIX 拒", uint32(unix.SYS_SOCKETPAIR), uint32(unix.AF_INET), ep},
	}
	for _, c := range cases {
		if got := runBpf(arch, c.nr, c.arg0); got != c.want {
			t.Errorf("%s: 裁决错 got=%#x want=%#x", c.name, got, c.want)
		}
	}
	// 架构不符（32 位兼容模式号表错位）→ KILL_PROCESS（fail-closed 方向）。
	if got := runBpf(arch^0x1, uint32(unix.SYS_READ), 0); got != unix.SECCOMP_RET_KILL_PROCESS {
		t.Errorf("架构不符应 KILL_PROCESS，实际 %#x", got)
	}
}

func TestProtectedReadOnlyNote(t *testing.T) {
	if protectedReadOnlyNote(&Policy{Mode: ModeWorkspaceWrite}) != "" {
		t.Fatal("未设 ProtectedReadOnly 不应告警")
	}
	note := protectedReadOnlyNote(&Policy{Mode: ModeWorkspaceWrite, ProtectedReadOnly: []string{".git"}})
	if !strings.Contains(note, "protected-readonly") || !strings.Contains(note, ".git") {
		t.Fatalf("告警应含未覆盖项语义与路径清单：%s", note)
	}
}
