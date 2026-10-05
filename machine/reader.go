// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"errors"
	"io"
)

// FileReader is the read half of a machine's files: what a reader
// outside the session's drive, such as the API serving a session's
// files (spec 044), reads of its working directory. A path is absolute
// or relative to the working directory; one outside the working
// directory is ErrOutside, one the credential deny-list names ErrDenied,
// and one that does not exist fs.ErrNotExist.
type FileReader interface {
	Stat(ctx context.Context, path string) (FileInfo, error)
	ReadFile(ctx context.Context, path string) (io.ReadCloser, error)
}

// ErrNotRunning is a session whose machine is not running now: never
// opened, stopped while the session was idle, or gone. A reader outside
// the session's drive never starts or creates a machine, so what is in
// it cannot be read until the session runs it again.
var ErrNotRunning = errors.New("machine: the session's machine is not running")
