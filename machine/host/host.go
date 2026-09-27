// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package host is the host machine of spec 009: the person's own
// computer, with the file tools confined to the session's roots through
// one os.Root each, the credential deny-list enforced on every path,
// and each command in its own process group.
package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"latere.ai/x/topos/machine"
)

// Shell runs every command.
const Shell = "/bin/sh"

// MaxOutput bounds what Exec keeps of one command's output; the tool
// layer spills anything past its own cap.
const MaxOutput = 64 << 20

// KillGrace is how long a canceled command has between SIGTERM and
// SIGKILL.
var KillGrace = 5 * time.Second

// Options configure a host machine.
type Options struct {
	// Workdir is the session's working directory.
	Workdir string
	// Roots are further directories the file tools may reach: memory
	// store directories and the agent's extra roots.
	Roots []string
	// SpillDir holds spill files and job logs, outside the working
	// directory.
	SpillDir string
	// Home and DataDir place the credential deny-list.
	Home    string
	DataDir string
	// Environ is the commands' environment before the deny-list; nil is
	// this process's.
	Environ []string
	// ID names the machine in session.machine.
	ID string
	// WorktreeDir holds the git worktrees of isolated threads; empty
	// offers none.
	WorktreeDir string
	// Sandbox, when set, runs every command inside a host sandbox, one
	// stage per command, with none of Environ; nil runs commands as the
	// person's own processes.
	Sandbox *Sandbox
}

// Host is the host machine.
type Host struct {
	opts  Options
	info  machine.Info
	roots []root
	spill string
	deny  machine.DenyList
	env   []string

	mu       sync.Mutex
	jobs     map[int]*exec.Cmd
	stages   map[string]*stageJob
	jobErrs  []error
	released bool
}

type root struct {
	dir string
	r   *os.Root
}

// Open resolves the working directory and the roots, with symlinks
// evaluated, and opens each.
func Open(o Options) (*Host, error) {
	if o.Workdir == "" {
		return nil, errors.New("machine: the host machine needs a working directory")
	}
	if o.SpillDir == "" {
		return nil, errors.New("machine: the host machine needs a spill directory")
	}
	if err := os.MkdirAll(o.SpillDir, 0o700); err != nil {
		return nil, fmt.Errorf("machine: create the spill directory: %w", err)
	}
	h := &Host{opts: o, deny: machine.DenyList{Home: o.Home, DataDir: o.DataDir}, jobs: map[int]*exec.Cmd{}, stages: map[string]*stageJob{}}
	dirs := append([]string{o.Workdir}, o.Roots...)
	dirs = append(dirs, o.SpillDir)
	for i, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("machine: root %s: %w", d, err), h.closeRoots())
		}
		abs, err = filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("machine: root %s: %w", d, err), h.closeRoots())
		}
		if slices.ContainsFunc(h.roots, func(r root) bool { return r.dir == abs }) {
			continue
		}
		r, err := os.OpenRoot(abs)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("machine: root %s: %w", d, err), h.closeRoots())
		}
		h.roots = append(h.roots, root{dir: abs, r: r})
		switch i {
		case 0:
			h.info = machine.Info{Kind: machine.KindHost, ID: o.ID, Workdir: abs, OS: runtime.GOOS, Arch: runtime.GOARCH}
		case len(dirs) - 1:
			h.spill = abs
		}
	}
	if h.spill == "" {
		h.spill = h.info.Workdir
	}
	if o.Sandbox != nil {
		if err := h.checkSandbox(); err != nil {
			return nil, errors.Join(err, h.closeRoots())
		}
		h.info.Sandbox = string(o.Sandbox.Driver.Name())
	}
	environ := o.Environ
	if environ == nil {
		environ = os.Environ()
	}
	h.env = machine.FilterEnv(environ)
	return h, nil
}

// checkSandbox checks a sandbox's driver and creates its stage
// directory, which must lie outside every root, since a command could
// otherwise read or rewrite its own transcript.
func (h *Host) checkSandbox() error {
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
	return nil
}

func (h *Host) closeRoots() error {
	var errs []error
	for _, r := range h.roots {
		errs = append(errs, r.r.Close())
	}
	h.roots = nil
	return errors.Join(errs...)
}

