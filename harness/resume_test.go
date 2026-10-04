// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// confirm appends a person's allow of one call and the claim that
// follows it, as a client and then a runner do.
func (e *env) confirm(ctx context.Context, id string) {
	e.t.Helper()
	c, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: id, Decision: session.DecisionAllow}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.appendEvents(ctx, c)
	if !session.HasPendingInput(e.all()) {
		e.t.Fatalf("the confirmation of %s is not pending input", id)
	}
	e.running(ctx)
}

// outcomes are the outcomes of the log's tool results, by tool_use id,
// in every thread.
func (e *env) outcomes() map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, ev := range e.all() {
		if ev.Type != session.TypeToolResult {
			continue
		}
		var res session.ToolResult
		if err := ev.Decode(&res); err != nil {
			e.t.Fatal(err)
		}
		if _, twice := out[res.ToolUseID]; twice {
			e.t.Fatalf("%s has two results", res.ToolUseID)
		}
		out[res.ToolUseID] = res.Outcome
	}
	return out
}

// TestTwoAsksConfirmedApart: two waits of one step, each answered and
// then claimed, as a runner claims on every pending input. The claim
// that reads a confirmation runs its call, whatever else of the step
// still waits, so the next claim does not count a second running status
// against a call that never started.
func TestTwoAsksConfirmedApart(t *testing.T) {
	t.Run("two asks", func(t *testing.T) {
		e := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} })
		ctx := t.Context()
		e.stub.Script(model,
			reply(ir.StopToolUse, call("toolu_a", "bash", `{"command":"make a"}`), call("toolu_b", "bash", `{"command":"make b"}`)),
			reply(ir.StopEndTurn, text("done")),
		)
		e.send(ctx, "Build.")
		if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
			t.Fatalf("outcome %+v", out)
		}
		e.confirm(ctx, "toolu_a")
		if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
			t.Fatalf("after the first confirmation %+v", out)
		}
		if got := e.write.ran(); !slices.Equal(got, []string{"toolu_a"}) {
			t.Fatalf("the claim that read toolu_a's confirmation ran %v", got)
		}
		if n := len(e.stub.Requests()); n != 1 {
			t.Fatalf("%d requests while toolu_b waits", n)
		}
		e.confirm(ctx, "toolu_b")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the second confirmation %+v", out)
		}
		if got := e.write.ran(); !slices.Equal(got, []string{"toolu_a", "toolu_b"}) {
			t.Fatalf("the shell ran %v, want each call once", got)
		}
		if got := e.outcomes(); got["toolu_a"] != tools.OutcomeOK || got["toolu_b"] != tools.OutcomeOK || len(got) != 2 {
			t.Fatalf("outcomes %v", got)
		}
	})

	// A confirmed call beside a thread that still waits for a
	// confirmation of its own.
	t.Run("an ask beside a waiting thread", func(t *testing.T) {
		e := setup(t, func(c *Config) {
			withReviewer(func(s *Subagent) { s.Tools = []string{"bash"} })(c)
			c.Machine = fakeMachine{kind: machine.KindHost}
		})
		ctx := t.Context()
		e.stub.Script(model,
			reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Run the tests."}`), call("toolu_root", "bash", `{"command":"make"}`)),
			reply(ir.StopEndTurn, text("done")),
		)
		e.stub.Script(reviewerModel,
			reply(ir.StopToolUse, call("toolu_child", "bash", `{"command":"make test"}`)),
			reply(ir.StopEndTurn, text("Tests pass.")),
		)
		e.send(ctx, "Build and test.")
		if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
			t.Fatalf("outcome %+v", out)
		}
		e.confirm(ctx, "toolu_root")
		if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
			t.Fatalf("after the root's confirmation %+v", out)
		}
		if got := e.write.ran(); !slices.Equal(got, []string{"toolu_root"}) {
			t.Fatalf("the claim that read toolu_root's confirmation ran %v", got)
		}
		e.confirm(ctx, "toolu_child")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the thread's confirmation %+v", out)
		}
		if got := e.write.ran(); !slices.Equal(got, []string{"toolu_root", "toolu_child"}) {
			t.Fatalf("the shell ran %v, want each call once", got)
		}
		got := e.outcomes()
		if got["toolu_root"] != tools.OutcomeOK || got["toolu_child"] != tools.OutcomeOK || got["toolu_s"] != tools.OutcomeOK || len(got) != 3 {
			t.Fatalf("outcomes %v", got)
		}
	})
}

