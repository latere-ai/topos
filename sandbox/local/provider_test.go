package local_test

import (
	"context"
	"strings"
	"testing"

	"latere.ai/x/agents/internal/sandbox"
	"latere.ai/x/agents/internal/sandbox/local"
)

// assertInterface ensures Provider implements SandboxProvider at compile time.
var _ sandbox.SandboxProvider = (*local.Provider)(nil)

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

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return err == sandbox.ErrNotFound || strings.Contains(err.Error(), "not found")
}
