// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"latere.ai/x/cella/client"

	"latere.ai/x/topos/machine"
)

// killGrace is the helper's grace between SIGTERM and SIGKILL, and
// closeMargin how much longer a canceled command's session is kept for
// the helper's answer before the machine closes it.
var (
	killGrace   = 5 * time.Second
	closeMargin = 10 * time.Second
)

// maxSession is the longest a command's session may be open: Cella's
// bound on one command.
const maxSession = time.Hour

// sessionTimeout is Cella's bound on a command's session. It lies past
// the command's own timeout and the grace, so the helper's timeout
// decides and Cella's is the backstop for a runner that went away; a
// command with no timeout, or one longer than Cella allows, is bounded
// at Cella's hour.
func sessionTimeout(t time.Duration) string {
	if t <= 0 || t+killGrace+closeMargin > maxSession {
		return maxSession.String()
	}
	return (t + killGrace + closeMargin).String()
}

// invoke runs the helper through the shell, so a helper that is missing
// answers exit 127 the same way on every driver.
func (m *Machine) invoke(args ...string) []string {
	return append([]string{shell, "-c", `exec "$0" "$@"`, m.helper()}, args...)
}

// dir resolves a command's starting directory: the request's, relative
// to the working directory, or the working directory.
func (m *Machine) dir(d string) string {
	if d == "" {
		return m.Info().Workdir
	}
	return m.abs(d)
}

// abs makes p absolute against the working directory and cleans it.
func (m *Machine) abs(p string) string {
	if !path.IsAbs(p) {
		p = path.Join(m.Info().Workdir, p)
	}
	return path.Clean(p)
}

// Exec runs a command and returns its output, or starts a background job.
func (m *Machine) Exec(ctx context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	if r.Background {
		return m.background(ctx, r)
	}
	st, err := m.start(ctx, r)
	if err != nil {
		return machine.ExecResult{}, err
	}
	out, rerr := readAll(st, MaxOutput)
	res, err := st.Wait()
	if err != nil {
		return machine.ExecResult{}, err
	}
	if rerr != nil {
		return machine.ExecResult{}, fmt.Errorf("machine: read the output: %w", rerr)
	}
	res.Output = out
	return res, nil
}

// ExecStream runs a command whose output the caller reads as the helper
// sends it, frame by frame through Cella's exec session.
func (m *Machine) ExecStream(ctx context.Context, r machine.ExecRequest) (machine.ExecStream, error) {
	if r.Background {
		return nil, errors.New("machine: a background job has no stream; its output is in its job log")
	}
	return m.start(ctx, r)
}

// start opens an exec session running the helper's run mode and waits for
// the helper to say the command started.
func (m *Machine) start(ctx context.Context, r machine.ExecRequest) (*stream, error) {
	dir := m.dir(r.Dir)
	args := []string{"run", "-dir", dir}
	if r.Timeout > 0 {
		args = append(args, "-timeout", r.Timeout.String())
	}
	if r.ReportDir {
		args = append(args, "-report-dir")
	}
	args = append(args, "--", r.Command)
	var st *stream
	err := m.call(ctx, true, func(id string) error {
		sess, err := m.session(ctx, id, client.ExecRequest{
			Command: m.invoke(args...), Env: r.Env, Timeout: sessionTimeout(r.Timeout),
		})
		if err != nil {
			return err
		}
		if err := opened(sess, "chdir", dir); err != nil {
			return err
		}
		st = &stream{sess: sess, w: &sender{s: sess}, done: make(chan struct{}), inputDone: make(chan error, 1),
			check: func(err error) error { return m.check(context.WithoutCancel(ctx), id, err) }}
		return nil
	})
	if err != nil {
		return nil, err
	}
	go st.input(r.Stdin)
	go st.watch(ctx)
	return st, nil
}

