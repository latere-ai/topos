// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/memory/arca"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

const memoryOwner = "https://issuer.example|ada"

// memoryFolders are a read-only user folder and a writable project folder
// at the files store stub.
func memoryFolders(stub *arcastub.Server) []session.MemoryFolder {
	return []session.MemoryFolder{
		{Name: "user", URL: stub.URL(), Owner: memoryOwner, Path: "files/memory/you", Access: session.MemoryReadOnly},
		{Name: "project", URL: stub.URL(), Owner: memoryOwner, Path: "files/memory/projects/p1", Access: session.MemoryReadWrite},
	}
}

// starts is a machine opened on demand that counts the starts the
// harness asks of it.
type starts struct {
	*machine.Deferred
	n *atomic.Int32
}

func (s starts) Start() { s.n.Add(1); s.Deferred.Start() }

// remembering is a harness whose session has the memory folders over the
// files store stub and the built-in file tools, and whose own machine
// opens on demand, its opens and starts counted.
type remembering struct {
	*env
	files         *arcastub.Server
	opens, starts *atomic.Int32
}

func rememberingSession(t *testing.T, policy Policy) remembering {
	t.Helper()
	r := remembering{files: arcastub.New(t), opens: &atomic.Int32{}, starts: &atomic.Int32{}}
	r.files.KeepTrash()
	r.files.RequireToken(func(tok string) bool { return tok == "session-token" })
	folders := memoryFolders(r.files)
	r.env = setupSession(t, func(c *Config) {
		d := machine.Defer(t.Context(), machine.KindCella, func(context.Context) (machine.Machine, error) {
			r.opens.Add(1)
			return fakeMachine{kind: machine.KindCella}, nil
		})
		c.Machine = starts{Deferred: d, n: r.starts}
		m, err := arca.New(folders, func(context.Context) (string, error) { return "session-token", nil }, r.files.Client(), c.Machine)
		if err != nil {
			t.Fatal(err)
		}
		c.Memory = m
		c.Policy = policy
		for _, tool := range tools.Builtins() {
			switch tool.Definition().Name {
			case tools.NameRead, tools.NameWrite, tools.NameEdit, tools.NameGlob, tools.NameGrep:
				if err := c.Tools.AddBuiltin(tool); err != nil {
					t.Fatal(err)
				}
			}
		}
	}, func(s *session.Session) { s.Memory = folders })
	return r
}

// result is the tool_result of the call id.
func (e *env) result(ctx context.Context, id string) (session.ToolResult, tools.Meta) {
	e.t.Helper()
	for _, ev := range e.events(ctx, session.TypeToolResult) {
		var p session.ToolResult
		if err := ev.Decode(&p); err != nil {
			e.t.Fatal(err)
		}
		if p.ToolUseID != id {
			continue
		}
		var m tools.Meta
		if len(p.Meta) > 0 {
			if err := json.Unmarshal(p.Meta, &m); err != nil {
				e.t.Fatal(err)
			}
		}
		return p, m
	}
	e.t.Fatalf("no result of %s", id)
	return session.ToolResult{}, tools.Meta{}
}

