// Seatbelt profile 纯构造（darwin 后端的平台无关部分，真源 §3）：组装
// 顺序对齐 codex seatbelt.rs——base（deny default 起手）→ file-read 段 →
// file-write 段 → 网络段 → 保护子路径 require-not 排除 → 锚点 deny。路径
// 全部经 -D KEY=VALUE 参数注入、profile 内 (param "KEY") 引用——路径不拼进
// policy 字符串（转义/注入面，codex 同款纪律）。本文件零 fs 调用（纯字符串
// 构造，任意平台可单测）；路径规范化（嵌套 symlink 拒绝/顶层别名解析）在
// seatbelt_darwin.go 运行时侧。
package sandbox

import (
	"fmt"
	"path"
	"strings"
)

// seatbeltBasePolicy 基础策略（照抄 codex seatbelt_base_policy.sbpl，源自
// Chrome sandbox policy，Apache-2.0；codex 仓 codex-rs/sandboxing/src/）：
// deny default 起手 + process-exec/fork/signal same-sandbox + sysctl 只读
// 白名单 + /dev/null 写 + PTY 兼容。文本整体移植不裁剪——sysctl 白名单
// 是工具链（go/java/python）在 mac 上可运行的保命件。
const seatbeltBasePolicy = `(version 1)

; inspired by Chrome's sandbox policy:
; https://source.chromium.org/chromium/chromium/src/+/main:sandbox/policy/mac/common.sb
; (Apache-2.0; 移植自 codex codex-rs/sandboxing/src/seatbelt_base_policy.sbpl)

; start with closed-by-default
(deny default)

; child processes inherit the policy of their parent
(allow process-exec)
(allow process-fork)
(allow signal (target same-sandbox))

; process-info
(allow process-info* (target same-sandbox))

(allow file-write-data
  (require-all
    (path "/dev/null")
    (vnode-type CHARACTER-DEVICE)))

; sysctls permitted.
(allow sysctl-read
  (sysctl-name "hw.activecpu")
  (sysctl-name "hw.busfrequency_compat")
  (sysctl-name "hw.byteorder")
  (sysctl-name "hw.cacheconfig")
  (sysctl-name "hw.cachelinesize_compat")
  (sysctl-name "hw.cpufamily")
  (sysctl-name "hw.cpufrequency_compat")
  (sysctl-name "hw.cputype")
  (sysctl-name "hw.l1dcachesize_compat")
  (sysctl-name "hw.l1icachesize_compat")
  (sysctl-name "hw.l2cachesize_compat")
  (sysctl-name "hw.l3cachesize_compat")
  (sysctl-name "hw.logicalcpu_max")
  (sysctl-name "hw.machine")
  (sysctl-name "hw.model")
  (sysctl-name "hw.memsize")
  (sysctl-name "hw.ncpu")
  (sysctl-name "hw.nperflevels")
  (sysctl-name-prefix "hw.optional.arm.")
  (sysctl-name-prefix "hw.optional.armv8_")
  (sysctl-name "hw.packages")
  (sysctl-name "hw.pagesize_compat")
  (sysctl-name "hw.pagesize")
  (sysctl-name "hw.physicalcpu")
  (sysctl-name "hw.physicalcpu_max")
  (sysctl-name "hw.logicalcpu")
  (sysctl-name "hw.cpufrequency")
  (sysctl-name "hw.tbfrequency_compat")
  (sysctl-name "hw.vectorunit")
  (sysctl-name "machdep.cpu.brand_string")
  (sysctl-name "kern.argmax")
  (sysctl-name "kern.hostname")
  (sysctl-name "kern.maxfilesperproc")
  (sysctl-name "kern.maxproc")
  (sysctl-name "kern.osproductversion")
  (sysctl-name "kern.osrelease")
  (sysctl-name "kern.ostype")
  (sysctl-name "kern.osvariant_status")
  (sysctl-name "kern.osversion")
  (sysctl-name "kern.secure_kernel")
  (sysctl-name "kern.usrstack64")
  (sysctl-name "kern.version")
  (sysctl-name "sysctl.proc_cputype")
  (sysctl-name "vm.loadavg")
  (sysctl-name-prefix "hw.perflevel")
  (sysctl-name-prefix "kern.proc.pgrp.")
  (sysctl-name-prefix "kern.proc.pid.")
  (sysctl-name-prefix "net.routetable.")
)

; Allow Java to read some CPU info. This is misclassified as a "write" because
; userspace passes a memory buffer to the sysctl, but conceptually it is a read.
(allow sysctl-write
  (sysctl-name "kern.grade_cputype"))

; IOKit
(allow iokit-open
  (iokit-registry-entry-class "RootDomainUserClient")
)

; needed to look up user info, see https://crbug.com/792228
(allow mach-lookup
  (global-name "com.apple.system.opendirectoryd.libinfo")
)

; Needed for python multiprocessing on MacOS for the SemLock
(allow ipc-posix-sem)

; Needed for PyTorch/libomp on macOS to register OpenMP runtimes.
(allow ipc-posix-shm-read-data
  ipc-posix-shm-write-create
  ipc-posix-shm-write-unlink
  (ipc-posix-name-regex #"^/__KMP_REGISTERED_LIB_[0-9]+$"))

(allow mach-lookup
  (global-name "com.apple.PowerManagement.control")
)

; allow openpty()
(allow pseudo-tty)
(allow file-read* file-write* file-ioctl (literal "/dev/ptmx"))
(allow file-read* file-write*
  (require-all
    (regex #"^/dev/ttys[0-9]+")
    (extension "com.apple.sandbox.pty")))
; PTYs created before entering seatbelt may lack the extension; allow ioctl
; on those slave ttys so interactive shells detect a TTY and remain functional.
(allow file-ioctl (regex #"^/dev/ttys[0-9]+"))
`

