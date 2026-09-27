// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"fmt"

	"latere.ai/x/topos/machine"
)

// Sandbox is the host sandbox a server runs every command of a host
// machine in, one stage per command (spec 009). latere.ai/x/pkg/hostsandbox
// has no driver for Windows, which has none of Seatbelt, Bubblewrap and
// Landlock, so on Windows the type carries only the policy fields and
// Open refuses a machine that sets one.
type Sandbox struct {
	// StageDir holds one directory per command.
	StageDir string
	// Denied are paths no command may read.
	Denied []string
	// Egress are the hosts commands and fetches may reach.
	Egress []string
}

// errNoSandbox refuses a sandboxed host machine on Windows.
var errNoSandbox = fmt.Errorf("machine: the host sandbox has no driver on windows, so this host cannot run a command in one: %w", errors.ErrUnsupported)

// stageJob is a background job running as a stage, of which Windows
// has none, since Open refuses a sandbox.
type stageJob struct{}

// openSandbox refuses the sandbox Open was given.
func (h *Host) openSandbox() error { return errNoSandbox }

// startStage refuses a stage; Open refuses the sandbox first.
func (h *Host) startStage(context.Context, machine.ExecRequest) (machine.ExecStream, error) {
	return nil, errNoSandbox
}

// stageBackground refuses a stage; Open refuses the sandbox first.
func (h *Host) stageBackground(context.Context, machine.ExecRequest) (machine.ExecResult, error) {
	return machine.ExecResult{}, errNoSandbox
}

// releaseStages has no stage to stop, since Open refuses a sandbox.
func (h *Host) releaseStages(context.Context) error { return nil }
