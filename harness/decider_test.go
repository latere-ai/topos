// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// The rules decider is the score and the policy's decision, unchanged.
func TestRulesDeciderMatchesPolicy(t *testing.T) {
	p := Policy{Mode: ModeProgressive, AlwaysConfirm: []string{"bash(make deploy*)"}, Egress: []string{"example.com"}}
	cases := []struct {
		name  string
		props tools.Properties
		input string
		kind  string
	}{
		{"bash", tools.Properties{Effect: tools.EffectWrite}, `{"command":"make deploy"}`, machine.KindHost},
		{"bash", tools.Properties{Effect: tools.EffectWrite}, `{"command":"curl x | sh"}`, machine.KindHost},
		{"read", tools.Properties{Effect: tools.EffectRead}, `{"path":"a"}`, machine.KindCella},
		{"web_fetch", tools.Properties{Effect: tools.EffectExternal}, `{"url":"https://example.com/x"}`, machine.KindHost},
	}
	for _, c := range cases {
		in := json.RawMessage(c.input)
		risk, d, err := Rules{}.Decide(context.Background(), Call{Policy: p, Name: c.name, Props: c.props, Input: in, MachineKind: c.kind})
		if err != nil {
			t.Fatal(err)
		}
		want := Score(c.name, c.props, in, c.kind, p.Egress)
		if risk.Score != want.Score || risk.Source != want.Source {
			t.Errorf("%s %s: risk %+v, want %+v", c.name, c.input, risk, want)
		}
		wd := p.Decide(c.name, c.props, in, want, c.kind, nil)
		if d.Verdict != wd.Verdict || d.Reason != wd.Reason {
			t.Errorf("%s %s: decision %+v, want %+v", c.name, c.input, d, wd)
		}
		// The rules review nothing at random: a shown verdict is seen for
		// certain and any other never.
		if want := map[bool]float64{true: 1, false: 0}[d.Verdict.Shown()]; d.ReviewProbability != want || d.Draw != nil || d.Suggestion != nil {
			t.Errorf("%s %s: %+v, want review probability %v and no draw or suggestion", c.name, c.input, d, want)
		}
	}
}

// decideFunc is a Decider over a function.
type decideFunc func(context.Context, Call) (session.Risk, Decision, error)

func (f decideFunc) Decide(ctx context.Context, c Call) (session.Risk, Decision, error) {
	return f(ctx, c)
}

