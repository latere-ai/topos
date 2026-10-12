// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"

	"latere.ai/x/topos/session"
)

// Files is one session's folder of a files store (spec 068), beside its
// blobs: the files a person attaches to a message and the files an answer
// shows or links are written there, each at a ref relative to the folder,
// and read back as a stream. Nothing deletes: a session's folder is the
// installation's to remove. The session's own records, model requests and
// responses, bundles and instructions, stay blobs. Its one implementation
// is a client of an Arca-compatible files API, which toposd builds.
type Files interface {
	// Put writes exactly size bytes of r at ref, create-only, as they
	// are read, and answers their digest. A ref already taken is
	// ErrFileExists, and nothing is written.
	Put(ctx context.Context, ref, mediaType string, size int64, r io.Reader) (session.Digest, error)
	// Open answers the file at ref as a stream and its length, -1 when
	// the files store does not say. Nothing at ref is ErrFileNotFound.
	Open(ctx context.Context, ref string) (io.ReadCloser, int64, error)
	// CopyFolder copies the folder dir of from, a folder of the same
	// space of the same files service, to the same place in this folder,
	// on the server. A dir with nothing under it is ErrFileNotFound.
	CopyFolder(ctx context.Context, from session.Folder, dir string) error
}

// CodeFilesUnavailable is the code of a file of a session's folder of a
// files store that could not be written, read or copied, or a session
// with a folder on a runner that reaches no files store (spec 068).
const CodeFilesUnavailable = "files_unavailable"

// The answers of Files besides a refusal.
var (
	// ErrFileExists is a create-only write onto a path that is taken.
	ErrFileExists = errors.New("harness: the files store's path is taken")
	// ErrFileNotFound is a read or a copy of nothing.
	ErrFileNotFound = errors.New("harness: nothing is at the files store's path")
	// ErrFileSizeChanged is a source that held more or fewer bytes than
	// the size its write named.
	ErrFileSizeChanged = errors.New("harness: the source is not the size its write named")
)

// FileError is a refusal of the files service: the call, its HTTP
// status, the service's code, and its developer detail.
type FileError struct {
	Op     string
	Status int
	Code   string
	Detail string
}

func (e *FileError) Error() string {
	return fmt.Sprintf("harness: the files store's %s answered %d %s: %s", e.Op, e.Status, e.Code, e.Detail)
}

// CodeObjectTooLarge is the code an Arca-compatible files API refuses an
// object past what it takes with, with the status StatusObjectTooLarge,
// HTTP's 413: the harness dials nothing, so it names the number itself.
const (
	CodeObjectTooLarge   = "object_too_large"
	StatusObjectTooLarge = 413
)

// FileTooLarge reports whether err is a refusal of a write for the size
// of what it writes.
func FileTooLarge(err error) bool {
	e, ok := errors.AsType[*FileError](err)
	return ok && e.Status == StatusObjectTooLarge && e.Code == CodeObjectTooLarge
}
