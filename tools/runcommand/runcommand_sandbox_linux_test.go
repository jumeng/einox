//go:build linux && (amd64 || arm64)

// run_command 沙箱端到端真实测（工具面全链：NewTools → buildCmd → re-exec
// helper → 围栏内执行；含拒绝提示标注与后台任务起/查/停组杀）。
package runcommand

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/sandbox"
)

// sbxSetup 工作区与围栏外目标都落 /var/tmp（/tmp 默认可写会让验证假绿）。
func sbxSetup(t *testing.T) (root, outside string) {
	t.Helper()
	base := "/var/tmp/einox-sbx-runcommand"
	if err := os.MkdirAll(base, 0o777); err != nil {
		t.Skipf("无法创建测试基目录（非 root）: %v", err)
	}
	root, err := os.MkdirTemp(base, "ws-")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(base, "out-")
	if err != nil {
		t.Fatal(err)
	}
	outside = f.Name()
	f.Close()
	t.Cleanup(func() {
		os.RemoveAll(root)
		os.Remove(outside)
	})
	return root, outside
}

func sbxInvoke(t *testing.T, tool contract.Tool, args string) map[string]any {
	t.Helper()
	out, err := tool.Invoke(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil {
		t.Fatalf("非 JSON 输出：%s", out)
	}
	return m
}

func TestRunCommandSandboxE2E(t *testing.T) {
	root, outside := sbxSetup(t)
	ts, err := NewTools(Config{Root: root, Sandbox: &sandbox.Policy{Mode: sandbox.ModeWorkspaceWrite}})
	if err != nil || len(ts) != 3 {
		t.Fatalf("构造失败：%v（工具数 %d）", err, len(ts))
	}
	runT, outT, stopT := ts[0], ts[1], ts[2]

	// 围栏内正常执行（cwd=工作区，产物落位）
	m := sbxInvoke(t, runT, `{"command":"echo in-sbx && echo w > ok.txt"}`)
	if m["ok"] != true || m["exit_code"].(float64) != 0 {
		t.Fatalf("围栏内执行应成功：%v", m)
	}
	if _, err := os.Stat(filepath.Join(root, "ok.txt")); err != nil {
		t.Fatalf("产物应落工作区：%v", err)
	}

	// 围栏外写拒 + 拒绝提示标注（DenialHint → note）
	m = sbxInvoke(t, runT, `{"command":"echo x > `+outside+`"}`)
	if m["exit_code"].(float64) == 0 {
		t.Fatalf("工作区外写应被拒：%v", m)
	}
	note, _ := m["note"].(string)
	if !strings.Contains(note, "沙箱拒绝") {
		t.Fatalf("拒绝输出应附提示行（note）：%v", m)
	}

	// 后台任务起/查/停（沙箱路径进程组杀）
	m = sbxInvoke(t, runT, `{"command":"sleep 30","background":true}`)
	if m["ok"] != true {
		t.Fatalf("后台任务应启动：%v", m)
	}
	id := m["task_id"].(string)
	out, _ := outT.Invoke(context.Background(), json.RawMessage(`{"task_id":"`+id+`"}`))
	var om map[string]any
	json.Unmarshal([]byte(out), &om)
	if om["running"] != true {
		t.Fatalf("任务应在运行：%v", om)
	}
	out, _ = stopT.Invoke(context.Background(), json.RawMessage(`{"task_id":"`+id+`"}`))
	json.Unmarshal([]byte(out), &om)
	if om["ok"] != true || om["stopped"] != true {
		t.Fatalf("task_stop 应停止运行中任务：%v", om)
	}
}

// TestRunCommandSandboxTimeoutGroupKill 超时通道组杀（cmd.Cancel——沙箱形态
// 整组终结不留孙进程）。
func TestRunCommandSandboxTimeoutGroupKill(t *testing.T) {
	root, _ := sbxSetup(t)
	ts, err := NewTools(Config{Root: root, Sandbox: &sandbox.Policy{Mode: sandbox.ModeWorkspaceWrite}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	m := sbxInvoke(t, ts[0], `{"command":"sleep 30 & sleep 30","timeout_ms":500}`)
	if m["timed_out"] != true {
		t.Fatalf("应超时终止：%v", m)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("超时应及时（组杀生效，非等孙进程管道关闭）：%v", time.Since(start))
	}
}
