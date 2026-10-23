// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package triggers

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// watching is a trigger spec that fires on events, each firing starting
// a new session, its allowance perWeek (none for 0). The tests fire it by
// hand, so no schedule fires beside them.
func watching(perWeek int64) string {
	budget := ""
	if perWeek > 0 {
		budget = fmt.Sprintf(`,"budget":{"perWeek":%d}`, perWeek)
	}
	return `{"agent":"agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C","timeZone":"UTC","on":{"product":"github","verbs":["*"]},"session":{"message":"Look.","endOnIdle":false},"skipIfActive":false,"maxAge":"1h","suspend":false` + budget + `}`
}

// spendIn closes the turn the latest message of session id began, as its
// runner does, with a model request that cost micro.
func (r *rig) spendIn(id string, micro int64) {
	r.t.Helper()
	s, err := r.sessions.Get(r.t.Context(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	var batch []session.Event
	for _, p := range []struct {
		typ     session.Type
		payload any
	}{
		{session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning}},
		{session.TypeModelRequest, session.ModelRequest{Model: "m", Outcome: "ok", CostUSDMicro: new(micro)}},
		{session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn}},
	} {
		ev, err := session.NewEvent(p.typ, p.payload, r.clock.Now())
		if err != nil {
			r.t.Fatal(err)
		}
		batch = append(batch, ev)
	}
	session.Stamp(id, s.LastSeq, batch)
	if _, err := r.sessions.Append(r.t.Context(), id, s.LastSeq, batch); err != nil {
		r.t.Fatal(err)
	}
}

// spendOf is the trigger's status.spend as the store keeps it.
func (r *rig) spendOf(id string) v1.TriggerSpend {
	r.t.Helper()
	t, err := r.objects.Trigger(r.t.Context(), id)
	if err != nil || t.Spend == nil {
		r.t.Fatalf("the spend of %s: %+v, %v", id, t, err)
	}
	return *t.Spend
}

// lastSpend is the spend the latest firing's message carried.
func (r *rig) lastSpend() *session.MessageSpend {
	r.t.Helper()
	if len(r.actor.spends) == 0 {
		r.t.Fatal("no firing's message was sent")
	}
	return r.actor.spends[len(r.actor.spends)-1]
}

// tick runs one pass of the minute loop.
func (r *rig) tick() {
	r.t.Helper()
	if err := r.engine.Tick(r.t.Context()); err != nil {
		r.t.Fatal(err)
	}
}

// putWake stores a wake of session id, a one-shot at at that continues
// it, with the origin a wake's set writes.
func (r *rig) putWake(id string, at time.Time, maxAge string) store.Trigger {
	r.t.Helper()
	tid := session.NewID(session.PrefixTrigger)
	doc := fmt.Sprintf(`{"apiVersion":"topos.latere.ai/v1","kind":"Trigger","metadata":{"name":"wake-x"},"spec":%s,"status":{"id":%q,"version":1,"digest":"sha256:1","origin":{"kind":"wake","sessionId":%q,"toolUseId":"toolu_w"}}}`,
		oneShot(at, id, maxAge), tid, id)
	t := store.Trigger{ID: tid, Name: "wake-" + tid, Owner: "alice", AgentID: "agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C", Version: 1, Digest: "sha256:1", Doc: []byte(doc), CreatedAt: r.clock.Now()}
	next := at.UTC()
	t.NextFireAt = &next
	if err := r.objects.PutTrigger(r.t.Context(), t); err != nil {
		r.t.Fatal(err)
	}
	return t
}

