// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package local_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
)

// assertInterface ensures Provider implements Provider at compile time.
var _ sandbox.Provider = (*local.Provider)(nil)

// TestLocalIgnoresVaultCredentials asserts the local provider treats the
// vault-credential fields as a no-op (it has no vault): SecretMounts on Create
// and SecretEnv on Exec neither error nor change behaviour.
func TestLocalIgnoresVaultCredentials(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, err := p.Create(ctx, sandbox.CreateOptions{SecretMounts: []string{"OPENAI_KEY"}})
	if err != nil {
		t.Fatalf("Create with SecretMounts: %v", err)
	}
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	res, err := p.Exec(ctx, sb.ID, sandbox.ExecOptions{
		Argv:      []string{"sh", "-c", "echo ok"},
		SecretEnv: map[string]string{"OPENAI_API_KEY": "openai_key"},
	})
	if err != nil {
		t.Fatalf("Exec with SecretEnv: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "ok") {
		t.Fatalf("exec result = %+v, want clean run ignoring SecretEnv", res)
	}
}

func TestCreateAndDestroy(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, err := p.Create(ctx, sandbox.CreateOptions{Name: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.ID == "" {
		t.Fatal("Create: empty ID")
	}
	if sb.State != sandbox.StateRunning {
		t.Fatalf("Create: state = %q, want running", sb.State)
	}

	// HealthCheck should pass while the sandbox is alive.
	if err := p.HealthCheck(ctx, sb.ID); err != nil {
		t.Fatalf("HealthCheck before destroy: %v", err)
	}

	if err := p.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	// HealthCheck should return ErrNotFound after destroy.
	if err := p.HealthCheck(ctx, sb.ID); !isNotFound(err) {
		t.Fatalf("HealthCheck after destroy: got %v, want ErrNotFound", err)
	}
}

func TestDestroyIdempotent(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	if err := p.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("Destroy 1: %v", err)
	}
	// Second destroy must be a no-op.
	if err := p.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("Destroy 2 (idempotent): %v", err)
	}
}

// TestNewAt_RootsExecAndFiles verifies rooted mode: Create does not mint a temp
// dir, Exec and the file methods operate against the caller-owned root, and
// Destroy leaves that root intact.
func TestNewAt_RootsExecAndFiles(t *testing.T) {
	root := t.TempDir()
	p := local.NewAt(root)
	ctx := context.Background()

	sb, err := p.Create(ctx, sandbox.CreateOptions{Name: "wt"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A relative-path write inside the command lands in the root.
	if _, err := p.Exec(ctx, sb.ID, sandbox.ExecOptions{Argv: []string{"sh", "-c", "echo hi > marker.txt"}}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got, err := p.ReadFile(ctx, sb.ID, "marker.txt")
	if err != nil || !strings.Contains(string(got), "hi") {
		t.Fatalf("ReadFile(marker.txt) = %q, %v; want it written under root", got, err)
	}

	// Destroy must NOT remove the caller-owned root.
	if err := p.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "marker.txt")); err != nil {
		t.Errorf("root was removed by Destroy (caller-owned): %v", err)
	}
}

// TestNewAt_DistinctIdsShareRoot verifies concurrent peers get distinct ids that
// all map to the shared root, and destroying one does not break another.
func TestNewAt_DistinctIdsShareRoot(t *testing.T) {
	p := local.NewAt(t.TempDir())
	ctx := context.Background()

	a, _ := p.Create(ctx, sandbox.CreateOptions{})
	b, _ := p.Create(ctx, sandbox.CreateOptions{})
	if a.ID == b.ID {
		t.Fatalf("ids must be distinct, both = %q", a.ID)
	}
	if err := p.Destroy(ctx, a.ID); err != nil {
		t.Fatalf("Destroy a: %v", err)
	}
	// b is still healthy (its own registration, shared root intact).
	if err := p.HealthCheck(ctx, b.ID); err != nil {
		t.Errorf("HealthCheck b after destroying a: %v", err)
	}
}

func TestExecSuccess(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	res, err := p.Exec(ctx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "echo hello"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", res.ExitCode)
	}
	if res.Phase != "exited" {
		t.Fatalf("phase = %q, want exited", res.Phase)
	}
	if !strings.Contains(string(res.Stdout), "hello") {
		t.Fatalf("stdout = %q, want 'hello'", res.Stdout)
	}
}

func TestExecNonzeroExit(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	res, err := p.Exec(ctx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "exit 42"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 42 {
		t.Fatalf("exit code = %d, want 42", res.ExitCode)
	}
}

// TestExecCancelledReportsKilled verifies that a command terminated by context
// cancellation is reported with Phase=="killed", not a normal "exited". Plain
// exec.CommandContext SIGKILLs the child, which surfaces as an *exec.ExitError,
// so ctx.Err() must be consulted before the ExitError classification.
func TestExecCancelledReportsKilled(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	runCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	res, err := p.Exec(runCtx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"sleep", "5"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Phase != "killed" {
		t.Fatalf("phase = %q, want killed (cancelled command)", res.Phase)
	}
}

func TestExecNotFound(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	_, err := p.Exec(ctx, "no-such-sandbox", sandbox.ExecOptions{Argv: []string{"echo"}})
	if !isNotFound(err) {
		t.Fatalf("Exec unknown sandbox: got %v, want ErrNotFound", err)
	}
}

func TestReadWriteFile(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	content := []byte("hello file")
	if err := p.WriteFile(ctx, sb.ID, "subdir/test.txt", content); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := p.ReadFile(ctx, sb.ID, "subdir/test.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("ReadFile = %q, want %q", got, content)
	}
}

func TestReadFileNotFound(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	_, err := p.ReadFile(ctx, sb.ID, "does-not-exist.txt")
	if !isNotFound(err) {
		t.Fatalf("ReadFile missing file: got %v, want ErrNotFound", err)
	}
}

func TestListFiles(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	if err := p.WriteFile(ctx, sb.ID, "a.txt", []byte("a")); err != nil {
		t.Fatalf("WriteFile a: %v", err)
	}
	if err := p.WriteFile(ctx, sb.ID, "b.txt", []byte("b")); err != nil {
		t.Fatalf("WriteFile b: %v", err)
	}

	files, err := p.ListFiles(ctx, sb.ID, ".")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) < 2 {
		t.Fatalf("ListFiles: got %d entries, want >= 2", len(files))
	}
}

func TestStreamExec(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	stream, err := p.StreamExec(ctx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "echo streaming"},
	})
	if err != nil {
		t.Fatalf("StreamExec: %v", err)
	}
	defer stream.Close() //nolint:errcheck

	var combined []byte
	for {
		chunk, recvErr := stream.Recv()
		if chunk != nil {
			combined = append(combined, chunk...)
		}
		if recvErr != nil {
			break
		}
	}

	if !strings.Contains(string(combined), "streaming") {
		t.Fatalf("stream output = %q, want 'streaming'", combined)
	}
	res := stream.Result()
	if res.ExitCode != 0 {
		t.Fatalf("stream exit code = %d, want 0", res.ExitCode)
	}
	// Result().Stdout must carry the full combined output that Recv accumulated;
	// the Wait goroutine must not clobber it when recording the terminal fields.
	if !strings.Contains(string(res.Stdout), "streaming") {
		t.Fatalf("Result().Stdout = %q, want it to contain the streamed output", res.Stdout)
	}
}

func TestConcurrentCreate(t *testing.T) {
	p := local.New()
	ctx := context.Background()
	const n = 10

	ch := make(chan string, n)
	for range n {
		go func() {
			sb, err := p.Create(ctx, sandbox.CreateOptions{})
			if err != nil {
				t.Errorf("Create: %v", err)
				ch <- ""
				return
			}
			ch <- sb.ID
		}()
	}

	ids := make(map[string]struct{}, n)
	for range n {
		id := <-ch
		if id == "" {
			continue
		}
		if _, dup := ids[id]; dup {
			t.Errorf("duplicate sandbox ID: %q", id)
		}
		ids[id] = struct{}{}
	}
	for id := range ids {
		p.Destroy(ctx, id) //nolint:errcheck
	}
}

// TestExecEmptyArgv verifies Exec rejects an empty argv with a clear error
// rather than panicking or invoking an empty command.
func TestExecEmptyArgv(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	_, err := p.Exec(ctx, sb.ID, sandbox.ExecOptions{Argv: nil})
	if err == nil {
		t.Fatal("Exec empty argv: got nil error, want non-nil")
	}
	if !strings.Contains(err.Error(), "argv is empty") {
		t.Fatalf("Exec empty argv: err = %v, want 'argv is empty'", err)
	}
}

// TestExecCwdOverride verifies that ExecOptions.Cwd, interpreted relative to
// the sandbox directory, overrides the command's default working directory.
func TestExecCwdOverride(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	p := local.NewAt(root)

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	res, err := p.Exec(ctx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "pwd"},
		Cwd:  "sub",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	// macOS /var/folders dirs are symlinked under /private; compare base names
	// to avoid symlink-resolution mismatches.
	if got := strings.TrimSpace(string(res.Stdout)); !strings.HasSuffix(got, "sub") {
		t.Fatalf("pwd = %q, want it to end with %q", got, "sub")
	}
}

// TestExecCommandNotFound verifies that a non-existent binary (a failure that
// is not an *exec.ExitError and not a context cancellation) is reported as
// exit code 127, matching shell convention.
func TestExecCommandNotFound(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	res, err := p.Exec(ctx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"this-binary-does-not-exist-xyz"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 127 {
		t.Fatalf("exit code = %d, want 127 (command not found)", res.ExitCode)
	}
	if res.Phase != "exited" {
		t.Fatalf("phase = %q, want exited", res.Phase)
	}
}

// TestExecEnvInjected verifies that ExecOptions.Env entries are passed through
// to the command's environment (exercising buildEnv's non-empty branch).
func TestExecEnvInjected(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	res, err := p.Exec(ctx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "echo $TOPOS_TEST_VAR"},
		Env:  map[string]string{"TOPOS_TEST_VAR": "injected-value"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "injected-value") {
		t.Fatalf("stdout = %q, want 'injected-value'", res.Stdout)
	}
}

// TestStreamExecEmptyArgv verifies StreamExec rejects an empty argv.
func TestStreamExecEmptyArgv(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	_, err := p.StreamExec(ctx, sb.ID, sandbox.ExecOptions{Argv: nil})
	if err == nil || !strings.Contains(err.Error(), "argv is empty") {
		t.Fatalf("StreamExec empty argv: err = %v, want 'argv is empty'", err)
	}
}

// TestStreamExecNotFound verifies StreamExec on an unknown sandbox returns
// ErrNotFound before doing any work.
func TestStreamExecNotFound(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	_, err := p.StreamExec(ctx, "no-such-sandbox", sandbox.ExecOptions{Argv: []string{"echo"}})
	if !isNotFound(err) {
		t.Fatalf("StreamExec unknown sandbox: got %v, want ErrNotFound", err)
	}
}

// TestStreamExecCwdOverride verifies a sandbox-relative ExecOptions.Cwd is
// honored by StreamExec.
func TestStreamExecCwdOverride(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	p := local.NewAt(root)

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	stream, err := p.StreamExec(ctx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "pwd"},
		Cwd:  "sub",
	})
	if err != nil {
		t.Fatalf("StreamExec: %v", err)
	}
	defer stream.Close() //nolint:errcheck

	out := drain(t, stream)
	if got := strings.TrimSpace(out); !strings.HasSuffix(got, "sub") {
		t.Fatalf("pwd = %q, want it to end with %q", got, "sub")
	}
}

