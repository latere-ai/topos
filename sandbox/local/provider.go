// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package local implements [sandbox.Provider] using stdlib os/exec and
// temp directories. It is the zero-dependency fallback for local development
// and tests — no Cella required.
//
// Each sandbox maps to a per-call temp directory (os.MkdirTemp). Exec runs
// commands with exec.CommandContext against that directory. Destroy removes the
// directory. ReadFile/WriteFile/ListFiles operate directly on the filesystem,
// confined to that directory: every path argument, and Exec/StreamExec's Cwd,
// is resolved against it and rejected with [ErrPathEscape] when it would land
// outside. HealthCheck returns nil iff the directory still exists.
//
// Concurrency: the id→dir map is protected by a sync.Mutex; all methods are
// safe for concurrent use.
package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"latere.ai/x/topos/sandbox"
)

// Provider implements [sandbox.Provider] using the local filesystem and
// os/exec. Suitable for tests and local-dev fallback; no external services
// required.
//
// In the default mode (New) each sandbox is a fresh temp directory that Destroy
// removes. In rooted mode (NewAt) every sandbox shares a single caller-owned
// directory — typically a git worktree — which Destroy never deletes, so an
// embedding host can run tools directly against an existing checkout.
type Provider struct {
	mu   sync.Mutex
	dirs map[string]string // sandboxID → dir
	root string            // when non-empty, every sandbox uses this dir (rooted mode)
	seq  int               // monotonic counter for unique rooted-mode ids
}

// ErrPathEscape is returned when a path argument resolves outside the
// sandbox's directory. It is the sentinel a caller errors.Is to tell a
// containment refusal from a filesystem failure. Every path a sandbox method
// accepts is interpreted relative to that directory, including one that looks
// absolute, so no argument can reach the host filesystem around it.
var ErrPathEscape = errors.New("local sandbox: path escapes sandbox directory")

// New returns a ready-to-use local Provider in temp-directory mode.
func New() *Provider {
	return &Provider{dirs: make(map[string]string)}
}

// NewAt returns a local Provider that roots every sandbox at the given existing
// directory instead of minting a per-sandbox temp dir. Create registers the root
// (it does not create or clear it); Destroy deregisters the id but never removes
// the caller-owned root. Use this to run tools directly in a host directory such
// as a git worktree. Multiple Create calls return distinct ids that all map to
// the shared root, so delegated peers operate on the same checkout.
func NewAt(root string) *Provider {
	return &Provider{dirs: make(map[string]string), root: root}
}

