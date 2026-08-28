//go:build linux && (amd64 || arm64)

// Landlock 文件系统围栏——dsh native/landlock-run main.c（C11）的 Go 直译
// （第一参照）。UAPI 经 x/sys/unix（syscall 号/访问位常量 = 内核稳定契约，
// 不依赖内核头），属性结构自携最小形并显式传内核 size——x/sys 结构含
// ABI4/6 字段（24 字节），旧内核自身 struct 只有 8 字节时 size 超限直接
// E2BIG。规则只能授不能收（allow-list 联合语义）：可写根内只读回盖
// （ProtectedReadOnly，如 .git）做不到——探测面如实报 partial + 未覆盖项
// （真源 §2.2）。
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// landlockMaxABI 本构建知晓的最新 ABI（5 = IOCTL_DEV；ABI 协商向下裁位）。
const landlockMaxABI = 5

// 只读授予面（dsh --ro = read+execute——保留工具链可执行性）。
const landlockReadSide = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR

// ABI1 访问位全集（bits 0..12）。
const landlockABI1Mask = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR |
	unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
	unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
	unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_SYM

// 非目录 grant 保留的文件兼容位（内核拒目录专属位 EINVAL——/dev/null 形态）。
const landlockFileBits = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE |
	unix.LANDLOCK_ACCESS_FS_IOCTL_DEV

// 自携最小属性结构（dsh 同款策略：布局逐字节对齐内核 UAPI）。
type llRulesetAttr struct{ handled uint64 }

// llPathBeneathAttr 12 字节逻辑形（add_rule 无 size 参数，内核按自身 struct
// 16B 含尾 pad 读取——多读的 4B 为 pad 被忽略，dsh packed 12B 同款形态）。
type llPathBeneathAttr struct {
	allowed  uint64
	parentFd int32
}

// llRulesetSize 内核取 min(size, 自身 struct) 且要求 ≥ 首字段末尾——8 字节通吃全 ABI。
const llRulesetSize = 8

func ptrOf[T any](p *T) uintptr { return uintptr(unsafe.Pointer(p)) }

func llCreateRuleset(attr *llRulesetAttr, size uintptr, flags uintptr) (uintptr, unix.Errno) {
	r1, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, ptrOf(attr), size, flags)
	return r1, e
}

func llAddRule(fd int, attr *llPathBeneathAttr) unix.Errno {
	_, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(fd),
		unix.LANDLOCK_RULE_PATH_BENEATH, ptrOf(attr), 0, 0, 0)
	return e
}

func llRestrictSelf(fd int) unix.Errno {
	_, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(fd), 0, 0)
	return e
}

// landlockMaskForABI 该 ABI 可治的 fs 访问位（handled 集——不在集内的访问
// 即不受围栏，旧 ABI 少位由探测面报 partial）。
func landlockMaskForABI(abi int) uint64 {
	mask := uint64(landlockABI1Mask)
	if abi >= 2 {
		mask |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		mask |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		mask |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return mask
}

// landlockABI 查询内核 ABI 版本（query 专用 flag，不建规则集）。
// ENOSYS=内核未含 / EOPNOTSUPP=编译了但 LSM 禁用 / EPERM=容器 seccomp 拦截。
func landlockABI() (int, error) {
	abi, errno := llCreateRuleset(nil, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0, fmt.Errorf("landlock_create_ruleset(VERSION): %w（ENOSYS=内核未含/EOPNOTSUPP=禁用/EPERM=容器 seccomp 拦）", errno)
	}
	if abi < 1 {
		return 0, fmt.Errorf("landlock ABI 版本异常: %d", abi)
	}
	return int(abi), nil
}

// landlockApply 按策略施加（前提 NO_NEW_PRIVS 已置——施加序在 seccomp 后）。
// fail-closed：任何 syscall 失败即 error 拒 exec，不接受半启用（dsh/codex
// 同款）。规则形状（真源 §2.2）：read-only = / 只 ro；workspace-write =
// / 整树 read+execute + 工作区 + WritableRoots + /dev/null + /tmp 与
// $TMPDIR（存在时，Exclude* 显式排除）rw——临时目录默认可写（审查 A1）。
// Landlock 规则为联合语义：/tmp 等可写根在 / 的 ro 之上并集生效。
func landlockApply(p *policyPayload) error {
	abiRaw, err := landlockABI()
	if err != nil {
		return err
	}
	abi := min(abiRaw, landlockMaxABI)
	handled := landlockMaskForABI(abi)

	fd, errno := llCreateRuleset(&llRulesetAttr{handled: handled}, llRulesetSize, 0)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	defer unix.Close(int(fd))

	ro := uint64(landlockReadSide) & handled
	if err := llAddPathRule(int(fd), "/", ro, false); err != nil {
		return err
	}
	if p.Mode == ModeWorkspaceWrite {
		rw := handled
		if p.Workspace != "" {
			if err := llAddPathRule(int(fd), p.Workspace, rw, false); err != nil {
				return err
			}
		}
		for _, root := range p.WritableRoots {
			if err := llAddPathRule(int(fd), root, rw, false); err != nil {
				return err
			}
		}
		if err := llAddPathRule(int(fd), "/dev/null", rw, false); err != nil {
			return err
		}
		if !p.ExcludeSlashTmp {
			if err := llAddPathRule(int(fd), "/tmp", rw, true); err != nil {
				return err
			}
		}
		if !p.ExcludeTmpdir {
			if td := os.Getenv("TMPDIR"); td != "" {
				if err := llAddPathRule(int(fd), td, rw, true); err != nil {
					return err
				}
			}
		}
	}
	if errno := llRestrictSelf(int(fd)); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

// llAddPathRule 加 path-beneath 规则（O_PATH 打开路径）。optional=true 时
// 路径不存在即跳过（临时目录「存在时」语义）；其余不可开 = fail-closed
// ——静默收窄授予面不可接受（dsh 同款）。非目录只留文件兼容位。
func llAddPathRule(rulesetFd int, path string, access uint64, optional bool) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		if optional && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("规则路径不可开 %s: %w", path, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err == nil && st.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &= uint64(landlockFileBits)
	}
	if errno := llAddRule(rulesetFd, &llPathBeneathAttr{allowed: access, parentFd: int32(fd)}); errno != 0 {
		return fmt.Errorf("landlock_add_rule(%s): %w", path, errno)
	}
	return nil
}
