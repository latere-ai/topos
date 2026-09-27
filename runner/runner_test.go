// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/dir"
	"latere.ai/x/topos/test/stubs/luxstub"
)

var t0 = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

const model = "builder-model"

type echo struct{}

func (echo) Definition() tools.Definition {
	return tools.Definition{Name: "echo", Description: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (echo) Properties() tools.Properties {
	return tools.Properties{Parallel: true, Effect: tools.EffectRead}
}
func (echo) Run(context.Context, tools.Call) (tools.Result, error) {
	return tools.Text(tools.OutcomeOK, "echoed"), nil
}

type fixture struct {
	t     *testing.T
	stub  *luxstub.Server
	store *dir.Store
	work  string
	r     *Runner
	s     session.Session
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) *fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, stub: luxstub.New(t), work: filepath.Join(base, "work")}
	write(t, filepath.Join(f.work, "AGENTS.md"), "Run make check before a commit.\n")
	write(t, filepath.Join(f.work, "CLAUDE.md"), "Nested rules.\n")
	write(t, filepath.Join(f.work, ".agents", "skills", "release", "SKILL.md"), "---\nname: release\ndescription: Cut a release.\n---\nSteps.\n")
	write(t, filepath.Join(f.work, ".agents", "skills", "broken", "SKILL.md"), "no frontmatter\n")
	personal := filepath.Join(base, "config", "AGENTS.md")
	write(t, personal, "Personal rules.\n")
	write(t, filepath.Join(base, "config", "skills", "notes", "SKILL.md"), "---\nname: notes\ndescription: \"Keep notes.\"\n---\n")
	if f.store, err = dir.Open(filepath.Join(base, "data")); err != nil {
		t.Fatal(err)
	}
	f.r, err = New(Options{
		Store: f.store, ID: "run_local", PersonalInstructions: personal, PersonalSkills: filepath.Join(base, "config", "skills"),
		Clock: func() time.Time { return t0 },
		Harness: func(ctx context.Context, s session.Session) (harness.Config, error) {
			m, err := host.Open(host.Options{Workdir: f.work, SpillDir: filepath.Join(base, "spill", s.ID), Environ: []string{"PATH=" + os.Getenv("PATH")}})
			if err != nil {
				return harness.Config{}, err
			}
			reg := tools.NewRegistry()
			if err := reg.AddBuiltin(echo{}); err != nil {
				return harness.Config{}, err
			}
			return harness.Config{
				Model: &dialect.Model{}, Connection: models.Connection{BaseURL: f.stub.URL() + "/anthropic", Model: model, Family: models.FamilyAnthropic},
				Entry: models.Entry{InputWindow: 100_000, MaxOutputTokens: 8_000}, Machine: m, Tools: reg,
				Clock: func() time.Time { return t0 }, Sleep: func(context.Context, time.Duration) error { return nil },
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s = session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 1},
		session.Sender{Subject: "usr_ada", Name: "Ada", Kind: session.SenderPerson}, session.RunnerExternal, session.Machine{Kind: machine.KindHost}, t0)
	if err := f.store.Create(t.Context(), f.s, nil); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) send(ctx context.Context, typ session.Type, payload any) {
	f.t.Helper()
	e, err := session.NewEvent(typ, payload, t0)
	if err != nil {
		f.t.Fatal(err)
	}
	s, err := f.store.Get(ctx, f.s.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	evs := []session.Event{e}
	session.Stamp(f.s.ID, s.LastSeq, evs)
	if _, err := f.store.Append(ctx, f.s.ID, s.LastSeq, evs); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) message(ctx context.Context, text string) {
	f.t.Helper()
	f.send(ctx, session.TypeUserMessage, session.UserMessage{Sender: f.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: text}}})
}

func (f *fixture) count(ctx context.Context, typ session.Type) int {
	f.t.Helper()
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func reply(blocks ...ir.Block) luxstub.Reply {
	stop := ir.StopEndTurn
	for _, b := range blocks {
		if b.Type == ir.BlockToolUse {
			stop = ir.StopToolUse
		}
	}
	return luxstub.Reply{Response: ir.Response{Model: model, Blocks: blocks, StopReason: stop, Usage: ir.Usage{InputTokens: 10, OutputTokens: 2}}}
}

func TestDriveAttachesTheMachineAndRunsATurn(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	f.stub.Script(model,
		reply(ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_1", Name: "echo", Args: json.RawMessage(`{}`)}}),
		reply(ir.Block{Type: ir.BlockText, Text: "Done."}),
	)
	f.message(ctx, "Echo.")
	out, err := f.r.Drive(ctx, f.s.ID)
	if err != nil || out.StopReason != session.StopEndTurn {
		t.Fatalf("drive %+v, %v", out, err)
	}
	system := f.stub.Requests()[0].Request.System
	var texts []string
	for _, b := range system {
		texts = append(texts, b.Text)
	}
	all := strings.Join(texts, "\n")
	for _, want := range []string{"Working directory: " + f.work, "Personal rules.", "Run make check", "Nested rules.", "name: release", "name: notes", "Machine: host"} {
		if !strings.Contains(all, want) {
			t.Fatalf("the system prompt lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "broken") {
		t.Fatal("a skill with no frontmatter was indexed")
	}
	if strings.Index(all, "Personal rules.") > strings.Index(all, "Run make check") || strings.Index(all, "Run make check") > strings.Index(all, "Nested rules.") {
		t.Fatal("the instruction files are not in order, the person's first and the nearest last")
	}
	s, err := f.store.Get(ctx, f.s.ID)
	if err != nil || s.Status != session.StatusIdle {
		t.Fatalf("header %+v, %v", s, err)
	}
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Again."}))
	f.message(ctx, "Once more.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if n := f.count(ctx, session.TypeSessionMachine); n != 1 {
		t.Fatalf("%d session.machine events; the same machine attaches once", n)
	}
	if n := f.count(ctx, session.TypeSessionStatus); n != 4 {
		t.Fatalf("%d session.status events, want running and idle for each drive", n)
	}
}

func TestDriveContinuesWhileInputIsPending(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	f.stub.Script(model,
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{{Type: ir.BlockText, Text: "one"}}, StopReason: ir.StopEndTurn}, Expect: func(*ir.Request) error {
			f.message(context.Background(), "A second message while the first turn runs.")
			return nil
		}},
		reply(ir.Block{Type: ir.BlockText, Text: "two"}),
	)
	f.message(ctx, "First.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(f.stub.Requests()); n != 2 {
		t.Fatalf("%d requests; the pending message should start a second turn", n)
	}
	s, err := f.store.Get(ctx, f.s.ID)
	if err != nil || s.Turn != 2 {
		t.Fatalf("turn %d, %v", s.Turn, err)
	}
}

func TestDriveRefusesWhatItCannotDrive(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	f.send(ctx, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopCompleted})
	if _, err := f.r.Drive(ctx, f.s.ID); !errors.Is(err, ErrEnded) {
		t.Fatalf("an ended session: %v", err)
	}
	g := setup(t)
	l, err := g.store.Acquire(ctx, g.s.ID, session.Holder{Runner: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.Drive(ctx, g.s.ID); !errors.Is(err, session.ErrLocked) {
		t.Fatalf("a held session: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.Drive(ctx, session.NewID(session.PrefixSession)); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a missing session: %v", err)
	}
	for name, o := range map[string]Options{
		"no store":   {Harness: g.r.o.Harness, ID: "x"},
		"no harness": {Store: g.store, ID: "x"},
		"no id":      {Store: g.store, Harness: g.r.o.Harness},
	} {
		if _, err := New(o); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// gitMachine answers git commands as a repository would.
type gitMachine struct {
	machine.Machine
	answers map[string]string
}

func (g gitMachine) Info() machine.Info {
	return machine.Info{Kind: machine.KindCella, Workdir: "/work/app", OS: "linux", Arch: "arm64", Environment: "gpu"}
}

func (g gitMachine) Exec(ctx context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	out, ok := g.answers[r.Command]
	if !ok {
		return machine.ExecResult{ExitCode: 1}, nil
	}
	return machine.ExecResult{Output: []byte(out + "\n")}, nil
}

func TestTheContextBlock(t *testing.T) {
	m := gitMachine{answers: map[string]string{
		"git rev-parse --abbrev-ref HEAD": "agents/builder/ses_1",
		"git rev-parse --short HEAD":      "1a2b3c4",
		"git status --porcelain":          " M a.go\n M b.go\n?? c.go",
		"git log -5 --format='%h %s'":     "1a2b3c4 parse: reject empty input\n0f0f0f0 init",
		"git rev-parse --show-toplevel":   "/work",
	}}
	top, ok := gitTop(t.Context(), m)
	if !ok || top != "/work" {
		t.Fatalf("top %q %v", top, ok)
	}
	got := contextBlock(t.Context(), m, m.Info(), true, t0)
	want := "<context>\nWorking directory: /work/app\nPlatform: linux/arm64\nMachine: Cella sandbox (gpu)\nDate: 2026-09-27\nGit: branch agents/builder/ses_1 at 1a2b3c4, 2 modified, 1 untracked\nRecent commits:\n- 1a2b3c4 parse: reject empty input\n- 0f0f0f0 init\n</context>"
	if got != want {
		t.Fatalf("context block:\n%s\nwant\n%s", got, want)
	}
}

func TestChainAndSkills(t *testing.T) {
	if got := chain("/work", "/work/a/b"); !slices.Equal(got, []string{"/work", "/work/a", "/work/a/b"}) {
		t.Fatalf("chain %v", got)
	}
	if got := chain("/work", "/work"); !slices.Equal(got, []string{"/work"}) {
		t.Fatalf("chain of the root %v", got)
	}
	if got := chain("/work", "/elsewhere"); !slices.Equal(got, []string{"/elsewhere"}) {
		t.Fatalf("chain outside %v", got)
	}
	for body, ok := range map[string]bool{
		"---\nname: good-one\ndescription: Does a thing.\n---\n":                 true,
		"---\r\nname: crlf\r\ndescription: Windows.\r\n---\r\n":                  true,
		"---\nname: Bad_Name\ndescription: x\n---\n":                             false,
		"---\nname: nodesc\n---\n":                                               false,
		"---\nname: long\ndescription: " + strings.Repeat("x", 1025) + "\n---\n": false,
		"---\nname: open\n": false,
	} {
		if _, got := parseSkill([]byte(body), "/s/SKILL.md"); got != ok {
			t.Fatalf("parseSkill(%q) = %v", body, got)
		}
	}
	var many []session.Skill
	seen := map[string]bool{}
	for i := range maxSkills + 5 {
		many = appendSkills(many, []session.Skill{{Name: "s" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)}}, seen)
	}
	if len(many) != maxSkills {
		t.Fatalf("%d skills indexed, at most %d", len(many), maxSkills)
	}
}

// repoMachine is a Cella-like machine in a repository at /repo, its
// files a map.
type repoMachine struct {
	gitMachine
	files map[string]string
	fail  string
}

func (m repoMachine) Info() machine.Info {
	return machine.Info{Kind: machine.KindCella, ID: "sbx_1", Workdir: "/repo/svc/api", OS: "linux", Arch: "amd64"}
}

func (m repoMachine) ReadFile(ctx context.Context, p string) (io.ReadCloser, error) {
	if p == m.fail {
		return nil, errors.New("input/output error")
	}
	b, ok := m.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(b)), nil
}

func (m repoMachine) List(ctx context.Context, p string) ([]machine.FileInfo, error) {
	if p == m.fail {
		return nil, errors.New("input/output error")
	}
	var out []machine.FileInfo
	seen := map[string]bool{}
	for f := range m.files {
		rest, ok := strings.CutPrefix(f, p+"/")
		if !ok {
			continue
		}
		name, _, isDir := strings.Cut(rest, "/")
		if !seen[name] {
			seen[name] = true
			out = append(out, machine.FileInfo{Path: p + "/" + name, IsDir: isDir})
		}
	}
	if len(out) == 0 {
		return nil, fs.ErrNotExist
	}
	return out, nil
}

func TestAttachInARepository(t *testing.T) {
	st := session.NewMemoryStore()
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent)}, session.Sender{Subject: "u"}, session.RunnerHosted, session.Machine{}, t0)
	if err := st.Create(t.Context(), s, nil); err != nil {
		t.Fatal(err)
	}
	l := NewLog(st, s.ID, 0)
	m := repoMachine{
		answers: map[string]string{"git rev-parse --show-toplevel": "/repo"},
		files: map[string]string{
			"/repo/AGENTS.md":                      strings.Repeat("r", maxInstructionFile+5),
			"/repo/svc/CLAUDE.md":                  "svc rules",
			"/repo/svc/api/AGENTS.md":              "api rules",
			"/repo/.claude/skills/deploy/SKILL.md": "---\nname: deploy\ndescription: Deploy it.\n---\n",
			"/repo/.agents/skills/deploy/SKILL.md": "---\nname: deploy\ndescription: The first source wins.\n---\n",
			"/repo/.agents/skills/deploy/notes.md": "x",
		},
	}
	a, err := attach(t.Context(), m, l, attachOptions{PersonalInstructions: "/nonexistent/AGENTS.md", Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, in := range a.Instructions {
		paths = append(paths, in.Path)
	}
	if !slices.Equal(paths, []string{"/repo/AGENTS.md", "/repo/svc/CLAUDE.md", "/repo/svc/api/AGENTS.md"}) {
		t.Fatalf("instruction files %v; a Cella machine reads no personal file", paths)
	}
	rc, err := st.Blob(t.Context(), s.ID, a.Instructions[0].Blob)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if cerr := rc.Close(); err != nil || cerr != nil {
		t.Fatal(errors.Join(err, cerr))
	}
	if !strings.HasSuffix(string(b), "was cut here]") || len(b) > maxInstructionFile+100 {
		t.Fatalf("a long file was stored at %d bytes", len(b))
	}
	if len(a.Skills) != 1 || a.Skills[0].Description != "The first source wins." {
		t.Fatalf("skills %+v", a.Skills)
	}
	if !strings.Contains(a.Context, "Machine: Cella sandbox") || !strings.Contains(a.Context, "Git: branch") {
		t.Fatalf("context %s", a.Context)
	}

	big := repoMachine{gitMachine: m.gitMachine, files: map[string]string{}}
	for i, d := range []string{"/repo", "/repo/svc", "/repo/svc/api"} {
		for _, n := range instructionNames {
			big.files[d+"/"+n] = strings.Repeat(string(rune('a'+i)), maxInstructionFile-10)
		}
	}
	a, err = attach(t.Context(), big, l, attachOptions{Now: t0})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Instructions) != 5 {
		t.Fatalf("%d files kept past the 256 KiB total", len(a.Instructions))
	}
	for _, fail := range []string{"/repo/svc/CLAUDE.md", "/repo/.agents/skills", "/repo/.claude/skills/deploy/SKILL.md"} {
		m.fail = fail
		if _, err := attach(t.Context(), m, l, attachOptions{Now: t0}); err == nil {
			t.Fatalf("a failing read of %s was skipped", fail)
		}
	}
	if l.Last() != 0 {
		t.Fatalf("attach appended to the log: last %d", l.Last())
	}
}

func TestAMovedMachineIsAHandoff(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "one"}), reply(ir.Block{Type: ir.BlockText, Text: "two"}))
	f.message(ctx, "First.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(filepath.Dir(f.work), "moved")
	if err := os.MkdirAll(moved, 0o755); err != nil {
		t.Fatal(err)
	}
	base := f.r.o.Harness
	f.r.o.Harness = func(ctx context.Context, s session.Session) (harness.Config, error) {
		c, err := base(ctx, s)
		if err != nil {
			return c, err
		}
		if err := c.Machine.Release(ctx, true); err != nil {
			return c, err
		}
		c.Machine, err = host.Open(host.Options{Workdir: moved, SpillDir: filepath.Join(moved, ".spill"), Environ: []string{}})
		return c, err
	}
	f.message(ctx, "Second.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, e := range evs {
		if e.Type == session.TypeSessionMachine {
			var p session.SessionMachine
			if err := e.Decode(&p); err != nil {
				t.Fatal(err)
			}
			reasons = append(reasons, p.Reason)
		}
	}
	if !slices.Equal(reasons, []string{"attached", "handoff"}) {
		t.Fatalf("reasons %v", reasons)
	}
}

