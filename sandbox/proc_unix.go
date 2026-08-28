//go:build unix

// 进程组治理（沙箱形态进程组杀的前提——修 stopTask 只杀单进程瑕疵的
// 沙箱路径部分，真源 §2.1）。
package sandbox

import (
	"os"
	"os/exec"
	"syscall"
)

// SetGroupLeader 设置进程组组长（子进程独立进程组——组杀锚点）。
func SetGroupLeader(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// KillGroup 进程组杀（负 pid SIGKILL 整组终结——后台任务/超时通道同款）。
// 未组化（同调用方进程组，负 pid 无组可寻 ESRCH）或已死则回退单进程杀。
func KillGroup(p *os.Process) {
	if p == nil {
		return
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err == nil {
		return
	}
	_ = p.Kill()
}

// syscallExec execve 替换当前进程（helper 末步；策略已施加，成功不返回）。
// PATH 查找须先做——execve 不做查找。
func syscallExec(argv []string, env []string) error {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, env)
}

// AttachToken unix 平台 no-op（windows 专用侧挂：restricted token 进
// SysProcAttr；unix 侧进程属性经 SetGroupLeader 完成）。
func AttachToken(*exec.Cmd, *Policy, string) error { return nil }
