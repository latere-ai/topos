// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package storetest is the suite every store.Store passes: the memory
// store, the directory store and the Postgres store run the same cases.
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
	t.Run("names per owner", func(t *testing.T) { namesPerOwner(t, f) })
	t.Run("versions", func(t *testing.T) { versions(t, f) })
	t.Run("rewrite latest", func(t *testing.T) { rewriteLatest(t, f) })
	t.Run("list", func(t *testing.T) { list(t, f) })
	t.Run("archive", func(t *testing.T) { archive(t, f) })
	t.Run("lookup", func(t *testing.T) { lookup(t, f) })
	t.Run("idempotency", func(t *testing.T) { idempotency(t, f) })
}

// Apply resolves an Agent manifest against owner's agents and stores
// the version it resolves to when that version is new, as the API's
// apply does. It returns the resolved agent.
func Apply(t *testing.T, st store.Store, owner, name, instructions string) manifest.Resolved {
	t.Helper()
	doc := fmt.Sprintf("apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: %s\nspec:\n  model: {name: claude-haiku-4-5}\n  instructions: %q\n", name, instructions)
	rs, err := manifest.Resolve(t.Context(), []byte(doc), manifest.Options{Lookup: store.Lookup(st, owner)})
	if err != nil {
		t.Fatal(err)
	}
	r := rs[0]
	stored, err := st.AgentByName(t.Context(), owner, name)
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
		a, err := store.FindAgent(t.Context(), st, "alice", ref)
		if err != nil || a.ID != id || a.Name != "reviewer" || a.Owner != "alice" || a.Latest != 1 || a.ArchivedAt != nil {
			t.Fatalf("FindAgent(%s) = %+v, %v", ref, a, err)
		}
	}
	if _, err := st.AgentByName(t.Context(), "alice", "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown name: %v", err)
	}
	if _, err := st.Agent(t.Context(), session.NewID(session.PrefixAgent)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown id: %v", err)
	}
	if _, err := st.Agent(t.Context(), "reviewer"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a name read as an id: %v", err)
	}
	v, err := st.Version(t.Context(), id, 1)
	if err != nil || v.Digest != r.Digest || len(v.Doc) == 0 || len(v.Bundle) == 0 || v.CreatedBy != "alice" {
		t.Fatalf("Version = %+v, %v", v, err)
	}
	other := r.Agent.Status
	other.ID = session.NewID(session.PrefixAgent)
	dup := store.Agent{ID: other.ID, Name: "reviewer", Owner: "alice"}
	if err := st.PutVersion(t.Context(), dup, store.AgentVersion{AgentID: other.ID, Version: 1, Digest: v.Digest, Doc: v.Doc, Bundle: v.Bundle}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second agent of one name for one owner: %v", err)
	}
	if _, err := st.Agent(t.Context(), other.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused second agent of the name was stored: %v", err)
	}
	if err := st.PutVersion(t.Context(), store.Agent{}, store.AgentVersion{AgentID: "nope", Version: 1}); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("a malformed version: %v", err)
	}
	binary := store.Agent{ID: session.NewID(session.PrefixAgent), Name: "binary", Owner: "alice"}
	if err := st.PutVersion(t.Context(), binary, store.AgentVersion{AgentID: binary.ID, Version: 1, Digest: v.Digest, Doc: []byte{0xff, 0xfe}, Bundle: v.Bundle}); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("a document that is not UTF-8 text: %v", err)
	}
	if _, err := st.AgentByName(t.Context(), "alice", "binary"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused version created its agent: %v", err)
	}
}

