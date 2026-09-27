// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package toposcli

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestRewindFromTheCommand(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	f := setup(t)
	notes := filepath.Join(f.work, "notes.txt")
	if err := os.WriteFile(notes, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.stub.Script(model, reply(text("first")))
	if code, _, errOut := f.run("run", "--model", model, "One."); code != ExitOK {
		t.Fatalf("exit %d %s", code, errOut)
	}
	id := f.sessions()[0].ID
	if err := os.WriteFile(notes, []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := f.run("rewind", id, "1")
	if code != ExitOK || !strings.Contains(out, "end of turn 1") {
		t.Fatalf("rewind: exit %d %q %q", code, out, errOut)
	}
	if b, err := os.ReadFile(notes); err != nil || string(b) != "one\n" {
		t.Fatalf("notes %q, %v", b, err)
	}
	for _, args := range [][]string{{"rewind", id}, {"rewind", id, "zero"}, {"rewind", id, "0"}} {
		if code, _, _ := f.run(args...); code != ExitUsage {
			t.Fatalf("%v exits %d", args, code)
		}
	}
	if code, _, _ := f.run("rewind", id, "7"); code != ExitError {
		t.Fatalf("a turn with no checkpoint exits %d", code)
	}
}

// manifest writes a file next to the working directory and returns its
// path relative to the working directory.
func (f *fixture) manifest(name, body string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.work), name), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return filepath.Join("..", name)
}

// toolNames are the tools a request offers.
func toolNames(r *ir.Request) string {
	var names []string
	for _, t := range r.Tools {
		names = append(names, t.Name)
	}
	return strings.Join(names, ",")
}

