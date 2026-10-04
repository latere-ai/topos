// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// changeMode appends the session.policy_changed a person's change of the
// approval mode appends, beside the running turn as the server does.
func (e *env) changeMode(ctx context.Context, from, to Mode) {
	e.t.Helper()
	e.alongside(ctx, session.TypePolicyChanged, session.PolicyChanged{By: e.s.Initiator, Old: session.PolicyRef{Mode: string(from)}, New: session.PolicyRef{Mode: string(to)}})
}

// uses are the agent.tool_use events of the log, by tool_use id, in the
// thread named, "" for the session's own.
func (e *env) uses(ctx context.Context) map[string]session.AgentToolUse {
	e.t.Helper()
	out := map[string]session.AgentToolUse{}
	for _, ev := range e.events(ctx, session.TypeAgentToolUse) {
		var u session.AgentToolUse
		if err := ev.Decode(&u); err != nil {
			e.t.Fatal(err)
		}
		out[u.ToolUseID] = u
	}
	return out
}

// TestAModeChangeHoldsFromTheNextStep: a change of the mode appended while
// a step runs leaves that step's calls with the verdicts they were decided
// with, and the next step of the same turn decides under the new mode:
// confirm runs a write in a Cella machine, plan blocks it.
func TestAModeChangeHoldsFromTheNextStep(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.echo.run = func(tools.Call) (tools.Result, error) {
		e.changeMode(context.Background(), ModeConfirm, ModePlan)
		return tools.Text(tools.OutcomeOK, "looked"), nil
	}
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_r", "echo", `{"text":"look"}`), call("toolu_w1", "bash", `{"command":"make"}`)),
		reply(ir.StopToolUse, call("toolu_w2", "bash", `{"command":"make install"}`)),
		reply(ir.StopEndTurn, text("done")),
	)
	e.send(ctx, "Build.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if got := e.write.ran(); len(got) != 1 {
		t.Fatalf("the shell ran %v; the call decided before the change runs and the one after is blocked", got)
	}
	uses := e.uses(ctx)
	if u := uses["toolu_w1"]; u.Mode != string(ModeConfirm) || u.Verdict != string(VerdictAllow) {
		t.Fatalf("the call decided before the change: %+v", u)
	}
	if u := uses["toolu_w2"]; u.Mode != string(ModePlan) || u.Verdict != string(VerdictBlock) {
		t.Fatalf("the call decided after the change: %+v", u)
	}
	if got := e.outcomes(); got["toolu_w2"] != tools.OutcomeBlocked {
		t.Fatalf("outcomes %v", got)
	}
}

// TestAWaitingCallStaysWaitingAfterAModeChange: a call waiting for a
// confirmation keeps waiting when the mode becomes more permissive, a
// claim with no answer sends no request, and the person's confirmation
// still runs it once.
func TestAWaitingCallStaysWaitingAfterAModeChange(t *testing.T) {
	e := setup(t, func(c *Config) {
		c.Machine = fakeMachine{kind: machine.KindHost}
		c.Policy.Thresholds = DefaultThresholds
	})
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"make"}`)),
		reply(ir.StopEndTurn, text("built")),
	)
	e.send(ctx, "Build.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	e.changeMode(ctx, ModeConfirm, ModeProgressive)
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation || len(e.stub.Requests()) != 1 || len(e.write.ran()) != 0 {
		t.Fatalf("a claim after the change: %+v, %d requests, ran %v", out, len(e.stub.Requests()), e.write.ran())
	}
	e.confirm(ctx, "toolu_w")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("after the confirmation %+v", out)
	}
	if got := e.write.ran(); len(got) != 1 {
		t.Fatalf("the shell ran %v", got)
	}
	if u := e.uses(ctx)["toolu_w"]; u.Verdict != string(VerdictAsk) || u.Mode != string(ModeConfirm) {
		t.Fatalf("the waiting call's record moved: %+v", u)
	}
}

// TestAThreadKeepsItsAgentsStricterMode: a session switched to
// progressive decides its own calls under it, while a thread of a
// subagent whose agent names plan stays in plan; a subagent that names no
// mode follows the session.
func TestAThreadKeepsItsAgentsStricterMode(t *testing.T) {
	for _, c := range []struct {
		name       string
		own, child Mode
		verdict    Verdict
	}{
		{"the agent names plan", ModePlan, ModePlan, VerdictBlock},
		{"the agent names none", "", ModeProgressive, VerdictAllow},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, withReviewer(func(s *Subagent) {
				s.Mode = c.own
				s.Tools = []string{"echo", "bash"}
			}))
			e.cfg.Policy.Thresholds = DefaultThresholds
			h, err := New(e.cfg)
			if err != nil {
				t.Fatal(err)
			}
			e.h = h
			ctx := t.Context()
			e.stub.Script(model,
				reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Build it."}`)),
				reply(ir.StopToolUse, call("toolu_p", "bash", `{"command":"make"}`)),
				reply(ir.StopEndTurn, text("Built.")),
			)
			e.stub.Script(reviewerModel,
				reply(ir.StopToolUse, call("toolu_c", "bash", `{"command":"make"}`)),
				reply(ir.StopEndTurn, text("Done.")),
			)
			e.send(ctx, "Build.")
			e.changeMode(ctx, ModeConfirm, ModeProgressive)
			if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
				t.Fatalf("outcome %+v", out)
			}
			uses := e.uses(ctx)
			if u := uses["toolu_p"]; u.Mode != string(ModeProgressive) || u.Verdict != string(VerdictAllow) {
				t.Fatalf("the session's own call: %+v", u)
			}
			if u := uses["toolu_c"]; u.Mode != string(c.child) || u.Verdict != string(c.verdict) {
				t.Fatalf("the thread's call: %+v", u)
			}
		})
	}
}

// TestProgressiveWithoutASandboxDecidesAsConfirm: a session on a host
// that records no operating-system sandbox, switched to progressive,
// decides its calls as confirm and records confirm.
func TestProgressiveWithoutASandboxDecidesAsConfirm(t *testing.T) {
	e := setup(t, func(c *Config) {
		c.Machine = fakeMachine{kind: machine.KindHost, sandbox: machine.SandboxNone}
		c.Policy.Thresholds = DefaultThresholds
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"make"}`)))
	e.send(ctx, "Build.")
	e.changeMode(ctx, ModeConfirm, ModeProgressive)
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	if u := e.uses(ctx)["toolu_w"]; u.Mode != string(ModeConfirm) || u.Verdict != string(VerdictAsk) {
		t.Fatalf("the call on a host with no sandbox: %+v", u)
	}
}
