//go:build linux && (amd64 || arm64)

// seccomp BPF 断网围栏（手写经典 BPF，x/sys/unix 原语）。deny 清单逐条照抄
// codex linux-sandbox/landlock.rs L179-216：recvfrom 故意放行（socketpair+
// 子进程管理的工具兼容性，codex 注释原话照搬）；connect 无条件 deny 连
// AF_UNIX 也断——防 docker.sock 类逃逸的故意设计（codex 同款，真源 §2.3
// D3 披露：docker CLI / psql -h /sock 类本地 socket 客户端同断）。
// default allow + 命中返回 EPERM；audit arch 不符（32 位兼容模式号表错位）
// KILL_PROCESS fail-closed。号表仅 x86_64/aarch64（x/sys per-arch 常量），
// 其余架构由 backend_stub 运行时桩报 unusable（审查 B2）。
package sandbox

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// seccomp_data 字偏移。
const (
	sdNr   = 0x0  // syscall 号
	sdArch = 0x4  // audit 架构字
	sdArg0 = 0x10 // args[0] 低 32 位
)

// seccompArch 本架构 audit arch（架构门——不符即 32 位兼容模式，号表错位）。
func seccompArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, nil
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, nil
	}
	return 0, fmt.Errorf("unsupported arch %s（号表仅 x86_64/aarch64）", runtime.GOARCH)
}

// seccompDenyAlways 无条件 deny（进程窥视与 io_uring 逃逸面，两档都禁）。
func seccompDenyAlways() []uintptr {
	return []uintptr{
		unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV,
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	}
}

// seccompDenyNet 断网 deny（recvfrom 不在表 = 故意放行）。
func seccompDenyNet() []uintptr {
	return []uintptr{
		unix.SYS_CONNECT, unix.SYS_ACCEPT, unix.SYS_ACCEPT4, unix.SYS_BIND,
		unix.SYS_LISTEN, unix.SYS_GETPEERNAME, unix.SYS_GETSOCKNAME, unix.SYS_SHUTDOWN,
		unix.SYS_SENDTO, unix.SYS_SENDMMSG, unix.SYS_RECVMMSG,
		unix.SYS_GETSOCKOPT, unix.SYS_SETSOCKOPT,
	}
}

// installSeccompFilter 装 filter（前提 NO_NEW_PRIVS 已置；filter 随
// fork/exec 继承）。
func installSeccompFilter() error {
	arch, err := seccompArch()
	if err != nil {
		return err
	}
	prog, err := buildSeccompProg(arch)
	if err != nil {
		return err
	}
	fp := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	return unix.Prctl(unix.PR_SET_SECCOMP, uintptr(unix.SECCOMP_MODE_FILTER),
		uintptr(unsafe.Pointer(&fp)), 0, 0)
}

// buildSeccompProg 组装过滤程序：架构门 → 号匹配 deny 表 → socket 族
// AF_UNIX 条件 → default allow。
func buildSeccompProg(auditArch uint32) ([]unix.SockFilter, error) {
	b := newBpfBuilder()
	b.stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, sdArch)
	b.jeq(uint32(auditArch), "loadNr", "kill")
	b.mark("loadNr")
	b.stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, sdNr)
	for _, nr := range seccompDenyAlways() {
		b.jeq(uint32(nr), "deny", "")
	}
	for _, nr := range seccompDenyNet() {
		b.jeq(uint32(nr), "deny", "")
	}
	// socket/socketpair：arg0 == AF_UNIX 放行、否则 EPERM（codex 条件规则同款）
	b.jeq(uint32(unix.SYS_SOCKET), "sock", "")
	b.jeq(uint32(unix.SYS_SOCKETPAIR), "sock", "")
	b.stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW)
	b.mark("sock")
	b.stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, sdArg0)
	b.jeq(uint32(unix.AF_UNIX), "allow", "deny")
	b.mark("allow")
	b.stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW)
	b.mark("deny")
	b.stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM))
	b.mark("kill")
	b.stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS)
	return b.finish()
}

// bpfBuilder 极简标签汇编器（经典 BPF jt/jf 为 8 位相对跳转——标签回填免
// 手算偏移；"" 标签 = 顺延下一条）。
type bpfBuilder struct {
	insns  []unix.SockFilter
	marks  map[string]int
	fixups []bpfFixup
}

type bpfFixup struct {
	at    int // 跳转指令下标
	field int // 0=Jt 1=Jf
	label string
}

func newBpfBuilder() *bpfBuilder { return &bpfBuilder{marks: map[string]int{}} }

func (b *bpfBuilder) stmt(code, k uint32) {
	b.insns = append(b.insns, unix.SockFilter{Code: uint16(code), K: k})
}

func (b *bpfBuilder) jeq(k uint32, jt, jf string) {
	at := len(b.insns)
	b.insns = append(b.insns, unix.SockFilter{Code: uint16(unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K), K: k})
	if jt != "" {
		b.fixups = append(b.fixups, bpfFixup{at: at, field: 0, label: jt})
	}
	if jf != "" {
		b.fixups = append(b.fixups, bpfFixup{at: at, field: 1, label: jf})
	}
}

func (b *bpfBuilder) mark(name string) { b.marks[name] = len(b.insns) }

func (b *bpfBuilder) finish() ([]unix.SockFilter, error) {
	for _, f := range b.fixups {
		target, ok := b.marks[f.label]
		if !ok {
			return nil, fmt.Errorf("bpf: 未定义标签 %s", f.label)
		}
		off := target - (f.at + 1)
		if off < 0 || off > 255 {
			return nil, fmt.Errorf("bpf: 跳转越界 %s(%d)", f.label, off)
		}
		if f.field == 0 {
			b.insns[f.at].Jt = uint8(off)
		} else {
			b.insns[f.at].Jf = uint8(off)
		}
	}
	return b.insns, nil
}
