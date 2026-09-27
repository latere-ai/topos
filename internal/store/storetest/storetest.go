// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package storetest is the suite every store.Store passes: the memory
// store, the directory store and the Postgres store run the same cases.
package storetest

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/session"
)

// Clock is a clock a case moves.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts a clock at a fixed instant.
func NewClock() *Clock { return &Clock{now: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)} }

// Now reads the clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Factory returns an empty store on the clock.
type Factory func(t *testing.T, now func() time.Time) store.Store

// Run runs every case against stores the factory returns.
func Run(t *testing.T, f Factory) {
	t.Run("agents", func(t *testing.T) { agents(t, f) })
	t.Run("versions", func(t *testing.T) { versions(t, f) })
	t.Run("list", func(t *testing.T) { list(t, f) })
	t.Run("archive", func(t *testing.T) { archive(t, f) })
	t.Run("lookup", func(t *testing.T) { lookup(t, f) })
	t.Run("idempotency", func(t *testing.T) { idempotency(t, f) })
}

// Apply resolves an Agent manifest against the store and stores the
// version it resolves to when that version is new, as the API's apply
// does. It returns the resolved agent.
func Apply(t *testing.T, st store.Store, owner, name, instructions string) manifest.Resolved {
	t.Helper()
	doc := fmt.Sprintf("apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: %s\nspec:\n  model: {name: claude-haiku-4-5}\n  instructions: %q\n", name, instructions)
	rs, err := manifest.Resolve(t.Context(), []byte(doc), manifest.Options{Lookup: store.Lookup(st)})
	if err != nil {
		t.Fatal(err)
	}
	r := rs[0]
	stored, err := st.Agent(t.Context(), name)
	switch {
	case err == nil && stored.Latest == r.Agent.Status.Version:
		return r
	case err != nil && !errors.Is(err, store.ErrNotFound):
		t.Fatal(err)
	}
	d, err := session.Marshal(r.Agent)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	status := r.Agent.Status
	a := store.Agent{ID: status.ID, Name: name, Owner: owner, CreatedAt: status.CreatedAt}
	v := store.AgentVersion{AgentID: status.ID, Version: status.Version, Digest: status.Digest, Doc: d, Bundle: b, CreatedBy: owner, CreatedAt: status.CreatedAt}
	if err := st.PutVersion(t.Context(), a, v); err != nil {
		t.Fatal(err)
	}
	return r
}

func agents(t *testing.T, f Factory) {
	st := f(t, NewClock().Now)
	r := Apply(t, st, "alice", "reviewer", "Review.")
	id := r.Agent.Status.ID
	for _, ref := range []string{"reviewer", id} {
		a, err := st.Agent(t.Context(), ref)
		if err != nil || a.ID != id || a.Name != "reviewer" || a.Owner != "alice" || a.Latest != 1 || a.ArchivedAt != nil {
			t.Fatalf("Agent(%s) = %+v, %v", ref, a, err)
		}
	}
	if _, err := st.Agent(t.Context(), "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown name: %v", err)
	}
	if _, err := st.Agent(t.Context(), session.NewID(session.PrefixAgent)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown id: %v", err)
	}
	v, err := st.Version(t.Context(), id, 1)
	if err != nil || v.Digest != r.Digest || len(v.Doc) == 0 || len(v.Bundle) == 0 || v.CreatedBy != "alice" {
		t.Fatalf("Version = %+v, %v", v, err)
	}
	other := r.Agent.Status
	other.ID = session.NewID(session.PrefixAgent)
	dup := store.Agent{ID: other.ID, Name: "reviewer", Owner: "bob"}
	if err := st.PutVersion(t.Context(), dup, store.AgentVersion{AgentID: other.ID, Version: 1, Digest: v.Digest, Doc: v.Doc, Bundle: v.Bundle}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second agent of one name: %v", err)
	}
	if err := st.PutVersion(t.Context(), store.Agent{}, store.AgentVersion{AgentID: "nope", Version: 1}); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("a malformed version: %v", err)
	}
}

func versions(t *testing.T, f Factory) {
	st := f(t, NewClock().Now)
	id := Apply(t, st, "alice", "reviewer", "one").Agent.Status.ID
	if again := Apply(t, st, "alice", "reviewer", "one"); again.Agent.Status.Version != 1 {
		t.Fatalf("an unchanged spec made version %d", again.Agent.Status.Version)
	}
	Apply(t, st, "alice", "reviewer", "two")
	Apply(t, st, "alice", "reviewer", "three")
	a, err := st.Agent(t.Context(), id)
	if err != nil || a.Latest != 3 {
		t.Fatalf("latest %+v, %v", a, err)
	}
	v3, err := st.Version(t.Context(), id, 3)
	if err != nil {
		t.Fatal(err)
	}
	skip := v3
	skip.Version = 5
	if err := st.PutVersion(t.Context(), a, skip); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a version that skips: %v", err)
	}
	if _, err := st.Version(t.Context(), id, 4); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a version not stored: %v", err)
	}
	var got []int
	cursor := ""
	for {
		page, next, err := st.Versions(t.Context(), id, 2, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range page {
			got = append(got, v.Version)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("the versions paged as %v", got)
	}
	if _, _, err := st.Versions(t.Context(), session.NewID(session.PrefixAgent), 0, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("versions of no agent: %v", err)
	}
	if _, _, err := st.Versions(t.Context(), id, 0, "!!"); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("a forged cursor: %v", err)
	}
}

