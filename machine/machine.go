// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package machine is where a session's tools run (spec 009): the
// Machine interface, the credential deny-list every machine enforces,
// and the Go-native search both machines share. machine/host is the
// computer the runner runs on; machine/cella is a Cella sandbox, and a
// person's own computer reached through the Cella environment it serves
// (spec 080).
package machine

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"
)

// Kinds of machine. KindDevice is a person's own computer reached
// through the Cella environment it serves (spec 080): it scores as a host
// and never as a sandbox, since its commands change the person's real
// files.
const (
	KindHost   = "host"
	KindCella  = "cella"
	KindDevice = "device"
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
	// Egress is the egress mode the machine reports it runs, open,
	// allowlist or none, which its admission may have narrowed from the
	// one asked (spec 052); empty on a machine that reports none.
	Egress string
}

// Network is the egress a session's network gives its machine (spec
// 052): the mode, open, allowlist or none, and under allowlist the hosts
// the machine may reach beside the ones it joins itself, such as the
// hosts its secrets are for.
type Network struct {
	Mode  string
	Hosts []string
}

// Connection is one connection the machine's egress gateway refused, and
// when it decided.
type Connection struct {
	Host string
	Port int
	At   time.Time
}

// Networked is the optional interface of a machine whose egress a
// session's network governs (spec 052). ApplyNetwork narrows or widens
// the machine to the network, and does nothing when it runs that network
// already; a refusal leaves the machine's egress as it was. Refused are
// the distinct hosts the gateway refused since a time, one connection
// each, oldest first.
type Networked interface {
	ApplyNetwork(ctx context.Context, n Network) error
	Refused(ctx context.Context, since time.Time) ([]Connection, error)
}

// HTTPConnection is a session's connection of kind http as its machine
// reaches it (spec 069): Env names Address, the connection's forwarding
// address, in each command's environment, and git rewrites GitURL, the
// git host the person's repositories live at, to GitAddress, the
// connection's git address; both are empty for a connection with no git.
type HTTPConnection struct {
	Env        string
	Address    string
	GitAddress string
	GitURL     string
}

// Connected is the optional interface of a machine that reaches a
// session's connections of kind http through the installation's
// connection service (spec 069). ApplyConnections gives the machine the
// session's list as it stands, which replaces the one it had: a
// connection the list no longer names leaves nothing behind. It does
// nothing when the machine runs that list already.
//
// ReachesConnections reports whether the machine reaches the connection
// service at all. A sandbox that opened without the service's Secret,
// because its host was another Secret's, reaches none of the session's
// http connections for as long as it lives: it takes no list, and the
// model is not told of them.
type Connected interface {
	ApplyConnections(ctx context.Context, cs []HTTPConnection) error
	ReachesConnections() bool
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
	// MoveNow, once it is closed, moves a foreground command still
	// running to the background as ServerGrace moves a server, whatever
	// it listens on (spec 098): the call returns at once with Moved set
	// and no Ports, and the group runs on with its output drained into a
	// job log. A nil channel never moves the command. A machine that
	// cannot move a command ignores it.
	MoveNow <-chan struct{}
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
	// Moved is a foreground command ServerGrace or MoveNow moved to the
	// background: PID leads its process group, Log receives what it
	// writes from the move on, Ports are the server ports it listens on,
	// none for a move by MoveNow, and Output is what it wrote before the
	// move. The machine stops the job when the session ends, as it stops
	// a background job.
	Moved bool
	Ports []int
	// ServerErr is why the check of ServerGrace stopped early for this
	// command, or why MoveNow did not move it: its group's sockets could
	// not be read, or the job log a move needs could not be made. The
	// command went on under its timeout.
	ServerErr error
	// Stopped is set when the machine stopped under the command, which
	// then has no exit of its own: Output is what it wrote before, and
	// ExitCode the code its session ended with, ExitKilled for a kill and
	// -1 when none is known.
	Stopped *Stop
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
	// ErrLost is a machine that is gone, or that failed and cannot be
	// started again: every call answers it until the machine is opened
	// anew, and what a call could not do for it is the loss's, not a
	// failure of its own (spec 009).
	ErrLost = errors.New("machine: the session's machine is gone")
)

// The error codes of spec 009.
const (
	CodeUnavailable = "machine_unavailable"
	CodeLost        = "machine_lost"
)
