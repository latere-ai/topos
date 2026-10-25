// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package triggers

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// listed is the spec of a trigger that fires at times, under policy, with
// maxAge and extra fields after the session's.
func listed(policy, maxAge, extra string, times ...time.Time) string {
	ts := make([]string, len(times))
	for i, t := range times {
		ts[i] = fmt.Sprintf("%q", t.Format(time.RFC3339))
	}
	return fmt.Sprintf(`{"agent":"agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C","timeZone":"UTC","times":[%s],"session":{"message":"Look at the fares.","policy":%q,"endOnIdle":false},"skipIfActive":false,"maxAge":%q,"suspend":false%s}`,
		strings.Join(ts, ","), policy, maxAge, extra)
}

// firings are trigger id's firings, oldest first.
func (r *rig) firings(id string) []store.Firing {
	r.t.Helper()
	fs, _, err := r.objects.Firings(r.t.Context(), id, 0, "")
	if err != nil {
		r.t.Fatal(err)
	}
	slices.Reverse(fs)
	return fs
}

// stored is trigger id as the store holds it, or false once it is gone.
func (r *rig) stored(id string) (store.Trigger, bool) {
	r.t.Helper()
	t, err := r.objects.Trigger(r.t.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Trigger{}, false
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return t, true
}

// TestEachListedTimeFiresOnce (spec 090): a trigger of listed times fires
// at each time once, the soonest first whatever the list's order, each a
// firing of that time; a pass again in the same minute fires nothing more;
// a time already past when the trigger was applied never fires; and the
// latest time's firing deletes the trigger, as a one-shot's does, while an
// earlier one keeps it with the next time as its next fire time.
func TestEachListedTimeFiresOnce(t *testing.T) {
	r := newRig(t)
	now := r.clock.Now()
	t1, t2, t3, past := now.Add(time.Minute), now.Add(time.Hour), now.Add(2*time.Hour), now.Add(-time.Hour)
	tr := r.put("fares", listed("new", "1h", "", t3, past, t1, t2))
	if tr.NextFireAt == nil || !tr.NextFireAt.Equal(t1) {
		t.Fatalf("applied, its next fire time is %v", tr.NextFireAt)
	}
	r.clock.Advance(time.Minute)
	r.tick()
	r.tick()
	got, ok := r.stored(tr.ID)
	if !ok || got.NextFireAt == nil || !got.NextFireAt.Equal(t2) || r.actor.starts != 1 {
		t.Fatalf("after the first time: %+v, %v, %d starts", got, ok, r.actor.starts)
	}
	r.clock.Advance(time.Hour)
	r.tick()
	fs := r.firings(tr.ID)
	if len(fs) != 2 || !fs[0].Time.Equal(t1) || !fs[1].Time.Equal(t2) || fs[0].Dedupe != ScheduleDedupe(t1) || fs[0].Outcome != store.OutcomeStarted || fs[1].Outcome != store.OutcomeStarted {
		t.Fatalf("the firings of the first two times: %+v", fs)
	}
	r.clock.Advance(time.Hour)
	r.tick()
	if _, ok := r.stored(tr.ID); ok || r.actor.starts != 3 {
		t.Fatalf("after the latest time the trigger is kept: %v, %d starts", ok, r.actor.starts)
	}
	if n := len(r.actor.spent); n != 1 || !r.actor.spent[0].Time.Equal(t3) || r.actor.spent[0].Outcome != store.OutcomeStarted {
		t.Fatalf("the firing that spent it: %+v", r.actor.spent)
	}
	for _, f := range r.actor.spent {
		if f.Time.Equal(past) {
			t.Fatal("a time past at the apply fired")
		}
	}
}

// TestAListedTimeIsLateOnceTheNextHasCome (spec 090): a listed time not
// fired before the next listed time has come is skipped_late, and the next
// time fires in the same pass; one fired past maxAge is skipped_late too.
func TestAListedTimeIsLateOnceTheNextHasCome(t *testing.T) {
	r := newRig(t)
	now := r.clock.Now()
	t1, t2, t3 := now.Add(time.Minute), now.Add(2*time.Minute), now.Add(time.Hour)
	tr := r.put("fares", listed("new", "24h", "", t1, t2, t3))
	r.clock.Advance(3 * time.Minute)
	r.tick()
	fs := r.firings(tr.ID)
	if len(fs) != 2 || fs[0].Outcome != store.OutcomeSkippedLate || !fs[0].Time.Equal(t1) || fs[1].Outcome != store.OutcomeStarted || !fs[1].Time.Equal(t2) {
		t.Fatalf("the firings after a pass past two times: %+v", fs)
	}
	if got, _ := r.stored(tr.ID); got.NextFireAt == nil || !got.NextFireAt.Equal(t3) {
		t.Fatalf("its next fire time: %v", got.NextFireAt)
	}

	aged := r.put("aged", listed("new", "1h", "", now.Add(4*time.Minute), now.Add(10*time.Hour)))
	r.clock.Advance(3 * time.Hour)
	r.tick()
	if fs := r.firings(aged.ID); len(fs) != 1 || fs[0].Outcome != store.OutcomeSkippedLate {
		t.Fatalf("a time past maxAge: %+v", fs)
	}
}

// TestAListedTimeRefusedUntilALimitResetsWaits (spec 090): a listed time a
// deny refuses for a limit that resets is kept and tried once more when it
// resets, under its first firing and its own time; a reset past the next
// listed time waits no later than it, which then fires in its place; a
// refusal that names no reset ends that time and keeps the trigger for the
// next.
func TestAListedTimeRefusedUntilALimitResetsWaits(t *testing.T) {
	r := newRig(t)
	now := r.clock.Now()
	t1, t2, t3, t4 := now.Add(time.Minute), now.Add(2*time.Hour), now.Add(5*time.Hour), now.Add(8*time.Hour)
	tr := r.put("fares", listed("new", "24h", "", t1, t2, t3, t4))
	resets := t1.Add(30 * time.Minute)
	r.actor.startErr = &resetting{at: resets}
	r.clock.Advance(time.Minute)
	r.tick()
	if got, _ := r.stored(tr.ID); got.NextFireAt == nil || !got.NextFireAt.Equal(resets) {
		t.Fatalf("a refused time's next fire time: %v", got.NextFireAt)
	}
	first := r.firings(tr.ID)[0]
	r.actor.startErr = nil
	r.clock.Advance(30 * time.Minute)
	r.tick()
	fs := r.firings(tr.ID)
	if len(fs) != 1 || fs[0].ID != first.ID || fs[0].Outcome != store.OutcomeStarted || !fs[0].Time.Equal(t1) {
		t.Fatalf("the first time once its limit reset: %+v", fs)
	}
	if got, _ := r.stored(tr.ID); got.NextFireAt == nil || !got.NextFireAt.Equal(t2) {
		t.Fatalf("then its next fire time: %v", got.NextFireAt)
	}

	// A reset past the next time waits for that time, which fires.
	r.actor.startErr = &resetting{at: t3.Add(time.Hour)}
	r.clock.Advance(2*time.Hour - 31*time.Minute)
	r.tick()
	if got, _ := r.stored(tr.ID); got.NextFireAt == nil || !got.NextFireAt.Equal(t3) {
		t.Fatalf("a reset past the next time: next fire %v", got.NextFireAt)
	}
	starts := r.actor.starts
	r.clock.Advance(time.Hour)
	r.tick()
	if r.actor.starts != starts {
		t.Fatal("a waiting time was tried before its limit reset or the next time came")
	}
	r.actor.startErr = errors.New("refused")
	r.clock.Advance(t3.Sub(r.clock.Now()))
	r.tick()
	fs = r.firings(tr.ID)
	if len(fs) != 3 || fs[1].Outcome != store.OutcomeRefused || !fs[1].Time.Equal(t2) || fs[2].Outcome != store.OutcomeRefused || !fs[2].Time.Equal(t3) {
		t.Fatalf("the second time gave way to the third, refused for good: %+v", fs)
	}
	if got, ok := r.stored(tr.ID); !ok || got.NextFireAt == nil || !got.NextFireAt.Equal(t4) {
		t.Fatalf("a time refused for good keeps the trigger for the next: %+v, %v", got, ok)
	}
	r.actor.startErr = nil
	r.clock.Advance(t4.Sub(r.clock.Now()))
	r.tick()
	if _, ok := r.stored(tr.ID); ok {
		t.Fatal("the latest time's firing kept the trigger")
	}
}

// TestAListedTimeAtItsAllowanceGivesWayToTheNext (spec 090): a listed time
// skipped at its trigger's allowance is tried at each pass, under its own
// time, until the next listed time comes, when it is skipped_late and the
// next time fires.
func TestAListedTimeAtItsAllowanceGivesWayToTheNext(t *testing.T) {
	r := newRig(t)
	now := r.clock.Now()
	t1, t2, t3 := now.Add(time.Minute), now.Add(time.Hour), now.Add(3*time.Hour)
	tr := r.put("fares", listed("new", "24h", `,"budget":{"perWeek":1000}`, t1, t2, t3))
	r.clock.Advance(time.Minute)
	r.tick()
	r.spendIn(r.firings(tr.ID)[0].SessionID, 1200)
	r.clock.Advance(time.Hour)
	r.tick()
	r.clock.Advance(time.Minute)
	r.tick()
	fs := r.firings(tr.ID)
	if len(fs) != 2 || fs[1].Outcome != store.OutcomeSkippedBudget || !fs[1].Time.Equal(t2) {
		t.Fatalf("the second time at its allowance: %+v", fs)
	}
	if got, _ := r.stored(tr.ID); got.NextFireAt == nil || !got.NextFireAt.Equal(t2) {
		t.Fatalf("it waits at its own time: %v", got.NextFireAt)
	}
	r.clock.Advance(t3.Sub(r.clock.Now()))
	r.tick()
	fs = r.firings(tr.ID)
	if len(fs) != 3 || fs[1].Outcome != store.OutcomeSkippedLate || fs[2].Outcome != store.OutcomeSkippedBudget || !fs[2].Time.Equal(t3) {
		t.Fatalf("once the third time came: %+v", fs)
	}
}

// TestAHeldListedTimeKeepsItsTrigger (spec 090): under policy continue, a
// listed time held while its session waits for a person keeps the
// trigger, the next time its next fire time; once the session runs on the
// sweep sends it, and the trigger stays for its later times; the latest
// time held keeps the trigger with no next fire time until the sweep sends
// it, which spends it.
func TestAHeldListedTimeKeepsItsTrigger(t *testing.T) {
	r := newRig(t)
	now := r.clock.Now()
	t1, t2, t3 := now.Add(time.Minute), now.Add(time.Hour), now.Add(2*time.Hour)
	tr := r.put("fares", listed("continue", "24h", "", t1, t2, t3))
	idle := func(reason session.StopReason) {
		t.Helper()
		id := r.firings(tr.ID)[0].SessionID
		s, err := r.sessions.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: reason}, r.clock.Now())
		if err != nil {
			t.Fatal(err)
		}
		batch := []session.Event{ev}
		session.Stamp(id, s.LastSeq, batch)
		if _, err := r.sessions.Append(t.Context(), id, s.LastSeq, batch); err != nil {
			t.Fatal(err)
		}
	}
	r.clock.Advance(time.Minute)
	r.tick()
	idle(session.StopToolConfirmation)
	r.clock.Advance(time.Hour)
	r.tick()
	if fs := r.firings(tr.ID); len(fs) != 2 || fs[1].Outcome != store.OutcomeHeld {
		t.Fatalf("the second time while its session waits: %+v", fs)
	}
	if got, ok := r.stored(tr.ID); !ok || got.NextFireAt == nil || !got.NextFireAt.Equal(t3) {
		t.Fatalf("a held time's trigger: %+v, %v", got, ok)
	}
	idle(session.StopEndTurn)
	r.tick()
	if fs := r.firings(tr.ID); fs[1].Outcome != store.OutcomeContinued {
		t.Fatalf("the sweep sent %+v", fs[1])
	}
	if _, ok := r.stored(tr.ID); !ok {
		t.Fatal("an earlier time sent by the sweep deleted the trigger")
	}
	idle(session.StopToolConfirmation)
	r.clock.Advance(time.Hour)
	r.tick()
	got, ok := r.stored(tr.ID)
	if !ok || got.NextFireAt != nil {
		t.Fatalf("the latest time held: %+v, %v", got, ok)
	}
	idle(session.StopEndTurn)
	r.tick()
	if _, ok := r.stored(tr.ID); ok {
		t.Fatal("the latest time sent by the sweep kept the trigger")
	}
	if last := r.actor.spent[len(r.actor.spent)-1]; !last.Time.Equal(t3) || last.Outcome != store.OutcomeContinued {
		t.Fatalf("the firing that spent it: %+v", last)
	}
}

