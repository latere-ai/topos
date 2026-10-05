// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"latere.ai/x/topos/machine"
)

// shell runs every command, as on the host machine.
const shell = "/bin/sh"

// reportPrefix makes the shell write its final directory to descriptor 3
// on every exit, an explicit exit included, as on the host machine.
const reportPrefix = "trap 'pwd -P >&3' EXIT\n"

// killGrace is how long a canceled command has between SIGTERM and
// SIGKILL, the host machine's grace.
const killGrace = 5 * time.Second

// runCommand runs one script under /bin/sh in its own process group and
// speaks the frames of wire.go: input frames on standard input, then the
// start, the output, the final directory and the exit on standard output.
// A script too long for the exec socket's first message, which Cella
// bounds with its body limit, arrives instead as input frames ended by an
// end frame, before the command's own input.
// The timeout kills the group; a kill frame, standard input ending, or a
// signal to the helper cancels it with SIGTERM and SIGKILL after the grace.
func runCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "", "the directory the command starts in")
	timeout := flags.Duration("timeout", 0, "kill the command's process group after this long; zero is no timeout")
	report := flags.Bool("report-dir", false, "report the shell's final directory")
	grace := flags.Duration("grace", killGrace, "how long a canceled command has between SIGTERM and SIGKILL")
	framed := flags.Bool("script-frames", false, "read the script from the input frames before the command's input")
	serverGrace := flags.Duration("server-grace", 0, "move the command to the background once it has run this long and listens on a server port; zero never moves it")
	jobs := flags.String("jobs", "", "the directory of job logs, where a moved command's output goes")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *framed != (flags.NArg() == 0) || flags.NArg() > 1 {
		return fail(stderr, "run [flags] -- <script>, or run -script-frames [flags]")
	}
	if *serverGrace > 0 && *jobs == "" {
		return fail(stderr, "run -server-grace needs -jobs")
	}
	out := &frameWriter{w: stdout}
	script := flags.Arg(0)
	if *framed {
		var b strings.Builder
		if err := copyFrames(&b, stdin); err != nil {
			return sent(out.json(frameFail, failure(fmt.Errorf("machine: read the script: %w", err))))
		}
		script = b.String()
	}
	c, err := start(ctx, *dir, script, *report)
	if err != nil {
		return sent(out.json(frameFail, failure(err)))
	}
	if err := out.frame(frameStart, nil); err != nil {
		return sent(errors.Join(err, c.abort()))
	}
	kill := make(chan struct{})
	var once sync.Once
	cancel := func() { once.Do(func() { close(kill) }) }
	go c.input(stdin, cancel)
	c.pumped = make(chan error, 1)
	go c.pumpTo(out, cancel)
	res, moved, err := c.wait(ctx, *timeout, *grace, kill, server{grace: *serverGrace, jobs: *jobs, out: out, cancel: cancel})
	if moved {
		// The command runs on as a job: its output is the log pump's,
		// and its shell has not exited, so it has no final directory to
		// report yet. Its input ends here, as a background job's is
		// empty; a command that already closed it has nothing to lose.
		_ = c.closeInput()
		return sent(out.json(frameExit, res))
	}
	err = errors.Join(err, <-c.pumped, c.out.Close())
	d := c.finalDir()
	if err != nil {
		return sent(out.json(frameFail, failure(err)))
	}
	if d != "" {
		if err := out.frame(frameDir, []byte(d)); err != nil {
			return 3
		}
	}
	return sent(out.json(frameExit, res))
}

// command is one started script: its process, the read end of its
// output, the write end of its input, and the read end of descriptor 3.
type command struct {
	cmd  *exec.Cmd
	out  *os.File
	in   *os.File
	dirs chan string
	// remove deletes the script's file, for a script too long to be the
	// shell's argument.
	remove func() error

	inOnce sync.Once
	inErr  error

	// pumped carries the output pump's end; handing is set while the
	// pump stops for the pipe to move to a job's log pump.
	pumped  chan error
	handing atomic.Bool
}

