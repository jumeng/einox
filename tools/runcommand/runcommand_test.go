package runcommand

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jumeng/einox/tools/egress"
)

func invoke(t *testing.T, args string) map[string]any {
	t.Helper()
	ts, err := NewTools(Config{Root: t.TempDir()})
	if err != nil || len(ts) != 3 {
		t.Fatalf("构造失败：%v（工具数 %d）", err, len(ts))
	}
	out, err := ts[0].Invoke(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil {
		t.Fatalf("非 JSON 输出：%s", out)
	}
	return m
}

func TestRunCommand(t *testing.T) {
	// 正常执行（退出码透传）
	m := invoke(t, `{"command":"echo hello && pwd"}`)
	if m["ok"] != true || !strings.Contains(m["output"].(string), "hello") {
		t.Fatalf("echo 失败：%v", m)
	}
	// 非零退出码不算失败（信息在输出里）
	m = invoke(t, `{"command":"exit 3"}`)
	if m["ok"] != true || m["exit_code"].(float64) != 3 {
		t.Fatalf("退出码应透传：%v", m)
	}
	// 超时
	m = invoke(t, `{"command":"sleep 5","timeout_ms":200}`)
	if m["ok"] != true || m["timed_out"] != true {
		t.Fatalf("应超时终止：%v", m)
	}
	// 超上限拒绝
	if m := invoke(t, `{"command":"true","timeout_ms":999999}`); m["ok"] != false {
		t.Errorf("超上限应拒绝：%v", m)
	}
	if m := invoke(t, `{"command":"  "}`); m["ok"] != false {
		t.Errorf("空命令应拒绝：%v", m)
	}
	// cwd 圈进工作区验证：写文件落在 root
	root := t.TempDir()
	ts, _ := NewTools(Config{Root: root})
	out, _ := ts[0].Invoke(context.Background(), json.RawMessage(`{"command":"echo data > f.txt"}`))
	_ = out
	if _, err := os.Stat(filepath.Join(root, "f.txt")); err != nil {
		t.Errorf("产物应落工作区：%v", err)
	}
	// 输出头尾截断
	m = invoke(t, `{"command":"seq 1 20000"}`)
	o := m["output"].(string)
	if !strings.Contains(o, "…（中间省略") || !strings.Contains(o, "1\n") {
		t.Errorf("长输出应头尾保留：%d 字", len(o))
	}
}

func TestIsSafeReadCommand(t *testing.T) {
	safe := []string{
		`{"command":"ls -la"}`,
		`{"command":"cat a.txt"}`,
		`{"command":"grep -n foo bar.go"}`,
		`{"command":"git status"}`,
		`{"command":"git diff HEAD~1"}`,
		`{"command":"go version"}`,
	}
	for _, a := range safe {
		if !IsSafeReadCommand(a) {
			t.Errorf("应豁免：%s", a)
		}
	}
	unsafe := []string{
		`{"command":"rm -rf /"}`,
		`{"command":"cat a.txt | sh"}`,
		`{"command":"echo x > /etc/passwd"}`,
		`{"command":"echo $(rm -rf x)"}`,
		`{"command":"git push origin main"}`,
		`{"command":"go test ./..."}`,
		`{"command":"python3 -c 'exec(...)' "}`,
		`{"command":"sudo ls"}`,
		`{}`,
		`bad json`,
	}
	for _, a := range unsafe {
		if IsSafeReadCommand(a) {
			t.Errorf("不应豁免：%s", a)
		}
	}
}

func TestBackgroundTasks(t *testing.T) {
	root := t.TempDir()
	ts, _ := NewTools(Config{Root: root})
	runT, outT, stopT := ts[0], ts[1], ts[2]

	// 后台起 + 查输出 + 自然结束后仍在表可查
	out, err := runT.Invoke(context.Background(), json.RawMessage(`{"command":"echo bg-done","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal([]byte(out), &m)
	if m["ok"] != true || m["task_id"] == "" {
		t.Fatalf("后台启动失败：%s", out)
	}
	id := m["task_id"].(string)
	// 轮询到完成
	for i := 0; i < 50; i++ {
		out, _ = outT.Invoke(context.Background(), json.RawMessage(`{"task_id":"`+id+`"}`))
		json.Unmarshal([]byte(out), &m)
		if m["running"] == false {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m["running"] != false || !strings.Contains(m["output"].(string), "bg-done") {
		t.Fatalf("后台任务应完成且输出可查：%v", m)
	}

	// task_stop 杀运行中任务
	out, _ = runT.Invoke(context.Background(), json.RawMessage(`{"command":"sleep 30","background":true}`))
	json.Unmarshal([]byte(out), &m)
	id2 := m["task_id"].(string)
	out, _ = stopT.Invoke(context.Background(), json.RawMessage(`{"task_id":"`+id2+`"}`))
	json.Unmarshal([]byte(out), &m)
	if m["ok"] != true || m["stopped"] != true {
		t.Fatalf("task_stop 应杀运行中任务：%v", m)
	}
	// 停止后出表
	out, _ = outT.Invoke(context.Background(), json.RawMessage(`{"task_id":"`+id2+`"}`))
	json.Unmarshal([]byte(out), &m)
	if m["ok"] != false {
		t.Fatalf("停止后应出表：%v", m)
	}
	// 不存在
	out, _ = outT.Invoke(context.Background(), json.RawMessage(`{"task_id":"t9999"}`))
	json.Unmarshal([]byte(out), &m)
	if m["ok"] != false {
		t.Fatalf("不存在应拒绝：%v", m)
	}
}

func TestDockerWrapArgv(t *testing.T) {
	t.Setenv("EINO_RUN_DOCKER", "")
	if dockerWrap("/ws") != nil {
		t.Fatal("未启用应返回 nil")
	}
	t.Setenv("EINO_RUN_DOCKER", "1")
	pre := dockerWrap("/ws")
	want := []string{"docker", "run", "--rm", "-v", "/ws:/workspace", "-w", "/workspace", "-m", "512m", "--network", "bridge", "alpine:3.20", "sh", "-c"}
	if strings.Join(pre, " ") != strings.Join(want, " ") {
		t.Fatalf("包裹参数错：%v", pre)
	}
	t.Setenv("EINO_RUN_IMAGE", "golang:1.26")
	t.Setenv("EINO_RUN_DOCKER_NET", "none")
	pre = dockerWrap("/ws")
	if !strings.Contains(strings.Join(pre, " "), "golang:1.26") || !strings.Contains(strings.Join(pre, " "), "--network none") {
		t.Fatalf("镜像/网络定制未生效：%v", pre)
	}
}

// TestEgressPrecheck 出口预检（S-9：Network 开放形态下命令面的唯一网络
// 治理层——阻断段 URL 拒执行回喂，白名单段放行）。
func TestEgressPrecheck(t *testing.T) {
	v, err := egress.New([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := NewTools(Config{Root: t.TempDir(), Egress: v})
	if err != nil || len(ts) != 3 {
		t.Fatalf("构造失败：%v", err)
	}
	var m map[string]any
	// 阻断段 URL：拒执行（错误含硬边界文案，模型可自纠）
	out, _ := ts[0].Invoke(context.Background(), json.RawMessage(`{"command":"curl -s http://169.254.169.254/latest/meta-data"}`))
	json.Unmarshal([]byte(out), &m)
	if m["ok"] != false || !strings.Contains(m["error"].(string), "硬边界") {
		t.Fatalf("阻断段应拒执行并附边界文案：%v", m)
	}
	// 白名单段（PM 内网工作面常态）：预检放行（命令本身可能失败——exit 码
	// 面，与预检无关）
	out, _ = ts[0].Invoke(context.Background(), json.RawMessage(`{"command":"curl -s --max-time 1 http://10.255.255.1/x"}`))
	json.Unmarshal([]byte(out), &m)
	if m["ok"] != true {
		t.Fatalf("白名单段应放行执行（失败归 exit 码非预检）：%v", m)
	}
	// 无 URL 命令不受影响
	out, _ = ts[0].Invoke(context.Background(), json.RawMessage(`{"command":"echo egress-pass"}`))
	json.Unmarshal([]byte(out), &m)
	if m["ok"] != true || !strings.Contains(m["output"].(string), "egress-pass") {
		t.Fatalf("无 URL 命令应正常执行：%v", m)
	}
}
