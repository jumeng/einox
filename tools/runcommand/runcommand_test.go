package runcommand

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jumeng/einox/sandbox"
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
	// 超时自动转后台（2026-09-22：zcode auto_on_timeout 对位——到点不杀，
	// 收编任务表返回 task_id；task_stop 收尾防进程残留）
	m = invoke(t, `{"command":"sleep 5","timeout_ms":200}`)
	if m["ok"] != true || m["task_id"] == "" || !strings.Contains(m["note"].(string), "自动转后台") {
		t.Fatalf("应自动转后台：%v", m)
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
		`{"command":"find . -type f -name '*.go'"}`,
		`{"command":"git branch"}`,
		`{"command":"git branch -a"}`,
		`{"command":"git tag"}`,
		`{"command":"git remote -v"}`,
		// 2026-09-22 增量：管道分段（两段白名单）+ env 赋值前缀
		`{"command":"grep -n foo bar.go | head -5"}`,
		`{"command":"cat a.txt | wc -l"}`,
		`{"command":"FOO=1 git status"}`,
		`{"command":"LC_ALL=C sort x.txt"}`,
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
		// 写型形态显式不豁免（安全审查 2026-09-06：白名单曾按子命令名/程序名放行）
		`{"command":"find . -name '*.log' -delete"}`,
		`{"command":"find . -fprint=/tmp/x"}`,
		`{"command":"find . -exec rm {} +"}`,
		`{"command":"tree -o out.txt"}`,
		`{"command":"git branch -D main"}`,
		`{"command":"git branch newbranch"}`, // 位置参数 = 建分支
		`{"command":"git tag v1.0"}`,         // 位置参数 = 打 tag
		`{"command":"git tag -d v1.0"}`,
		`{"command":"git remote add origin https://x"}`,
		`{"command":"git remote rename a b"}`,
		// 管道/env 增量的负例：一段非白名单/写型/病态分段即整体拒
		`{"command":"cat a.txt | sh"}`,
		`{"command":"cat a.txt | sudo ls"}`,
		`{"command":"grep x . | tee out.txt"}`,
		`{"command":"FOO=1 curl https://x"}`,
		`{"command":"FOO=1 git branch -D main"}`,
		`{"command":"ls -la || ls"}`,
		`{"command":"ls -la |"}`,
		`{"command":"cat a.txt; ls"}`,
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

// TestDockerEnvRetired EINO_RUN_DOCKER env 魔法开关退役（批次 C，设计真源
// 定案《2026-08-29-assembly-seams-design》（工作区档案，不入库） §4）：旧开关不再有任何效果
// ——容器形态正规化为 sandbox.DockerProvider 经 Config.SandboxProvider 注入
// （argv 映射测试归 sandbox/docker_test.go）。
func TestDockerEnvRetired(t *testing.T) {
	t.Setenv("EINO_RUN_DOCKER", "1")
	cmd, sandboxed, err := sandbox.BuildCommand(context.Background(), "/ws", "/ws", nil, nil, []string{"sh", "-c", "echo hi"}, nil)
	if err != nil || sandboxed || cmd.Args[0] != "sh" {
		t.Fatalf("退役开关不应再生效（应直执行）：%v %v", cmd.Args, err)
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

// TestRunCwdEnvTail C3 会话态：cwd 圈禁/env 注入黑名单/tail_lines 取样。
func TestRunCwdEnvTail(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	ts, err := NewTools(Config{Root: root})
	if err != nil || len(ts) != 3 {
		t.Fatalf("构造失败：%v", err)
	}
	run0 := func(args string) map[string]any {
		t.Helper()
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

	// cwd 生效：sub 下 pwd
	m := run0(`{"command":"basename $(pwd)","cwd":"sub"}`)
	if m["ok"] != true || !strings.Contains(m["output"].(string), "sub") {
		t.Fatalf("cwd 应生效：%v", m)
	}
	// cwd 越界拒
	m = run0(`{"command":"pwd","cwd":"../outside"}`)
	if m["ok"] != false || !strings.Contains(m["error"].(string), "越界") {
		t.Fatalf("cwd 越界应拒：%v", m)
	}
	// env 注入生效
	m = run0(`{"command":"echo $MYVAR","env":["MYVAR=hello_env"]}`)
	if !strings.Contains(m["output"].(string), "hello_env") {
		t.Fatalf("env 注入应生效：%v", m)
	}
	// env 黑名单拒（大小写折叠）
	for _, kv := range []string{"PATH=/x", "Home=/x", "LD_PRELOAD=/x"} {
		m = run0(fmt.Sprintf(`{"command":"true","env":[%q]}`, kv))
		if m["ok"] != false || !strings.Contains(m["error"].(string), "黑名单") {
			t.Fatalf("env 黑名单应拒 %s：%v", kv, m)
		}
	}
	// env 坏形态拒
	if m = run0(`{"command":"true","env":["NOEQ"]}`); m["ok"] != false {
		t.Fatalf("坏形态应拒：%v", m)
	}
	// tail_lines：尾 N 行取样 + 前面省略标注
	m = run0(`{"command":"seq 1 50","tail_lines":3}`)
	out := m["output"].(string)
	if !strings.Contains(out, "前面省略") || !strings.Contains(out, "49") || !strings.Contains(out, "50") || strings.Contains(out, "1\n2") {
		t.Fatalf("tail_lines 应取尾行：%q", out)
	}
}

// TestTimeoutAutoBackground 超时转后台全链：task_id 可查（running）→ task_stop
// 终结；短命令零变化。
func TestTimeoutAutoBackground(t *testing.T) {
	root := t.TempDir()
	ts, err := NewTools(Config{Root: root})
	if err != nil || len(ts) != 3 {
		t.Fatalf("构造失败：%v", err)
	}
	out, _ := ts[0].Invoke(context.Background(), json.RawMessage(`{"command":"sleep 3","timeout_ms":150}`))
	var m map[string]any
	json.Unmarshal([]byte(out), &m)
	if m["ok"] != true || m["task_id"] == "" {
		t.Fatalf("超时应转后台：%v", m)
	}
	id := m["task_id"].(string)
	// task_output：running 态可见
	out2, _ := ts[1].Invoke(context.Background(), json.RawMessage(`{"task_id":"`+id+`"}`))
	var m2 map[string]any
	json.Unmarshal([]byte(out2), &m2)
	if m2["ok"] != true || m2["running"] != true {
		t.Fatalf("转后台任务应可查 running：%v", m2)
	}
	// task_stop 终结（收尾防进程残留）
	out3, _ := ts[2].Invoke(context.Background(), json.RawMessage(`{"task_id":"`+id+`"}`))
	var m3 map[string]any
	json.Unmarshal([]byte(out3), &m3)
	if m3["ok"] != true || m3["stopped"] != true {
		t.Fatalf("task_stop 应终结：%v", m3)
	}
	// 短命令零变化（完成路径：exit_code/duration/output 形态不变）
	out4, _ := ts[0].Invoke(context.Background(), json.RawMessage(`{"command":"echo quick"}`))
	var m4 map[string]any
	json.Unmarshal([]byte(out4), &m4)
	if m4["ok"] != true || m4["exit_code"].(float64) != 0 || !strings.Contains(m4["output"].(string), "quick") {
		t.Fatalf("短命令应零变化：%v", m4)
	}
}