// seatbeltNetworkPolicy 网络启用段追加件（照抄 codex
// seatbelt_network_policy.sbpl，Apache-2.0）：AF_SYSTEM 本地平台服务 socket
// + TLS/证书/DNS 服务的 mach-lookup 白名单。
const seatbeltNetworkPolicy = `(allow system-socket
  (require-all
    (socket-domain AF_SYSTEM)
    (socket-protocol 2)
  )
)

(allow mach-lookup
    ; Used by platform helpers that resolve user directory locations.
    (global-name "com.apple.bsd.dirhelper")
    (global-name "com.apple.system.opendirectoryd.membership")

    ; Communicate with the security server for TLS certificate information.
    (global-name "com.apple.SecurityServer")
    (global-name "com.apple.networkd")
    (global-name "com.apple.ocspd")
    (global-name "com.apple.trustd.agent")

    ; Read network configuration.
    (global-name "com.apple.SystemConfiguration.DNSConfiguration")
    (global-name "com.apple.SystemConfiguration.configd")
)

(allow sysctl-read
  (sysctl-name-regex #"^net.routetable")
)
`

// seatbeltWriteRoot 可写根描述（构造输入——darwin 侧已完成路径规范化：
// 嵌套 symlink 拒绝、顶层别名解析、literal/subpath 判定）。
type seatbeltWriteRoot struct {
	Path      string   // 绝对路径
	Literal   bool     // 现存非目录文件 = 字面量匹配（codex 同款）；目录/不存在 = 子树
	Protected []string // 该根内要求只读的子路径（require-not 排除，含逻辑与解析两种形态）
	// ProtectedAncestors 保护路径的根内中间祖先目录（deny rename/unlink——
	// 改名任一中间祖先都会把保护子路径搬出 require-not 刻画；根本体已有锚点
	// deny、保护路径本体的 rename 已被 literal require-not 拦，补的是中间层。
	// codex protected_ancestors 同款语义，收官二轮审查 B-2 补齐）。
	ProtectedAncestors []string
}

// protectedAncestors 保护路径变体在可写根内的中间祖先目录（不含根本体）。
// 纯路径数学（跨平台可单测）；输入应为已规范化的绝对路径。
func protectedAncestors(root string, variants []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range variants {
		for cur := path.Dir(v); cur != root && withinSeatbeltRoot(root, cur) && cur != "/"; cur = path.Dir(cur) {
			if !seen[cur] {
				seen[cur] = true
				out = append(out, cur)
			}
		}
	}
	return out
}

