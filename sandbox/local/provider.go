// Package local implements [sandbox.SandboxProvider] using stdlib os/exec and
// temp directories. It is the zero-dependency fallback for local development
// and tests — no Cella required.
//
// Each sandbox maps to a per-call temp directory (os.MkdirTemp). Exec runs
// commands with exec.CommandContext against that directory. Destroy removes the
// directory. ReadFile/WriteFile/ListFiles operate directly on the filesystem.
// HealthCheck returns nil iff the directory still exists.
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
	"sync"
	"time"

	"latere.ai/x/agents/internal/sandbox"
)

// Provider implements [sandbox.SandboxProvider] using the local filesystem and
// os/exec. Suitable for tests and local-dev fallback; no external services
// required.
type Provider struct {
	mu   sync.Mutex
	dirs map[string]string // sandboxID → tempDir
}

// New returns a ready-to-use local Provider.
func New() *Provider {
	return &Provider{dirs: make(map[string]string)}
}

// Create provisions a new sandbox: a temp directory with a random ID.
func (p *Provider) Create(_ context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	prefix := "topos-sandbox-"
	if opts.Name != "" {
		prefix = "topos-sandbox-" + opts.Name + "-"
	}
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return sandbox.Sandbox{}, fmt.Errorf("local sandbox: create temp dir: %w", err)
	}

	// Generate a stable ID from the directory path (just use the base name).
	id := filepath.Base(dir)

	p.mu.Lock()
	p.dirs[id] = dir
	p.mu.Unlock()

	return sandbox.Sandbox{
		ID:        id,
		Name:      opts.Name,
		State:     sandbox.StateRunning,
		Tier:      "ephemeral",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// Destroy removes the sandbox's temp directory and deregisters its ID.
// Idempotent: if the sandbox does not exist, returns nil.
func (p *Provider) Destroy(_ context.Context, id string) error {
	p.mu.Lock()
	dir, ok := p.dirs[id]
	if ok {
		delete(p.dirs, id)
	}
	p.mu.Unlock()

	if !ok {
		return nil // idempotent
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
	dir, err := p.sandboxDir(id)
	if err != nil {
		return sandbox.ExecResult{}, err
	}

	if len(opts.Argv) == 0 {
		return sandbox.ExecResult{}, errors.New("local sandbox: exec: argv is empty")
	}

	cwd := dir
	if opts.Cwd != "" {
		cwd = opts.Cwd
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
		if ctx.Err() != nil {
			phase = "killed"
		} else if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
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
	dir, err := p.sandboxDir(id)
	if err != nil {
		return nil, err
	}

	if len(opts.Argv) == 0 {
		return nil, errors.New("local sandbox: stream exec: argv is empty")
	}

	cwd := dir
	if opts.Cwd != "" {
		cwd = opts.Cwd
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
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("local sandbox: stream exec: start: %w", err)
	}

	s := &localStream{
		r:   pr,
		pw:  pw,
		cmd: cmd,
	}

	go func() {
		waitErr := cmd.Wait()
		pw.Close() // signal EOF to the reader
		exitCode := 0
		phase := "exited"
		if waitErr != nil {
			var exitErr *exec.ExitError
			// Check ctx first: a cancelled/timed-out command is SIGKILLed by
			// CommandContext and surfaces as an *exec.ExitError, which would
			// otherwise mask the cancellation as a normal exit.
			if ctx.Err() != nil {
				phase = "killed"
			} else if errors.As(waitErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 127
			}
		}
		// Set only the terminal fields; do not replace the whole struct, which
		// would discard the Stdout that Recv accumulated (Result must return the
		// full combined output, matching the cella provider's semantics).
		s.mu.Lock()
		s.result.ExitCode = exitCode
		s.result.Phase = phase
		s.done = true
		s.mu.Unlock()
	}()

	return s, nil
}

// ReadFile reads a file from the sandbox's temp directory.
func (p *Provider) ReadFile(_ context.Context, id, path string) ([]byte, error) {
	dir, err := p.sandboxDir(id)
	if err != nil {
		return nil, err
	}
	full := filepath.Join(dir, path)
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
	dir, err := p.sandboxDir(id)
	if err != nil {
		return err
	}
	full := filepath.Join(dir, path)
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
	dir, err := p.sandboxDir(id)
	if err != nil {
		return nil, err
	}
	full := filepath.Join(dir, path)
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
	done   bool
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
