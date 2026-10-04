// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
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

// deleteAs sends DELETE for a session as token, with the questions asked
// before it forgotten, and answers what came back and what it asked.
func (f *fixture) deleteAs(token, id string) (answer, []string) {
	f.t.Helper()
	f.authz.take()
	a := f.do(http.MethodDelete, "/v1/sessions/"+id, token, "")
	return a, f.authz.take()
}

// TestADeletedSessionIsGoneFromEveryRead: a delete asks session.read and
// then session.delete and answers 204; the session is then not_found on
// every read of it, and the list and the summary leave it out while they
// keep the session beside it.
func TestADeletedSessionIsGoneFromEveryRead(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	kept := f.create("alice", "reviewer")
	s := f.create("alice", "reviewer")
	f.turn(s.ID, 1, "Done.", 1)
	a, asked := f.deleteAs("alice", s.ID)
	if a.status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", a.status, a.body)
	}
	if !slices.Equal(asked, []string{authorizer.ActionSessionRead, authorizer.ActionSessionDelete}) {
		t.Fatalf("a delete asked %v", asked)
	}
	for _, path := range []string{"/v1/sessions/" + s.ID, "/v1/sessions/" + s.ID + "/events", "/v1/sessions/" + s.ID + "/stream", "/v1/sessions/" + s.ID + "/blobs/" + string(s.Agent.Digest)} {
		if a := f.do(http.MethodGet, path, "alice", ""); a.status != http.StatusNotFound || a.code() != CodeNotFound {
			t.Errorf("GET %s after the delete: %d %s", path, a.status, a.body)
		}
	}
	if ids := f.listIDs("alice", ""); !slices.Equal(ids, []string{kept.ID}) {
		t.Fatalf("the list after the delete holds %v", ids)
	}
	if got := f.summary("alice", ""); got != counts(0, 0, 1, 0, 1) {
		t.Fatalf("the summary after the delete: %+v", got)
	}
	if a, _ := f.deleteAs("alice", s.ID); a.status != http.StatusNotFound {
		t.Fatalf("a second delete: %d %s", a.status, a.body)
	}
}

// TestDeletingARunningSessionAsksNothing: a delete of a running session is
// conflict and asks no session.delete, whose allow a decider may act on
// before it answers, and the session stays; once the turn closes, the
// delete removes it.
func TestDeletingARunningSessionAsksNothing(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	f.appendTo(s.ID, 1, session.SessionStatus{Status: session.StatusRunning})
	a, asked := f.deleteAs("alice", s.ID)
	if a.code() != CodeConflict {
		t.Fatalf("a delete of a running session: %d %s", a.status, a.body)
	}
	if slices.Contains(asked, authorizer.ActionSessionDelete) {
		t.Fatalf("a delete of a running session asked %v", asked)
	}
	if _, err := f.sessions.Get(t.Context(), s.ID); err != nil {
		t.Fatalf("the running session after a refused delete: %v", err)
	}
	f.appendTo(s.ID, 1, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopInterrupted})
	if a, asked := f.deleteAs("alice", s.ID); a.status != http.StatusNoContent || !slices.Contains(asked, authorizer.ActionSessionDelete) {
		t.Fatalf("a delete after the interrupt: %d %s, asked %v", a.status, a.body, asked)
	}
	if _, err := f.sessions.Get(t.Context(), s.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("the session after its delete: %v", err)
	}
}

// TestADeleteIsRefused: a caller who may not read the session hears
// not_found and session.delete is not asked; a denied session.delete is
// forbidden with the authorizer's reason, and the session stays.
func TestADeleteIsRefused(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	a, asked := f.deleteAs("bob", s.ID)
	if a.status != http.StatusNotFound || a.code() != CodeNotFound {
		t.Fatalf("bob deletes alice's session: %d %s", a.status, a.body)
	}
	if slices.Contains(asked, authorizer.ActionSessionDelete) {
		t.Fatalf("a delete of a session the caller may not read asked %v", asked)
	}
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionDelete {
			if req.Resource.ID != s.ID || req.Resource.String("owner") != alice || req.Resource.String("agent") != s.Agent.ID {
				t.Errorf("session.delete asked about %+v", req.Resource)
			}
			return authz.Decision{Reason: "role_insufficient"}, nil
		}
		return f.authz.next.Authorize(t.Context(), req)
	}
	a, _ = f.deleteAs("alice", s.ID)
	if a.status != http.StatusForbidden || !strings.Contains(string(a.body), "role_insufficient") {
		t.Fatalf("a denied delete: %d %s", a.status, a.body)
	}
	if _, err := f.sessions.Get(t.Context(), s.ID); err != nil {
		t.Fatalf("the session after a denied delete: %v", err)
	}
}

// TestADeleteRacedByAClaimIsRetried: a runner that claims the session
// while the authorizer decides makes the store refuse the delete, which
// answers conflict and leaves the session; once the runner lets go, the
// same delete removes it.
func TestADeleteRacedByAClaimIsRetried(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	var lease session.Lease
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionDelete && lease == nil {
			l, err := f.sessions.Acquire(t.Context(), s.ID, session.Holder{Runner: "runner-a", AcquiredAt: time.Now()})
			if err != nil {
				t.Errorf("claim the session: %v", err)
			}
			lease = l
		}
		return f.authz.next.Authorize(t.Context(), req)
	}
	if a, _ := f.deleteAs("alice", s.ID); a.code() != CodeConflict {
		t.Fatalf("a delete raced by a claim: %d %s", a.status, a.body)
	}
	if _, err := f.sessions.Get(t.Context(), s.ID); err != nil {
		t.Fatalf("the claimed session after a refused delete: %v", err)
	}
	if lease == nil {
		t.Fatal("the race never claimed the session")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if a, _ := f.deleteAs("alice", s.ID); a.status != http.StatusNoContent {
		t.Fatalf("the retried delete: %d %s", a.status, a.body)
	}
	if _, err := f.sessions.Get(t.Context(), s.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("the session after the retried delete: %v", err)
	}
}
