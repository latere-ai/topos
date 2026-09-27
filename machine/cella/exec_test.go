// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/test/stubs/cellastub"
)

func TestExec(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	if err := os.Mkdir(filepath.Join(f.ws(), "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := f.m.Exec(ctx, machine.ExecRequest{Command: `echo out; echo err >&2; echo "$ADDED"; pwd; exit 3`, Env: map[string]string{"ADDED": "yes"}})
	want := "out\nerr\nyes\n" + f.ws() + "\n"
	if err != nil || res.ExitCode != 3 || string(res.Output) != want || res.Dir != "" || res.TimedOut || res.Canceled {
		t.Errorf("exec = %+v %q %v", res, res.Output, err)
	}
	res, err = f.m.Exec(ctx, machine.ExecRequest{Command: "pwd; cd ..; mkdir -p ../other && cd ../other", Dir: "sub", ReportDir: true})
	if err != nil || strings.TrimSpace(string(res.Output)) != filepath.Join(f.ws(), "sub") || res.Dir != filepath.Join(filepath.Dir(f.ws()), "other") {
		t.Errorf("a relative dir and the final dir = %+v %q %v", res, res.Output, err)
	}
	res, err = f.m.Exec(ctx, machine.ExecRequest{Command: "cd " + f.ws() + "/sub; exit 1", ReportDir: true})
	if err != nil || res.ExitCode != 1 || res.Dir != filepath.Join(f.ws(), "sub") {
		t.Errorf("the final dir after an exit = %+v %v", res, err)
	}
	res, err = f.m.Exec(ctx, machine.ExecRequest{Command: "tr a-z A-Z", Stdin: strings.NewReader("hello input\n")})
	if err != nil || string(res.Output) != "HELLO INPUT\n" {
		t.Errorf("stdin = %q %v", res.Output, err)
	}
	res, err = f.m.Exec(ctx, machine.ExecRequest{Command: "cat; echo end"})
	if err != nil || string(res.Output) != "end\n" {
		t.Errorf("no stdin reads as empty: %q %v", res.Output, err)
	}
	big := strings.Repeat("0123456789abcdef", 16<<10)
	res, err = f.m.Exec(ctx, machine.ExecRequest{Command: "cat", Stdin: strings.NewReader(big)})
	if err != nil || string(res.Output) != big {
		t.Errorf("a large stdin came back as %d bytes, want %d: %v", len(res.Output), len(big), err)
	}
	if _, err := f.m.Exec(ctx, machine.ExecRequest{Command: "cat", Stdin: failingReader{}}); err == nil || !strings.Contains(err.Error(), "the command's input") {
		t.Errorf("a stdin that fails: %v", err)
	}
	_, err = f.m.Exec(ctx, machine.ExecRequest{Command: "true", Dir: "gone"})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.ws(), "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = f.m.Exec(ctx, machine.ExecRequest{Command: "true", Dir: "file"}); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("a file as the dir: %v", err)
	}
}

// failingReader fails every read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the disk went away") }

func TestExecTimeoutAndCancel(t *testing.T) {
	f := open(t)
	begun := time.Now()
	res, err := f.m.Exec(t.Context(), machine.ExecRequest{Command: "echo begun; sleep 30", Timeout: 300 * time.Millisecond})
	if err != nil || !res.TimedOut || res.Canceled || string(res.Output) != "begun\n" {
		t.Errorf("a timeout = %+v %q %v", res, res.Output, err)
	}
	if time.Since(begun) > 15*time.Second {
		t.Errorf("the timeout took %s", time.Since(begun))
	}
	ctx, cancel := context.WithCancel(t.Context())
	st, err := f.m.ExecStream(ctx, machine.ExecRequest{Command: "trap 'echo term; exit 7' TERM; echo ready; while :; do sleep 0.05; done"})
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, st, "ready")
	cancel()
	rest, err := io.ReadAll(st)
	if err != nil {
		t.Fatal(err)
	}
	res, err = st.Wait()
	if err != nil || !res.Canceled || res.ExitCode != 7 || !strings.Contains(string(rest), "term") {
		t.Errorf("a cancel = %+v %q %v", res, rest, err)
	}
}

