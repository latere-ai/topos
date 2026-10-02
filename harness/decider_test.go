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
		if wd := p.Decide(c.name, c.props, in, want, c.kind, nil); d != wd {
			t.Errorf("%s %s: decision %+v, want %+v", c.name, c.input, d, wd)
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
