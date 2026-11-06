// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// builtin is the built-in tool of the name.
func builtin(t *testing.T, name string) tools.Tool {
	t.Helper()
	for _, b := range tools.Builtins() {
		if b.Definition().Name == name {
			return b
		}
	}
	t.Fatalf("no built-in %s", name)
	return nil
}

// withTools adds tools to a test's registry.
func withTools(t *testing.T, add ...tools.Tool) func(*Config) {
	return func(c *Config) {
		for _, tool := range add {
			if err := c.Tools.AddBuiltin(tool); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// keepsCheckpoints gives a harness a checkpoint function, as a runner
// does for a machine that keeps them.
func keepsCheckpoints(c *Config) {
	c.Checkpoint = func(context.Context, int, string) (*session.CheckpointRef, error) { return nil, nil }
}

// scoped is a decider whose decision carries the scope a decision
// service read, as the decision service's client records it.
type scoped struct{ scope session.CallScope }

func (s scoped) Decide(ctx context.Context, c Call) (session.Risk, Decision, error) {
	risk, d, err := Rules{}.Decide(ctx, c)
	d.Scope = &s.scope
	return risk, d, err
}

// TestToolUseRecordsScope (spec 101): a call's agent.tool_use records the
// reach it was decided on before it waits: Topos's own claims with source
// topos without a decision service, a bash call no member but its source,
// and the decider's scope where it gives one.
func TestToolUseRecordsScope(t *testing.T) {
	ctx := t.Context()
	e := setup(t, func(c *Config) {
		c.Machine = fakeMachine{kind: machine.KindHost}
		keepsCheckpoints(c)
		withTools(t, builtin(t, tools.NameWrite))(c)
	})
	e.stub.Script(model, reply(ir.StopToolUse,
		call("toolu_in", "write", `{"path":"notes.md","content":"x"}`),
		call("toolu_out", "write", `{"path":"/etc/hosts","content":"x"}`),
		call("toolu_sh", "bash", `{"command":"make"}`),
	))
	e.send(ctx, "Write.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	uses := e.uses(ctx)
	want := map[string]session.CallScope{
		"toolu_in": {Undo: session.UndoReversible, Crosses: &session.Crosses{Machine: new(false), Remote: new(false)},
			TargetClass: session.TargetShared, TargetKind: "path", Source: session.ScopeFromTopos},
		"toolu_out": {Crosses: &session.Crosses{Machine: new(true), Remote: new(false)}, TargetClass: session.TargetShared, TargetKind: "path", Source: session.ScopeFromTopos},
		"toolu_sh":  {Source: session.ScopeFromTopos},
	}
	for id, w := range want {
		if u := uses[id]; u.Verdict != "ask" || u.Scope == nil || !reflect.DeepEqual(*u.Scope, w) {
			t.Errorf("%s: %s %+v, want %+v", id, u.Verdict, u.Scope, w)
		}
	}

	read := scoped{scope: session.CallScope{Undo: session.UndoIrreversible, TargetClass: session.TargetPrimary, TargetKind: "ref", Source: session.ScopeFromService}}
	s := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost}; c.Decider = read })
	s.stub.Script(model, reply(ir.StopToolUse, call("toolu_p", "bash", `{"command":"git push --force origin main"}`)))
	s.send(ctx, "Push.")
	if out := s.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	if u := s.uses(ctx)["toolu_p"]; u.Scope == nil || !reflect.DeepEqual(*u.Scope, read.scope) {
		t.Fatalf("the decider's scope: %+v", u.Scope)
	}
}

// TestRecordedScopeHoldsNoName (spec 101): no member of a recorded scope
// holds a path, a host or a ref a call's input names.
func TestRecordedScopeHoldsNoName(t *testing.T) {
	ctx := t.Context()
	e := setup(t, func(c *Config) {
		c.Machine = fakeMachine{kind: machine.KindHost}
		withTools(t, builtin(t, tools.NameWrite), builtin(t, tools.NameWebFetch))(c)
	})
	e.stub.Script(model, reply(ir.StopToolUse,
		call("toolu_1", "write", `{"path":"/work/zzmarker/zzmarker.txt","content":"zzmarker"}`),
		call("toolu_2", "web_fetch", `{"url":"https://zzmarker.example.com/zzmarker"}`),
		call("toolu_3", "bash", `{"command":"git push origin zzmarker"}`),
	))
	e.send(ctx, "Go.")
	e.turn(ctx)
	for id, u := range e.uses(ctx) {
		b, err := json.Marshal(u.Scope)
		if err != nil || u.Scope == nil || strings.Contains(string(b), "zzmarker") {
			t.Errorf("%s records %s (%v)", id, b, err)
		}
	}
}

// refusing is a harness on a Cella machine in progressive mode whose
// thresholds block every call of ext, an external tool, and let bash, a
// write, run below the flag threshold.
func refusing(t *testing.T, mut func(*Config)) (*env, *fakeTool) {
	ext := &fakeTool{name: "ext", props: tools.Properties{Effect: tools.EffectExternal}}
	e := setup(t, func(c *Config) {
		c.Policy = Policy{Mode: ModeProgressive, Thresholds: Thresholds{FlagAt: 0.3, AskAt: 0.5, BlockAt: 0.55}}
		withTools(t, ext)(c)
		if mut != nil {
			mut(c)
		}
	})
	return e, ext
}

// extCall is a call of ext, each with its own input so that no two
// repeat.
func extCall(id string) ir.Block { return call(id, "ext", `{"text":"`+id+`"}`) }

// reached are the session.denials_reached of the log.
func (e *env) reached(ctx context.Context) []session.DenialsReached {
	e.t.Helper()
	var out []session.DenialsReached
	for _, ev := range e.events(ctx, session.TypeDenialsReached) {
		var p session.DenialsReached
		if err := ev.Decode(&p); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// TestRefusalsAreCounted (spec 101): three refusals in a row append
// session.denials_reached after the third; a read between them neither
// counts nor ends the run, a change that runs ends it, and plan mode's
// blocks are no refusals.
func TestRefusalsAreCounted(t *testing.T) {
	ctx := t.Context()
	e, _ := refusing(t, nil)
	e.stub.Script(model,
		reply(ir.StopToolUse, extCall("toolu_1"), call("toolu_r", "echo", `{"text":"r"}`), extCall("toolu_2")),
		reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"one"}`)),
		reply(ir.StopToolUse, extCall("toolu_3"), extCall("toolu_4")),
		reply(ir.StopEndTurn, text("Stopped.")),
	)
	e.send(ctx, "Try.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if got := e.reached(ctx); len(got) != 0 {
		t.Fatalf("a change between them ended the run, yet %+v", got)
	}
	if r := session.RefusalsOf(e.all()); r.Consecutive != 2 || r.Total != 4 {
		t.Fatalf("counts %+v", r)
	}
	e.stub.Script(model,
		reply(ir.StopToolUse, extCall("toolu_5"), call("toolu_r2", "echo", `{"text":"r"}`)),
		reply(ir.StopEndTurn, text("Stopped again.")),
	)
	e.send(ctx, "Again.")
	e.turn(ctx)
	got := e.reached(ctx)
	if len(got) != 1 || got[0] != (session.DenialsReached{Consecutive: 3, Total: 5, Limit: session.LimitConsecutive, Effect: session.DenialsAsk}) {
		t.Fatalf("denials_reached %+v", got)
	}
	// It follows the result of the refusal that reached the limit.
	evs := e.all()
	at := slices.IndexFunc(evs, func(ev session.Event) bool { return ev.Type == session.TypeDenialsReached })
	if r := evs[at-1]; r.Type != session.TypeToolResult || !strings.Contains(string(r.Payload), "toolu_5") {
		t.Fatalf("denials_reached follows %s %s", r.Type, r.Payload)
	}

	p := setup(t, func(c *Config) { c.Policy.Mode = ModePlan })
	p.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", "bash", `{"command":"a"}`), call("toolu_2", "bash", `{"command":"b"}`), call("toolu_3", "bash", `{"command":"c"}`)),
		reply(ir.StopEndTurn, text("Planned.")),
	)
	p.send(ctx, "Plan.")
	p.turn(ctx)
	if got := p.reached(ctx); len(got) != 0 || session.RefusalsOf(p.all()).Total != 0 {
		t.Fatalf("plan mode's blocks counted: %+v", got)
	}
}

// TestTheFallbackAsksForEveryChange (spec 101): once the refusals reach
// a limit, every call that changes something asks with decided_by
// denials and review probability 1, whatever the score, an always_allow
// pattern or the sandbox rule would decide; a call the rules block stays
// blocked, a read runs, and the person's next message ends the fallback.
func TestTheFallbackAsksForEveryChange(t *testing.T) {
	ctx := t.Context()
	e, _ := refusing(t, func(c *Config) { c.Denials = &session.DenialLimits{Consecutive: 2, Total: 20} })
	e.stub.Script(model,
		reply(ir.StopToolUse, extCall("toolu_1"), extCall("toolu_2")),
		reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"one"}`), extCall("toolu_3"), call("toolu_r", "echo", `{"text":"r"}`)),
	)
	e.send(ctx, "Try.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	uses := e.uses(ctx)
	if u := uses["toolu_w"]; u.Verdict != "ask" || u.DecidedBy != session.DecidedDenials || u.ReviewProbability == nil || *u.ReviewProbability != 1 || u.Reason != ReasonDenials {
		t.Fatalf("a change in the fallback: %+v", u)
	}
	if u := uses["toolu_3"]; u.Verdict != "block" || u.DecidedBy != session.DecidedScore {
		t.Fatalf("a blocked call in the fallback: %+v", u)
	}
	if u := uses["toolu_r"]; u.Verdict != "allow" || u.DecidedBy == session.DecidedDenials {
		t.Fatalf("a read in the fallback: %+v", u)
	}
	// The person writes in place of an answer: the fallback ends.
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_w2", "bash", `{"command":"two"}`)),
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.send(ctx, "Go on.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("after the message %+v", out)
	}
	if u := e.uses(ctx)["toolu_w2"]; u.Verdict != "allow" || u.DecidedBy != session.DecidedScore {
		t.Fatalf("after the fallback: %+v", u)
	}

	// confirm on a Cella machine: always_allow and the sandbox rule ask.
	c := setup(t, func(c *Config) { c.Policy.AlwaysAllow = []string{"bash(ls*)"} })
	reached, err := session.NewEvent(session.TypeDenialsReached, session.DenialsReached{Consecutive: 3, Total: 3, Limit: session.LimitConsecutive, Effect: session.DenialsAsk}, t0)
	if err != nil {
		t.Fatal(err)
	}
	c.send(ctx, "Start.")
	c.appendEvents(ctx, reached)
	c.stub.Script(model, reply(ir.StopToolUse, call("toolu_ls", "bash", `{"command":"ls"}`), call("toolu_mk", "bash", `{"command":"make"}`)))
	if out := c.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("confirm outcome %+v", out)
	}
	for id, u := range c.uses(ctx) {
		if u.Verdict != "ask" || u.DecidedBy != session.DecidedDenials {
			t.Errorf("%s in confirm's fallback: %+v", id, u)
		}
	}
}