// Create provisions a new sandbox. In temp-directory mode it is a fresh temp dir
// with an id derived from its name; in rooted mode it is the shared root with a
// distinct sequence id (so concurrent peers do not collide in the id→dir map).
func (p *Provider) Create(_ context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	var dir, id string
	if p.root != "" {
		dir = p.root
		p.mu.Lock()
		p.seq++
		id = fmt.Sprintf("root-%d", p.seq)
		p.dirs[id] = dir
		p.mu.Unlock()
	} else {
		prefix := "topos-sandbox-"
		if opts.Name != "" {
			prefix = "topos-sandbox-" + opts.Name + "-"
		}
		var err error
		dir, err = os.MkdirTemp("", prefix)
		if err != nil {
			return sandbox.Sandbox{}, fmt.Errorf("local sandbox: create temp dir: %w", err)
		}
		// Generate a stable ID from the directory path (just use the base name).
		id = filepath.Base(dir)
		p.mu.Lock()
		p.dirs[id] = dir
		p.mu.Unlock()
	}

	return sandbox.Sandbox{
		ID:        id,
		Name:      opts.Name,
		State:     sandbox.StateRunning,
		Tier:      "ephemeral",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// Destroy deregisters the sandbox id and, in temp-directory mode, removes its
// directory. In rooted mode the caller-owned root is left intact. Idempotent: an
// unknown id returns nil.
func (p *Provider) Destroy(_ context.Context, id string) error {
	p.mu.Lock()
	dir, ok := p.dirs[id]
	if ok {
		delete(p.dirs, id)
	}
	rooted := p.root != ""
	p.mu.Unlock()

	if !ok {
		return nil // idempotent
	}
	if rooted {
		return nil // never remove a caller-owned root
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("local sandbox: destroy %q: %w", id, err)
	}
	return nil
}

// Exec runs a command to completion inside the sandbox's temp directory.
// Stdout and Stderr are captured and merged into ExecResult.Stdout to match
// the Cella backend's combined-stream contract.
func (p *Provider) Exec(ctx context.Context, id string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
	cwd, err := p.resolve(id, opts.Cwd)
	if err != nil {
		return sandbox.ExecResult{}, err
	}

	if len(opts.Argv) == 0 {
		return sandbox.ExecResult{}, errors.New("local sandbox: exec: argv is empty")
	}

	//nolint:gosec // argv is caller-controlled; local sandbox is trusted
	cmd := exec.CommandContext(ctx, opts.Argv[0], opts.Argv[1:]...)
	cmd.Dir = cwd

	// Merge env: inherit none from the parent (clean slate), then add opts.Env.
	// Using an empty base preserves sandbox isolation in tests.
	cmd.Env = buildEnv(opts.Env)

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	runErr := cmd.Run()

	phase := "exited"
	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		// Check ctx first: a cancelled/timed-out command is SIGKILLed by
		// CommandContext and surfaces as an *exec.ExitError, so the ExitError
		// branch would otherwise mask the cancellation as a normal exit.
		switch {
		case ctx.Err() != nil:
			phase = "killed"
		case errors.As(runErr, &exitErr):
			exitCode = exitErr.ExitCode()
		default:
			// command not found or similar
			exitCode = 127
		}
	}

	return sandbox.ExecResult{
		Stdout:   combined.Bytes(),
		ExitCode: exitCode,
		Phase:    phase,
	}, nil
}

// StreamExec starts the command and streams its output chunk-by-chunk.
// It uses a pipe so output is delivered incrementally.
func (p *Provider) StreamExec(ctx context.Context, id string, opts sandbox.ExecOptions) (sandbox.ExecStream, error) {
	cwd, err := p.resolve(id, opts.Cwd)
	if err != nil {
		return nil, err
	}

	if len(opts.Argv) == 0 {
		return nil, errors.New("local sandbox: stream exec: argv is empty")
	}

	//nolint:gosec // argv is caller-controlled; local sandbox is trusted
	cmd := exec.CommandContext(ctx, opts.Argv[0], opts.Argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = buildEnv(opts.Env)

	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("local sandbox: stream exec: pipe: %w", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, fmt.Errorf("local sandbox: stream exec: start: %w", err)
	}

	s := &localStream{
		r:   pr,
		pw:  pw,
		cmd: cmd,
	}

	go func() {
		waitErr := cmd.Wait()
		exitCode := 0
		phase := "exited"
		if waitErr != nil {
			var exitErr *exec.ExitError
			// Check ctx first: a cancelled/timed-out command is SIGKILLed by
			// CommandContext and surfaces as an *exec.ExitError, which would
			// otherwise mask the cancellation as a normal exit.
			switch {
			case ctx.Err() != nil:
				phase = "killed"
			case errors.As(waitErr, &exitErr):
				exitCode = exitErr.ExitCode()
			default:
				exitCode = 127
			}
		}
		// Set only the terminal fields; do not replace the whole struct, which
		// would discard the Stdout that Recv accumulated (Result must return the
		// full combined output, matching the cella provider's semantics).
		s.mu.Lock()
		s.result.ExitCode = exitCode
		s.result.Phase = phase
		s.mu.Unlock()
		// Close the write end only after the terminal fields are recorded, so a
		// caller that observes io.EOF from Recv is guaranteed to see the final
		// ExitCode/Phase via Result (the EOF happens-after this close).
		_ = pw.Close()
	}()

	return s, nil
}

// ReadFile reads a file from the sandbox's temp directory.
func (p *Provider) ReadFile(_ context.Context, id, path string) ([]byte, error) {
	full, err := p.resolve(id, path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, sandbox.ErrNotFound
		}
		return nil, fmt.Errorf("local sandbox: read file %q: %w", path, err)
	}
	return data, nil
}

// WriteFile writes a file into the sandbox's temp directory, creating parent
// directories as needed.
func (p *Provider) WriteFile(_ context.Context, id, path string, data []byte) error {
	full, err := p.resolve(id, path)
	if err != nil {
		return err
	}
	if mkdirErr := os.MkdirAll(filepath.Dir(full), 0o755); mkdirErr != nil {
		return fmt.Errorf("local sandbox: write file mkdir %q: %w", path, mkdirErr)
	}
	if writeErr := os.WriteFile(full, data, 0o644); writeErr != nil {
		return fmt.Errorf("local sandbox: write file %q: %w", path, writeErr)
	}
	return nil
}

// ListFiles lists the immediate children of a directory in the sandbox.
func (p *Provider) ListFiles(_ context.Context, id, path string) ([]sandbox.FileInfo, error) {
	full, err := p.resolve(id, path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, sandbox.ErrNotFound
		}
		return nil, fmt.Errorf("local sandbox: list files %q: %w", path, err)
	}
	out := make([]sandbox.FileInfo, 0, len(entries))
	for _, e := range entries {
		info, infoErr := e.Info()
		if infoErr != nil {
			return nil, fmt.Errorf("local sandbox: stat %q: %w", e.Name(), infoErr)
		}
		out = append(out, sandbox.FileInfo{
			Name:  e.Name(),
			Size:  info.Size(),
			Mode:  uint32(info.Mode() & fs.ModePerm),
			IsDir: e.IsDir(),
		})
	}
	return out, nil
}

