// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// createLabeled creates a session of agent as token with metadata.
func (f *fixture) createLabeled(token, agent string, meta map[string]string) session.Session {
	f.t.Helper()
	b, err := json.Marshal(map[string]any{"agent": agent, "metadata": meta})
	if err != nil {
		f.t.Fatal(err)
	}
	a := f.do(http.MethodPost, "/v1/sessions", token, string(b))
	if a.status != http.StatusCreated {
		f.t.Fatalf("create with %v: %d %s", meta, a.status, a.body)
	}
	var s session.Session
	a.decode(f.t, &s)
	return s
}

// deny makes f's authorizer deny action and answer every other question
// as before.
func (f *fixture) deny(action string) {
	next := f.authz.next
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == action {
			return authz.Decision{Allow: false, Reason: "label_not_allowed"}, nil
		}
		return next.Authorize(context.Background(), req)
	}
}

// TestTheCreateQuestionNamesMetadata: a create's session.create carries
// the metadata the session will hold, as an object of strings, and none
// when the session has none; a deny creates nothing.
func TestTheCreateQuestionNamesMetadata(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	var create questions
	create.keep(f, authorizer.ActionSessionCreate)
	s := f.createLabeled("alice", "reviewer", map[string]string{"project": "prj_1", "pinned": "yes"})
	q := create.last(t)
	if got, ok := q.Resource.Fields["metadata"].(map[string]any); !ok || len(got) != 2 || got["project"] != "prj_1" || got["pinned"] != "yes" {
		t.Fatalf("session.create asked with metadata %#v", q.Resource.Fields["metadata"])
	}
	if !maps.Equal(s.Metadata, map[string]string{"project": "prj_1", "pinned": "yes"}) {
		t.Fatalf("the session holds %v", s.Metadata)
	}
	f.create("alice", "reviewer")
	if q := create.last(t); q.Resource.Fields["metadata"] != nil {
		t.Fatalf("a session with no metadata was asked with %#v", q.Resource.Fields["metadata"])
	}
	before := f.count()
	f.deny(authorizer.ActionSessionCreate)
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","metadata":{"project":"prj_2"}}`)
	if a.status != http.StatusForbidden || f.count() != before {
		t.Fatalf("a denied create: %d %s, %d sessions, had %d", a.status, a.body, f.count(), before)
	}
}

// TestAForkQuestionNamesMetadata: a fork's session.fork carries the
// metadata it copies from the session it forks, and the fork holds it.
func TestAForkQuestionNamesMetadata(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.createLabeled("alice", "reviewer", map[string]string{"project": "prj_1"})
	f.turn(s.ID, 1, "Done.", 1)
	var fork questions
	fork.keep(f, authorizer.ActionSessionFork)
	child := f.forked(s.ID, "")
	if got, ok := fork.last(t).Resource.Fields["metadata"].(map[string]any); !ok || len(got) != 1 || got["project"] != "prj_1" {
		t.Fatalf("session.fork asked with metadata %#v", fork.last(t).Resource.Fields["metadata"])
	}
	if child.Metadata["project"] != "prj_1" {
		t.Fatalf("the fork holds %v", child.Metadata)
	}
}

