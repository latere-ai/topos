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