// TestAnUnattendedTurnStopsOnRefusals (spec 101): a trigger's turn whose
// refusals reach a limit records the stop and ends with the stop reason
// denials once the step's calls are answered, and the next turn a person
// begins asks for every change until that person's next message.
func TestAnUnattendedTurnStopsOnRefusals(t *testing.T) {
	ctx := t.Context()
	e, _ := refusing(t, nil)
	e.stub.Script(model,
		reply(ir.StopToolUse, extCall("toolu_1"), extCall("toolu_2"), extCall("toolu_3"), call("toolu_r", "echo", `{"text":"r"}`)),
	)
	e.fire(ctx, false)
	out := e.turn(ctx)
	if out.StopReason != session.StopDenials {
		t.Fatalf("outcome %+v", out)
	}
	if got := e.reached(ctx); len(got) != 1 || got[0].Effect != session.DenialsStop {
		t.Fatalf("denials_reached %+v", got)
	}
	if ran := e.echo.ran(); len(ran) != 1 {
		t.Fatalf("the step's read ran %v", ran)
	}
	// The person's turn: a change asks; the person's next message ends it.
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"one"}`)),
		reply(ir.StopToolUse, call("toolu_w2", "bash", `{"command":"two"}`)),
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.send(ctx, "What happened?")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("the person's turn %+v", out)
	}
	if u := e.uses(ctx)["toolu_w"]; u.DecidedBy != session.DecidedDenials {
		t.Fatalf("the person's turn decided %+v", u)
	}
	e.send(ctx, "Go on.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("after the next message %+v", out)
	}
	if u := e.uses(ctx)["toolu_w2"]; u.Verdict != "allow" {
		t.Fatalf("after the fallback %+v", u)
	}

	// A rollout's run has no person to ask either: it stops.
	ext := &fakeTool{name: "ext", props: tools.Properties{Effect: tools.EffectExternal}}
	r := setupSession(t, func(c *Config) {
		c.Policy = Policy{Mode: ModeProgressive, Thresholds: Thresholds{FlagAt: 0.3, AskAt: 0.5, BlockAt: 0.55}}
		withTools(t, ext)(c)
	}, func(s *session.Session) { s.Rollout = "rol_1" })
	r.stub.Script(model, reply(ir.StopToolUse, extCall("toolu_1"), extCall("toolu_2"), extCall("toolu_3")))
	r.send(ctx, "Run.")
	if out := r.turn(ctx); out.StopReason != session.StopDenials {
		t.Fatalf("a rollout's run %+v", out)
	}
	if got := r.reached(ctx); len(got) != 1 || got[0].Effect != session.DenialsStop {
		t.Fatalf("a rollout's denials_reached %+v", got)
	}
}

// TestSubagentsShareTheCounts (spec 101): a subagent's refusals count
// toward the session's and its own calls then ask; a subagent started in
// the fallback runs in it; and a fork reads the same counts from the log
// it copied.
func TestSubagentsShareTheCounts(t *testing.T) {
	ctx := t.Context()
	ext := &fakeTool{name: "ext", props: tools.Properties{Effect: tools.EffectExternal}}
	e := setup(t, func(c *Config) {
		c.Policy = Policy{Mode: ModeProgressive, Thresholds: Thresholds{FlagAt: 0.3, AskAt: 0.5, BlockAt: 0.55}}
		withTools(t, ext)(c)
		c.Denials = &session.DenialLimits{Consecutive: 2, Total: 20}
		withReviewer(func(s *Subagent) { s.Tools = []string{"echo", "ext", "bash"} })(c)
	})
	e.stub.Script(model,
		reply(ir.StopToolUse, extCall("toolu_p")),
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"subagent":"reviewer","task":"Review."}`)),
	)
	e.stub.Script(reviewerModel,
		reply(ir.StopToolUse, extCall("toolu_c")),
		reply(ir.StopToolUse, call("toolu_cw", "bash", `{"command":"x"}`)),
	)
	e.send(ctx, "Work.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	got := e.reached(ctx)
	if len(got) != 1 || got[0].Consecutive != 2 {
		t.Fatalf("the parent's refusal and the subagent's: %+v", got)
	}
	if u := e.uses(ctx)["toolu_cw"]; u.DecidedBy != session.DecidedDenials {
		t.Fatalf("the subagent's change: %+v", u)
	}

	// A subagent started in the fallback runs in it.
	f := setup(t, withReviewer(func(s *Subagent) { s.Tools = []string{"echo", "bash"} }))
	reached, err := session.NewEvent(session.TypeDenialsReached, session.DenialsReached{Consecutive: 3, Total: 3, Limit: session.LimitConsecutive, Effect: session.DenialsAsk}, t0)
	if err != nil {
		t.Fatal(err)
	}
	f.send(ctx, "Start.")
	f.appendEvents(ctx, reached)
	f.stub.Script(model, reply(ir.StopToolUse, spawnCall("toolu_s", `{"subagent":"reviewer","task":"Review."}`)))
	f.stub.Script(reviewerModel, reply(ir.StopToolUse, call("toolu_cw", "bash", `{"command":"x"}`)))
	if out := f.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("a subagent in the fallback: %+v", out)
	}
	if u := f.uses(ctx)["toolu_cw"]; u.DecidedBy != session.DecidedDenials {
		t.Fatalf("a subagent in the fallback decided %+v", u)
	}

	// A fork reads the counts from the log it copied, as any runner that
	// takes the session over does.
	evs := e.all()
	child := session.New(e.s.Agent, e.s.Initiator, session.RunnerHosted, e.s.Machine, t0)
	if _, err := session.Fork(ctx, e.store, child, nil, e.s, evs); err != nil {
		t.Fatal(err)
	}
	copied, err := e.store.Events(ctx, child.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := session.RefusalsOf(evs), session.RefusalsOf(copied); a != b || !b.Fallback || b.Consecutive != 2 {
		t.Fatalf("a fork's counts %+v, the session's %+v", b, a)
	}
}