func list(t *testing.T, f Factory) {
	st := f(t, NewClock().Now)
	var want []string
	for i, owner := range []string{"alice", "bob", "alice", "alice", "bob"} {
		want = append(want, Apply(t, st, owner, fmt.Sprintf("agent%d", i), "x").Agent.Status.ID)
		time.Sleep(2 * time.Millisecond) // ids sort by their millisecond
	}
	var got []string
	cursor := ""
	for {
		page, next, err := st.ListAgents(t.Context(), store.AgentList{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range page {
			got = append(got, a.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the agents paged as %v, want %v", got, want)
	}
	mine, next, err := st.ListAgents(t.Context(), store.AgentList{Owners: []string{"bob"}})
	if err != nil || next != "" || len(mine) != 2 || mine[0].ID != want[1] || mine[1].ID != want[4] {
		t.Fatalf("bob's agents %+v, %q, %v", mine, next, err)
	}
	if _, _, err := st.ListAgents(t.Context(), store.AgentList{Cursor: "!!"}); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("a forged cursor: %v", err)
	}
}

func archive(t *testing.T, f Factory) {
	clock := NewClock()
	st := f(t, clock.Now)
	id := Apply(t, st, "alice", "reviewer", "x").Agent.Status.ID
	if err := st.Archive(t.Context(), id, clock.Now()); err != nil {
		t.Fatal(err)
	}
	a, err := st.Agent(t.Context(), id)
	if err != nil || a.ArchivedAt == nil || !a.ArchivedAt.Equal(clock.Now()) {
		t.Fatalf("archived %+v, %v", a, err)
	}
	if err := st.Archive(t.Context(), id, clock.Now()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second archive: %v", err)
	}
	if err := st.Archive(t.Context(), session.NewID(session.PrefixAgent), clock.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an archive of no agent: %v", err)
	}
}

func lookup(t *testing.T, f Factory) {
	st := f(t, NewClock().Now)
	id := Apply(t, st, "alice", "reviewer", "one").Agent.Status.ID
	Apply(t, st, "alice", "reviewer", "two")
	l := store.Lookup(st)
	for ref, version := range map[string]int{"reviewer": 2, id: 2, id + "@1": 1} {
		a, err := l.Agent(t.Context(), ref)
		if err != nil || a.Status.Version != version || a.Status.ID != id {
			t.Fatalf("Lookup.Agent(%s) = %+v, %v", ref, a, err)
		}
	}
	for _, ref := range []string{id + "@9", id + "@x", id + "@01", "nobody"} {
		if _, err := l.Agent(t.Context(), ref); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Lookup.Agent(%s): %v", ref, err)
		}
	}
	if _, err := l.Trigger(t.Context(), "t"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a trigger was found")
	}
	if _, err := l.MemoryStore(t.Context(), "m"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a memory store was found")
	}
	if _, err := l.Connection(t.Context(), "c"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a connection was found")
	}
	if _, err := l.Credential(t.Context(), "c"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a credential was found")
	}
}

func idempotency(t *testing.T, f Factory) {
	clock := NewClock()
	st := f(t, clock.Now)
	r := store.Idempotency{Subject: "alice", Key: "k1", Route: "POST /v1/sessions", BodyHash: "h1", ExpiresAt: clock.Now().Add(24 * time.Hour)}
	got, fresh, err := st.Begin(t.Context(), r)
	if err != nil || !fresh || got.Done {
		t.Fatalf("Begin %+v, %v, %v", got, fresh, err)
	}
	held, fresh, err := st.Begin(t.Context(), r)
	if err != nil || fresh || held.Done || held.BodyHash != "h1" {
		t.Fatalf("a second Begin while the first runs: %+v, %v, %v", held, fresh, err)
	}
	r.Status, r.ContentType, r.Body = 201, "application/json", []byte(`{"id":"ses_1"}`)
	if err := st.Finish(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	held, fresh, err = st.Begin(t.Context(), r)
	if err != nil || fresh || !held.Done || held.Status != 201 || string(held.Body) != `{"id":"ses_1"}` || held.ContentType != "application/json" {
		t.Fatalf("a repeat after the answer: %+v, %v, %v", held, fresh, err)
	}
	other := r
	other.Subject = "bob"
	if _, fresh, err := st.Begin(t.Context(), other); err != nil || !fresh {
		t.Fatalf("the same key from another subject: %v, %v", fresh, err)
	}
	clock.Advance(25 * time.Hour)
	if _, fresh, err := st.Begin(t.Context(), r); err != nil || !fresh {
		t.Fatalf("a key past its expiry: %v, %v", fresh, err)
	}
	if err := st.Abandon(t.Context(), "alice", "k1"); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := st.Begin(t.Context(), r); err != nil || !fresh {
		t.Fatalf("a key after its request was abandoned: %v, %v", fresh, err)
	}
	if err := st.Finish(t.Context(), store.Idempotency{Subject: "carol", Key: "none"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("finishing a record never begun: %v", err)
	}
}
