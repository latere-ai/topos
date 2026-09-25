// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	cellaclient "latere.ai/x/cella/client"

	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/cella"
)

// running is a fake core with one running sandbox and the provider on it.
func running(t *testing.T) (*fakeCore, *cella.Provider, string) {
	t.Helper()
	f := newFakeCore(t)
	p := f.provider(t)
	sb, err := p.Create(t.Context(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return f, p, sb.ID
}

// TestExecSendsCommandAndCombinesOutput is the exec contract: the command, its
// environment and a workdir resolved under the workspace go to the synchronous
// route, and Stdout carries stdout followed by stderr, since callers read it
// alone for the combined output.
func TestExecSendsCommandAndCombinesOutput(t *testing.T) {
	f, p, id := running(t)
	f.execResult = cellaclient.ExecResult{ExitCode: 3, Stdout: "out\n", Stderr: "err\n"}
	res, err := p.Exec(t.Context(), id, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "work"},
		Env:  map[string]string{"DEBUG": "1"},
		Cwd:  "app/src",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(res.Stdout) != "out\nerr\n" || res.Stderr != nil || res.ExitCode != 3 || res.Phase != "exited" {
		t.Errorf("Exec = %+v, want stdout then stderr, exit 3, exited", res)
	}
	req := f.lastExec(t)
	if !slices.Equal(req.Command, []string{"sh", "-c", "work"}) || req.Env["DEBUG"] != "1" || req.Workdir != "/workspace/app/src" {
		t.Errorf("exec body = %+v", req)
	}
	// With no deadline the server is given its longest bound rather than its
	// ten-minute default.
	if req.Timeout != "1h0m0s" {
		t.Errorf("timeout = %q, want 1h0m0s", req.Timeout)
	}
}

func TestExecAbsoluteCwdAndNoCwd(t *testing.T) {
	f, p, id := running(t)
	if _, err := p.Exec(t.Context(), id, sandbox.ExecOptions{Argv: []string{"pwd"}, Cwd: "/workspace/../workspace/x"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := f.lastExec(t).Workdir; got != "/workspace/x" {
		t.Errorf("workdir = %q, want the cleaned absolute path", got)
	}
	if _, err := p.Exec(t.Context(), id, sandbox.ExecOptions{Argv: []string{"pwd"}}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := f.lastExec(t).Workdir; got != "" {
		t.Errorf("workdir = %q, want the sandbox's own", got)
	}
}

// TestExecTimeoutFollowsTheDeadline: the server ends the command when its
// caller stops waiting.
func TestExecTimeoutFollowsTheDeadline(t *testing.T) {
	f, p, id := running(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if _, err := p.Exec(ctx, id, sandbox.ExecOptions{Argv: []string{"true"}}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got, err := time.ParseDuration(f.lastExec(t).Timeout)
	if err != nil || got > 2*time.Minute || got < time.Minute {
		t.Errorf("timeout = %q, want the context's remaining two minutes", f.lastExec(t).Timeout)
	}
}

// TestExecCancelledIsKilled: a context cancelled while the command runs is the
// killed phase with no error, as in the local provider.
func TestExecCancelledIsKilled(t *testing.T) {
	f, p, id := running(t)
	f.execBlock = true
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-f.execArrived
		cancel()
	}()
	res, err := p.Exec(ctx, id, sandbox.ExecOptions{Argv: []string{"sleep", "60"}})
	if err != nil || res.Phase != "killed" {
		t.Fatalf("Exec = %+v, %v, want killed and no error", res, err)
	}
}

// TestExecDeadlineIsKilled: a deadline reaches the client as a wrapped
// transport error, and is still the killed phase.
func TestExecDeadlineIsKilled(t *testing.T) {
	f, p, id := running(t)
	f.execBlock = true
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	res, err := p.Exec(ctx, id, sandbox.ExecOptions{Argv: []string{"sleep", "60"}})
	if err != nil || res.Phase != "killed" {
		t.Fatalf("Exec = %+v, %v, want killed and no error", res, err)
	}
}

func TestExecOnAnEndedContextSendsNothing(t *testing.T) {
	for name, ctx := range map[string]func() (context.Context, context.CancelFunc){
		"cancelled": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		},
		"expired": func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, p, id := running(t)
			before := len(f.requestLog())
			c, cancel := ctx()
			defer cancel()
			res, err := p.Exec(c, id, sandbox.ExecOptions{Argv: []string{"true"}})
			if err != nil || res.Phase != "killed" {
				t.Fatalf("Exec = %+v, %v, want killed", res, err)
			}
			if len(f.requestLog()) != before {
				t.Errorf("requests = %v, want no exec sent", f.requestLog()[before:])
			}
		})
	}
}

func TestExecRefusesBeforeSending(t *testing.T) {
	for name, opts := range map[string]sandbox.ExecOptions{
		"empty argv": {},
		"secret env": {Argv: []string{"deploy"}, SecretEnv: map[string]string{"KEY": "vault_key"}},
	} {
		t.Run(name, func(t *testing.T) {
			f, p, id := running(t)
			before := len(f.requestLog())
			if _, err := p.Exec(t.Context(), id, opts); err == nil {
				t.Fatal("Exec succeeded, want a refusal")
			}
			if len(f.requestLog()) != before {
				t.Error("a request was sent")
			}
		})
	}
}

func TestExecErrors(t *testing.T) {
	f, p, id := running(t)
	if _, err := p.Exec(t.Context(), "sbx_missing", sandbox.ExecOptions{Argv: []string{"true"}}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("Exec on a missing sandbox = %v, want ErrNotFound", err)
	}
	f.execErr = &reply{http.StatusConflict, "phase_conflict", "The sandbox is not running."}
	if _, err := p.Exec(t.Context(), id, sandbox.ExecOptions{Argv: []string{"true"}}); !errors.Is(err, sandbox.ErrConflict) {
		t.Errorf("Exec on a stopped sandbox = %v, want ErrConflict", err)
	}
}

// TestStreamExecDeliversTheOutputOnce: the stream holds the whole output as
// one chunk, then io.EOF, with the terminal result.
func TestStreamExecDeliversTheOutputOnce(t *testing.T) {
	f, p, id := running(t)
	f.execResult = cellaclient.ExecResult{ExitCode: 0, Stdout: "hello\n"}
	s, err := p.StreamExec(t.Context(), id, sandbox.ExecOptions{Argv: []string{"echo", "hello"}})
	if err != nil {
		t.Fatalf("StreamExec: %v", err)
	}
	chunk, err := s.Recv()
	if err != nil || string(chunk) != "hello\n" {
		t.Fatalf("Recv = %q, %v", chunk, err)
	}
	if _, err := s.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("second Recv = %v, want EOF", err)
	}
	if res := s.Result(); res.Phase != "exited" || res.ExitCode != 0 || string(res.Stdout) != "hello\n" {
		t.Errorf("Result = %+v", res)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestStreamExecEmptyOutputAndClose(t *testing.T) {
	f, p, id := running(t)
	f.execResult = cellaclient.ExecResult{ExitCode: 1, Stdout: "unread"}
	s, err := p.StreamExec(t.Context(), id, sandbox.ExecOptions{Argv: []string{"false"}})
	if err != nil {
		t.Fatalf("StreamExec: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("Recv after Close = %v, want EOF", err)
	}
	if s.Result().ExitCode != 1 {
		t.Errorf("Result = %+v", s.Result())
	}
}

func TestStreamExecError(t *testing.T) {
	_, p, _ := running(t)
	s, err := p.StreamExec(t.Context(), "sbx_missing", sandbox.ExecOptions{Argv: []string{"true"}})
	if s != nil || !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("StreamExec = %v, %v, want no stream and ErrNotFound", s, err)
	}
}