func (h *Host) Info() machine.Info { return h.info }

func (h *Host) Roots() []string {
	out := make([]string, len(h.roots))
	for i, r := range h.roots {
		out[i] = r.dir
	}
	return out
}

func (h *Host) SpillDir() string { return h.spill }

// abs makes p absolute against the working directory and cleans it.
func (h *Host) abs(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(h.info.Workdir, p)
	}
	return filepath.Clean(p)
}

// resolve finds the root a path is in, the deepest one first, and checks
// the deny-list.
func (h *Host) resolve(p string) (root, string, string, error) {
	h.mu.Lock()
	released := h.released
	h.mu.Unlock()
	if released {
		return root{}, "", "", machine.ErrReleased
	}
	a := h.abs(p)
	if h.deny.Path(a) {
		return root{}, "", a, fmt.Errorf("%w: %s", machine.ErrDenied, a)
	}
	best := -1
	for i, r := range h.roots {
		if a == r.dir || strings.HasPrefix(a, r.dir+string(filepath.Separator)) {
			if best < 0 || len(r.dir) > len(h.roots[best].dir) {
				best = i
			}
		}
	}
	if best < 0 {
		return root{}, "", a, fmt.Errorf("%w: %s", machine.ErrOutside, a)
	}
	r := h.roots[best]
	rel, err := filepath.Rel(r.dir, a)
	if err != nil {
		return root{}, "", a, fmt.Errorf("%w: %s", machine.ErrOutside, a)
	}
	return r, filepath.ToSlash(rel), a, nil
}

// confine turns an os.Root refusal (a symlink or ".." leaving the root)
// into ErrOutside.
func confine(err error, a string) error {
	if err == nil {
		return nil
	}
	var pe *os.PathError
	if errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "escapes") {
		return fmt.Errorf("%w: %s", machine.ErrOutside, a)
	}
	return err
}

func (h *Host) ReadFile(ctx context.Context, p string) (io.ReadCloser, error) {
	r, rel, a, err := h.resolve(p)
	if err != nil {
		return nil, err
	}
	f, err := r.r.Open(rel)
	if err != nil {
		return nil, confine(err, a)
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if fi.IsDir() {
		return nil, errors.Join(fmt.Errorf("machine: %s is a directory", a), f.Close())
	}
	return f, nil
}

// WriteFile writes through a temporary file in the same directory and
// renames it over the path, creating parent directories.
func (h *Host) WriteFile(ctx context.Context, p string, src io.Reader, mode fs.FileMode) error {
	r, rel, a, err := h.resolve(p)
	if err != nil {
		return err
	}
	if rel == "." {
		return fmt.Errorf("machine: %s is a directory", a)
	}
	if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." {
		if err := r.r.MkdirAll(dir, 0o755); err != nil {
			return confine(err, a)
		}
	}
	if mode == 0 {
		mode = 0o644
		if fi, err := r.r.Stat(rel); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	tmp := rel + ".topos-" + fmt.Sprint(time.Now().UnixNano()) + ".tmp"
	f, err := r.r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return confine(err, a)
	}
	_, werr := io.Copy(f, src)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return errors.Join(fmt.Errorf("machine: write %s: %w", a, err), r.r.Remove(tmp))
	}
	if err := r.r.Rename(tmp, rel); err != nil {
		return errors.Join(confine(err, a), r.r.Remove(tmp))
	}
	return nil
}

func info(a string, fi fs.FileInfo) machine.FileInfo {
	return machine.FileInfo{Path: a, Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime(), IsDir: fi.IsDir()}
}

func (h *Host) Stat(ctx context.Context, p string) (machine.FileInfo, error) {
	r, rel, a, err := h.resolve(p)
	if err != nil {
		return machine.FileInfo{}, err
	}
	fi, err := r.r.Stat(rel)
	if err != nil {
		return machine.FileInfo{}, confine(err, a)
	}
	return info(a, fi), nil
}

