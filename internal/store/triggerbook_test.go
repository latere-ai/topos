// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// saver records what a book writes, and fails every write once fail is
// set.
type saver struct {
	fail     bool
	triggers []string
	firings  []string
	keys     []string
	removed  []string
	holds    []string
	runs     []string
}

var errDisk = errors.New("the disk is full")

func (s *saver) SaveTrigger(t Trigger) error {
	if s.fail {
		return errDisk
	}
	s.triggers = append(s.triggers, t.ID)
	return nil
}

func (s *saver) SaveHold(owner string, since *time.Time) error {
	if s.fail {
		return errDisk
	}
	s.holds = append(s.holds, fmt.Sprintf("%s %v", owner, since != nil))
	return nil
}

func (s *saver) RemoveTrigger(id string) error {
	if s.fail {
		return errDisk
	}
	s.removed = append(s.removed, id)
	return nil
}

func (s *saver) SaveFiring(f Firing) error {
	if s.fail {
		return errDisk
	}
	s.firings = append(s.firings, f.ID)
	return nil
}

func (s *saver) SaveKey(_, key, sessionID string) error {
	if s.fail {
		return errDisk
	}
	s.keys = append(s.keys, key+"="+sessionID)
	return nil
}

func (s *saver) SaveRun(r Run) error {
	if s.fail {
		return errDisk
	}
	s.runs = append(s.runs, fmt.Sprintf("%s %s %d %v", r.FiringID, r.Outcome, r.CostUSDMicro, r.Ended))
	return nil
}

func (s *saver) RemoveRuns(ids []string) error {
	if s.fail {
		return errDisk
	}
	for _, id := range ids {
		s.runs = append(s.runs, "-"+id)
	}
	return nil
}

func bookTrigger(owner, name string) Trigger {
	return Trigger{ID: session.NewID(session.PrefixTrigger), Name: name, Owner: owner, Version: 1, Digest: "sha256:1", Doc: []byte(`{}`)}
}

func bookFiring(id, dedupe string) Firing {
	return Firing{ID: session.NewID(session.PrefixFiring), TriggerID: id, Dedupe: dedupe, Origin: OriginEvent}
}

// TestATriggerBookWritesBeforeItChanges: every change is written through
// the saver first, and a write that fails leaves the book as it was.
func TestATriggerBookWritesBeforeItChanges(t *testing.T) {
	ctx := t.Context()
	sv := &saver{}
	b := NewTriggerBook(nil, sv)
	tr := bookTrigger("alice", "t")
	if err := b.PutTrigger(ctx, tr); err != nil {
		t.Fatal(err)
	}
	fr := bookFiring(tr.ID, "d1")
	if _, _, err := b.ClaimFiring(ctx, fr); err != nil {
		t.Fatal(err)
	}
	fr.Outcome, fr.SessionID, fr.Open = OutcomeStarted, "ses_1", true
	if err := b.RecordFiring(ctx, fr, ""); err != nil {
		t.Fatal(err)
	}
	if err := b.SetTriggerSession(ctx, tr.ID, "k", "ses_1"); err != nil {
		t.Fatal(err)
	}
	failed := bookFiring(tr.ID, "d2")
	if _, _, err := b.ClaimFiring(ctx, failed); err != nil {
		t.Fatal(err)
	}
	failed.Outcome = OutcomeFailed
	if err := b.RecordFiring(ctx, failed, ""); err != nil {
		t.Fatal(err)
	}
	if len(sv.triggers) != 3 || len(sv.firings) != 4 || len(sv.keys) != 1 {
		t.Fatalf("writes: %+v", sv)
	}

	sv.fail = true
	at := time.Now()
	next := tr
	next.Version = 2
	for name, call := range map[string]func() error{
		"put":      func() error { return b.PutTrigger(ctx, next) },
		"create":   func() error { return b.PutTrigger(ctx, bookTrigger("alice", "other")) },
		"next":     func() error { return b.SetNextFire(ctx, tr.ID, &at) },
		"claim":    func() error { _, _, err := b.ClaimFiring(ctx, bookFiring(tr.ID, "d3")); return err },
		"re-claim": func() error { _, _, err := b.ClaimFiring(ctx, bookFiring(tr.ID, "d2")); return err },
		"record":   func() error { f := fr; f.Outcome = OutcomeRefused; return b.RecordFiring(ctx, f, OutcomeStarted) },
		"filtered": func() error { return b.CountFiltered(ctx, tr.ID) },
		"close":    func() error { return b.CloseSession(ctx, tr.ID, "ses_1") },
		"key":      func() error { return b.SetTriggerSession(ctx, tr.ID, "k2", "ses_2") },
		"delete":   func() error { return b.DeleteTrigger(ctx, tr.ID) },
	} {
		if err := call(); !errors.Is(err, errDisk) {
			t.Errorf("%s with a failing disk: %v", name, err)
		}
	}
	got, err := b.Trigger(ctx, tr.ID)
	if err != nil || got.Version != 1 || got.NextFireAt != nil || got.Counts != (v1.TriggerCounts{Started: 1, Failed: 1}) {
		t.Fatalf("a failed write changed the trigger: %+v, %v", got, err)
	}
	if _, err := b.TriggerByName(ctx, "alice", "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a failed create stored the trigger: %v", err)
	}
	if open, err := b.OpenFirings(ctx, tr.ID); err != nil || len(open) != 1 {
		t.Fatalf("a failed close closed the firing: %+v, %v", open, err)
	}
	if s, err := b.TriggerSession(ctx, tr.ID, "k"); err != nil || s != "ses_1" {
		t.Fatalf("a failed close cleared the key: %q, %v", s, err)
	}
	if _, err := b.TriggerSession(ctx, tr.ID, "k2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a failed key write mapped it: %v", err)
	}

	sv.fail = false
	if err := b.CloseSession(ctx, tr.ID, "ses_1"); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteTrigger(ctx, tr.ID); err != nil || len(sv.removed) != 1 {
		t.Fatalf("delete: %v, %+v", err, sv.removed)
	}
}