// TestAManualFireSpendsNoListedTime (spec 090): a manual fire of a trigger
// of listed times runs once now and keeps the trigger and its next time.
func TestAManualFireSpendsNoListedTime(t *testing.T) {
	r := newRig(t)
	t1 := r.clock.Now().Add(time.Hour)
	tr := r.put("fares", listed("new", "1h", "", t1))
	r.fire(tr, nil, store.OutcomeStarted)
	if got, ok := r.stored(tr.ID); !ok || got.NextFireAt == nil || !got.NextFireAt.Equal(t1) || len(r.actor.spent) != 0 {
		t.Fatalf("after a manual fire: %+v, %v, spent %+v", got, ok, r.actor.spent)
	}
}

// TestTheFireTimesOfAList (spec 090): a list's fire times are its times
// after from and before until, the soonest first, bounded by limit, and
// none while it is suspended; a firing's expected looks count them.
func TestTheFireTimesOfAList(t *testing.T) {
	from := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	a, b, c := from.Add(time.Hour), from.Add(26*time.Hour), from.Add(30*24*time.Hour)
	berlin := time.FixedZone("CET", 3600)
	spec := v1.TriggerSpec{Times: []time.Time{c.In(berlin), from.Add(-time.Hour), b, a.In(berlin)}}
	got, more, err := FireTimes(spec, from, from.Add(7*24*time.Hour), 10)
	if err != nil || more || !slices.EqualFunc(got, []time.Time{a, b}, time.Time.Equal) {
		t.Fatalf("a week's fire times: %v, %v, %v", got, more, err)
	}
	if got, more, _ := FireTimes(spec, from, from.Add(31*24*time.Hour), 2); !more || len(got) != 2 {
		t.Fatalf("bounded by the limit: %v, %v", got, more)
	}
	spec.Suspend = true
	if got, _, _ := FireTimes(spec, from, from.Add(31*24*time.Hour), 10); len(got) != 0 {
		t.Fatalf("a suspended list fires at %v", got)
	}
	spec.Suspend = false
	n, err := expectedLooks(store.Trigger{}, spec, 0, from, from.Add(7*24*time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("a week's expected looks: %d, %v", n, err)
	}
}