func (h *Host) List(ctx context.Context, p string) ([]machine.FileInfo, error) {
	r, rel, a, err := h.resolve(p)
	if err != nil {
		return nil, err
	}
	f, err := r.r.Open(rel)
	if err != nil {
		return nil, confine(err, a)
	}
	entries, rerr := f.ReadDir(-1)
	if err := errors.Join(rerr, f.Close()); err != nil {
		return nil, fmt.Errorf("machine: list %s: %w", a, err)
	}
	out := make([]machine.FileInfo, 0, len(entries))
	for _, e := range entries {
		ea := filepath.Join(a, e.Name())
		if h.deny.Path(ea) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, info(ea, fi))
	}
	return out, nil
}

func (h *Host) Remove(ctx context.Context, p string) error {
	r, rel, a, err := h.resolve(p)
	if err != nil {
		return err
	}
	if rel == "." {
		return fmt.Errorf("machine: %s is a root and is not removed", a)
	}
	return confine(r.r.Remove(rel), a)
}

func (h *Host) Rename(ctx context.Context, from, to string) error {
	rf, relFrom, af, err := h.resolve(from)
	if err != nil {
		return err
	}
	rt, relTo, at, err := h.resolve(to)
	if err != nil {
		return err
	}
	if rf.dir != rt.dir {
		return fmt.Errorf("machine: %s and %s are in different roots", af, at)
	}
	return confine(rf.r.Rename(relFrom, relTo), af)
}

func (h *Host) Search(ctx context.Context, q machine.SearchRequest) (machine.SearchResult, error) {
	p := q.Path
	if p == "" {
		p = h.info.Workdir
	}
	r, rel, _, err := h.resolve(p)
	if err != nil {
		return machine.SearchResult{}, err
	}
	fsys := denyFS{FS: r.r.FS(), base: r.dir, deny: h.deny}
	return machine.SearchFS(ctx, fsys, filepath.ToSlash(r.dir), rel, q)
}

// denyFS hides the deny-list's paths from a search.
type denyFS struct {
	fs.FS
	base string
	deny machine.DenyList
}

func (d denyFS) denied(name string) bool {
	return d.deny.Path(filepath.Join(d.base, filepath.FromSlash(name)))
}

