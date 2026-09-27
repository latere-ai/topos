// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/client"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// raw is one frame the fake helper sends.
type raw struct {
	kind    byte
	payload string
}

// emit is a shell command that writes the frames, and then the tail
// bytes, as a helper that misbehaves would.
func emit(tail string, frames ...raw) string {
	var b []byte
	for _, f := range frames {
		h := make([]byte, 5)
		h[0] = f.kind
		binary.BigEndian.PutUint32(h[1:], uint32(len(f.payload)))
		b = append(append(b, h...), f.payload...)
	}
	b = append(b, tail...)
	var s strings.Builder
	for _, c := range b {
		fmt.Fprintf(&s, "\\%03o", c)
	}
	return "printf '" + s.String() + "'"
}

// session opens an exec session running command in the fixture's sandbox.
func (f fixture) session(t *testing.T, command string) *client.Session {
	t.Helper()
	sess, err := f.m.c.ExecSession(t.Context(), f.m.Info().ID, client.ExecRequest{Command: []string{shell, "-c", command}})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestOpenedTellsTheHelperFromAnythingElse(t *testing.T) {
	f := open(t)
	for name, c := range map[string]struct {
		command string
		want    error
	}{
		"nothing":            {"true", errNoHelper},
		"a shell message":    {"echo 'sh: topos-machine: not found' >&2; exit 127", errNoHelper},
		"a failure":          {emit("", raw{frameFail, `{"kind":"not_exist","message":"chdir gone: no such file"}`}), fs.ErrNotExist},
		"a bad failure":      {emit("", raw{frameFail, `not json`}), errNoHelper},
		"a cut failure":      {emit("f\x00\x00\x00dabc"), errNoHelper},
		"a start with bytes": {emit("", raw{frameStart, "x"}), errNoHelper},
	} {
		err := opened(f.session(t, c.command), "chdir", "/gone")
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	sess, err := f.m.c.ExecSession(t.Context(), f.m.Info().ID, client.ExecRequest{Command: []string{"sh"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if err := opened(sess, "chdir", "/"); client.CodeOf(err) != "capability_unsupported" {
		t.Errorf("a refusal the socket carried: %v", err)
	}
}

// stream is a command stream over frames a fake helper sends.
func (f fixture) stream(t *testing.T, frames ...raw) *stream {
	t.Helper()
	sess := f.session(t, emit("", append([]raw{{frameStart, ""}}, frames...)...))
	if err := opened(sess, "chdir", "/"); err != nil {
		t.Fatal(err)
	}
	st := &stream{sess: sess, w: &sender{s: sess}, done: make(chan struct{}), inputDone: make(chan error, 1),
		check: func(err error) error { return err }}
	st.inputDone <- nil
	return st
}

func TestTheStreamReadsTheHelpersFrames(t *testing.T) {
	f := open(t)
	st := f.stream(t, raw{frameOutput, "out"}, raw{frameDir, "/w/sub"}, raw{frameExit, `{"code":2,"timedOut":true}`})
	out, err := io.ReadAll(st)
	res, werr := st.Wait()
	if err != nil || werr != nil || string(out) != "out" || res.Dir != "/w/sub" || res.ExitCode != 2 || !res.TimedOut {
		t.Errorf("stream = %q %+v %v %v", out, res, err, werr)
	}
	for name, frames := range map[string][]raw{
		"a failure":      {{frameFail, `{"kind":"other","message":"machine: wait for the command: x"}`}},
		"a bad failure":  {{frameFail, `{`}},
		"a bad exit":     {{frameExit, `{`}},
		"an odd frame":   {{'?', ""}},
		"no exit at all": {{frameOutput, "partial"}},
	} {
		st := f.stream(t, frames...)
		if _, err := st.Wait(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestTheFileReaderReadsTheHelpersFrames(t *testing.T) {
	f := open(t)
	for name, c := range map[string]struct {
		frames []raw
		tail   string
		want   string
	}{
		"a file":      {[]raw{{frameOutput, "ab"}, {frameOutput, "c"}, {frameExit, "{}"}}, "", ""},
		"a failure":   {[]raw{{frameOutput, "ab"}, {frameFail, `{"kind":"other","message":"read: x"}`}}, "", "machine: read: x"},
		"a bad frame": {[]raw{{frameFail, `{`}}, "", "the helper's answer"},
		"an odd one":  {[]raw{{'?', ""}}, "", "a frame of kind"},
		"a cut one":   {[]raw{{frameOutput, "ab"}}, "o\000", "the read ended early"},
	} {
		sess := f.session(t, emit(c.tail, append([]raw{{frameStart, ""}}, c.frames...)...))
		if err := opened(sess, "open", "/x"); err != nil {
			t.Fatal(err)
		}
		fr := &fileReader{sess: sess}
		b, err := io.ReadAll(fr)
		if cerr := fr.Close(); cerr != nil {
			t.Errorf("%s: close: %v", name, cerr)
		}
		switch {
		case c.want == "" && (err != nil || string(b) != "abc"):
			t.Errorf("%s: %q %v", name, b, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestSmallPieces(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"Linux", "x86_64"}: "linux/amd64", {"Linux", "aarch64"}: "linux/arm64", {"Darwin", "arm64"}: "darwin/arm64",
		{"Linux", "riscv64"}: "linux/riscv64", {"Linux", "amd64"}: "linux/amd64",
	} {
		if goos, goarch := platform(in[0], in[1]); goos+"/"+goarch != want {
			t.Errorf("platform(%q) = %s/%s, want %s", in, goos, goarch, want)
		}
	}
	for in, want := range map[time.Duration]string{time.Second: "1s", 1500 * time.Millisecond: "2s", 15 * time.Minute: "900s"} {
		if got := duration(in); string(got) != want {
			t.Errorf("duration(%s) = %s", in, got)
		}
	}
	if cmpOr("", "") != "" || cmpOr("", "b", "c") != "b" {
		t.Error("cmpOr")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleep = %v", err)
	}
	if _, err := decodeResponse([]byte(strings.Repeat("x", 600))); err == nil || !strings.Contains(err.Error(), "...") {
		t.Errorf("a long answer that is not JSON: %v", err)
	}
	if err := jsonDecode([]byte("{"), &struct{}{}); err == nil {
		t.Error("jsonDecode read a broken payload")
	}
	if under("/w/x", "/w/") != true || under("/wx", "/w") != false {
		t.Error("under")
	}
}

func TestWaitsAndRaces(t *testing.T) {
	t.Run("a create that races another runner's", func(t *testing.T) {
		f := open(t)
		f.stub.Fail(cellastub.OpGet, cellastub.Failure{Status: 404, Code: "not_found"})
		again, err := Open(t.Context(), f.o)
		if err != nil {
			t.Fatal(err)
		}
		if again.Created() || again.Info().ID != f.m.Info().ID {
			t.Errorf("took %s created %v, want the other runner's %s", again.Info().ID, again.Created(), f.m.Info().ID)
		}
	})
	t.Run("a sandbox that fails as it starts", func(t *testing.T) {
		stub := cellastub.New(t)
		o := options(t, stub)
		stub.StartAfter(1 << 20)
		go func() {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if stub.SetFailed(SandboxName(o.Session), "ImagePullBackOff") {
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		if _, err := Open(t.Context(), o); Code(err) != machine.CodeUnavailable || !strings.Contains(err.Error(), "ImagePullBackOff") {
			t.Errorf("a failed start: %v", err)
		}
	})
	t.Run("a sandbox deleted while it starts", func(t *testing.T) {
		stub := cellastub.New(t)
		o := options(t, stub)
		stub.StartAfter(1 << 20)
		go func() {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if stub.Remove(SandboxName(o.Session)) {
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		if _, err := Open(t.Context(), o); Code(err) != machine.CodeUnavailable {
			t.Errorf("a sandbox gone while it started: %v", err)
		}
	})
	t.Run("a read that fails while it waits", func(t *testing.T) {
		stub := cellastub.New(t)
		o := options(t, stub)
		stub.StartAfter(1 << 20)
		stub.Fail(cellastub.OpGet, cellastub.Failure{Status: 404, Code: "not_found"})
		stub.Fail(cellastub.OpGet, cellastub.Failure{Status: 422, Code: "invalid_field"})
		if _, err := Open(t.Context(), o); Code(err) != machine.CodeUnavailable {
			t.Errorf("a read Cella refused: %v", err)
		}
	})
	t.Run("a canceled wait", func(t *testing.T) {
		stub := cellastub.New(t)
		o := options(t, stub)
		stub.StartAfter(1 << 20)
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if _, err := Open(ctx, o); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a wait whose context ended: %v", err)
		}
	})
	t.Run("a start that fails", func(t *testing.T) {
		f := open(t)
		f.stub.Stop(f.m.Info().ID)
		f.stub.Fail(cellastub.OpStart, cellastub.Failure{Status: 422, Code: "quota_exceeded"})
		if err := execErr(t, f.m); Code(err) != machine.CodeUnavailable {
			t.Errorf("a start Cella refused: %v", err)
		}
		f.stub.Fail(cellastub.OpStart, cellastub.Failure{Status: 409, Code: "phase_conflict"})
		if err := execErr(t, f.m); err != nil {
			t.Errorf("a start that raced another: %v", err)
		}
	})
	t.Run("a failed sandbox whose delete is refused", func(t *testing.T) {
		f := open(t)
		f.stub.SetFailed(f.m.Info().ID, "Exited")
		f.stub.Fail(cellastub.OpDelete, cellastub.Failure{Status: 403, Code: "forbidden"})
		if _, err := Open(t.Context(), f.o); Code(err) != machine.CodeUnavailable {
			t.Errorf("a failed sandbox that cannot be deleted: %v", err)
		}
	})
	t.Run("a wake whose read fails", func(t *testing.T) {
		f := open(t)
		f.stub.Stop(f.m.Info().ID)
		f.stub.Fail(cellastub.OpGet, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
		if err := execErr(t, f.m); err == nil {
			t.Error("a wake whose read failed went ahead")
		}
		f.stub.Fail(cellastub.OpGet, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
		if err := f.m.ImportTar(t.Context(), ".", strings.NewReader("")); err == nil {
			t.Error("an import whose read failed went ahead")
		}
		f.stub.Fail(cellastub.OpFiles, cellastub.Failure{Status: 404, Code: "not_found"})
		f.stub.Fail(cellastub.OpGet, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
		if _, err := f.m.Stat(t.Context(), "x"); err == nil {
			t.Error("a stat whose check failed was answered")
		}
	})
	t.Run("a wake another call already did", func(t *testing.T) {
		f := open(t)
		if err := f.m.wakeUp(t.Context(), "sbx_other"); err != nil {
			t.Errorf("a wake for a sandbox the machine no longer uses: %v", err)
		}
	})
}

func TestFetchScriptFailure(t *testing.T) {
	f := open(t)
	if err := os.MkdirAll(f.m.SpillDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.m.SpillDir(), "fetch"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Fetch(t.Context(), machine.FetchRequest{URL: "http://example.com/"}); err == nil || !strings.Contains(err.Error(), "the fetch script exited") {
		t.Errorf("a fetch whose directory cannot be made: %v", err)
	}
	f.stub.Fail(cellastub.OpSession, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
	if _, err := f.m.Fetch(t.Context(), machine.FetchRequest{URL: "http://example.com/"}); err == nil {
		t.Error("a fetch Cella could not start was answered")
	}
}