// TestAMemoryCallOpensNoMachine: in a session with memory folders, a
// write, a read, a glob and a grep under /memory/ run on the memory
// machine, the session's machine neither started as their blocks begin
// nor opened; a later call of a tool that acts on the machine opens it as
// before; and a call with a relative path, a path that climbs out of
// /memory, a path missing, or one of several paths outside /memory runs
// on the session's machine (spec 093).
func TestAMemoryCallOpensNoMachine(t *testing.T) {
	ctx := t.Context()
	r := rememberingSession(t, Policy{Mode: ModeConfirm})
	r.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_w", "write", `{"path":"/memory/project/color.md","content":"The club's color is navy.\n"}`)),
		reply(ir.StopToolUse, call("toolu_r", "read", `{"path":"/memory/project/color.md"}`), call("toolu_g", "glob", `{"pattern":"*.md","path":"/memory/project"}`), call("toolu_s", "grep", `{"pattern":"navy","path":"/memory"}`)),
		reply(ir.StopEndTurn, text("Saved.")),
	)
	r.send(ctx, "Remember the club's color.")
	if out := r.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if r.opens.Load() != 0 || r.starts.Load() != 0 {
		t.Fatalf("memory calls opened the machine %d times and started it %d", r.opens.Load(), r.starts.Load())
	}
	if o, ok := r.files.Object(memoryOwner, "files/memory/projects/p1/color.md"); !ok || string(o.Data) != "The club's color is navy.\n" || o.Token != "session-token" {
		t.Fatalf("the store holds %+v %t", o, ok)
	}
	p, meta := r.result(ctx, "toolu_w")
	if p.IsError || meta.Memory == nil || *meta.Memory != (tools.MemoryMeta{Folder: "project", Path: "color.md", Action: tools.MemorySaved}) {
		t.Fatalf("the write: %+v %+v", p, meta)
	}
	for _, id := range []string{"toolu_r", "toolu_g", "toolu_s"} {
		if p, _ := r.result(ctx, id); p.IsError || !strings.Contains(p.Content[0].Text, "color.md") && !strings.Contains(p.Content[0].Text, "navy") {
			t.Fatalf("%s: %+v", id, p)
		}
	}

	r.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_f", "write", `{"path":"/memory/project/color.md","content":""}`)),
		reply(ir.StopToolUse, call("toolu_e", "echo", `{"text":"e"}`)),
		reply(ir.StopEndTurn, text("Done.")),
	)
	r.send(ctx, "Forget it and look.")
	r.turn(ctx)
	if p, meta := r.result(ctx, "toolu_f"); p.Content[0].Text != "Forgot /memory/project/color.md." || meta.Memory.Action != tools.MemoryForgotten {
		t.Fatalf("the forget: %+v %+v", p, meta)
	}
	if r.opens.Load() != 1 || r.starts.Load() != 1 {
		t.Fatalf("a call that acts on the machine opened it %d times and started it %d, want once each", r.opens.Load(), r.starts.Load())
	}

	write, _ := r.h.c.Tools.Get(tools.NameWrite)
	for in, want := range map[string]bool{
		`{"path":"/memory/user/a.md"}`:      true,
		`{"path":"/memory"}`:                true,
		`{"path":"memory/user/a.md"}`:       false,
		`{"path":"/memory/../work/a.md"}`:   false,
		`{"content":"x"}`:                   false,
		`{"path":"/workspace/memory/a.md"}`: false,
	} {
		if got := r.h.onMemory(write, []byte(in)); got != want {
			t.Errorf("onMemory(write %s) = %t", in, got)
		}
	}
	two := paths{fakeTool: &fakeTool{name: "pair"}, of: []string{"/memory/user/a.md", "/work/b.md"}}
	if r.h.onMemory(two, nil) {
		t.Error("a call with a path outside /memory ran on the memory machine")
	}
	if !r.h.onMemory(paths{fakeTool: two.fakeTool, of: []string{"/memory/user/a.md", "/memory/project/b.md"}}, nil) {
		t.Error("a call whose every path is under /memory did not run on the memory machine")
	}
	if r.h.onMemory(paths{fakeTool: two.fakeTool}, nil) || r.h.onMemory(r.echo, []byte(`{"path":"/memory/user/a.md"}`)) {
		t.Error("a call that names no path ran on the memory machine")
	}
	plain := setup(t, nil)
	if plain.h.onMemory(write, []byte(`{"path":"/memory/user/a.md"}`)) {
		t.Error("a session with no memory folders ran a call on a memory machine")
	}
}

// paths is a tool that names the paths of.
type paths struct {
	*fakeTool
	of []string
}

func (p paths) Paths(json.RawMessage) []string { return p.of }

// TestTheAttachmentsBlockNamesMemory: a session with memory folders names
// each one and its access in the attachments block after its apps, every
// request of every turn the same bytes; a session with apps and no memory
// folders renders version 1 as it did; a run of a rollout names none
// (spec 093).
func TestTheAttachmentsBlockNamesMemory(t *testing.T) {
	ctx := t.Context()
	r := rememberingSession(t, Policy{Mode: ModeConfirm})
	r.stub.Script(model, reply(ir.StopEndTurn, text("one")), reply(ir.StopEndTurn, text("two")))
	r.send(ctx, "First.")
	r.turn(ctx)
	r.send(ctx, "Second.")
	r.turn(ctx)
	const want = "<attachments>\n" +
		"Memory folders, kept across sessions. read, write, edit, glob and grep reach them, and so does a command that uses only ls, cat, head, tail, wc, grep, find, sort, uniq, cut, mkdir, rm, mv, cp, touch and the shell's builtins; any other program does not see them. Write a file empty to forget it:\n" +
		"- /memory/user/ (read only)\n" +
		"- /memory/project/ (read and write)\n" +
		"</attachments>"
	texts, _ := r.systemTexts()
	if len(texts) != 2 {
		t.Fatalf("%d requests", len(texts))
	}
	for i, blocks := range texts {
		if !slices.Contains(blocks, want) {
			t.Fatalf("request %d: system %q", i, blocks)
		}
	}

	app := &session.ResourceApp{Slug: "tide", Name: "Tide", URL: "https://tide.apps.example.com"}
	s := session.Session{ID: "ses_1", Resources: []session.Resource{{Type: session.ResourceRepository, URL: "https://git.example.com/r/1.git", App: app, Attached: true}}}
	v1 := headerBlocks(s, true, false, nil)
	s.Memory = memoryFolders(r.files)[1:]
	v2 := headerBlocks(s, true, false, nil)
	apps := "<attachments>\nApps, each a checkout of its source in the working directory, on your branch " + session.Branch(s) + ":\n" +
		`- tide/ : Tide, published at https://tide.apps.example.com (publish with app "tide")` + "\n"
	if len(v1) != 1 || v1[0] != apps+"</attachments>" {
		t.Fatalf("a session with no memory folders: %q", v1)
	}
	if len(v2) != 1 || v2[0] != apps+"Memory folders, kept across sessions. read, write, edit, glob and grep reach them, and so does a command that uses only ls, cat, head, tail, wc, grep, find, sort, uniq, cut, mkdir, rm, mv, cp, touch and the shell's builtins; any other program does not see them. Write a file empty to forget it:\n- /memory/project/ (read and write)\n</attachments>" {
		t.Fatalf("a session with apps and memory folders: %q", v2)
	}
	s.Rollout = "rol_1"
	if got := headerBlocks(s, true, false, nil); len(got) != 1 || strings.Contains(got[0], "/memory/") {
		t.Fatalf("a run of a rollout: %q", got)
	}
}

