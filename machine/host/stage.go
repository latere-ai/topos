// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/hostsandbox"

	"latere.ai/x/topos/machine"
)

// Sandbox runs every command of a host machine as one stage of a host
// sandbox driver, which is how a server runs sessions on its own host:
// that host is shared by every principal the installation serves, so a
// command reaches no path outside the session's roots and the system
// directories, none of the server's environment, and no host outside
// the agent's egress (spec 009).
type Sandbox struct {
	// Driver runs the stages.
	Driver hostsandbox.Sandbox
	// StageDir holds one directory per command with its log, the
	// driver's settings and its exit status. It lies outside every root,
	// so a command neither reads nor rewrites its own transcript.
	StageDir string
	// Denied are paths no command may read though nothing else denies
	// them: the data directory, whose roots are granted back, and the
	// files the server's configuration names. srt receives them as
	// written and matches them itself; the case-insensitive match of
	// machine.DenyList covers the file tools and search, not commands.
	Denied []string
	// Egress are the hosts commands and fetches may reach, each a host
	// name or "*." and a domain; empty reaches none.
	Egress []string
}

// openSandbox checks a sandbox's driver and creates its stage
// directory, which must lie outside every root, since a command could
// otherwise read or rewrite its own transcript, and records the
// driver's name.
func (h *Host) openSandbox() error {
	sb := h.opts.Sandbox
	switch {
	case sb.Driver == nil:
		return errors.New("machine: the host sandbox has no driver")
	case sb.StageDir == "":
		return errors.New("machine: the host sandbox has no stage directory")
	}
	if err := os.MkdirAll(sb.StageDir, 0o700); err != nil {
		return fmt.Errorf("machine: create the stage directory: %w", err)
	}
	dir, err := filepath.EvalSymlinks(sb.StageDir)
	if err != nil {
		return fmt.Errorf("machine: the stage directory: %w", err)
	}
	for _, r := range h.roots {
		if dir == r.dir || strings.HasPrefix(dir, r.dir+string(filepath.Separator)) || strings.HasPrefix(r.dir, dir+string(filepath.Separator)) {
			return fmt.Errorf("machine: the stage directory %s overlaps the root %s", dir, r.dir)
		}
	}
	h.info.Sandbox = string(sb.Driver.Name())
	return nil
}

// srtTempDir is the variable srt reads for the TMPDIR it hands a stage.
// Unset, every stage on the host gets /tmp/claude, one directory all of
// them may write and read; set, a session's temporary files stay in its
// own spill directory.
const srtTempDir = "CLAUDE_CODE_TMPDIR"

// stagePoll is how often a running stage's log is read for new output
// and its process group checked; stageObserve bounds how long a stage
// waits between observations, each of which the driver answers with a
// ps call while the stage runs.
var (
	stagePoll    = 20 * time.Millisecond
	stageObserve = 100 * time.Millisecond
)

