// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package toposcli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/dir"
	"latere.ai/x/topos/test/stubs/luxstub"
)

const model = "claude-haiku-4-5"

type fixture struct {
	t     *testing.T
	stub  *luxstub.Server
	vars  map[string]string
	work  string
	stdin string
	sig   chan os.Signal
}

func setup(t *testing.T) *fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, stub: luxstub.New(t), work: filepath.Join(base, "work"), sig: make(chan os.Signal, 2)}
	if err := os.MkdirAll(f.work, 0o755); err != nil {
		t.Fatal(err)
	}
	f.vars = map[string]string{
		"TOPOS_DATA_DIR":   filepath.Join(base, "data"),
		"TOPOS_MODELS_URL": f.stub.URL() + "/anthropic",
		"TOPOS_MODELS_KEY": "sk-test",
		"HOME":             filepath.Join(base, "home"),
		"USER":             "ada",
		"PATH":             os.Getenv("PATH"),
	}
	return f
}

func (f *fixture) run(args ...string) (int, string, string) {
	f.t.Helper()
	var out, errOut bytes.Buffer
	code := Run(f.t.Context(), args, Env{
		Getenv: func(k string) string { return f.vars[k] }, Stdin: strings.NewReader(f.stdin),
		Stdout: &out, Stderr: &errOut, Dir: f.work, Signals: f.sig,
	})
	return code, out.String(), errOut.String()
}

func reply(blocks ...ir.Block) luxstub.Reply {
	stop := ir.StopEndTurn
	for _, b := range blocks {
		if b.Type == ir.BlockToolUse {
			stop = ir.StopToolUse
		}
	}
	return luxstub.Reply{Response: ir.Response{Model: model, Blocks: blocks, StopReason: stop, Usage: ir.Usage{InputTokens: 1000, OutputTokens: 100}}}
}

func text(s string) ir.Block { return ir.Block{Type: ir.BlockText, Text: s} }

func toolUse(id, name, args string) ir.Block {
	return ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: name, Args: json.RawMessage(args)}}
}

