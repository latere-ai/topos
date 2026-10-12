// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// githubConnection and trackerConnection are an http and an mcp
// connection of the connection service at connections.example.com.
var (
	githubConnection = session.Connection{
		Name: "github", Kind: session.ConnectionHTTP, Title: "GitHub", Audience: "connections",
		Address:    "https://connections.example.com/v1/connections/c_1/http",
		GitAddress: "https://connections.example.com/v1/connections/c_1/git", GitURL: "https://git.example.com",
	}
	trackerConnection = session.Connection{
		Name: "tracker", Kind: session.ConnectionMCP, Title: "Issue tracker", Audience: "tracker-core",
		Address: "https://tracker.example.com/mcp", ReadOnly: []string{"list_*"},
	}
)

// withConnections names the connection service of connections.example.com.
func withConnections(o *Options) {
	o.ConnectionsURL, o.ConnectionsAudience = "https://connections.example.com/v1", "connections"
}

// TestConnectionsAreTheAllows: a create holds its allow's connections, a
// fork its own allow's and never its parent's, and a send's allow
// replaces them with session.connections_changed before what it sent,
// none when it names none; a send whose allow names the list the session
// holds appends no change. An http connection of another audience or
// host, or on a server with no connection service, refuses the create
// authorizer_unavailable and writes nothing (spec 069).
func TestConnectionsAreTheAllows(t *testing.T) {
	f := newFixture(t, withConnections)
	f.apply("alice", "reviewer", "Review.")
	var answer *authorizer.WireLimits
	l := f.level(func(req authz.Request) *authorizer.WireLimits {
		if req.Action == authorizer.ActionSessionFork {
			return &authorizer.WireLimits{Connections: []session.Connection{trackerConnection}}
		}
		return answer
	})
	answer = &authorizer.WireLimits{Connections: []session.Connection{githubConnection, trackerConnection}}
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create: %d %s", a.status, a.body)
	}
	var s session.Session
	a.decode(t, &s)
	if !session.SameConnections(s.Connections, answer.Connections) || !session.SameConnections(f.header(s.ID).Connections, answer.Connections) {
		t.Fatalf("the session holds %+v", s.Connections)
	}

	changes := func(id string) []session.ConnectionsChanged {
		t.Helper()
		var out []session.ConnectionsChanged
		evs := f.log(id)
		for i, e := range evs {
			if e.Type != session.TypeConnectionsChanged {
				continue
			}
			if i+1 >= len(evs) || evs[i+1].Type != session.TypeUserMessage {
				t.Fatalf("session.connections_changed is not followed by the message it came with")
			}
			var p session.ConnectionsChanged
			if err := e.Decode(&p); err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
		return out
	}
	if a := f.send(s.ID, "Again."); a.status != http.StatusAccepted && a.status != http.StatusOK && a.status != http.StatusCreated {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if got := changes(s.ID); len(got) != 0 {
		t.Fatalf("a send of the same list changed it: %+v", got)
	}
	answer = &authorizer.WireLimits{Connections: []session.Connection{trackerConnection}}
	f.send(s.ID, "Without GitHub.")
	if got := changes(s.ID); len(got) != 1 || !session.SameConnections(got[0].Connections, answer.Connections) {
		t.Fatalf("the change %+v", got)
	}
	if h := f.header(s.ID); !session.SameConnections(h.Connections, answer.Connections) {
		t.Fatalf("the header after the change %+v", h.Connections)
	}
	answer = nil
	f.send(s.ID, "With none.")
	if got := changes(s.ID); len(got) != 2 || got[1].Connections == nil || len(got[1].Connections) != 0 {
		t.Fatalf("an allow with none: %+v", got)
	}
	if h := f.header(s.ID); len(h.Connections) != 0 {
		t.Fatalf("the header after an allow with none %+v", h.Connections)
	}
	if req := l.last(t, authorizer.ActionSessionSend); req.Action == "" {
		t.Fatal("no send was asked")
	}

	f.turn(s.ID, 1, "Done.", 1)
	fork := f.forked(s.ID, "")
	if !session.SameConnections(fork.Connections, []session.Connection{trackerConnection}) {
		t.Fatalf("the fork holds %+v", fork.Connections)
	}

	before := f.count()
	other := githubConnection
	other.Audience = "elsewhere"
	moved := githubConnection
	moved.GitAddress = "https://git-proxy.example.com/c_1/git"
	for name, c := range map[string]session.Connection{"another audience": other, "a git address on another host": moved} {
		answer = &authorizer.WireLimits{Connections: []session.Connection{c}}
		a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`)
		if a.status != http.StatusServiceUnavailable || a.code() != "authorizer_unavailable" || !strings.Contains(string(a.body), "limits.connections[0] github") {
			t.Errorf("%s: %d %s", name, a.status, a.body)
		}
	}
	if f.count() != before {
		t.Fatalf("a refused create wrote a session: %d, had %d", f.count(), before)
	}
}

// TestAnHTTPConnectionNeedsAConnectionService: a server with no
// connection service refuses an allow's http connection, at a create and
// at a send, and takes an mcp one.
func TestAnHTTPConnectionNeedsAConnectionService(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	answer := &authorizer.WireLimits{Connections: []session.Connection{trackerConnection}}
	f.level(func(authz.Request) *authorizer.WireLimits { return answer })
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`)
	if a.status != http.StatusCreated {
		t.Fatalf("an mcp connection alone: %d %s", a.status, a.body)
	}
	var s session.Session
	a.decode(t, &s)
	answer = &authorizer.WireLimits{Connections: []session.Connection{githubConnection}}
	if a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`); a.code() != "authorizer_unavailable" || !strings.Contains(string(a.body), "TOPOS_CONNECTIONS_URL") {
		t.Fatalf("an http connection at a create: %d %s", a.status, a.body)
	}
	if a := f.send(s.ID, "Clone it."); a.code() != "authorizer_unavailable" {
		t.Fatalf("an http connection at a send: %d %s", a.status, a.body)
	}
	if h := f.header(s.ID); !session.SameConnections(h.Connections, []session.Connection{trackerConnection}) {
		t.Fatalf("a refused send changed the connections to %+v", h.Connections)
	}
}
