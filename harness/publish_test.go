// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// systemText is a request's system prompt as one text.
func systemText(r *ir.Request) string {
	var b strings.Builder
	for _, block := range r.System {
		b.WriteString(block.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// TestTheRequestNamesTheRoute: every request of a session that runs by a
// routed name carries that name as a system part, and a session that
// runs by none carries no such part (spec 043).
func TestTheRequestNamesTheRoute(t *testing.T) {
	for name, c := range map[string]struct {
		model *session.ModelRef
		want  bool
	}{
		"routed":   {&session.ModelRef{Name: model, Via: "tier/quick"}, true},
		"unrouted": {&session.ModelRef{Name: model}, false},
		"no model": {nil, false},
	} {
		e := setupSession(t, nil, func(s *session.Session) { s.Model = c.model })
		ctx := t.Context()
		e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("ok")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			got := strings.Contains(systemText(r), "<context>\nModel route: tier/quick\n</context>")
			if got != c.want {
				return fmt.Errorf("the system prompt names the route: %v, want %v:\n%s", got, c.want, systemText(r))
			}
			return nil
		}})
		e.send(ctx, "Hi.")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("%s: outcome %+v", name, out)
		}
	}
}

// TestASpawnedThreadHoldsNoPublish: the session's own thread holds the
// publish tool its registry carries, and a thread a spawn starts holds
// none, whatever its subagent names, so a session has one app (spec 043).
func TestASpawnedThreadHoldsNoPublish(t *testing.T) {
	pub := &fakeTool{name: session.ToolPublish, props: tools.Properties{Effect: tools.EffectExternal}}
	e := setup(t, func(c *Config) {
		if err := c.Tools.Add(pub); err != nil {
			t.Fatal(err)
		}
		withReviewer(func(s *Subagent) { s.Tools = []string{"echo", session.ToolPublish} })(c)
	})
	ctx := t.Context()
	e.stub.Script(model,
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{spawnCall("toolu_s", `{"agent":"reviewer","task":"Publish it.","tools":["echo","publish"]}`)}, StopReason: ir.StopToolUse}, Expect: func(r *ir.Request) error {
			if names := offered(r); !slices.Contains(names, session.ToolPublish) {
				return fmt.Errorf("the session's own thread is offered %v", names)
			}
			return nil
		}},
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.stub.Script(reviewerModel, luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{text("Fine.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
		if names := offered(r); !slices.Equal(names, []string{"echo"}) {
			return fmt.Errorf("a spawned thread is offered %v, want echo alone", names)
		}
		return nil
	}})
	e.send(ctx, "Publish.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
}

// TestAPublishCallAsksInConfirm: a publish call on a Cella machine in
// confirm mode waits for a person, a release included, since its effect
// reaches outside the sandbox: the session goes idle tool_confirmation
// with the call recorded ask, and the tool does not run (spec 043).
func TestAPublishCallAsksInConfirm(t *testing.T) {
	pub := &fakeTool{name: session.ToolPublish, props: tools.Properties{Effect: tools.EffectExternal},
		schema: `{"type":"object","properties":{"path":{"type":"string"},"release":{"type":"boolean"}},"additionalProperties":false}`}
	e := setup(t, func(c *Config) {
		if err := c.Tools.Add(pub); err != nil {
			t.Fatal(err)
		}
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_r", session.ToolPublish, `{"release":true}`)))
	e.send(ctx, "Release it.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v, want a wait for the person", out)
	}
	var use session.AgentToolUse
	for _, ev := range e.all() {
		if ev.Type == session.TypeAgentToolUse {
			if err := ev.Decode(&use); err != nil {
				t.Fatal(err)
			}
		}
	}
	if use.Name != session.ToolPublish || use.Verdict != string(VerdictAsk) {
		t.Fatalf("the call is recorded %+v, want publish asked", use)
	}
	for _, ev := range e.all() {
		if ev.Type == session.TypeToolResult {
			t.Fatalf("the call ran before the person answered: %s", ev.Payload)
		}
	}
}

// checkingTool is a fake tool of an external effect whose Check refuses
// an input of text "nope", recording each state it was handed.
type checkingTool struct {
	*fakeTool
	mu     sync.Mutex
	checks []string
}

func (c *checkingTool) Check(call tools.Call) *tools.Result {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return nil
	}
	c.mu.Lock()
	c.checks = append(c.checks, call.ID)
	c.mu.Unlock()
	if in.Text != "nope" {
		return nil
	}
	res := tools.Text(tools.OutcomeError, "refused: nope")
	return &res
}

