//go:build linux && !(amd64 || arm64)

// 其余 linux 架构运行时桩（seccomp 号表仅 x86_64/aarch64）：探测/施加
// 如实报 unusable——编译期拒绝会令 einox 整模块在该平台连沙箱关着都编
// 不过（审查 B2：codex unimplemented!() 实为运行时 panic，非编译期拦截）。
// darwin/windows 已有真后端（seatbelt_darwin.go / token_windows.go）。
package sandbox

import (
	"errors"
	"fmt"
	"os"
)

func probeEnforceChild() int {
	fmt.Fprintln(os.Stderr, "einox-sandbox: 当前架构后端未实现（seccomp 号表仅 x86_64/aarch64，unsupported-arch）")
	return helperFail
}

func applySandbox(*policyPayload) error {
	return errors.New("当前架构后端未实现")
}
