// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package sandbox defines the Provider interface — the single
// abstraction boundary between all upstream Topos code (control plane,
// harness, tools) and any sandbox execution backend.
//
// No upstream code may import the cella package or any other backend
// package directly; all upstream dependencies are on this interface and
// the shared types in this file only.
package sandbox

import (
	"context"
	"errors"
	"fmt"
)

// ErrNotFound is returned when the referenced sandbox or resource does
// not exist. Backends MUST wrap or be this error on HTTP 404.
var ErrNotFound = errors.New("sandbox: not found")

// ErrConflict is returned when a creation or rename conflicts with an
// existing resource. Backends MUST wrap or be this error on HTTP 409.
var ErrConflict = errors.New("sandbox: conflict")

// APIError carries a structured error response from the backend API.
// It is used for all error statuses not covered by ErrNotFound and
// ErrConflict (e.g. 5xx, 400, 429).
type APIError struct {
	// Status is the HTTP status code returned by the backend.
	Status int
	// Code is the machine-readable error code from the backend (stable
	// across backend versions; callers may branch on it).
	Code string
	// Message is the human-readable error detail.
	Message string
	// RequestID is the opaque correlation id to include in bug reports.
	RequestID string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("sandbox: API error %d %s: %s (request_id=%s)",
		e.Status, e.Code, e.Message, e.RequestID)
}

// State mirrors the Cella sandbox state enum but is defined here
// so interface consumers don't need to know about Cella.
type State string

// Sandbox lifecycle states reported by the backend.
const (
	StateCreating State = "creating"
	StateRunning  State = "running"
	StateStopped  State = "stopped"
	StateDeleting State = "deleting"
	StateError    State = "error"
)

// Sandbox is a lightweight handle to a sandbox instance. It carries the
// fields needed by upstream callers; backends may populate additional
// information via their own types but MUST convert to this shape at the
// interface boundary.
type Sandbox struct {
	// ID is the stable backend-assigned UUIDv7.
	ID string
	// Name is the human-friendly slug (may be empty if the backend did
	// not assign one yet, or if the caller did not request one).
	Name string
	// State is the last-known lifecycle state of the sandbox.
	State State
	// Tier is "ephemeral" or "persistent".
	Tier string
	// CreatedAt is the RFC3339 creation timestamp from the backend.
	CreatedAt string
}

// CreateOptions controls sandbox creation parameters. All fields are
// optional; zero values let the backend choose defaults.
type CreateOptions struct {
	// Name is the desired human-friendly slug. If empty the backend
	// generates one.
	Name string
	// Image is the container image ref. If empty the backend uses its
	// platform base image.
	Image string
	// Env is the set of environment variables to inject at sandbox start.
	Env map[string]string
	// Labels are arbitrary key/value pairs attached to the sandbox for
	// filtering and audit; the Cella backend always sets "kind=agent".
	Labels map[string]string
	// Tier selects "ephemeral" (default) or "persistent".
	Tier string
	// Policy names the sandbox policy to request (e.g. "brain"). If empty
	// the backend resolves the caller's default policy. The brain runner
	// sets "brain" so the loop pod is compute-only and locked down,
	// overriding the user's default (which is open for personal accounts).
	Policy string
}

// ExecOptions controls command execution parameters.
type ExecOptions struct {
	// Argv is the command and its arguments. Required.
	Argv []string
	// Env overrides or extends the sandbox's environment for this
	// command only.
	Env map[string]string
	// Cwd is the working directory for the command. Defaults to the
	// sandbox's workdir when empty.
	Cwd string
}