// stagePID is the pid a job reports: the srt driver's handle carries it
// in the form hostsandbox documents, "pid:<pid>@<start>:<log>". It is for
// display only; the driver ends a stage's processes itself. A handle of
// another driver names no pid, and 0 is returned.
func stagePID(h hostsandbox.StageHandle) int {
	if h.Driver != hostsandbox.Host {
		return 0
	}
	rest, ok := strings.CutPrefix(h.ID, "pid:")
	if !ok {
		return 0
	}
	text, _, ok := strings.Cut(rest, "@")
	if !ok {
		return 0
	}
	pid, err := strconv.Atoi(text)
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// stageNetwork is the egress policy of a stage: the allowlist of the
// sandbox's hosts, or none.
func stageNetwork(egress []string) hostsandbox.Network {
	if len(egress) == 0 {
		return hostsandbox.Network{Mode: hostsandbox.NetworkNone}
	}
	return hostsandbox.Network{Mode: hostsandbox.NetworkAllowlist, Domains: egress}
}

// launched is a stage the machine started and the directory it keeps
// the stage's files in.
type launched struct {
	handle hostsandbox.StageHandle
	dir    string
}

// launch starts argv as a stage in dir, relative to the working
// directory, with env added to HOME set to the working directory. Every
// root is readable and writable, the sandbox's denied paths are not
// readable, and the network is the sandbox's egress.
func (h *Host) launch(ctx context.Context, argv []string, dir string, env map[string]string) (launched, error) {
	sb := h.opts.Sandbox
	stageDir, err := os.MkdirTemp(sb.StageDir, "cmd-*")
	if err != nil {
		return launched{}, fmt.Errorf("machine: create the stage directory: %w", err)
	}
	tmp := filepath.Join(h.spill, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return launched{}, errors.Join(fmt.Errorf("machine: create the temporary directory: %w", err), os.RemoveAll(stageDir))
	}
	stageEnv := map[string]string{"HOME": h.info.Workdir, srtTempDir: tmp}
	maps.Copy(stageEnv, env)
	paths := make([]hostsandbox.Path, len(h.roots))
	for i, r := range h.roots {
		paths[i] = hostsandbox.Path{Host: r.dir, Access: hostsandbox.ReadWrite}
	}
	spec := hostsandbox.StageSpec{
		Name: filepath.Base(stageDir), Argv: argv, Env: stageEnv, Workdir: h.dir(dir),
		Paths: paths, Denied: sb.Denied, Network: stageNetwork(sb.Egress),
		LogPath: filepath.Join(stageDir, "out.log"),
	}
	// A stage outlives the call that started it until the machine stops
	// it, so the call's context never ends it.
	handle, err := sb.Driver.Launch(context.WithoutCancel(ctx), spec)
	if err != nil {
		return launched{}, errors.Join(fmt.Errorf("machine: start the command: %w", err), os.RemoveAll(stageDir))
	}
	return launched{handle: handle, dir: stageDir}, nil
}

// finish lets the driver discard a stage that has exited, which ends
// what it left running in its process group, as the host does once its
// shell exits, and removes the stage's directory.
func (h *Host) finish(ctx context.Context, l launched) error {
	var errs []error
	if err := h.opts.Sandbox.Driver.Discard(context.WithoutCancel(ctx), l.handle); err != nil {
		errs = append(errs, fmt.Errorf("machine: discard the command: %w", err))
	}
	if err := os.RemoveAll(l.dir); err != nil {
		errs = append(errs, fmt.Errorf("machine: remove the stage directory: %w", err))
	}
	return errors.Join(errs...)
}

// stop stops a running stage through its driver, which sends SIGTERM to
// its process group, returns once the group is empty, and sends SIGKILL
// once grace has passed.
func (h *Host) stop(ctx context.Context, l launched, grace time.Duration) error {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	err := h.opts.Sandbox.Driver.Stop(sctx, l.handle)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("machine: stop the command: %w", err)
	}
	return nil
}

// stageExit is the exit code of a stage that has ended: its own, or -1,
// as for a process a signal ended, when the driver holds no status,
// which is how a stage the machine stopped ends.
func stageExit(s hostsandbox.StageStatus) int {
	if s.Gone {
		return -1
	}
	return s.ExitCode
}

// waitStage waits for a stage to end. The timeout stops it at once and
// the context with KillGrace between SIGTERM and SIGKILL, as on the
// host; either way the stage is observed until it has ended.
func (h *Host) waitStage(ctx context.Context, l launched, timeout time.Duration) (machine.ExecResult, error) {
	var res machine.ExecResult
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	canceled := ctx.Done()
	interval := stagePoll
	for {
		s, err := h.opts.Sandbox.Driver.Observe(context.WithoutCancel(ctx), l.handle)
		if err != nil {
			return res, errors.Join(fmt.Errorf("machine: observe the command: %w", err), h.stop(ctx, l, 0))
		}
		if !s.Running {
			res.ExitCode = stageExit(s)
			return res, nil
		}
		next := time.NewTimer(interval)
		select {
		case <-next.C:
			interval = min(2*interval, stageObserve)
		case <-timer:
			next.Stop()
			res.TimedOut, timer = true, nil
			if err := h.stop(ctx, l, 0); err != nil {
				return res, err
			}
		case <-canceled:
			next.Stop()
			res.Canceled, canceled = true, nil
			if err := h.stop(ctx, l, KillGrace); err != nil {
				return res, err
			}
		}
	}
}

// stageStream is a sandboxed command whose output is read from its
// stage log as the command writes it.
type stageStream struct {
	out    io.ReadCloser
	exited chan struct{}
	done   chan struct{}
	res    machine.ExecResult
	err    error
}

// Read returns what the log holds past what was read. At the end of the
// log it waits for more until the stage has ended; the log then holds
// all it ever will, and one more read drains what was written between
// the last read and the end.
func (s *stageStream) Read(p []byte) (int, error) {
	t := time.NewTicker(stagePoll)
	defer t.Stop()
	for {
		n, err := s.out.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		select {
		case <-s.exited:
			n, err := s.out.Read(p)
			if n > 0 {
				return n, nil
			}
			if err == nil || errors.Is(err, io.EOF) {
				return 0, io.EOF
			}
			return 0, err
		case <-t.C:
		}
	}
}

