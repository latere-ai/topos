// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"io"
)

// ExitKilled is the exit code of a command a kill signal ended, as a
// shell and a container runtime report it: 128 and the number of SIGKILL.
// A machine that runs out of memory ends a process this way, and so does
// anything else that kills one, so the code alone names no cause.
const ExitKilled = 128 + 9

// Restart is a machine that stopped on a failure and runs again as the
// same machine (spec 009): the files of its working directory are kept,
// and every process that ran on it is gone.
type Restart struct {
	// OutOfMemory is true when the machine's provider says it stopped
	// because it ran out of memory. False says nothing about why.
	OutOfMemory bool
}

// Stop is what became of a machine that stopped under a running command
// (spec 009). Exactly one of Restarted, Lost and Err says where the
// machine stands when the command's result is returned.
type Stop struct {
	// OutOfMemory is true when the machine's provider says it stopped
	// because it ran out of memory. False says nothing about why.
	OutOfMemory bool
	// Restarted is true when the machine runs again as the same machine:
	// the files of its working directory are kept, and every process that
	// ran on it is gone.
	Restarted bool
	// Lost is true when the machine could not be started again and is
	// gone for the rest of the drive: its files are not reached again.
	Lost bool
	// Err is why the machine does not run yet when it is neither
	// restarted nor lost. Nothing of it was deleted, and the next call
	// tries to start it again.
	Err error
}

// Recovering is the optional interface of a machine that says what its
// open found and tells of each restart after it (spec 009), so a runner
// records a machine that restarted with its files apart from one that
// was replaced without them.
type Recovering interface {
	// Created reports whether the open made the machine new, with an
	// empty working directory. For a session that had a machine before,
	// what that machine held is gone.
	Created() bool
	// Restarted reports the restart the open made: it found the machine
	// failed and started it again.
	Restarted() (Restart, bool)
	// OnRestart sets the hook the machine calls after each later restart,
	// once it runs again and before the operation that found it stopped
	// goes on. A failed hook fails that operation; the machine stays as
	// it is.
	OnRestart(hook func(ctx context.Context, r Restart) error)
}

// Importer is the optional interface of a machine that takes many files
// in one call: ImportTar extracts a tar stream of regular files and
// directories below dest, a directory of the machine, creating it and
// what the archive names.
type Importer interface {
	ImportTar(ctx context.Context, dest string, r io.Reader) error
}
