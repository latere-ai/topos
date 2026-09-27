// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cellastub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/httpjson"
)

// outputCap is the synchronous route's bound on each output, whose head
// is kept.
const outputCap = 1 << 20

// waitDelay bounds how long a command that exited may keep its output
// open through a child it left behind.
const waitDelay = 2 * time.Second

// inputGrace is how long a command whose session went away has after its
// input closed before it is killed, so a helper that cancels its own
// process group on the input's end has the time to.
const inputGrace = 2 * time.Second

// MaxBodyBytes is Cella's default bound on a JSON body, the exec route's
// and the exec socket's first message alike.
const MaxBodyBytes = 64 << 10

// envPattern is the name a command's variable must have.
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnv are the variables Cella sets itself and refuses from a
// caller: its own, and the egress gateway's proxy and trust.
var reservedEnv = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO", "CURL_CA_BUNDLE"}

// validExec holds a command to Cella's rule: no NUL in an argument, each
// variable named as a shell names one and none of Cella's own, and a
// working directory in the workspace.
func validExec(sb sandbox, req execRequest) error {
	for _, arg := range req.Command {
		if strings.ContainsRune(arg, 0) {
			return errors.New("command contains NUL")
		}
	}
	for k, v := range req.Env {
		if !envPattern.MatchString(k) || strings.HasPrefix(k, "CELLA_") || slices.Contains(reservedEnv, strings.ToUpper(k)) || strings.ContainsRune(v, 0) {
			return fmt.Errorf("invalid exec environment variable %q", k)
		}
	}
	if req.Workdir != "" {
		if _, err := inWorkspace(sb, req.Workdir); err != nil {
			return errors.New("workdir must be an absolute path in the workspace")
		}
	}
	return nil
}

// execRequest is the body of both exec routes and the first frame of the
// socket.
type execRequest struct {
	Command []string          `json:"command"`
	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
	Cols    int               `json:"cols,omitempty"`
	Rows    int               `json:"rows,omitempty"`
}

// capped keeps the head of an output, up to outputCap bytes.
type capped struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := outputCap - c.buf.Len(); len(p) > room {
		c.truncated = true
		c.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return c.buf.Write(p)
}

// execWait serves POST /exec?wait=1: the command's two outputs, each
// capped, and its exit code, 124 when its timeout ended it.
func (s *Server) execWait(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("wait") != "1" {
		refuse(w, http.StatusBadRequest, "bad_request", "the stub serves the ?wait=1 form of this route alone")
		return
	}
	var req execRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Cols != 0 || req.Rows != 0 {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			refuse(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
			return
		}
		refuse(w, http.StatusBadRequest, "invalid_field", "the body is one exec request without a window")
		return
	}
	timeout, err := execTimeout(req.Timeout)
	if err != nil || len(req.Command) == 0 {
		refuse(w, http.StatusBadRequest, "invalid_field", "the command is empty, or the timeout is not positive and at most 1h")
		return
	}
	sb, ok := s.running(w, r)
	if !ok {
		return
	}
	if err := validExec(sb, req); err != nil {
		refuse(w, http.StatusBadRequest, "invalid_field", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, lookPath(req.Command[0]), req.Command[1:]...)
	cmd.Dir = workdir(sb, req.Workdir)
	cmd.Env = environ(sb, req.Env)
	cmd.WaitDelay = waitDelay
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	began := time.Now()
	err = cmd.Run()
	code := 0
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil && r.Context().Err() == nil:
		code = 124
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		refuse(w, http.StatusServiceUnavailable, "driver_unavailable", err.Error())
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"exitCode": code, "stdout": stdout.buf.String(), "stderr": stderr.buf.String(),
		"truncated": stdout.truncated || stderr.truncated, "durationMs": time.Since(began).Milliseconds(),
	})
}

