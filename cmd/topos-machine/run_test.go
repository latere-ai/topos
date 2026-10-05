// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// driven is the run mode driven as the machine drives it: frames in
// through a pipe that stays open until the test closes it, frames out
// read as they come.
type driven struct {
	in   *frameWriter
	inW  *io.PipeWriter
	out  *io.PipeReader
	done chan int
}

func drive(t *testing.T, ctx context.Context, args ...string) *driven {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	d := &driven{in: &frameWriter{w: inW}, inW: inW, out: outR, done: make(chan int, 1)}
	go func() {
		code := run(ctx, append([]string{"run"}, args...), inR, outW, io.Discard)
		if err := outW.Close(); err != nil {
			t.Error(err)
		}
		d.done <- code
	}()
	t.Cleanup(func() {
		if err := errors.Join(inW.Close(), outR.Close()); err != nil {
			t.Error(err)
		}
	})
	return d
}

func (d *driven) send(t *testing.T, kind byte, payload string) {
	t.Helper()
	if err := d.in.frame(kind, []byte(payload)); err != nil {
		t.Fatal(err)
	}
}

// next reads one frame.
func (d *driven) next(t *testing.T) frame {
	t.Helper()
	kind, payload, err := readFrame(d.out)
	if err != nil {
		t.Fatalf("read a frame: %v", err)
	}
	return frame{kind, payload}
}

// rest reads the frames to the end and the exit code.
func (d *driven) rest(t *testing.T) ([]frame, int) {
	t.Helper()
	var out []frame
	for {
		kind, payload, err := readFrame(d.out)
		if errors.Is(err, io.EOF) {
			return out, <-d.done
		}
		if err != nil {
			t.Fatalf("read a frame: %v", err)
		}
		out = append(out, frame{kind, payload})
	}
}

// outcome folds frames into the output, the directory and the exit.
func outcome(t *testing.T, frames []frame) (string, string, exitBody) {
	t.Helper()
	var out strings.Builder
	dir := ""
	var exit exitBody
	sawExit := false
	for _, f := range frames {
		switch f.kind {
		case frameOutput:
			out.Write(f.payload)
		case frameDir:
			dir = string(f.payload)
		case frameExit:
			if err := json.Unmarshal(f.payload, &exit); err != nil {
				t.Fatal(err)
			}
			sawExit = true
		default:
			t.Fatalf("an unexpected frame %c %q", f.kind, f.payload)
		}
	}
	if !sawExit {
		t.Fatalf("no exit frame in %v", frames)
	}
	return out.String(), dir, exit
}

// started reads the start frame.
func (d *driven) started(t *testing.T) {
	t.Helper()
	if f := d.next(t); f.kind != frameStart {
		t.Fatalf("the first frame is %c %q, want the start", f.kind, f.payload)
	}
}

// until reads output frames until the output holds s.
func (d *driven) until(t *testing.T, s string) {
	t.Helper()
	var got strings.Builder
	for !strings.Contains(got.String(), s) {
		f := d.next(t)
		if f.kind != frameOutput {
			t.Fatalf("frame %c %q before %q", f.kind, f.payload, s)
		}
		got.Write(f.payload)
	}
}

func TestRunOutputAndExit(t *testing.T) {
	dir := tempDir(t)
	d := drive(t, t.Context(), "-dir", dir, "--", "echo out; echo err >&2; exit 4")
	d.started(t)
	d.send(t, frameEOF, "")
	frames, code := d.rest(t)
	out, gotDir, exit := outcome(t, frames)
	if code != 0 || out != "out\nerr\n" || gotDir != "" || !reflect.DeepEqual(exit, exitBody{Code: 4}) {
		t.Errorf("code %d, output %q, dir %q, exit %+v", code, out, gotDir, exit)
	}
}

func TestRunReportsTheFinalDirectory(t *testing.T) {
	dir := tempDir(t)
	for script, want := range map[string]string{
		"/bin/mkdir -p sub && cd sub": filepath.Join(dir, "sub"),
		"cd /; exit 3":                "/",
	} {
		d := drive(t, t.Context(), "-dir", dir, "-report-dir", "--", script)
		d.started(t)
		d.send(t, frameEOF, "")
		frames, _ := d.rest(t)
		_, got, _ := outcome(t, frames)
		if got != want {
			t.Errorf("%q: dir %q, want %q", script, got, want)
		}
	}
}

