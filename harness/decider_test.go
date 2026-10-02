// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
		risk, d, err := Rules{Policy: p}.Decide(context.Background(), Call{Name: c.name, Props: c.props, Input: in, MachineKind: c.kind})
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