func (s *stageStream) Wait() (machine.ExecResult, error) {
	<-s.done
	return s.res, errors.Join(s.err, s.out.Close())
}

// startStage runs a command as a stage. A stage's input is the null
// device, so a command's input is written to a file in the spill
// directory first and the shell reads it from there; its final
// directory, which the host reads from descriptor 3, is written by the
// shell's exit trap to another.
func (h *Host) startStage(ctx context.Context, r machine.ExecRequest) (*stageStream, error) {
	var removes []func() error
	cleanup := func() error {
		var errs []error
		for _, rm := range removes {
			errs = append(errs, rm())
		}
		return errors.Join(errs...)
	}
	script := r.Command
	var dirFile string
	if r.ReportDir {
		name, remove, err := h.spillFile("topos-dir-*", nil)
		if err != nil {
			return nil, err
		}
		dirFile, removes = name, append(removes, remove)
		script = "trap " + hostsandbox.ShellQuote("pwd -P >"+hostsandbox.ShellQuote(dirFile)) + " EXIT\n" + script
	}
	args, remove, err := machine.ShellArgs(h.spill, script)
	if err != nil {
		return nil, errors.Join(err, cleanup())
	}
	removes = append(removes, remove)
	argv := append([]string{Shell}, args...)
	if r.Stdin != nil {
		name, remove, err := h.spillFile("topos-stdin-*", r.Stdin)
		if err != nil {
			return nil, errors.Join(err, cleanup())
		}
		removes = append(removes, remove)
		argv = append([]string{Shell, "-c", "exec <" + hostsandbox.ShellQuote(name) + " && exec " + Shell + ` "$@"`, "sh"}, args...)
	}
	l, err := h.launch(ctx, argv, r.Dir, r.Env)
	if err != nil {
		return nil, errors.Join(err, cleanup())
	}
	out, err := h.opts.Sandbox.Driver.Output(context.WithoutCancel(ctx), l.handle, 0)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("machine: read the output: %w", err), h.stop(ctx, l, 0), h.finish(ctx, l), cleanup())
	}
	s := &stageStream{out: out, exited: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		res, err := h.waitStage(ctx, l, r.Timeout)
		err = errors.Join(err, h.finish(ctx, l))
		close(s.exited)
		if dirFile != "" {
			b, rerr := os.ReadFile(dirFile)
			if rerr != nil {
				err = errors.Join(err, fmt.Errorf("machine: read the final directory: %w", rerr))
			}
			res.Dir = strings.TrimSpace(string(b))
		}
		s.res, s.err = res, errors.Join(err, cleanup())
	}()
	return s, nil
}

// stageJob is a background job running as a stage.
type stageJob struct {
	l    launched
	done chan struct{}
}

// stageBackground starts a background job as a stage. The machine
// copies the stage's log into the job log in the spill directory as the
// job writes it, and appends the host's exit line when the job ends, so
// the job's output is where the host's would be while the stage's own
// files stay out of every root.
func (h *Host) stageBackground(ctx context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return machine.ExecResult{}, machine.ErrReleased
	}
	jobs := filepath.Join(h.spill, "jobs")
	if err := os.MkdirAll(jobs, 0o700); err != nil {
		return machine.ExecResult{}, fmt.Errorf("machine: create the job directory: %w", err)
	}
	log, err := os.CreateTemp(jobs, "job-*.log")
	if err != nil {
		return machine.ExecResult{}, fmt.Errorf("machine: create the job log: %w", err)
	}
	args, remove, err := machine.ShellArgs(jobs, r.Command)
	if err != nil {
		return machine.ExecResult{}, errors.Join(err, log.Close())
	}
	l, err := h.launch(ctx, append([]string{Shell}, args...), r.Dir, r.Env)
	if err != nil {
		return machine.ExecResult{}, errors.Join(err, log.Close(), remove())
	}
	out, err := h.opts.Sandbox.Driver.Output(context.WithoutCancel(ctx), l.handle, 0)
	if err != nil {
		return machine.ExecResult{}, errors.Join(fmt.Errorf("machine: read the job's output: %w", err), h.stop(ctx, l, 0), h.finish(ctx, l), log.Close(), remove())
	}
	// The job's handle is kept in its stage directory, so the end of a
	// session whose later turns opened another machine still finds it.
	b, err := json.Marshal(l.handle)
	if err == nil {
		err = os.WriteFile(filepath.Join(l.dir, jobHandle), b, 0o600)
	}
	if err != nil {
		return machine.ExecResult{}, errors.Join(fmt.Errorf("machine: keep the job's handle: %w", err), h.stop(ctx, l, 0), h.finish(ctx, l), out.Close(), log.Close(), remove())
	}
	pid := stagePID(l.handle)
	job := &stageJob{l: l, done: make(chan struct{})}
	h.stages[l.handle.ID] = job
	go func() {
		defer close(job.done)
		err := h.follow(ctx, l, out, log)
		err = errors.Join(err, remove())
		h.mu.Lock()
		delete(h.stages, l.handle.ID)
		if err != nil {
			h.jobErrs = append(h.jobErrs, fmt.Errorf("machine: job %d: %w", pid, err))
		}
		h.mu.Unlock()
	}()
	return machine.ExecResult{PID: pid, Log: log.Name()}, nil
}

