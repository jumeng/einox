package sandbox

// DockerProvider argv 映射回归（设计真源 findings/2026-08-29-assembly-seams-
// design.md §4；策略翻译表见 docker.go 头注）。纯构造测试——不依赖 docker
// daemon（Wrap 的探测门与一次性容器真实执行归部署机验证）。

import (
	"strings"
	"testing"
)

func dockerArgvJoin(image string, pol *Policy, ws, cmd string) string {
	return strings.Join(dockerArgv(image, pol, ws, cmd), " ")
}

func TestDockerArgvModeMapping(t *testing.T) {
	ws := "/ws"
	// readonly：--read-only + 工作区 ro（唯一一条工作区挂载——docker daemon
	// 对同一请求内重复目标 bind 报 Duplicate mount point，不能靠先 rw 再 ro）
	ro := dockerArgvJoin("alpine:3.20", &Policy{Mode: ModeReadOnly}, ws, "ls")
	if !strings.Contains(ro, "--read-only") || !strings.Contains(ro, ws+":/workspace:ro") {
		t.Fatalf("readonly 档翻译错：%s", ro)
	}
	if n := strings.Count(ro, ":/workspace"); n != 1 {
		t.Fatalf("readonly 档工作区挂载应唯一（实得 %d 条）：%s", n, ro)
	}
	// workspace-write：容器根 ro，工作区默认 rw
	ww := dockerArgvJoin("alpine:3.20", &Policy{Mode: ModeWorkspaceWrite}, ws, "ls")
	if !strings.Contains(ww, "--read-only") || strings.Contains(ww, ":/workspace:ro") {
		t.Fatalf("workspace-write 档翻译错：%s", ww)
	}
	if n := strings.Count(ww, ":/workspace"); n != 1 {
		t.Fatalf("workspace-write 档工作区挂载应唯一（实得 %d 条）：%s", n, ww)
	}
	// danger：无 --read-only（隔离仍在），工作区默认 rw
	dg := dockerArgvJoin("alpine:3.20", &Policy{Mode: ModeDangerFullAccess}, ws, "ls")
	if strings.Contains(dg, "--read-only") {
		t.Fatalf("danger 档不应只读容器根：%s", dg)
	}
	if n := strings.Count(dg, ":/workspace"); n != 1 {
		t.Fatalf("danger 档工作区挂载应唯一（实得 %d 条）：%s", n, dg)
	}
}

func TestDockerArgvNetworkRootsProtection(t *testing.T) {
	pol := &Policy{
		Mode:              ModeWorkspaceWrite,
		Network:           false,
		WritableRoots:     []string{"/cache"},
		ProtectedReadOnly: []string{"/ws/repos/base/.git"},
		Env:               []string{"GOCACHE=/cache/go-build"},
		Limit:             Limit{NProc: 64},
	}
	got := dockerArgvJoin("golang:1.26", pol, "/ws", "go build ./...")
	for _, want := range []string{
		"--network none",
		"-v /cache:/cache:rw",
		"-v /ws/repos/base/.git:/ws/repos/base/.git:ro",
		"-e GOCACHE=/cache/go-build",
		"--pids-limit 64",
		"golang:1.26 sh -c go build ./...",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("翻译缺 %q：%s", want, got)
		}
	}
	// Network=true = 默认 bridge：不出 --network none
	on := dockerArgvJoin("alpine:3.20", &Policy{Mode: ModeReadOnly, Network: true}, "/ws", "ls")
	if strings.Contains(on, "--network") {
		t.Fatalf("开网档不应显式 --network：%s", on)
	}
}

// TestDockerArgvMountDedupe 同目标重复 bind 去重（Duplicate mount point 的
// 同类残留面）：列表内重复条目收敛为一条；同路径横跨两列表时取 :ro
// （收紧方向——ProtectedReadOnly 语义优先）。
func TestDockerArgvMountDedupe(t *testing.T) {
	pol := &Policy{
		Mode:              ModeWorkspaceWrite,
		WritableRoots:     []string{"/cache", "/cache", "/opt/data"},
		ProtectedReadOnly: []string{"/ws/.git", "/cache"},
	}
	got := dockerArgvJoin("alpine:3.20", pol, "/ws", "ls")
	if n := strings.Count(got, "-v /cache:/cache"); n != 1 {
		t.Fatalf("/cache 应只挂一条（ro 收紧胜出），实得 %d 条：%s", n, got)
	}
	if !strings.Contains(got, "-v /cache:/cache:ro") {
		t.Fatalf("冲突路径应收紧为 :ro：%s", got)
	}
	if n := strings.Count(got, "/opt/data:/opt/data:rw"); n != 1 {
		t.Fatalf("/opt/data 应只挂一条 rw：%s", got)
	}
	if n := strings.Count(got, "/ws/.git:/ws/.git:ro"); n != 1 {
		t.Fatalf("/ws/.git 应只挂一条 ro：%s", got)
	}
}

func TestDockerProviderImageDefault(t *testing.T) {
	d := &DockerProvider{}
	if got := d.image(); got != "alpine:3.20" {
		t.Fatalf("缺省镜像应与既有 dockerWrap 一致：%s", got)
	}
	if got := (&DockerProvider{Image: "golang:1.26"}).image(); got != "golang:1.26" {
		t.Fatalf("定制镜像未生效：%s", got)
	}
}

// TestDockerProviderWrapUnusableDegrades daemon 不可达的降级语义钉板
// （审查 A-3 明示）：nil argv = 调用方按姿态降级（当前接线 auto = 裸跑 +
// 告警）——语义显式成文并受测，fail-closed 拒跑属 require 姿态接线
// （真源 §10.4），不隐式混入后端。
func TestDockerProviderWrapUnusableDegrades(t *testing.T) {
	d := &DockerProvider{}
	d.once.Do(func() { // 预置探测结果（包内测试可及；跳过真实 daemon 探测）
		d.status = Status{Enforcement: EnforcementUnusable, Detail: "test: daemon 不可达"}
	})
	argv, env := d.Wrap(&Policy{Mode: ModeWorkspaceWrite}, "/ws", "ls")
	if argv != nil || env != nil {
		t.Fatalf("不可达应返回 nil argv/env（调用方按姿态降级裸跑），实得 %v %v", argv, env)
	}
}