func TestRunDeliversInput(t *testing.T) {
	d := drive(t, t.Context(), "--", "/bin/cat; echo done")
	d.started(t)
	d.send(t, frameInput, "hello ")
	d.send(t, frameInput, "world\n")
	d.send(t, frameEOF, "")
	frames, _ := d.rest(t)
	if out, _, exit := outcome(t, frames); out != "hello world\ndone\n" || exit.Code != 0 {
		t.Errorf("output %q, exit %+v", out, exit)
	}
	// Input past what the command reads is dropped.
	d = drive(t, t.Context(), "--", "exec 0<&-; echo closed")
	d.started(t)
	d.until(t, "closed")
	d.send(t, frameInput, "ignored")
	d.send(t, frameEOF, "")
	if _, code := d.rest(t); code != 0 {
		t.Errorf("exit %d", code)
	}
}

func TestRunRefusesAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	file := filepath.Join(tempDir(t), "file")
	writeFile(t, file, "x")
	for dir, kind := range map[string]string{missing: kindNotExist, file: kindNotDir} {
		d := drive(t, t.Context(), "-dir", dir, "--", "true")
		f := d.next(t)
		var e errorBody
		if f.kind != frameFail || json.Unmarshal(f.payload, &e) != nil || e.Kind != kind {
			t.Errorf("%s: frame %c %q, want a failure of kind %s", dir, f.kind, f.payload, kind)
		}
		if _, code := d.rest(t); code != 0 {
			t.Errorf("exit %d", code)
		}
	}
}

func TestRunTimesOut(t *testing.T) {
	d := drive(t, t.Context(), "-timeout", "200ms", "--", "echo begun; /bin/sleep 30")
	d.started(t)
	d.send(t, frameEOF, "")
	begun := time.Now()
	frames, _ := d.rest(t)
	out, _, exit := outcome(t, frames)
	if !exit.TimedOut || exit.Canceled || out != "begun\n" {
		t.Errorf("output %q, exit %+v", out, exit)
	}
	if time.Since(begun) > 10*time.Second {
		t.Errorf("the timeout took %s", time.Since(begun))
	}
}

func TestRunCancels(t *testing.T) {
	// The shell traps SIGTERM, so a canceled command gets its SIGTERM
	// before anything harder.
	script := "trap 'echo term; exit 7' TERM; echo ready; while :; do /bin/sleep 0.05; done"
	t.Run("kill frame", func(t *testing.T) {
		d := drive(t, t.Context(), "--", script)
		d.started(t)
		d.send(t, frameEOF, "")
		d.until(t, "ready")
		d.send(t, frameKill, "")
		frames, _ := d.rest(t)
		out, _, exit := outcome(t, frames)
		if !exit.Canceled || exit.Code != 7 || !strings.Contains(out, "term") {
			t.Errorf("output %q, exit %+v", out, exit)
		}
	})
	t.Run("input ends", func(t *testing.T) {
		d := drive(t, t.Context(), "--", script)
		d.started(t)
		d.until(t, "ready")
		if err := d.inW.Close(); err != nil {
			t.Fatal(err)
		}
		frames, _ := d.rest(t)
		if _, _, exit := outcome(t, frames); !exit.Canceled {
			t.Errorf("exit %+v", exit)
		}
	})
	t.Run("signal", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		d := drive(t, ctx, "--", script)
		d.started(t)
		d.send(t, frameEOF, "")
		d.until(t, "ready")
		cancel()
		frames, _ := d.rest(t)
		if _, _, exit := outcome(t, frames); !exit.Canceled {
			t.Errorf("exit %+v", exit)
		}
	})
	t.Run("grace", func(t *testing.T) {
		d := drive(t, t.Context(), "-grace", "100ms", "--", "trap '' TERM; echo ready; /bin/sleep 30")
		d.started(t)
		d.send(t, frameEOF, "")
		d.until(t, "ready")
		d.send(t, frameKill, "")
		frames, _ := d.rest(t)
		if _, _, exit := outcome(t, frames); !exit.Canceled || exit.Code != -1 {
			t.Errorf("a command that ignores SIGTERM: exit %+v", exit)
		}
	})
}

func TestRunEndsTheProcessGroup(t *testing.T) {
	d := drive(t, t.Context(), "--", "/bin/sleep 30 & echo started")
	d.started(t)
	d.send(t, frameEOF, "")
	begun := time.Now()
	frames, _ := d.rest(t)
	if out, _, exit := outcome(t, frames); out != "started\n" || exit.Code != 0 {
		t.Errorf("output %q, exit %+v", out, exit)
	}
	if time.Since(begun) > 10*time.Second {
		t.Errorf("a stray child held the output for %s", time.Since(begun))
	}
}