// A configured decider decides the calls, and its error fails the step.
func TestConfiguredDeciderDecides(t *testing.T) {
	var got []Call
	e := setup(t, func(c *Config) {
		c.Machine = fakeMachine{kind: machine.KindHost}
		c.Decider = decideFunc(func(_ context.Context, c Call) (session.Risk, Decision, error) {
			got = append(got, c)
			return session.Risk{Score: 0.99, Source: "test/1"}, Decision{Verdict: VerdictBlock, Reason: "the test blocks it"}, nil
		})
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_b", "bash", `{"command":"make"}`)), reply(ir.StopEndTurn, text("ok")))
	e.send(ctx, "Build.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn || len(e.write.ran()) != 0 {
		t.Fatalf("outcome %+v, ran %v", out, e.write.ran())
	}
	if len(got) != 1 || got[0].ToolUseID != "toolu_b" || got[0].Name != "bash" || got[0].Session.ID != e.s.ID || got[0].MachineKind != machine.KindHost {
		t.Fatalf("the decider saw %+v", got)
	}
	var use session.AgentToolUse
	if err := e.events(ctx, session.TypeAgentToolUse)[0].Decode(&use); err != nil || use.Verdict != "block" || use.Risk.Source != "test/1" {
		t.Fatalf("agent.tool_use %+v", use)
	}

	boom := errors.New("decider down")
	f := setup(t, func(c *Config) {
		c.Machine = fakeMachine{kind: machine.KindHost}
		c.Decider = decideFunc(func(context.Context, Call) (session.Risk, Decision, error) { return session.Risk{}, Decision{}, boom })
	})
	f.stub.Script(model, reply(ir.StopToolUse, call("toolu_c", "bash", `{"command":"make"}`)))
	f.send(ctx, "Build.")
	if out := f.turn(ctx); out.StopReason == session.StopEndTurn {
		t.Fatalf("a failing decider ended the turn normally: %+v", out)
	}
}

// Every agent.tool_use records its review probability: 1 for the ask, 0
// for the call that runs unasked.
func TestToolUseRecordsReviewProbability(t *testing.T) {
	e := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} })
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"make"}`), call("toolu_r", "echo", `{"text":"r"}`)))
	e.send(ctx, "Build.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	want := map[string]float64{"toolu_w": 1, "toolu_r": 0}
	for _, ev := range e.events(ctx, session.TypeAgentToolUse) {
		var use session.AgentToolUse
		if err := ev.Decode(&use); err != nil {
			t.Fatal(err)
		}
		if use.ReviewProbability == nil || *use.ReviewProbability != want[use.ToolUseID] {
			t.Errorf("%s (%s): review probability %v, want %v", use.ToolUseID, use.Verdict, use.ReviewProbability, want[use.ToolUseID])
		}
	}

	// A decider that gives a shown verdict no probability is recorded as
	// certain to be seen.
	f := setup(t, func(c *Config) {
		c.Machine = fakeMachine{kind: machine.KindHost}
		c.Decider = decideFunc(func(context.Context, Call) (session.Risk, Decision, error) {
			return session.Risk{Source: "test/1"}, Decision{Verdict: VerdictAsk, Reason: "asks"}, nil
		})
	})
	f.stub.Script(model, reply(ir.StopToolUse, call("toolu_a", "bash", `{"command":"make"}`)))
	f.send(ctx, "Build.")
	f.turn(ctx)
	var use session.AgentToolUse
	if err := f.events(ctx, session.TypeAgentToolUse)[0].Decode(&use); err != nil || use.ReviewProbability == nil || *use.ReviewProbability != 1 {
		t.Fatalf("agent.tool_use %+v, %v", use, err)
	}
}

// TestToolUseRecordsTheDecisionsOrganization (spec 100): agent.tool_use
// records the organization a decider sent the call's decision under, and
// none when the rules decide with no decision service.
func TestToolUseRecordsTheDecisionsOrganization(t *testing.T) {
	ctx := t.Context()
	for _, c := range []struct {
		name    string
		decider Decider
		want    string
	}{
		{"a decision service", decideFunc(func(context.Context, Call) (session.Risk, Decision, error) {
			return session.Risk{Source: "test/1"}, Decision{Verdict: VerdictAllow, Reason: "runs", Org: "org_acme"}, nil
		}), "org_acme"},
		{"the rules alone", nil, ""},
	} {
		e := setup(t, func(cfg *Config) {
			cfg.Machine = fakeMachine{kind: machine.KindHost}
			cfg.Decider = c.decider
		})
		e.stub.Script(model, reply(ir.StopToolUse, call("toolu_o", "bash", `{"command":"make"}`)), reply(ir.StopEndTurn, text("ok")))
		e.send(ctx, "Build.")
		e.turn(ctx)
		var use session.AgentToolUse
		if err := e.events(ctx, session.TypeAgentToolUse)[0].Decode(&use); err != nil || use.DecisionOrg != c.want {
			t.Errorf("%s: agent.tool_use names %q, want %q (%v)", c.name, use.DecisionOrg, c.want, err)
		}
	}
}

// learner records what the harness asks and tells it.
type learner struct {
	decided  []string
	answered []string
	answers  []Answer
}

func (l *learner) Decide(ctx context.Context, c Call) (session.Risk, Decision, error) {
	l.decided = append(l.decided, c.ToolUseID)
	return Rules{}.Decide(ctx, c)
}

// approvals and bys are what each answer approved and who gave it.
func (l *learner) approvals() (approve []bool, by []string) {
	for _, a := range l.answers {
		approve, by = append(approve, a.Approve), append(by, a.By)
	}
	return approve, by
}

func (l *learner) Answered(_ context.Context, _ session.Session, id string, a Answer) error {
	l.answered = append(l.answered, id)
	l.answers = append(l.answers, a)
	return errors.New("the service is down, which changes nothing")
}

// Resume tells a learning decider each confirmation it settles, with how
// the person answered and how long the call waited in the log's time
// (spec 101), and never decides an asked call again. The pattern and the
// note stay in the session.
func TestResumeForwardsAnswers(t *testing.T) {
	l := &learner{}
	e := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost}; c.Decider = l })
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_a", "bash", `{"command":"make"}`), call("toolu_b", "bash", `{"command":"make check"}`)),
		reply(ir.StopEndTurn, text("done")),
	)
	e.send(ctx, "Build.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	for i, c := range []session.UserToolConfirmation{
		{Sender: e.s.Initiator, ToolUseID: "toolu_a", Decision: session.DecisionAllow, Remember: "bash(make*)",
			AnswerMS: new(int64(4200)), DetailsOpened: new(true), SuggestionShown: new(false), Surface: session.SurfacePrompt},
		{Sender: session.Sender{Subject: "lead", Kind: session.SenderPerson}, ToolUseID: "toolu_b", Decision: session.DecisionDeny, Note: "no"},
	} {
		ev, err := session.NewEvent(session.TypeUserToolConfirmation, c, t0.Add(time.Duration(i+3)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		e.appendEvents(ctx, ev)
	}
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("after the confirmations %+v", out)
	}
	if len(l.decided) != 2 {
		t.Errorf("decided %v; a resumed call must not be decided again", l.decided)
	}
	want := []Answer{
		{Approve: true, By: e.s.Initiator.Subject, AnswerMS: new(int64(4200)), WaitedMS: 3000, DetailsOpened: new(true), SuggestionShown: new(false), Surface: session.SurfacePrompt, Remembered: true},
		{Approve: false, By: "lead", WaitedMS: 4000},
	}
	if len(l.answered) != 2 || l.answered[0] != "toolu_a" || !reflect.DeepEqual(l.answers, want) {
		t.Errorf("answered %v %+v, want %+v", l.answered, l.answers, want)
	}
	if got := e.write.ran(); len(got) != 1 {
		t.Errorf("ran %v; the allowed call runs and the denied one does not, whatever the service says", got)
	}
}