func (f *fixture) sessions() []session.Session {
	f.t.Helper()
	st, err := dir.Open(f.vars["TOPOS_DATA_DIR"])
	if err != nil {
		f.t.Fatal(err)
	}
	list, _, err := st.List(f.t.Context(), session.ListOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return list
}

func TestRunATurnInTheWorkingDirectory(t *testing.T) {
	f := setup(t)
	if err := os.WriteFile(filepath.Join(f.work, "notes.txt"), []byte("hello from the working directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.stub.Script(model,
		reply(text("Reading."), toolUse("toolu_1", "read", `{"path":"notes.txt"}`)),
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("The file says hello.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			if tr := last.Blocks[0].ToolResult; tr == nil || !strings.Contains(tr.Blocks[0].Text, "hello from the working directory") {
				return errUnexpected("the read result did not reach the model")
			}
			return nil
		}},
	)
	code, out, errOut := f.run("run", "--model", model, "What does notes.txt say?")
	if code != ExitOK || out != "The file says hello.\n" || !strings.Contains(errOut, "read notes.txt [allow]") {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
	if h := f.stub.Requests()[0].Header.Get("x-api-key"); h != "sk-test" {
		t.Fatalf("the credential %q", h)
	}
	list := f.sessions()
	if len(list) != 1 || list[0].Machine.Workdir != f.work || list[0].Status != session.StatusIdle {
		t.Fatalf("sessions %+v", list)
	}

	f.stub.Script(model, reply(text("Still here.")))
	code, out, _ = f.run("run", "--session", list[0].ID, "--output", "json", "Again?")
	var res map[string]any
	if code != ExitOK || json.Unmarshal([]byte(out), &res) != nil || res["text"] != "Still here." || res["session_id"] != list[0].ID {
		t.Fatalf("json output: exit %d %q", code, out)
	}
}

type errUnexpected string

func (e errUnexpected) Error() string { return string(e) }

func TestAConfirmationRoundTrip(t *testing.T) {
	f := setup(t)
	f.stub.Script(model,
		reply(toolUse("toolu_b", "bash", `{"command":"echo built > out.txt"}`)),
		reply(text("Built.")),
	)
	code, _, errOut := f.run("run", "--model", model, "Build it.")
	if code != ExitWaiting || !strings.Contains(errOut, "topos confirm") || !strings.Contains(errOut, "toolu_b") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	id := f.sessions()[0].ID
	if code, _, _ := f.run("confirm", id, "toolu_b", "maybe"); code != ExitUsage {
		t.Fatalf("a bad decision exits %d", code)
	}
	code, out, errOut := f.run("confirm", id, "toolu_b", "allow", "--remember", "bash(echo *)")
	if code != ExitOK || out != "Built.\n" {
		t.Fatalf("confirm: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	b, err := os.ReadFile(filepath.Join(f.work, "out.txt"))
	if err != nil || string(b) != "built\n" {
		t.Fatalf("the confirmed command did not run: %q, %v", b, err)
	}
}

func TestStreamJSONAndLimits(t *testing.T) {
	f := setup(t)
	f.stub.Script(model, reply(text("One.")))
	code, out, _ := f.run("run", "--model", model, "--output", "stream-json", "Hi.")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != ExitOK || len(lines) < 4 {
		t.Fatalf("stream-json: exit %d, %d lines", code, len(lines))
	}
	for _, l := range lines {
		var e session.Event
		if err := json.Unmarshal([]byte(l), &e); err != nil || e.ID == "" {
			t.Fatalf("a line that is not an event: %q", l)
		}
	}

	g := setup(t)
	g.stub.Script(model, reply(toolUse("toolu_1", "glob", `{"pattern":"*"}`)), reply(text("never")))
	if code, _, errOut := g.run("run", "--model", model, "--max-cost", "0.001", "Spend."); code != ExitLimit || !strings.Contains(errOut, "budget") {
		t.Fatalf("a budget: exit %d, stderr %q", code, errOut)
	}
}

func TestUsageErrors(t *testing.T) {
	f := setup(t)
	for name, c := range map[string]struct {
		args []string
		mut  func()
	}{
		"no command":   {nil, nil},
		"unknown":      {[]string{"launch"}, nil},
		"bad flag":     {[]string{"run", "--nope"}, nil},
		"bad mode":     {[]string{"run", "--mode", "yolo", "x"}, nil},
		"bad output":   {[]string{"run", "--output", "xml", "x"}, nil},
		"bad cost":     {[]string{"run", "--max-cost", "-1", "x"}, nil},
		"no prompt":    {[]string{"run", "--model", model}, nil},
		"no model":     {[]string{"run", "x"}, nil},
		"no url":       {[]string{"run", "--model", model, "x"}, func() { delete(f.vars, "TOPOS_MODELS_URL") }},
		"server":       {[]string{"run", "--model", model, "x"}, func() { f.vars["TOPOS_URL"] = "https://topos.example.com" }},
		"confirm args": {[]string{"confirm", "ses_x"}, nil},
	} {
		if c.mut != nil {
			c.mut()
		}
		if code, _, _ := f.run(c.args...); code != ExitUsage {
			t.Fatalf("%s: exit %d", name, code)
		}
		f.vars["TOPOS_MODELS_URL"] = f.stub.URL() + "/anthropic"
		delete(f.vars, "TOPOS_URL")
	}
	if code, out, _ := f.run("help"); code != ExitOK || !strings.Contains(out, "topos confirm") {
		t.Fatalf("help: %d %q", code, out)
	}
	if code, _, _ := f.run("run", "--model", "no-such-model", "x"); code != ExitError {
		t.Fatalf("an unknown model exits %d", code)
	}
	if code, _, _ := f.run("run", "--session", session.NewID(session.PrefixSession), "--model", model, "x"); code != ExitError {
		t.Fatalf("a missing session exits %d", code)
	}
}

func TestAPromptFromStdin(t *testing.T) {
	f := setup(t)
	f.stdin = "  from stdin \n"
	f.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("got it")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
		if r.Messages[0].Blocks[0].Text != "from stdin" {
			return errUnexpected("the stdin prompt did not arrive")
		}
		return nil
	}})
	if code, out, errOut := f.run("run", "--model", model); code != ExitOK || out != "got it\n" {
		t.Fatalf("exit %d %q %q", code, out, errOut)
	}
}

func TestTwoInterruptsExitAtOnce(t *testing.T) {
	f := setup(t)
	f.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("slow")}, StopReason: ir.StopEndTurn}, Expect: func(*ir.Request) error {
		f.sig <- os.Interrupt
		f.sig <- os.Interrupt
		return nil
	}})
	if code, _, _ := f.run("run", "--model", model, "Go."); code != ExitInterrupted {
		t.Fatalf("exit %d", code)
	}
}