// limitedWriter accepts n writes and refuses the rest, as an exec socket
// that closes part way does.
func TestRunStopsWhenTheOutputCannotBeWritten(t *testing.T) {
	inR, inW := io.Pipe()
	t.Cleanup(func() {
		if err := inW.Close(); err != nil {
			t.Error(err)
		}
	})
	if code := run(t.Context(), []string{"run", "--", "/bin/sleep 30"}, inR, &limitedWriter{}, io.Discard); code != 3 {
		t.Errorf("a start that cannot be reported: exit %d", code)
	}
	begun := time.Now()
	if code := run(t.Context(), []string{"run", "--", "echo out; /bin/sleep 30"}, inR, &limitedWriter{n: 1}, io.Discard); code != 3 {
		t.Errorf("output that cannot be sent: exit %d", code)
	}
	if time.Since(begun) > 20*time.Second {
		t.Errorf("the command ran on for %s", time.Since(begun))
	}
	if code := run(t.Context(), []string{"run", "-dir", filepath.Join(t.TempDir(), "gone"), "--", "true"}, inR, &limitedWriter{}, io.Discard); code != 3 {
		t.Errorf("a failure that cannot be reported: exit %d", code)
	}
}

func TestJob(t *testing.T) {
	dir := tempDir(t)
	jobs := filepath.Join(dir, "spill", "jobs")
	start := func(args ...string) response {
		t.Helper()
		var stdout bytes.Buffer
		if code := run(t.Context(), append([]string{"job"}, args...), nil, &stdout, io.Discard); code != 0 {
			t.Fatalf("exit %d", code)
		}
		return decode(t, &stdout)
	}
	res := start("-jobs", jobs, "-dir", dir, "--", "pwd; echo hi; exit 5")
	if res.Job == nil || res.Job.PID <= 0 || filepath.Dir(res.Job.Log) != jobs {
		t.Fatalf("job = %+v", res)
	}
	want := fmt.Sprintf("[job %d exited with code 5]", res.Job.PID)
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(res.Job.Log)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), want) {
			if !strings.HasPrefix(string(b), dir+"\nhi\n") {
				t.Errorf("log = %q", b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("log = %q, want %q", b, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	res = start("-jobs", jobs, "-dir", filepath.Join(dir, "gone"), "--", "true")
	if res.Error == nil || res.Error.Kind != kindNotExist {
		t.Errorf("a missing directory: %+v", res)
	}
	logs, err := filepath.Glob(filepath.Join(jobs, "*.log"))
	if err != nil || len(logs) != 1 {
		t.Errorf("logs %v %v: the log of a job that did not start is removed", logs, err)
	}
	script := filepath.Join(dir, "script.sh")
	writeFile(t, script, "echo from a file")
	res = start("-jobs", jobs, "-dir", dir, "-script-file", script)
	if res.Job == nil {
		t.Fatalf("a job from a file = %+v", res)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		b, err := os.ReadFile(res.Job.Log)
		if err == nil && strings.HasPrefix(string(b), "from a file\n") && strings.Contains(string(b), "exited with code 0") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("log = %q %v", b, err)
		}
	}
	if _, err := os.Stat(script); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the script file is left after the job: %v", err)
	}
	if res := start("-jobs", jobs, "-script-file", script); res.Error == nil || res.Error.Kind != kindNotExist {
		t.Errorf("a missing script file: %+v", res)
	}
	for _, args := range [][]string{{"job", "-jobs", jobs, "-script-file", script, "--", "x"}, {"job", "-jobs", jobs}} {
		if code := run(t.Context(), args, nil, io.Discard, io.Discard); code != 2 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
	file := filepath.Join(dir, "file")
	writeFile(t, file, "x")
	if res := start("-jobs", filepath.Join(file, "jobs"), "--", "true"); res.Error == nil {
		t.Error("a jobs directory below a file was made")
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	if res := start("-jobs", locked, "--", "true"); res.Error == nil {
		t.Error("a log was made in a directory that refuses it")
	}
}

func TestRunReadsAFramedScript(t *testing.T) {
	d := drive(t, t.Context(), "-script-frames", "--")
	script := "read line; echo \"got $line\"; " + strings.Repeat(": padding;", 10000) + " echo done"
	d.send(t, frameInput, script[:len(script)/2])
	d.send(t, frameInput, script[len(script)/2:])
	d.send(t, frameEOF, "")
	d.started(t)
	d.send(t, frameInput, "input\n")
	d.send(t, frameEOF, "")
	frames, _ := d.rest(t)
	if out, _, exit := outcome(t, frames); out != "got input\ndone\n" || exit.Code != 0 {
		t.Errorf("output %q, exit %+v", out, exit)
	}
	d = drive(t, t.Context(), "-script-frames", "--")
	d.send(t, frameKill, "")
	if f := d.next(t); f.kind != frameFail || !strings.Contains(string(f.payload), "read the script") {
		t.Errorf("a script cut short: %c %q", f.kind, f.payload)
	}
	for _, args := range [][]string{{"run", "-script-frames", "--", "echo"}, {"run", "--", "a", "b"}} {
		if code := run(t.Context(), args, nil, io.Discard, io.Discard); code != 2 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}
