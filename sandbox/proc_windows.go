//go:build windows

// Windows 进程组桩：无进程组语义。杀退化为单进程 TerminateProcess——超时/
// 停止只终结直接子进程，孙进程残留（孙进程继承 restricted token，围栏不破，
// 属存活性缺口非逃逸）；进程级硬约束归 Job Object（未做，真源 §11.10 记档）。
package sandbox

import (
	"errors"
	"os"
	"os/exec"
)

func SetGroupLeader(*exec.Cmd) {}

func KillGroup(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}

func syscallExec([]string, []string) error {
	return errors.New("沙箱 exec 仅 unix 形态（Windows 后端 = S-7）")
}