// TestAFiringAtItsAllowanceStartsNothing (spec 087): a firing while the
// trigger's spend of the last 7 days is at or past its allowance records
// skipped_budget and starts nothing, its spend kept on the trigger; once
// the window rolls past the oldest look the next firing runs, with
// nothing resumed by hand; and a trigger with no allowance always fires.
func TestAFiringAtItsAllowanceStartsNothing(t *testing.T) {
	r := newRig(t)
	tr := r.put("watch", watching(1000))
	first := r.fire(tr, nil, store.OutcomeStarted)
	if m := r.lastSpend(); m == nil || m.TriggerID != tr.ID || m.SpentUSDMicro != 0 || m.PerWeekUSDMicro == nil || *m.PerWeekUSDMicro != 1000 || m.PerLookUSDMicro != nil {
		t.Fatalf("the first firing's message carried %+v", m)
	}
	r.spendIn(first.SessionID, 600)
	r.clock.Advance(time.Hour)
	second := r.fire(tr, nil, store.OutcomeStarted)
	if m := r.lastSpend(); m.SpentUSDMicro != 600 || m.PerLookUSDMicro == nil || *m.PerLookUSDMicro != 600 {
		t.Fatalf("the second firing's message carried %+v", m)
	}
	r.spendIn(second.SessionID, 500)
	r.clock.Advance(time.Hour)
	r.fire(tr, nil, store.OutcomeSkippedBudget)
	if r.actor.starts != 2 {
		t.Fatalf("%d starts; a firing at the allowance starts nothing", r.actor.starts)
	}
	if sp := r.spendOf(tr.ID); sp.SpentUSDMicro != 1100 || sp.WindowDays != 7 || *sp.PerLookUSDMicro != 550 {
		t.Fatalf("the spend at the allowance %+v", sp)
	}
	if got, err := r.objects.Trigger(t.Context(), tr.ID); err != nil || got.Counts.SkippedBudget != 1 || got.Counts.Started != 2 {
		t.Fatalf("counts %+v, %v", got.Counts, err)
	}
	// The window rolls past the first look: 500 of 1000 are spent, and
	// the next firing runs.
	r.clock.Advance(7*24*time.Hour - 2*time.Hour + time.Minute)
	r.fire(tr, nil, store.OutcomeStarted)
	if sp := r.spendOf(tr.ID); sp.SpentUSDMicro != 500 {
		t.Fatalf("the spend after the window rolled %+v", sp)
	}

	free := r.put("free", watching(0))
	for range 3 {
		f := r.fire(free, nil, store.OutcomeStarted)
		r.spendIn(f.SessionID, 5000)
	}
	if sp := r.spendOf(free.ID); sp.PerWeekUSDMicro != nil || sp.SpentUSDMicro != 10000 || r.lastSpend().PerWeekUSDMicro != nil {
		t.Fatalf("a trigger with no allowance %+v", sp)
	}
}

// TestAWakeCountsTowardTheTriggerThatStartedItsSession (spec 087): the
// looks wakes began in a session a trigger started count toward that
// trigger, the wake's message naming it; a wake of such a session is
// skipped at the trigger's allowance; and a wake of a session no trigger
// started counts toward none and carries no spend.
func TestAWakeCountsTowardTheTriggerThatStartedItsSession(t *testing.T) {
	r := newRig(t)
	tr := r.put("watch", watching(1000))
	started := r.fire(tr, nil, store.OutcomeStarted)
	r.spendIn(started.SessionID, 300)

	wake := r.putWake(started.SessionID, r.clock.Now().Add(time.Minute), "24h")
	r.clock.Advance(time.Minute)
	r.tick()
	if m := r.lastSpend(); r.actor.sends != 1 || m == nil || m.TriggerID != tr.ID || m.SpentUSDMicro != 300 {
		t.Fatalf("the wake under the allowance: %d sends, its message carried %+v", r.actor.sends, m)
	}
	if _, err := r.objects.Trigger(t.Context(), wake.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the wake that fired: %v", err)
	}
	r.spendIn(started.SessionID, 800)

	// The wake's look counted toward the routine: 1100 of 1000, and the
	// next wake is skipped and kept.
	next := r.putWake(started.SessionID, r.clock.Now().Add(time.Minute), "24h")
	r.clock.Advance(time.Minute)
	r.tick()
	kept, err := r.objects.Trigger(t.Context(), next.ID)
	if err != nil || kept.Counts.SkippedBudget != 1 || r.actor.sends != 1 {
		t.Fatalf("the wake at the routine's allowance: %+v, %v, %d sends", kept.Counts, err, r.actor.sends)
	}
	if sp := r.spendOf(tr.ID); sp.SpentUSDMicro != 1100 {
		t.Fatalf("the routine's spend %+v", sp)
	}

	// Once the routine is deleted, its session's wake counts toward none
	// and fires; a wake of a session that is gone is skipped_ended.
	if err := r.objects.DeleteTrigger(t.Context(), tr.ID); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Minute)
	r.tick()
	if r.actor.sends != 2 || r.lastSpend() != nil {
		t.Fatalf("the wake of a deleted routine's session: %d sends, spend %+v", r.actor.sends, r.lastSpend())
	}
	r.putWake("ses_01JZZZZZZZZZZZZZZZZZZZZZZZ", r.clock.Now().Add(time.Minute), "24h")
	r.clock.Advance(time.Minute)
	r.tick()
	if last := r.actor.spent[len(r.actor.spent)-1]; last.Outcome != store.OutcomeSkippedEnded {
		t.Fatalf("the wake of a session that is gone: %+v", last)
	}

	// A wake of a session a person started counts toward none.
	own := r.open()
	r.status(own.ID, session.StatusIdle, session.StopEndTurn)
	r.putWake(own.ID, r.clock.Now().Add(time.Minute), "24h")
	r.clock.Advance(time.Minute)
	r.tick()
	if r.actor.sends != 3 || r.lastSpend() != nil {
		t.Fatalf("a person's session's wake: %d sends, spend %+v", r.actor.sends, r.lastSpend())
	}
}