// TestARefusedSubagentWarnsItsCaller (spec 101): a subagent whose run had
// a refusal returns its result after the fixed warning, with refused in
// the result's meta; one without returns as before.
func TestARefusedSubagentWarnsItsCaller(t *testing.T) {
	ctx := t.Context()
	ext := &fakeTool{name: "ext", props: tools.Properties{Effect: tools.EffectExternal}}
	e := setup(t, func(c *Config) {
		c.Policy = Policy{Mode: ModeProgressive, Thresholds: Thresholds{FlagAt: 0.3, AskAt: 0.5, BlockAt: 0.55}}
		withTools(t, ext)(c)
		withReviewer(func(s *Subagent) { s.Tools = []string{"echo", "ext"} })(c)
	})
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"subagent":"reviewer","task":"Review."}`)),
		reply(ir.StopToolUse, spawnCall("toolu_t", `{"subagent":"reviewer","task":"Read."}`)),
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.stub.Script(reviewerModel,
		reply(ir.StopToolUse, extCall("toolu_c1"), extCall("toolu_c2")),
		reply(ir.StopEndTurn, text("Reviewed.")),
		reply(ir.StopToolUse, call("toolu_e", "echo", `{"text":"x"}`)),
		reply(ir.StopEndTurn, text("Read.")),
	)
	e.send(ctx, "Work.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	results := map[string]session.ToolResult{}
	for _, ev := range e.events(ctx, session.TypeToolResult) {
		var r session.ToolResult
		if err := ev.Decode(&r); err != nil {
			t.Fatal(err)
		}
		results[r.ToolUseID] = r
	}
	warning := prompts.Render(prompts.SubagentRefused, prompts.Data{"Count": 2})
	r := results["toolu_s"]
	if len(r.Content) < 2 || r.Content[0].Text != warning || !strings.Contains(string(r.Meta), `"refused":2`) {
		t.Fatalf("the refused subagent's result %+v meta %s", r.Content, r.Meta)
	}
	r = results["toolu_t"]
	if strings.Contains(r.Content[0].Text, warning) || strings.Contains(string(r.Meta), "refused") {
		t.Fatalf("a subagent with no refusal %+v meta %s", r.Content, r.Meta)
	}
}

// TestHandoffAndDepthReachTheDecider (spec 101): a call carries the depth
// of the thread that made it, and agent and send_message what they hand
// over; the claims of a read and of a fetch follow the session's network
// and boundary, and a fetch's contact the hosts earlier fetches reached.
func TestHandoffAndDepthReachTheDecider(t *testing.T) {
	ctx := t.Context()
	var calls []Call
	rec := deciderFunc(func(ctx context.Context, c Call) (session.Risk, Decision, error) {
		calls = append(calls, c)
		return Rules{}.Decide(ctx, c)
	})
	e := setup(t, func(c *Config) {
		c.Decider = rec
		withReviewer(func(s *Subagent) { s.Tools = []string{"echo", "bash"} })(c)
	})
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"subagent":"reviewer","task":"Review.","isolation":"shared"}`)),
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.stub.Script(reviewerModel, reply(ir.StopToolUse, call("toolu_e", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("ok")))
	e.send(ctx, "Work.")
	e.turn(ctx)
	byID := map[string]Call{}
	for _, c := range calls {
		byID[c.ToolUseID] = c
	}
	if s := byID["toolu_s"]; s.Depth != 0 || s.Handoff == nil || !s.Handoff.Writes || s.Handoff.External || s.Handoff.Isolation != "shared" || s.Reach.Undo != session.UndoNone {
		t.Fatalf("agent %+v handoff %+v", s, s.Handoff)
	}
	if c := byID["toolu_e"]; c.Depth != 1 || c.Handoff != nil {
		t.Fatalf("the subagent's call %+v", c)
	}
}

