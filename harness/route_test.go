// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/memory/arca"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

// commands is the session's machine of a shell test: it runs every
// command it is given with output naming it, ending in the working
// directory, and records each request.
type commands struct {
	fakeMachine
	mu   sync.Mutex
	runs []machine.ExecRequest
}

func (c *commands) Exec(_ context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runs = append(c.runs, r)
	return machine.ExecResult{Output: []byte("ran " + r.Command + "\n"), Dir: "/work"}, nil
}

func (c *commands) SpillDir() string { return "/spill" }

func (c *commands) WriteFile(context.Context, string, io.Reader, fs.FileMode) error { return nil }

// meter records what a turn counted.
type meter struct {
	mu          sync.Mutex
	ran, opened []string
}

func (m *meter) Ran(_ context.Context, where, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ran = append(m.ran, where+":"+reason)
}

func (m *meter) Opened(_ context.Context, tool, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opened = append(m.opened, tool+":"+reason)
}

// lining is a harness whose session has the memory folders over the
// files store stub, the bash tool, and a machine opened on demand that
// runs every command, its opens and starts counted, its log and its
// counts kept.
type lining struct {
	*env
	files         *arcastub.Server
	machine       *commands
	opens, starts *atomic.Int32
	log           *bytes.Buffer
	meter         *meter
}

func liningSession(t *testing.T) lining {
	t.Helper()
	r := lining{files: arcastub.New(t), machine: &commands{kind: machine.KindCella}, opens: &atomic.Int32{}, starts: &atomic.Int32{}, log: &bytes.Buffer{}, meter: &meter{}}
	r.files.RequireToken(func(tok string) bool { return tok == "session-token" })
	r.files.Put(memoryOwner, "files/memory/you/nikita-heumann-your-latere-ai-cofounder.md", arcastub.Object{Data: []byte("# Nikita\n\nCofounder.\n")})
	folders := memoryFolders(r.files)
	r.env = setupSession(t, func(c *Config) {
		d := machine.Defer(t.Context(), machine.KindCella, func(context.Context) (machine.Machine, error) {
			r.opens.Add(1)
			return r.machine, nil
		})
		c.Machine = starts{Deferred: d, n: r.starts}
		m, err := arca.New(folders, func(context.Context) (string, error) { return "session-token", nil }, r.files.Client(), c.Machine)
		if err != nil {
			t.Fatal(err)
		}
		c.Memory = m
		c.Policy = Policy{Mode: ModeConfirm, AlwaysAllow: []string{"bash"}}
		c.Log = slog.New(slog.NewJSONHandler(r.log, nil))
		c.Meter = r.meter
		reg := tools.NewRegistry()
		for _, tool := range tools.Builtins() {
			if tool.Definition().Name == tools.NameBash {
				if err := reg.AddBuiltin(tool); err != nil {
					t.Fatal(err)
				}
			}
		}
		c.Tools = reg
	}, func(s *session.Session) { s.Memory = folders })
	return r
}

// text is the text of a call's result.
func (r lining) text(ctx context.Context, id string) (string, tools.Meta) {
	r.t.Helper()
	p, meta := r.result(ctx, id)
	if p.IsError {
		r.t.Fatalf("%s failed: %+v", id, p)
	}
	return p.Content[0].Text, meta
}

// TestABashCallOverMemoryOpensNoMachine: in a session with memory
// folders, a bash call whose line reaches the folders alone runs in the
// process, as a machine's shell answers it, and neither starts nor opens
// the session's machine; the line's directory is kept for the next call,
// which runs in the process from it; a call that runs a program opens
// the machine, in its working directory with a note, and the log line
// and the counts name it (spec 097).
func TestABashCallOverMemoryOpensNoMachine(t *testing.T) {
	ctx := t.Context()
	r := liningSession(t)
	r.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", "bash", `{"command":"cd /memory/user && ls && cat nikita-heumann-your-latere-ai-cofounder.md"}`)),
		reply(ir.StopToolUse, call("toolu_2", "bash", `{"command":"wc -l nikita-heumann-your-latere-ai-cofounder.md"}`)),
		reply(ir.StopEndTurn, text("Nikita is the cofounder.")),
	)
	r.send(ctx, "Who is Nikita?")
	if out := r.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if r.opens.Load() != 0 || r.starts.Load() != 0 {
		t.Fatalf("lines over memory opened the machine %d times and started it %d", r.opens.Load(), r.starts.Load())
	}
	got, meta := r.text(ctx, "toolu_1")
	if want := "nikita-heumann-your-latere-ai-cofounder.md\n# Nikita\n\nCofounder.\nExit code: 0"; got != want || meta.Dir != "/memory/user" || meta.ExitCode == nil || *meta.ExitCode != 0 {
		t.Fatalf("the production line: %q, dir %q", got, meta.Dir)
	}
	if got, meta := r.text(ctx, "toolu_2"); !strings.HasPrefix(got, "3 nikita-heumann-your-latere-ai-cofounder.md\n") || meta.Dir != "/memory/user" {
		t.Fatalf("a relative path after cd: %q, dir %q", got, meta.Dir)
	}

	r.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_3", "bash", `{"command":"cat nikita-heumann-your-latere-ai-cofounder.md | python3 summarize.py"}`)),
		reply(ir.StopToolUse, call("toolu_4", "bash", `{"command":"cat /memory/user/nikita-heumann-your-latere-ai-cofounder.md | head -1"}`)),
		reply(ir.StopEndTurn, text("Summarized.")),
	)
	r.send(ctx, "Summarize it.")
	r.turn(ctx)
	if r.opens.Load() != 1 {
		t.Fatalf("a call that runs a program opened the machine %d times", r.opens.Load())
	}
	got, meta = r.text(ctx, "toolu_3")
	if !strings.HasPrefix(got, "[/memory/user is in a memory folder, which the programs of this command do not see; the command ran in the working directory /work]\nran cat") || meta.Dir != "/work" {
		t.Fatalf("a program from a memory folder: %q, dir %q", got, meta.Dir)
	}
	if len(r.machine.runs) != 1 || r.machine.runs[0].Dir != "" {
		t.Fatalf("the machine ran %+v", r.machine.runs)
	}
	if got, _ := r.text(ctx, "toolu_4"); got != "# Nikita\nExit code: 0" || len(r.machine.runs) != 1 {
		t.Fatalf("a line over memory once the machine is open: %q, %d runs", got, len(r.machine.runs))
	}

	var line map[string]any
	for l := range strings.Lines(r.log.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		if m["msg"] == "a tool call opened the session's machine" {
			if line != nil {
				t.Fatalf("the open was logged twice: %s", r.log)
			}
			line = m
		}
	}
	if line == nil || line["tool"] != "bash" || line["tool_use_id"] != "toolu_3" || line["reason"] != "program" || line["program"] != "python3" || line["session"] != r.s.ID {
		t.Fatalf("the open's line: %v", line)
	}
	if want := []string{"process:", "process:", "machine:program", "process:"}; !slices.Equal(r.meter.ran, want) {
		t.Fatalf("the runs counted %v, want %v", r.meter.ran, want)
	}
	if want := []string{"bash:program"}; !slices.Equal(r.meter.opened, want) {
		t.Fatalf("the opens counted %v, want %v", r.meter.opened, want)
	}
}

