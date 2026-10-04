// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package machine is where a session's tools run (spec 009): the
// Machine interface, the credential deny-list every machine enforces,
// and the Go-native search both machines share. machine/host is the
// person's own computer; machine/cella is a Cella sandbox.
package machine

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"
)

// Kinds of machine.
const (
	KindHost  = "host"
	KindCella = "cella"
)

// Info describes a machine as session.machine records it.
type Info struct {
	Kind        string
	ID          string
	Workdir     string
	OS          string
	Arch        string
	Environment string
	// Sandbox names the host sandbox driver every command runs under;
	// empty on a machine that runs commands without one, and SandboxNone
	// on a host whose operating system offers no sandbox at all.
	Sandbox string
}

// SandboxNone is the Sandbox of a host with none of the mechanisms the
// host sandbox is built on, Seatbelt, Bubblewrap and Landlock, which is
// every Windows host. A session on it runs only in plan or confirm
// (spec 012).
const SandboxNone = "none"

// ExecRequest is one command. Command runs under /bin/sh -c.
type ExecRequest struct {
	Command string
	// Dir is the directory the command starts in; empty is the working
	// directory.
	Dir string
	// Env adds variables to the machine's environment.
	Env map[string]string
	// Stdin is the command's standard input; nil is empty.
	Stdin io.Reader
	// Timeout kills the command's process group when it passes; zero is
	// no timeout beyond the context's.
	Timeout time.Duration
	// Background starts the command detached, with its output in a job
	// log in the spill directory, and returns at once.
	Background bool
	// ReportDir asks for the shell's final directory, written to a
	// separate descriptor so it never mixes with the output.
	ReportDir bool
	// ServerGrace, when positive on a foreground command, moves the
	// command to the background once it has run this long and a process
	// of its group listens on a server port, as ServerPorts reads them,
	// checked again every ServerPoll (spec 045): the call returns at
	// once with Moved set, and the group runs on with its output drained
	// into a job log. Zero waits for the command's end. A machine that
	// cannot see its commands' sockets leaves the command to Timeout.
	ServerGrace time.Duration
}

// ExecResult is a finished command, or a started background job.
type ExecResult struct {
	// Output is standard output and standard error, in the order
	// written.
	Output   []byte
	ExitCode int
	TimedOut bool
	// Canceled is set when the context ended the command.
	Canceled bool
	// Dir is the shell's final directory when ReportDir asked for it and
	// the shell reached its exit.
	Dir string
	// PID and Log name a background job.
	PID int
	Log string
	// Moved is a foreground command ServerGrace moved to the background:
	// PID leads its process group, Log receives what it writes from the
	// move on, Ports are the server ports it listens on, and Output is
	// what it wrote before the move. The machine stops the job when the
	// session ends, as it stops a background job.
	Moved bool
	Ports []int
	// ServerErr is why the check of ServerGrace stopped early for this
	// command: its group's sockets could not be read, or the job log a
	// move needs could not be made. The command went on under its
	// timeout.
	ServerErr error
}

// ExecStream is a running command whose output arrives as it is
// written.
type ExecStream interface {
	io.Reader
	// Wait returns the result once the command ends. Output is empty in
	// it: the stream carried it.
	Wait() (ExecResult, error)
}

// FileInfo is what Stat and List return.
type FileInfo struct {
	Path    string
	Size    int64
	Mode    fs.FileMode
	ModTime time.Time
	IsDir   bool
}

// Machine is where a session's tools run. Every path is the machine's
// own: an absolute path is used as given, a relative one resolves against
// the working directory.
type Machine interface {
	Info() Info
	Roots() []string
	SpillDir() string
	Exec(ctx context.Context, r ExecRequest) (ExecResult, error)
	ExecStream(ctx context.Context, r ExecRequest) (ExecStream, error)
	ReadFile(ctx context.Context, path string) (io.ReadCloser, error)
	WriteFile(ctx context.Context, path string, r io.Reader, mode fs.FileMode) error
	Stat(ctx context.Context, path string) (FileInfo, error)
	List(ctx context.Context, path string) ([]FileInfo, error)
	Remove(ctx context.Context, path string) error
	Rename(ctx context.Context, from, to string) error
	Search(ctx context.Context, q SearchRequest) (SearchResult, error)
	// Release lets go of the machine while the session is idle, and
	// removes it when end is true.
	Release(ctx context.Context, end bool) error
}

// Worktrees is the optional interface of a machine that gives an
// isolated thread a git worktree of its own (spec 013). Worktree returns
// the machine of the worktree named name on branch, creating it from the
// HEAD commit of the machine's working directory the first time, and
// reopening it after.
type Worktrees interface {
	Worktree(ctx context.Context, name, branch string) (Machine, error)
}

// Errors every machine returns.
var (
	// ErrOutside is a path in none of the machine's roots.
	ErrOutside = errors.New("machine: the path is outside the working directory")
	// ErrDenied is a path or a variable the credential deny-list names.
	ErrDenied = errors.New("machine: the path holds credentials and is not readable by tools")
	// ErrReleased is a call after Release.
	ErrReleased = errors.New("machine: the machine was released")
)

// The error codes of spec 009.
const (
	CodeUnavailable = "machine_unavailable"
	CodeLost        = "machine_lost"
)
