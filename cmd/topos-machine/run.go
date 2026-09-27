// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
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
// The timeout kills the group; a kill frame, standard input ending, or a
// signal to the helper cancels it with SIGTERM and SIGKILL after the grace.
func runCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "", "the directory the command starts in")
	timeout := flags.Duration("timeout", 0, "kill the command's process group after this long; zero is no timeout")
	report := flags.Bool("report-dir", false, "report the shell's final directory")
	grace := flags.Duration("grace", killGrace, "how long a canceled command has between SIGTERM and SIGKILL")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		return fail(stderr, "run [flags] -- <script>")
	}
	out := &frameWriter{w: stdout}
	c, err := start(*dir, flags.Arg(0), *report)
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
	pumped := make(chan error, 1)
	go func() { pumped <- c.pump(out, cancel) }()
	res, err := c.wait(ctx, *timeout, *grace, kill)
	err = errors.Join(err, <-pumped, c.out.Close())
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

// sent is the exit code once the last frame was written, or not.
func sent(err error) int {
	if err != nil {
		return 3
	}
	return 0
}

// command is one started script: its process, the read end of its
// output, the write end of its input, and the read end of descriptor 3.
type command struct {
	cmd  *exec.Cmd
	out  *os.File
	in   *os.File
	dirs chan string

	inOnce sync.Once
	inErr  error
}

// start starts the script with one pipe for its standard output and
// standard error, in the order written, and one for its input.
func start(dir, script string, report bool) (*command, error) {
	if report {
		script = reportPrefix + script
	}
	cmd := exec.Command(shell, "-c", script)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("machine: create the output pipe: %w", err)
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("machine: create the input pipe: %w", err), outR.Close(), outW.Close())
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, outW
	closers := []*os.File{outR, outW, inR, inW}
	var dirR, dirW *os.File
	if report {
		if dirR, dirW, err = os.Pipe(); err != nil {
			return nil, errors.Join(fmt.Errorf("machine: create the directory pipe: %w", err), closeAll(closers))
		}
		cmd.ExtraFiles = []*os.File{dirW}
		closers = append(closers, dirR, dirW)
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, closeAll(closers))
	}
	// The child holds its own ends now; the helper keeps the other ones.
	errs := []error{outW.Close(), inR.Close()}
	c := &command{cmd: cmd, out: outR, in: inW}
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
	return errors.Join(err, c.closeInput(), c.out.Close())
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

// pump sends the command's output as it is written, until the pipe ends,
// which is once every process in the group has exited or been killed. A
// frame that cannot be written cancels the command.
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

// wait waits for the shell and kills its process group when the timeout
// passes, when the command is canceled, and once the shell has exited,
// so a stray child cannot hold the output open. It is the host machine's
// wait.
func (c *command) wait(ctx context.Context, timeout, grace time.Duration, kill <-chan struct{}) (exitBody, error) {
	pgid := c.cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- c.cmd.Wait() }()
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
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
	select {
	case werr = <-exited:
	case <-timer:
		res.TimedOut = true
		kerr = signalGroup(pgid, syscall.SIGKILL)
		werr = <-exited
	case <-kill:
		canceled()
	case <-ctx.Done():
		canceled()
	}
	kerr = errors.Join(kerr, signalGroup(pgid, syscall.SIGKILL), c.closeInput())
	res.Code = c.cmd.ProcessState.ExitCode()
	var ee *exec.ExitError
	if werr != nil && !errors.As(werr, &ee) {
		return res, errors.Join(fmt.Errorf("machine: wait for the command: %w", werr), kerr)
	}
	return res, kerr
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

// jobWrapper runs a background job's script and appends the line the host
// machine appends to a job log when the job exits. Its $$ is the job's
// pid, which leads the job's process group.
const jobWrapper = shell + ` -c "$1"; code=$?; printf '\n[job %d exited with code %d]\n' "$$" "$code"`

// job starts a script detached in its own process group, with its output
// in a new log in the jobs directory, and answers its pid and log at
// once. The job outlives the helper and ends with the sandbox.
func job(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("job", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jobs := flags.String("jobs", "", "the directory of job logs")
	dir := flags.String("dir", "", "the directory the job starts in")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || *jobs == "" {
		return fail(stderr, "job -jobs <dir> [-dir <dir>] -- <script>")
	}
	if err := os.MkdirAll(*jobs, 0o700); err != nil {
		return answer(stdout, response{Error: failure(fmt.Errorf("machine: create the job directory: %w", err))})
	}
	log, err := os.CreateTemp(*jobs, "job-*.log")
	if err != nil {
		return answer(stdout, response{Error: failure(fmt.Errorf("machine: create the job log: %w", err))})
	}
	cmd := exec.Command(shell, "-c", jobWrapper, "job", flags.Arg(0))
	cmd.Dir = *dir
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	serr := cmd.Start()
	cerr := log.Close()
	if serr != nil {
		return answer(stdout, response{Error: failure(errors.Join(serr, cerr, os.Remove(log.Name())))})
	}
	pid := cmd.Process.Pid
	if err := errors.Join(cerr, cmd.Process.Release()); err != nil {
		return answer(stdout, response{Error: failure(err)})
	}
	return answer(stdout, response{Job: &jobBody{PID: pid, Log: log.Name()}})
}