// TestAMessageDeniesTheCallsThatWait: a person's message appended after
// a call that waits for a confirmation denies it, with the message as
// its note, in the session's thread and in a thread it spawned; a
// message appended before the call denies nothing, and a confirmation
// that came first stands.
func TestAMessageDeniesTheCallsThatWait(t *testing.T) {
	said := func(e *env, ctx context.Context, words string) session.Event {
		t.Helper()
		msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: words}}}, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.appendEvents(ctx, msg)
		return msg
	}
	denied := func(e *env, id string) session.ToolResult {
		t.Helper()
		for _, ev := range e.all() {
			var res session.ToolResult
			if ev.Type == session.TypeToolResult && ev.Decode(&res) == nil && res.ToolUseID == id {
				return res
			}
		}
		t.Fatalf("%s has no result", id)
		return session.ToolResult{}
	}

	t.Run("two asks of one step", func(t *testing.T) {
		e := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} })
		ctx := t.Context()
		e.stub.Script(model,
			reply(ir.StopToolUse, call("toolu_a", "bash", `{"command":"make a"}`), call("toolu_b", "bash", `{"command":"make b"}`)),
			luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("Using the Makefile's default target instead.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
				n := len(r.Messages)
				if n < 2 {
					return fmt.Errorf("%d messages", n)
				}
				last := r.Messages[n-1]
				if len(last.Blocks) != 3 || last.Blocks[0].ToolResult == nil || last.Blocks[1].ToolResult == nil || !last.Blocks[0].ToolResult.IsError || last.Blocks[2].Text != "Run plain make instead." {
					return fmt.Errorf("the model did not read both denials and then the message: %+v", last)
				}
				return nil
			}},
		)
		e.send(ctx, "Build.")
		if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
			t.Fatalf("outcome %+v", out)
		}
		said(e, ctx, "Run plain make instead.")
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the message %+v", out)
		}
		if got := e.write.ran(); len(got) != 0 {
			t.Fatalf("a denied call ran: %v", got)
		}
		for _, id := range []string{"toolu_a", "toolu_b"} {
			if res := denied(e, id); res.Outcome != tools.OutcomeDenied || !strings.Contains(res.Content[0].Text, "Run plain make instead.") {
				t.Fatalf("%s: %+v", id, res)
			}
		}
		if got := session.Awaiting(e.all()); len(got) != 0 {
			t.Fatalf("calls still await an answer: %v", got)
		}
	})

	t.Run("a message before the call denies nothing", func(t *testing.T) {
		e := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} })
		ctx := t.Context()
		e.stub.Script(model, reply(ir.StopToolUse, call("toolu_a", "bash", `{"command":"make a"}`)), reply(ir.StopEndTurn, text("done")))
		e.send(ctx, "Build.")
		if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
			t.Fatalf("outcome %+v", out)
		}
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation || len(e.outcomes()) != 0 {
			t.Fatalf("the message that asked for the work denied its call: %+v, %v", out, e.outcomes())
		}
		if got := session.Awaiting(e.all()); got["toolu_a"] != session.AnswerConfirmation {
			t.Fatalf("awaiting %v", got)
		}
	})

	t.Run("a confirmation that came first stands", func(t *testing.T) {
		e := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} })
		ctx := t.Context()
		e.stub.Script(model,
			reply(ir.StopToolUse, call("toolu_a", "bash", `{"command":"make a"}`), call("toolu_b", "bash", `{"command":"make b"}`)),
			reply(ir.StopEndTurn, text("done")),
		)
		e.send(ctx, "Build.")
		if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
			t.Fatalf("outcome %+v", out)
		}
		c, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: "toolu_a", Decision: session.DecisionAllow}, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.appendEvents(ctx, c)
		said(e, ctx, "Skip the second one.")
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the message %+v", out)
		}
		if got := e.write.ran(); !slices.Equal(got, []string{"toolu_a"}) {
			t.Fatalf("the shell ran %v, want the confirmed call alone", got)
		}
		if got := e.outcomes(); got["toolu_a"] != tools.OutcomeOK || got["toolu_b"] != tools.OutcomeDenied {
			t.Fatalf("outcomes %v", got)
		}
	})

	t.Run("a call a thread waits on", func(t *testing.T) {
		e := setup(t, func(c *Config) {
			withReviewer(func(s *Subagent) { s.Tools = []string{"bash"} })(c)
			c.Machine = fakeMachine{kind: machine.KindHost}
		})
		ctx := t.Context()
		e.stub.Script(model,
			reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Run the tests."}`)),
			reply(ir.StopEndTurn, text("Left untested.")),
		)
		e.stub.Script(reviewerModel,
			reply(ir.StopToolUse, call("toolu_child", "bash", `{"command":"make test"}`)),
			luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{text("Not run.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
				last := r.Messages[len(r.Messages)-1]
				if last.Blocks[0].ToolResult == nil || !strings.Contains(last.Blocks[0].ToolResult.Blocks[0].Text, "Do not run the tests.") {
					return errors.New("the thread did not read the denial with the person's message")
				}
				return nil
			}},
		)
		e.send(ctx, "Test it.")
		if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
			t.Fatalf("outcome %+v", out)
		}
		said(e, ctx, "Do not run the tests.")
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the message %+v", out)
		}
		if got := e.write.ran(); len(got) != 0 {
			t.Fatalf("a denied call ran: %v", got)
		}
		if got := e.outcomes(); got["toolu_child"] != tools.OutcomeDenied || got["toolu_s"] != tools.OutcomeOK {
			t.Fatalf("outcomes %v", got)
		}
	})
}