// deciderFunc is a Decider of a function.
type deciderFunc func(ctx context.Context, c Call) (session.Risk, Decision, error)

func (f deciderFunc) Decide(ctx context.Context, c Call) (session.Risk, Decision, error) {
	return f(ctx, c)
}

// TestTheReachOfACall (spec 101): the claims of each kind of call, on a
// Cella machine and a host, and the holds of the machine.
func TestTheReachOfACall(t *testing.T) {
	network := &Network{Mode: session.NetworkAllowlist, Hosts: []string{"docs.example.com"}}
	cella := Reaching{Kind: machine.KindCella, Workdir: "/work", Checkpoints: true, ProtectedRefs: true, Network: network,
		Boundary: &session.Boundary{Hosts: []string{"*.example.com"}}, Fetched: map[string]bool{"docs.example.com": true}}
	if got := cella.Holds(); !slices.Equal(got, []string{HoldEgress, HoldProtectedRefs, HoldCheckpoints}) {
		t.Fatalf("holds %v", got)
	}
	if got := (Reaching{Kind: machine.KindHost}).Holds(); len(got) != 0 {
		t.Fatalf("a host's holds %v", got)
	}
	of := func(r Reaching, name, input string) tools.Reach {
		t.Helper()
		var tool tools.Tool = &fakeTool{name: name, props: tools.Properties{Effect: tools.EffectWrite}}
		for _, b := range tools.Builtins() {
			if b.Definition().Name == name {
				tool = b
			}
		}
		return r.Of(tool, name, tools.Call{Input: jsontext.Value(input)}, false)
	}
	if got := of(cella, "read", `{"path":"a"}`); got.Undo != session.UndoNone || got.Target != nil {
		t.Errorf("read %+v", got)
	}
	if got := of(cella, "write", `{"path":"a.go","content":""}`); got.Undo != session.UndoReversible || *got.Crosses.Machine || got.Target.Class != session.TargetOwn {
		t.Errorf("write on cella %+v", got)
	}
	host := Reaching{Kind: machine.KindHost, Workdir: "/work"}
	if got := of(host, "edit", `{"path":"../x"}`); got.Undo != "" || !*got.Crosses.Machine || got.Target.Class != session.TargetShared {
		t.Errorf("edit outside on a host %+v", got)
	}
	if got := of(Reaching{Kind: machine.KindHost}, "write", `{"path":"a"}`); got.Crosses.Machine != nil || got.Target.Kind != "path" {
		t.Errorf("a write with no working directory %+v", got)
	}
	inside := of(cella, "web_fetch", `{"url":"https://docs.example.com/a"}`)
	if inside.Undo != session.UndoNone || *inside.Crosses.Network || *inside.Crosses.Organization || !*inside.Crosses.Remote || *inside.Crosses.Public ||
		inside.Contact != session.ContactRepeat || inside.Target.Kind != "host" || inside.Target.ID != "" {
		t.Errorf("a fetch inside %+v", inside)
	}
	outside := of(cella, "web_fetch", `{"url":"https://other.org/a"}`)
	if !*outside.Crosses.Network || !*outside.Crosses.Organization || outside.Contact != session.ContactFirst {
		t.Errorf("a fetch outside %+v", outside)
	}
	bare := of(host, "web_fetch", `{"url":"https://other.org/a"}`)
	if bare.Crosses.Network != nil || bare.Crosses.Organization != nil {
		t.Errorf("a fetch with no network and no boundary %+v", bare)
	}
	if got := of(cella, "bash", `{"command":"rm -rf /"}`); got.Undo != "" || !got.Crosses.Empty() || got.Target != nil {
		t.Errorf("bash %+v", got)
	}
	if got := cella.Of(builtin(t, "write"), "write", tools.Call{}, true); got.Target != nil {
		t.Errorf("a call on memory %+v", got)
	}
	if got := of(cella, "mcp__a__b", `{}`); got.Undo != "" || got.Target != nil {
		t.Errorf("an MCP tool %+v", got)
	}
	// What the machine opened with: the visibility of its repositories.
	open := func(vis ...string) []session.Event {
		var repos []session.DeliveredRepository
		for _, v := range vis {
			repos = append(repos, session.DeliveredRepository{URL: "https://git.example.com/r", Visibility: v})
		}
		e, err := session.NewEvent(session.TypeSessionMachine, session.SessionMachine{Repositories: repos}, t0)
		if err != nil {
			t.Fatal(err)
		}
		return []session.Event{e}
	}
	for _, c := range []struct {
		vis  []string
		want string
	}{
		{nil, ""}, {[]string{"private"}, RepositoriesPrivate}, {[]string{"public", "public"}, RepositoriesPublic},
		{[]string{"private", "public"}, RepositoriesMixed}, {[]string{"private", ""}, ""}, {[]string{"unlisted"}, ""},
	} {
		if got := repositories(open(c.vis...)); got != c.want {
			t.Errorf("repositories %v: %q, want %q", c.vis, got, c.want)
		}
	}
}
