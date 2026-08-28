package runcommand

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jumeng/einox/sandbox"
)

// TestMain 沙箱探测握手宿主：Wrap 探测会 re-exec 本测试二进制——挂上
// RunHelper 才是「产品 main 已挂钩」形态（真源 §2.1.3）。
func TestMain(m *testing.M) {
	sandbox.RunHelper(os.Args)
	os.Exit(m.Run())
}

// TestBuildCmdDefaultOff 默认关 = 行为等价（真源红线③：Sandbox == nil 时
// run_command 执行路径与既有直执行形态一致——sh -c、无 env 覆盖、无
// SysProcAttr）。
func TestBuildCmdDefaultOff(t *testing.T) {
	t.Setenv("EINO_RUN_DOCKER", "")
	cmd, sandboxed := buildCmd(context.Background(), "/ws", nil, "echo hi")
	if sandboxed {
		t.Fatal("nil 策略不应走沙箱")
	}
	if len(cmd.Args) != 3 || cmd.Args[0] != "sh" || cmd.Args[1] != "-c" || cmd.Args[2] != "echo hi" {
		t.Fatalf("默认应直执行 sh -c：%v", cmd.Args)
	}
	if cmd.Env != nil || cmd.SysProcAttr != nil {
		t.Fatalf("默认路径不应动 env/SysProcAttr：%v %v", cmd.Env, cmd.SysProcAttr)
	}
}

// TestBuildCmdDockerPriority 三分支优先级定死（审查 C3）：dockerWrap 显式
// 启用 > 沙箱。
func TestBuildCmdDockerPriority(t *testing.T) {
	t.Setenv("EINO_RUN_DOCKER", "1")
	cmd, sandboxed := buildCmd(context.Background(), "/ws",
		&sandbox.Policy{Mode: sandbox.ModeWorkspaceWrite}, "echo hi")
	if sandboxed || cmd.Args[0] != "docker" {
		t.Fatalf("dockerWrap 应优先于沙箱：%v", cmd.Args)
	}
}

// TestSandboxArgvEnv 沙箱 argv/env 纯构造（TestDockerWrapArgv 模式——不探测
// 不 exec 跨平台可跑）：re-exec 形态 + 同名键去重覆盖 + 载荷可解码往返。
func TestSandboxArgvEnv(t *testing.T) {
	t.Setenv("EINO_RUN_DOCKER", "")
	t.Setenv("GOCACHE", "/old-value") // 父进程已有同键——Policy.Env 必须覆盖
	pol := &sandbox.Policy{
		Mode: sandbox.ModeWorkspaceWrite,
		Env:  []string{"GOCACHE=/cache/go-build", "GOMODCACHE=/cache/go-mod"},
	}
	argv, env := sandbox.ArgvEnv(pol, "/ws", "echo hi")
	if len(argv) != 6 || argv[1] != "__einox-sandbox" || argv[2] != "--" ||
		argv[3] != "sh" || argv[4] != "-c" || argv[5] != "echo hi" {
		t.Fatalf("re-exec argv 形态错：%v", argv)
	}
	goc, payload := 0, ""
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GOCACHE="):
			goc++
			if kv != "GOCACHE=/cache/go-build" {
				t.Fatalf("GOCACHE 应为重定向值：%s", kv)
			}
		case strings.HasPrefix(kv, "EINOX_SANDBOX_POLICY="):
			payload = strings.TrimPrefix(kv, "EINOX_SANDBOX_POLICY=")
		}
	}
	if goc != 1 {
		t.Fatalf("GOCACHE 应去重为一项：%d", goc)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		t.Fatalf("载荷应可解码：%v（%s）", err, payload)
	}
	if m["Mode"] != "workspace-write" || m["workspace"] != "/ws" {
		t.Fatalf("载荷字段错：%v", m)
	}
}

// TestNewToolsInvalidSandbox 策略非法构造期拒绝（fail-fast 而非静默裸跑）。
func TestNewToolsInvalidSandbox(t *testing.T) {
	if _, err := NewTools(Config{Root: t.TempDir(), Sandbox: &sandbox.Policy{Mode: "weird"}}); err == nil {
		t.Fatal("非法模式应构造失败")
	}
}