// session opens a command's exec session. The machine cancels a command
// itself, with a kill frame the helper answers, so the session's own
// context never ends it; the dial still ends with ctx, and a session that
// opens after it is closed.
func (m *Machine) session(ctx context.Context, id string, req client.ExecRequest) (*client.Session, error) {
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	type dialed struct {
		s   *client.Session
		err error
	}
	ch := make(chan dialed, 1)
	go func() {
		s, err := m.c.ExecSession(context.WithoutCancel(ctx), id, req)
		ch <- dialed{s, err}
	}()
	select {
	case d := <-ch:
		return d.s, d.err
	case <-ctx.Done():
		go func() {
			if d := <-ch; d.s != nil {
				// Nobody is left to report the close of a session
				// nobody asked for any more.
				_ = closeSession(d.s)
			}
		}()
		return nil, context.Cause(ctx)
	}
}

// opened reads the helper's first frame: the start, or a failure that is
// the operation op's on the path p. Anything else is output that is not
// the helper's, a shell saying the helper is missing, and errNoHelper.
func opened(sess *client.Session, op, p string) error {
	var h [5]byte
	if _, err := io.ReadFull(sess, h[:]); err != nil {
		return errors.Join(noHelper(sess, h[:0], err), closeSession(sess))
	}
	n := binary.BigEndian.Uint32(h[1:])
	switch {
	case h[0] == frameStart && n == 0:
		return nil
	case h[0] == frameFail && n <= 1<<16:
		payload := make([]byte, n)
		if _, err := io.ReadFull(sess, payload); err != nil {
			return errors.Join(noHelper(sess, h[:], err), closeSession(sess))
		}
		var e errorBody
		if json.Unmarshal(payload, &e) == nil && e.Kind != "" {
			return errors.Join(e.err(op, p), closeSession(sess))
		}
		return errors.Join(fmt.Errorf("%w: %q", errNoHelper, truncate(append(h[:], payload...))), closeSession(sess))
	}
	return errors.Join(noHelper(sess, h[:], nil), closeSession(sess))
}

// noHelper names a session that did not begin with a helper frame. A
// refusal the socket carried is Cella's and is returned as it is.
func noHelper(sess *client.Session, head []byte, err error) error {
	var ce *client.Error
	if errors.As(err, &ce) {
		return err
	}
	rest, rerr := readAll(sess, 512)
	if rerr != nil && !errors.As(rerr, &ce) {
		rerr = nil
	}
	if rerr != nil {
		return rerr
	}
	return fmt.Errorf("%w: %q", errNoHelper, truncate(append(head, rest...)))
}

// stream is one command's exec session, decoded from the helper's frames.
type stream struct {
	sess  *client.Session
	w     *sender
	done  chan struct{}
	check func(error) error

	mu       sync.Mutex
	pending  []byte
	res      machine.ExecResult
	err      error
	finished bool

	// inputDone carries the input pump's error once it has sent the
	// input's end.
	inputDone chan error

	forceMu sync.Mutex
	forced  bool

	closeOnce sync.Once
	closeErr  error

	waitOnce sync.Once
	inputErr error
}

// input sends the command's standard input and its end.
func (st *stream) input(r io.Reader) { st.inputDone <- st.w.input(r) }

// watch cancels the command when the context ends: the helper sends
// SIGTERM to its process group, SIGKILL after its grace, and answers with
// the exit. A helper that does not answer has its session closed.
func (st *stream) watch(ctx context.Context) {
	select {
	case <-st.done:
		return
	case <-ctx.Done():
	}
	if err := st.w.frame(frameKill, nil); err != nil {
		st.force()
		return
	}
	t := time.NewTimer(killGrace + closeMargin)
	defer t.Stop()
	select {
	case <-st.done:
	case <-t.C:
		st.force()
	}
}

// force closes the session of a canceled command; its end reads as a
// cancellation.
func (st *stream) force() {
	st.forceMu.Lock()
	st.forced = true
	st.forceMu.Unlock()
	// The session is torn down on purpose, so how its close went says
	// nothing about the command; the end reads the command as canceled.
	_ = st.close()
}

func (st *stream) close() error {
	st.closeOnce.Do(func() { st.closeErr = closeSession(st.sess) })
	return st.closeErr
}

