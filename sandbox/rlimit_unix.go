//go:build darwin || linux

// rlimit 资源限额（helper 内 exec 前 setrlimit，真源 §2.4）：NPROC 默认 512
// ——内核按有效 uid 计数且含线程，容器形态下服务进程 Go 运行时线程与同
// uid 任务共享同一预算，256 会被「服务高线程 + go build -p 并行 fork」打穿
// 且报 EAGAIN 不命中拒绝签名；FSIZE 默认 1GB 防写满挂载卷；CORE=0。跳过
// RLIMIT_AS——Go 工具链运行时/链接器 GB 级虚拟地址预留会被误杀，内存硬
// 限额归 cgroup/容器层。
package sandbox

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func applyRlimits(l Limit) error {
	nproc := l.NProc
	if nproc == 0 {
		nproc = DefaultNProc
	}
	fsizeMB := l.FileSizeMB
	if fsizeMB == 0 {
		fsizeMB = DefaultFileSizeMB
	}
	if err := setRlimitClamp(unix.RLIMIT_NPROC, uint64(nproc)); err != nil {
		return fmt.Errorf("RLIMIT_NPROC: %w", err)
	}
	if err := setRlimitClamp(unix.RLIMIT_FSIZE, uint64(fsizeMB)<<20); err != nil {
		return fmt.Errorf("RLIMIT_FSIZE: %w", err)
	}
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}); err != nil {
		return fmt.Errorf("RLIMIT_CORE: %w", err)
	}
	return nil
}

// setRlimitClamp 目标超既有硬限即钳到硬限（更严不更松——非特权不可升硬限，
// 钳制保持限额生效而非失败）。
func setRlimitClamp(res int, target uint64) error {
	var cur unix.Rlimit
	if err := unix.Getrlimit(res, &cur); err != nil {
		return err
	}
	if cur.Max < target {
		target = cur.Max
	}
	return unix.Setrlimit(res, &unix.Rlimit{Cur: target, Max: target})
}