func (c *checkingTool) checked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.checks)
}

// TestACallRefusedOnItsInputIsNotAsked: in confirm mode, a call its tool
// refuses on its input is answered with the refusal before it is
// decided: it gets no agent.tool_use, the session does not wait for a
// person, the tool does not run, and the model reads the refusal in the
// next step. A call the check passes is asked as before, and a call after
// another of the same tool in its step is not checked, since the earlier
// call's result may change what it would be refused for (spec 012).
func TestACallRefusedOnItsInputIsNotAsked(t *testing.T) {
	pub := &checkingTool{fakeTool: &fakeTool{name: session.ToolPublish, props: tools.Properties{Effect: tools.EffectExternal}}}
	e := setup(t, func(c *Config) {
		if err := c.Tools.Add(pub); err != nil {
			t.Fatal(err)
		}
	})
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", session.ToolPublish, `{"text":"nope"}`)),
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("It was refused.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			if len(last.Blocks) != 1 || last.Blocks[0].ToolResult == nil || last.Blocks[0].ToolResult.ToolUseID != "toolu_1" || !last.Blocks[0].ToolResult.IsError {
				return fmt.Errorf("the model was not sent the refusal: %+v", last)
			}
			return nil
		}},
	)
	e.send(ctx, "Publish it.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v, want the turn to end without a person", out)
	}
	var results []session.ToolResult
	for _, ev := range e.all() {
		switch ev.Type {
		case session.TypeAgentToolUse:
			t.Fatalf("a refused call was recorded for a decision: %s", ev.Payload)
		case session.TypeToolResult:
			var p session.ToolResult
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			results = append(results, p)
		}
	}
	if len(results) != 1 || results[0].ToolUseID != "toolu_1" || !results[0].IsError || results[0].Outcome != tools.OutcomeError || results[0].Content[0].Text != "refused: nope" {
		t.Fatalf("the results %+v", results)
	}
	if ran := pub.ran(); len(ran) != 0 {
		t.Fatalf("a refused call ran: %v", ran)
	}

	// A refused call leaves the next call of its tool to be checked; that
	// one passes and is asked, and the one after it is asked unchecked.
	e.stub.Script(model, reply(ir.StopToolUse,
		call("toolu_2", session.ToolPublish, `{"text":"nope"}`),
		call("toolu_3", session.ToolPublish, `{"text":"ok"}`),
		call("toolu_4", session.ToolPublish, `{"text":"nope"}`)))
	e.send(ctx, "Publish both.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v, want a wait for the person", out)
	}
	if got := pub.checked(); !slices.Equal(got, []string{"toolu_1", "toolu_2", "toolu_3"}) {
		t.Fatalf("checked %v, want toolu_1, toolu_2 and toolu_3", got)
	}
	asked := map[string]string{}
	for _, ev := range e.all() {
		if ev.Type == session.TypeAgentToolUse {
			var use session.AgentToolUse
			if err := ev.Decode(&use); err != nil {
				t.Fatal(err)
			}
			asked[use.ToolUseID] = use.Verdict
		}
	}
	if len(asked) != 2 || asked["toolu_3"] != string(VerdictAsk) || asked["toolu_4"] != string(VerdictAsk) {
		t.Fatalf("the decided calls %v, want toolu_3 and toolu_4 asked", asked)
	}
}

