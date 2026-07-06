//go:build windows

package agent

import "os/exec"

func setProcessGroup(_ *exec.Cmd)            {}
func signalProcessGroup(_ *exec.Cmd, _ bool) {}
