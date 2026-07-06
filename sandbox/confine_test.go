// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package sandbox_test

import (
	"context"
	"errors"
	"testing"

	"latere.ai/x/topos/sandbox"
)

// spyProvider records which paths reached the inner provider, so a test can prove
// a confined refusal never touches the backend.
type spyProvider struct {
	reads     []string
	writes    []string
	lists     []string
	execCwds  []string
	created   int
	destroyed int
	healths   int
}

func (s *spyProvider) Create(context.Context, sandbox.CreateOptions) (sandbox.Sandbox, error) {
	s.created++
	return sandbox.Sandbox{ID: "sb"}, nil
}
func (s *spyProvider) Destroy(context.Context, string) error { s.destroyed++; return nil }
func (s *spyProvider) Exec(_ context.Context, _ string, o sandbox.ExecOptions) (sandbox.ExecResult, error) {
	s.execCwds = append(s.execCwds, o.Cwd)
	return sandbox.ExecResult{}, nil
}
func (s *spyProvider) StreamExec(context.Context, string, sandbox.ExecOptions) (sandbox.ExecStream, error) {
	return nil, nil
}
func (s *spyProvider) ReadFile(_ context.Context, _, p string) ([]byte, error) {
	s.reads = append(s.reads, p)
	return []byte("ok"), nil
}
func (s *spyProvider) WriteFile(_ context.Context, _, p string, _ []byte) error {
	s.writes = append(s.writes, p)
	return nil
}
func (s *spyProvider) ListFiles(_ context.Context, _, p string) ([]sandbox.FileInfo, error) {
	s.lists = append(s.lists, p)
	return nil, nil
}
func (s *spyProvider) HealthCheck(context.Context, string) error { s.healths++; return nil }

func TestConfineAllowsInRoot(t *testing.T) {
	spy := &spyProvider{}
	c := sandbox.Confine(spy, "/work")
	if _, err := c.ReadFile(context.Background(), "sb", "src/main.go"); err != nil {
		t.Fatalf("in-root read refused: %v", err)
	}
	if len(spy.reads) != 1 || spy.reads[0] != "src/main.go" {
		t.Fatalf("inner read = %v, want the original path passed through", spy.reads)
	}
}

func TestConfineRejectsEscape(t *testing.T) {
	spy := &spyProvider{}
	c := sandbox.Confine(spy, "/work")
	for _, p := range []string{"../secret", "../../etc/passwd", "a/../../b"} {
		if _, err := c.ReadFile(context.Background(), "sb", p); !errors.Is(err, sandbox.ErrConfined) {
			t.Fatalf("read %q = %v, want ErrConfined", p, err)
		}
	}
	if len(spy.reads) != 0 {
		t.Fatalf("escape reached inner provider: %v", spy.reads)
	}
}

func TestConfineRejectsAbsoluteOutsideRoot(t *testing.T) {
	spy := &spyProvider{}
	c := sandbox.Confine(spy, "/work")
	if _, err := c.ReadFile(context.Background(), "sb", "/etc/passwd"); !errors.Is(err, sandbox.ErrConfined) {
		t.Fatalf("absolute outside root = %v, want ErrConfined", err)
	}
	// An absolute path INSIDE root is allowed and passed through.
	if _, err := c.ReadFile(context.Background(), "sb", "/work/src/a.go"); err != nil {
		t.Fatalf("absolute in-root read refused: %v", err)
	}
	if len(spy.reads) != 1 {
		t.Fatalf("inner reads = %v, want only the in-root absolute path", spy.reads)
	}
}