// start starts the script with one pipe for its standard output and
// standard error, in the order written, and one for its input. The helper
// cancels a command itself, SIGTERM to its process group and SIGKILL after
// the grace, so the context never kills it.
func start(ctx context.Context, dir, script string, report bool) (*command, error) {
	if report {
		script = reportPrefix + script
	}
	args, remove, err := machine.ShellArgs(os.TempDir(), script)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), shell, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("machine: create the output pipe: %w", err), remove())
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("machine: create the input pipe: %w", err), outR.Close(), outW.Close(), remove())
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, outW
	closers := []*os.File{outR, outW, inR, inW}
	var dirR, dirW *os.File
	if report {
		if dirR, dirW, err = os.Pipe(); err != nil {
			return nil, errors.Join(fmt.Errorf("machine: create the directory pipe: %w", err), closeAll(closers), remove())
		}
		cmd.ExtraFiles = []*os.File{dirW}
		closers = append(closers, dirR, dirW)
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, closeAll(closers), remove())
	}
	// The child holds its own ends now; the helper keeps the other ones.
	errs := []error{outW.Close(), inR.Close()}
	c := &command{cmd: cmd, out: outR, in: inW, remove: remove}
	if dirW != nil {
		errs = append(errs, dirW.Close())
		c.dirs = make(chan string, 1)
		go func() {
			b, rerr := io.ReadAll(io.LimitReader(dirR, 1<<16))
			if cerr := dirR.Close(); rerr == nil {
				rerr = cerr
			}
			if rerr != nil {
				c.dirs <- ""
				return
			}
			c.dirs <- strings.TrimSpace(string(b))
		}()
	}
	if err := errors.Join(errs...); err != nil {
		return nil, errors.Join(err, c.abort())
	}
	return c, nil
}

func closeAll(files []*os.File) error {
	var errs []error
	for _, f := range files {
		errs = append(errs, f.Close())
	}
	return errors.Join(errs...)
}

// abort kills a command whose start could not be reported and reaps it.
func (c *command) abort() error {
	err := signalGroup(c.cmd.Process.Pid, syscall.SIGKILL)
	// The wait reports the kill itself, which is the outcome asked for.
	_ = c.cmd.Wait()
	return errors.Join(err, c.closeInput(), c.out.Close(), c.remove())
}

// closeInput ends the command's standard input once.
func (c *command) closeInput() error {
	c.inOnce.Do(func() { c.inErr = c.in.Close() })
	return c.inErr
}

// input reads the machine's frames: bytes for the command's input, the
// input's end, and a kill. Standard input ending is the exec session
// ending, and cancels the command.
func (c *command) input(stdin io.Reader, cancel func()) {
	for {
		kind, payload, err := readFrame(stdin)
		if err != nil {
			// The session is gone; nothing is left to report a close
			// error to, and the command is canceled.
			_ = c.closeInput()
			cancel()
			return
		}
		switch kind {
		case frameInput:
			if _, err := c.in.Write(payload); err != nil {
				// The command closed its input; what it did not read is
				// dropped, as a pipe drops it.
				_ = c.closeInput()
			}
		case frameEOF:
			_ = c.closeInput()
		case frameKill:
			cancel()
		}
	}
}

// pumpTo runs pump and hands its end to pumped.
func (c *command) pumpTo(out *frameWriter, cancel func()) { c.pumped <- c.pump(out, cancel) }

// errHandedOver is the pump stopping for a move to the background: the
// pipe is still open, and the job's log pump reads it from then on.
var errHandedOver = errors.New("machine: the output moved to the job's log")