// namesPerOwner: two owners each hold an agent of one name, each reads
// its own by the name, a third owner reads neither by it, and every id
// reads its agent whoever asks. A lookup of another owner's name answers
// as one of a name nobody holds.
func namesPerOwner(t *testing.T, f Factory) {
	st := f(t, NewClock().Now)
	mine := Apply(t, st, "alice", "coding-agent", "Alice's.").Agent.Status
	theirs := Apply(t, st, "bob", "coding-agent", "Bob's.").Agent.Status
	if mine.ID == theirs.ID || mine.Version != 1 || theirs.Version != 1 {
		t.Fatalf("two owners' agents of one name: %+v and %+v", mine, theirs)
	}
	for owner, id := range map[string]string{"alice": mine.ID, "bob": theirs.ID} {
		a, err := st.AgentByName(t.Context(), owner, "coding-agent")
		if err != nil || a.ID != id || a.Owner != owner {
			t.Fatalf("%s's coding-agent = %+v, %v", owner, a, err)
		}
		for _, reader := range []string{"alice", "bob", "carol"} {
			if a, err := store.FindAgent(t.Context(), st, reader, id); err != nil || a.Owner != owner {
				t.Fatalf("%s finds %s's agent by id: %+v, %v", reader, owner, a, err)
			}
		}
	}
	_, none := st.AgentByName(t.Context(), "carol", "nobody")
	if _, err := st.AgentByName(t.Context(), "carol", "coding-agent"); !errors.Is(err, store.ErrNotFound) || err.Error() != strings.Replace(none.Error(), "nobody", "coding-agent", 1) {
		t.Fatalf("another owner's name: %v, a name nobody holds: %v", err, none)
	}
	for owner, instructions := range map[string]string{"alice": "Alice's.", "bob": "Bob's."} {
		a, err := store.Lookup(st, owner).Agent(t.Context(), "coding-agent")
		if err != nil || a.Spec.Instructions != instructions {
			t.Fatalf("%s's Lookup of coding-agent = %+v, %v", owner, a, err)
		}
	}
	if _, err := store.Lookup(st, "carol").Agent(t.Context(), "coding-agent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("carol's Lookup of another owner's name: %v", err)
	}
	if a, err := store.Lookup(st, "carol").Agent(t.Context(), theirs.ID+"@1"); err != nil || a.Status.ID != theirs.ID {
		t.Fatalf("carol's Lookup of a pinned id: %+v, %v", a, err)
	}
	if again := Apply(t, st, "bob", "coding-agent", "Bob's, closer."); again.Agent.Status.ID != theirs.ID || again.Agent.Status.Version != 2 {
		t.Fatalf("bob's second apply: %+v", again.Agent.Status)
	}
	if a, err := st.Agent(t.Context(), mine.ID); err != nil || a.Latest != 1 {
		t.Fatalf("bob's version moved alice's agent: %+v, %v", a, err)
	}
	v, err := st.Version(t.Context(), mine.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	dup := store.Agent{ID: session.NewID(session.PrefixAgent), Name: "coding-agent", Owner: "bob"}
	if err := st.PutVersion(t.Context(), dup, store.AgentVersion{AgentID: dup.ID, Version: 1, Digest: v.Digest, Doc: v.Doc, Bundle: v.Bundle}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second agent of bob's name for bob: %v", err)
	}
}

// rewriteLatest: the latest version's document and bundle are replaced
// under its digest, keeping its creator and time; a version that is not
// the latest, another digest, an unknown agent and a malformed version
// are refused, and a refused rewrite changes nothing.
func rewriteLatest(t *testing.T, f Factory) {
	st := f(t, NewClock().Now)
	id := Apply(t, st, "alice", "reviewer", "one").Agent.Status.ID
	v1, err := st.Version(t.Context(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := store.DecodeAgent(v1.Doc)
	if err != nil {
		t.Fatal(err)
	}
	doc.Metadata.DisplayName = "Code Reviewer"
	renamed, err := session.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	next := v1
	next.Doc, next.Bundle, next.CreatedBy = renamed, append(slices.Clone(renamed), '\n'), "bob"
	if err := st.RewriteLatest(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	got, err := st.Version(t.Context(), id, 1)
	if err != nil || string(got.Doc) != string(next.Doc) || string(got.Bundle) != string(next.Bundle) || got.Digest != v1.Digest || got.CreatedBy != v1.CreatedBy || !got.CreatedAt.Equal(v1.CreatedAt) {
		t.Fatalf("rewritten version 1 = %+v, %v", got, err)
	}
	if a, err := st.Agent(t.Context(), id); err != nil || a.Latest != 1 {
		t.Fatalf("a rewrite moved the latest: %+v, %v", a, err)
	}
	other := next
	other.Digest = "sha256:" + strings.Repeat("0", 64)
	if err := st.RewriteLatest(t.Context(), other); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("another digest: %v", err)
	}
	Apply(t, st, "alice", "reviewer", "two")
	stale := next
	stale.Doc = v1.Doc
	if err := st.RewriteLatest(t.Context(), stale); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a version that is no longer the latest: %v", err)
	}
	if got, err := st.Version(t.Context(), id, 1); err != nil || string(got.Doc) != string(next.Doc) {
		t.Fatalf("a refused rewrite changed version 1: %s, %v", got.Doc, err)
	}
	unknown := next
	unknown.AgentID = session.NewID(session.PrefixAgent)
	if err := st.RewriteLatest(t.Context(), unknown); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("an unknown agent: %v", err)
	}
	for name, bad := range map[string]store.AgentVersion{
		"no id":      {Version: 1, Digest: v1.Digest, Doc: v1.Doc, Bundle: v1.Bundle},
		"no version": {AgentID: id, Digest: v1.Digest, Doc: v1.Doc, Bundle: v1.Bundle},
		"no digest":  {AgentID: id, Version: 2, Doc: v1.Doc, Bundle: v1.Bundle},
		"not text":   {AgentID: id, Version: 2, Digest: v1.Digest, Doc: []byte{0xff}, Bundle: v1.Bundle},
	} {
		if err := st.RewriteLatest(t.Context(), bad); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
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
	for _, c := range []string{"!!", store.Cursor("x"), store.Cursor("-1")} {
		if _, _, err := st.Versions(t.Context(), id, 0, c); !errors.Is(err, session.ErrInvalid) {
			t.Fatalf("a forged cursor %q: %v", c, err)
		}
	}
	if rest, next, err := st.Versions(t.Context(), id, 0, store.Cursor("9")); err != nil || len(rest) != 0 || next != "" {
		t.Fatalf("a cursor past the latest: %v, %q, %v", rest, next, err)
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
	l := store.Lookup(st, "alice")
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
