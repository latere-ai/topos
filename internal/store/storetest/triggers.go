// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// NewTrigger is a trigger of owner's under name at version 1, with a
// document that reads back as a Trigger.
func NewTrigger(owner, name string, created time.Time) store.Trigger {
	id := session.NewID(session.PrefixTrigger)
	doc := fmt.Sprintf(`{"apiVersion":"topos.latere.ai/v1","kind":"Trigger","metadata":{"name":%q},"spec":{"agent":"agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C","schedule":"@hourly","timeZone":"UTC","session":{"message":"Go.","endOnIdle":true},"skipIfActive":true,"maxAge":"1h","suspend":false},"status":{"id":%q,"version":1,"digest":"sha256:1"}}`, name, id)
	return store.Trigger{ID: id, Name: name, Owner: owner, OrgID: "org_1", AgentID: "agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C",
		Version: 1, Digest: "sha256:1", Doc: []byte(doc), CreatedAt: created, UpdatedAt: created}
}

// NewFiring is a firing of the trigger id, deduplicated by dedupe.
func NewFiring(id, dedupe, origin string, at time.Time) store.Firing {
	return store.Firing{ID: session.NewID(session.PrefixFiring), TriggerID: id, Dedupe: dedupe, Origin: origin, Replica: "r1", Time: at, ReceivedAt: at}
}

