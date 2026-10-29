// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arca

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

// chatsStub is a reach to a folder of conversations that answers the
// files it holds and counts its reads.
type chatsStub struct {
	mu      sync.Mutex
	files   map[string]string
	version map[string]string
	reads   int
	listErr error
	readErr error
}

func (c *chatsStub) ListConversations(_ context.Context, folder string) ([]memory.ConversationFile, bool, error) {
	if folder != "chats" {
		return nil, false, fmt.Errorf("listed the folder %s", folder)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.listErr != nil {
		return nil, false, c.listErr
	}
	var out []memory.ConversationFile
	for name := range c.files {
		out = append(out, memory.ConversationFile{Name: name, Version: c.version[name], Modified: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)})
	}
	slices.SortFunc(out, func(a, b memory.ConversationFile) int { return strings.Compare(b.Name, a.Name) })
	return out, false, nil
}

func (c *chatsStub) ReadConversation(_ context.Context, folder, name string) (memory.ConversationFile, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	if c.readErr != nil {
		return memory.ConversationFile{}, nil, c.readErr
	}
	text, ok := c.files[name]
	if folder != "chats" || !ok {
		return memory.ConversationFile{}, nil, fmt.Errorf("%w: %s", fs.ErrNotExist, name)
	}
	return memory.ConversationFile{Name: name, Version: c.version[name], Size: int64(len(text))}, []byte(text), nil
}

var chatsFolder = session.MemoryFolder{Name: "chats", Access: session.MemoryReadOnly, Sessions: &session.MemorySessions{Metadata: map[string]*string{"project": nil}}}

// TestAFolderOfConversations: the memory machine lists, stats, reads,
// globs and greps a folder of conversations through its reach, reads a
// file once per version for a grep, refuses every change before asking,
// says a file the reach does not answer is not there, and needs no token
// for a folder of conversations alone (spec 094).
func TestAFolderOfConversations(t *testing.T) {
	ctx := t.Context()
	chats := &chatsStub{
		files: map[string]string{
			"2026-10-28-ses_2.md": "# Tide tables\n\n## User, 2026-10-28 09:00 UTC\n\nWhen is high tide at Kiel?\n",
			"2026-10-27-ses_1.md": "# Trains\n\n## User, 2026-10-27 09:00 UTC\n\nThe 8:12 to the coast.\n",
		},
		version: map[string]string{"2026-10-28-ses_2.md": "v2", "2026-10-27-ses_1.md": "v1"},
	}
	if _, err := New([]session.MemoryFolder{chatsFolder}, nil, nil, nil); err == nil {
		t.Fatal("a folder of conversations with no reach made a machine")
	}
	m, err := New([]session.MemoryFolder{chatsFolder}, nil, nil, nil, WithConversations(chats))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := m.List(ctx, "/memory/chats")
	if err != nil || len(entries) != 2 || entries[0].Path != "/memory/chats/2026-10-28-ses_2.md" || entries[0].Mode != 0o444 {
		t.Fatalf("the listing %+v, %v", entries, err)
	}
	if root, err := m.List(ctx, "/memory"); err != nil || len(root) != 1 || root[0].Path != "/memory/chats" || root[0].Mode != fs.ModeDir|0o555 {
		t.Fatalf("/memory %+v, %v", root, err)
	}
	got, err := readAll(t, m, "/memory/chats/2026-10-27-ses_1.md")
	if err != nil || !strings.Contains(got, "The 8:12") {
		t.Fatalf("a read %q, %v", got, err)
	}
	info, err := m.Stat(ctx, "/memory/chats/2026-10-28-ses_2.md")
	if err != nil || info.Size != int64(len(chats.files["2026-10-28-ses_2.md"])) || info.IsDir {
		t.Fatalf("a stat %+v, %v", info, err)
	}
	if _, err := readAll(t, m, "/memory/chats/2026-10-26-ses_0.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file no conversation answers: %v", err)
	}
	if _, err := readAll(t, m, "/memory/chats/a/b.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a nested path: %v", err)
	}
	glob, err := m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*.md", Path: "/memory/chats"})
	slices.Sort(glob.Lines)
	if err != nil || !slices.Equal(glob.Lines, []string{"/memory/chats/2026-10-27-ses_1.md", "/memory/chats/2026-10-28-ses_2.md"}) {
		t.Fatalf("a glob %+v, %v", glob, err)
	}
	before := chats.reads
	grep, err := m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGrep, Pattern: `high tide`, Path: "/memory/chats", OutputMode: machine.ModeContent, HeadLimit: 50})
	if err != nil || len(grep.Lines) != 1 || !strings.Contains(grep.Lines[0], "2026-10-28-ses_2.md") {
		t.Fatalf("a grep %+v, %v", grep, err)
	}
	if read := chats.reads - before; read != 0 {
		t.Fatalf("a grep read %d conversations the session had read at their versions", read)
	}
	chats.mu.Lock()
	chats.files["2026-10-27-ses_1.md"] += "\n## Agent, 2026-10-27 09:01 UTC\n\nHigh tide is at 14:12.\n"
	chats.version["2026-10-27-ses_1.md"] = "v1b"
	chats.mu.Unlock()
	before = chats.reads
	grep, err = m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGrep, Pattern: `(?i)high tide`, Path: "/memory", OutputMode: machine.ModeFiles, HeadLimit: 50})
	if err != nil || len(grep.Lines) != 2 {
		t.Fatalf("a grep after a conversation changed %+v, %v", grep, err)
	}
	if read := chats.reads - before; read != 1 {
		t.Fatalf("a grep read %d conversations, want the one that changed", read)
	}
	for name, err := range map[string]error{
		"a write":  write(t, m, "/memory/chats/2026-10-28-ses_2.md", "changed"),
		"a forget": write(t, m, "/memory/chats/2026-10-28-ses_2.md", ""),
		"a remove": m.Remove(ctx, "/memory/chats/2026-10-28-ses_2.md"),
		"a rename": m.Rename(ctx, "/memory/chats/2026-10-28-ses_2.md", "/memory/chats/x.md"),
	} {
		if err == nil {
			t.Errorf("%s of a conversation went through", name)
		}
	}
	if ro, ok := errors.AsType[*memory.ReadOnly](write(t, m, "/memory/chats/new.md", "x")); !ok || ro.Folder != "chats" {
		t.Fatalf("a write is refused as read only before any request, got %v", ro)
	}
	if chats.files["2026-10-28-ses_2.md"] == "changed" {
		t.Fatal("a write changed a conversation")
	}
}

