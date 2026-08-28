//go:build darwin

// macOS 后端（Seatbelt，真源 §3）：fs/网络围栏经 /usr/bin/sandbox-exec
// 包裹施加（它 spawn 真实命令），资源限额仍走哨兵 helper（rlimit 在 exec
// sandbox-exec 前置、随进程链继承）。路径纪律（codex 防提权规则照搬）：
// 只解析顶层系统别名（/tmp → /private/tmp），嵌套 symlink 组件拒绝——
// 深层组件可被围栏内进程篡改，跟随后解析会把路径检查变成新的授权授予。
// 验证口径：编译级 + profile 纯字符串单测（seatbelt_profile_test.go）；
// 本环境无 Mac，运行时未验（诚实声明，docs/09 同写）。
package sandbox

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// sandbox-exec 固定 /usr/bin 路径（codex 同款：不查 PATH，防注入恶意副本；
// /usr/bin 副本被换即攻击者已有 root，非本层威胁模型）。
const seatbeltExecPath = "/usr/bin/sandbox-exec"

// seatbeltPrepWarn 构造失败告警（进程一次——auto 语义裸跑降级，与 Probe
// unusable 告警同款节流）。
var seatbeltPrepWarn sync.Once

// wrapOSBackend darwin 分支：Seatbelt 包裹形。哨兵 → sandbox-exec
// -p <profile> -DKEY=V … -- sh -c <cmd>；fs/网络围栏由 sandbox-exec 施加。
// 构造失败（嵌套 symlink 等）= auto 语义裸跑 + 告警（拒跑须显式 require）。
func wrapOSBackend(pol *Policy, workspace, cmdLine string) ([]string, []string) {
	roots, err := seatbeltRoots(pol, workspace)
	if err == nil {
		var profile string
		var params [][2]string
		profile, params, err = buildSeatbeltProfile(pol.Mode, pol.Network, roots)
		if err == nil {
			argv := []string{selfExe, sentinelCmd, "--", seatbeltExecPath, "-p", profile}
			for _, kv := range params {
				argv = append(argv, "-D"+kv[0]+"="+kv[1])
			}
			argv = append(argv, "--", "sh", "-c", cmdLine)
			return argv, payloadEnv(pol, workspace)
		}
	}
	seatbeltPrepWarn.Do(func() {
		log.Printf("einox-sandbox: Seatbelt 策略构造失败（%v）——命令将裸跑（auto 档降级）", err)
	})
	return nil, nil
}

// applySandbox darwin：fs/网络围栏不在 helper 内施加（sandbox-exec 包裹已
// 覆盖——argv 已被 wrapOSBackend 包住），仅置资源限额。
func applySandbox(p *policyPayload) error {
	return applyRlimits(p.Limit)
}

// probeEnforceChild 后端实测（真源 §1.3 ②）：read-only + 断网 profile 包裹
// 探针写围栏外路径——写被拒 = 围栏真实生效（dsh「自施加才算诚实信号」）。
// Seatbelt 对 fs/网络为全有或全无，无 partial 态。
func probeEnforceChild() int {
	if _, err := os.Stat(seatbeltExecPath); err != nil {
		fmt.Fprintf(os.Stderr, "einox-sandbox: %s 不可用（%v）\n", seatbeltExecPath, err)
		return helperFail
	}
	profile, params, err := buildSeatbeltProfile(ModeReadOnly, false, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "einox-sandbox: %v\n", err)
		return helperFail
	}
	out := filepath.Join(os.TempDir(), fmt.Sprintf("einox-sbx-probe-%d", os.Getpid()))
	_ = os.Remove(out)
	defer os.Remove(out)
	argv := []string{seatbeltExecPath, "-p", profile}
	for _, kv := range params {
		argv = append(argv, "-D"+kv[0]+"="+kv[1])
	}
	argv = append(argv, "--", selfExe, sentinelCmd, flagProbeWrite, out)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	runErr := cmd.Run()
	if runErr == nil {
		fmt.Fprintln(os.Stderr, "einox-sandbox: seatbelt 未拒绝只读档围栏外写——围栏失效")
		return helperFail
	}
	if _, statErr := os.Stat(out); statErr == nil {
		fmt.Fprintln(os.Stderr, "einox-sandbox: 只读档下探针文件落盘——围栏失效")
		return helperFail
	}
	if ee, ok := runErr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		// 探针以退出码 1 报「写被拒」= 围栏生效；abi=0 为协议占位（无 ABI 协商概念）
		fmt.Println("einox-sandbox-enforce full abi=0")
		return 0
	}
	fmt.Fprintf(os.Stderr, "einox-sandbox: sandbox-exec 自身失败（%v）\n", runErr)
	return helperFail
}