// triggers: a trigger is stored, read by id and by its owner's name,
// versioned by apply, and listed; its names are per owner; it is
// deleted with its firings and keys; and the lookup reads it.
func triggers(t *testing.T, f Factory) {
	clock := NewClock()
	st := f(t, clock.Now)
	ctx := t.Context()
	tr := NewTrigger("alice", "nightly", clock.Now())
	next := clock.Now().Add(time.Hour)
	tr.NextFireAt = &next
	if err := st.PutTrigger(ctx, tr); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() (store.Trigger, error){
		func() (store.Trigger, error) { return st.Trigger(ctx, tr.ID) },
		func() (store.Trigger, error) { return st.TriggerByName(ctx, "alice", "nightly") },
	} {
		got, err := read()
		if err != nil || got.ID != tr.ID || got.Owner != "alice" || got.OrgID != "org_1" || got.AgentID != tr.AgentID || got.Version != 1 ||
			string(got.Doc) != string(tr.Doc) || got.NextFireAt == nil || !got.NextFireAt.Equal(next) || !got.CreatedAt.Equal(tr.CreatedAt) {
			t.Fatalf("the stored trigger = %+v, %v", got, err)
		}
	}
	if _, err := st.TriggerByName(ctx, "bob", "nightly"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("alice's name read as bob's: %v", err)
	}
	if _, err := st.Trigger(ctx, session.NewID(session.PrefixTrigger)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown id: %v", err)
	}
	dup := NewTrigger("alice", "nightly", clock.Now())
	if err := st.PutTrigger(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second trigger of alice's name: %v", err)
	}
	bobs := NewTrigger("bob", "nightly", clock.Now())
	if err := st.PutTrigger(ctx, bobs); err != nil {
		t.Fatalf("bob's trigger of alice's name: %v", err)
	}
	if err := st.PutTrigger(ctx, store.Trigger{ID: "trg_x"}); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("a malformed trigger: %v", err)
	}

	// A firing is recorded, and the next apply keeps it.
	fr := NewFiring(tr.ID, "d1", store.OriginEvent, clock.Now())
	if _, fresh, err := st.ClaimFiring(ctx, fr); err != nil || !fresh {
		t.Fatalf("claim: %v, %v", fresh, err)
	}
	fr.Outcome, fr.SessionID = store.OutcomeStarted, "ses_1"
	if err := st.RecordFiring(ctx, fr, ""); err != nil {
		t.Fatal(err)
	}
	v2 := tr
	v2.Version, v2.Digest, v2.Suspended, v2.NextFireAt, v2.OrgID = 2, "sha256:2", true, nil, ""
	if err := st.PutTrigger(ctx, v2); err != nil {
		t.Fatal(err)
	}
	got, err := st.Trigger(ctx, tr.ID)
	if err != nil || got.Version != 2 || !got.Suspended || got.NextFireAt != nil || got.OrgID != "" || got.Counts.Started != 1 || got.LastSessionID != "ses_1" || got.LastFiredAt == nil || !got.CreatedAt.Equal(tr.CreatedAt) {
		t.Fatalf("after the second apply: %+v, %v", got, err)
	}
	same := v2
	same.Doc = []byte(strings.Replace(string(tr.Doc), `"metadata":{"name":"nightly"}`, `"metadata":{"name":"nightly","displayName":"Nightly"}`, 1))
	if err := st.PutTrigger(ctx, same); err != nil {
		t.Fatalf("a change to the metadata alone: %v", err)
	}
	for _, v := range []int{1, 4} {
		skip := v2
		skip.Version = v
		if err := st.PutTrigger(ctx, skip); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("version %d after 2: %v", v, err)
		}
	}
	if err := st.SetTriggerSession(ctx, tr.ID, "k", "ses_1"); err != nil {
		t.Fatal(err)
	}

	l := store.Lookup(st, "alice")
	if doc, err := l.Trigger(ctx, "nightly"); err != nil || doc.Status.ID != tr.ID || doc.Metadata.DisplayName != "Nightly" {
		t.Fatalf("the lookup of alice's trigger: %+v, %v", doc, err)
	}
	if _, err := store.Lookup(st, "carol").Trigger(ctx, "nightly"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("carol's lookup of another owner's name: %v", err)
	}

	var ids []string
	cursor := ""
	for {
		page, next, err := st.ListTriggers(ctx, store.TriggerList{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range page {
			ids = append(ids, x.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if want := []string{tr.ID, bobs.ID}; !slices.Equal(ids, want) {
		t.Fatalf("the triggers paged as %v, want %v", ids, want)
	}
	if mine, _, err := st.ListTriggers(ctx, store.TriggerList{Owners: []string{"bob"}}); err != nil || len(mine) != 1 || mine[0].ID != bobs.ID {
		t.Fatalf("bob's triggers: %+v, %v", mine, err)
	}
	if _, _, err := st.ListTriggers(ctx, store.TriggerList{Cursor: "!!"}); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("a forged cursor: %v", err)
	}

	if err := st.DeleteTrigger(ctx, tr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Trigger(ctx, tr.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a deleted trigger: %v", err)
	}
	if _, _, err := st.Firings(ctx, tr.ID, 0, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a deleted trigger's firings: %v", err)
	}
	if _, err := st.TriggerSession(ctx, tr.ID, "k"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a deleted trigger's key: %v", err)
	}
	if err := st.DeleteTrigger(ctx, tr.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a second delete: %v", err)
	}
	again := NewTrigger("alice", "nightly", clock.Now())
	if err := st.PutTrigger(ctx, again); err != nil {
		t.Fatalf("the name after a delete: %v", err)
	}
}

// triggerSchedules: a due trigger is listed until its next fire time
// moves past the instant, a suspended one never is, and an event
// trigger has no fire time.
func triggerSchedules(t *testing.T, f Factory) {
	clock := NewClock()
	st := f(t, clock.Now)
	ctx := t.Context()
	now := clock.Now()
	due, later, paused, event := NewTrigger("alice", "due", now), NewTrigger("alice", "later", now), NewTrigger("alice", "paused", now), NewTrigger("alice", "event", now)
	at, after := now.Add(-time.Minute), now.Add(time.Hour)
	due.NextFireAt, later.NextFireAt, paused.NextFireAt, paused.Suspended = &at, &after, &at, true
	for _, x := range []store.Trigger{due, later, paused, event} {
		if err := st.PutTrigger(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	list, err := st.DueTriggers(ctx, now)
	if err != nil || len(list) != 1 || list[0].ID != due.ID {
		t.Fatalf("due at now: %+v, %v", list, err)
	}
	if list, err := st.DueTriggers(ctx, after); err != nil || len(list) != 2 || list[0].ID != due.ID || list[1].ID != later.ID {
		t.Fatalf("due in an hour, soonest first: %+v, %v", list, err)
	}
	if err := st.SetNextFire(ctx, due.ID, &after); err != nil {
		t.Fatal(err)
	}
	if list, err := st.DueTriggers(ctx, now); err != nil || len(list) != 0 {
		t.Fatalf("due after the move: %+v, %v", list, err)
	}
	if err := st.SetNextFire(ctx, due.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Trigger(ctx, due.ID); err != nil || got.NextFireAt != nil {
		t.Fatalf("a cleared fire time: %+v, %v", got, err)
	}
	if err := st.SetNextFire(ctx, session.NewID(session.PrefixTrigger), &at); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the fire time of no trigger: %v", err)
	}
}

// triggerLeases: one holder at a time holds a trigger's lease, it renews
// its own, another takes it once it expires or is released, and a
// release by another holder keeps it.
func triggerLeases(t *testing.T, f Factory) {
	clock := NewClock()
	st := f(t, clock.Now)
	ctx := t.Context()
	tr := NewTrigger("alice", "t", clock.Now())
	if err := st.PutTrigger(ctx, tr); err != nil {
		t.Fatal(err)
	}
	until := clock.Now().Add(time.Minute)
	take := func(holder string, want bool) {
		t.Helper()
		if ok, err := st.LeaseTrigger(ctx, tr.ID, holder, until); err != nil || ok != want {
			t.Fatalf("%s takes the lease: %v, %v; want %v", holder, ok, err, want)
		}
	}
	take("a", true)
	take("a", true)
	take("b", false)
	if err := st.ReleaseTrigger(ctx, tr.ID, "b"); err != nil {
		t.Fatal(err)
	}
	take("b", false)
	clock.Advance(2 * time.Minute)
	until = clock.Now().Add(time.Minute)
	take("b", true)
	take("a", false)
	if err := st.ReleaseTrigger(ctx, tr.ID, "b"); err != nil {
		t.Fatal(err)
	}
	take("a", true)
	if _, err := st.LeaseTrigger(ctx, session.NewID(session.PrefixTrigger), "a", until); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the lease of no trigger: %v", err)
	}

	// Replicas racing one free lease take it once.
	other := NewTrigger("alice", "raced", clock.Now())
	if err := st.PutTrigger(ctx, other); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := range 8 {
		wg.Go(func() {
			ok, err := st.LeaseTrigger(ctx, other.ID, fmt.Sprintf("h%d", i), until)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d holders took one lease", won)
	}
}

// triggerFirings: a firing is claimed once per dedupe string, a failed
// or unrecorded one is taken again under its id, a record moves its
// outcome only from the one named and moves the counts with it, a
// filtered event is counted, and firings list newest first.
func triggerFirings(t *testing.T, f Factory) {
	clock := NewClock()
	st := f(t, clock.Now)
	ctx := t.Context()
	tr := NewTrigger("alice", "t", clock.Now())
	if err := st.PutTrigger(ctx, tr); err != nil {
		t.Fatal(err)
	}
	first := NewFiring(tr.ID, "github:1", store.OriginEvent, clock.Now())
	first.Envelope = []byte(`{"id":"1","product":"github"}`)
	got, fresh, err := st.ClaimFiring(ctx, first)
	if err != nil || !fresh || got.ID != first.ID || got.Outcome != "" {
		t.Fatalf("a first claim: %+v, %v, %v", got, fresh, err)
	}
	// Unrecorded, a claim under the lease takes it again.
	retry := NewFiring(tr.ID, "github:1", store.OriginEvent, clock.Now())
	retry.Envelope = first.Envelope
	if got, fresh, err := st.ClaimFiring(ctx, retry); err != nil || !fresh || got.ID != first.ID {
		t.Fatalf("a claim of an unrecorded firing: %+v, %v, %v", got, fresh, err)
	}
	first.Outcome, first.Key, first.SessionID, first.Open = store.OutcomeStarted, "o/r#1", "ses_1", true
	if err := st.RecordFiring(ctx, first, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordFiring(ctx, first, ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second record from no outcome: %v", err)
	}
	got, fresh, err = st.ClaimFiring(ctx, retry)
	if err != nil || fresh || got.ID != first.ID || got.Outcome != store.OutcomeStarted || got.Key != "o/r#1" || got.SessionID != "ses_1" || string(got.Envelope) != string(first.Envelope) || got.Origin != store.OriginEvent || !got.Open {
		t.Fatalf("a redelivery answers the first firing: %+v, %v, %v", got, fresh, err)
	}

	failed := NewFiring(tr.ID, "github:2", store.OriginEvent, clock.Now())
	if _, _, err := st.ClaimFiring(ctx, failed); err != nil {
		t.Fatal(err)
	}
	failed.Outcome, failed.Reason = store.OutcomeFailed, "authorizer_unavailable"
	if err := st.RecordFiring(ctx, failed, ""); err != nil {
		t.Fatal(err)
	}
	if tt, err := st.Trigger(ctx, tr.ID); err != nil || tt.Counts.Failed != 1 || tt.Counts.Started != 1 {
		t.Fatalf("counts after a failure: %+v, %v", tt.Counts, err)
	}
	again, fresh, err := st.ClaimFiring(ctx, NewFiring(tr.ID, "github:2", store.OriginEvent, clock.Now()))
	if err != nil || !fresh || again.ID != failed.ID || again.Outcome != "" || again.Reason != "" {
		t.Fatalf("a failed firing is taken again: %+v, %v, %v", again, fresh, err)
	}
	again.Outcome, again.SessionID = store.OutcomeContinued, "ses_1"
	if err := st.RecordFiring(ctx, again, ""); err != nil {
		t.Fatal(err)
	}
	held := NewFiring(tr.ID, "github:3", store.OriginEvent, clock.Now())
	if _, _, err := st.ClaimFiring(ctx, held); err != nil {
		t.Fatal(err)
	}
	held.Outcome = store.OutcomeHeld
	if err := st.RecordFiring(ctx, held, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.CountFiltered(ctx, tr.ID); err != nil {
		t.Fatal(err)
	}
	tt, err := st.Trigger(ctx, tr.ID)
	want := v1.TriggerCounts{Started: 1, Continued: 1, Held: 1, Filtered: 1}
	if err != nil || tt.Counts != want || tt.LastSessionID != "ses_1" || tt.LastFiredAt == nil {
		t.Fatalf("counts %+v, want %+v; %v", tt.Counts, want, err)
	}
	held.Outcome, held.SessionID = store.OutcomeContinued, "ses_1"
	if err := st.RecordFiring(ctx, held, store.OutcomeHeld); err != nil {
		t.Fatal(err)
	}
	if tt, err := st.Trigger(ctx, tr.ID); err != nil || tt.Counts.Held != 0 || tt.Counts.Continued != 2 {
		t.Fatalf("a held firing sent moves its count: %+v, %v", tt.Counts, err)
	}
	if err := st.RecordFiring(ctx, NewFiring(tr.ID, "none", store.OriginEvent, clock.Now()), ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a record of an unclaimed firing: %v", err)
	}
	if _, _, err := st.ClaimFiring(ctx, store.Firing{ID: "frg_x"}); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("a malformed firing: %v", err)
	}
	if _, _, err := st.ClaimFiring(ctx, NewFiring(session.NewID(session.PrefixTrigger), "d", store.OriginEvent, clock.Now())); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a firing of no trigger: %v", err)
	}

	var ids []string
	cursor := ""
	for {
		page, next, err := st.Firings(ctx, tr.ID, 2, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range page {
			ids = append(ids, x.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if want := []string{held.ID, failed.ID, first.ID}; !slices.Equal(ids, want) {
		t.Fatalf("the firings paged as %v, want newest first %v", ids, want)
	}
}

// triggerHeldAndOpen: held firings list oldest first across triggers;
// open firings are a trigger's started ones until their session is
// closed, which also clears the key that names it.
func triggerHeldAndOpen(t *testing.T, f Factory) {
	clock := NewClock()
	st := f(t, clock.Now)
	ctx := t.Context()
	a, b := NewTrigger("alice", "a", clock.Now()), NewTrigger("alice", "b", clock.Now())
	for _, x := range []store.Trigger{a, b} {
		if err := st.PutTrigger(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	record := func(tr store.Trigger, dedupe, outcome, sess string) store.Firing {
		t.Helper()
		fr := NewFiring(tr.ID, dedupe, store.OriginEvent, clock.Now())
		if _, _, err := st.ClaimFiring(ctx, fr); err != nil {
			t.Fatal(err)
		}
		fr.Outcome, fr.SessionID, fr.Open = outcome, sess, outcome == store.OutcomeStarted
		if err := st.RecordFiring(ctx, fr, ""); err != nil {
			t.Fatal(err)
		}
		return fr
	}
	h1 := record(a, "1", store.OutcomeHeld, "")
	s1 := record(a, "2", store.OutcomeStarted, "ses_1")
	h2 := record(b, "1", store.OutcomeHeld, "")
	h3 := record(a, "3", store.OutcomeHeld, "")
	s2 := record(a, "4", store.OutcomeStarted, "ses_2")
	record(a, "5", store.OutcomeContinued, "ses_1")
	held, err := st.HeldFirings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, x := range held {
		ids = append(ids, x.ID)
	}
	if want := []string{h1.ID, h2.ID, h3.ID}; !slices.Equal(ids, want) {
		t.Fatalf("held firings %v, want oldest first %v", ids, want)
	}
	open, err := st.OpenFirings(ctx, a.ID)
	if err != nil || len(open) != 2 || open[0].ID != s1.ID || open[1].ID != s2.ID {
		t.Fatalf("open firings: %+v, %v", open, err)
	}
	if err := st.SetTriggerSession(ctx, a.ID, "k1", "ses_1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTriggerSession(ctx, a.ID, "k2", "ses_2"); err != nil {
		t.Fatal(err)
	}
	if s, err := st.TriggerSession(ctx, a.ID, "k1"); err != nil || s != "ses_1" {
		t.Fatalf("key k1: %q, %v", s, err)
	}
	if err := st.SetTriggerSession(ctx, a.ID, "k1", "ses_3"); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseSession(ctx, a.ID, "ses_1"); err != nil {
		t.Fatal(err)
	}
	if open, err := st.OpenFirings(ctx, a.ID); err != nil || len(open) != 1 || open[0].ID != s2.ID {
		t.Fatalf("open after ses_1 closed: %+v, %v", open, err)
	}
	if s, err := st.TriggerSession(ctx, a.ID, "k1"); err != nil || s != "ses_3" {
		t.Fatalf("a key mapped to another session is kept: %q, %v", s, err)
	}
	if err := st.CloseSession(ctx, a.ID, "ses_2"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TriggerSession(ctx, a.ID, "k2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the key of a closed session: %v", err)
	}
	if _, err := st.TriggerSession(ctx, b.ID, "k1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another trigger's key: %v", err)
	}
	for _, call := range []func() error{
		func() error { _, err := st.OpenFirings(ctx, session.NewID(session.PrefixTrigger)); return err },
		func() error { return st.CloseSession(ctx, session.NewID(session.PrefixTrigger), "ses_1") },
		func() error { return st.SetTriggerSession(ctx, session.NewID(session.PrefixTrigger), "k", "ses_1") },
		func() error { return st.CountFiltered(ctx, session.NewID(session.PrefixTrigger)) },
	} {
		if err := call(); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a call on no trigger: %v", err)
		}
	}
}
