// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package triggers

import (
	"errors"
	"testing"
	"time"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// runOfFiring is the run the store keeps for firing id.
func (r *rig) runOfFiring(id string) (store.Run, bool) {
	r.t.Helper()
	rs, err := r.objects.Runs(r.t.Context(), "alice", time.Time{})
	if err != nil {
		r.t.Fatal(err)
	}
	for _, run := range rs {
		if run.FiringID == id {
			return run, true
		}
	}
	return store.Run{}, false
}

// TestAFiringRecordsItsRun (spec 088): a firing that starts or continues a
// session records its run, not yet ended; one skipped at its allowance or
// refused records an ended run with no cost, a refusal with its reason; a
// failed one, which runs again, records none; a wake's run names the
// trigger that started its session, or none for a person's, and outlives
// the wake; and a late wake records none.
func TestAFiringRecordsItsRun(t *testing.T) {
	r := newRig(t)
	tr := r.put("watch", watching(1000))
	started := r.fire(tr, nil, store.OutcomeStarted)
	run, ok := r.runOfFiring(started.ID)
	if !ok || run != (store.Run{FiringID: started.ID, Owner: "alice", TriggerID: tr.ID, SessionID: started.SessionID, At: started.Time, Outcome: store.OutcomeStarted}) {
		t.Fatalf("a started firing's run: %+v, %v", run, ok)
	}
	r.spendIn(started.SessionID, 1200)
	skipped := r.fire(tr, nil, store.OutcomeSkippedBudget)
	if run, ok := r.runOfFiring(skipped.ID); !ok || run.Outcome != store.OutcomeSkippedBudget || !run.Ended || run.SessionID != "" || run.CostUSDMicro != 0 {
		t.Fatalf("a skipped firing's run: %+v, %v", run, ok)
	}

	other := r.put("other", watching(0))
	r.actor.startErr = errors.New("refused")
	refused := r.fire(other, nil, store.OutcomeRefused)
	if run, ok := r.runOfFiring(refused.ID); !ok || run.Outcome != store.OutcomeRefused || run.Reason != "forbidden" || !run.Ended {
		t.Fatalf("a refused firing's run: %+v, %v", run, ok)
	}
	r.actor.startErr = errBusy
	failed := r.fire(other, nil, store.OutcomeFailed)
	if _, ok := r.runOfFiring(failed.ID); ok {
		t.Fatal("a failed firing, which runs again, recorded a run")
	}
	r.actor.startErr = nil

	// A wake of the session the routine started counts toward it; its run
	// outlives the wake, which its firing deletes.
	wake := r.putWake(started.SessionID, r.clock.Now().Add(time.Minute), "24h")
	if err := r.objects.DeleteTrigger(t.Context(), tr.ID); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Minute)
	r.tick()
	rs, err := r.objects.Runs(t.Context(), "alice", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	last := rs[len(rs)-1]
	if last.TriggerID != wake.ID || !last.Wake || last.SessionTriggerID != tr.ID || last.SessionID != started.SessionID || last.Outcome != store.OutcomeContinued || last.Ended {
		t.Fatalf("a wake's run: %+v", last)
	}
	if _, err := r.objects.Trigger(t.Context(), wake.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the wake that fired: %v", err)
	}

	// A wake of a person's session names no trigger; one past its maxAge
	// is late and records no run.
	own := r.open()
	r.status(own.ID, session.StatusIdle, session.StopEndTurn)
	r.putWake(own.ID, r.clock.Now().Add(time.Minute), "24h")
	r.clock.Advance(time.Minute)
	r.tick()
	if rs, _ = r.objects.Runs(t.Context(), "alice", time.Time{}); rs[len(rs)-1].SessionID != own.ID || rs[len(rs)-1].SessionTriggerID != "" || !rs[len(rs)-1].Wake {
		t.Fatalf("a person's wake's run: %+v", rs[len(rs)-1])
	}
	n := len(rs)
	r.status(own.ID, session.StatusIdle, session.StopEndTurn)
	r.putWake(own.ID, r.clock.Now().Add(time.Minute), "1m")
	r.clock.Advance(time.Hour)
	r.tick()
	if rs, _ = r.objects.Runs(t.Context(), "alice", time.Time{}); len(rs) != n {
		t.Fatalf("a late wake recorded a run: %+v", rs[len(rs)-1])
	}
}

// TestTheCountWritesEachRunsCost (spec 088): the count at a firing reads
// the logs of the trigger's sessions and writes each look's cost to its
// run, ended once its turn closed and so far while it runs; an ended
// run's cost is final.
func TestTheCountWritesEachRunsCost(t *testing.T) {
	r := newRig(t)
	tr := r.put("watch", watching(100000))
	first := r.fire(tr, nil, store.OutcomeStarted)
	r.spendIn(first.SessionID, 300)
	second := r.fire(tr, nil, store.OutcomeStarted)
	if run, _ := r.runOfFiring(first.ID); run.CostUSDMicro != 300 || !run.Ended {
		t.Fatalf("the first run after the second firing's count: %+v", run)
	}
	if run, _ := r.runOfFiring(second.ID); run.CostUSDMicro != 0 || run.Ended {
		t.Fatalf("the second run before its turn: %+v", run)
	}
	// The second turn is running and has spent 40 so far.
	s, err := r.sessions.Get(t.Context(), second.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	running, _ := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning}, r.clock.Now())
	cost, _ := session.NewEvent(session.TypeModelRequest, session.ModelRequest{Model: "m", Outcome: "ok", CostUSDMicro: new(int64(40))}, r.clock.Now())
	batch := []session.Event{running, cost}
	session.Stamp(s.ID, s.LastSeq, batch)
	if _, err := r.sessions.Append(t.Context(), s.ID, s.LastSeq, batch); err != nil {
		t.Fatal(err)
	}
	r.fire(tr, nil, store.OutcomeStarted)
	if run, _ := r.runOfFiring(second.ID); run.CostUSDMicro != 40 || run.Ended {
		t.Fatalf("a run still running: %+v", run)
	}
	if err := r.objects.SetRunCosts(t.Context(), []store.RunCost{{FiringID: first.ID, CostUSDMicro: 1}}); err != nil {
		t.Fatal(err)
	}
	if run, _ := r.runOfFiring(first.ID); run.CostUSDMicro != 300 {
		t.Fatalf("an ended run's cost moved: %+v", run)
	}
}