// TestTheRouteOfABashCall: a bash call runs on the memory machine when
// its line reaches the folders alone from the directory it starts in,
// and on the session's machine when it runs in the background, names a
// program or a path the folders do not hold, or cannot be decoded; a
// session whose memory machine runs no lines asks nothing; and a bash
// call's verdict stays a command's (spec 097).
func TestTheRouteOfABashCall(t *testing.T) {
	r := liningSession(t)
	bash, _ := r.h.c.Tools.Get(tools.NameBash)
	for _, c := range []struct {
		input, dir string
		memory     bool
		reason     string
	}{
		{`{"command":"cd /memory/user && ls"}`, "", true, ""},
		{`{"command":"ls"}`, "/memory/user", true, ""},
		{`{"command":"ls"}`, "/work", false, "path"},
		{`{"command":"ls /memory/user","background":true}`, "", false, ReasonBackground},
		{`{"command":"cat $(ls /memory/user)"}`, "", false, "expansion"},
		{`{"command":"echo x > /memory/user/a.md"}`, "", true, ""},
		{`{"command":"cat /memory/user/a.md | python3"}`, "", false, "program"},
		{`{"command":`, "", false, ""},
	} {
		got := r.h.route(bash, []byte(c.input), c.dir)
		if got.memory != c.memory || got.reason != c.reason {
			t.Errorf("route(%s, %q) = memory %t reason %q", c.input, c.dir, got.memory, got.reason)
		}
	}
	if r.h.onMemory(bash, []byte(`{"command":"ls /memory"}`)) {
		t.Error("a bash call took the memory verdict")
	}
	plain := setup(t, nil)
	if got := plain.h.route(bash, []byte(`{"command":"ls /memory"}`), ""); got.memory || got.asked {
		t.Errorf("a session with no memory machine: %+v", got)
	}
	nolines := setupSession(t, func(c *Config) { c.Memory = fakeMachine{kind: "memory"} }, nil)
	if got := nolines.h.route(bash, []byte(`{"command":"ls /memory"}`), ""); got.memory || got.asked || nolines.h.runsLines() {
		t.Errorf("a memory machine that runs no lines: %+v", got)
	}
}

// TestALongLineOverMemorySpills: a line over the memory folders whose
// output passes the result's bound runs in the process and writes the
// whole output to a file on the session's machine, which it opens for
// that alone, and the open is recorded as the spill's (spec 097).
func TestALongLineOverMemorySpills(t *testing.T) {
	ctx := t.Context()
	r := liningSession(t)
	r.files.Put(memoryOwner, "files/memory/you/long.md", arcastub.Object{Data: []byte(strings.Repeat("a line of a long note\n", 4000))})
	r.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", "bash", `{"command":"cat /memory/user/long.md"}`)),
		reply(ir.StopEndTurn, text("Long.")),
	)
	r.send(ctx, "Read the long note.")
	r.turn(ctx)
	if r.opens.Load() != 1 || r.starts.Load() != 0 || len(r.machine.runs) != 0 {
		t.Fatalf("a long line opened the machine %d times, started it %d, and ran %d commands there", r.opens.Load(), r.starts.Load(), len(r.machine.runs))
	}
	if got, _ := r.text(ctx, "toolu_1"); !strings.Contains(got, "/spill/tool-") {
		t.Fatalf("the long line's result: %.200q", got)
	}
	if want := []string{"bash:" + ReasonSpill}; !slices.Equal(r.meter.opened, want) {
		t.Fatalf("the opens counted %v, want %v", r.meter.opened, want)
	}
	if !strings.Contains(r.log.String(), `"reason":"spill"`) {
		t.Fatalf("the open's line: %s", r.log)
	}
}
