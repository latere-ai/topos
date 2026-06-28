// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package cella

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strconv"
	"sync"
	"time"

	"latere.ai/x/topos/sandbox"
)

// defaultPollInterval is how long the log poller waits between cursor reads
// while a command is still running. Cella commands are asynchronous: start
// returns immediately and output is pulled with cursor-based polling
// (?stream=false&cursor=N), which — unlike SSE — carries the terminal phase and
// exit code inline in every envelope.
const defaultPollInterval = 250 * time.Millisecond

// createCommandReq is the POST /v1/sandboxes/{id}/commands body.
type createCommandReq struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
}

// commandResp is the Command response from starting a command.
type commandResp struct {
	CommandID string `json:"command_id"`
	Phase     string `json:"phase"`
	ExitCode  *int   `json:"exit_code"`
}

// logEnvelope is the cursor-mode logs response
// (GET .../commands/{cid}/logs?stream=false&cursor=N).
type logEnvelope struct {
	Bytes      string `json:"bytes"`
	NextCursor int64  `json:"next_cursor"`
	Phase      string `json:"phase"`
	ExitCode   *int   `json:"exit_code"`
}

// Exec runs a command to completion and returns its combined output and
// terminal status. It is implemented on top of StreamExec so the
// start → poll → terminal lifecycle lives in one place.
func (p *Provider) Exec(ctx context.Context, id string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
	stream, err := p.StreamExec(ctx, id, opts)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	defer stream.Close() //nolint:errcheck
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return sandbox.ExecResult{}, err
		}
	}
	return stream.Result(), nil
}

// StreamExec starts a command and returns a stream over its output. Output is
// pulled from Cella with cursor-based polling in a background goroutine that
// writes into an io.Pipe; Recv reads the pipe, so a slow consumer applies
// backpressure to the poller.
func (p *Provider) StreamExec(ctx context.Context, id string, opts sandbox.ExecOptions) (sandbox.ExecStream, error) {
	if len(opts.Argv) == 0 {
		return nil, errors.New("cella: exec: argv is empty")
	}

	var started commandResp
	if err := p.doJSON(ctx, "POST", "/v1/sandboxes/"+url.PathEscape(id)+"/commands",
		createCommandReq{Argv: opts.Argv, Env: opts.Env, Cwd: opts.Cwd}, &started); err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	s := &execStream{pr: pr, pw: pw}

	go p.pollLogs(ctx, id, started.CommandID, s)

	return s, nil
}

// pollLogs reads the command's combined output via cursor polling and feeds it
// into the stream's pipe, recording the terminal phase and exit code before
// closing the writer. The io.EOF a consumer observes from Recv happens-after
// the terminal fields are set, so Result is always populated by then.
func (p *Provider) pollLogs(ctx context.Context, sandboxID, commandID string, s *execStream) {
	logPath := "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/commands/" + url.PathEscape(commandID) + "/logs"
	var cursor int64
	for {
		if ctx.Err() != nil {
			p.killed(s)
			return
		}

		var env logEnvelope
		q := logPath + "?stream=false&cursor=" + strconv.FormatInt(cursor, 10)
		if err := p.doJSON(ctx, "GET", q, nil, &env); err != nil {
			// A cancelled context is a terminal "killed" phase, not a transport
			// error — matching the local provider, Exec returns the result with
			// no error. Any other failure propagates through the pipe.
			if ctx.Err() != nil {
				p.killed(s)
				return
			}
			_ = s.pw.CloseWithError(err)
			return
		}
		if env.Bytes != "" {
			// Write blocks until Recv drains it (backpressure). A consumer that
			// gives up closes the read end, surfacing here as an error.
			if _, err := s.pw.Write([]byte(env.Bytes)); err != nil {
				return
			}
		}
		cursor = env.NextCursor

		if env.Phase != string(phaseRunning) {
			s.finish(env.Phase, env.ExitCode)
			_ = s.pw.Close()
			return
		}

		select {
		case <-ctx.Done():
		case <-time.After(p.pollIntervalOrDefault()):
		}
	}
}

// killed records the killed phase and closes the stream cleanly (EOF), so a
// consumer sees the terminal phase rather than a transport error.
func (p *Provider) killed(s *execStream) {
	s.finish("killed", nil)
	_ = s.pw.Close()
}

// phaseRunning is the only non-terminal command phase.
const phaseRunning = "running"

func (p *Provider) pollIntervalOrDefault() time.Duration {
	if p.pollInterval > 0 {
		return p.pollInterval
	}
	return defaultPollInterval
}

// execStream implements [sandbox.ExecStream] over a cursor-polled command.
type execStream struct {
	pr *io.PipeReader
	pw *io.PipeWriter

	mu     sync.Mutex
	result sandbox.ExecResult
}

// Recv returns the next chunk of combined output, or io.EOF when the command
// terminates.
func (s *execStream) Recv() ([]byte, error) {
	buf := make([]byte, 4096)
	n, err := s.pr.Read(buf)
	if n > 0 {
		out := make([]byte, n)
		copy(out, buf[:n])
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

// Result returns the terminal ExecResult. Valid only after Recv returns io.EOF.
func (s *execStream) Result() sandbox.ExecResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// Close releases the pipe. Safe to call multiple times.
func (s *execStream) Close() error {
	return s.pr.Close()
}

// finish records the terminal phase and exit code under the lock, preserving
// the Stdout that Recv accumulated.
func (s *execStream) finish(phase string, exitCode *int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result.Phase = phase
	if exitCode != nil {
		s.result.ExitCode = *exitCode
	}
}
