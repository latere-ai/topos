// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

// listIDs lists sessions as token with the query q and answers their ids.
func (f *fixture) listIDs(token, q string) []string {
	f.t.Helper()
	var page struct {
		Items []session.Session `json:"items"`
	}
	a := f.do(http.MethodGet, "/v1/sessions"+q, token, "")
	if a.status != http.StatusOK {
		f.t.Fatalf("list %s: %d %s", q, a.status, a.body)
	}
	a.decode(f.t, &page)
	var ids []string
	for _, s := range page.Items {
		ids = append(ids, s.ID)
	}
	return ids
}

// ended creates a session of agent as token and ends it.
func (f *fixture) ended(token, agent string) session.Session {
	f.t.Helper()
	s := f.create(token, agent)
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/end", token, `{"reason":"completed"}`); a.status != http.StatusOK {
		f.t.Fatalf("end: %d %s", a.status, a.body)
	}
	return s
}

// TestArchiveASession: an ended session archives and unarchives, each
// asking session.read and then session.update with archived; a repeat
// keeps the first archived_at; the archived session is read, streamed's
// log listed and forked by id, and nothing is appended to its log.
func TestArchiveASession(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	clock := now
	f := newFixture(t, func(o *Options) { o.Now = func() time.Time { return clock } })
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	f.turn(s.ID, 1, "Done.", 1)
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/end", "alice", `{"reason":"completed"}`); a.status != http.StatusOK {
		t.Fatalf("end: %d %s", a.status, a.body)
	}
	before, err := f.sessions.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var update questions
	update.keep(f, authorizer.ActionSessionUpdate)
	f.authz.take()
	a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/archive", "alice", "")
	if a.status != http.StatusOK {
		t.Fatalf("archive: %d %s", a.status, a.body)
	}
	if got := f.authz.take(); !slices.Equal(got, []string{authorizer.ActionSessionRead, authorizer.ActionSessionUpdate}) {
		t.Fatalf("the archive asked %v", got)
	}
	if q := update.last(t); q.Resource.ID != s.ID || q.Resource.Fields["archived"] != true || q.Resource.String("session_id") != s.ID {
		t.Fatalf("session.update asked with %v", q.Resource.Fields)
	}
	var got session.Session
	a.decode(t, &got)
	if got.ArchivedAt == nil || !got.ArchivedAt.Equal(now) || got.LastSeq != before.LastSeq {
		t.Fatalf("archived %v at last_seq %d, want %v at %d", got.ArchivedAt, got.LastSeq, now, before.LastSeq)
	}
	clock = now.Add(time.Hour)
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/archive", "alice", "{}"); a.status != http.StatusOK {
		t.Fatalf("archive again: %d %s", a.status, a.body)
	} else if a.decode(t, &got); !got.ArchivedAt.Equal(now) {
		t.Fatalf("a second archive moved archived_at to %v", got.ArchivedAt)
	}
	for _, path := range []string{"", "/events"} {
		if a := f.do(http.MethodGet, "/v1/sessions/"+s.ID+path, "alice", ""); a.status != http.StatusOK {
			t.Fatalf("read %s of an archived session: %d %s", path, a.status, a.body)
		}
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", ""); a.status != http.StatusCreated {
		t.Fatalf("fork an archived session: %d %s", a.status, a.body)
	}
	a = f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/unarchive", "alice", "")
	if a.status != http.StatusOK {
		t.Fatalf("unarchive: %d %s", a.status, a.body)
	}
	if q := update.last(t); q.Resource.Fields["archived"] != false {
		t.Fatalf("session.update on unarchive asked with %v", q.Resource.Fields)
	}
	var unarchived session.Session
	if a.decode(t, &unarchived); unarchived.ArchivedAt != nil {
		t.Fatalf("unarchived session keeps archived_at %v", unarchived.ArchivedAt)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/unarchive", "alice", ""); a.status != http.StatusOK {
		t.Fatalf("unarchive again: %d %s", a.status, a.body)
	}
	after, err := f.sessions.Get(t.Context(), s.ID)
	if err != nil || after.LastSeq != before.LastSeq {
		t.Fatalf("archiving appended to the log: last_seq %d, was %d, %v", after.LastSeq, before.LastSeq, err)
	}
}