// TestStreamExecStartError verifies that a non-existent binary fails at
// cmd.Start and surfaces as a start error from StreamExec.
func TestStreamExecStartError(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	_, err := p.StreamExec(ctx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"this-binary-does-not-exist-xyz"},
	})
	if err == nil || !strings.Contains(err.Error(), "start") {
		t.Fatalf("StreamExec bad binary: err = %v, want a start error", err)
	}
}

// TestStreamExecNonzeroExit verifies the Wait goroutine records a non-zero
// exit code (an *exec.ExitError that is not a cancellation).
func TestStreamExecNonzeroExit(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	stream, err := p.StreamExec(ctx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "exit 7"},
	})
	if err != nil {
		t.Fatalf("StreamExec: %v", err)
	}
	defer stream.Close() //nolint:errcheck

	drain(t, stream)
	res := stream.Result()
	if res.ExitCode != 7 {
		t.Fatalf("stream exit code = %d, want 7", res.ExitCode)
	}
	if res.Phase != "exited" {
		t.Fatalf("phase = %q, want exited", res.Phase)
	}
}

// TestStreamExecCancelledReportsKilled verifies the Wait goroutine reports
// Phase=="killed" when the command is terminated by context cancellation,
// rather than misclassifying the SIGKILL as a normal exit.
func TestStreamExecCancelledReportsKilled(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	runCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	stream, err := p.StreamExec(runCtx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"sleep", "5"},
	})
	if err != nil {
		t.Fatalf("StreamExec: %v", err)
	}
	defer stream.Close() //nolint:errcheck

	drain(t, stream)
	if res := stream.Result(); res.Phase != "killed" {
		t.Fatalf("phase = %q, want killed (cancelled command)", res.Phase)
	}
}

