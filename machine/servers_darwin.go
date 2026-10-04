// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

const seesServers = true

// lsofPath is macOS's lsof, named by its path so a command's PATH does
// not decide which program reads the sockets.
const lsofPath = "/usr/sbin/lsof"

// lsofTimeout bounds one read of a group's sockets; lsof answers in tens
// of milliseconds.
const lsofTimeout = 5 * time.Second

// serverPorts asks lsof for the TCP sockets in state LISTEN of the
// processes of group pgid. lsof exits 1 with nothing printed when no
// process matches, which is no server.
func serverPorts(ctx context.Context, pgid int) ([]int, error) {
	ctx, cancel := context.WithTimeout(ctx, lsofTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, lsofPath, "-nP", "-a", "-g", strconv.Itoa(pgid), "-iTCP", "-sTCP:LISTEN", "-Fn").Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("machine: read the listening sockets of process group %d: %w", pgid, err)
	}
	return lsofPorts(out, darwinLow()), nil
}

// darwinLow is the low bound of macOS's ephemeral range, or the IANA
// bound macOS ships with when the setting cannot be read.
func darwinLow() int {
	low, err := syscall.SysctlUint32("net.inet.ip.portrange.first")
	if err != nil || low == 0 {
		return darwinEphemeralLow
	}
	return int(low)
}
