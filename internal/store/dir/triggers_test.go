// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/internal/store/storetest"
	"latere.ai/x/topos/session"
)

// triggerWithFiring stores a trigger with one started firing and a key
// naming its session, and returns them.
func triggerWithFiring(t *testing.T, st *Store, now time.Time) (store.Trigger, store.Firing) {
	t.Helper()
	tr := storetest.NewTrigger("alice", "nightly", now)
	next := now.Add(time.Hour)
	tr.NextFireAt = &next
	if err := st.PutTrigger(t.Context(), tr); err != nil {
		t.Fatal(err)
	}
	fr := storetest.NewFiring(tr.ID, "github:1", store.OriginEvent, now)
	fr.Envelope = []byte(`{"id":"1"}`)
	if _, _, err := st.ClaimFiring(t.Context(), fr); err != nil {
		t.Fatal(err)
	}
	fr.Outcome, fr.Key, fr.SessionID, fr.Open = store.OutcomeStarted, "o/r#1", "ses_1", true
	if err := st.RecordFiring(t.Context(), fr, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTriggerSession(t.Context(), tr.ID, "o/r#1", "ses_1"); err != nil {
		t.Fatal(err)
	}
	return tr, fr
}

// TestReopenReadsTriggersBack: a trigger, its firing and its key are
// read back after a restart, a closed session's key is gone after one,
// and a deleted trigger leaves no file behind.
func TestReopenReadsTriggersBack(t *testing.T) {
	root := t.TempDir()
	clock := storetest.NewClock()
	st := open(t, root, clock.Now)
	tr, fr := triggerWithFiring(t, st, clock.Now())

	again := open(t, root, clock.Now)
	got, err := again.Trigger(t.Context(), tr.ID)
	if err != nil || got.Name != "nightly" || got.OrgID != "org_1" || got.Counts.Started != 1 || got.LastSessionID != "ses_1" ||
		got.NextFireAt == nil || !got.NextFireAt.Equal(*tr.NextFireAt) || string(got.Doc) != string(tr.Doc) {
		t.Fatalf("the trigger after a restart: %+v, %v", got, err)
	}
	held, fresh, err := again.ClaimFiring(t.Context(), storetest.NewFiring(tr.ID, "github:1", store.OriginEvent, clock.Now()))
	if err != nil || fresh || held.ID != fr.ID || held.Outcome != store.OutcomeStarted || !held.Open || string(held.Envelope) != `{"id":"1"}` {
		t.Fatalf("the firing after a restart: %+v, %v, %v", held, fresh, err)
	}
	if s, err := again.TriggerSession(t.Context(), tr.ID, "o/r#1"); err != nil || s != "ses_1" {
		t.Fatalf("the key after a restart: %q, %v", s, err)
	}
	if err := again.CloseSession(t.Context(), tr.ID, "ses_1"); err != nil {
		t.Fatal(err)
	}
	third := open(t, root, clock.Now)
	if _, err := third.TriggerSession(t.Context(), tr.ID, "o/r#1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a closed session's key after a restart: %v", err)
	}
	if open, err := third.OpenFirings(t.Context(), tr.ID); err != nil || len(open) != 0 {
		t.Fatalf("a closed session's firing after a restart: %+v, %v", open, err)
	}
	if err := third.DeleteTrigger(t.Context(), tr.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(root, "objects", kindTrigger, tr.ID+".json"),
		filepath.Join(root, "objects", kindTriggerFiring, tr.ID),
		filepath.Join(root, "objects", kindTriggerKey, tr.ID),
	} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after the delete: %v", p, err)
		}
	}
	if _, err := open(t, root, clock.Now).Trigger(t.Context(), tr.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a deleted trigger after a restart: %v", err)
	}
}