func TestDriveReportsAHarnessThatCannotStart(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	f.message(ctx, "Go.")
	f.r.o.Harness = func(context.Context, session.Session) (harness.Config, error) {
		return harness.Config{}, errors.New("no credential")
	}
	if _, err := f.r.Drive(ctx, f.s.ID); err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("a failing configuration: %v", err)
	}
	g := setup(t)
	g.message(ctx, "Go.")
	base := g.r.o.Harness
	g.r.o.Harness = func(ctx context.Context, s session.Session) (harness.Config, error) {
		c, err := base(ctx, s)
		c.Entry = models.Entry{}
		return c, err
	}
	var coded *models.Coded
	if _, err := g.r.Drive(ctx, g.s.ID); !errors.As(err, &coded) || coded.Code != models.CodeUnknown {
		t.Fatalf("an unknown model: %v", err)
	}
}

func TestLocalSkillsReportsAnUnreadableFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	d := t.TempDir()
	write(t, filepath.Join(d, "s", "SKILL.md"), "---\nname: s\ndescription: d\n---\n")
	if err := os.Chmod(filepath.Join(d, "s", "SKILL.md"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(filepath.Join(d, "s", "SKILL.md"), 0o644); err != nil {
			t.Error(err)
		}
	})
	if _, err := localSkills(d); err == nil {
		t.Fatal("an unreadable SKILL.md was skipped")
	}
	if skills, err := localSkills(filepath.Join(d, "absent")); err != nil || skills != nil {
		t.Fatalf("a missing folder: %v %v", skills, err)
	}
}