// TestAOneShotAtItsAllowanceWaits (spec 087): a one-shot whose trigger is
// at its allowance is skipped_budget and kept at its time; the minute
// loop tries it at each pass without counting it twice, it fires once the
// spend falls below the allowance, and one that waits past its maxAge is
// skipped_late and spent.
func TestAOneShotAtItsAllowanceWaits(t *testing.T) {
	r := newRig(t)
	tr := r.put("watch", watching(1))
	s := r.fire(tr, nil, store.OutcomeStarted)
	r.spendIn(s.SessionID, 2)

	at := r.clock.Now().Add(time.Minute)
	wake := r.putWake(s.SessionID, at, "200h")
	r.clock.Advance(time.Minute)
	r.tick()
	kept, err := r.objects.Trigger(t.Context(), wake.ID)
	if err != nil || kept.NextFireAt == nil || !kept.NextFireAt.Equal(at) || kept.Counts.SkippedBudget != 1 || r.actor.sends != 0 {
		t.Fatalf("the one-shot at its allowance: %+v, %v, %d sends", kept, err, r.actor.sends)
	}
	r.clock.Advance(time.Hour)
	r.tick()
	if kept, err = r.objects.Trigger(t.Context(), wake.ID); err != nil || kept.Counts.SkippedBudget != 1 || r.actor.sends != 0 {
		t.Fatalf("the one-shot tried again: %+v, %v, %d sends", kept.Counts, err, r.actor.sends)
	}
	// The look leaves the window: the one-shot fires and is spent.
	r.clock.Advance(7 * 24 * time.Hour)
	r.tick()
	if _, err := r.objects.Trigger(t.Context(), wake.ID); !errors.Is(err, store.ErrNotFound) || r.actor.sends != 1 {
		t.Fatalf("the one-shot once the spend fell: %v, %d sends", err, r.actor.sends)
	}
	r.spendIn(s.SessionID, 2)

	// At the allowance again, a one-shot that waits past its maxAge is
	// skipped_late and spent.
	late := r.putWake(s.SessionID, r.clock.Now().Add(time.Minute), "1h")
	r.clock.Advance(time.Minute)
	r.tick()
	if got, err := r.objects.Trigger(t.Context(), late.ID); err != nil || got.Counts.SkippedBudget != 1 {
		t.Fatalf("the late one-shot at its allowance: %+v, %v", got.Counts, err)
	}
	r.clock.Advance(2 * time.Hour)
	r.tick()
	if _, err := r.objects.Trigger(t.Context(), late.ID); !errors.Is(err, store.ErrNotFound) || r.actor.spent[len(r.actor.spent)-1].Outcome != store.OutcomeSkippedLate {
		t.Fatalf("the one-shot past its maxAge: %v, %+v", err, r.actor.spent)
	}
}

