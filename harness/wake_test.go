// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

// wakingTool is a fake wake tool: a set records a pending wake, a cancel
// clears it, and a set past the chain's bound is held, as the hosted
// wake tool does (spec 086).
type wakingTool struct {
	*fakeTool
	// counts are the wakes in a row each Confirm and Run read.
	confirmed, ran []int
}

const wakeSchema = `{"type":"object","properties":{"action":{"type":"string"}},"additionalProperties":false}`

func newWakingTool() *wakingTool {
	w := &wakingTool{fakeTool: &fakeTool{name: session.ToolWake, props: tools.Properties{Effect: tools.EffectRead}, schema: wakeSchema}}
	w.run = func(c tools.Call) (tools.Result, error) {
		w.ran = append(w.ran, c.State.Wakes)
		res := tools.Text(tools.OutcomeOK, "set")
		if string(c.Input) == `{"action":"cancel"}` {
			res.WakeCleared = &session.WakeCleared{TriggerID: "trg_w", Reason: session.WakeCanceled}
			return res, nil
		}
		res.WakeSet = &session.WakeSet{TriggerID: "trg_w", At: t0.Add(time.Hour), Title: "Look", Note: "Again.", Count: c.State.Wakes + 1}
		return res, nil
	}
	return w
}

func (w *wakingTool) Confirm(c tools.Call) string {
	w.confirmed = append(w.confirmed, c.State.Wakes)
	if c.State.Wakes >= 10 {
		return "the eleventh wake"
	}
	return ""
}

