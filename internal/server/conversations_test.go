// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/session"
)

// spoken creates a session of alice's reviewer with a first message, the
// metadata meta and, when rollout is set, as a run of that rollout.
func (f *fixture) spoken(token, message string, meta map[string]string, rollout string) session.Session {
	f.t.Helper()
	body := map[string]any{"agent": "reviewer", "message": message}
	if meta != nil {
		body["metadata"] = meta
	}
	if rollout != "" {
		body["rollout"] = rollout
	}
	b, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	a := f.do(http.MethodPost, "/v1/sessions", token, string(b))
	if a.status != http.StatusCreated {
		f.t.Fatalf("create %s: %d %s", b, a.status, a.body)
	}
	var s session.Session
	a.decode(f.t, &s)
	return s
}

// reader creates a session of alice's reviewer whose allow names the
// folder of conversations chats, kept by entry, with the metadata meta.
func (f *fixture) reader(entry map[string]*string, meta map[string]string) session.Session {
	f.t.Helper()
	folder := session.MemoryFolder{Name: "chats", Access: session.MemoryReadOnly, Sessions: &session.MemorySessions{Metadata: entry}}
	f.level(func(authz.Request) *authorizer.WireLimits {
		return &authorizer.WireLimits{Memory: []session.MemoryFolder{folder}}
	})
	defer func() { f.authz.answer = nil }()
	s := f.spoken("alice", "As we said last week.", meta, "")
	if !slices.Equal(s.Memory, []session.MemoryFolder{{Name: "chats", Access: session.MemoryReadOnly}}) {
		f.t.Fatalf("the reader answers %+v", s.Memory)
	}
	if h := f.header(s.ID); len(h.Memory) != 1 || !h.Memory[0].Conversations() {
		f.t.Fatalf("the reader's header holds %+v", h.Memory)
	}
	return s
}

// asking records the session.list and session.read questions of f's
// authorizer that name an origin, and denies a read of any session id in
// deny and, while refuse is set, every list; every other question is
// answered as before.
type asking struct {
	mu     sync.Mutex
	reqs   []authz.Request
	deny   map[string]bool
	refuse bool
}

func (q *asking) watch(f *fixture) {
	next := f.authz.next
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if _, ok := req.Resource.Fields["origin"]; ok && (req.Action == authorizer.ActionSessionList || req.Action == authorizer.ActionSessionRead) {
			q.mu.Lock()
			q.reqs = append(q.reqs, req)
			deny := q.deny[req.Resource.ID] || (req.Action == authorizer.ActionSessionList && q.refuse)
			q.mu.Unlock()
			if deny {
				return authz.Decision{Allow: false, Reason: "chats_off"}, nil
			}
		}
		return next.Authorize(context.Background(), req)
	}
}

func (q *asking) take() []authz.Request {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.reqs
	q.reqs = nil
	return out
}

func names(files []memory.ConversationFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Name)
	}
	slices.Sort(out)
	return out
}

func nameOf(ss ...session.Session) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, memory.ConversationName(s))
	}
	slices.Sort(out)
	return out
}