// execSocket serves GET /exec: a command whose input the client writes in
// binary frames, whose two outputs interleave into binary frames back,
// and whose exit is a text frame followed by a close, as Cella's socket
// is. The protocol has no half close: the command's input ends when the
// session does.
func (s *Server) execSocket(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.running(w, r)
	if !ok {
		return
	}
	c, err := upgrade(w, r)
	if err != nil {
		refuse(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	defer func() { s.record(c.conn.Close()) }()
	kind, first, err := c.read()
	switch {
	case err == nil && kind == opText && len(first) > MaxBodyBytes:
		s.record(c.closeWith(1009, "the first frame is past the body limit"))
		return
	case err != nil || kind != opText:
		s.record(c.closeWith(1008, "the first frame is the JSON request, as text"))
		return
	}
	var req execRequest
	dec := json.NewDecoder(bytes.NewReader(first))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.record(c.closeWith(1008, err.Error()))
		return
	}
	timeout, err := execTimeout(req.Timeout)
	switch {
	case err != nil, len(req.Command) == 0:
		s.fail(c, "invalid_field", "the command is empty, or the timeout is not positive and at most 1h")
		return
	case req.Cols > 0 && req.Rows > 0:
		s.fail(c, "capability_unsupported", "the stub serves no terminal")
		return
	}
	if err := validExec(sb, req); err != nil {
		s.fail(c, "invalid_field", err.Error())
		return
	}
	s.drive(r.Context(), c, sb, req, timeout)
}

// fail sends the error frame and closes with 1011.
func (s *Server) fail(c *wsConn, code, detail string) {
	frame, err := json.Marshal(map[string]any{"error": map[string]any{
		"code": code, "message": messages[code], "details": map[string]any{"detail": detail, "request_id": "req_stub"},
	}})
	if err == nil {
		err = c.write(opText, frame)
	}
	s.record(errors.Join(err, c.closeWith(closeInternal, code)))
}

// drive runs the command until it exits, its timeout passes, or the
// client goes away. The stub ends the command itself in each case, input
// first as Cella does, so the request's context never kills it.
func (s *Server) drive(ctx context.Context, c *wsConn, sb sandbox, req execRequest, timeout time.Duration) {
	cmd := exec.CommandContext(context.WithoutCancel(ctx), lookPath(req.Command[0]), req.Command[1:]...)
	cmd.Dir = workdir(sb, req.Workdir)
	cmd.Env = environ(sb, req.Env)
	cmd.WaitDelay = waitDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.fail(c, "driver_unavailable", err.Error())
		return
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		s.fail(c, "driver_unavailable", err.Error())
		return
	}
	cmd.Stdout, cmd.Stderr = outW, outW
	if err := cmd.Start(); err != nil {
		s.fail(c, "driver_unavailable", errors.Join(err, outR.Close(), outW.Close()).Error())
		return
	}
	if err := outW.Close(); err != nil {
		s.fail(c, "driver_unavailable", err.Error())
		return
	}
	var once sync.Once
	gone := make(chan struct{})
	leave := func() { once.Do(func() { close(gone) }) }
	go func() {
		for {
			kind, payload, err := c.read()
			if err != nil {
				leave()
				return
			}
			if kind == opBinary && len(payload) > 0 {
				if _, err := stdin.Write(payload); err != nil {
					// The command closed its input; the rest is dropped,
					// as a pipe drops it.
					continue
				}
			}
		}
	}()
	timedOut := make(chan struct{})
	exited := make(chan struct{})
	defer close(exited)
	timer := time.AfterFunc(timeout, func() {
		close(timedOut)
		s.record(kill(cmd))
	})
	defer timer.Stop()
	go func() {
		<-gone
		// The session is gone: the input ends first, as Cella's does, then
		// the command is killed if it is still there.
		s.record(stdin.Close())
		t := time.NewTimer(inputGrace)
		defer t.Stop()
		select {
		case <-t.C:
			s.record(kill(cmd))
		case <-exited:
		}
	}()
	delivered := true
	buf := make([]byte, 32<<10)
	for {
		n, rerr := outR.Read(buf)
		if n > 0 && delivered {
			if err := c.write(opBinary, buf[:n]); err != nil {
				delivered = false
				leave()
			}
		}
		if rerr != nil {
			break
		}
	}
	werr := cmd.Wait()
	cerr := outR.Close()
	if !delivered {
		return
	}
	code := cmd.ProcessState.ExitCode()
	select {
	case <-timedOut:
		code = 124
	default:
	}
	var ee *exec.ExitError
	if werr != nil && !errors.As(werr, &ee) && !errors.Is(werr, exec.ErrWaitDelay) {
		s.fail(c, "driver_unavailable", werr.Error())
		return
	}
	if cerr != nil {
		s.fail(c, "driver_unavailable", cerr.Error())
		return
	}
	frame, err := json.Marshal(map[string]int{"exit": code})
	if err == nil {
		err = c.write(opText, frame)
	}
	if err == nil {
		err = c.closeWith(closeNormal, "")
	}
	s.record(err)
	leave()
}

// kill ends a command that may already have exited.
func kill(cmd *exec.Cmd) error {
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
