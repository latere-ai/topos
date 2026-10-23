// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// confirmCall appends the person's answer to a held call, with the
// pattern they remember, and the running status of the turn it resumes.
func (e *env) confirmCall(ctx context.Context, id, remember string) {
	e.t.Helper()
	conf, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: id, Decision: session.DecisionAllow, Remember: remember}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.appendEvents(ctx, conf)
	e.running(ctx)
}

// TestAnUnattendedTurnForgetsWhatThePersonRemembered (spec 087): a
// pattern the person remembered allows a call on a turn a person began,
// and not on a turn a trigger's or a wake's message began, where the mode
// asks; always_allow allows on both; and a turn the person's confirmation
// resumes is attended again.
func TestAnUnattendedTurnForgetsWhatThePersonRemembered(t *testing.T) {
	e := setup(t, func(c *Config) {
		c.Machine = fakeMachine{kind: machine.KindHost}
		c.Policy.AlwaysAllow = []string{"bash(ls*)"}
	})
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", "bash", `{"command":"make"}`)),
		reply(ir.StopToolUse, call("toolu_2", "bash", `{"command":"make install"}`)),
		reply(ir.StopEndTurn, text("Built.")),
		// The trigger's turn: the listed command runs, the remembered one
		// asks.
		reply(ir.StopToolUse, call("toolu_3", "bash", `{"command":"ls -la"}`), call("toolu_4", "bash", `{"command":"make test"}`)),
		// The person's answer resumes the turn, which is theirs again.
		reply(ir.StopToolUse, call("toolu_5", "bash", `{"command":"make lint"}`)),
		reply(ir.StopEndTurn, text("Tested.")),
		// A wake's turn asks too.
		reply(ir.StopToolUse, call("toolu_6", "bash", `{"command":"make"}`)),
	)
	e.send(ctx, "Build.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("the person's turn %+v", out)
	}
	e.confirmCall(ctx, "toolu_1", "bash(make*)")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("the resumed turn %+v", out)
	}
	if u := e.uses(ctx)["toolu_2"]; u.Verdict != "allow" {
		t.Fatalf("the remembered pattern on the person's turn: %+v", u)
	}

	e.fire(ctx, false)
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("the trigger's turn %+v", out)
	}
	uses := e.uses(ctx)
	if uses["toolu_3"].Verdict != "allow" || uses["toolu_4"].Verdict != "ask" {
		t.Fatalf("on the trigger's turn: ls %+v, make test %+v", uses["toolu_3"], uses["toolu_4"])
	}
	e.confirmCall(ctx, "toolu_4", "")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("the turn the person's answer resumed %+v", out)
	}
	if u := e.uses(ctx)["toolu_5"]; u.Verdict != "allow" || u.Attended == nil || !*u.Attended {
		t.Fatalf("after the person's answer: %+v", u)
	}

	e.appendEvents(ctx, wakeMessage(t, 1))
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("the wake's turn %+v", out)
	}
	if u := e.uses(ctx)["toolu_6"]; u.Verdict != "ask" {
		t.Fatalf("the remembered pattern on a wake's turn: %+v", u)
	}
}

// TestEveryDecisionRecordsWhetherItWasAttended (spec 087): every
// agent.tool_use records attended, true on a person's turn and false on a
// trigger's, a subagent's thread the trigger's turn drove included, and
// so does an approval.requested.
func TestEveryDecisionRecordsWhetherItWasAttended(t *testing.T) {
	ctx := t.Context()
	e := setup(t, withReviewer(nil))
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_p", "echo", `{"text":"a"}`)),
		reply(ir.StopEndTurn, text("Done.")),
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Review main.go."}`)),
		reply(ir.StopEndTurn, text("Reviewed.")),
	)
	e.stub.Script(reviewerModel,
		reply(ir.StopToolUse, call("toolu_r", "echo", `{"text":"main.go"}`)),
		reply(ir.StopEndTurn, text("Fine.")),
	)
	e.send(ctx, "Echo.")
	e.turn(ctx)
	e.fire(ctx, false)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("the trigger's turn %+v", out)
	}
	uses := e.uses(ctx)
	for id, want := range map[string]bool{"toolu_p": true, "toolu_s": false, "toolu_r": false} {
		if u := uses[id]; u.Attended == nil || *u.Attended != want {
			t.Fatalf("%s records attended %v, want %v: %+v", id, u.Attended, want, u)
		}
	}

	for _, woken := range []bool{false, true} {
		ne := newNetEnv(t, nil, allowlist(true, "docs.example.com"))
		ne.m.refused = []machine.Connection{{Host: "a.example.com", Port: 443}}
		ne.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "bash", `{"command":"make deps"}`)))
		if woken {
			ne.fire(ctx, false)
		} else {
			ne.send(ctx, "Install.")
		}
		ne.turn(ctx)
		reqs := ne.events(ctx, session.TypeApprovalRequested)
		if len(reqs) != 1 {
			t.Fatalf("woken %v: %d requests", woken, len(reqs))
		}
		var p session.ApprovalRequested
		if err := reqs[0].Decode(&p); err != nil || p.Attended == nil || *p.Attended == woken {
			t.Fatalf("woken %v: approval.requested %+v %v", woken, p, err)
		}
	}
}