// pump sends the command's output as it is written, until the pipe ends,
// which is once every process in the group has exited or been killed, or
// until a move to the background stops it. A frame that cannot be
// written cancels the command.
func (c *command) pump(out *frameWriter, cancel func()) error {
	buf := make([]byte, chunk)
	for {
		n, err := c.out.Read(buf)
		if n > 0 {
			if werr := out.frame(frameOutput, buf[:n]); werr != nil {
				cancel()
				_, derr := io.Copy(io.Discard, c.out)
				return errors.Join(werr, derr)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, os.ErrDeadlineExceeded) && c.handing.Load() {
			return errHandedOver
		}
		if err != nil {
			return err
		}
	}
}

// finalDir is the shell's final directory, or empty when none was asked
// for or the shell did not reach its exit.
func (c *command) finalDir() string {
	if c.dirs == nil {
		return ""
	}
	return <-c.dirs
}

// server is the check that moves a server to the background (spec 045):
// the grace before the first look at the group's sockets, the jobs
// directory its log goes in, and the output pump's writer and cancel,
// which a move that could not be made starts again.
type server struct {
	grace  time.Duration
	jobs   string
	out    *frameWriter
	cancel func()
}

// wait waits for the shell and kills its process group when the timeout
// passes, when the command is canceled, and once the shell has exited,
// so a stray child cannot hold the output open. It is the host machine's
// wait. With a server grace, a command still running after it whose
// group listens on a server port is moved to the background instead, and
// wait reports the move with the group left running.
func (c *command) wait(ctx context.Context, timeout, grace time.Duration, kill <-chan struct{}, srv server) (exitBody, bool, error) {
	pgid := c.cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- c.cmd.Wait() }()
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	var probe *time.Timer
	var probed <-chan time.Time
	if srv.grace > 0 && machine.SeesServers {
		probe = time.NewTimer(srv.grace)
		defer probe.Stop()
		probed = probe.C
	}
	var res exitBody
	var werr, kerr error
	canceled := func() {
		res.Canceled = true
		kerr = signalGroup(pgid, syscall.SIGTERM)
		select {
		case werr = <-exited:
		case <-time.After(grace):
			kerr = errors.Join(kerr, signalGroup(pgid, syscall.SIGKILL))
			werr = <-exited
		}
	}
waiting:
	for {
		select {
		case werr = <-exited:
			break waiting
		case <-timer:
			res.TimedOut = true
			kerr = signalGroup(pgid, syscall.SIGKILL)
			werr = <-exited
			break waiting
		case <-kill:
			canceled()
			break waiting
		case <-ctx.Done():
			canceled()
			break waiting
		case <-probed:
			ports, err := machine.ServerPorts(ctx, pgid)
			if err == nil && len(ports) > 0 {
				var log string
				var after error
				if log, after, err = c.handOver(ctx, srv, pgid); err == nil {
					moved := exitBody{Moved: true, PID: pgid, Log: log, Ports: ports}
					if after != nil {
						moved.ServerErr = after.Error()
					}
					return moved, true, nil
				}
			}
			switch {
			case err != nil && ctx.Err() == nil:
				// The check cannot be had for this command; it goes on
				// under its timeout, and the exit says why.
				res.ServerErr, probed = err.Error(), nil
			case err == nil:
				probe.Reset(machine.ServerPoll)
			}
		}
	}
	kerr = errors.Join(kerr, signalGroup(pgid, syscall.SIGKILL), c.closeInput(), c.remove())
	res.Code = c.cmd.ProcessState.ExitCode()
	var ee *exec.ExitError
	if werr != nil && !errors.As(werr, &ee) {
		return res, false, errors.Join(fmt.Errorf("machine: wait for the command: %w", werr), kerr)
	}
	return res, false, kerr
}

// handOver moves the command's output to a new job log in the jobs
// directory: the helper's own pump stops, so the pipe has one reader at a
// time, and a log pump, a copy of this helper in a session of its own,
// takes the pipe and outlives the helper, as a background job does. A
// move that was made answers the log, and after, what went wrong in
// letting go of the helper's copies, which leaves the move standing. A
// move that cannot be made gives the pipe back to the helper's pump, and
// the command stays in the foreground.
func (c *command) handOver(ctx context.Context, srv server, pgid int) (log string, after, err error) {
	if err := os.MkdirAll(srv.jobs, 0o700); err != nil {
		return "", nil, fmt.Errorf("machine: create the job directory: %w", err)
	}
	f, err := os.CreateTemp(srv.jobs, "job-*.log")
	if err != nil {
		return "", nil, fmt.Errorf("machine: create the job log: %w", err)
	}
	drop := func(err error) (string, error, error) {
		return "", nil, errors.Join(err, f.Close(), os.Remove(f.Name()))
	}
	exe, err := os.Executable()
	if err != nil {
		return drop(fmt.Errorf("machine: find the helper for the job's log pump: %w", err))
	}
	c.handing.Store(true)
	if err := c.out.SetReadDeadline(time.Now()); err != nil {
		c.handing.Store(false)
		return drop(fmt.Errorf("machine: stop reading the command's output: %w", err))
	}
	perr := <-c.pumped
	derr := c.out.SetReadDeadline(time.Time{})
	if !errors.Is(perr, errHandedOver) || derr != nil {
		// The pump ended on its own, at the output's end or a frame it
		// could not send, and its end is the command's to report; or the
		// pipe could not be read again. Either way nothing moves.
		c.handing.Store(false)
		if errors.Is(perr, errHandedOver) {
			go c.pumpTo(srv.out, srv.cancel)
		} else {
			c.pumped <- perr
		}
		return drop(errors.Join(errors.New("machine: the command's output could not be moved"), derr))
	}
	// The log pump outlives the helper that starts it, and so its
	// context.
	p := exec.CommandContext(context.WithoutCancel(ctx), exe, "pump", "-job", strconv.Itoa(pgid))
	p.Stdin, p.Stdout, p.Stderr = c.out, f, f
	p.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := p.Start(); err != nil {
		c.handing.Store(false)
		go c.pumpTo(srv.out, srv.cancel)
		return drop(fmt.Errorf("machine: start the job's log pump: %w", err))
	}
	// The log pump holds its own copies of the pipe and the log, so the
	// move stands whatever letting go of the helper's says, which is
	// answered as after.
	return f.Name(), errors.Join(f.Close(), c.out.Close(), p.Process.Release()), nil
}

