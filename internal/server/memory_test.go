// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/session"
)

// memoryFolder is a memory folder of the files store at
// files.example.com.
func memoryFolder(name, path, access string) session.MemoryFolder {
	return session.MemoryFolder{Name: name, URL: "https://files.example.com/v1", Owner: "https://issuer.example|alice", Path: path, Access: access}
}

// withFilesStore keeps a files store, whose folders these tests never
// open.
func withFilesStore(o *Options) {
	none := func(context.Context, session.Session) (harness.Files, error) { return nil, nil }
	o.Files = &Files{Write: none, Read: none}
}

// TestACreateKeepsTheMemoryFolders: a create keeps its allow's memory
// folders in its header and answers them by name and access alone; a
// fork keeps its own allow's and never its parent's; a rollout's run
// keeps none; and an allow that names memory on a server with no files
// store, or memory that does not decode, refuses the create
// authorizer_unavailable naming the member and writes nothing (spec 093).
func TestACreateKeepsTheMemoryFolders(t *testing.T) {
	f := newFixture(t, withFilesStore)
	f.apply("alice", "reviewer", "Review.")
	user := memoryFolder("user", "files/memory/you", session.MemoryReadOnly)
	project := memoryFolder("project", "files/memory/projects/p1", session.MemoryReadWrite)
	other := memoryFolder("project", "files/memory/projects/p2", session.MemoryReadWrite)
	answer := &authorizer.WireLimits{Memory: []session.MemoryFolder{user, project}}
	f.level(func(req authz.Request) *authorizer.WireLimits {
		if req.Action == authorizer.ActionSessionFork {
			return &authorizer.WireLimits{Memory: []session.MemoryFolder{other}}
		}
		return answer
	})
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create: %d %s", a.status, a.body)
	}
	var s session.Session
	a.decode(t, &s)
	if !slices.Equal(s.Memory, []session.MemoryFolder{{Name: "user", Access: session.MemoryReadOnly}, {Name: "project", Access: session.MemoryReadWrite}}) {
		t.Fatalf("the session answers %+v", s.Memory)
	}
	if strings.Contains(string(a.body), "files.example.com") || strings.Contains(string(a.body), "files/memory") {
		t.Fatalf("the answer names the store: %s", a.body)
	}
	if h := f.header(s.ID); !slices.Equal(h.Memory, answer.Memory) {
		t.Fatalf("the header holds %+v", h.Memory)
	}
	if a := f.do(http.MethodGet, "/v1/sessions/"+s.ID, "alice", ""); a.status != http.StatusOK || !strings.Contains(string(a.body), `"memory":[{"name":"user","access":"read_only"},{"name":"project","access":"read_write"}]`) {
		t.Fatalf("get: %d %s", a.status, a.body)
	}

	f.turn(s.ID, 1, "Done.", 1)
	fork := f.forked(s.ID, "")
	if h := f.header(fork.ID); !slices.Equal(h.Memory, []session.MemoryFolder{other}) {
		t.Fatalf("the fork holds %+v", h.Memory)
	}

	a = f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","rollout":"cel_01.smoke"}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create of a run: %d %s", a.status, a.body)
	}
	var run session.Session
	a.decode(t, &run)
	if h := f.header(run.ID); len(h.Memory) != 0 || len(run.Memory) != 0 {
		t.Fatalf("a rollout's run holds %+v", h.Memory)
	}

	before := f.count()
	broken := project
	broken.Access = "write"
	for name, c := range map[string]struct {
		memory []session.MemoryFolder
		want   string
	}{
		"an access it cannot take": {[]session.MemoryFolder{broken}, "limits.memory[0]"},
		"a name twice":             {[]session.MemoryFolder{project, project}, "limits.memory[1]"},
	} {
		answer = &authorizer.WireLimits{Memory: c.memory}
		a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`)
		if a.status != http.StatusServiceUnavailable || a.code() != "authorizer_unavailable" || !strings.Contains(string(a.body), c.want) {
			t.Errorf("%s: %d %s", name, a.status, a.body)
		}
	}
	answer = &authorizer.WireLimits{Memory: []session.MemoryFolder{user}}
	f.api.o.Files = nil
	a = f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`)
	if a.status != http.StatusServiceUnavailable || a.code() != "authorizer_unavailable" || !strings.Contains(string(a.body), "limits.memory") || !strings.Contains(string(a.body), "TOPOS_FILES_AUDIENCE") {
		t.Fatalf("memory on a server with no files store: %d %s", a.status, a.body)
	}
	if f.count() != before {
		t.Fatalf("a refused create wrote a session: %d, had %d", f.count(), before)
	}
	answer = nil
	if s := f.create("alice", "reviewer"); len(s.Memory) != 0 {
		t.Fatalf("an allow with no memory: %+v", s.Memory)
	}
}
