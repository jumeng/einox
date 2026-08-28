//go:build linux && (amd64 || arm64)

// Linux 后端编排：施加序与探测子进程（真源 §2.1/§1.3）。
package sandbox

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// applySandbox 施加序（真源 §2.1：NO_NEW_PRIVS → seccomp → Landlock →
// rlimit，对齐 codex apply_permission_profile_to_current_thread）。
// danger-full-access 不加 fs 围栏；断网（Network=false）即便 danger 也装
// seccomp（codex fail-closed 哲学）；nnp 仅在装 seccomp/Landlock 时置
// （setuid 工具兼容，codex 同款条件）。
func applySandbox(p *policyPayload) error {
	needSeccomp := !p.Network
	needLandlock := p.Mode != ModeDangerFullAccess
	if needSeccomp || needLandlock {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("PR_SET_NO_NEW_PRIVS: %w", err)
		}
	}
	if needSeccomp {
		if err := installSeccompFilter(); err != nil {
			return fmt.Errorf("seccomp: %w", err)
		}
	}
	if needLandlock {
		if err := landlockApply(p); err != nil {
			return fmt.Errorf("landlock: %w", err)
		}
	}
	return applyRlimits(p.Limit)
}

// probeEnforceChild 后端实测子进程（真源 §1.3 ②：自施加才算诚实信号——
// 「--version 式检查会漏有 syscall 但拒执行的内核」，dsh probe 哲学）。
// read-only + 断网全栈自施加（nnp+seccomp+landlock+rlimit），报告行由父
// 进程 probeOSBackend 解析：einox-sandbox-enforce full|partial abi=N。
func probeEnforceChild() int {
	abi, err := landlockABI()
	if err != nil {
		fmt.Fprintf(os.Stderr, "einox-sandbox: %v\n", err)
		return helperFail
	}
	if err := applySandbox(&policyPayload{Policy: Policy{Mode: ModeReadOnly}}); err != nil {
		fmt.Fprintf(os.Stderr, "einox-sandbox: %v\n", err)
		return helperFail
	}
	state := "full"
	if abi < landlockMaxABI {
		state = "partial"
	}
	fmt.Printf("einox-sandbox-enforce %s abi=%d\n", state, min(abi, landlockMaxABI))
	return 0
}
