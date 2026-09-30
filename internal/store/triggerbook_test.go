// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
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
}

var errDisk = errors.New("the disk is full")

func (s *saver) SaveTrigger(t Trigger) error {
	if s.fail {
		return errDisk
	}
	s.triggers = append(s.triggers, t.ID)
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