// TestAnArchiveIsRefused: an idle session is conflict and stays as it
// is, a denied update is forbidden and changes nothing, and a caller who
// may not read the session hears not_found.
func TestAnArchiveIsRefused(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	idle := f.create("alice", "reviewer")
	if a := f.do(http.MethodPost, "/v1/sessions/"+idle.ID+"/archive", "alice", ""); a.code() != CodeConflict {
		t.Fatalf("archive an idle session: %d %s", a.status, a.body)
	}
	if s, err := f.sessions.Get(t.Context(), idle.ID); err != nil || s.ArchivedAt != nil || s.Status != session.StatusIdle {
		t.Fatalf("the idle session after a refused archive: %+v, %v", s, err)
	}
	done := f.ended("alice", "reviewer")
	if a := f.do(http.MethodPost, "/v1/sessions/"+done.ID+"/archive", "bob", ""); a.status != http.StatusNotFound {
		t.Fatalf("bob archives alice's session: %d %s", a.status, a.body)
	}
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionUpdate {
			return authz.Decision{Reason: "role_insufficient"}, nil
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+done.ID+"/archive", "alice", ""); a.status != http.StatusForbidden {
		t.Fatalf("a denied archive: %d %s", a.status, a.body)
	}
	if s, err := f.sessions.Get(t.Context(), done.ID); err != nil || s.ArchivedAt != nil {
		t.Fatalf("a denied archive changed %+v, %v", s.ArchivedAt, err)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+done.ID+"/archive", "alice", `{"now":true}`); a.code() != CodeInvalidRequest {
		t.Fatalf("an archive with a body member: %d %s", a.status, a.body)
	}
}

// TestListLeavesArchivedSessionsOut: a list leaves archived sessions out
// by default and with archived=false, lists only them with
// archived=true and both with archived=any; another value is
// invalid_request.
func TestListLeavesArchivedSessionsOut(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	open := f.create("alice", "reviewer").ID
	kept := f.ended("alice", "reviewer").ID
	filed := f.ended("alice", "reviewer").ID
	if a := f.do(http.MethodPost, "/v1/sessions/"+filed+"/archive", "alice", ""); a.status != http.StatusOK {
		t.Fatalf("archive: %d %s", a.status, a.body)
	}
	for q, want := range map[string][]string{
		"":                             {kept, open},
		"?archived=false":              {kept, open},
		"?archived=true":               {filed},
		"?archived=any":                {filed, kept, open},
		"?archived=true&status=ended":  {filed},
		"?archived=any&agent=reviewer": {filed, kept, open},
	} {
		if got := f.listIDs("alice", q); !slices.Equal(got, want) {
			t.Errorf("list %q: %v, want %v", q, got, want)
		}
	}
	if a := f.do(http.MethodGet, "/v1/sessions?archived=maybe", "alice", ""); a.code() != CodeInvalidRequest {
		t.Fatalf("archived=maybe: %d %s", a.status, a.body)
	}
}

// TestReapDeletesAnArchivedSession: retention runs for an archived
// session as for any other.
func TestReapDeletesAnArchivedSession(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	f := newFixture(t, func(o *Options) { o.Now = func() time.Time { return now } })
	ctx := t.Context()
	s := storetest.NewSession()
	s.ExpiresAt, s.Limits.Retention = now.Add(time.Hour), "1h"
	if err := f.sessions.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	ev, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopCompleted}, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	batch := []session.Event{ev}
	session.Stamp(s.ID, 0, batch)
	if _, err := f.sessions.Append(ctx, s.ID, 0, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.(session.Archiver).SetArchived(ctx, s.ID, &now); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.Get(ctx, s.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("the archived session past its retention: %v", err)
	}
}
