package sandbox

// BuildCommand require 姿态接线(D1)纯逻辑测试:require + 不可用后端拒跑、
// auto 裸跑、argv quote 往返。平台真实围栏测试见 sandbox_linux_test.go。

import (
	"context"
	"strings"
	"testing"
)

// fakeBackend 注入桩(可控可用性)。
type fakeBackend struct {
	usable bool
	probed bool
}

func (f *fakeBackend) Wrap(pol *Policy, workspace, cmdLine string) ([]string, []string) {
	if !f.usable {
		return nil, nil
	}
	return []string{"echo", "WRAPPED", cmdLine}, nil
}

func (f *fakeBackend) Probe() Status {
	f.probed = true
	return Status{Enforcement: EnforcementUnusable, Detail: "fake 不可用"}
}

// TestBuildCommandRequireFailClosed require 姿态:Wrap 不可用 → 拒跑(不构造
// 命令);auto(缺省)同条件裸跑直执行。
func TestBuildCommandRequireFailClosed(t *testing.T) {
	ctx := context.Background()
	down := &fakeBackend{usable: false}

	_, _, err := BuildCommand(ctx, "/ws", "/ws",
		&Policy{Mode: ModeWorkspaceWrite, Backend: BackendRequire}, down,
		[]string{"sh", "-c", "echo hi"}, nil)
	if err == nil || !strings.Contains(err.Error(), "require") || !strings.Contains(err.Error(), "拒绝执行") {
		t.Fatalf("require 不可用应拒跑：%v", err)
	}
	if !down.probed {
		t.Fatal("拒跑文案应附探测诊断（Probe 已调）")
	}

	// auto（缺省零值同义）：同条件裸跑直执行（不拒）
	cmd, sandboxed, err := BuildCommand(ctx, "/ws", "/ws",
		&Policy{Mode: ModeWorkspaceWrite}, down,
		[]string{"sh", "-c", "echo hi"}, nil)
	if err != nil || sandboxed || cmd.Args[0] != "sh" {
		t.Fatalf("auto 不可用应裸跑：%v %v %v", cmd.Args, sandboxed, err)
	}

	// 后端可用：require 照常走沙箱（argv 变 fake 包装形态）
	up := &fakeBackend{usable: true}
	cmd, sandboxed, err = BuildCommand(ctx, "/ws", "/ws",
		&Policy{Mode: ModeWorkspaceWrite, Backend: BackendRequire}, up,
		[]string{"sh", "-c", "echo hi"}, nil)
	if err != nil || !sandboxed || cmd.Args[0] != "echo" {
		t.Fatalf("可用后端 require 应走沙箱：%v %v %v", cmd.Args, sandboxed, err)
	}
}

// TestPolicyBackendValidate Backend 取值校验：空/auto/require 过、off/未知拒。
func TestPolicyBackendValidate(t *testing.T) {
	for _, ok := range []Backend{"", BackendAuto, BackendRequire} {
		if err := (&Policy{Mode: ModeReadOnly, Backend: ok}).Validate(); err != nil {
			t.Fatalf("%q 应过：%v", ok, err)
		}
	}
	for _, bad := range []Backend{BackendOff, "bogus"} {
		if err := (&Policy{Mode: ModeReadOnly, Backend: bad}).Validate(); err == nil {
			t.Fatalf("%q 应拒", bad)
		}
	}
}

// TestShellQuoteJoin argv → cmdLine 往返：空格/单引号/元字符元素单引号包裹，
// sh -c 解析还原等价（以 sh 实测还原为准）。
func TestShellQuoteJoin(t *testing.T) {
	argv := []string{"python3", "/tmp/my dir/x'y.py", "$HOME", "a;b|c"}
	line := shellQuoteJoin(argv)
	// sh 循环回显还原（单引号内禁展开——$HOME 原样）
	out, err := shellFields(line)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(argv) {
		t.Fatalf("还原数量不符：%v vs %v", out, argv)
	}
	for i := range argv {
		if out[i] != argv[i] {
			t.Fatalf("元素 %d 不符：%q vs %q", i, out[i], argv[i])
		}
	}
}

// shellFields sh 词法还原（for a in <quoted-line> 逐字段回显——POSIX 单引号
// 内禁展开与切分，还原即原 argv 元素）。
func shellFields(line string) ([]string, error) {
	cmd, _, err := BuildCommand(context.Background(), "", "", nil, nil,
		[]string{"sh", "-c", "for a in " + line + "; do printf '%s\\n' \"$a\"; done"}, nil)
	if err != nil {
		return nil, err
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"), nil
}