func (st *stream) Read(p []byte) (int, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for len(st.pending) == 0 {
		if st.finished {
			if st.err != nil {
				return 0, st.err
			}
			return 0, io.EOF
		}
		st.next()
	}
	n := copy(p, st.pending)
	st.pending = st.pending[n:]
	return n, nil
}

// next reads one frame. The caller holds mu.
func (st *stream) next() {
	kind, payload, err := readFrame(st.sess)
	if err != nil {
		st.end(err)
		return
	}
	switch kind {
	case frameOutput:
		st.pending = payload
	case frameDir:
		st.res.Dir = string(payload)
	case frameExit:
		var e exitBody
		if err := json.Unmarshal(payload, &e); err != nil {
			st.end(fmt.Errorf("machine: the helper's exit %q: %w", payload, err))
			return
		}
		st.res.ExitCode, st.res.TimedOut, st.res.Canceled = e.Code, e.TimedOut, e.Canceled
		st.end(nil)
	case frameFail:
		var e errorBody
		if err := json.Unmarshal(payload, &e); err != nil {
			st.end(fmt.Errorf("machine: the helper's failure %q: %w", payload, err))
			return
		}
		st.end(e.err("run", ""))
	default:
		st.end(fmt.Errorf("machine: the helper sent a frame of kind %q", kind))
	}
}

// end finishes the stream with err, closes the session, and releases
// Wait. The caller holds mu.
func (st *stream) end(err error) {
	st.forceMu.Lock()
	forced := st.forced
	st.forceMu.Unlock()
	if err != nil && !forced && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		// The session ended with no exit from the helper: the helper
		// was killed, and the session's exit code says how.
		code, werr := st.sess.Wait()
		err = errors.Join(fmt.Errorf("machine: the command's session ended before its exit (exit code %d)", code), werr)
	}
	cerr := st.close()
	switch {
	case forced:
		// The machine closed the session of a canceled command whose
		// helper did not answer: the command was canceled.
		st.res.Canceled, st.res.ExitCode = true, -1
		err = nil
	case err == nil:
		err = cerr
	}
	if err != nil {
		err = st.check(err)
	}
	st.finished, st.err = true, err
	close(st.done)
}

// Wait returns the result once the command ends and its input has been
// sent, as exec.Cmd.Wait waits for a command's input; output nobody read
// is discarded, as the stream carried it.
func (st *stream) Wait() (machine.ExecResult, error) {
	_, err := io.Copy(io.Discard, st)
	<-st.done
	st.waitOnce.Do(func() { st.inputErr = <-st.inputDone })
	st.mu.Lock()
	defer st.mu.Unlock()
	if err != nil {
		return st.res, err
	}
	return st.res, st.inputErr
}

// background starts a detached job through the helper's job mode, with
// its output in a job log in the spill directory, and returns at once.
func (m *Machine) background(ctx context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	dir := m.dir(r.Dir)
	resp, err := m.runHelper(ctx, r.Env, "job", "-jobs", path.Join(m.SpillDir(), "jobs"), "-dir", dir, "--", r.Command)
	if err != nil {
		return machine.ExecResult{}, err
	}
	if resp.Error != nil {
		return machine.ExecResult{}, resp.Error.err("chdir", dir)
	}
	if resp.Job == nil {
		return machine.ExecResult{}, errors.New("machine: the helper started no job")
	}
	return machine.ExecResult{PID: resp.Job.PID, Log: resp.Job.Log}, nil
}

// runHelper runs a helper mode whose answer is small through Cella's
// synchronous exec route and decodes the answer.
func (m *Machine) runHelper(ctx context.Context, env map[string]string, args ...string) (response, error) {
	var res client.ExecResult
	err := m.call(ctx, true, func(id string) error {
		var err error
		res, _, err = m.c.Exec(ctx, id, client.ExecRequest{Command: m.invoke(args...), Env: env, Timeout: "1m"})
		if err == nil && strings.TrimSpace(res.Stdout) == "" && res.ExitCode != 0 {
			return fmt.Errorf("%w: exit %d: %s", errNoHelper, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		return err
	})
	if err != nil {
		return response{}, err
	}
	return decodeResponse([]byte(res.Stdout))
}