// TestAFolderOfConversationsFails: a refused list is the refusal the file
// tools say, any other failure of the reach is Memory could not be
// reached, and a conversation past the file bound is too large.
func TestAFolderOfConversationsFails(t *testing.T) {
	ctx := t.Context()
	refused := &memory.Refusal{Op: "listing", Status: 403, Code: "forbidden", Message: "This session may not read these conversations now.", Reason: "chats_off"}
	chats := &chatsStub{files: map[string]string{"2026-10-28-ses_2.md": strings.Repeat("x", memory.MaxFileBytes+1)}, version: map[string]string{}, listErr: refused}
	m, err := New([]session.MemoryFolder{chatsFolder}, nil, nil, nil, WithConversations(chats))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*", Path: "/memory/chats"}); !errors.Is(err, refused) {
		t.Fatalf("a refused list: %v", err)
	}
	chats.listErr = errors.New("the authorizer gave no decision")
	_, err = m.List(ctx, "/memory/chats")
	if u, ok := errors.AsType[*memory.Unreachable](err); !ok || u.Detail != "the folder's conversations could not be listed" {
		t.Fatalf("a failed list: %v", err)
	}
	if _, err := readAll(t, m, "/memory/chats/2026-10-28-ses_2.md"); !errors.As(err, new(*memory.TooLarge)) {
		t.Fatalf("a conversation past the bound: %v", err)
	}
	chats.readErr = context.DeadlineExceeded
	if _, err := readAll(t, m, "/memory/chats/2026-10-28-ses_2.md"); !errors.As(err, new(*memory.Unreachable)) {
		t.Fatalf("a failed read: %v", err)
	}
}

// TestAFolderOfConversationsBesideAStore: a session with a folder of a
// files store and a folder of conversations reaches each through its
// own way, and a store's folder still needs a token.
func TestAFolderOfConversationsBesideAStore(t *testing.T) {
	arca := arcastub.New(t)
	chats := &chatsStub{files: map[string]string{"2026-10-28-ses_2.md": "tide\n"}, version: map[string]string{"2026-10-28-ses_2.md": "v"}}
	both := append(folders(arca), chatsFolder)
	if _, err := New(both, nil, arca.Client(), nil, WithConversations(chats)); err == nil {
		t.Fatal("a folder of a files store with no token made a machine")
	}
	m, err := New(both, func(context.Context) (string, error) { return "session-token", nil }, arca.Client(), nil, WithConversations(chats))
	if err != nil {
		t.Fatal(err)
	}
	arca.Put(owner, "files/memory/you/name.md", arcastub.Object{Data: []byte("Ada, who likes the tide.\n")})
	grep, err := m.Search(t.Context(), machine.SearchRequest{Kind: machine.SearchGrep, Pattern: `tide`, Path: "/memory", OutputMode: machine.ModeFiles, HeadLimit: 50})
	slices.Sort(grep.Lines)
	if err != nil || !slices.Equal(grep.Lines, []string{"/memory/chats/2026-10-28-ses_2.md", "/memory/user/name.md"}) {
		t.Fatalf("a grep over /memory %+v, %v", grep, err)
	}
}
