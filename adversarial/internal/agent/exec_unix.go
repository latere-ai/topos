//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalProcessGroup(cmd *exec.Cmd, interrupt bool) {
	if cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}
	sig := syscall.SIGKILL
	if interrupt {
		sig = syscall.SIGINT
	}
	_ = syscall.Kill(-pgid, sig)
}
