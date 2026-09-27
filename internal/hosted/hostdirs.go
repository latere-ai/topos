// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"latere.ai/x/topos/machine"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// HostSessionsDir is the directory under the data directory that holds
// each host session's working directory, named by its id, and beside
// it the session's spill and stage directories.
const HostSessionsDir = "host-sessions"

// hostDirs are a host session's working, spill and stage directories.
func hostDirs(dataDir, id string) (work, spill, stages string, err error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return "", "", "", fmt.Errorf("%q is not a session id", id)
	}
	work = filepath.Join(dataDir, HostSessionsDir, id)
	return work, work + ".spill", work + ".stages", nil
}

// RemoveHostSession removes the directories of the host session id
// under the data directory: its working, spill and stage directories.
// A session that never ran on this host has none, which is not an
// error.
func RemoveHostSession(dataDir, id string) error {
	work, spill, stages, err := hostDirs(dataDir, id)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range []string{work, spill, stages} {
		if err := os.RemoveAll(d); err != nil {
			errs = append(errs, fmt.Errorf("host sessions: remove %s: %w", d, err))
		}
	}
	return errors.Join(errs...)
}

// ByKind opens each session's machine by the kind it asks for: a host
// machine through host, which is nil when TOPOS_HOST_SESSIONS is off,
// and every other kind through cella.
func ByKind(cella, host Machines) Machines {
	return func(ctx context.Context, s session.Session, m v1.Machine) (machine.Machine, error) {
		if cmp.Or(s.Machine.Kind, m.Kind) != session.MachineHost {
			return cella(ctx, s, m)
		}
		if host == nil {
			return nil, setup(CodeMachineUnavailable, errors.New("TOPOS_HOST_SESSIONS is off, so this server runs no session on its own host"))
		}
		return host(ctx, s, m)
	}
}