// TestAFolderOfConversations: a folder of conversations lists the
// initiator's own sessions of the context that its entry keeps, a null
// entry those without the key, one per fork tree, never the reading
// session's tree, no rollout's run and none the authorizer denies; a read
// answers a listed conversation rendered from its log and any other name
// as a file that is not there; every question names the reading session
// and the folder as its origin, a read the target's metadata; a refused
// list is the refusal the file tools say (spec 094).
func TestAFolderOfConversations(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	f.apply("bob", "reviewer", "Review.")
	p1 := map[string]string{"project": "p1"}
	tide := f.spoken("alice", "When is high tide at Kiel?", p1, "")
	f.turn(tide.ID, 1, "High tide is at 14:12.", 1)
	tideFork := f.forked(tide.ID, "")
	trains := f.spoken("alice", "The 8:12 to the coast.", p1, "")
	denied := f.spoken("alice", "A conversation the authorizer keeps out.", p1, "")
	other := f.spoken("alice", "Another project.", map[string]string{"project": "p2"}, "")
	none := f.spoken("alice", "No project at all.", nil, "")
	run := f.spoken("alice", "A run of a rollout.", p1, "cel_01.smoke")
	bobs := f.spoken("bob", "Bob's own conversation.", p1, "")

	r := f.reader(map[string]*string{"project": new("p1")}, p1)
	q := &asking{deny: map[string]bool{denied.ID: true}}
	q.watch(f)
	chats := f.api.SessionConversations(r.ID)
	ctx := t.Context()

	files, more, err := chats.ListConversations(ctx, "chats")
	if err != nil || more {
		t.Fatalf("the list: %v, more %v", err, more)
	}
	if got, want := names(files), nameOf(tideFork, trains); !slices.Equal(got, want) {
		t.Fatalf("the folder holds %v, want %v", got, want)
	}
	for _, file := range files {
		if file.Version == "" || file.Modified.IsZero() {
			t.Fatalf("a listed file %+v", file)
		}
	}
	asked := q.take()
	lists, reads := 0, map[string]authz.Request{}
	for _, req := range asked {
		origin, ok := req.Resource.Fields["origin"].(map[string]any)
		if !ok || origin["kind"] != OriginMemory || origin["session_id"] != r.ID || origin["folder"] != "chats" {
			t.Fatalf("%s names the origin %v", req.Action, req.Resource.Fields["origin"])
		}
		if req.Sub != "alice" {
			t.Fatalf("%s is asked as %s", req.Action, req.Sub)
		}
		switch req.Action {
		case authorizer.ActionSessionList:
			lists++
		case authorizer.ActionSessionRead:
			reads[req.Resource.ID] = req
		}
	}
	if lists != 1 {
		t.Fatalf("a list asked session.list %d times", lists)
	}
	if got := slices.Sorted(maps.Keys(reads)); !slices.Equal(got, sortedIDs(tideFork, trains, denied)) {
		t.Fatalf("a list asked session.read of %v", got)
	}
	if meta, ok := reads[trains.ID].Resource.Fields["metadata"].(map[string]any); !ok || meta["project"] != "p1" {
		t.Fatalf("a read names the metadata %v", reads[trains.ID].Resource.Fields["metadata"])
	}

	file, text, err := chats.ReadConversation(ctx, "chats", memory.ConversationName(trains))
	if err != nil || file.Name != memory.ConversationName(trains) || file.Size != int64(len(text)) || !strings.Contains(string(text), "The 8:12 to the coast.") {
		t.Fatalf("a read %+v %q, %v", file, text, err)
	}
	if _, text, err := chats.ReadConversation(ctx, "chats", memory.ConversationName(tideFork)); err != nil || !strings.Contains(string(text), "High tide is at 14:12.") {
		t.Fatalf("a read of a fork %q, %v", text, err)
	}
	q.take()
	for name, s := range map[string]session.Session{"a denied session": denied, "another project's": other, "no project's": none,
		"a rollout's run": run, "another person's": bobs, "the reading session": r} {
		if _, _, err := chats.ReadConversation(ctx, "chats", memory.ConversationName(s)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a read of %s: %v", name, err)
		}
	}
	for _, req := range q.take() {
		if req.Action == authorizer.ActionSessionList {
			t.Fatal("a read asked session.list")
		}
		if req.Resource.ID != denied.ID {
			t.Fatalf("a read asked session.read of %s, which the folder does not hold", req.Resource.ID)
		}
	}
	wrongDay := "2001-01-01-" + trains.ID + ".md"
	for _, name := range []string{wrongDay, "MEMORY.md", "ses_missing.md", "2026-10-28-ses_missing.md"} {
		if _, _, err := chats.ReadConversation(ctx, "chats", name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a read of %s: %v", name, err)
		}
	}
	if _, _, err := chats.ListConversations(ctx, "user"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a folder the session does not hold: %v", err)
	}

	q.refuse = true
	_, _, err = chats.ListConversations(ctx, "chats")
	if refusal, ok := errors.AsType[*memory.Refusal](err); !ok || refusal.Status != http.StatusForbidden || refusal.Reason != "chats_off" || refusal.Message != errListRefused {
		t.Fatalf("a refused list: %v", err)
	}
	q.refuse = false

	f.authz.answer = nil
	without := f.reader(map[string]*string{"project": nil}, nil)
	q.watch(f)
	files, _, err = f.api.SessionConversations(without.ID).ListConversations(ctx, "chats")
	if got, want := names(files), nameOf(none); err != nil || !slices.Equal(got, want) {
		t.Fatalf("a folder of the sessions without a project holds %v, %v, want %v", got, err, want)
	}
}

func sortedIDs(ss ...session.Session) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.ID)
	}
	slices.Sort(out)
	return out
}
