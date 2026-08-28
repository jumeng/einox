//go:build linux

// Linux 分支 Wrap 装配：re-exec 哨兵形（真源 §2.1）。arch 桩平台同样编入
// 本文件（不可达——unsupported-arch 探测 unusable 已在 Wrap 短路）。
package sandbox

import (
	"log"
	"strings"
	"sync"
)

// protReadOnlyWarn 告警一次（静态一次装配假设——真源 §7.5，单 policy；
// per-policy 告警随 per-call 演进再议）。
var protReadOnlyWarn sync.Once

// protectedReadOnlyNote ProtectedReadOnly 后端不支持告警（审查 B-2：Landlock
// allow-list 联合语义做不到可写区内只读回盖——真源 §2.2 partial 语义的如实
// 上报渠道；require 档接线前以告警出口，bwrap/Docker 后端落地后此项可治）。
func protectedReadOnlyNote(pol *Policy) string {
	if len(pol.ProtectedReadOnly) == 0 {
		return ""
	}
	return "einox-sandbox: ProtectedReadOnly 不被 Landlock 后端支持（partial: protected-readonly），保护子路径实际可写: " +
		strings.Join(pol.ProtectedReadOnly, ", ")
}

// wrapOSBackend Linux：ProtectedReadOnly 告警（Landlock allow-list 联合
// 语义做不到可写区内只读回盖，审查 B-2 的如实上报渠道）+ re-exec 哨兵
// argv/env。
func wrapOSBackend(pol *Policy, workspace, cmdLine string) ([]string, []string) {
	if note := protectedReadOnlyNote(pol); note != "" {
		protReadOnlyWarn.Do(func() { log.Print(note) })
	}
	return ArgvEnv(pol, workspace, cmdLine)
}