func TestExitCodes(t *testing.T) {
	for reason, want := range map[session.StopReason]int{
		session.StopEndTurn: ExitOK, session.StopCompleted: ExitOK,
		session.StopToolConfirmation: ExitWaiting, session.StopToolResult: ExitWaiting,
		session.StopBudget: ExitLimit, session.StopTurnLimit: ExitLimit, session.StopOutputLimit: ExitLimit,
		session.StopInterrupted: ExitInterrupted, session.StopError: ExitError, session.StopFailed: ExitError,
	} {
		if got := exitCode(outcome(reason)); got != want {
			t.Fatalf("%s exits %d, want %d", reason, got, want)
		}
	}
	for env, want := range map[string]string{"TOPOS_DATA_DIR=/d": "/d", "XDG_STATE_HOME=/x": "/x/topos", "HOME=/h": "/h/.local/state/topos"} {
		k, v, _ := strings.Cut(env, "=")
		got, err := DataDir(func(n string) string {
			if n == k {
				return v
			}
			return ""
		})
		if err != nil || got != want {
			t.Fatalf("DataDir with %s = %q, %v", env, got, err)
		}
	}
	if _, err := DataDir(func(string) string { return "" }); err == nil {
		t.Fatal("a data directory from nothing")
	}
	if summarize(json.RawMessage(`{"command":"`+strings.Repeat("x", 100)+`"}`)) != strings.Repeat("x", 80)+"..." || summarize(json.RawMessage(`[`)) != "" || summarize(json.RawMessage(`{}`)) != "" {
		t.Fatal("summarize")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestLostOutputExitsOne(t *testing.T) {
	code := Run(t.Context(), []string{"version"}, Env{Getenv: func(string) string { return "" }, Stdout: failingWriter{}, Stderr: &bytes.Buffer{}})
	if code != ExitError {
		t.Fatalf("exit %d", code)
	}
}

func outcome(r session.StopReason) harness.Outcome { return harness.Outcome{StopReason: r} }

func TestConfirmPathsAndDenial(t *testing.T) {
	f := setup(t)
	if code, _, _ := f.run("confirm", "ses_x", "t", "allow", "--nope"); code != ExitUsage {
		t.Fatalf("a bad confirm flag exits %d", code)
	}
	if code, _, _ := f.run("confirm", "ses_x", "t", "allow", "--mode", "yolo"); code != ExitUsage {
		t.Fatalf("a bad confirm mode exits %d", code)
	}
	if code, _, _ := f.run("confirm", session.NewID(session.PrefixSession), "t", "allow"); code != ExitError {
		t.Fatalf("confirming a missing session exits %d", code)
	}
	other := filepath.Join(filepath.Dir(f.work), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	delete(f.vars, "USER")
	f.stub.Script(model, reply(toolUse("toolu_x", "bash", `{"command":"echo no > gone.txt"}`)), reply(text("Understood, not running it.")))
	if code, _, _ := f.run("run", "--model", model, "--dir", other, "Do it."); code != ExitWaiting {
		t.Fatalf("exit %d", code)
	}
	s := f.sessions()[0]
	if s.Machine.Workdir != other || s.Initiator.Subject != "local:local" {
		t.Fatalf("session %+v", s)
	}
	code, out, _ := f.run("confirm", s.ID, "toolu_x", "deny", "--note", "not today")
	if code != ExitOK || out != "Understood, not running it.\n" {
		t.Fatalf("deny: exit %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(other, "gone.txt")); !os.IsNotExist(err) {
		t.Fatalf("a denied command ran: %v", err)
	}
	g := setup(t)
	for _, k := range []string{"TOPOS_DATA_DIR", "HOME", "XDG_STATE_HOME"} {
		delete(g.vars, k)
	}
	if code, _, _ := g.run("run", "--model", model, "x"); code != ExitError {
		t.Fatalf("no data directory exits %d", code)
	}
}

func TestAScriptedRun(t *testing.T) {
	f := setup(t)
	path := filepath.Join(f.work, "..", "script.yaml")
	body := "steps:\n  - tool_calls: [{name: glob, input: {pattern: '*.txt'}}]\n  - expect: {tool_result_contains: notes.txt}\n    text: Found notes.txt.\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.work, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.vars["TOPOS_MODELS_URL"] = "scripted:" + path
	code, out, errOut := f.run("run", "--model", "scripted", "Find the text files.")
	if code != ExitOK || out != "Found notes.txt.\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}