// TestATriggerBookLoadsWhatWasStored: a loaded book answers its objects,
// and a second trigger of one owner's name, or a firing or key of no
// trigger, is refused.
func TestATriggerBookLoadsWhatWasStored(t *testing.T) {
	ctx := t.Context()
	tr := bookTrigger("alice", "t")
	fr := bookFiring(tr.ID, "d1")
	fr.Outcome = OutcomeHeld
	b := NewTriggerBook(nil, nil)
	if err := b.Load([]Trigger{tr}, []Firing{fr}, []KeySession{{TriggerID: tr.ID, Key: "k", SessionID: "ses_1"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := b.TriggerByName(ctx, "alice", "t"); err != nil || got.ID != tr.ID {
		t.Fatalf("the loaded trigger: %+v, %v", got, err)
	}
	if held, err := b.HeldFirings(ctx); err != nil || len(held) != 1 || held[0].ID != fr.ID {
		t.Fatalf("the loaded firing: %+v, %v", held, err)
	}
	if s, err := b.TriggerSession(ctx, tr.ID, "k"); err != nil || s != "ses_1" {
		t.Fatalf("the loaded key: %q, %v", s, err)
	}
	other := bookTrigger("alice", "t")
	for name, load := range map[string]func(*TriggerBook) error{
		"a name twice":        func(b *TriggerBook) error { return b.Load([]Trigger{tr, other}, nil, nil) },
		"a firing of nothing": func(b *TriggerBook) error { return b.Load(nil, []Firing{fr}, nil) },
		"a key of nothing":    func(b *TriggerBook) error { return b.Load(nil, nil, []KeySession{{TriggerID: tr.ID, Key: "k"}}) },
	} {
		if err := load(NewTriggerBook(nil, nil)); err == nil {
			t.Errorf("%s loaded", name)
		}
	}
}

func TestTheShapesOfATriggerAndAFiring(t *testing.T) {
	ok := bookTrigger("alice", "t")
	for name, tr := range map[string]Trigger{
		"no id":      {Name: "t", Owner: "a", Version: 1, Digest: "d", Doc: []byte("{}")},
		"no owner":   {ID: ok.ID, Name: "t", Version: 1, Digest: "d", Doc: []byte("{}")},
		"no version": {ID: ok.ID, Name: "t", Owner: "a", Digest: "d", Doc: []byte("{}")},
		"not text":   {ID: ok.ID, Name: "t", Owner: "a", Version: 1, Digest: "d", Doc: []byte{0xff}},
	} {
		if err := CheckTrigger(tr); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	good := bookFiring(ok.ID, "d")
	for name, f := range map[string]Firing{
		"no id":        {TriggerID: ok.ID, Dedupe: "d", Origin: OriginEvent},
		"no dedupe":    {ID: good.ID, TriggerID: ok.ID, Origin: OriginEvent},
		"an origin":    {ID: good.ID, TriggerID: ok.ID, Dedupe: "d", Origin: "cron"},
		"not text":     {ID: good.ID, TriggerID: ok.ID, Dedupe: "d", Origin: OriginEvent, Envelope: []byte{0xff}},
		"no trigger's": {ID: good.ID, Dedupe: "d", Origin: OriginManual},
	} {
		if err := CheckFiring(f); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	var c v1.TriggerCounts
	for _, o := range []string{OutcomeStarted, OutcomeContinued, OutcomeHeld, OutcomeFiltered, OutcomeSkippedActive, OutcomeSkippedBusy, OutcomeSkippedLate, OutcomeRefused, OutcomeFailed, ""} {
		Count(&c, o, 2)
	}
	if want := (v1.TriggerCounts{Started: 2, Continued: 2, Held: 2, Filtered: 2, SkippedActive: 2, SkippedBusy: 2, SkippedLate: 2, Refused: 2, Failed: 2}); c != want {
		t.Fatalf("counts %+v", c)
	}
	if _, err := DecodeTrigger([]byte("{")); err == nil {
		t.Fatal("a stored trigger that does not read decoded")
	}
}

// TestAHoldIsSavedBeforeTheBookChanges: a hold and its release reach the
// saver first, a saver that fails leaves the book as it was, and the
// holds a store loads read back without being written again.
func TestAHoldIsSavedBeforeTheBookChanges(t *testing.T) {
	sv := &saver{}
	b := NewTriggerBook(nil, sv)
	ctx := t.Context()
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	sv.fail = true
	if err := b.SetHold(ctx, "alice", &now); !errors.Is(err, errDisk) {
		t.Fatalf("a hold the saver refused: %v", err)
	}
	if since, _ := b.Hold(ctx, "alice"); since != nil {
		t.Fatalf("a refused hold is kept: %v", since)
	}
	sv.fail = false
	if err := b.SetHold(ctx, "alice", &now); err != nil {
		t.Fatal(err)
	}
	sv.fail = true
	if err := b.SetHold(ctx, "alice", nil); !errors.Is(err, errDisk) {
		t.Fatalf("a release the saver refused: %v", err)
	}
	if since, _ := b.Hold(ctx, "alice"); since == nil {
		t.Fatal("a refused release dropped the hold")
	}
	sv.fail = false
	if err := b.SetHold(ctx, "alice", nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"alice true", "alice false"}; !slices.Equal(sv.holds, want) {
		t.Fatalf("the saver was asked %v, want %v", sv.holds, want)
	}
	b.LoadHolds(map[string]time.Time{"bob": now})
	if since, _ := b.Hold(ctx, "bob"); since == nil || !since.Equal(now) || len(sv.holds) != 2 {
		t.Fatalf("a loaded hold reads %v, saved %v", since, sv.holds)
	}
}

// TestARunIsSavedBeforeTheBookChanges: a run, its cost and its prune reach
// the saver first, a saver that fails leaves the book as it was, a cost
// that changes nothing writes nothing, and loaded runs read back without
// being written again (spec 088).
func TestARunIsSavedBeforeTheBookChanges(t *testing.T) {
	sv := &saver{}
	b := NewTriggerBook(nil, sv)
	ctx := t.Context()
	at := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	r := Run{FiringID: session.NewID(session.PrefixFiring), Owner: "alice", TriggerID: session.NewID(session.PrefixTrigger), At: at, Outcome: OutcomeStarted, SessionID: "ses_1"}
	if err := b.PutRun(ctx, Run{FiringID: r.FiringID, Owner: "alice", TriggerID: r.TriggerID, Outcome: OutcomeHeld}); err == nil {
		t.Fatal("a held firing was put as a run")
	}
	sv.fail = true
	if err := b.PutRun(ctx, r); !errors.Is(err, errDisk) {
		t.Fatalf("a run the saver refused: %v", err)
	}
	if rs, _ := b.Runs(ctx, "alice", at); len(rs) != 0 {
		t.Fatalf("a refused run is kept: %v", rs)
	}
	sv.fail = false
	if err := b.PutRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	sv.fail = true
	if err := b.SetRunCosts(ctx, []RunCost{{FiringID: r.FiringID, CostUSDMicro: 40, Ended: true}}); !errors.Is(err, errDisk) {
		t.Fatalf("a cost the saver refused: %v", err)
	}
	if rs, _ := b.Runs(ctx, "alice", at); len(rs) != 1 || rs[0].CostUSDMicro != 0 || rs[0].Ended {
		t.Fatalf("a refused cost is kept: %v", rs)
	}
	sv.fail = false
	if err := b.SetRunCosts(ctx, []RunCost{{FiringID: r.FiringID, CostUSDMicro: 40, Ended: true}, {FiringID: "frg_none", CostUSDMicro: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetRunCosts(ctx, []RunCost{{FiringID: r.FiringID, CostUSDMicro: 90}}); err != nil {
		t.Fatal(err)
	}
	sv.fail = true
	if err := b.PruneRuns(ctx, at.Add(time.Second)); !errors.Is(err, errDisk) {
		t.Fatalf("a prune the saver refused: %v", err)
	}
	if rs, _ := b.Runs(ctx, "alice", at); len(rs) != 1 || rs[0].CostUSDMicro != 40 || !rs[0].Ended {
		t.Fatalf("after a refused prune the runs read %v", rs)
	}
	sv.fail = false
	if err := b.PruneRuns(ctx, at); err != nil {
		t.Fatal(err)
	}
	if err := b.PruneRuns(ctx, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if want := []string{r.FiringID + " started 0 false", r.FiringID + " started 40 true", "-" + r.FiringID}; !slices.Equal(sv.runs, want) {
		t.Fatalf("the saver was asked %v, want %v", sv.runs, want)
	}
	b.LoadRuns([]Run{r})
	if rs, _ := b.Runs(ctx, "alice", at); len(rs) != 1 || len(sv.runs) != 3 {
		t.Fatalf("a loaded run reads %v, saved %v", rs, sv.runs)
	}
}