// TestTheMemoryVerdict: a call whose every path is under /memory/ scores
// with the feature memory at 0.1; it is allowed in confirm and
// progressive as a memory the person can review; in plan a read is
// allowed and a change blocked; always_confirm naming memory or the call
// asks it, and blocks it in plan; a decision service settles nothing of
// it but the rules' verdict; and in a turn the call's use records the
// score and the reason (spec 093).
func TestTheMemoryVerdict(t *testing.T) {
	ctx := t.Context()
	writeProps, readProps := tools.Properties{Effect: tools.EffectWrite}, tools.Properties{Effect: tools.EffectRead}
	in := json.RawMessage(`{"path":"/memory/project/a.md","content":"x"}`)
	for _, tc := range []struct {
		policy  Policy
		props   tools.Properties
		verdict Verdict
		reason  string
	}{
		{Policy{Mode: ModeConfirm}, writeProps, VerdictAllow, ReasonMemory},
		{Policy{Mode: ModeProgressive}, writeProps, VerdictAllow, ReasonMemory},
		{Policy{Mode: ModeConfirm}, readProps, VerdictAllow, ReasonMemory},
		{Policy{Mode: ModePlan}, readProps, VerdictAllow, "read-only"},
		{Policy{Mode: ModePlan}, writeProps, VerdictBlock, ""},
		{Policy{Mode: ModeConfirm, AlwaysConfirm: []string{FeatureMemory}}, readProps, VerdictAsk, "on the organization's always-confirm list"},
		{Policy{Mode: ModeProgressive, AlwaysConfirm: []string{"write(/memory/project/*)"}}, writeProps, VerdictAsk, "on the organization's always-confirm list"},
		{Policy{Mode: ModePlan, AlwaysConfirm: []string{FeatureMemory}}, readProps, VerdictBlock, ""},
	} {
		c := Call{Policy: tc.policy, Name: "write", Props: tc.props, Input: in, MachineKind: machine.KindCella, Memory: true}
		risk, d, err := Rules{}.Decide(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if risk.Score != 0.1 || !slices.Equal(risk.Features, []string{FeatureMemory}) || risk.Source != RiskSource {
			t.Errorf("%+v: risk %+v", tc.policy, risk)
		}
		if d.Verdict != tc.verdict || tc.reason != "" && d.Reason != tc.reason {
			t.Errorf("%+v %s: %+v, want %s %q", tc.policy, tc.props.Effect, d, tc.verdict, tc.reason)
		}
		settled, ok := tc.policy.Settle(c, session.Risk{Score: 0.9})
		if !ok || settled.Verdict != d.Verdict || settled.Verdict == VerdictAsk && settled.ReviewProbability != 1 {
			t.Errorf("%+v: settled %+v %t, want the rules' %s", tc.policy, settled, ok, d.Verdict)
		}
	}
	// On a person's own computer, a write is asked in confirm; a memory
	// write is not, since it reaches no disk of theirs.
	if _, d, err := (Rules{}).Decide(ctx, Call{Policy: Policy{Mode: ModeConfirm}, Name: "write", Props: writeProps, Input: in, MachineKind: machine.KindHost}); err != nil || d.Verdict != VerdictAsk {
		t.Fatalf("a write on the host: %+v %v", d, err)
	}
	if _, d, err := (Rules{}).Decide(ctx, Call{Policy: Policy{Mode: ModeConfirm}, Name: "write", Props: writeProps, Input: in, MachineKind: machine.KindHost, Memory: true}); err != nil || d.Verdict != VerdictAllow {
		t.Fatalf("a memory write beside the host: %+v %v", d, err)
	}

	r := rememberingSession(t, Policy{Mode: ModeConfirm})
	r.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_w", "write", `{"path":"/memory/project/a.md","content":"x"}`)),
		reply(ir.StopEndTurn, text("Saved.")),
	)
	r.send(ctx, "Remember x.")
	r.turn(ctx)
	u := r.uses(ctx)["toolu_w"]
	if u.Verdict != string(VerdictAllow) || u.Reason != ReasonMemory || u.Risk == nil || u.Risk.Score != 0.1 || !slices.Equal(u.Risk.Features, []string{FeatureMemory}) {
		t.Fatalf("the use %+v, risk %+v", u, u.Risk)
	}
	if _, ok := r.files.Object(memoryOwner, "files/memory/projects/p1/a.md"); !ok {
		t.Fatal("an allowed memory write did not land")
	}
}
