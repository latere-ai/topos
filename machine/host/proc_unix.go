// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package host

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// Shell runs every command. It is named by its absolute path, so a
// command does not depend on the PATH it was started with.
const Shell = "/bin/sh"

// shell is the shell a command runs under.
func shell() (string, error) { return Shell, nil }

// group is the process group a command runs in. The shell leads it, so
// the shell's pid is the group's id, and a signal to the group reaches
// every process the shell started that did not leave it.
type group struct{ pgid int }

// prepare makes cmd start a process group of its own.
func prepare(cmd *exec.Cmd) (*group, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &group{}, nil
}

// started records the group of a command that has started.
func (g *group) started(cmd *exec.Cmd) error {
	g.pgid = cmd.Process.Pid
	return nil
}

// terminate asks every process in the group to end, with SIGTERM.
func (g *group) terminate() error { return signalGroup(g.pgid, syscall.SIGTERM) }

// kill ends every process in the group, with SIGKILL.
func (g *group) kill() error { return signalGroup(g.pgid, syscall.SIGKILL) }

// close releases what the group holds; a process group holds nothing
// in this process.
func (g *group) close() error { return nil }

// signalGroup signals a process group. A group already gone is not an
// error: ESRCH, or EPERM, which macOS returns for a group whose
// remaining members are zombies waiting to be reaped.
func signalGroup(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("machine: signal process group %d: %w", pgid, err)
	}
	return nil
}