// follow copies a job's stage log into its job log until the job ends,
// then appends the exit line and closes both.
func (h *Host) follow(ctx context.Context, l launched, out io.ReadCloser, log *os.File) error {
	bg := context.WithoutCancel(ctx)
	var errs []error
	code := -1
	for {
		if _, err := io.Copy(log, out); err != nil {
			errs = append(errs, fmt.Errorf("copy the output: %w", err))
			break
		}
		s, err := h.opts.Sandbox.Driver.Observe(bg, l.handle)
		if err != nil {
			errs = append(errs, fmt.Errorf("observe: %w", err), h.stop(bg, l, 0))
			break
		}
		if !s.Running {
			code = stageExit(s)
			if _, err := io.Copy(log, out); err != nil {
				errs = append(errs, fmt.Errorf("copy the output: %w", err))
			}
			break
		}
		time.Sleep(stageObserve)
	}
	errs = append(errs, h.finish(bg, l))
	if _, err := fmt.Fprintf(log, "\n[job %d exited with code %d]\n", stagePID(l.handle), code); err != nil {
		errs = append(errs, fmt.Errorf("write the exit line: %w", err))
	}
	return errors.Join(append(errs, log.Close(), out.Close())...)
}

// jobHandle is the file in a job's stage directory that holds its
// handle while the job runs.
const jobHandle = "handle.json"

// StopJobs stops every background job whose stage directory is still
// under stageDir, each with KillGrace between SIGTERM and SIGKILL, and
// removes the directories. A machine stops its own jobs at the session's
// end; this reaches the jobs of machines the session opened before, one
// per drive of a server's runner, which the last machine never held.
func StopJobs(ctx context.Context, driver hostsandbox.Sandbox, stageDir string) error {
	paths, err := filepath.Glob(filepath.Join(stageDir, "*", jobHandle))
	if err != nil {
		return err
	}
	h := &Host{opts: Options{Sandbox: &Sandbox{Driver: driver}}}
	errs := make(chan error, len(paths))
	for _, p := range paths {
		go func() {
			b, err := os.ReadFile(p)
			if errors.Is(err, fs.ErrNotExist) {
				errs <- nil
				return
			}
			var handle hostsandbox.StageHandle
			if err == nil {
				err = json.Unmarshal(b, &handle)
			}
			if err != nil {
				errs <- fmt.Errorf("machine: read the job's handle %s: %w", p, err)
				return
			}
			l := launched{handle: handle, dir: filepath.Dir(p)}
			errs <- errors.Join(h.stop(ctx, l, KillGrace), h.finish(ctx, l))
		}()
	}
	var all []error
	for range paths {
		all = append(all, <-errs)
	}
	return errors.Join(all...)
}

// releaseStages stops every running job at the session's end, each with
// KillGrace between SIGTERM and SIGKILL, and waits until each has
// written its exit line.
func (h *Host) releaseStages(ctx context.Context) error {
	h.mu.Lock()
	jobs := make([]*stageJob, 0, len(h.stages))
	for _, j := range h.stages {
		jobs = append(jobs, j)
	}
	h.mu.Unlock()
	errs := make(chan error, len(jobs))
	for _, j := range jobs {
		go func() { errs <- h.stop(ctx, j.l, KillGrace) }()
	}
	var all []error
	for range jobs {
		all = append(all, <-errs)
	}
	for _, j := range jobs {
		<-j.done
	}
	return errors.Join(all...)
}
