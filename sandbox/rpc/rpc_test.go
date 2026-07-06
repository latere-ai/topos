// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package rpc_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
	"latere.ai/x/topos/sandbox/rpc"
)

// pipeClient wires an rpc client to a Serve peer backed by `backend` over an
// in-memory pipe (no network). It returns the client provider and a stop func.
func pipeClient(t *testing.T, backend sandbox.Provider) (sandbox.Provider, func()) {
	t.Helper()
	cConn, sConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		_ = rpc.Serve(context.Background(), sConn, backend)
		close(done)
	}()
	cli := rpc.NewClient(cConn)
	stop := func() {
		if cl, ok := cli.(interface{ Close() error }); ok {
			_ = cl.Close()
		}
		_ = sConn.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	return cli, stop
}

// TestRPCRoundTripOverPipe drives every unary method through the client and
// asserts it reaches a real sandbox/local backend on the other end and returns
// the same result — the client is indistinguishable from a direct provider.
func TestRPCRoundTripOverPipe(t *testing.T) {
	ctx := context.Background()
	cli, stop := pipeClient(t, local.New())
	defer stop()

	sb, err := cli.Create(ctx, sandbox.CreateOptions{Name: "rt"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.ID == "" {
		t.Fatal("Create returned no id over the wire")
	}
	if err := cli.HealthCheck(ctx, sb.ID); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}

	// WriteFile → ReadFile round-trip (binary content, exact bytes).
	content := []byte("hello\x00\xffworld")
	if err := cli.WriteFile(ctx, sb.ID, "dir/a.bin", content); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := cli.ReadFile(ctx, sb.ID, "dir/a.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("ReadFile = %q, want %q", got, content)
	}

	// ListFiles sees the written entry.
	files, err := cli.ListFiles(ctx, sb.ID, "dir")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	var found bool
	for _, f := range files {
		if f.Name == "a.bin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListFiles did not return a.bin: %+v", files)
	}

	// Exec round-trips stdout + exit code.
	res, err := cli.Exec(ctx, sb.ID, sandbox.ExecOptions{Argv: []string{"sh", "-c", "echo hi"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "hi") {
		t.Fatalf("Exec result = %+v, want exit 0 + 'hi'", res)
	}

	if err := cli.Destroy(ctx, sb.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

// TestRPCNotFoundRoundTrips proves a typed sentinel (ErrNotFound) survives the
// wire so callers can errors.Is it.
func TestRPCNotFoundRoundTrips(t *testing.T) {
	ctx := context.Background()
	cli, stop := pipeClient(t, local.New())
	defer stop()

	sb, _ := cli.Create(ctx, sandbox.CreateOptions{})
	if _, err := cli.ReadFile(ctx, sb.ID, "missing.txt"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("ReadFile missing = %v, want ErrNotFound across the wire", err)
	}
}

// TestRPCConfinedRefusedServerSide proves the ratified trust protections hold over
// the wire: a client request to an escaping or secret path is refused server-side
// with ErrConfined (the backend is a confined local provider).
func TestRPCConfinedRefusedServerSide(t *testing.T) {
	ctx := context.Background()
	cli, stop := pipeClient(t, sandbox.Confine(local.New(), "."))
	defer stop()

	sb, _ := cli.Create(ctx, sandbox.CreateOptions{})
	for _, p := range []string{"../../etc/passwd", ".ssh/id_rsa", ".env"} {
		if _, err := cli.ReadFile(ctx, sb.ID, p); !errors.Is(err, sandbox.ErrConfined) {
			t.Fatalf("ReadFile %q over wire = %v, want ErrConfined", p, err)
		}
	}
}

// errProvider returns a fixed error from every method, to prove error kinds
// round-trip across the wire.
type errProvider struct{ err error }

func (e errProvider) Create(context.Context, sandbox.CreateOptions) (sandbox.Sandbox, error) {
	return sandbox.Sandbox{}, e.err
}
func (e errProvider) Destroy(context.Context, string) error { return e.err }
func (e errProvider) Exec(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, e.err
}
func (e errProvider) StreamExec(context.Context, string, sandbox.ExecOptions) (sandbox.ExecStream, error) {
	return nil, e.err
}
func (e errProvider) ReadFile(context.Context, string, string) ([]byte, error) { return nil, e.err }
func (e errProvider) WriteFile(context.Context, string, string, []byte) error  { return e.err }
func (e errProvider) ListFiles(context.Context, string, string) ([]sandbox.FileInfo, error) {
	return nil, e.err
}
func (e errProvider) HealthCheck(context.Context, string) error { return e.err }

func TestRPCErrorKindsRoundTrip(t *testing.T) {
	ctx := context.Background()

	// ErrConflict round-trips on Create.
	cli, stop := pipeClient(t, errProvider{err: sandbox.ErrConflict})
	if _, err := cli.Create(ctx, sandbox.CreateOptions{}); !errors.Is(err, sandbox.ErrConflict) {
		t.Fatalf("Create = %v, want ErrConflict across wire", err)
	}
	stop()

	// *APIError round-trips with its fields on Destroy.
	api := &sandbox.APIError{Status: 503, Code: "unavailable", Message: "down", RequestID: "req1"}
	cli, stop = pipeClient(t, errProvider{err: api})
	err := cli.HealthCheck(ctx, "sb")
	var got *sandbox.APIError
	if !errors.As(err, &got) || got.Status != 503 || got.Code != "unavailable" {
		t.Fatalf("HealthCheck = %v, want *APIError{503,unavailable}", err)
	}
	stop()

	// A generic error round-trips as a plain (non-sentinel) error carrying its msg.
	cli, stop = pipeClient(t, errProvider{err: errors.New("boom")})
	if err := cli.WriteFile(ctx, "sb", "a", nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("WriteFile = %v, want an error carrying 'boom'", err)
	}
	if _, err := cli.ListFiles(ctx, "sb", "."); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("ListFiles = %v, want an error carrying 'boom'", err)
	}
	if _, err := cli.Exec(ctx, "sb", sandbox.ExecOptions{}); err == nil {
		t.Fatalf("Exec should surface the backend error")
	}
	stop()
}

// TestRPCTransportError: once the connection is closed, a call fails with a
// transport error rather than hanging.
func TestRPCTransportError(t *testing.T) {
	ctx := context.Background()
	cConn, sConn := net.Pipe()
	_ = sConn.Close()
	cli := rpc.NewClient(cConn)
	_ = cConn.Close()

	// Every method surfaces the transport failure rather than hanging.
	if _, err := cli.Create(ctx, sandbox.CreateOptions{}); err == nil {
		t.Fatal("Create on a closed conn should error")
	}
	if err := cli.Destroy(ctx, "sb"); err == nil {
		t.Fatal("Destroy on a closed conn should error")
	}
	if _, err := cli.Exec(ctx, "sb", sandbox.ExecOptions{}); err == nil {
		t.Fatal("Exec on a closed conn should error")
	}
	if _, err := cli.ReadFile(ctx, "sb", "a"); err == nil {
		t.Fatal("ReadFile on a closed conn should error")
	}
	if err := cli.WriteFile(ctx, "sb", "a", nil); err == nil {
		t.Fatal("WriteFile on a closed conn should error")
	}
	if _, err := cli.ListFiles(ctx, "sb", "."); err == nil {
		t.Fatal("ListFiles on a closed conn should error")
	}
	if err := cli.HealthCheck(ctx, "sb"); err == nil {
		t.Fatal("HealthCheck on a closed conn should error")
	}
}

// TestRPCStreamExecUnsupported: streaming exec is a named follow-on leaf.
func TestRPCStreamExecUnsupported(t *testing.T) {
	cli, stop := pipeClient(t, local.New())
	defer stop()
	if _, err := cli.StreamExec(context.Background(), "sb", sandbox.ExecOptions{}); !errors.Is(err, rpc.ErrStreamUnsupported) {
		t.Fatalf("StreamExec = %v, want ErrStreamUnsupported", err)
	}
}