func (d denyFS) Open(name string) (fs.File, error) {
	if d.denied(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return d.FS.Open(name)
}

func (d denyFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(d.FS, name)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(entries, func(e fs.DirEntry) bool {
		return d.denied(filepath.ToSlash(filepath.Join(name, e.Name())))
	}), nil
}

// dir resolves a command's starting directory: the request's, relative
// to the working directory, or the working directory.
func (h *Host) dir(d string) string {
	if d == "" {
		return h.info.Workdir
	}
	return h.abs(d)
}

func (h *Host) environ(add map[string]string) []string {
	env := slices.Clone(h.env)
	keys := make([]string, 0, len(add))
	for k := range add {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		env = append(env, k+"="+add[k])
	}
	return env
}

// Exec runs a command and returns its output, or starts a background job.
func (h *Host) Exec(ctx context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	if r.Background {
		if h.opts.Sandbox != nil {
			return h.stageBackground(ctx, r)
		}
		return h.background(ctx, r)
	}
	s, err := h.stream(ctx, r)
	if err != nil {
		return machine.ExecResult{}, err
	}
	var out bytes.Buffer
	_, rerr := io.Copy(&out, io.LimitReader(s, MaxOutput))
	if _, err := io.Copy(io.Discard, s); err != nil && rerr == nil {
		rerr = err
	}
	res, err := s.Wait()
	if err != nil {
		return machine.ExecResult{}, err
	}
	if rerr != nil {
		return machine.ExecResult{}, fmt.Errorf("machine: read the output: %w", rerr)
	}
	res.Output = out.Bytes()
	return res, nil
}

// ExecStream runs a command whose output the caller reads as it comes.
func (h *Host) ExecStream(ctx context.Context, r machine.ExecRequest) (machine.ExecStream, error) {
	if r.Background {
		return nil, errors.New("machine: a background job has no stream; its output is in its job log")
	}
	return h.stream(ctx, r)
}

// stream starts a command in the foreground: as a stage of the host
// sandbox when the machine has one, as a process of its own otherwise.
func (h *Host) stream(ctx context.Context, r machine.ExecRequest) (machine.ExecStream, error) {
	// Each start is checked before it becomes the interface, so a failed
	// start answers a nil stream rather than a typed nil.
	if h.opts.Sandbox == nil {
		s, err := h.start(ctx, r)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	h.mu.Lock()
	released := h.released
	h.mu.Unlock()
	if released {
		return nil, machine.ErrReleased
	}
	s, err := h.startStage(ctx, r)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// reportPrefix makes the shell write its final directory to descriptor 3
// on every exit, an explicit exit included.
const reportPrefix = "trap 'pwd -P >&3' EXIT\n"

type stream struct {
	out  *os.File
	done chan struct{}
	res  machine.ExecResult
	err  error
}

func (s *stream) Read(p []byte) (int, error) { return s.out.Read(p) }

func (s *stream) Wait() (machine.ExecResult, error) {
	<-s.done
	return s.res, errors.Join(s.err, s.out.Close())
}

func (h *Host) start(ctx context.Context, r machine.ExecRequest) (*stream, error) {
	h.mu.Lock()
	released := h.released
	h.mu.Unlock()
	if released {
		return nil, machine.ErrReleased
	}
	script := r.Command
	if r.ReportDir {
		script = reportPrefix + script
	}
	args, remove, err := machine.ShellArgs(h.spill, script)
	if err != nil {
		return nil, err
	}
	// The machine cancels a command itself, SIGTERM to its process group
	// and SIGKILL after KillGrace, so the command's own context never
	// kills it.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), Shell, args...)
	cmd.Dir = h.dir(r.Dir)
	cmd.Env = h.environ(r.Env)
	cmd.Stdin = r.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("machine: create the output pipe: %w", err), remove())
	}
	cmd.Stdout, cmd.Stderr = outW, outW
	var dirR, dirW *os.File
	if r.ReportDir {
		if dirR, dirW, err = os.Pipe(); err != nil {
			return nil, errors.Join(fmt.Errorf("machine: create the directory pipe: %w", err), outR.Close(), outW.Close(), remove())
		}
		cmd.ExtraFiles = []*os.File{dirW}
	}
	if err := cmd.Start(); err != nil {
		errs := []error{fmt.Errorf("machine: start the command: %w", err), outR.Close(), outW.Close(), remove()}
		if dirR != nil {
			errs = append(errs, dirR.Close(), dirW.Close())
		}
		return nil, errors.Join(errs...)
	}
	s := &stream{out: outR, done: make(chan struct{})}
	werr := outW.Close()
	var dirOut chan string
	if dirW != nil {
		werr = errors.Join(werr, dirW.Close())
		dirOut = make(chan string, 1)
		go func() {
			b, err := io.ReadAll(io.LimitReader(dirR, 1<<16))
			if cerr := dirR.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				dirOut <- ""
				return
			}
			dirOut <- strings.TrimSpace(string(b))
		}()
	}
	go func() {
		defer close(s.done)
		s.res, s.err = h.wait(ctx, cmd, r.Timeout)
		s.err = errors.Join(werr, s.err, remove())
		if dirOut != nil {
			s.res.Dir = <-dirOut
		}
	}()
	return s, nil
}

// wait waits for the shell and kills its process group when the
// timeout passes, when ctx ends, and once the shell has exited, so a
// stray child cannot hold the output open.
func (h *Host) wait(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) (machine.ExecResult, error) {
	pgid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	var res machine.ExecResult
	var werr, kerr error
	select {
	case werr = <-exited:
	case <-timer:
		res.TimedOut = true
		kerr = kill(pgid, syscall.SIGKILL)
		werr = <-exited
	case <-ctx.Done():
		res.Canceled = true
		kerr = kill(pgid, syscall.SIGTERM)
		select {
		case werr = <-exited:
		case <-time.After(KillGrace):
			kerr = errors.Join(kerr, kill(pgid, syscall.SIGKILL))
			werr = <-exited
		}
	}
	kerr = errors.Join(kerr, kill(pgid, syscall.SIGKILL))
	res.ExitCode = cmd.ProcessState.ExitCode()
	var ee *exec.ExitError
	if werr != nil && !errors.As(werr, &ee) {
		return res, errors.Join(fmt.Errorf("machine: wait for the command: %w", werr), kerr)
	}
	return res, kerr
}

