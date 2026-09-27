// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"syscall"
	"time"

	"latere.ai/x/topos/machine"
)

// The frames of the run, read and write modes. A frame is one kind byte,
// the payload's length as four big-endian bytes, and the payload. The
// exec socket has no half close, so an input that ends is a frame of its
// own, and the command's output, its final directory and its exit travel
// apart in one byte stream. machine/cella holds the same table.
const (
	// Machine to helper.
	frameInput = 'i' // bytes of the command's standard input
	frameEOF   = 'e' // the input ended
	frameKill  = 'k' // cancel the command
	// Helper to machine.
	frameStart  = 's' // the command started, or the file opened
	frameFail   = 'f' // a failure, an errorBody
	frameOutput = 'o' // output bytes, or the file's bytes
	frameDir    = 'd' // the shell's final directory
	frameExit   = 'x' // the command ended, an exitBody; the read finished
)

// maxFrame bounds one frame's payload, the exec socket's own bound on a
// message.
const maxFrame = 1 << 20

// chunk is how much output one frame carries.
const chunk = 32 << 10

// frameWriter writes whole frames, one at a time: the output pump and the
// final frames of a command write from different goroutines.
type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (f *frameWriter) frame(kind byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := make([]byte, 5, 5+len(payload))
	b[0] = kind
	binary.BigEndian.PutUint32(b[1:], uint32(len(payload)))
	_, err := f.w.Write(append(b, payload...))
	return err
}

func (f *frameWriter) json(kind byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return f.frame(kind, b)
}

// readFrame reads one frame.
func readFrame(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("a frame of %d bytes is past the bound of %d", n, maxFrame)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return h[0], payload, nil
}

// errorBody is a failure the machine turns back into the error the host
// machine would have returned.
type errorBody struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// The kinds of failure, one per error the machine tells apart.
const (
	kindNotExist   = "not_exist"
	kindExist      = "exist"
	kindNotEmpty   = "not_empty"
	kindNotDir     = "not_dir"
	kindIsDir      = "is_dir"
	kindPermission = "permission"
	kindOutside    = "outside"
	kindDenied     = "denied"
	kindOther      = "other"
)

// failure renders an error with its kind.
func failure(err error) *errorBody {
	return &errorBody{Kind: kindOf(err), Message: err.Error()}
}

func kindOf(err error) string {
	var pe *fs.PathError
	switch {
	case errors.Is(err, machine.ErrOutside):
		return kindOutside
	case errors.Is(err, machine.ErrDenied):
		return kindDenied
	case errors.Is(err, fs.ErrNotExist):
		return kindNotExist
	case errors.Is(err, syscall.ENOTEMPTY):
		// Checked before ErrExist, which a directory with entries also
		// answers.
		return kindNotEmpty
	case errors.Is(err, fs.ErrExist):
		return kindExist
	case errors.Is(err, syscall.ENOTDIR):
		return kindNotDir
	case errors.Is(err, syscall.EISDIR):
		return kindIsDir
	case errors.Is(err, fs.ErrPermission):
		return kindPermission
	case errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "escapes"):
		// os.Root refuses a symlink or ".." that leaves the root with an
		// error that has no sentinel; the host machine reads it the same
		// way.
		return kindOutside
	}
	return kindOther
}

// exitBody is how a command ended.
type exitBody struct {
	Code     int  `json:"code"`
	TimedOut bool `json:"timedOut,omitempty"`
	Canceled bool `json:"canceled,omitempty"`
}

// entry is one file as the fs modes answer it: the whole mode, so a
// directory and a symlink keep their type bits.
type entry struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    uint32    `json:"mode"`
	ModTime time.Time `json:"modTime"`
	IsDir   bool      `json:"isDir"`
}

// jobBody names a started background job.
type jobBody struct {
	PID int    `json:"pid"`
	Log string `json:"log"`
}

// response is the one JSON answer of the search, job and fs modes: one of
// its fields is set.
type response struct {
	Result  *machine.SearchResult `json:"result,omitempty"`
	Entries []entry               `json:"entries,omitempty"`
	Job     *jobBody              `json:"job,omitempty"`
	Done    bool                  `json:"done,omitempty"`
	Error   *errorBody            `json:"error,omitempty"`
}

// answer writes a response and returns the mode's exit code.
func answer(stdout io.Writer, r response) int {
	if err := json.NewEncoder(stdout).Encode(r); err != nil {
		return 3
	}
	return 0
}

// sent is the exit code once the last frame was written, or not.
func sent(err error) int {
	if err != nil {
		return 3
	}
	return 0
}
