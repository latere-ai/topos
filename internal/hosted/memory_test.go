// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// TestNoMemoryCredentialInMachine: a hosted session with memory folders
// saves and reads a memory with the session's own token for the files
// audience and the workload session, and creates no sandbox for it; the
// sandbox a command later opens is created, mounted and written with no
// token of the files store anywhere in it (spec 093).
func TestNoMemoryCredentialInMachine(t *testing.T) {
	arca := arcastub.New(t)
	arca.RequireToken(func(tok string) bool { return strings.HasPrefix(tok, "arca-session-") })
	c := newCloudOf(t, writerAgent, func(s *session.Session) {
		s.Memory = []session.MemoryFolder{{Name: "project", URL: arca.URL(), Owner: "o", Path: "files/memory/projects/p1", Access: session.MemoryReadWrite}}
	})
	c.o.FilesAudience = "arca"
	creds := &issued{life: 15 * time.Minute}
	c.creds = creds
	c.drive("Remember the club's color.",
		tool(t, "toolu_1", "write", map[string]any{"path": "/memory/project/color.md", "content": "The club's color is navy.\n"}),
		tool(t, "toolu_2", "read", map[string]any{"path": "/memory/project/color.md"}),
		said("Saved."),
	)
	if n := c.cella.Count(cellastub.OpCreate); n != 0 {
		t.Fatalf("a session that only remembers created %d sandboxes", n)
	}
	o, ok := arca.Object("o", "files/memory/projects/p1/color.md")
	if !ok || string(o.Data) != "The club's color is navy.\n" || !strings.HasPrefix(o.Token, "arca-session-") {
		t.Fatalf("the memory %+v %t", o, ok)
	}
	if !slices.Contains(creds.calls, "arca/"+runner.WorkloadSession) {
		t.Fatalf("no token for the files audience was asked: %v", creds.calls)
	}
	if texts := c.resultTexts(); len(texts) != 2 || !strings.Contains(texts[1], "navy") {
		t.Fatalf("the results %q", texts)
	}

	c.drive("Check the sandbox.", bash("toolu_3", "env"), said("Checked."))
	if out := c.lastResult(); strings.Contains(out, "arca-session") {
		t.Fatalf("the sandbox's environment holds a files store token: %q", out)
	}
	sb := c.created()
	raw, err := json.Marshal(sb)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "arca-session") {
		t.Fatalf("the sandbox's manifest carries a files store token: %s", raw)
	}
	for _, m := range sb.Spec.Secrets {
		if _, value, _ := c.cella.Secret(m.Name); strings.Contains(value, "arca-session") {
			t.Fatalf("the Secret %s carries a files store token", m.Name)
		}
	}
	for _, r := range c.cella.Requests() {
		for _, vs := range r.Header {
			if slices.ContainsFunc(vs, func(v string) bool { return strings.Contains(v, "arca-session") }) {
				t.Fatalf("the %s request carries a files store token", r.Op)
			}
		}
	}
	err = filepath.WalkDir(c.cella.Workspace(cella.SandboxName(c.s.ID)), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err == nil && strings.Contains(string(b), "arca-session") {
			t.Errorf("%s in the sandbox holds a files store token", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTheMemoryMachineOfASession: a session with no memory folders, and a
// rollout's run, get no memory machine; a session with folders on a
// runner with no files audience or no token source, or with folders that
// break a rule, is files_unavailable (spec 093).
func TestTheMemoryMachineOfASession(t *testing.T) {
	ctx := runner.WithTokens(t.Context(), runner.NewTokenSource(&issued{life: time.Minute}, nil, nil))
	b := builder{o: Options{FilesAudience: "arca"}}
	folders := []session.MemoryFolder{{Name: "user", URL: "https://files.example.com/v1", Owner: "o", Path: "files/memory/you", Access: session.MemoryReadOnly}}
	s := session.Session{ID: session.NewID(session.PrefixSession), Memory: folders}
	if m, err := b.memory(ctx, s, nil); err != nil || m == nil || m.Info().Kind != "memory" {
		t.Fatalf("a session with folders: %v %v", m, err)
	}
	if m, err := b.memory(ctx, session.Session{}, nil); err != nil || m != nil {
		t.Fatalf("a session with no folders: %v %v", m, err)
	}
	run := s
	run.Rollout = "rol_1"
	if m, err := b.memory(ctx, run, nil); err != nil || m != nil {
		t.Fatalf("a rollout's run: %v %v", m, err)
	}
	bad := s
	bad.Memory = []session.MemoryFolder{{Name: "User", URL: "https://files.example.com/v1", Owner: "o", Path: "p", Access: session.MemoryReadOnly}}
	for name, c := range map[string]struct {
		b   builder
		ctx bool
		s   session.Session
	}{
		"no audience":     {builder{}, true, s},
		"no token source": {b, false, s},
		"a bad folder":    {b, true, bad},
	} {
		use := ctx
		if !c.ctx {
			use = t.Context()
		}
		_, err := c.b.memory(use, c.s, nil)
		if se, ok := errors.AsType[*runner.SetupError](err); !ok || se.Code != CodeFilesUnavailable {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// oneConversation is a reach to a folder of conversations that holds one
// file.
type oneConversation struct{}

func (oneConversation) ListConversations(context.Context, string) ([]memory.ConversationFile, bool, error) {
	return []memory.ConversationFile{{Name: "2026-10-28-ses_2.md", Version: "v"}}, false, nil
}

func (oneConversation) ReadConversation(_ context.Context, _, name string) (memory.ConversationFile, []byte, error) {
	return memory.ConversationFile{Name: name, Version: "v", Size: 6}, []byte("tides\n"), nil
}

// TestAFolderOfConversationsNeedsNoFilesAudience: a session whose only
// memory folder is a folder of conversations gets a memory machine on a
// runner with no files audience and no token source, reaching the folder
// through the drive's reach to the server; with no such reach, or beside
// a folder of a files store it cannot reach, it is files_unavailable
// (spec 094).
func TestAFolderOfConversationsNeedsNoFilesAudience(t *testing.T) {
	chats := session.MemoryFolder{Name: "chats", Access: session.MemoryReadOnly, Sessions: &session.MemorySessions{}}
	s := session.Session{ID: session.NewID(session.PrefixSession), Memory: []session.MemoryFolder{chats}}
	ctx := runner.WithConversations(t.Context(), oneConversation{})
	m, err := builder{}.memory(ctx, s, nil)
	if err != nil || m == nil {
		t.Fatalf("a session with a folder of conversations: %v %v", m, err)
	}
	entries, err := m.List(ctx, "/memory/chats")
	if err != nil || len(entries) != 1 || entries[0].Path != "/memory/chats/2026-10-28-ses_2.md" {
		t.Fatalf("the folder %+v, %v", entries, err)
	}
	both := s
	both.Memory = []session.MemoryFolder{chats, {Name: "user", URL: "https://files.example.com/v1", Owner: "o", Path: "files/memory/you", Access: session.MemoryReadOnly}}
	for name, c := range map[string]struct {
		ctx context.Context
		s   session.Session
	}{
		"no reach to the server":       {t.Context(), s},
		"a store folder and no tokens": {ctx, both},
	} {
		_, err := builder{}.memory(c.ctx, c.s, nil)
		if se, ok := errors.AsType[*runner.SetupError](err); !ok || se.Code != CodeFilesUnavailable {
			t.Errorf("%s: %v", name, err)
		}
	}
}