// seatbeltRoots 从策略推导可写根表（含规范化/去重/literal 判定/保护子路径
// 归并）。read-only 档无可写根；裸 danger 走 regex 全盘写（无根表）；danger
// + ProtectedReadOnly 退化为「/ 子树 + 排除」形（codex 同款路径）。
func seatbeltRoots(pol *Policy, workspace string) ([]seatbeltWriteRoot, error) {
	if pol.Mode == ModeReadOnly {
		return nil, nil
	}
	if pol.Mode == ModeDangerFullAccess && len(pol.ProtectedReadOnly) == 0 {
		return nil, nil
	}
	var paths []string
	if pol.Mode == ModeDangerFullAccess {
		paths = []string{"/"}
	} else {
		paths = append([]string{workspace}, pol.WritableRoots...)
		if !pol.ExcludeSlashTmp {
			paths = append(paths, "/tmp") // 默认计入（审查 A1；排除须显式声明）
		}
		if !pol.ExcludeTmpdir {
			paths = append(paths, os.TempDir())
		}
	}
	roots := make([]seatbeltWriteRoot, 0, len(paths))
	seen := map[string]bool{}
	for _, p := range paths {
		np, err := normalizeSeatbeltPath(p)
		if err != nil {
			return nil, fmt.Errorf("可写根 %s: %w", p, err)
		}
		if seen[np] {
			continue
		}
		seen[np] = true
		root := seatbeltWriteRoot{Path: np}
		if fi, err := os.Lstat(np); err == nil && !fi.IsDir() {
			root.Literal = true // 现存非目录文件 = 字面量匹配
		}
		roots = append(roots, root)
	}
	for _, prot := range pol.ProtectedReadOnly {
		// 保护子路径取「原始形 + 顶层别名规范化形 + 完全解析形」变体（codex
		// 同款：symlink 语义下多种访问路径都要排到）；规范化失败保留原样——
		// 保护面宁多勿漏，不为保护路径的形态问题否掉整个沙箱。**归属判定与
		// 排除参数同用变体集**（收官二轮审查 C-1：别名拼写的保护路径——如
		// /var/... 形而根已归一为 /private/var/...——用单一原始形匹配会静默
		// 漏保）。
		variants := []string{prot}
		var norm []string
		if np, err := normalizeSeatbeltPath(prot); err == nil {
			if np != prot {
				variants = append(variants, np)
			}
			norm = append(norm, np)
		}
		if resolved, err := filepath.EvalSymlinks(prot); err == nil {
			if rp, err := normalizeSeatbeltPath(resolved); err == nil {
				variants = append(variants, rp)
				norm = append(norm, rp)
			}
		}
		best := -1
		for i, r := range roots {
			for _, v := range variants {
				if pathWithinSeatbelt(r.Path, v) && (best < 0 || len(r.Path) > len(roots[best].Path)) {
					best = i
					break
				}
			}
		}
		if best < 0 {
			continue // 不落在任何可写根内 = 本就只读（danger 形必命中 / 根）
		}
		roots[best].Protected = append(roots[best].Protected, variants...)
		// 保护路径的根内中间祖先 → deny rename/unlink（B-2：改名中间祖先会把
		// 保护子路径搬出 require-not 刻画；规范化失败的原样形不做祖先推演——
		// 未归一路径逐级上溯不可靠，保护面以排除变体兜底）。
		for _, anc := range protectedAncestors(roots[best].Path, norm) {
			if !slices.Contains(roots[best].ProtectedAncestors, anc) {
				roots[best].ProtectedAncestors = append(roots[best].ProtectedAncestors, anc)
			}
		}
	}
	return roots, nil
}

// pathWithinSeatbelt p 是否位于 root 子树内（含 root 本身）。
func pathWithinSeatbelt(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// normalizeSeatbeltPath 路径规范化：嵌套 symlink 组件拒绝 + 顶层系统别名
// 解析（/tmp → /private/tmp、/var → /private/var）。
func normalizeSeatbeltPath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("须绝对路径")
	}
	clean := filepath.Clean(p)
	if bad, ok := nestedSymlinkComponent(clean); ok {
		return "", fmt.Errorf("包含嵌套 symlink 组件 %s（顶层别名除外）", bad)
	}
	return normalizeTopLevelAlias(clean)
}

// nestedSymlinkComponent 深层（≥2 层）symlink 组件探测——含路径本体；顶层
// 组件（父为 /）豁免（codex nested_symlink_component 同构）。
func nestedSymlinkComponent(p string) (string, bool) {
	for cur := p; ; {
		if parent := filepath.Dir(cur); parent != "/" {
			if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				return cur, true
			}
		}
		next := filepath.Dir(cur)
		if next == "/" || next == "." || next == cur {
			return "", false
		}
		cur = next
	}
}

// normalizeTopLevelAlias 只解析顶层组件的系统别名（codex 同构）。
func normalizeTopLevelAlias(p string) (string, error) {
	first := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 2)[0]
	if first == "" {
		return p, nil
	}
	top := "/" + first
	fi, err := os.Lstat(top)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return p, nil // 非别名或不可查（不存在路径的可写根合法——留待首次创建）
	}
	resolved, err := filepath.EvalSymlinks(top)
	if err != nil {
		return "", fmt.Errorf("顶层别名 %s 解析失败: %w", top, err)
	}
	return filepath.Join(resolved, strings.TrimPrefix(p, top)), nil
}