// TestRecvAfterClose verifies Recv surfaces a non-EOF transport error when the
// underlying pipe has been closed out from under it.
func TestRecvAfterClose(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	stream, err := p.StreamExec(ctx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "sleep 1; echo done"},
	})
	if err != nil {
		t.Fatalf("StreamExec: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Reading from a closed pipe yields a non-EOF error, not io.EOF.
	_, recvErr := stream.Recv()
	if recvErr == nil || errors.Is(recvErr, io.EOF) {
		t.Fatalf("Recv after close: err = %v, want a non-EOF error", recvErr)
	}
}

// TestReadFileNotFoundSandbox verifies ReadFile on an unknown sandbox returns
// ErrNotFound.
func TestReadFileNotFoundSandbox(t *testing.T) {
	p := local.New()
	_, err := p.ReadFile(context.Background(), "no-such-sandbox", "f.txt")
	if !isNotFound(err) {
		t.Fatalf("ReadFile unknown sandbox: got %v, want ErrNotFound", err)
	}
}

// TestReadFileIsDirectory verifies ReadFile on a path that is a directory
// returns a wrapped error distinct from ErrNotFound (the non-ErrNotExist
// read-error branch).
func TestReadFileIsDirectory(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	if err := p.WriteFile(ctx, sb.ID, "adir/keep.txt", []byte("x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := p.ReadFile(ctx, sb.ID, "adir")
	if err == nil {
		t.Fatal("ReadFile on directory: got nil error, want read error")
	}
	if isNotFound(err) {
		t.Fatalf("ReadFile on directory: got ErrNotFound, want a non-NotFound read error: %v", err)
	}
}

// TestWriteFileNotFoundSandbox verifies WriteFile on an unknown sandbox returns
// ErrNotFound.
func TestWriteFileNotFoundSandbox(t *testing.T) {
	p := local.New()
	err := p.WriteFile(context.Background(), "no-such-sandbox", "f.txt", []byte("x"))
	if !isNotFound(err) {
		t.Fatalf("WriteFile unknown sandbox: got %v, want ErrNotFound", err)
	}
}

// TestWriteFileMkdirError verifies WriteFile fails when a parent path
// component is an existing regular file (MkdirAll cannot create the dir).
func TestWriteFileMkdirError(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	if err := p.WriteFile(ctx, sb.ID, "afile", []byte("x")); err != nil {
		t.Fatalf("WriteFile seed: %v", err)
	}
	// "afile" is a regular file, so creating "afile/child" must fail at mkdir.
	err := p.WriteFile(ctx, sb.ID, "afile/child.txt", []byte("y"))
	if err == nil {
		t.Fatal("WriteFile under a file: got nil error, want mkdir error")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Fatalf("WriteFile under a file: err = %v, want a mkdir error", err)
	}
}

// TestWriteFileWriteError verifies WriteFile fails when the target path is an
// existing directory (the os.WriteFile call cannot write to it).
func TestWriteFileWriteError(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	if err := p.WriteFile(ctx, sb.ID, "somedir/keep.txt", []byte("x")); err != nil {
		t.Fatalf("WriteFile seed: %v", err)
	}
	// "somedir" is a directory; writing to it as a file must fail.
	err := p.WriteFile(ctx, sb.ID, "somedir", []byte("y"))
	if err == nil {
		t.Fatal("WriteFile onto a directory: got nil error, want write error")
	}
	if !strings.Contains(err.Error(), "write file") {
		t.Fatalf("WriteFile onto a directory: err = %v, want a write-file error", err)
	}
}

// TestListFilesNotFoundSandbox verifies ListFiles on an unknown sandbox
// returns ErrNotFound.
func TestListFilesNotFoundSandbox(t *testing.T) {
	p := local.New()
	_, err := p.ListFiles(context.Background(), "no-such-sandbox", ".")
	if !isNotFound(err) {
		t.Fatalf("ListFiles unknown sandbox: got %v, want ErrNotFound", err)
	}
}

// TestListFilesMissingDir verifies ListFiles on a non-existent directory maps
// the ErrNotExist to sandbox.ErrNotFound.
func TestListFilesMissingDir(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	_, err := p.ListFiles(ctx, sb.ID, "no-such-dir")
	if !isNotFound(err) {
		t.Fatalf("ListFiles missing dir: got %v, want ErrNotFound", err)
	}
}

// TestListFilesOnFile verifies ListFiles on a path that is a regular file
// returns a wrapped error distinct from ErrNotFound (the non-ErrNotExist
// ReadDir-error branch, e.g. ENOTDIR).
func TestListFilesOnFile(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	if err := p.WriteFile(ctx, sb.ID, "plain.txt", []byte("x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := p.ListFiles(ctx, sb.ID, "plain.txt")
	if err == nil {
		t.Fatal("ListFiles on a file: got nil error, want read error")
	}
	if isNotFound(err) {
		t.Fatalf("ListFiles on a file: got ErrNotFound, want a non-NotFound error: %v", err)
	}
}

// TestListFilesReportsDirEntry verifies ListFiles reports directories with
// IsDir set and files with their size, exercising the entry-mapping loop.
func TestListFilesReportsDirEntry(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	if err := p.WriteFile(ctx, sb.ID, "nested/inner.txt", []byte("data")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := p.WriteFile(ctx, sb.ID, "top.txt", []byte("12345")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	files, err := p.ListFiles(ctx, sb.ID, ".")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	byName := make(map[string]sandbox.FileInfo, len(files))
	for _, f := range files {
		byName[f.Name] = f
	}
	dir, ok := byName["nested"]
	if !ok || !dir.IsDir {
		t.Fatalf("entry 'nested' = %+v, want a directory", dir)
	}
	top, ok := byName["top.txt"]
	if !ok || top.IsDir || top.Size != 5 {
		t.Fatalf("entry 'top.txt' = %+v, want a 5-byte regular file", top)
	}
}

// TestHealthCheckUnknown verifies HealthCheck on an unknown sandbox returns
// ErrNotFound.
func TestHealthCheckUnknown(t *testing.T) {
	p := local.New()
	if err := p.HealthCheck(context.Background(), "no-such-sandbox"); !isNotFound(err) {
		t.Fatalf("HealthCheck unknown: got %v, want ErrNotFound", err)
	}
}

// TestHealthCheckDirRemoved verifies HealthCheck reports ErrNotFound when the
// sandbox's temp directory is removed out from under the provider (still
// registered in the id map, but gone on disk).
func TestHealthCheckDirRemoved(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	// Remove the sandbox's backing directory out from under the provider by
	// running rmdir against the sandbox's own working directory.
	if _, err := p.Exec(ctx, sb.ID, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "rmdir \"$PWD\""},
	}); err != nil {
		t.Fatalf("Exec rmdir: %v", err)
	}
	if err := p.HealthCheck(ctx, sb.ID); !isNotFound(err) {
		t.Fatalf("HealthCheck after dir removed: got %v, want ErrNotFound", err)
	}
}

// drain reads a stream to io.EOF and returns the combined output.
func drain(t *testing.T, stream sandbox.ExecStream) string {
	t.Helper()
	var out []byte
	for {
		chunk, err := stream.Recv()
		if chunk != nil {
			out = append(out, chunk...)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("Recv: unexpected error: %v", err)
			}
			break
		}
	}
	return string(out)
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, sandbox.ErrNotFound) || strings.Contains(err.Error(), "not found")
}

// rootedFixture returns a provider rooted at outer/work plus the outer
// directory holding a secret file the sandbox must never reach.
func rootedFixture(t *testing.T) (*local.Provider, string, string) {
	t.Helper()
	outer := t.TempDir()
	work := filepath.Join(outer, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outer, "secret.txt"), []byte("top-secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	p := local.NewAt(work)
	sb, err := p.Create(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return p, sb.ID, outer
}

// TestReadFileRejectsParentTraversal asserts a "../" path cannot read a file
// above the sandbox root.
func TestReadFileRejectsParentTraversal(t *testing.T) {
	p, id, _ := rootedFixture(t)
	data, err := p.ReadFile(context.Background(), id, "../secret.txt")
	if err == nil {
		t.Fatalf("ReadFile(../secret.txt) = %q, nil; want an error", data)
	}
	if strings.Contains(string(data), "top-secret") {
		t.Fatalf("ReadFile leaked host file contents: %q", data)
	}
}

// TestWriteFileRejectsParentTraversal asserts a "../" path cannot write above
// the sandbox root.
func TestWriteFileRejectsParentTraversal(t *testing.T) {
	p, id, outer := rootedFixture(t)
	if err := p.WriteFile(context.Background(), id, "../planted.txt", []byte("x")); err == nil {
		t.Fatal("WriteFile(../planted.txt) = nil; want an error")
	}
	if _, err := os.Stat(filepath.Join(outer, "planted.txt")); err == nil {
		t.Fatal("WriteFile created a file above the sandbox root")
	}
}

// TestListFilesRejectsParentTraversal asserts a "../" path cannot list a
// directory above the sandbox root.
func TestListFilesRejectsParentTraversal(t *testing.T) {
	p, id, _ := rootedFixture(t)
	entries, err := p.ListFiles(context.Background(), id, "..")
	if err == nil {
		t.Fatalf("ListFiles(..) = %+v, nil; want an error", entries)
	}
}

// TestExecCwdRejectsParentTraversal asserts a "../" Cwd cannot move the command
// above the sandbox root.
func TestExecCwdRejectsParentTraversal(t *testing.T) {
	p, id, _ := rootedFixture(t)
	if _, err := p.Exec(context.Background(), id, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "pwd"},
		Cwd:  "..",
	}); err == nil {
		t.Fatal("Exec with Cwd=.. = nil; want an error")
	}
}

// TestStreamExecCwdRejectsParentTraversal asserts StreamExec confines Cwd the
// same way Exec does.
func TestStreamExecCwdRejectsParentTraversal(t *testing.T) {
	p, id, _ := rootedFixture(t)
	stream, err := p.StreamExec(context.Background(), id, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "pwd"},
		Cwd:  "..",
	})
	if err == nil {
		_ = stream.Close()
		t.Fatal("StreamExec with Cwd=.. = nil; want an error")
	}
}