// holdingTool is a fake tool of an external effect that holds for a
// person each call whose input's text is "hold".
type holdingTool struct{ *fakeTool }

// heldReason is why holdingTool holds a call.
const heldReason = "changes who can open the app, which a person approves every time"

func (h *holdingTool) Confirm(c tools.Call) string {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(c.Input, &in); err != nil || in.Text != "hold" {
		return ""
	}
	return heldReason
}

// TestACallItsToolHoldsWaitsForAPerson: a call its tool holds for a person
// is recorded ask with the tool's reason and does not run, whatever would
// run a call of that tool without a person: an always_allow pattern, a
// pattern the person's answer added to the session, the progressive mode's
// score, and a decider that allows everything. A call of the same tool it
// does not hold runs as the policy says. Plan mode blocks the held call,
// and once the person allows it, it runs (spec 012).
func TestACallItsToolHoldsWaitsForAPerson(t *testing.T) {
	ctx := t.Context()
	uses := func(e *env) map[string]session.AgentToolUse {
		t.Helper()
		out := map[string]session.AgentToolUse{}
		for _, ev := range e.all() {
			if ev.Type != session.TypeAgentToolUse {
				continue
			}
			var use session.AgentToolUse
			if err := ev.Decode(&use); err != nil {
				t.Fatal(err)
			}
			out[use.ToolUseID] = use
		}
		return out
	}
	for name, policy := range map[string]func(*Config){
		"an always_allow pattern": func(c *Config) { c.Policy = Policy{Mode: ModeConfirm, AlwaysAllow: []string{session.ToolPublish}} },
		"a score below every threshold": func(c *Config) {
			c.Policy = Policy{Mode: ModeProgressive, Thresholds: Thresholds{FlagAt: 0.7, AskAt: 0.8, BlockAt: 0.9}}
		},
		"a decider that allows every call": func(c *Config) {
			c.Decider = decideFunc(func(context.Context, Call) (session.Risk, Decision, error) {
				return session.Risk{Source: "test/1"}, Decision{Verdict: VerdictAllow, Reason: "the test allows it"}, nil
			})
		},
		"a decider that flags every call": func(c *Config) {
			c.Decider = decideFunc(func(context.Context, Call) (session.Risk, Decision, error) {
				return session.Risk{Source: "test/1"}, Decision{Verdict: VerdictFlag, Reason: "the test flags it"}, nil
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			pub := &holdingTool{&fakeTool{name: session.ToolPublish, props: tools.Properties{Effect: tools.EffectExternal}}}
			e := setup(t, func(c *Config) {
				if err := c.Tools.Add(pub); err != nil {
					t.Fatal(err)
				}
				policy(c)
			})
			e.stub.Script(model,
				reply(ir.StopToolUse, call("toolu_go", session.ToolPublish, `{"text":"go"}`), call("toolu_hold", session.ToolPublish, `{"text":"hold"}`)),
				reply(ir.StopEndTurn, text("Done.")))
			e.send(ctx, "Publish and open it.")
			if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
				t.Fatalf("outcome %+v, want a wait for the person", out)
			}
			got := uses(e)
			if v := got["toolu_go"].Verdict; v != string(VerdictAllow) && v != string(VerdictFlag) {
				t.Errorf("the call the tool does not hold is recorded %+v, want it to run", got["toolu_go"])
			}
			held := got["toolu_hold"]
			if held.Verdict != string(VerdictAsk) || held.Reason != heldReason || held.ReviewProbability == nil || *held.ReviewProbability != 1 {
				t.Fatalf("the held call is recorded %+v, want ask with the tool's reason, seen for certain", held)
			}
			if ran := pub.ran(); !slices.Equal(ran, []string{"toolu_go"}) {
				t.Fatalf("the tool ran %v before the person answered, want toolu_go alone", ran)
			}
			// An unanswered ask keeps waiting, and the person's allow runs it.
			e.running(ctx)
			if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation || len(pub.ran()) != 1 {
				t.Fatalf("a claim with no answer: %+v, ran %v", out, pub.ran())
			}
			conf, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: "toolu_hold", Decision: session.DecisionAllow}, t0)
			if err != nil {
				t.Fatal(err)
			}
			e.appendEvents(ctx, conf)
			e.running(ctx)
			if out := e.turn(ctx); out.StopReason != session.StopEndTurn || !slices.Equal(pub.ran(), []string{"toolu_go", "toolu_hold"}) {
				t.Fatalf("after the person's allow: %+v, ran %v", out, pub.ran())
			}
		})
	}

	// A pattern the person's answer added to the session allows a later
	// call of the tool, and never one the tool holds: not the answer to the
	// first call, nor the answer to a held one.
	pub := &holdingTool{&fakeTool{name: session.ToolPublish, props: tools.Properties{Effect: tools.EffectExternal}}}
	e := setup(t, func(c *Config) {
		if err := c.Tools.Add(pub); err != nil {
			t.Fatal(err)
		}
	})
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", session.ToolPublish, `{"text":"go"}`)),
		reply(ir.StopToolUse, call("toolu_2", session.ToolPublish, `{"text":"go"}`), call("toolu_3", session.ToolPublish, `{"text":"hold"}`)),
		reply(ir.StopToolUse, call("toolu_4", session.ToolPublish, `{"text":"hold"}`)),
	)
	allow := func(id string) {
		t.Helper()
		conf, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: id, Decision: session.DecisionAllow, Remember: session.ToolPublish}, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.appendEvents(ctx, conf)
		e.running(ctx)
	}
	e.send(ctx, "Publish, then open it.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("the first call: %+v, want a wait", out)
	}
	allow("toolu_1")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation || !slices.Equal(pub.ran(), []string{"toolu_1", "toolu_2"}) {
		t.Fatalf("after a remembered allow: %+v, ran %v; want the second call run and the held one waiting", out, pub.ran())
	}
	if got := uses(e); got["toolu_2"].Verdict != string(VerdictAllow) || got["toolu_3"].Verdict != string(VerdictAsk) || got["toolu_3"].Reason != heldReason {
		t.Fatalf("recorded %+v and %+v", got["toolu_2"], got["toolu_3"])
	}
	allow("toolu_3")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation || !slices.Equal(pub.ran(), []string{"toolu_1", "toolu_2", "toolu_3"}) {
		t.Fatalf("after the held call's remembered allow: %+v, ran %v; want the next held call waiting", out, pub.ran())
	}
	if got := uses(e)["toolu_4"]; got.Verdict != string(VerdictAsk) || got.Reason != heldReason {
		t.Fatalf("a held call after a remembered allow of one is recorded %+v", got)
	}

	// Plan mode blocks the held call as it blocks every call that is not
	// read-only, and a block is never turned into an ask.
	blocked := &holdingTool{&fakeTool{name: session.ToolPublish, props: tools.Properties{Effect: tools.EffectExternal}}}
	p := setup(t, func(c *Config) {
		if err := c.Tools.Add(blocked); err != nil {
			t.Fatal(err)
		}
		c.Policy = Policy{Mode: ModePlan, AlwaysAllow: []string{session.ToolPublish}}
	})
	p.stub.Script(model, reply(ir.StopToolUse, call("toolu_hold", session.ToolPublish, `{"text":"hold"}`)), reply(ir.StopEndTurn, text("It is blocked.")))
	p.send(ctx, "Open it.")
	if out := p.turn(ctx); out.StopReason != session.StopEndTurn || len(blocked.ran()) != 0 {
		t.Fatalf("plan mode: %+v, ran %v", out, blocked.ran())
	}
	if got := uses(p)["toolu_hold"]; got.Verdict != string(VerdictBlock) {
		t.Fatalf("plan mode recorded %+v, want block", got)
	}
}