// withinSeatbeltRoot p 是否严格位于 root 子树内（root 自身不算；root 为 "/"
// 时任意绝对路径均在其下）。
func withinSeatbeltRoot(root, p string) bool {
	return root == "/" || strings.HasPrefix(p, root+"/")
}

// buildSeatbeltProfile 组装 profile 文本与 -D 参数表。
//
// 组装顺序（真源 §3，codex seatbelt.rs 同构）：base（deny default）→
// file-read 段（全盘读）→ file-write 段（danger=regex 全盘/其余=可写根
// 子树；ProtectedReadOnly 经 require-not 排除——Seatbelt 可实现真回盖，
// 与 Landlock 的 partial 语义不同）→ 网络段（断网 = 不追加任何 allow 即
// 全断；启用 = network-outbound/inbound + DNS/mach-lookup 白名单）→
// 可写根锚点 deny（防围栏内进程替换授权边界目录本体，codex 同款）。
func buildSeatbeltProfile(mode Mode, network bool, roots []seatbeltWriteRoot) (string, [][2]string, error) {
	if err := (&Policy{Mode: mode}).Validate(); err != nil {
		return "", nil, err
	}
	var params [][2]string
	var sections []string
	sections = append(sections, seatbeltBasePolicy)
	sections = append(sections, "; allow read-only file operations\n(allow file-read*)")

	// file-write 段：read-only 档不追加（base 的 /dev/null 除外）。
	var components []string
	var anchorDenies []string
	switch mode {
	case ModeReadOnly:
		// 无可写根
	case ModeDangerFullAccess:
		if len(roots) == 0 {
			// 全盘写（codex 注：此形比 (allow file-write*) 更宽松高效）
			sections = append(sections, `(allow file-write* (regex #"^/"))`)
			break
		}
		// danger + ProtectedReadOnly：退化为「/ 子树 + 排除」形（codex 同款路径）
		fallthrough
	case ModeWorkspaceWrite:
		for i, root := range roots {
			key := fmt.Sprintf("WRITABLE_ROOT_%d", i)
			params = append(params, [2]string{key, root.Path})
			filter := fmt.Sprintf("(subpath (param %q))", key)
			if root.Literal {
				filter = fmt.Sprintf("(literal (param %q))", key)
			} else {
				// 授权边界目录本体不可替换（rename/rmdir 会带着围栏语义跑路）
				anchorDenies = append(anchorDenies,
					fmt.Sprintf("(deny file-write-unlink (require-all (literal (param %q)) (vnode-type DIRECTORY)))", key))
			}
			require := filter
			for j, prot := range root.Protected {
				pkey := fmt.Sprintf("WRITABLE_ROOT_%d_PROTECTED_%d", i, j)
				params = append(params, [2]string{pkey, prot})
				// 同时排除精确路径与其子树——subpath 单用会漏「保护目录本体的首次创建」
				require += fmt.Sprintf(" (require-not (literal (param %q)))", pkey)
				require += fmt.Sprintf(" (require-not (subpath (param %q)))", pkey)
			}
			if len(root.Protected) > 0 {
				require = "(require-all " + require + ")"
			}
			components = append(components, require)
		}
		if len(components) > 0 {
			sections = append(sections, "(allow file-write*\n"+strings.Join(components, " ")+"\n)")
		}
	}

	if network {
		sections = append(sections,
			"(allow network-outbound)\n(allow network-inbound)\n"+seatbeltNetworkPolicy)
	}
	sections = append(sections, anchorDenies...)
	// 保护祖先 deny 收尾（codex seatbelt.rs protected_ancestors 同款，注释原话
	// 「Keep these denies last so no broader allowance can reopen the unlink
	// operation used by rename」）：跨根去重后逐个 deny rename/unlink。
	ancSeen := map[string]bool{}
	for _, root := range roots {
		for _, anc := range root.ProtectedAncestors {
			if ancSeen[anc] {
				continue
			}
			ancSeen[anc] = true
			key := fmt.Sprintf("PROTECTED_ANCHOR_%d", len(ancSeen)-1)
			params = append(params, [2]string{key, anc})
			sections = append(sections,
				fmt.Sprintf("(deny file-write-unlink (require-all (vnode-type DIRECTORY) (literal (param %q))))", key))
		}
	}
	return strings.Join(sections, "\n"), params, nil
}