func TestAnEndOnIdleSessionEndsAndReleasesTheMachine(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Version: 1}, f.s.Initiator, session.RunnerExternal, session.Machine{Kind: machine.KindHost}, t0)
	s.EndOnIdle = true
	if err := f.store.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	f.s = s
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Finished."}))
	f.message(ctx, "One shot.")
	out, err := f.r.Drive(ctx, s.ID)
	if err != nil || out.Status != session.StatusEnded || out.StopReason != session.StopCompleted {
		t.Fatalf("drive %+v, %v", out, err)
	}
	if _, err := f.r.Drive(ctx, s.ID); !errors.Is(err, ErrEnded) {
		t.Fatalf("a second drive of an ended session: %v", err)
	}
	if f.r.o.Kind != KindLocal {
		t.Fatalf("the default kind %q", f.r.o.Kind)
	}
}

func TestLogAppendReportsTheStore(t *testing.T) {
	st := session.NewMemoryStore()
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent)}, session.Sender{Subject: "u"}, session.RunnerHosted, session.Machine{}, t0)
	if err := st.Create(t.Context(), s, nil); err != nil {
		t.Fatal(err)
	}
	l := NewLog(st, s.ID, 0)
	e, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(t.Context(), []session.Event{e}); err != nil || l.Last() != 1 {
		t.Fatalf("append: last %d, %v", l.Last(), err)
	}
	stale := NewLog(st, s.ID, 5)
	e2, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Append(t.Context(), []session.Event{e2}); !errors.Is(err, session.ErrSequenceConflict) {
		t.Fatalf("a log ahead of the store: %v", err)
	}
	if err := st.Delete(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(t.Context(), []session.Event{e2}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("append to a deleted session: %v", err)
	}
}

