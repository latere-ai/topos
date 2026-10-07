// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// tideTables is the app an allow attaches in these tests.
var tideTables = &session.ResourceApp{Slug: "tide-tables", Name: "Tide tables", URL: "https://tide-tables.apps.example.com"}

// TestACreateTakesTheAllowsAttachments: an allow's repositories follow
// the request's, each marked attached with its app, one whose URL the
// request names is dropped and the request's keeps its ref; its context
// is the session's; the question names the request's repositories alone;
// an allow that gives more repositories than a session holds, or one that
// breaks a member's rule, is authorizer_unavailable and creates nothing.
func TestACreateTakesTheAllowsAttachments(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	context := []session.ContextPart{{Title: "Project", Text: "Tide tables for the harbor club."}, {Title: "Memory", Text: "- ent_01: navy.\n"}}
	answer := &authorizer.WireLimits{
		Repositories: []authorizer.WireRepository{
			{URL: "https://git.example.com/acme/web.git"},
			{URL: "https://git.example.com/r/7f3c.git", App: tideTables},
		},
		Context: context,
	}
	l := f.level(func(authz.Request) *authorizer.WireLimits { return answer })
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","resources":[{"type":"repository","url":"https://git.example.com/acme/web.git","ref":"main"}]}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create: %d %s", a.status, a.body)
	}
	var s session.Session
	a.decode(t, &s)
	want := []session.Resource{
		{Type: session.ResourceRepository, URL: "https://git.example.com/acme/web.git", Ref: "main"},
		{Type: session.ResourceRepository, URL: "https://git.example.com/r/7f3c.git", App: tideTables, Attached: true},
	}
	if len(s.Resources) != 2 || s.Resources[0] != want[0] || s.Resources[1].URL != want[1].URL || *s.Resources[1].App != *tideTables || !s.Resources[1].Attached {
		t.Fatalf("resources %+v", s.Resources)
	}
	if !slices.Equal(s.Context, context) {
		t.Fatalf("context %+v", s.Context)
	}
	if h := f.header(s.ID); len(h.Resources) != 2 || !slices.Equal(h.Context, context) {
		t.Fatalf("stored %+v, %+v", h.Resources, h.Context)
	}
	if repos, _ := l.last(t, authorizer.ActionSessionCreate).Resource.Fields["repositories"].([]any); len(repos) != 1 {
		t.Fatalf("the question named %v", repos)
	}

	many := make([]authorizer.WireRepository, session.MaxRepositories)
	for i := range many {
		many[i] = authorizer.WireRepository{URL: fmt.Sprintf("https://git.example.com/r/%d.git", i)}
	}
	before := f.count()
	for name, w := range map[string]*authorizer.WireLimits{
		"one past the bound with the request's": {Repositories: many},
		"an app of no slug":                     {Repositories: []authorizer.WireRepository{{URL: "https://git.example.com/r/1.git", App: &session.ResourceApp{Name: "T", URL: tideTables.URL}}}},
		"a context with no text":                {Context: []session.ContextPart{{Title: "Project"}}},
	} {
		l.to(w)
		a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","resources":[{"type":"repository","url":"https://git.example.com/acme/web.git"}]}`)
		if a.status != http.StatusServiceUnavailable || a.code() != "authorizer_unavailable" {
			t.Fatalf("%s: %d %s", name, a.status, a.body)
		}
		if !strings.Contains(string(a.body), "repositories") && !strings.Contains(string(a.body), "context") {
			t.Fatalf("%s: the refusal names no member: %s", name, a.body)
		}
	}
	if f.count() != before {
		t.Fatalf("a refused create wrote a session: %d, had %d", f.count(), before)
	}
	// Exactly the bound, the request's included, is taken.
	l.to(&authorizer.WireLimits{Repositories: many[:session.MaxRepositories-1]})
	if a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","resources":[{"type":"repository","url":"https://git.example.com/acme/web.git"}]}`); a.status != http.StatusCreated {
		t.Fatalf("exactly %d repositories: %d %s", session.MaxRepositories, a.status, a.body)
	}
}

// TestARequestCannotAttach: a request that names a repository's app, or
// marks one attached, is invalid_request before any question.
func TestARequestCannotAttach(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	for name, body := range map[string]string{
		"an app":   `{"agent":"reviewer","resources":[{"type":"repository","url":"https://git.example.com/r/1.git","app":{"slug":"tide","name":"T","url":"https://tide.example"}}]}`,
		"attached": `{"agent":"reviewer","resources":[{"type":"repository","url":"https://git.example.com/r/1.git","attached":true}]}`,
		"a file":   `{"agent":"reviewer","resources":[{"type":"file","url":"https://storage.example.com/f"}]}`,
	} {
		f.authz.take()
		a := f.do(http.MethodPost, "/v1/sessions", "alice", body)
		if a.status != http.StatusBadRequest || a.code() != CodeInvalidRequest {
			t.Fatalf("%s: %d %s", name, a.status, a.body)
		}
		if asked := f.authz.take(); slices.Contains(asked, authorizer.ActionSessionCreate) {
			t.Fatalf("%s asked %v", name, asked)
		}
	}
}

// TestAForkIsAttachedByItsOwnAllow: a fork carries its parent's
// requested repositories and asks about them alone; the repositories and
// context its parent's allow attached it holds only as its own allow
// attaches them.
func TestAForkIsAttachedByItsOwnAllow(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	l := f.level(func(req authz.Request) *authorizer.WireLimits {
		switch req.Action {
		case authorizer.ActionSessionCreate:
			return &authorizer.WireLimits{
				Repositories: []authorizer.WireRepository{{URL: "https://git.example.com/r/7f3c.git", App: tideTables}},
				Context:      []session.ContextPart{{Title: "Project", Text: "The parent's."}},
			}
		case authorizer.ActionSessionFork:
			return &authorizer.WireLimits{Context: []session.ContextPart{{Title: "Project", Text: "The fork's."}}}
		}
		return nil
	})
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","resources":[{"type":"repository","url":"https://git.example.com/acme/web.git"}]}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create: %d %s", a.status, a.body)
	}
	var parent session.Session
	a.decode(t, &parent)
	f.turn(parent.ID, 1, "Done.", 1)
	fork := f.forked(parent.ID, "")
	if len(fork.Resources) != 1 || fork.Resources[0].URL != "https://git.example.com/acme/web.git" || fork.Resources[0].Attached {
		t.Fatalf("the fork's resources %+v", fork.Resources)
	}
	if len(fork.Context) != 1 || fork.Context[0].Text != "The fork's." {
		t.Fatalf("the fork's context %+v", fork.Context)
	}
	if repos, _ := l.last(t, authorizer.ActionSessionFork).Resource.Fields["repositories"].([]any); len(repos) != 1 {
		t.Fatalf("session.fork named %v", repos)
	}
	// A fork whose allow attaches the app again holds it again.
	l.to(&authorizer.WireLimits{Repositories: []authorizer.WireRepository{{URL: "https://git.example.com/r/7f3c.git", App: tideTables}}})
	again := f.forked(parent.ID, "")
	if len(again.Resources) != 2 || again.Resources[1].App == nil || !again.Resources[1].Attached || again.Context != nil {
		t.Fatalf("a fork attached again: %+v, %+v", again.Resources, again.Context)
	}
}