// TestMetadataBounds: a key outside the rule, a value past
// MaxMetadataValue or holding a control character, and more than
// MaxMetadata entries are invalid_request at a create and at a change,
// before any question, and a change is checked on the merged result.
func TestMetadataBounds(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	full := map[string]string{}
	for i := range session.MaxMetadata {
		full["k"+strings.Repeat("x", i)] = "v"
	}
	s := f.createLabeled("alice", "reviewer", full)
	long := strings.Repeat("v", session.MaxMetadataValue+1)
	for _, c := range []struct {
		name string
		meta map[string]string
	}{
		{"a key with a space", map[string]string{"a key": "v"}},
		{"a key that starts with a dot", map[string]string{".k": "v"}},
		{"a long key", map[string]string{strings.Repeat("k", 64): "v"}},
		{"a long value", map[string]string{"k": long}},
		{"a control character", map[string]string{"k": "a\nb"}},
		{"too many entries", func() map[string]string { m := maps.Clone(full); m["one-more"] = "v"; return m }()},
	} {
		t.Run("create/"+c.name, func(t *testing.T) {
			b, err := json.Marshal(map[string]any{"agent": "reviewer", "metadata": c.meta})
			if err != nil {
				t.Fatal(err)
			}
			f.authz.take()
			a := f.do(http.MethodPost, "/v1/sessions", "alice", string(b))
			if a.status != http.StatusBadRequest || a.code() != CodeInvalidRequest {
				t.Fatalf("%d %s", a.status, a.body)
			}
			if asked := f.authz.take(); len(asked) != 0 {
				t.Fatalf("a refused create asked %v", asked)
			}
		})
	}
	for _, c := range []struct{ name, body string }{
		{"a key with a space", `{"metadata":{"a key":"v"}}`},
		{"a long value", `{"metadata":{"k":"` + long + `"}}`},
		{"a control character", `{"metadata":{"k":"a\tb"}}`},
		{"an empty change", `{"metadata":{}}`},
		{"a value that is no string", `{"metadata":{"k":1}}`},
		{"past the entries once merged", `{"metadata":{"one-more":"v"}}`},
	} {
		t.Run("change/"+c.name, func(t *testing.T) {
			f.authz.take()
			a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", c.body)
			if a.status != http.StatusBadRequest || a.code() != CodeInvalidRequest {
				t.Fatalf("%d %s", a.status, a.body)
			}
			if asked := f.authz.take(); slices.Contains(asked, authorizer.ActionSessionUpdate) {
				t.Fatalf("a refused change asked %v", asked)
			}
		})
	}
	// A change that deletes one entry and sets another stays within the
	// bound once merged.
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"metadata":{"k":null,"one-more":"v"}}`); a.status != http.StatusOK {
		t.Fatalf("a change within the bound once merged: %d %s", a.status, a.body)
	}
	// A session that holds a key from before the rules is held to them
	// on a change, unless the change deletes it.
	legacy := f.createLabeled("alice", "reviewer", map[string]string{"ok": "v"})
	if _, err := f.sessions.(session.Labeler).SetMetadata(t.Context(), legacy.ID, map[string]*string{"old key": new("v")}); err != nil {
		t.Fatal(err)
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+legacy.ID, "alice", `{"metadata":{"ok":"w"}}`); a.status != http.StatusBadRequest {
		t.Fatalf("a change that keeps a key outside the rule: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+legacy.ID, "alice", `{"metadata":{"old key":null}}`); a.status != http.StatusBadRequest {
		t.Fatalf("a body naming a key outside the rule is refused before the merge: %d %s", a.status, a.body)
	}
}

// listedBy lists the sessions as token with the query q, one page of at
// most MaxLimit, and answers their ids.
func listedBy(t *testing.T, f *fixture, token, q string) []string {
	t.Helper()
	ids := f.listIDs(token, q)
	if ids == nil {
		ids = []string{}
	}
	return ids
}

// TestListFiltersByOneMetadataEntry: metadata.<key>=<value> lists the
// sessions holding that entry exactly, within the caller's scope, beside
// the other filters and under group=tree.
func TestListFiltersByOneMetadataEntry(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	f.apply("bob", "reviewer", "Review.")
	a1 := f.createLabeled("alice", "reviewer", map[string]string{"project": "prj_1"})
	a2 := f.createLabeled("alice", "reviewer", map[string]string{"project": "prj_2"})
	a3 := f.createLabeled("alice", "reviewer", map[string]string{"project": "prj_1", "pinned": "yes"})
	f.createLabeled("bob", "reviewer", map[string]string{"project": "prj_1"})
	f.turn(a3.ID, 1, "Done.", 1)
	fork := f.forked(a3.ID, "")
	f.ended("alice", "reviewer")
	for _, c := range []struct {
		name, q string
		want    []string
	}{
		{"one entry within the caller's own", "?metadata.project=prj_1", []string{fork.ID, a3.ID, a1.ID}},
		{"another value", "?metadata.project=prj_2", []string{a2.ID}},
		{"another key", "?metadata.pinned=yes", []string{fork.ID, a3.ID}},
		{"beside a status", "?metadata.project=prj_1&status=idle", []string{fork.ID, a3.ID, a1.ID}},
		{"beside a parent", "?metadata.project=prj_1&parent=" + a3.ID, []string{fork.ID}},
		{"grouped by tree", "?metadata.project=prj_1&group=tree", []string{fork.ID, a1.ID}},
		{"a value no session holds", "?metadata.project=prj_9", []string{}},
		{"escaped", "?metadata.project=" + url.QueryEscape("prj 1"), []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := listedBy(t, f, "alice", c.q); !slices.Equal(got, c.want) {
				t.Fatalf("listed %v, want %v", got, c.want)
			}
		})
	}
	// An archived session holding the entry is listed when archived asks.
	if a := f.do(http.MethodPost, "/v1/sessions/"+a1.ID+"/archive", "alice", ""); a.status != http.StatusOK {
		t.Fatalf("archive: %d %s", a.status, a.body)
	}
	if got := listedBy(t, f, "alice", "?metadata.project=prj_1&archived=true"); !slices.Equal(got, []string{a1.ID}) {
		t.Fatalf("archived and labeled: %v", got)
	}
}

// TestSummaryFiltersByMetadata: the summary counts the sessions the list
// would page through under the same filter.
func TestSummaryFiltersByMetadata(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	f.apply("alice", "writer", "Write.")
	f.createLabeled("alice", "reviewer", map[string]string{"project": "prj_1"})
	f.createLabeled("alice", "writer", map[string]string{"project": "prj_1"})
	f.createLabeled("alice", "writer", map[string]string{"project": "prj_2"})
	ended := f.ended("alice", "reviewer")
	if a := f.do(http.MethodPatch, "/v1/sessions/"+ended.ID, "alice", `{"metadata":{"project":"prj_1"}}`); a.status != http.StatusOK {
		t.Fatalf("label an ended session: %d %s", a.status, a.body)
	}
	a := f.do(http.MethodGet, "/v1/sessions/summary?metadata.project=prj_1", "alice", "")
	if a.status != http.StatusOK {
		t.Fatalf("summary: %d %s", a.status, a.body)
	}
	var sum session.Summary
	a.decode(t, &sum)
	if sum.Sessions.Idle != 2 || sum.Sessions.Ended != 1 || sum.Agents != 2 {
		t.Fatalf("summary of prj_1: %+v", sum)
	}
	if a := f.do(http.MethodGet, "/v1/sessions/summary?metadata.project=", "alice", ""); a.status != http.StatusBadRequest {
		t.Fatalf("a summary with an empty value: %d %s", a.status, a.body)
	}
}

// TestMetadataFilterRefusals: two filters, a key outside the rule and an
// empty value are invalid_request naming the parameter.
func TestMetadataFilterRefusals(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	for _, c := range []struct{ name, q, names string }{
		{"two keys", "?metadata.a=1&metadata.b=2", "metadata.b"},
		{"one key twice", "?metadata.a=1&metadata.a=2", "metadata.a"},
		{"a key outside the rule", "?metadata.a%20b=1", "metadata.a b"},
		{"an empty key", "?metadata.=1", "metadata."},
		{"an empty value", "?metadata.a=", "metadata.a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := f.do(http.MethodGet, "/v1/sessions"+c.q, "alice", "")
			if a.status != http.StatusBadRequest || a.code() != CodeInvalidRequest || !strings.Contains(string(a.body), c.names) {
				t.Fatalf("%d %s, want invalid_request naming %s", a.status, a.body, c.names)
			}
		})
	}
}

// TestPatchMergesMetadata: a PATCH with metadata sets a key, deletes one
// with null and keeps one it does not name; it asks session.update with
// metadata as sent and current_metadata, the values the named keys hold
// now; it appends no event, answers the Session after, and a change to
// what the session holds writes nothing. A deny changes nothing.
func TestPatchMergesMetadata(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.createLabeled("alice", "reviewer", map[string]string{"project": "prj_1", "pinned_from": "inbox", "kept": "yes"})
	before := f.header(s.ID)
	var update questions
	update.keep(f, authorizer.ActionSessionUpdate)
	f.authz.take()
	a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"metadata":{"project":"prj_2","pinned_from":null,"absent":null}}`)
	if a.status != http.StatusOK {
		t.Fatalf("patch: %d %s", a.status, a.body)
	}
	if asked := f.authz.take(); !slices.Equal(asked, []string{authorizer.ActionSessionRead, authorizer.ActionSessionUpdate}) {
		t.Fatalf("the change asked %v", asked)
	}
	q := update.last(t)
	change, _ := q.Resource.Fields["metadata"].(map[string]any)
	current, _ := q.Resource.Fields["current_metadata"].(map[string]any)
	switch {
	case q.Resource.String("session_id") != s.ID || q.Resource.ID != s.ID:
		t.Fatalf("session.update asked about %q, %q", q.Resource.ID, q.Resource.String("session_id"))
	case len(change) != 3 || change["project"] != "prj_2" || change["pinned_from"] != nil || !hasNull(change, "absent"):
		t.Fatalf("metadata asked %#v", change)
	case len(current) != 2 || current["project"] != "prj_1" || current["pinned_from"] != "inbox":
		t.Fatalf("current_metadata asked %#v", current)
	case q.Resource.Fields["model"] != nil || q.Resource.Fields["title"] != nil || q.Resource.Fields["approval_mode"] != nil:
		t.Fatalf("a change of the metadata alone asked with %v", q.Resource.Fields)
	}
	var got session.Session
	a.decode(t, &got)
	want := map[string]string{"project": "prj_2", "kept": "yes"}
	if !maps.Equal(got.Metadata, want) {
		t.Fatalf("answered %v, want %v", got.Metadata, want)
	}
	if h := f.header(s.ID); h.LastSeq != before.LastSeq || !maps.Equal(h.Metadata, want) {
		t.Fatalf("stored %v at %d, was at %d", h.Metadata, h.LastSeq, before.LastSeq)
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"metadata":{"project":"prj_2"}}`); a.status != http.StatusOK {
		t.Fatalf("a change to what the session holds: %d %s", a.status, a.body)
	}
	f.deny(authorizer.ActionSessionUpdate)
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"metadata":{"project":"prj_3"}}`); a.status != http.StatusForbidden {
		t.Fatalf("a denied change: %d %s", a.status, a.body)
	}
	if h := f.header(s.ID); !maps.Equal(h.Metadata, want) {
		t.Fatalf("a denied change wrote %v", h.Metadata)
	}
	f.authz.answer = nil
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "bob", `{"metadata":{"project":"prj_3"}}`); a.status != http.StatusNotFound {
		t.Fatalf("another person's change: %d %s", a.status, a.body)
	}
}

