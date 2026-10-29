// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package triggers

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest/trigger"
	"latere.ai/x/topos/session"
)

// splitting is a schedule trigger whose reach splits by scope (spec 095),
// firing at 03:00 UTC, with extra appended to its spec.
func splitting(extra string) string {
	return `{"agent":"agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C","schedule":"0 3 * * *","timeZone":"UTC","session":{"message":"Tidy.","endOnIdle":false},` +
		`"skipIfActive":true,"maxAge":"1h","suspend":false,"reach":{"kind":"memory_tidy","split":"scope"}` + extra + `}`
}

// runsOf are the runs the store keeps for firing id, by scope.
func (r *rig) runsOf(id string) []store.Run {
	r.t.Helper()
	rs, err := r.objects.Runs(r.t.Context(), "alice", time.Time{})
	if err != nil {
		r.t.Fatal(err)
	}
	return slices.DeleteFunc(rs, func(run store.Run) bool { return run.FiringID != id })
}

// keyed is the session a trigger's key names, empty for none.
func (r *rig) keyed(triggerID, key string) string {
	r.t.Helper()
	id, err := r.objects.TriggerSession(r.t.Context(), triggerID, key)
	if errors.Is(err, store.ErrNotFound) {
		return ""
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

// TestASplitFiring (spec 095): a split firing asks trigger.runs with the
// trigger, the firing and each scope's last started run; nothing due
// records skipped_nothing_new, a run that cost nothing, with no start;
// two due scopes start two sessions, each carrying the reach and keyed by
// its scope, a run each; a deny is refused with its reason and when its
// limit resets; an invalid answer is runs_invalid; an authorizer that does
// not answer fails the firing; and a trigger with no split asks nothing.
func TestASplitFiring(t *testing.T) {
	r := newRig(t)
	plain := r.put("plain", watching(0))
	r.fire(plain, nil, store.OutcomeStarted)
	kinded := r.put("kinded", `{"agent":"agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C","timeZone":"UTC","on":{"product":"github","verbs":["*"]},"session":{"message":"Look.","endOnIdle":false},"skipIfActive":false,"maxAge":"1h","suspend":false,"reach":{"kind":"inbox"}}`)
	r.fire(kinded, nil, store.OutcomeStarted)
	if len(r.actor.asks) != 0 || r.actor.reaches[0] != nil || *r.actor.reaches[1] != (StartReach{Kind: "inbox"}) {
		t.Fatalf("a trigger with no split asked %+v and started with %+v", r.actor.asks, r.actor.reaches)
	}
	r.actor.reaches = nil
	starts := r.actor.starts

	tr := r.put("tidy", splitting(""))
	nothing := r.fire(tr, nil, store.OutcomeSkippedNothingNew)
	if r.actor.starts != starts || len(r.actor.asks) != 1 {
		t.Fatalf("nothing due: %d starts, %d asks", r.actor.starts-starts, len(r.actor.asks))
	}
	ask := r.actor.asks[0]
	if ask.Trigger.ID != tr.ID || ask.Spec.Reach.Kind != "memory_tidy" || ask.Firing.ID != nothing.ID || ask.Firing.Origin != store.OriginManual ||
		!ask.Firing.Time.Equal(nothing.Time) || ask.Last == nil || len(ask.Last) != 0 {
		t.Fatalf("the first ask: %+v", ask)
	}
	if rs := r.runsOf(nothing.ID); len(rs) != 1 || rs[0] != (store.Run{FiringID: nothing.ID, Owner: "alice", TriggerID: tr.ID, At: nothing.Time,
		Outcome: store.OutcomeSkippedNothingNew, Ended: true}) {
		t.Fatalf("the run of nothing due: %+v", rs)
	}
	if got, err := r.objects.Trigger(t.Context(), tr.ID); err != nil || got.Counts.SkippedNothingNew != 1 || got.Counts.Started != 0 {
		t.Fatalf("the counts after nothing due: %+v, %v", got.Counts, err)
	}

	since := r.clock.Now().Add(-24 * time.Hour)
	r.actor.due = []DueRun{{Scope: "you", Since: &since}, {Scope: "projects/prj_01JA4R"}}
	r.clock.Advance(time.Hour)
	two := r.fire(tr, nil, store.OutcomeStarted)
	if r.actor.starts != starts+2 || len(r.actor.reaches) != 2 ||
		*r.actor.reaches[0] != (StartReach{Kind: "memory_tidy", Scope: "you", Since: &since}) ||
		*r.actor.reaches[1] != (StartReach{Kind: "memory_tidy", Scope: "projects/prj_01JA4R"}) {
		t.Fatalf("two due: %d starts with %+v", r.actor.starts-starts, r.actor.reaches)
	}
	rs := r.runsOf(two.ID)
	if len(rs) != 2 || rs[0].Scope != "projects/prj_01JA4R" || rs[1].Scope != "you" || rs[1].SessionID != two.SessionID || rs[0].SessionID == two.SessionID ||
		rs[0].Outcome != store.OutcomeStarted || rs[0].Ended || rs[1].Outcome != store.OutcomeStarted {
		t.Fatalf("two due: the firing %+v, its runs %+v", two, rs)
	}
	if r.keyed(tr.ID, "you") != two.SessionID || r.keyed(tr.ID, "projects/prj_01JA4R") != rs[0].SessionID {
		t.Fatalf("the scopes' keys: %q, %q", r.keyed(tr.ID, "you"), r.keyed(tr.ID, "projects/prj_01JA4R"))
	}
	for _, run := range rs {
		s, err := r.sessions.Get(t.Context(), run.SessionID)
		if err != nil || s.TriggerID != tr.ID {
			t.Fatalf("the session of %s: %+v, %v", run.Scope, s, err)
		}
	}

	// Each scope's last started run is in the next ask.
	r.actor.due = nil
	r.clock.Advance(time.Hour)
	r.fire(tr, nil, store.OutcomeSkippedNothingNew)
	if last := r.actor.asks[len(r.actor.asks)-1].Last; len(last) != 2 || !last["you"].Equal(two.Time) || !last["projects/prj_01JA4R"].Equal(two.Time) {
		t.Fatalf("the last runs asked: %+v", last)
	}

	until := r.clock.Now().Add(6 * time.Hour)
	r.actor.runsErr = &resetting{at: until}
	denied := r.fire(tr, nil, store.OutcomeRefused)
	if denied.Reason != "share_spent" || denied.Until == nil || !denied.Until.Equal(until) {
		t.Fatalf("a deny: %+v", denied)
	}
	if rs := r.runsOf(denied.ID); len(rs) != 1 || rs[0].Outcome != store.OutcomeRefused || rs[0].Reason != "share_spent" || rs[0].Scope != "" {
		t.Fatalf("a deny's run: %+v", rs)
	}
	r.actor.runsErr = errBusy
	r.fire(tr, nil, store.OutcomeFailed)

	many := make([]DueRun, trigger.MaxRunsPerFiring+1)
	for i := range many {
		many[i].Scope = fmt.Sprintf("projects/p%d", i)
	}
	starts = r.actor.starts
	for name, answer := range map[string]struct {
		due []DueRun
		err error
	}{
		"twice":      {due: []DueRun{{Scope: "you"}, {Scope: "you"}}},
		"no scope":   {due: []DueRun{{}}},
		"a space":    {due: []DueRun{{Scope: "a b"}}},
		"too many":   {due: many},
		"unreadable": {err: fmt.Errorf("limits: %w", ErrRunsInvalid)},
	} {
		r.actor.due, r.actor.runsErr = answer.due, answer.err
		invalid := r.fire(tr, nil, store.OutcomeRefused)
		if invalid.Reason != ReasonRunsInvalid {
			t.Errorf("%s: %+v", name, invalid)
		}
	}
	if r.actor.starts != starts {
		t.Fatalf("an invalid answer started %d sessions", r.actor.starts-starts)
	}
}

// TestASplitFiringAtItsSchedule (spec 095): the minute loop's firing of a
// split trigger asks as a schedule firing, at its scheduled time.
func TestASplitFiringAtItsSchedule(t *testing.T) {
	r := newRig(t)
	tr := r.put("tidy", splitting(""))
	r.actor.due = []DueRun{{Scope: "you"}}
	r.clock.Advance(16 * time.Hour)
	r.tick()
	if len(r.actor.asks) != 1 {
		t.Fatalf("%d asks at the schedule", len(r.actor.asks))
	}
	ask := r.actor.asks[0]
	if want := time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC); ask.Firing.Origin != store.OriginSchedule || !ask.Firing.Time.Equal(want) {
		t.Fatalf("the schedule's ask: %+v", ask.Firing)
	}
	fs := r.firings(tr.ID)
	if len(fs) != 1 || fs[0].Outcome != store.OutcomeStarted || r.keyed(tr.ID, "you") != fs[0].SessionID {
		t.Fatalf("the schedule's firing: %+v", fs)
	}
}

// TestASplitFiringsScopesEachStandAlone (spec 095): a scope whose session
// is still active is passed over and recorded nowhere while the others
// start; one the authorizer refuses is a refused run while the others
// start; one whose start fails is recorded nowhere; and a firing whose
// every scope was refused, failed or passed over records so. With
// skipIfActive false, a scope's active session does not hold back its
// next one; maxActive holds back no scope.
func TestASplitFiringsScopesEachStandAlone(t *testing.T) {
	r := newRig(t)
	tr := r.put("tidy", splitting(""))
	r.actor.due = []DueRun{{Scope: "you"}}
	first := r.fire(tr, nil, store.OutcomeStarted)

	// you's session is still active; the project's starts.
	r.clock.Advance(time.Hour)
	r.actor.due = []DueRun{{Scope: "you"}, {Scope: "projects/p1"}}
	second := r.fire(tr, nil, store.OutcomeStarted)
	if rs := r.runsOf(second.ID); len(rs) != 1 || rs[0].Scope != "projects/p1" || rs[0].SessionID != second.SessionID {
		t.Fatalf("a scope passed over: %+v", rs)
	}
	if r.keyed(tr.ID, "you") != first.SessionID {
		t.Fatal("a scope passed over lost its key")
	}

	// Every scope passed over.
	r.clock.Advance(time.Hour)
	passed := r.fire(tr, nil, store.OutcomeSkippedActive)
	if rs := r.runsOf(passed.ID); len(rs) != 0 {
		t.Fatalf("a firing passed over has runs %+v", rs)
	}

	// you's session ends its turn; the project's is refused, you's starts.
	r.spendIn(first.SessionID, 10)
	r.spendIn(second.SessionID, 10)
	r.clock.Advance(time.Hour)
	r.actor.refuse = map[string]error{"projects/p1": errors.New("refused")}
	third := r.fire(tr, nil, store.OutcomeStarted)
	rs := r.runsOf(third.ID)
	if len(rs) != 2 || rs[0].Scope != "projects/p1" || rs[0].Outcome != store.OutcomeRefused || rs[0].Reason != "forbidden" || !rs[0].Ended || rs[0].SessionID != "" ||
		rs[1].Scope != "you" || rs[1].Outcome != store.OutcomeStarted || rs[1].SessionID != third.SessionID || third.SessionID == first.SessionID {
		t.Fatalf("one scope refused: the firing %+v, its runs %+v", third, rs)
	}

	// Every scope refused: the firing is refused with the first reason.
	r.spendIn(third.SessionID, 10)
	r.clock.Advance(time.Hour)
	until := r.clock.Now().Add(time.Hour)
	r.actor.refuse = map[string]error{"you": &resetting{at: until}, "projects/p1": errors.New("refused")}
	refused := r.fire(tr, nil, store.OutcomeRefused)
	if refused.Reason != "share_spent" || refused.Until == nil || !refused.Until.Equal(until) || len(r.runsOf(refused.ID)) != 2 {
		t.Fatalf("every scope refused: %+v, runs %+v", refused, r.runsOf(refused.ID))
	}

	// Every start failed: the firing failed and keeps no run.
	r.clock.Advance(time.Hour)
	r.actor.refuse = map[string]error{"you": errBusy, "projects/p1": errBusy}
	failed := r.fire(tr, nil, store.OutcomeFailed)
	if rs := r.runsOf(failed.ID); len(rs) != 0 {
		t.Fatalf("a failed firing's runs: %+v", rs)
	}
	// One failed and one refused: refused, with the refused scope's run.
	r.clock.Advance(time.Hour)
	r.actor.refuse = map[string]error{"you": errBusy, "projects/p1": errors.New("refused")}
	mixed := r.fire(tr, nil, store.OutcomeRefused)
	if rs := r.runsOf(mixed.ID); len(rs) != 1 || rs[0].Scope != "projects/p1" {
		t.Fatalf("one failed and one refused: %+v", rs)
	}

	// With skipIfActive false and maxActive 1, an active scope starts
	// again and a second scope starts beside it.
	free := r.put("free", `{"agent":"agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C","schedule":"0 3 * * *","timeZone":"UTC","session":{"message":"Tidy.","endOnIdle":false},`+
		`"skipIfActive":false,"maxActive":1,"maxAge":"1h","suspend":false,"reach":{"kind":"memory_tidy","split":"scope"}}`)
	r.actor.refuse = nil
	r.actor.due = []DueRun{{Scope: "you"}, {Scope: "projects/p1"}}
	a := r.fire(free, nil, store.OutcomeStarted)
	b := r.fire(free, nil, store.OutcomeStarted)
	if len(r.runsOf(a.ID)) != 2 || len(r.runsOf(b.ID)) != 2 || a.SessionID == b.SessionID {
		t.Fatalf("skipIfActive false: %+v, %+v", r.runsOf(a.ID), r.runsOf(b.ID))
	}
}

// TestASplitFiringsRunsCountTogether (spec 095): the looks of every
// session one split firing starts count toward the trigger's allowance,
// the second session's too, once each has ended; a firing at the
// allowance asks nothing and starts nothing; and each run's cost is its
// own session's.
func TestASplitFiringsRunsCountTogether(t *testing.T) {
	r := newRig(t)
	tr := r.put("tidy", splitting(`,"budget":{"perWeek":1000}`))
	r.actor.due = []DueRun{{Scope: "you"}, {Scope: "projects/p1"}}
	first := r.fire(tr, nil, store.OutcomeStarted)
	rs := r.runsOf(first.ID)
	if len(rs) != 2 {
		t.Fatalf("the first firing's runs: %+v", rs)
	}
	for _, run := range rs {
		cost := int64(300)
		if run.Scope == "you" {
			cost = 400
		}
		r.spendIn(run.SessionID, cost)
		// A session that ended counts through the runs that name it alone.
		r.status(run.SessionID, session.StatusEnded, session.StopEndTurn)
	}
	// Both scopes' sessions ended, the second's known by its key alone:
	// each starts again.
	r.clock.Advance(time.Hour)
	second := r.fire(tr, nil, store.OutcomeStarted)
	if m := r.lastSpend(); m == nil || m.SpentUSDMicro != 700 {
		t.Fatalf("the second firing's message carried %+v", m)
	}
	again := r.runsOf(second.ID)
	if len(again) != 2 || again[0].SessionID == rs[0].SessionID || again[1].SessionID == rs[1].SessionID || r.keyed(tr.ID, "projects/p1") != again[0].SessionID {
		t.Fatalf("the second firing's runs: %+v after %+v", again, rs)
	}
	for _, run := range r.runsOf(first.ID) {
		want := map[string]int64{"you": 400, "projects/p1": 300}[run.Scope]
		if run.CostUSDMicro != want || !run.Ended {
			t.Fatalf("the cost of %s's run: %+v", run.Scope, run)
		}
	}
	r.spendIn(second.SessionID, 400)
	r.clock.Advance(time.Hour)
	r.actor.due = []DueRun{{Scope: "you"}}
	asks := len(r.actor.asks)
	r.fire(tr, nil, store.OutcomeSkippedBudget)
	if len(r.actor.asks) != asks {
		t.Fatal("a firing at its allowance asked trigger.runs")
	}
}
