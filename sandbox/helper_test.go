package sandbox

import (
	"os"
	"strings"
	"testing"
)

// TestMain 沙箱自测锚点（真源 §2.1.3）：测试二进制自身可 re-exec 作 helper
// 宿主——沙箱真实测经 [selfExe, __einox-sandbox, ...] 回到本 TestMain 再入
// RunHelper 施加路径。
func TestMain(m *testing.M) {
	RunHelper(os.Args)
	os.Exit(m.Run())
}

func TestPolicyValidate(t *testing.T) {
	for _, m := range []Mode{ModeReadOnly, ModeWorkspaceWrite, ModeDangerFullAccess} {
		if err := (&Policy{Mode: m}).Validate(); err != nil {
			t.Errorf("合法模式被拒 %v: %v", m, err)
		}
	}
	if err := (&Policy{Mode: "weird"}).Validate(); err == nil {
		t.Fatal("未知模式应拒绝")
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2", "C=3"}, []string{"B=22", "D=4"})
	if strings.Join(got, " ") != "A=1 C=3 B=22 D=4" {
		t.Fatalf("去重合并（后者覆盖）错：%v", got)
	}
}

func TestDenialHint(t *testing.T) {
	// shell 文案（dash/bash 首字母大写）与 Go errno 文案（小写）都要命中
	if DenialHint("sh: 1: cannot create /x: Permission denied") == "" {
		t.Fatal("EACCES 文案（大写）应命中")
	}
	if DenialHint("dial tcp 127.0.0.1:1: operation not permitted") == "" {
		t.Fatal("EPERM 文案应命中")
	}
	if DenialHint("build ok") != "" {
		t.Fatal("正常输出不应命中")
	}
	if h := DenialHint("mkdir: cannot create ‘x’: Permission denied"); !strings.Contains(h, "ask_user") {
		t.Fatal("提示应指向真实出口 ask_user（C5：escalation 前禁指走审批）")
	}
}

func TestAbiUncovered(t *testing.T) {
	if u := abiUncovered(5); len(u) != 0 {
		t.Fatalf("ABI5 应无未覆盖项：%v", u)
	}
	if u := abiUncovered(3); strings.Join(u, ",") != "ioctl-dev" {
		t.Fatalf("ABI3 未覆盖项错：%v", u)
	}
	if u := abiUncovered(1); strings.Join(u, ",") != "refer,truncate,ioctl-dev" {
		t.Fatalf("ABI1 未覆盖项错：%v", u)
	}
}

func TestCleanseEnv(t *testing.T) {
	got := cleanseEnv([]string{
		"PATH=/usr/bin", "LLM_API_KEY=secret", "LLM_BASE_URL=http://x",
		"EINOX_SANDBOX_POLICY={}", "HOME=/root",
	})
	if strings.Join(got, " ") != "PATH=/usr/bin HOME=/root" {
		t.Fatalf("LLM_* 与策略载荷应剔除、普通 env 保留：%v", got)
	}
}