// TestTheRulesHoldACallForAPerson: the built-in decider and the rules a
// decision service cannot suggest past both ask for a call its tool holds,
// with the tool's reason, where the lists and the score would allow it; a
// call that asks or is blocked already keeps its own verdict and reason,
// and a call no tool holds is decided as before.
func TestTheRulesHoldACallForAPerson(t *testing.T) {
	ext := tools.Properties{Effect: tools.EffectExternal}
	in := json.RawMessage(`{"access":"link"}`)
	for name, c := range map[string]struct {
		policy  Policy
		confirm string
		want    Verdict
		reason  string
	}{
		"always_allow, held":      {Policy{Mode: ModeConfirm, AlwaysAllow: []string{"publish"}}, heldReason, VerdictAsk, heldReason},
		"always_allow, not held":  {Policy{Mode: ModeConfirm, AlwaysAllow: []string{"publish"}}, "", VerdictAllow, "on the always-allow list"},
		"progressive low, held":   {Policy{Mode: ModeProgressive, Thresholds: Thresholds{FlagAt: 0.7, AskAt: 0.8, BlockAt: 0.9}}, heldReason, VerdictAsk, heldReason},
		"progressive flag, held":  {Policy{Mode: ModeProgressive, Thresholds: Thresholds{FlagAt: 0.5, AskAt: 0.8, BlockAt: 0.9}}, heldReason, VerdictAsk, heldReason},
		"confirm asks, held":      {Policy{Mode: ModeConfirm}, heldReason, VerdictAsk, "needs a confirmation"},
		"always_confirm, held":    {Policy{Mode: ModeConfirm, AlwaysConfirm: []string{"publish"}, AlwaysAllow: []string{"publish"}}, heldReason, VerdictAsk, "on the organization's always-confirm list"},
		"plan, held":              {Policy{Mode: ModePlan, AlwaysAllow: []string{"publish"}}, heldReason, VerdictBlock, ""},
		"progressive block, held": {Policy{Mode: ModeProgressive, Thresholds: Thresholds{FlagAt: 0.1, AskAt: 0.2, BlockAt: 0.3}}, heldReason, VerdictBlock, ""},
		"remembered, held":        {Policy{Mode: ModeConfirm}, heldReason, VerdictAsk, heldReason},
		"remembered, not held":    {Policy{Mode: ModeConfirm}, "", VerdictAllow, "allowed earlier in this session"},
	} {
		call := Call{Policy: c.policy, Name: "publish", Props: ext, Input: in, MachineKind: machine.KindCella, Confirm: c.confirm}
		if strings.HasPrefix(name, "remembered") {
			call.Remembered = []string{"publish"}
		}
		_, d, err := Rules{}.Decide(context.Background(), call)
		if err != nil {
			t.Fatal(err)
		}
		if d.Verdict != c.want || (c.reason != "" && d.Reason != c.reason) {
			t.Errorf("%s: the rules decided %+v, want %s %q", name, d, c.want, c.reason)
		}
		if d.Verdict == VerdictAsk && d.ReviewProbability != 1 {
			t.Errorf("%s: an ask with review probability %v", name, d.ReviewProbability)
		}
	}
	// What a decision service cannot suggest past: a held call is settled
	// as an ask, before an always_allow or a remembered pattern allows it.
	held := Call{Policy: Policy{Mode: ModeProgressive, AlwaysAllow: []string{"publish"}}, Name: "publish", Props: ext, Input: in, Remembered: []string{"publish"}, Confirm: heldReason}
	if d, ok := held.Policy.Settle(held, session.Risk{Score: 0.6}); !ok || d.Verdict != VerdictAsk || d.Reason != heldReason || d.ReviewProbability != 1 {
		t.Errorf("a held call is settled %+v, %v; want an ask the service cannot loosen", d, ok)
	}
	held.Confirm = ""
	if d, ok := held.Policy.Settle(held, session.Risk{Score: 0.6}); !ok || d.Verdict != VerdictAllow {
		t.Errorf("the same call not held is settled %+v, %v; want the always_allow", d, ok)
	}
}
