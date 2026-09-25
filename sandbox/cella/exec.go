// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package cella

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	cellaclient "latere.ai/x/cella/client"

	"latere.ai/x/topos/sandbox"
)

// maxExecTimeout is the longest command the control plane runs: its exec
// routes refuse a timeout above an hour.
const maxExecTimeout = time.Hour

// The terminal phases this provider reports, from the interface's vocabulary.
const (
	phaseExited = "exited"
	phaseKilled = "killed"
)

// Exec runs a command to completion on the control plane's synchronous exec
// route and returns its output and exit code. The command's standard input is
// at end of file, as in the local provider.
//
// Stdout carries the command's standard output followed by its standard error,
// and Stderr stays empty: callers read Stdout alone for the combined output.
// The control plane keeps the first mebibyte of each channel. A context that
// ends while the command runs is the killed phase with no error.
func (p *Provider) Exec(ctx context.Context, id string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
	if len(opts.Argv) == 0 {
		return sandbox.ExecResult{}, errors.New("cella: exec: argv is empty")
	}
	if len(opts.SecretEnv) > 0 {
		return sandbox.ExecResult{}, errors.New("cella: ExecOptions.SecretEnv has no equivalent: the Cella exec route resolves no secret for one command")
	}
	timeout, live := execTimeout(ctx)
	if !live {
		return sandbox.ExecResult{Phase: phaseKilled}, nil
	}
	req := cellaclient.ExecRequest{
		Command: opts.Argv,
		Env:     opts.Env,
		Timeout: timeout.String(),
	}
	if opts.Cwd != "" {
		req.Workdir = resolvePath(opts.Cwd)
	}
	res, _, err := p.core.Exec(ctx, id, req)
	// The context decides killed, not the error's type: the client returns a
	// cancellation as it is and wraps a deadline in *client.Unreachable.
	if ctx.Err() != nil {
		return sandbox.ExecResult{Phase: phaseKilled}, nil
	}
	if err != nil {
		return sandbox.ExecResult{}, mapError(err)
	}
	return sandbox.ExecResult{
		Stdout:   []byte(res.Stdout + res.Stderr),
		ExitCode: res.ExitCode,
		Phase:    phaseExited,
	}, nil
}

// execTimeout is the timeout sent with a command: the context's remaining
// time, at most maxExecTimeout, and maxExecTimeout when there is no deadline,
// so the control plane ends a command its caller stopped waiting for rather
// than applying its own shorter default. It reports false for a context whose
// deadline has already passed.
func execTimeout(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return maxExecTimeout, ctx.Err() == nil
	}
	remaining := time.Until(deadline).Round(time.Millisecond)
	if remaining <= 0 || ctx.Err() != nil {
		return 0, false
	}
	return min(remaining, maxExecTimeout), true
}

// StreamExec runs the command as Exec does and returns a stream holding its
// whole output as one chunk, delivered once the command has ended. The control
// plane's live exec form keeps the command's standard input open until the
// session closes, so a command that reads its input would wait there; one
// contract for both methods is the one this provider keeps.
func (p *Provider) StreamExec(ctx context.Context, id string, opts sandbox.ExecOptions) (sandbox.ExecStream, error) {
	res, err := p.Exec(ctx, id, opts)
	if err != nil {
		return nil, err
	}
	return &execStream{pending: res.Stdout, result: res}, nil
}

// execStream implements [sandbox.ExecStream] over a command that has already
// ended.
type execStream struct {
	mu      sync.Mutex
	pending []byte
	result  sandbox.ExecResult
}

// Recv returns the command's output on the first call, and io.EOF after it.
func (s *execStream) Recv() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil, io.EOF
	}
	out := s.pending
	s.pending = nil
	return out, nil
}

// Result returns the terminal ExecResult.
func (s *execStream) Result() sandbox.ExecResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// Close releases the stream. Safe to call multiple times.
func (s *execStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = nil
	return nil
}
