// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/blob"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/session"
	sessiondir "latere.ai/x/topos/session/dir"
	"latere.ai/x/topos/session/storetest"
)

// downOnce is a blob store whose first session delete fails, a crash
// between a session's rows and its bodies.
type downOnce struct {
	session.Blobs
	failed bool
}

func (d *downOnce) DeleteSession(ctx context.Context, id string) error {
	if !d.failed {
		d.failed = true
		return errors.New("the object store is down")
	}
	return d.Blobs.DeleteSession(ctx, id)
}

// TestSessionDeletionOrder: deleting a session removes its rows and then
// its blob objects; with the objects' delete failing the session is gone
// and its objects are left, never a session without its blobs, and the
// reaper removes those orphans once they are past the sweep's grace.
func TestSessionDeletionOrder(t *testing.T) {
	ctx := t.Context()
	outside := t.TempDir()
	blobs := &downOnce{Blobs: blob.NewDir(outside)}
	st, err := sessiondir.OpenWith(t.TempDir(), sessiondir.Options{Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	srv, err := New(Options{Sessions: st, Objects: store.NewMemory(nil), Verifier: tokens{}, Guard: auth.Guard{Authorizer: &auth.OwnerPolicy{}}, PublicURL: "https://topos.example", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	clean, crashed := storetest.NewSession(), storetest.NewSession()
	for _, s := range []session.Session{clean, crashed} {
		if err := st.Create(ctx, s, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutBlob(ctx, s.ID, bytes.NewReader([]byte("a raw response of "+s.ID))); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Delete(ctx, crashed.ID); err == nil {
		t.Fatal("a delete whose objects could not go reported nothing")
	}
	if err := st.Delete(ctx, clean.ID); err != nil {
		t.Fatal(err)
	}
	for _, s := range []session.Session{clean, crashed} {
		if _, err := st.Get(ctx, s.ID); !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("%s after its delete: %v", s.ID, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, clean.ID)); !os.IsNotExist(err) {
		t.Fatalf("the cleanly deleted session's objects: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, crashed.ID)); err != nil {
		t.Fatalf("the crashed delete's objects: %v", err)
	}
	if err := srv.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, crashed.ID)); err != nil {
		t.Fatalf("the reaper removed objects within the grace: %v", err)
	}
	now = now.Add(session.SweepGrace + time.Minute)
	if err := srv.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, crashed.ID)); !os.IsNotExist(err) {
		t.Fatalf("the reaper left the orphaned objects: %v", err)
	}
}