// TestAHeldFiringIsHeldToTheAllowance (spec 087): a firing held while its
// session waits for a person is skipped_budget when it would be sent past
// the allowance, and a held one-shot goes back to its time and is tried
// again.
func TestAHeldFiringIsHeldToTheAllowance(t *testing.T) {
	r := newRig(t)
	tr := r.put("triage", `{"agent":"agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C","timeZone":"UTC","on":{"product":"github","verbs":["*"]},"session":{"message":"{{event.resource}}","policy":"continue","endOnIdle":false},"skipIfActive":true,"maxAge":"1h","suspend":false,"budget":{"perWeek":100}}`)
	first := r.fire(tr, r.env("1", "o/r#1"), store.OutcomeStarted)
	r.status(first.SessionID, session.StatusIdle, session.StopToolConfirmation)
	r.fire(tr, r.env("2", "o/r#1"), store.OutcomeHeld)
	// Another key's look takes the trigger past its allowance; what the
	// waiting session spends after the person's answer is the person's.
	other := r.fire(tr, r.env("3", "o/r#2"), store.OutcomeStarted)
	r.spendIn(other.SessionID, 150)
	r.status(first.SessionID, session.StatusIdle, session.StopEndTurn)
	r.tick()
	got, err := r.objects.Trigger(t.Context(), tr.ID)
	if err != nil || got.Counts.SkippedBudget != 1 || got.Counts.Held != 0 || got.Counts.Started != 2 || r.actor.sends != 0 {
		t.Fatalf("the held firing past the allowance: %+v, %v, %d sends", got.Counts, err, r.actor.sends)
	}

	// A wake held while its session waited, sent once another look took
	// the routine past its allowance, goes back to its time.
	routine := r.put("watch", watching(100))
	s := r.fire(routine, nil, store.OutcomeStarted)
	r.status(s.SessionID, session.StatusIdle, session.StopToolConfirmation)
	at := r.clock.Now().Add(time.Minute)
	wake := r.putWake(s.SessionID, at, "24h")
	r.clock.Advance(time.Minute)
	r.tick()
	if held, err := r.objects.Trigger(t.Context(), wake.ID); err != nil || held.Counts.Held != 1 {
		t.Fatalf("the wake of a session that waits: %+v, %v", held.Counts, err)
	}
	another := r.fire(routine, nil, store.OutcomeStarted)
	r.spendIn(another.SessionID, 150)
	r.status(s.SessionID, session.StatusIdle, session.StopEndTurn)
	r.tick()
	back, err := r.objects.Trigger(t.Context(), wake.ID)
	if err != nil || back.Counts.Held != 0 || back.Counts.SkippedBudget != 1 || back.NextFireAt == nil || !back.NextFireAt.Equal(at) || r.actor.sends != 0 {
		t.Fatalf("the held wake past the allowance: %+v, %v, %d sends", back, err, r.actor.sends)
	}
	r.tick()
	if again, err := r.objects.Trigger(t.Context(), wake.ID); err != nil || again.Counts.SkippedBudget != 1 || r.actor.sends != 0 {
		t.Fatalf("the wake tried again: %+v, %v, %d sends", again.Counts, err, r.actor.sends)
	}
}

