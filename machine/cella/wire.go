// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"bytes"
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

	"latere.ai/x/cella/client"

	"latere.ai/x/topos/machine"
)

// shell runs every command inside the sandbox.
const shell = "/bin/sh"

// The helper's frames, the same table as cmd/topos-machine's: a kind
// byte, the payload's length as four big-endian bytes, and the payload.
const (
	frameInput  = 'i'
	frameEOF    = 'e'
	frameKill   = 'k'
	frameStart  = 's'
	frameFail   = 'f'
	frameOutput = 'o'
	frameDir    = 'd'
	frameExit   = 'x'
)

// maxFrame bounds one frame's payload.
const maxFrame = 1 << 20

// chunk is how much one input frame, or one socket write, carries: the
// exec socket reads messages of at most a mebibyte.
const chunk = 32 << 10

// errNoHelper is an exec session that ended before the helper said
// anything: the helper is not in the sandbox, which a start of the
// sandbox by another caller can leave behind.
var errNoHelper = errors.New("machine: the helper did not answer")

// errorBody is a failure the helper reports.
type errorBody struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// err turns a helper's failure into an error that answers errors.Is as
// the host machine's error for the same failure does, with the helper's
// sentence; op and p name the operation for a failure the helper could
// not name.
func (e *errorBody) err(op, p string) error {
	var is error
	switch e.Kind {
	case "not_exist":
		is = fs.ErrNotExist
	case "exist":
		is = fs.ErrExist
	case "not_empty":
		is = syscall.ENOTEMPTY
	case "not_dir":
		is = syscall.ENOTDIR
	case "is_dir":
		return fmt.Errorf("machine: %s is a directory", p)
	case "permission":
		is = fs.ErrPermission
	case "outside":
		return fmt.Errorf("%w: %s", machine.ErrOutside, p)
	case "denied":
		return fmt.Errorf("%w: %s", machine.ErrDenied, p)
	}
	msg := e.Message
	if msg == "" {
		msg = op + " " + p + ": " + e.Kind
	}
	if !strings.HasPrefix(msg, "machine: ") {
		msg = "machine: " + msg
	}
	return helperError{msg: msg, is: is}
}

// helperError is a failure the helper reported: its sentence, and the
// sentinel it answers errors.Is for.
type helperError struct {
	msg string
	is  error
}

func (e helperError) Error() string { return e.msg }

func (e helperError) Unwrap() error { return e.is }

// exitBody is how a command ended, or that it moved to the background
// (spec 045): its group's leader, its job log, the server ports it
// listens on, and why the check for a server stopped early, if it did.
type exitBody struct {
	Code      int    `json:"code"`
	TimedOut  bool   `json:"timedOut,omitempty"`
	Canceled  bool   `json:"canceled,omitempty"`
	Moved     bool   `json:"moved,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Log       string `json:"log,omitempty"`
	Ports     []int  `json:"ports,omitempty"`
	ServerErr string `json:"serverError,omitempty"`
}

// entry is one file as the helper answers it.
type entry struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Mode    uint32    `json:"mode"`
	ModTime time.Time `json:"modTime"`
	IsDir   bool      `json:"isDir"`
}

func (e entry) info() machine.FileInfo {
	return machine.FileInfo{Path: e.Path, Size: e.Size, Mode: fs.FileMode(e.Mode), ModTime: e.ModTime, IsDir: e.IsDir}
}

// response is the helper's one JSON answer.
type response struct {
	Result  *machine.SearchResult `json:"result,omitempty"`
	Entries []entry               `json:"entries,omitempty"`
	Job     *struct {
		PID int    `json:"pid"`
		Log string `json:"log"`
	} `json:"job,omitempty"`
	Done  bool       `json:"done,omitempty"`
	Error *errorBody `json:"error,omitempty"`
}

// decodeResponse reads the helper's answer from its output.
func decodeResponse(out []byte) (response, error) {
	var r response
	if err := json.Unmarshal(bytes.TrimSpace(out), &r); err != nil {
		return r, fmt.Errorf("machine: the helper's answer %q: %w", truncate(out), err)
	}
	return r, nil
}

// truncate keeps an unreadable answer short enough for an error.
func truncate(b []byte) string {
	if len(b) > 512 {
		return string(b[:512]) + "..."
	}
	return string(b)
}

// readFrame reads one frame.
func readFrame(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("machine: the helper sent a frame of %d bytes, past the bound of %d", n, maxFrame)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return h[0], payload, nil
}

// sender writes frames to a session, one socket message each, from the
// input pump and a kill alike.
type sender struct {
	mu sync.Mutex
	s  *client.Session
}

func (w *sender) frame(kind byte, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	b := make([]byte, 5, 5+len(payload))
	b[0] = kind
	binary.BigEndian.PutUint32(b[1:], uint32(len(payload)))
	_, err := w.s.Write(append(b, payload...))
	return err
}

// input sends r as input frames and then the end frame, and returns r's
// read error, which ends the input as its end does.
func (w *sender) input(r io.Reader) error {
	var rerr error
	if r != nil {
		rerr = w.pipe(r)
	}
	// A failed end frame is the session failing, which its reader reports.
	_ = w.frame(frameEOF, nil)
	return rerr
}

// pipe sends r's bytes as input frames until r ends. A frame that cannot
// be written ends the copy without an error: the session failing is its
// reader's to report, and a command that exits before it reads its input
// is not a failure, as exec.Cmd drops EPIPE on a command's input.
func (w *sender) pipe(r io.Reader) error {
	buf := make([]byte, chunk)
	for {
		n, err := r.Read(buf)
		if n > 0 && !w.sent(buf[:n]) {
			return nil
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("machine: read the command's input: %w", err)
		}
	}
}

// sent reports whether an input frame reached the session.
func (w *sender) sent(p []byte) bool { return w.frame(frameInput, p) == nil }

// readAll reads a session's output to its end, keeping at most limit
// bytes and discarding the rest.
func readAll(s io.Reader, limit int64) ([]byte, error) {
	var out bytes.Buffer
	_, err := io.Copy(&out, io.LimitReader(s, limit))
	if _, derr := io.Copy(io.Discard, s); err == nil {
		err = derr
	}
	return out.Bytes(), err
}

// closeSession ends a session. Its output is drained while it closes, so
// a socket reader blocked on output nobody reads lets the close finish.
func closeSession(s *client.Session) error {
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		// The drain only unblocks the close; what it reads is past what
		// the caller wanted.
		_, _ = io.Copy(io.Discard, s)
	}()
	err := s.Close()
	<-drained
	return err
}

// jsonDecode decodes a helper's JSON payload, naming it when it is not.
func jsonDecode(payload []byte, v any) error {
	if err := json.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("machine: the helper's answer %q: %w", truncate(payload), err)
	}
	return nil
}
