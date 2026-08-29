package sandbox

// EnvMode 环境档回归（设计真源 findings/2026-08-29-assembly-seams-design.md
// §8.3）：缺省 inherit 零行为变化；minimal 白名单——凭据面默认不进围栏，
// Policy.Env 显式注入照常覆盖/追加。

import (
	"strings"
	"testing"
)

func envHas(env []string, prefix string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func TestBaseEnvInheritDefault(t *testing.T) {
	t.Setenv("EINOX_TEST_CRED", "secret")
	env := baseEnv(&Policy{}) // 缺省 inherit = 全继承（LLM_*/载荷由 cleanseEnv 剥）
	if !envHas(env, "EINOX_TEST_CRED=") {
		t.Fatal("inherit 档应全继承（零行为变化锚）")
	}
}

func TestBaseEnvMinimalWhitelist(t *testing.T) {
	t.Setenv("EINOX_TEST_CRED", "secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "leak")
	t.Setenv("GITHUB_TOKEN", "leak")
	env := baseEnv(&Policy{EnvMode: EnvMinimal})
	if !envHas(env, "PATH=") {
		t.Fatal("minimal 档应保留 PATH")
	}
	for _, cred := range []string{"EINOX_TEST_CRED=", "AWS_SECRET_ACCESS_KEY=", "GITHUB_TOKEN=", "LLM_API_KEY="} {
		if envHas(env, cred) {
			t.Fatalf("minimal 档不应下传凭据面：%s", cred)
		}
	}
}

func TestPayloadEnvMinimalKeepsExplicit(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "leak")
	pol := &Policy{
		Mode:    ModeWorkspaceWrite,
		EnvMode: EnvMinimal,
		Env:     []string{"GOCACHE=/cache/go-build"},
	}
	env := payloadEnv(pol, "/ws")
	if !envHas(env, "PATH=") || !envHas(env, "EINOX_SANDBOX_POLICY=") || !envHas(env, "GOCACHE=/cache/go-build") {
		t.Fatalf("minimal 档应保 PATH/载荷/显式注入：%v", env)
	}
	if envHas(env, "AWS_SECRET_ACCESS_KEY=") {
		t.Fatal("minimal 档下凭据不得进载荷环境")
	}
}
