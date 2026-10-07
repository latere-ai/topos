// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"latere.ai/x/topos/session"
)

// labeled creates a session per entry of labels, newest last, each with
// that metadata and of the initiator its name's first letter gives: a
// for usr_1, anything else for usr_2. Sessions named f, a fork of the
// session named e, joins e's tree.
func labeled(t *testing.T, st session.Store, labels []struct {
	name string
	meta map[string]string
}) map[string]session.Session {
	t.Helper()
	out := map[string]session.Session{}
	for _, l := range labels {
		s := NewSession()
		s.Metadata = maps.Clone(l.meta)
		if l.name[0] != 'a' {
			s.Initiator.Subject = "usr_2"
		}
		if l.name == "f" {
			s.Parent, s.Root = &session.Parent{SessionID: out["e"].ID, Seq: 2}, out["e"].ID
		}
		if err := st.Create(t.Context(), s, nil); err != nil {
			t.Fatal(err)
		}
		out[l.name] = s
	}
	return out
}

// testListByMetadata holds a store to spec 057's filter: a list keeps the
// sessions whose metadata holds the entry, exactly, beside every other
// filter and under a grouping by tree, a summary counts the same set, and
// a change of a session's metadata moves it between lists at once.
func testListByMetadata(t *testing.T, st session.Store) {
	s := labeled(t, st, []struct {
		name string
		meta map[string]string
	}{
		{"a1", map[string]string{"folder": "f1", "pinned": "yes"}},
		{"b", map[string]string{"folder": "f1"}},
		{"a2", map[string]string{"folder": "f2"}},
		{"c", map[string]string{"folder": "F1"}},
		{"e", map[string]string{"other": "f1"}},
		{"f", map[string]string{"folder": "f1"}},
		{"g", nil},
	})
	entry := func(k, v string) *session.MetadataEntry { return &session.MetadataEntry{Key: k, Value: v} }
	for _, c := range []struct {
		name string
		o    session.ListOptions
		want []string
	}{
		{"one entry", session.ListOptions{Metadata: entry("folder", "f1")}, []string{"f", "b", "a1"}},
		{"a value matches exactly", session.ListOptions{Metadata: entry("folder", "F1")}, []string{"c"}},
		{"a value under another key", session.ListOptions{Metadata: entry("other", "f1")}, []string{"e"}},
		{"within the owners", session.ListOptions{Metadata: entry("folder", "f1"), Owners: []string{"usr_1"}}, []string{"a1"}},
		{"within a tree", session.ListOptions{Metadata: entry("folder", "f1"), Root: s["e"].ID}, []string{"f"}},
		{"grouped by tree", session.ListOptions{Metadata: entry("folder", "f1"), Group: session.GroupTree}, []string{"f(e 1)", "b(b 1)", "a1(a1 1)"}},
		{"no session holds it", session.ListOptions{Metadata: entry("folder", "f9")}, []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := listed(t, st, c.o, s); !slices.Equal(got, c.want) {
				t.Fatalf("listed %v, want %v", got, c.want)
			}
		})
	}
	if summarizer, ok := st.(session.Summarizer); ok {
		sum, err := summarizer.Summarize(t.Context(), session.ListOptions{Metadata: entry("folder", "f1")})
		if err != nil || sum.Sessions.Idle != 3 || sum.Agents != 3 {
			t.Fatalf("summary by folder f1: %+v, %v; want 3 idle sessions of 3 agents", sum, err)
		}
	}
	l, ok := st.(session.Labeler)
	if !ok {
		return
	}
	if _, err := l.SetMetadata(t.Context(), s["a2"].ID, map[string]*string{"folder": new("f1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SetMetadata(t.Context(), s["b"].ID, map[string]*string{"folder": nil}); err != nil {
		t.Fatal(err)
	}
	if got := listed(t, st, session.ListOptions{Metadata: entry("folder", "f1")}, s); !slices.Equal(got, []string{"f", "a2", "a1"}) {
		t.Fatalf("after a2 moved in and b out, listed %v", got)
	}
}

// testSetMetadata holds a Labeler to spec 057: a change merges, a value
// setting its key and nil deleting it, appends nothing to the log, keeps
// across a later batch's rewrite of the header, writes nothing when it
// changes nothing, refuses a merge past MaxMetadata, and answers a
// missing session as not found.
func testSetMetadata(t *testing.T, st session.Store) {
	l, ok := st.(session.Labeler)
	if !ok {
		t.Skip("the store does not relabel")
	}
	s := NewSession()
	s.Metadata = map[string]string{"folder": "f1", "pinned_from": "inbox", "kept": "yes"}
	if err := st.Create(t.Context(), s, nil); err != nil {
		t.Fatal(err)
	}
	last := appendAll(t, st, s.ID, 0, Status(t, session.StatusEnded, session.StopCompleted, t0))
	got, err := l.SetMetadata(t.Context(), s.ID, map[string]*string{"folder": new("f2"), "pinned_from": nil, "new": new("1"), "absent": nil})
	want := map[string]string{"folder": "f2", "kept": "yes", "new": "1"}
	if err != nil || !maps.Equal(got.Metadata, want) {
		t.Fatalf("SetMetadata answered %v, %v; want %v", got.Metadata, err, want)
	}
	read, err := st.Get(t.Context(), s.ID)
	if err != nil || !maps.Equal(read.Metadata, want) || read.LastSeq != last || read.Status != session.StatusEnded {
		t.Fatalf("read after the change: %v at %d %s, %v", read.Metadata, read.LastSeq, read.Status, err)
	}
	if evs, err := st.Events(t.Context(), s.ID, 1, 0); err != nil || len(evs) != int(last) {
		t.Fatalf("the change appended to the log: %d events, %v", len(evs), err)
	}
	if got, err := l.SetMetadata(t.Context(), s.ID, map[string]*string{"folder": new("f2"), "gone": nil}); err != nil || !maps.Equal(got.Metadata, want) {
		t.Fatalf("a change to what the session holds answered %v, %v", got.Metadata, err)
	}
	// A batch that rewrites the header keeps what the change set.
	other := create(t, st)
	if _, err := l.SetMetadata(t.Context(), other.ID, map[string]*string{"folder": new("f3")}); err != nil {
		t.Fatal(err)
	}
	appendAll(t, st, other.ID, 0, Status(t, session.StatusEnded, session.StopCompleted, t0))
	if read, err := st.Get(t.Context(), other.ID); err != nil || read.Metadata["folder"] != "f3" {
		t.Fatalf("metadata after an append: %v, %v", read.Metadata, err)
	}
	if got, err := l.SetMetadata(t.Context(), other.ID, map[string]*string{"folder": nil}); err != nil || got.Metadata != nil {
		t.Fatalf("deleting the last entry answered %v, %v; want none", got.Metadata, err)
	}
	full := map[string]*string{}
	for i := range session.MaxMetadata {
		full["k"+string(rune('a'+i%26))+string(rune('a'+i/26))] = new("v")
	}
	if _, err := l.SetMetadata(t.Context(), other.ID, full); err != nil {
		t.Fatalf("a change to exactly %d entries: %v", session.MaxMetadata, err)
	}
	if _, err := l.SetMetadata(t.Context(), other.ID, map[string]*string{"one-more": new("v")}); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("a merge past %d entries: %v, want invalid", session.MaxMetadata, err)
	}
	if read, err := st.Get(t.Context(), other.ID); err != nil || len(read.Metadata) != session.MaxMetadata {
		t.Fatalf("a refused merge wrote %d entries, %v", len(read.Metadata), err)
	}
	if _, err := l.SetMetadata(t.Context(), session.NewID(session.PrefixSession), map[string]*string{"folder": new("f1")}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a change of a missing session: %v, want not found", err)
	}
}