// kill signals a process group. A group already gone is not an error:
// ESRCH, or EPERM, which macOS returns for a group whose remaining
// members are zombies waiting to be reaped.
func kill(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("machine: signal process group %d: %w", pgid, err)
	}
	return nil
}

// background starts a detached job whose output goes to a job log in the
// spill directory.
func (h *Host) background(ctx context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
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
	// A job outlives the call that started it and ends with the session.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), Shell, args...)
	cmd.Dir = h.dir(r.Dir)
	cmd.Env = h.environ(r.Env)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	serr := cmd.Start()
	cerr := log.Close()
	if serr != nil {
		return machine.ExecResult{}, errors.Join(fmt.Errorf("machine: start the job: %w", serr), cerr, remove())
	}
	pid := cmd.Process.Pid
	h.jobs[pid] = cmd
	go func() {
		werr := cmd.Wait()
		code := cmd.ProcessState.ExitCode()
		err := errors.Join(appendLine(log.Name(), fmt.Sprintf("\n[job %d exited with code %d]\n", pid, code)), remove())
		var ee *exec.ExitError
		if werr != nil && !errors.As(werr, &ee) {
			err = errors.Join(werr, err)
		}
		h.mu.Lock()
		delete(h.jobs, pid)
		if err != nil {
			h.jobErrs = append(h.jobErrs, fmt.Errorf("machine: job %d: %w", pid, err))
		}
		h.mu.Unlock()
	}()
	return machine.ExecResult{PID: pid, Log: log.Name()}, cerr
}

// Release lets go of the machine. The host keeps nothing for an idle
// session; at the session's end it stops every background job and
// closes the roots.
func (h *Host) Release(ctx context.Context, end bool) error {
	if !end {
		return nil
	}
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		return nil
	}
	h.released = true
	pids := make([]int, 0, len(h.jobs))
	for pid := range h.jobs {
		pids = append(pids, pid)
	}
	h.mu.Unlock()
	var errs []error
	for _, pid := range pids {
		errs = append(errs, kill(pid, syscall.SIGTERM))
	}
	deadline := time.Now().Add(KillGrace)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := len(h.jobs)
		h.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, pid := range pids {
		errs = append(errs, kill(pid, syscall.SIGKILL))
	}
	errs = append(errs, h.releaseStages(ctx))
	h.mu.Lock()
	errs = append(errs, h.jobErrs...)
	h.mu.Unlock()
	return errors.Join(append(errs, h.closeRoots())...)
}

// appendLine appends text to the file at path.
func appendLine(path, text string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(text)
	return errors.Join(werr, f.Close())
}

var _ machine.Machine = (*Host)(nil)

// Worktree gives an isolated thread its own git worktree under the
// worktree directory, on branch, from the HEAD commit of the working
// directory; a worktree made before is reopened.
func (h *Host) Worktree(ctx context.Context, name, branch string) (machine.Machine, error) {
	if h.opts.WorktreeDir == "" {
		return nil, fmt.Errorf("machine: this host keeps no worktrees: %w", errors.ErrUnsupported)
	}
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return nil, fmt.Errorf("machine: %q is not a worktree name", name)
	}
	dir := filepath.Join(h.opts.WorktreeDir, name)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(h.opts.WorktreeDir, 0o755); err != nil {
			return nil, fmt.Errorf("machine: create the worktree directory: %w", err)
		}
		res, err := h.Exec(ctx, machine.ExecRequest{Command: "git worktree add -q -b " + shellQuote(branch) + " " + shellQuote(dir) + " HEAD", Timeout: time.Minute})
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("machine: git worktree add: %s", strings.TrimSpace(string(res.Output)))
		}
	} else if err != nil {
		return nil, fmt.Errorf("machine: stat the worktree: %w", err)
	}
	o := h.opts
	o.Workdir, o.WorktreeDir = dir, ""
	o.SpillDir = filepath.Join(h.spill, "worktrees", name)
	o.ID = h.info.ID + "/" + name
	o.Environ = h.opts.Environ
	return Open(o)
}

// shellQuote renders an argument for /bin/sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var _ machine.Worktrees = (*Host)(nil)