// TestTheMinuteLoopPrunesOldRuns (spec 088): each pass deletes the runs
// older than trigger.RunsKept, and keeps the others.
func TestTheMinuteLoopPrunesOldRuns(t *testing.T) {
	r := newRig(t)
	now := r.clock.Now()
	old := store.Run{FiringID: session.NewID(session.PrefixFiring), Owner: "alice", TriggerID: "trg_1", At: now.Add(-trigger.RunsKept - time.Minute), Outcome: store.OutcomeStarted, Ended: true}
	kept := store.Run{FiringID: session.NewID(session.PrefixFiring), Owner: "alice", TriggerID: "trg_1", At: now.Add(-trigger.RunsKept + time.Minute), Outcome: store.OutcomeStarted, Ended: true}
	for _, run := range []store.Run{old, kept} {
		if err := r.objects.PutRun(t.Context(), run); err != nil {
			t.Fatal(err)
		}
	}
	r.tick()
	if rs, err := r.objects.Runs(t.Context(), "alice", time.Time{}); err != nil || len(rs) != 1 || rs[0].FiringID != kept.FiringID {
		t.Fatalf("the runs after a pass: %+v, %v", rs, err)
	}
}

// TestFireTimes (spec 088): a schedule's fire times are walked in its zone
// after a time and before an end, cut at a bound with the cut said; a
// one-shot's is its time when it falls between; an event trigger and a
// suspended one have none; and a zone the build does not know is an error.
func TestFireTimes(t *testing.T) {
	from := time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC)
	until := from.Add(72 * time.Hour)
	daily := v1.TriggerSpec{Schedule: "0 9 * * *", TimeZone: "Europe/Berlin"}
	got, more, err := FireTimes(daily, from, until, 10)
	// Berlin leaves summer time early on October 25, so its 9:00 is 7:00
	// UTC on the 24th and 8:00 UTC after.
	want := []time.Time{time.Date(2026, 10, 24, 7, 0, 0, 0, time.UTC), time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)}
	if err != nil || more || len(got) != len(want) {
		t.Fatalf("a daily schedule's times: %v, %v, %v", got, more, err)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("time %d = %v, want %v", i, got[i], want[i])
		}
	}
	if got, more, err := FireTimes(daily, from, until, 2); err != nil || !more || len(got) != 2 {
		t.Fatalf("a daily schedule cut at two: %v, %v, %v", got, more, err)
	}
	at := from.Add(time.Hour)
	for _, c := range []struct {
		name string
		spec v1.TriggerSpec
		want int
	}{
		{"a one-shot between", v1.TriggerSpec{At: &at}, 1},
		{"a one-shot past", v1.TriggerSpec{At: new(from.Add(-time.Hour))}, 0},
		{"a one-shot after the end", v1.TriggerSpec{At: new(until.Add(time.Hour))}, 0},
		{"an event trigger", v1.TriggerSpec{On: &v1.TriggerOn{Product: "github", Verbs: []string{"push"}}}, 0},
		{"a suspended schedule", v1.TriggerSpec{Schedule: "0 9 * * *", TimeZone: "UTC", Suspend: true}, 0},
	} {
		if got, more, err := FireTimes(c.spec, from, until, 10); err != nil || more || len(got) != c.want {
			t.Errorf("%s: %v, %v, %v", c.name, got, more, err)
		}
	}
	if _, _, err := FireTimes(v1.TriggerSpec{Schedule: "0 9 * * *", TimeZone: "Mars/Olympus"}, from, until, 10); err == nil {
		t.Error("a zone the build does not know")
	}
}