// signalGroup signals a process group. A group already gone is not an
// error: ESRCH, or EPERM, which macOS returns for a group whose remaining
// members are zombies.
func signalGroup(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("machine: signal process group %d: %w", pgid, err)
	}
	return nil
}

// jobWrapper runs a background job's script, given as the shell
// arguments machine.ShellArgs chose, removes the script's file when it
// had one, and appends the line the host machine appends to a job log
// when the job exits. Its $$ is the job's pid, which leads the job's
// process group. rm is named by its path, as the shell is, so a job
// does not depend on the PATH it was started with.
const jobWrapper = shell + ` "$@"; code=$?; [ "$1" = -c ] || /bin/rm -f "$1"; printf '\n[job %d exited with code %d]\n' "$$" "$code"`

// job starts a script detached in its own process group, with its output
// in a new log in the jobs directory, and answers its pid and log at
// once. The job outlives the helper and ends with the sandbox.
func job(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("job", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jobs := flags.String("jobs", "", "the directory of job logs")
	dir := flags.String("dir", "", "the directory the job starts in")
	file := flags.String("script-file", "", "read the script from this file and remove it")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if (*file == "") != (flags.NArg() == 1) || flags.NArg() > 1 || *jobs == "" {
		return fail(stderr, "job -jobs <dir> [-dir <dir>] -- <script>, or job -jobs <dir> -script-file <file> [-dir <dir>]")
	}
	if err := os.MkdirAll(*jobs, 0o700); err != nil {
		return answer(stdout, response{Error: failure(fmt.Errorf("machine: create the job directory: %w", err))})
	}
	// A script too long for Cella's synchronous exec body arrives as a
	// file the machine wrote in the spill directory, which the shell reads
	// as its script and the wrapper removes when the job ends.
	args, remove := []string{*file}, func() error { return os.Remove(*file) }
	if *file != "" {
		if _, err := os.Stat(*file); err != nil {
			return answer(stdout, response{Error: failure(fmt.Errorf("machine: read the script: %w", err))})
		}
	} else {
		var err error
		if args, remove, err = machine.ShellArgs(*jobs, flags.Arg(0)); err != nil {
			return answer(stdout, response{Error: failure(err)})
		}
	}
	log, err := os.CreateTemp(*jobs, "job-*.log")
	if err != nil {
		return answer(stdout, response{Error: failure(errors.Join(fmt.Errorf("machine: create the job log: %w", err), remove()))})
	}
	// A job outlives the helper that started it, and so its context.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), shell, append([]string{"-c", jobWrapper, "job"}, args...)...)
	cmd.Dir = *dir
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	serr := cmd.Start()
	cerr := log.Close()
	if serr != nil {
		return answer(stdout, response{Error: failure(errors.Join(serr, cerr, os.Remove(log.Name()), remove()))})
	}
	pid := cmd.Process.Pid
	if err := errors.Join(cerr, cmd.Process.Release()); err != nil {
		return answer(stdout, response{Error: failure(err)})
	}
	return answer(stdout, response{Job: &jobBody{PID: pid, Log: log.Name()}})
}