// hasNull reports whether m holds k with a null value.
func hasNull(m map[string]any, k string) bool {
	v, ok := m[k]
	return ok && v == nil
}

// TestPatchMetadataInEveryStatus: metadata alone is taken on an idle, a
// running and an ended session; beside a title on an ended session it is
// conflict and changes nothing; beside a title on an idle session both
// change in one question, the title's event appended and the metadata
// written with none.
func TestPatchMetadataInEveryStatus(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	idle := f.create("alice", "reviewer")
	running := f.create("alice", "reviewer")
	f.appendTo(running.ID, 1, session.SessionStatus{Status: session.StatusRunning})
	ended := f.ended("alice", "reviewer")
	for _, s := range []session.Session{idle, running, ended} {
		before := f.header(s.ID)
		a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"metadata":{"project":"prj_1"}}`)
		if a.status != http.StatusOK {
			t.Fatalf("%s session: %d %s", before.Status, a.status, a.body)
		}
		if h := f.header(s.ID); h.Metadata["project"] != "prj_1" || h.LastSeq != before.LastSeq || h.Status != before.Status {
			t.Fatalf("%s session after the change: %v at %d %s", before.Status, h.Metadata, h.LastSeq, h.Status)
		}
	}
	a := f.do(http.MethodPatch, "/v1/sessions/"+ended.ID, "alice", `{"metadata":{"project":"prj_2"},"title":"Notes"}`)
	if a.status != http.StatusConflict {
		t.Fatalf("metadata beside a title on an ended session: %d %s", a.status, a.body)
	}
	if h := f.header(ended.ID); h.Metadata["project"] != "prj_1" || h.Title != "" {
		t.Fatalf("a refused change wrote %v, %q", h.Metadata, h.Title)
	}
	var update questions
	update.keep(f, authorizer.ActionSessionUpdate)
	before := f.header(idle.ID)
	if a := f.do(http.MethodPatch, "/v1/sessions/"+idle.ID, "alice", `{"metadata":{"project":"prj_2"},"title":"Notes"}`); a.status != http.StatusOK {
		t.Fatalf("metadata beside a title: %d %s", a.status, a.body)
	}
	if q := update.last(t); q.Resource.Fields["title"] != "Notes" || q.Resource.Fields["metadata"] == nil || q.Resource.Fields["current_metadata"] == nil {
		t.Fatalf("one question asked with %v", q.Resource.Fields)
	}
	if h := f.header(idle.ID); h.Metadata["project"] != "prj_2" || h.Title != "Notes" || h.LastSeq != before.LastSeq+1 {
		t.Fatalf("after metadata beside a title: %v, %q, at %d, was %d", h.Metadata, h.Title, h.LastSeq, before.LastSeq)
	}
}

// TestMetadataChangeReachesTheSinkWithoutValues: an allowed change that
// changes the metadata is reported to the sink as session.update with the
// keys it changed and none of their values; one that changes nothing is
// not reported.
func TestMetadataChangeReachesTheSinkWithoutValues(t *testing.T) {
	var sink sinkEvents
	f := newFixture(t, func(o *Options) {
		o.Sink = func(_ context.Context, e SinkEvent) error {
			sink.mu.Lock()
			defer sink.mu.Unlock()
			sink.got = append(sink.got, e)
			return nil
		}
	})
	f.apply("alice", "reviewer", "Review.")
	s := f.createLabeled("alice", "reviewer", map[string]string{"project": "prj_secret_1", "kept": "yes"})
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"metadata":{"project":"prj_secret_2","kept":"yes","absent":null}}`); a.status != http.StatusOK {
		t.Fatalf("patch: %d %s", a.status, a.body)
	}
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("the sink received %d events: %+v", len(got), got)
	}
	e := got[0]
	switch {
	case e.Type != authorizer.ActionSessionUpdate || e.Object != (SinkObject{Kind: authorizer.KindSession, ID: s.ID}) || e.SessionID != s.ID || e.Outcome != "ok" || e.Subject != alice:
		t.Fatalf("the event %+v", e)
	case !slices.Equal(e.Attributes["metadata_keys"].([]string), []string{"project"}):
		t.Fatalf("the event's keys %v", e.Attributes["metadata_keys"])
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "prj_secret") {
		t.Fatalf("the event carries a value: %s", b)
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"metadata":{"project":"prj_secret_2"}}`); a.status != http.StatusOK {
		t.Fatalf("an unchanged patch: %d %s", a.status, a.body)
	}
	if n := len(sink.all()); n != 1 {
		t.Fatalf("a change that changed nothing was reported: %d events", n)
	}
}