func TestAttachedSetsThePromptSections(t *testing.T) {
	m, err := session.NewEvent(session.TypeSessionMachine, session.SessionMachine{Context: "<context>\nGit: branch main at abc, 0 modified, 0 untracked\n</context>"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	mem, err := session.NewEvent(session.TypeMemoryAttached, session.MemoryAttached{Name: "notes"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if git, memory := attached([]session.Event{m, mem}); !git || !memory {
		t.Fatalf("git %v memory %v", git, memory)
	}
	plain, err := session.NewEvent(session.TypeSessionMachine, session.SessionMachine{Context: "<context>\n</context>"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if git, memory := attached([]session.Event{m, plain}); git || memory {
		t.Fatal("a later machine outside a repository kept the git section")
	}
}

func TestCheckpointsAndRewind(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	f := setup(t)
	f.r.o.CheckpointDir = filepath.Join(filepath.Dir(f.work), "checkpoints")
	ctx := t.Context()
	notes := filepath.Join(f.work, "notes.txt")
	write(t, notes, "first draft\n")
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "one"}))
	f.message(ctx, "Turn one.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	write(t, notes, "second draft\n")
	write(t, filepath.Join(f.work, "extra.txt"), "added in turn two\n")
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "two"}))
	f.message(ctx, "Turn two.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	cp1, _ := checkpointOf(evs, 1)
	cp2, _ := checkpointOf(evs, 2)
	if cp1 == nil || cp2 == nil || cp1.Commit == cp2.Commit || !strings.HasSuffix(cp2.Ref, f.s.ID+"/2") {
		t.Fatalf("checkpoints %+v %+v", cp1, cp2)
	}
	if n := f.count(ctx, session.TypeSessionError); n != 0 {
		t.Fatalf("%d session errors", n)
	}

	write(t, notes, "unsaved edit\n")
	rw, err := f.r.Rewind(ctx, f.s.ID, 1, f.s.Initiator)
	if err != nil {
		t.Fatal(err)
	}
	if rw.ToTurn != 1 || rw.Checkpoint.Commit != cp1.Commit || rw.Saved == nil || rw.Saved.Commit == cp2.Commit {
		t.Fatalf("rewound %+v; the unsaved edit gets its own saved checkpoint", rw)
	}
	if b, err := os.ReadFile(notes); err != nil || string(b) != "first draft\n" {
		t.Fatalf("notes after rewind %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(f.work, "extra.txt")); !os.IsNotExist(err) {
		t.Fatalf("a file turn one lacked survived the rewind: %v", err)
	}
	f.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{{Type: ir.BlockText, Text: "three"}}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
		for _, m := range r.Messages {
			for _, b := range m.Blocks {
				if strings.Contains(b.Text, "restored to its state at the end of turn 1") {
					return nil
				}
			}
		}
		return errors.New("the model was not told of the rewind")
	}})
	f.message(ctx, "Turn three.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	unchanged, err := f.r.Rewind(ctx, f.s.ID, 3, f.s.Initiator)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Saved == nil || unchanged.Saved.Ref != unchanged.Checkpoint.Ref {
		t.Fatalf("a rewind over an unchanged tree saved %+v", unchanged.Saved)
	}

	var coded *models.Coded
	if _, err := f.r.Rewind(ctx, f.s.ID, 9, f.s.Initiator); !errors.As(err, &coded) || coded.Code != "checkpoint_missing" {
		t.Fatalf("a turn with no checkpoint: %v", err)
	}
	f.send(ctx, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning})
	if _, err := f.r.Rewind(ctx, f.s.ID, 1, f.s.Initiator); !errors.As(err, &coded) || coded.Code != "rewind_not_idle" {
		t.Fatalf("a running session: %v", err)
	}
}

