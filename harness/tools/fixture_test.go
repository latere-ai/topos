// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/session"
)

// fixture is a host machine in a temporary directory: the working
// directory, a directory outside every root, a home directory for the
// deny-list, and the spill directory.
type fixture struct {
	h       *host.Host
	work    string
	outside string
	home    string
	spill   string
}

func open(t *testing.T) fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{
		work: filepath.Join(base, "work"), outside: filepath.Join(base, "outside"),
		home: filepath.Join(base, "home"), spill: filepath.Join(base, "spill"),
	}
	for _, d := range []string{f.work, f.outside, filepath.Join(f.home, ".ssh")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h, err := host.Open(host.Options{
		Workdir: f.work, SpillDir: f.spill, Home: f.home, DataDir: filepath.Join(base, "data"), ID: "host-test",
		Environ: []string{"LANG=C", "HOME=" + f.home},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.h = h
	t.Cleanup(func() {
		if err := h.Release(context.Background(), true); err != nil {
			t.Error(err)
		}
	})
	return f
}

// put writes a file on disk, behind the tools' back.
func put(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// get reads a file on disk.
func get(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// builtinTool returns the built-in of that name.
func builtinTool(t *testing.T, name string) Tool {
	t.Helper()
	for _, tool := range Builtins() {
		if tool.Definition().Name == name {
			return tool
		}
	}
	t.Fatalf("no built-in %s", name)
	return nil
}

// run calls a tool and fails the test on a Go error.
func run(ctx context.Context, t *testing.T, tool Tool, m machine.Machine, st State, input string) Result {
	t.Helper()
	res, err := tool.Run(ctx, Call{ID: "toolu_1", Input: json.RawMessage(input), Machine: m, State: st})
	if err != nil {
		t.Fatalf("%s %s: %v", tool.Definition().Name, input, err)
	}
	return res
}

// text is a result's text, its text blocks joined.
func text(r Result) string {
	var parts []string
	for _, b := range r.Content {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "")
}

// mustInput renders a tool input.
func mustInput(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// thread is one thread's log of tool results, folded by StateOf as the
// harness folds it.
type thread struct {
	id     string
	events []session.Event
}

// call runs a tool with the thread's folded state and appends its
// result to the log.
func (th *thread) call(ctx context.Context, t *testing.T, tool Tool, m machine.Machine, input string) Result {
	t.Helper()
	res := run(ctx, t, tool, m, th.state(), input)
	th.record(t, res)
	return res
}

func (th *thread) record(t *testing.T, res Result) {
	t.Helper()
	var meta json.RawMessage
	if res.Meta != nil {
		b, err := json.Marshal(res.Meta)
		if err != nil {
			t.Fatal(err)
		}
		meta = b
	}
	e, err := session.NewEvent(session.TypeToolResult, session.ToolResult{
		ToolUseID: "toolu_1", Content: res.Content, IsError: res.IsError(), Outcome: res.Outcome, Spill: res.Spill, Meta: meta,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.Thread = th.id
	th.events = append(th.events, e)
}

func (th *thread) state() State { return StateOf(th.events, th.id) }

// faulty is a machine whose operations fail as a test asks.
type faulty struct {
	machine.Machine
	statErr, readErr, writeErr, execErr, searchErr error
	// body replaces a read file's content; closeErr fails its Close.
	body     io.Reader
	closeErr error
}

func (f faulty) Stat(ctx context.Context, p string) (machine.FileInfo, error) {
	if f.statErr != nil {
		return machine.FileInfo{}, f.statErr
	}
	return f.Machine.Stat(ctx, p)
}

func (f faulty) ReadFile(ctx context.Context, p string) (io.ReadCloser, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	rc, err := f.Machine.ReadFile(ctx, p)
	if err != nil {
		return nil, err
	}
	if f.body == nil && f.closeErr == nil {
		return rc, nil
	}
	r := io.Reader(rc)
	if f.body != nil {
		r = f.body
	}
	return readCloser{Reader: r, close: func() error { return errors.Join(rc.Close(), f.closeErr) }}, nil
}

func (f faulty) WriteFile(ctx context.Context, p string, r io.Reader, mode fs.FileMode) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	return f.Machine.WriteFile(ctx, p, r, mode)
}

func (f faulty) Exec(ctx context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	if f.execErr != nil {
		return machine.ExecResult{}, f.execErr
	}
	return f.Machine.Exec(ctx, r)
}

func (f faulty) Search(ctx context.Context, q machine.SearchRequest) (machine.SearchResult, error) {
	if f.searchErr != nil {
		return machine.SearchResult{}, f.searchErr
	}
	return f.Machine.Search(ctx, q)
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }

// failingReader fails every read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the disk went away") }