func TestConfineRejectsSecrets(t *testing.T) {
	spy := &spyProvider{}
	c := sandbox.Confine(spy, "/work")
	secrets := []string{
		".env", ".env.local", "config/.env", "key.pem", "id_rsa", "id_ed25519",
		".ssh/id_rsa", "deep/.ssh/known_hosts", ".aws/credentials", ".netrc",
	}
	for _, p := range secrets {
		if _, err := c.ReadFile(context.Background(), "sb", p); !errors.Is(err, sandbox.ErrConfined) {
			t.Fatalf("read secret %q = %v, want ErrConfined", p, err)
		}
		if err := c.WriteFile(context.Background(), "sb", p, nil); !errors.Is(err, sandbox.ErrConfined) {
			t.Fatalf("write secret %q = %v, want ErrConfined", p, err)
		}
	}
	if len(spy.reads) != 0 || len(spy.writes) != 0 {
		t.Fatalf("a secret path reached inner: reads=%v writes=%v", spy.reads, spy.writes)
	}
}

func TestConfineListAndExecCwdConfined(t *testing.T) {
	spy := &spyProvider{}
	c := sandbox.Confine(spy, "/work")
	if _, err := c.ListFiles(context.Background(), "sb", "../.."); !errors.Is(err, sandbox.ErrConfined) {
		t.Fatalf("list escape = %v, want ErrConfined", err)
	}
	if _, err := c.Exec(context.Background(), "sb", sandbox.ExecOptions{Argv: []string{"ls"}, Cwd: "../etc"}); !errors.Is(err, sandbox.ErrConfined) {
		t.Fatalf("exec cwd escape = %v, want ErrConfined", err)
	}
	// A normal in-root exec (empty cwd) passes through.
	if _, err := c.Exec(context.Background(), "sb", sandbox.ExecOptions{Argv: []string{"ls"}}); err != nil {
		t.Fatalf("in-root exec refused: %v", err)
	}
	if len(spy.execCwds) != 1 {
		t.Fatalf("exec reached inner %d times, want 1 (only the allowed one)", len(spy.execCwds))
	}
}

func TestConfineStreamExecCwdConfined(t *testing.T) {
	spy := &spyProvider{}
	c := sandbox.Confine(spy, "/work")
	if _, err := c.StreamExec(context.Background(), "sb", sandbox.ExecOptions{Cwd: "../etc"}); !errors.Is(err, sandbox.ErrConfined) {
		t.Fatalf("stream exec cwd escape = %v, want ErrConfined", err)
	}
	// An in-root stream exec (empty cwd) passes through to the inner provider.
	if _, err := c.StreamExec(context.Background(), "sb", sandbox.ExecOptions{Argv: []string{"tail"}}); err != nil {
		t.Fatalf("in-root stream exec refused: %v", err)
	}
}

func TestConfineDefaultRootAndWriteList(t *testing.T) {
	spy := &spyProvider{}
	c := sandbox.Confine(spy, "") // empty root defaults to "."
	// A normal relative path is allowed and passes through (write + list success).
	if err := c.WriteFile(context.Background(), "sb", "a/b.txt", []byte("x")); err != nil {
		t.Fatalf("in-root write refused: %v", err)
	}
	if _, err := c.ListFiles(context.Background(), "sb", "a"); err != nil {
		t.Fatalf("in-root list refused: %v", err)
	}
	if len(spy.writes) != 1 || len(spy.lists) != 1 {
		t.Fatalf("write/list did not pass through: writes=%v lists=%v", spy.writes, spy.lists)
	}
	// Escape is still rejected under the default root.
	if _, err := c.ListFiles(context.Background(), "sb", "../x"); !errors.Is(err, sandbox.ErrConfined) {
		t.Fatalf("escape under default root = %v, want ErrConfined", err)
	}
}

func TestConfinePassesThroughNoPathMethods(t *testing.T) {
	spy := &spyProvider{}
	c := sandbox.Confine(spy, "/work")
	_, _ = c.Create(context.Background(), sandbox.CreateOptions{})
	_ = c.Destroy(context.Background(), "sb")
	_ = c.HealthCheck(context.Background(), "sb")
	if spy.created != 1 || spy.destroyed != 1 || spy.healths != 1 {
		t.Fatalf("no-path methods did not pass through: %+v", spy)
	}
}
