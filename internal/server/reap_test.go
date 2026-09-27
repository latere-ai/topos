// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"testing"
	"time"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

// TestReaperExpiresAndDeletes: an idle session past expires_at is ended
// expired; an ended session past its retention is deleted, one within it
// and one with no retention are kept; an idle session before its expiry
// is left alone.
func TestReaperExpiresAndDeletes(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	f := newFixture(t, func(o *Options) { o.Now = func() time.Time { return now } })
	ctx := t.Context()
	create := func(expires time.Time, retention string) session.Session {
		t.Helper()
		s := storetest.NewSession()
		s.ExpiresAt, s.Limits.Retention = expires, retention
		if err := f.sessions.Create(ctx, s, nil); err != nil {
			t.Fatal(err)
		}
		return s
	}
	end := func(s session.Session, at time.Time) {
		t.Helper()
		ev, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopCompleted}, at)
		if err != nil {
			t.Fatal(err)
		}
		batch := []session.Event{ev}
		session.Stamp(s.ID, 0, batch)
		if _, err := f.sessions.Append(ctx, s.ID, 0, batch); err != nil {
			t.Fatal(err)
		}
	}
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	expired := create(past, "")
	current := create(future, "")
	gone := create(future, "1h")
	end(gone, now.Add(-2*time.Hour))
	kept := create(future, "24h")
	end(kept, now.Add(-2*time.Hour))
	forever := create(future, "")
	end(forever, now.Add(-1000*time.Hour))

	if err := f.api.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if s, err := f.sessions.Get(ctx, expired.ID); err != nil || s.Status != session.StatusEnded || s.StopReason != session.StopExpired {
		t.Fatalf("the expired session %+v, %v", s, err)
	}
	if s, err := f.sessions.Get(ctx, current.ID); err != nil || s.Status != session.StatusIdle {
		t.Fatalf("the current session %+v, %v", s, err)
	}
	if _, err := f.sessions.Get(ctx, gone.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("the session past its retention: %v", err)
	}
	for _, s := range []session.Session{kept, forever} {
		if _, err := f.sessions.Get(ctx, s.ID); err != nil {
			t.Fatalf("%s was deleted: %v", s.ID, err)
		}
	}
}