// system is a request's system text.
func system(r *ir.Request) string {
	var parts []string
	for _, b := range r.System {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

// expectAgent checks that a request carries the agent's instructions
// and exactly its tools.
func expectAgent(instructions, tools string) func(*ir.Request) error {
	return func(r *ir.Request) error {
		if !strings.Contains(system(r), instructions) {
			return errUnexpected("the instructions did not reach the model")
		}
		if got := toolNames(r); got != tools {
			return errUnexpected("tools " + got + ", want " + tools)
		}
		return nil
	}
}

const reviewer = `apiVersion: topos.latere.ai/v1
kind: Agent
metadata: {name: reviewer}
spec:
  model: {name: claude-haiku-4-5}
  instructionsFile: reviewer.md
  tools: [read, glob]
  budget: {maxCost: "1.50"}
  limits: {turnTimeout: 30m, maxAge: 48h}
`

func TestRunAnAgentManifest(t *testing.T) {
	f := setup(t)
	path := f.manifest("reviewer.yaml", reviewer)
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.work), "reviewer.md"), []byte("You review changes and report findings.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const instr = "You review changes and report findings."
	f.stub.Script(model, luxstub.Reply{Response: reply(text("Reviewed.")).Response, Expect: expectAgent(instr, "read,glob")})
	code, out, errOut := f.run("run", "--agent", path, "Review it.")
	if code != ExitOK || out != "Reviewed.\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	s := f.sessions()[0]
	if s.Agent.Name != "reviewer" || s.Agent.Version != 1 || !strings.HasPrefix(s.Agent.ID, "agent_") || !s.Agent.Digest.Valid() {
		t.Fatalf("agent %+v", s.Agent)
	}
	if s.Budget.MaxCostUSDMicro == nil || *s.Budget.MaxCostUSDMicro != 1_500_000 || s.Limits.TurnTimeout != "30m" || s.Limits.MaxAge != "48h" || !s.ExpiresAt.Equal(s.CreatedAt.Add(48*time.Hour)) {
		t.Fatalf("budget %v, limits %+v, expires %v", s.Budget.MaxCostUSDMicro, s.Limits, s.ExpiresAt)
	}
	st, err := dir.Open(f.vars["TOPOS_DATA_DIR"])
	if err != nil {
		t.Fatal(err)
	}
	rc, err := st.Blob(t.Context(), s.ID, s.Agent.Digest)
	if err != nil {
		t.Fatalf("the resolved spec is not a blob of the session: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}

	// Continuing the session runs the same agent without the file.
	if err := os.Remove(filepath.Join(filepath.Dir(f.work), "reviewer.yaml")); err != nil {
		t.Fatal(err)
	}
	f.stub.Script(model, luxstub.Reply{Response: reply(text("Still reviewing.")).Response, Expect: expectAgent(instr, "read,glob")})
	if code, out, errOut := f.run("run", "--session", s.ID, "And now?"); code != ExitOK || out != "Still reviewing.\n" {
		t.Fatalf("continue: exit %d, stdout %q, stderr %q", code, out, errOut)
	}

	// A bundle that no longer matches its digest, or is gone, stops the
	// session rather than running another agent.
	var blob string
	hex := s.Agent.Bundle.Hex()
	if err := filepath.WalkDir(f.vars["TOPOS_DATA_DIR"], func(p string, _ fs.DirEntry, err error) error {
		if err == nil && filepath.Base(p) == hex {
			blob = p
		}
		return err
	}); err != nil || blob == "" {
		t.Fatalf("no bundle blob: %v", err)
	}
	if err := os.Chmod(blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte(`{"apiVersion":"topos.latere.ai/v1","kind":"Agent"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The store refuses the tampered body before the runner reads it.
	if code, _, errOut := f.run("run", "--session", s.ID, "Again?"); code != ExitError || !strings.Contains(errOut, "does not match its digest") {
		t.Fatalf("a tampered bundle: exit %d, stderr %q", code, errOut)
	}
	if err := os.Remove(blob); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := f.run("run", "--session", s.ID, "Again?"); code != ExitError {
		t.Fatalf("a missing bundle: exit %d", code)
	}
}

func TestTheManifestModeHoldsAcrossAConfirmation(t *testing.T) {
	f := setup(t)
	path := f.manifest("builder.yaml", `apiVersion: topos.latere.ai/v1
kind: Agent
metadata: {name: builder}
spec:
  model: {name: claude-haiku-4-5}
  instructions: You build things.
  tools: [bash]
  approvals: {mode: plan}
`)
	// In the manifest's plan mode the command is blocked.
	f.stub.Script(model,
		reply(toolUse("toolu_p", "bash", `{"command":"echo built > out.txt"}`)),
		luxstub.Reply{Response: reply(text("Blocked, as planned.")).Response, Expect: expectAgent("You build things.", "bash")},
	)
	if code, out, errOut := f.run("run", "--agent", path, "Build it."); code != ExitOK || out != "Blocked, as planned.\n" || !strings.Contains(errOut, "[block]") {
		t.Fatalf("plan: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	// --mode replaces it for the session: the call asks, and topos
	// confirm continues under the manifest's instructions and tools.
	f.stub.Script(model,
		reply(toolUse("toolu_c", "bash", `{"command":"echo built > out.txt"}`)),
		luxstub.Reply{Response: reply(text("Built.")).Response, Expect: expectAgent("You build things.", "bash")},
	)
	code, _, errOut := f.run("run", "--agent", path, "--mode", "confirm", "Build it.")
	if code != ExitWaiting || !strings.Contains(errOut, "toolu_c") {
		t.Fatalf("confirm mode: exit %d, stderr %q", code, errOut)
	}
	var id string
	for _, s := range f.sessions() {
		if s.Metadata["mode"] == "confirm" {
			id = s.ID
		}
	}
	if code, out, errOut := f.run("confirm", id, "toolu_c", "allow"); code != ExitOK || out != "Built.\n" {
		t.Fatalf("confirm: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if b, err := os.ReadFile(filepath.Join(f.work, "out.txt")); err != nil || string(b) != "built\n" {
		t.Fatalf("out.txt %q, %v", b, err)
	}
}

func TestAnAgentSpawnsItsSubagent(t *testing.T) {
	f := setup(t)
	path := f.manifest("lead.yaml", `apiVersion: topos.latere.ai/v1
kind: Agent
metadata: {name: lead}
spec:
  model: {name: claude-haiku-4-5, effort: low}
  instructions: You lead.
  tools: [read, glob]
  subagents: [{name: tester, agent: tester}]
  threads: {maxDepth: 1, maxConcurrent: 2}
---
apiVersion: topos.latere.ai/v1
kind: Agent
metadata: {name: tester}
spec:
  model: {name: claude-haiku-4-5}
  instructions: You test.
  tools: [read]
`)
	f.stub.Script(model,
		luxstub.Reply{Response: reply(toolUse("toolu_s", "spawn", `{"agent":"tester","task":"Run the tests."}`)).Response, Expect: expectAgent("You lead.", "read,glob,spawn,message")},
		luxstub.Reply{Response: reply(text("All green.")).Response, Expect: expectAgent("You test.", "read")},
		luxstub.Reply{Response: reply(text("The tests pass.")).Response, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			if tr := last.Blocks[0].ToolResult; tr == nil || !strings.Contains(tr.Blocks[0].Text, "All green.") {
				return errUnexpected("the thread's answer did not reach the lead")
			}
			if r.Reasoning == nil || r.Reasoning.Effort != ir.Effort("low") {
				return errUnexpected("the lead's effort did not reach the request")
			}
			return nil
		}},
	)
	code, out, errOut := f.run("run", "--agent", path, "Check the tests.")
	if code != ExitOK || out != "The tests pass.\n" || !strings.Contains(errOut, "spawn") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestTheModelFlagReplacesTheManifestModel(t *testing.T) {
	f := setup(t)
	path := f.manifest("a.yaml", "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata: {name: a}\nspec:\n  model: {name: no-such-model, inputWindow: 1000}\n  instructions: Answer briefly.\n")
	if code, _, errOut := f.run("run", "--agent", path, "Hi."); code != ExitError || !strings.Contains(errOut, "no-such-model") {
		t.Fatalf("the manifest's model: exit %d, stderr %q", code, errOut)
	}
	f.stub.Script(model, luxstub.Reply{Response: reply(text("Hello.")).Response, Expect: expectAgent("Answer briefly.", "read,write,edit,bash,grep,glob,web_fetch,todo")})
	if code, out, errOut := f.run("run", "--agent", path, "--model", model, "Hi."); code != ExitOK || out != "Hello.\n" {
		t.Fatalf("--model: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestTheDefaultAgentManifest(t *testing.T) {
	f := setup(t)
	cfg := filepath.Join(filepath.Dir(f.work), "config")
	f.vars["XDG_CONFIG_HOME"] = cfg
	if err := os.MkdirAll(filepath.Join(cfg, "topos"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata: {name: mine}\nspec:\n  model: {name: " + model + "}\n  instructions: Always answer in one line.\n  tools: [grep]\n"
	if err := os.WriteFile(filepath.Join(cfg, "topos", "agent.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f.stub.Script(model, luxstub.Reply{Response: reply(text("One line.")).Response, Expect: expectAgent("Always answer in one line.", "grep")})
	if code, out, errOut := f.run("run", "Hi."); code != ExitOK || out != "One line.\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if s := f.sessions()[0]; s.Agent.Name != "mine" {
		t.Fatalf("agent %+v", s.Agent)
	}
	for env, want := range map[string]string{"XDG_CONFIG_HOME=/x": "/x/topos", "HOME=/h": "/h/.config/topos", "USERPROFILE=/u": "/u/.config/topos", "": ""} {
		k, v, _ := strings.Cut(env, "=")
		if got := ConfigDir(func(n string) string {
			if n == k {
				return v
			}
			return ""
		}); got != want {
			t.Fatalf("ConfigDir with %q = %q", env, got)
		}
	}
	// No configuration directory is the built-in agent.
	g := setup(t)
	delete(g.vars, "HOME")
	g.vars["TOPOS_DATA_DIR"] = filepath.Join(filepath.Dir(g.work), "data")
	g.stub.Script(model, luxstub.Reply{Response: reply(text("Built in.")).Response, Expect: expectAgent("", "read,write,edit,bash,grep,glob,web_fetch,todo")})
	if code, out, errOut := g.run("run", "--model", model, "Hi."); code != ExitOK || out != "Built in.\n" {
		t.Fatalf("built in: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestAgentManifestRefusals(t *testing.T) {
	f := setup(t)
	agent := func(spec string) string {
		return "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata: {name: a}\nspec:\n  model: {name: " + model + "}\n" + spec
	}
	for name, c := range map[string]struct {
		body, want string
	}{
		"invalid":     {agent("  threads: {maxDepth: 5}\n"), "invalid_manifest"},
		"secret":      {agent("  instructions: use sk-ant-api03-Zq8vXk2Lr9TnB4wYc7HdM1pF\n"), "manifest_holds_secret"},
		"unknown":     {agent("  subagents: [{name: g, agent: ghost}]\n"), "unknown_reference"},
		"hooks":       {agent("  hooks: [{event: turn_end, command: 'true'}]\n"), "does not apply spec.hooks yet"},
		"client tool": {agent("  tools: [{name: ask, client: true, description: Ask., inputSchema: {type: object}}]\n"), "does not apply spec.tools[0] yet"},
		"cella":       {agent("  machine: {kind: cella}\n"), "machine.kind cella"},
		"inline":      {agent("  subagents: [{name: s, spec: {model: {name: m}, advisor: {model: {name: m}}}}]\n"), "spec.subagents[0].spec.advisor"},
		"referenced":  {agent("  subagents: [{name: s, agent: b}]\n") + "---\napiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata: {name: b}\nspec: {model: {name: m}, skills: [{path: /s}]}\n", "b.spec.skills"},
		"no agent":    {"apiVersion: topos.latere.ai/v1\nkind: MemoryStore\nmetadata: {name: n}\nspec: {description: Notes.}\n", "no Agent document"},
	} {
		path := f.manifest(strings.ReplaceAll(name, " ", "-")+".yaml", c.body)
		code, _, errOut := f.run("run", "--agent", path, "x")
		if code != ExitUsage || !strings.Contains(errOut, c.want) {
			t.Errorf("%s: exit %d, stderr %q", name, code, errOut)
		}
	}
	if code, _, errOut := f.run("run", "--agent", "missing.yaml", "x"); code != ExitUsage || !strings.Contains(errOut, "--agent") {
		t.Fatalf("a missing file: exit %d, stderr %q", code, errOut)
	}
	if code, _, _ := f.run("run", "--agent", "a.yaml", "--session", session.NewID(session.PrefixSession), "x"); code != ExitUsage {
		t.Fatalf("--agent with --session: exit %d", code)
	}
	if len(f.sessions()) != 0 {
		t.Fatal("a refused manifest created a session")
	}
}