func TestRewindReportsWhatStopsIt(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	if _, err := f.r.Rewind(ctx, session.NewID(session.PrefixSession), 1, f.s.Initiator); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a missing session: %v", err)
	}
	cp := session.CheckpointRef{Ref: "refs/topos/checkpoints/x/1", Commit: strings.Repeat("a", 40)}
	e, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn, Checkpoint: &cp}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.Turn = 1
	s, err := f.store.Get(ctx, f.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	evs := []session.Event{e}
	session.Stamp(f.s.ID, s.LastSeq, evs)
	if _, err := f.store.Append(ctx, f.s.ID, s.LastSeq, evs); err != nil {
		t.Fatal(err)
	}
	f.r.o.Harness = func(context.Context, session.Session) (harness.Config, error) {
		return harness.Config{}, errors.New("no machine today")
	}
	if _, err := f.r.Rewind(ctx, f.s.ID, 1, f.s.Initiator); err == nil || !strings.Contains(err.Error(), "no machine today") {
		t.Fatalf("a failing configuration: %v", err)
	}
	l, err := f.store.Acquire(ctx, f.s.ID, session.Holder{Runner: "other"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := f.r.Rewind(ctx, f.s.ID, 1, f.s.Initiator); !errors.Is(err, session.ErrLocked) {
		t.Fatalf("a held session: %v", err)
	}
}

func TestAFailedCheckpointStillEndsTheTurn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	f := setup(t)
	blocker := filepath.Join(filepath.Dir(f.work), "not-a-dir")
	write(t, blocker, "x")
	f.r.o.CheckpointDir = blocker
	ctx := t.Context()
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "done"}))
	f.message(ctx, "Go.")
	out, err := f.r.Drive(ctx, f.s.ID)
	if err != nil || out.StopReason != session.StopEndTurn {
		t.Fatalf("drive %+v, %v", out, err)
	}
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		var p session.SessionError
		if e.Type == session.TypeSessionError && e.Decode(&p) == nil && p.Code == harness.CodeCheckpointFailed {
			found = true
		}
	}
	if !found {
		t.Fatal("no checkpoint_failed error")
	}
	if cp := lastCheckpointRef(evs); cp != nil {
		t.Fatalf("a checkpoint %+v after a failure", cp)
	}
}
