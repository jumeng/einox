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
	// readonly：--read-only + 工作区 ro 遮蔽
	ro := dockerArgvJoin("alpine:3.20", &Policy{Mode: ModeReadOnly}, ws, "ls")
	if !strings.Contains(ro, "--read-only") || !strings.Contains(ro, ws+":/workspace:ro") {
		t.Fatalf("readonly 档翻译错：%s", ro)
	}
	// workspace-write：容器根 ro，工作区保持默认 rw（无 :ro 遮蔽）
	ww := dockerArgvJoin("alpine:3.20", &Policy{Mode: ModeWorkspaceWrite}, ws, "ls")
	if !strings.Contains(ww, "--read-only") || strings.Contains(ww, ":/workspace:ro") {
		t.Fatalf("workspace-write 档翻译错：%s", ww)
	}
	// danger：无 --read-only（隔离仍在），工作区默认 rw
	dg := dockerArgvJoin("alpine:3.20", &Policy{Mode: ModeDangerFullAccess}, ws, "ls")
	if strings.Contains(dg, "--read-only") {
		t.Fatalf("danger 档不应只读容器根：%s", dg)
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

func TestDockerProviderImageDefault(t *testing.T) {
	d := &DockerProvider{}
	if got := d.image(); got != "alpine:3.20" {
		t.Fatalf("缺省镜像应与既有 dockerWrap 一致：%s", got)
	}
	if got := (&DockerProvider{Image: "golang:1.26"}).image(); got != "golang:1.26" {
		t.Fatalf("定制镜像未生效：%s", got)
	}
}