// HealthCheck returns nil iff the sandbox directory still exists.
func (p *Provider) HealthCheck(_ context.Context, id string) error {
	p.mu.Lock()
	dir, ok := p.dirs[id]
	p.mu.Unlock()
	if !ok {
		return sandbox.ErrNotFound
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sandbox.ErrNotFound
		}
		return fmt.Errorf("local sandbox: health check %q: %w", id, err)
	}
	return nil
}

// sandboxDir looks up the temp directory for id, returning ErrNotFound if unknown.
func (p *Provider) sandboxDir(id string) (string, error) {
	p.mu.Lock()
	dir, ok := p.dirs[id]
	p.mu.Unlock()
	if !ok {
		return "", sandbox.ErrNotFound
	}
	return dir, nil
}

// resolve maps a caller-supplied path to an absolute path inside the sandbox's
// directory. The path is always joined onto that directory, so an absolute
// argument is re-rooted rather than honoured against the host, and the joined
// result is verified to stay at or below the directory. Escapes return
// [ErrPathEscape]; an empty path resolves to the directory itself. This is the
// containment check that makes the package doc's per-sandbox-directory
// invariant true for ReadFile, WriteFile, ListFiles, and Exec/StreamExec Cwd.
func (p *Provider) resolve(id, path string) (string, error) {
	dir, err := p.sandboxDir(id)
	if err != nil {
		return "", err
	}
	full := filepath.Join(dir, path)
	rel, err := filepath.Rel(dir, full)
	if err != nil || escapes(rel) {
		return "", fmt.Errorf("%w: %q", ErrPathEscape, path)
	}
	return full, nil
}

// escapes reports whether a cleaned relative path steps above its root.
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// buildEnv converts a map to a slice of "KEY=VALUE" entries suitable for
// exec.Cmd.Env. A nil map yields a non-nil but empty slice so that Cmd.Env
// is explicitly set (empty, not inherited).
func buildEnv(env map[string]string) []string {
	// Start with minimal PATH so commands like "sh" resolve correctly.
	base := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/tmp",
	}
	if len(env) == 0 {
		return base
	}
	out := make([]string, len(base), len(base)+len(env))
	copy(out, base)
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// localStream implements [sandbox.ExecStream] over a local exec.Cmd.
type localStream struct {
	r   *os.File
	pw  *os.File
	cmd *exec.Cmd

	mu     sync.Mutex
	result sandbox.ExecResult
}

// Recv reads the next chunk of output from the command. Returns io.EOF when
// the command exits.
func (s *localStream) Recv() ([]byte, error) {
	buf := make([]byte, 4096)
	n, err := s.r.Read(buf)
	if n > 0 {
		out := make([]byte, n)
		copy(out, buf[:n])
		// Update Stdout accumulation in result.
		s.mu.Lock()
		s.result.Stdout = append(s.result.Stdout, out...)
		s.mu.Unlock()
		return out, nil
	}
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	}
	return nil, err
}

// Result returns the final ExecResult. Valid only after Recv has returned io.EOF.
func (s *localStream) Result() sandbox.ExecResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// Close releases the pipe file descriptor.
func (s *localStream) Close() error {
	return s.r.Close()
}