// wakeMessage is a wake's message, as its firing sends it.
func wakeMessage(t *testing.T, n int) session.Event {
	t.Helper()
	ev, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: session.Sender{Subject: "trigger:trg_w", Kind: session.SenderTrigger},
		Content: []lux.Block{{Type: ir.BlockText, Text: "Look again."}}, FiringID: "frg_1", Wake: &session.MessageWake{TriggerID: "trg_w", SetAt: t0, DueAt: t0, Count: n}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// TestEndOnIdleWaitsForAWake is Topos spec 086's end_on_idle criterion: a
// session that ends on idle stays open at the end of a turn that set a
// wake, records the wake beside the call's result, and ends completed at
// the idle after its last wake.
func TestEndOnIdleWaitsForAWake(t *testing.T) {
	wake := newWakingTool()
	e := setupSession(t, func(c *Config) {
		if err := c.Tools.Add(wake); err != nil {
			t.Fatal(err)
		}
	}, func(s *session.Session) { s.EndOnIdle = true })
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_w", session.ToolWake, `{"action":"set"}`)), reply(ir.StopEndTurn, text("I will look again in an hour.")))
	e.send(ctx, "Watch the fare for me.")
	if out := e.turn(ctx); out.Status != session.StatusIdle || out.StopReason != session.StopEndTurn {
		t.Fatalf("a turn that set a wake: %+v, want the session left open", out)
	}
	sets := e.events(ctx, session.TypeWakeSet)
	var set session.WakeSet
	if len(sets) != 1 || sets[0].Decode(&set) != nil || set.ToolUseID != "toolu_w" || set.TriggerID != "trg_w" || set.Count != 1 {
		t.Fatalf("the wake was recorded as %+v", set)
	}
	if s, err := e.store.Get(ctx, e.s.ID); err != nil || s.Wake == nil || s.Wake.TriggerID != "trg_w" {
		t.Fatalf("the header's wake is %+v, %v", s.Wake, err)
	}
	// The wake fires; its turn sets none, and the session ends.
	e.appendEvents(ctx, wakeMessage(t, 1))
	e.running(ctx)
	e.stub.Script(model, reply(ir.StopEndTurn, text("The fare is the same.")))
	if out := e.turn(ctx); out.Status != session.StatusEnded || out.StopReason != session.StopCompleted {
		t.Fatalf("the turn after the last wake: %+v, want the session ended", out)
	}
}

// TestACanceledWakeLetsTheSessionEnd: a turn that cancels its pending
// wake records the clear beside the call's result, and end_on_idle ends
// the session at that turn's idle.
func TestACanceledWakeLetsTheSessionEnd(t *testing.T) {
	wake := newWakingTool()
	e := setupSession(t, func(c *Config) {
		if err := c.Tools.Add(wake); err != nil {
			t.Fatal(err)
		}
	}, func(s *session.Session) { s.EndOnIdle = true })
	ctx := t.Context()
	set, err := session.NewEvent(session.TypeWakeSet, session.WakeSet{TriggerID: "trg_w", At: t0.Add(time.Hour), Title: "Look", Note: "Again.", ToolUseID: "toolu_0", Count: 1}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, set)
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_c", session.ToolWake, `{"action":"cancel"}`)), reply(ir.StopEndTurn, text("Nothing more to watch.")))
	e.send(ctx, "Stop watching.")
	if out := e.turn(ctx); out.Status != session.StatusEnded || out.StopReason != session.StopCompleted {
		t.Fatalf("a turn that canceled its wake: %+v, want the session ended", out)
	}
	var c session.WakeCleared
	if cl := e.events(ctx, session.TypeWakeCleared); len(cl) != 1 || cl[0].Decode(&c) != nil || c.ToolUseID != "toolu_c" || c.Reason != session.WakeCanceled {
		t.Fatalf("the cancel was recorded as %+v", c)
	}
}

// TestTheEleventhWakeAsks is Topos spec 086's chain criterion: after ten
// wakes in a row with no word from the initiator, the next set reads the
// count in its State and is held for the person; their allow is their
// word, so the set runs with the count started again; a message of
// another sender does not start it again.
func TestTheEleventhWakeAsks(t *testing.T) {
	wake := newWakingTool()
	e := setup(t, func(c *Config) {
		c.Policy = Policy{Mode: ModeConfirm, AlwaysAllow: []string{session.ToolWake}}
		if err := c.Tools.Add(wake); err != nil {
			t.Fatal(err)
		}
	})
	ctx := t.Context()
	e.send(ctx, "Watch the fare for me.")
	e.stub.Script(model, reply(ir.StopEndTurn, text("Watching.")))
	e.turn(ctx)
	other, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: session.Sender{Subject: "usr_bob", Kind: session.SenderPerson}, Content: []lux.Block{{Type: ir.BlockText, Text: "Hi."}}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 10; n++ {
		e.appendEvents(ctx, wakeMessage(t, n))
		if n == 5 {
			e.appendEvents(ctx, other)
		}
	}
	e.running(ctx)
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_11", session.ToolWake, `{"action":"set"}`)), reply(ir.StopEndTurn, text("Asked.")))
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("the eleventh wake: %+v, want a wait for the person", out)
	}
	if len(wake.confirmed) != 1 || wake.confirmed[0] != 10 || len(wake.ran) != 0 {
		t.Fatalf("the eleventh wake read %v and ran %v", wake.confirmed, wake.ran)
	}
	allow, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: "toolu_11", Decision: session.DecisionAllow}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, allow)
	e.running(ctx)
	e.stub.Script(model, reply(ir.StopEndTurn, text("Watching again.")))
	e.turn(ctx)
	if len(wake.ran) != 1 || wake.ran[0] != 0 {
		t.Fatalf("the allowed wake ran with %v wakes in a row, want the count started again", wake.ran)
	}
	var set session.WakeSet
	if sets := e.events(ctx, session.TypeWakeSet); len(sets) != 1 || sets[0].Decode(&set) != nil || set.Count != 1 {
		t.Fatalf("the allowed wake was recorded as %+v", set)
	}
}

// TestAWakesMessageSaysWhenItWasDue: the fold leads a wake's message with
// when it was set and due and its place in the chain, and says it came
// late when it fired past its time by more than the loop's pass.
func TestAWakesMessageSaysWhenItWasDue(t *testing.T) {
	onTime, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: session.Sender{Subject: "trigger:trg_w", Kind: session.SenderTrigger},
		Content: []lux.Block{{Type: ir.BlockText, Text: "Look again."}}, FiringID: "frg_1", Wake: &session.MessageWake{TriggerID: "trg_w", SetAt: t0.Add(-time.Hour), DueAt: t0, Count: 3}}, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	late := onTime
	late.Time = t0.Add(3 * time.Hour)
	for name, c := range map[string]struct {
		ev   session.Event
		late bool
	}{"on time": {onTime, false}, "late": {late, true}} {
		c.ev.Seq = 1
		tr, err := session.Fold([]session.Event{c.ev}, "")
		if err != nil {
			t.Fatal(err)
		}
		lead := tr.Messages[0].Blocks[0].Text
		if !strings.Contains(lead, "wake 3 in a row") || !strings.Contains(lead, t0.Format(time.RFC3339)) || strings.Contains(lead, "It came late") != c.late {
			t.Errorf("%s: the wake's message is led by %q", name, lead)
		}
		if tr.Messages[0].Blocks[1].Text != "Look again." {
			t.Errorf("%s: the note reads %q", name, tr.Messages[0].Blocks[1].Text)
		}
	}
}
