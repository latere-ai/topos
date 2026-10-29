// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// TestTheMemoryAnswer: the API answers a session's memory folders by
// name and access alone, none for a session with none, and only a
// read_write folder is writable (spec 093).
func TestTheMemoryAnswer(t *testing.T) {
	fs := []MemoryFolder{
		{Name: "user", URL: "https://files.example.com/v1", Owner: "o", Path: "files/memory/you", Access: MemoryReadOnly},
		{Name: "project", URL: "https://files.example.com/v1", Owner: "o", Path: "files/memory/projects/p1", Access: MemoryReadWrite},
	}
	got := MemoryAnswer(fs)
	if !slices.Equal(got, []MemoryFolder{{Name: "user", Access: MemoryReadOnly}, {Name: "project", Access: MemoryReadWrite}}) {
		t.Fatalf("the answer %+v", got)
	}
	b, err := json.Marshal(got)
	if err != nil || string(b) != `[{"name":"user","access":"read_only"},{"name":"project","access":"read_write"}]` {
		t.Fatalf("on the wire %s, %v", b, err)
	}
	if fs[0].URL == "" || MemoryAnswer(nil) != nil {
		t.Fatal("the answer changed the folders, or answered folders for none")
	}
	if fs[0].Writable() || !fs[1].Writable() {
		t.Fatal("writable")
	}
}

// TestAStoreKeepsItsOwnMemoryFolders: the memory store answers a copy of
// a session's memory folders, so a change to one it answered changes
// nothing it holds.
func TestAStoreKeepsItsOwnMemoryFolders(t *testing.T) {
	st := NewMemoryStore()
	s := New(AgentRef{ID: "agt_1", Version: 1}, Sender{Subject: "usr_ada", Kind: SenderPerson}, RunnerHosted, Machine{Kind: MachineCella}, time.Now())
	s.Memory = []MemoryFolder{{Name: "user", URL: "https://files.example.com/v1", Owner: "o", Path: "files/memory/you", Access: MemoryReadOnly}}
	if err := st.Create(t.Context(), s, nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(t.Context(), s.ID)
	if err != nil || len(got.Memory) != 1 {
		t.Fatalf("get %+v, %v", got.Memory, err)
	}
	got.Memory[0].Access = MemoryReadWrite
	again, err := st.Get(t.Context(), s.ID)
	if err != nil || again.Memory[0].Access != MemoryReadOnly {
		t.Fatalf("the store's folders changed: %+v, %v", again.Memory, err)
	}
}

// TestAFolderOfConversations: a folder may name sessions in place of a
// files store's address (spec 094): read only, at most one metadata
// entry of a key and a value the metadata rules take, and none of url,
// owner or path; its entry keeps by value or, for null, by the key's
// absence; and a store keeps its own copy of the entry.
func TestAFolderOfConversations(t *testing.T) {
	inProject := MemoryFolder{Name: "chats", Access: MemoryReadOnly, Sessions: &MemorySessions{Metadata: map[string]*string{"project": new("p1")}}}
	inNone := MemoryFolder{Name: "chats", Access: MemoryReadOnly, Sessions: &MemorySessions{Metadata: map[string]*string{"project": nil}}}
	every := MemoryFolder{Name: "chats", Access: MemoryReadOnly, Sessions: &MemorySessions{}}
	user := MemoryFolder{Name: "user", URL: "https://files.example.com/v1", Owner: "o", Path: "files/memory/you", Access: MemoryReadOnly}
	for _, fs := range [][]MemoryFolder{{inProject}, {inNone}, {every}, {user, inProject}} {
		if err := CheckMemory(fs); err != nil {
			t.Fatalf("%+v: %v", fs, err)
		}
	}
	with := func(mut func(*MemoryFolder)) []MemoryFolder {
		f := CloneMemory([]MemoryFolder{inProject})[0]
		mut(&f)
		return []MemoryFolder{f}
	}
	for name, fs := range map[string][]MemoryFolder{
		"writable":        with(func(f *MemoryFolder) { f.Access = MemoryReadWrite }),
		"a store's url":   with(func(f *MemoryFolder) { f.URL = "https://files.example.com/v1" }),
		"a store's owner": with(func(f *MemoryFolder) { f.Owner = "o" }),
		"a store's path":  with(func(f *MemoryFolder) { f.Path = "files/x" }),
		"two entries":     with(func(f *MemoryFolder) { f.Sessions.Metadata["folder"] = new("f1") }),
		"a bad key":       with(func(f *MemoryFolder) { f.Sessions.Metadata = map[string]*string{"-x": new("p1")} }),
		"a control value": with(func(f *MemoryFolder) { f.Sessions.Metadata = map[string]*string{"project": new("p\n1")} }),
		"a long value": with(func(f *MemoryFolder) {
			f.Sessions.Metadata = map[string]*string{"project": new(string(make([]byte, MaxMetadataValue+1)))}
		}),
		"no name": with(func(f *MemoryFolder) { f.Name = "" }),
	} {
		if err := CheckMemory(fs); err == nil {
			t.Errorf("%s: %+v checked", name, fs[0])
		}
	}
	if !inProject.Conversations() || user.Conversations() {
		t.Fatal("Conversations")
	}
	s := func(meta map[string]string) Session { return Session{Metadata: meta} }
	for _, c := range []struct {
		f    MemoryFolder
		s    Session
		want bool
	}{
		{inProject, s(map[string]string{"project": "p1"}), true},
		{inProject, s(map[string]string{"project": "p2"}), false},
		{inProject, s(nil), false},
		{inNone, s(nil), true},
		{inNone, s(map[string]string{"other": "x"}), true},
		{inNone, s(map[string]string{"project": "p1"}), false},
		{every, s(map[string]string{"project": "p1"}), true},
	} {
		if got := c.f.Sessions.Entry().Keeps(c.s); got != c.want {
			t.Errorf("%v keeps %v: %v, want %v", c.f.Sessions.Metadata, c.s.Metadata, got, c.want)
		}
	}
	b, err := json.Marshal([]MemoryFolder{inNone})
	if err != nil || string(b) != `[{"name":"chats","access":"read_only","sessions":{"metadata":{"project":null}}}]` {
		t.Fatalf("on the wire %s, %v", b, err)
	}
	if got := MemoryAnswer([]MemoryFolder{inProject}); !slices.Equal(got, []MemoryFolder{{Name: "chats", Access: MemoryReadOnly}}) {
		t.Fatalf("the answer %+v", got)
	}
	st := NewMemoryStore()
	sess := New(AgentRef{ID: "agt_1", Version: 1}, Sender{Subject: "usr_ada", Kind: SenderPerson}, RunnerHosted, Machine{Kind: MachineCella}, time.Now())
	sess.Memory = []MemoryFolder{inProject}
	if err := st.Create(t.Context(), sess, nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(t.Context(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	*got.Memory[0].Sessions.Metadata["project"] = "p9"
	again, err := st.Get(t.Context(), sess.ID)
	if err != nil || *again.Memory[0].Sessions.Metadata["project"] != "p1" {
		t.Fatalf("the store's entry changed: %+v, %v", again.Memory[0].Sessions, err)
	}
}