// TestACorruptTriggerFileIsRefused: a trigger, firing or key file that
// does not decode, or holds another object than its name says, refuses
// the open; the files of a trigger whose own file is gone, and a file
// whose name is no object's, are ignored.
func TestACorruptTriggerFileIsRefused(t *testing.T) {
	clock := storetest.NewClock()
	for name, spoil := range map[string]func(root string, tr store.Trigger, fr store.Firing) error{
		"a trigger that does not decode": func(root string, tr store.Trigger, _ store.Firing) error {
			return os.WriteFile(filepath.Join(root, "objects", kindTrigger, tr.ID+".json"), []byte("{"), 0o600)
		},
		"a trigger of another id": func(root string, tr store.Trigger, _ store.Firing) error {
			other := session.NewID(session.PrefixTrigger)
			return os.Rename(filepath.Join(root, "objects", kindTrigger, tr.ID+".json"), filepath.Join(root, "objects", kindTrigger, other+".json"))
		},
		"a firing that does not decode": func(root string, tr store.Trigger, fr store.Firing) error {
			return os.WriteFile(filepath.Join(root, "objects", kindTriggerFiring, tr.ID, fr.ID+".json"), []byte("["), 0o600)
		},
		"a firing of another id": func(root string, tr store.Trigger, fr store.Firing) error {
			other := session.NewID(session.PrefixFiring)
			return os.Rename(filepath.Join(root, "objects", kindTriggerFiring, tr.ID, fr.ID+".json"), filepath.Join(root, "objects", kindTriggerFiring, tr.ID, other+".json"))
		},
		"a key that does not decode": func(root string, tr store.Trigger, _ store.Firing) error {
			return os.WriteFile(triggerFiles{root: filepath.Join(root, "objects")}.keyPath(tr.ID, "o/r#1"), []byte("x"), 0o600)
		},
		"a key under another name": func(root string, tr store.Trigger, _ store.Firing) error {
			files := triggerFiles{root: filepath.Join(root, "objects")}
			return os.Rename(files.keyPath(tr.ID, "o/r#1"), files.keyPath(tr.ID, "another"))
		},
	} {
		root := t.TempDir()
		tr, fr := triggerWithFiring(t, open(t, root, clock.Now), clock.Now())
		if err := spoil(root, tr, fr); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(root, clock.Now); !errors.Is(err, session.ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}

	root := t.TempDir()
	tr, _ := triggerWithFiring(t, open(t, root, clock.Now), clock.Now())
	for _, p := range []string{
		filepath.Join(root, "objects", kindTrigger, "notes.txt"),
		filepath.Join(root, "objects", kindTriggerFiring, tr.ID, ".tmp-123"),
		filepath.Join(root, "objects", kindTriggerKey, tr.ID, "short.json"),
		filepath.Join(root, "objects", kindTriggerFiring, "stray.json"),
	} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(root, "objects", kindTrigger, tr.ID+".json")); err != nil {
		t.Fatal(err)
	}
	st := open(t, root, clock.Now)
	if list, _, err := st.ListTriggers(t.Context(), store.TriggerList{}); err != nil || len(list) != 0 {
		t.Fatalf("the files of a trigger whose own file is gone: %+v, %v", list, err)
	}
}

// TestATriggerWriteThatFailsChangesNothing: a firing whose file cannot be
// written is not claimed, and the store reads as it did.
func TestATriggerWriteThatFailsChangesNothing(t *testing.T) {
	root := t.TempDir()
	clock := storetest.NewClock()
	st := open(t, root, clock.Now)
	tr, _ := triggerWithFiring(t, st, clock.Now())
	injected := failWrites(t, kindTriggerFiring)
	if _, _, err := st.ClaimFiring(t.Context(), storetest.NewFiring(tr.ID, "github:2", store.OriginEvent, clock.Now())); !errors.Is(err, injected) {
		t.Fatalf("a claim with a failing disk: %v", err)
	}
	if list, _, err := open(t, root, clock.Now).Firings(t.Context(), tr.ID, 0, ""); err != nil || len(list) != 1 {
		t.Fatalf("the firings after a failed claim: %+v, %v", list, err)
	}
	if err := os.Chmod(filepath.Join(root, "objects", kindTriggerKey), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(filepath.Join(root, "objects", kindTriggerKey), 0o700); err != nil {
			t.Error(err)
		}
	})
	other := storetest.NewTrigger("alice", "other", clock.Now())
	if err := st.PutTrigger(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTriggerSession(t.Context(), other.ID, "k", "ses_2"); err == nil {
		t.Fatal("a key under a directory that cannot be made was mapped")
	}
}