func TestACanceledDialIsAbandoned(t *testing.T) {
	f := open(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.m.Exec(ctx, machine.ExecRequest{Command: "true"}); !errors.Is(err, context.Canceled) {
		t.Errorf("an exec whose context had ended: %v", err)
	}
	// The session that opens after is closed; the machine is still usable.
	if _, err := f.m.Exec(t.Context(), machine.ExecRequest{Command: "true"}); err != nil {
		t.Error(err)
	}
}

// readUntil reads a stream until what it read holds s.
func readUntil(t *testing.T, r io.Reader, s string) string {
	t.Helper()
	var got bytes.Buffer
	buf := make([]byte, 256)
	for !strings.Contains(got.String(), s) {
		n, err := r.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			t.Fatalf("read %q before %q: %v", got.String(), s, err)
		}
	}
	return got.String()
}

func TestAHelperThatDoesNotAnswerACancelIsClosed(t *testing.T) {
	f := open(t)
	oldGrace, oldMargin := killGrace, closeMargin
	killGrace, closeMargin = 0, 200*time.Millisecond
	t.Cleanup(func() { killGrace, closeMargin = oldGrace, oldMargin })
	ctx, cancel := context.WithCancel(t.Context())
	st, err := f.m.ExecStream(ctx, machine.ExecRequest{Command: "echo $PPID; sleep 5"})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(readUntil(t, st, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	// SIGSTOP holds the helper, so it answers no kill frame.
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Error(err)
		}
	})
	cancel()
	res, err := st.Wait()
	if err != nil || !res.Canceled || res.ExitCode != -1 {
		t.Errorf("a helper that did not answer = %+v %v", res, err)
	}
}

func TestCellaExecStreams(t *testing.T) {
	f := open(t)
	gate := filepath.Join(f.ws(), "gate")
	st, err := f.m.ExecStream(t.Context(), machine.ExecRequest{
		Command: fmt.Sprintf("echo first; while [ ! -e %s ]; do sleep 0.02; done; echo second; exit 4", gate),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The first line arrives while the command is still waiting for the
	// gate, so it is output as produced and not at the exit.
	if got := readUntil(t, st, "first\n"); got != "first\n" {
		t.Errorf("first = %q", got)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(st)
	if err != nil || string(rest) != "second\n" {
		t.Errorf("rest = %q %v", rest, err)
	}
	res, err := st.Wait()
	if err != nil || res.ExitCode != 4 || len(res.Output) != 0 {
		t.Errorf("wait = %+v %v", res, err)
	}
	// A stream nobody read to its end is drained by Wait.
	st, err = f.m.ExecStream(t.Context(), machine.ExecRequest{Command: "seq 1 20000"})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := st.Wait(); err != nil || res.ExitCode != 0 {
		t.Errorf("an unread stream = %+v %v", res, err)
	}
	if _, err := f.m.ExecStream(t.Context(), machine.ExecRequest{Command: "true", Background: true}); err == nil {
		t.Error("a background job was given a stream")
	}
}

func TestBackgroundJobs(t *testing.T) {
	f := open(t)
	res, err := f.m.Exec(t.Context(), machine.ExecRequest{Command: "echo from the job; exit 2", Background: true, Env: map[string]string{"X": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.PID <= 0 || filepath.Dir(res.Log) != f.m.SpillDir()+"/jobs" {
		t.Fatalf("job = %+v", res)
	}
	want := fmt.Sprintf("from the job\n\n[job %d exited with code 2]\n", res.PID)
	deadline := time.Now().Add(10 * time.Second)
	for {
		rc, err := f.m.ReadFile(t.Context(), res.Log)
		if err != nil {
			t.Fatal(err)
		}
		b, rerr := io.ReadAll(rc)
		if err := errors.Join(rerr, rc.Close()); err != nil {
			t.Fatal(err)
		}
		if string(b) == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("log = %q, want %q", b, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := f.m.Exec(t.Context(), machine.ExecRequest{Command: "true", Background: true, Dir: "gone"}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a job in a missing dir: %v", err)
	}
	f.stub.Fail(cellastub.OpExec, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
	if _, err := f.m.Exec(t.Context(), machine.ExecRequest{Command: "true", Background: true}); err == nil {
		t.Error("a job Cella could not start was reported started")
	}
}

func TestSessionTimeout(t *testing.T) {
	for in, want := range map[time.Duration]string{
		0:                "1h0m0s",
		time.Minute:      "1m15s",
		59 * time.Minute: "59m15s",
		time.Hour:        "1h0m0s",
		2 * time.Hour:    "1h0m0s",
	} {
		if got := sessionTimeout(in); got != want {
			t.Errorf("sessionTimeout(%s) = %s, want %s", in, got, want)
		}
	}
}