// ExecResult holds the result of a completed command execution.
//
// Note: the Cella backend merges stdout and stderr into a single
// combined stream in arrival order. As a result, for CellaSandboxProvider:
//   - Stdout carries the COMBINED stdout+stderr output.
//   - Stderr is nil/empty; Cella provides no per-stream separation.
//
// The interface retains separate Stdout/Stderr fields so that backends
// with native per-stream separation can use both without an interface
// break.
type ExecResult struct {
	// Stdout holds the command's standard output. For the Cella backend
	// this is the combined stdout+stderr in arrival order.
	Stdout []byte
	// Stderr holds the command's standard error. Unused by the Cella
	// backend (see comment above); reserved for backends that separate
	// streams.
	Stderr []byte
	// ExitCode is the process exit status. Meaningful only when Phase
	// is "exited".
	ExitCode int
	// Phase is the terminal phase of the command:
	// "exited", "killed", or "lost".
	Phase string
}

// FileInfo describes a single filesystem entry returned by ListFiles.
type FileInfo struct {
	// Name is the base name of the entry (not a full path).
	Name string
	// Size is the file size in bytes. Zero for directories.
	Size int64
	// Mode is the Unix permission bits as a decimal integer.
	Mode uint32
	// IsDir is true when the entry is a directory.
	IsDir bool
}

// ExecStream is a handle to a streaming command execution. Recv delivers
// incremental output chunks until io.EOF signals the command has
// terminated. Result returns the final ExecResult; it is valid only
// after Recv has returned io.EOF.
type ExecStream interface {
	// Recv returns the next chunk of output. Returns (nil, io.EOF) when
	// the command terminates. Other errors indicate a transport failure.
	Recv() ([]byte, error)
	// Result returns the terminal ExecResult. Callers MUST drain Recv
	// until io.EOF before calling Result; calling Result before EOF
	// returns a zero value.
	Result() ExecResult
	// Close releases resources associated with the stream. Safe to call
	// multiple times.
	Close() error
}

// Provider is the interface all Topos code uses to create and
// operate sandbox environments. Implementations MUST be safe for
// concurrent use.
//
// Error contract:
//   - ErrNotFound for missing sandboxes/commands (HTTP 404 from Cella).
//   - ErrConflict for name/id collisions (HTTP 409 from Cella).
//   - *APIError for all other backend errors (including 5xx).
//   - Standard library errors for local failures (ctx cancelled, etc.).
type Provider interface {
	// Create provisions a new sandbox and returns a handle to it. The
	// returned Sandbox may still be in the "creating" state; callers
	// that need "running" state should poll HealthCheck.
	Create(ctx context.Context, opts CreateOptions) (Sandbox, error)

	// Destroy deletes the sandbox and all associated workspace data.
	// Destroy is idempotent: ErrNotFound from the backend is treated
	// as success.
	Destroy(ctx context.Context, id string) error

	// Exec runs a command to completion inside the sandbox and returns
	// the result. The context governs the total execution time; callers
	// should set an appropriate deadline.
	Exec(ctx context.Context, id string, opts ExecOptions) (ExecResult, error)

	// StreamExec starts a command and returns a stream handle for
	// consuming output incrementally. The caller must call Close on the
	// returned stream when done.
	StreamExec(ctx context.Context, id string, opts ExecOptions) (ExecStream, error)

	// ReadFile reads the contents of a single file from the sandbox
	// filesystem. Returns ErrNotFound if the file does not exist.
	ReadFile(ctx context.Context, id, path string) ([]byte, error)

	// WriteFile writes data to a file in the sandbox filesystem,
	// creating any missing parent directories. Bounded sizes only;
	// large-file bulk transfer is handled by the lift/drop lifecycle
	// (out of scope for this interface method).
	WriteFile(ctx context.Context, id, path string, data []byte) error

	// ListFiles lists the immediate children of a directory path inside
	// the sandbox. Returns ErrNotFound if the directory does not exist.
	ListFiles(ctx context.Context, id, path string) ([]FileInfo, error)

	// HealthCheck returns nil if the sandbox is running and reachable,
	// ErrNotFound if it does not exist, or another error for degraded
	// states.
	HealthCheck(ctx context.Context, id string) error
}