// TestTheSpendOfAWatch (spec 087): status.spend holds the cost of the
// window's looks, a running one's so far included, the allowance, the
// mean of the latest five ended looks, the looks expected to the end of
// the window that the oldest look opened, and the projection; the
// expected looks are a schedule's fire times, none more for a one-shot or
// a suspended trigger, and for events the window's looks spread over the
// time left.
func TestTheSpendOfAWatch(t *testing.T) {
	r := newRig(t)
	t0 := r.clock.Now()
	daily := `{"agent":"agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C","timeZone":"UTC","schedule":"0 9 * * *","session":{"message":"Look.","endOnIdle":false},"skipIfActive":false,"maxAge":"1h","suspend":false,"budget":{"perWeek":1000}}`
	tr := r.put("daily", daily)
	for _, c := range []int64{10, 20, 30, 40, 50, 60} {
		f := r.fire(tr, nil, store.OutcomeStarted)
		r.spendIn(f.SessionID, c)
		r.clock.Advance(time.Hour)
	}
	// A look still running counts what it spent so far, and is no look's
	// mean.
	open := r.fire(tr, nil, store.OutcomeStarted)
	s, err := r.sessions.Get(t.Context(), open.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	running, _ := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning}, r.clock.Now())
	cost, _ := session.NewEvent(session.TypeModelRequest, session.ModelRequest{Model: "m", Outcome: "ok", CostUSDMicro: new(int64(5))}, r.clock.Now())
	batch := []session.Event{running, cost}
	session.Stamp(s.ID, s.LastSeq, batch)
	if _, err := r.sessions.Append(t.Context(), s.ID, s.LastSeq, batch); err != nil {
		t.Fatal(err)
	}

	now := r.clock.Now()
	sp, err := r.engine.Spend(t.Context(), tr, mustSpec(t, tr), now)
	if err != nil {
		t.Fatal(err)
	}
	// The window opened at the oldest look, t0, and ends seven days on: the
	// firing's own look and the seven 9:00 runs before it.
	if sp.WindowDays != 7 || sp.SpentUSDMicro != 215 || *sp.PerWeekUSDMicro != 1000 || *sp.PerLookUSDMicro != 40 || sp.ExpectedLooks != 8 || *sp.ProjectedUSDMicro != 215+40*8 {
		t.Fatalf("the spend %+v", sp)
	}
	if !t0.Add(7 * 24 * time.Hour).After(now) {
		t.Fatal("the test's clock left the window")
	}

	// The kinds of trigger, the window ending in a day.
	end := now.Add(24 * time.Hour)
	young := store.Trigger{CreatedAt: now.Add(-12 * time.Hour)}
	old := store.Trigger{CreatedAt: now.Add(-30 * 24 * time.Hour)}
	at := now.Add(time.Hour)
	for _, c := range []struct {
		name string
		t    store.Trigger
		spec v1.TriggerSpec
		seen int
		want int
	}{
		// The clock is on the hour: the hourly runs after it and before
		// the window's end, a day on, are 23.
		{"a schedule", old, v1.TriggerSpec{Schedule: "0 * * * *", TimeZone: "UTC"}, 0, 1 + 23},
		{"a one-shot", old, v1.TriggerSpec{At: &at}, 0, 1},
		{"a suspended schedule", old, v1.TriggerSpec{Schedule: "0 * * * *", TimeZone: "UTC", Suspend: true}, 0, 1},
		// Fourteen looks of a week are two a day.
		{"events", old, v1.TriggerSpec{On: &v1.TriggerOn{Product: "github", Verbs: []string{"push"}}}, 14, 1 + 2},
		// A trigger younger than a day spreads its looks over a day.
		{"events of a young trigger", young, v1.TriggerSpec{On: &v1.TriggerOn{Product: "github", Verbs: []string{"push"}}}, 3, 1 + 3},
	} {
		n, err := expectedLooks(c.t, c.spec, c.seen, now, end)
		if err != nil || n != c.want {
			t.Errorf("%s: %d expected looks, want %d; %v", c.name, n, c.want, err)
		}
	}
	if n, err := expectedLooks(old, v1.TriggerSpec{Schedule: "0 * * * *", TimeZone: "UTC"}, 0, now, now); err != nil || n != 1 {
		t.Errorf("a window that ends now: %d, %v", n, err)
	}
	if _, err := expectedLooks(old, v1.TriggerSpec{Schedule: "0 * * * *", TimeZone: "Mars/Olympus"}, 0, now, end); err == nil {
		t.Error("a zone the build does not know")
	}

	// A trigger with no looks yet projects nothing, and its window is the
	// next seven days.
	fresh := r.put("fresh", daily)
	sp, err = r.engine.Spend(t.Context(), fresh, mustSpec(t, fresh), now)
	if err != nil || sp.SpentUSDMicro != 0 || sp.PerLookUSDMicro != nil || sp.ProjectedUSDMicro != nil || sp.ExpectedLooks != 1+7 {
		t.Fatalf("a fresh trigger's spend %+v, %v", sp, err)
	}
}

// mustSpec is tr's spec.
func mustSpec(t *testing.T, tr store.Trigger) v1.TriggerSpec {
	t.Helper()
	sp, err := Spec(tr)
	if err != nil {
		t.Fatal(err)
	}
	return sp
}
