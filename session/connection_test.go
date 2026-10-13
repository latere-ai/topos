// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func httpConnection() Connection {
	return Connection{
		Name: "code-host", Kind: ConnectionHTTP, Title: "Code host", Audience: "connections",
		Address:    "https://connections.example.com/v1/connections/c_1/http",
		GitAddress: "https://connections.example.com/v1/connections/c_1/git",
		GitURL:     "https://git.example.com/",
	}
}

func mcpConnection() Connection {
	return Connection{
		Name: "tracker", Kind: ConnectionMCP, Title: "Issue tracker", Audience: "connections",
		Address: "https://connections.example.com/v1/connections/c_2/mcp", ReadOnly: []string{"list_*", "get_issue"},
	}
}

// TestCheckConnections: each rule of spec 069's member, its bounds, and
// the copy it answers, a git URL without its trailing "/".
func TestCheckConnections(t *testing.T) {
	in := []Connection{httpConnection(), mcpConnection()}
	got, err := CheckConnections(in)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].GitURL != "https://git.example.com" || in[0].GitURL != "https://git.example.com/" {
		t.Fatalf("the git URL is %q, the input's %q", got[0].GitURL, in[0].GitURL)
	}
	got[1].ReadOnly[0] = "changed"
	if in[1].ReadOnly[0] != "list_*" {
		t.Fatal("the answer shares the input's read_only")
	}
	if out, err := CheckConnections(nil); err != nil || out != nil {
		t.Fatalf("no connections: %v %v", out, err)
	}
	plain := httpConnection()
	plain.GitAddress, plain.GitURL = "", ""
	if _, err := CheckConnections([]Connection{plain}); err != nil {
		t.Fatalf("an http connection with no git: %v", err)
	}
	many := make([]Connection, MaxConnections+1)
	for i := range many {
		many[i] = mcpConnection()
		many[i].Name = fmt.Sprintf("c%d", i)
	}
	if _, err := CheckConnections(many[:MaxConnections]); err != nil {
		t.Fatalf("%d connections: %v", MaxConnections, err)
	}
	if _, err := CheckConnections(many); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("%d connections: %v", len(many), err)
	}
	for name, change := range map[string]func(*Connection){
		"an empty name":                func(c *Connection) { c.Name = "" },
		"a name with a capital":        func(c *Connection) { c.Name = "Tracker" },
		"a name from a digit":          func(c *Connection) { c.Name = "1tracker" },
		"a name ending in -":           func(c *Connection) { c.Name = "tracker-" },
		"a name with _":                func(c *Connection) { c.Name = "issue_tracker" },
		"a name past its bound":        func(c *Connection) { c.Name = "t" + strings.Repeat("r", MaxConnectionName) },
		"an unknown kind":              func(c *Connection) { c.Kind = "ssh" },
		"an empty title":               func(c *Connection) { c.Title = "" },
		"a title of two lines":         func(c *Connection) { c.Title = "Issue\ntracker" },
		"a title past its bound":       func(c *Connection) { c.Title = strings.Repeat("é", MaxConnectionTitle+1) },
		"an empty audience":            func(c *Connection) { c.Audience = "" },
		"an audience with a space":     func(c *Connection) { c.Audience = "a b" },
		"an audience past bound":       func(c *Connection) { c.Audience = strings.Repeat("a", MaxConnectionAudience+1) },
		"a plain http address":         func(c *Connection) { c.Address = "http://connections.example.com/mcp" },
		"a relative address":           func(c *Connection) { c.Address = "/v1/connections/c_2/mcp" },
		"an address with a user":       func(c *Connection) { c.Address = "https://u:p@connections.example.com/mcp" },
		"an address with a query":      func(c *Connection) { c.Address = "https://connections.example.com/mcp?x=1" },
		"an address with a ?":          func(c *Connection) { c.Address = "https://connections.example.com/mcp?" },
		"an address with a #":          func(c *Connection) { c.Address = "https://connections.example.com/mcp#x" },
		"an address that is not a URL": func(c *Connection) { c.Address = "https://connections.example.com/%zz" },
		"an mcp git address":           func(c *Connection) { c.GitAddress = "https://connections.example.com/git" },
		"an empty read_only glob":      func(c *Connection) { c.ReadOnly = []string{""} },
		"a broken read_only glob":      func(c *Connection) { c.ReadOnly = []string{"list_["} },
		"a long read_only glob":        func(c *Connection) { c.ReadOnly = []string{strings.Repeat("a", MaxReadOnlyGlob+1)} },
		"too many read_only globs":     func(c *Connection) { c.ReadOnly = slices.Repeat([]string{"a"}, MaxReadOnlyTools+1) },
	} {
		c := mcpConnection()
		change(&c)
		if _, err := CheckConnections([]Connection{c}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for name, change := range map[string]func(*Connection){
		"an http read_only":        func(c *Connection) { c.ReadOnly = []string{"*"} },
		"a git address alone":      func(c *Connection) { c.GitURL = "" },
		"a git URL alone":          func(c *Connection) { c.GitAddress = "" },
		"a plain http git address": func(c *Connection) { c.GitAddress = "http://connections.example.com/git" },
		"a git URL with a query":   func(c *Connection) { c.GitURL = "https://git.example.com/?x" },
	} {
		c := httpConnection()
		change(&c)
		if _, err := CheckConnections([]Connection{c}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	twice := mcpConnection()
	if _, err := CheckConnections([]Connection{mcpConnection(), twice}); err == nil || !strings.Contains(err.Error(), "second time") {
		t.Fatalf("a name twice: %v", err)
	}
	other := httpConnection()
	other.Name = "mirror"
	other.GitURL = "https://git.example.com"
	if _, err := CheckConnections([]Connection{httpConnection(), other}); err == nil || !strings.Contains(err.Error(), "git URL") {
		t.Fatalf("a git URL twice: %v", err)
	}
}

// TestAConnectionsVariableAndTools: an http connection's variable is its
// name upper-cased with "_" for "-", and an mcp connection's read_only
// globs say which of its tools read.
func TestAConnectionsVariableAndTools(t *testing.T) {
	if got := httpConnection().APIEnv(); got != "CODE_HOST_API_URL" {
		t.Fatalf("the variable is %q", got)
	}
	c := mcpConnection()
	for tool, want := range map[string]bool{"list_issues": true, "get_issue": true, "get_issues": false, "create_issue": false} {
		if c.ReadsOnly(tool) != want {
			t.Errorf("%s reads only: %v", tool, !want)
		}
	}
	if got := HTTPConnections([]Connection{mcpConnection(), httpConnection()}); len(got) != 1 || got[0].Name != "code-host" {
		t.Fatalf("the http connections are %+v", got)
	}
	if !SameConnections(nil, []Connection{}) || SameConnections([]Connection{c}, nil) {
		t.Fatal("an empty list is not the same as none, or one is")
	}
	d := mcpConnection()
	d.ReadOnly = []string{"list_*"}
	if SameConnections([]Connection{c}, []Connection{d}) || !SameConnections([]Connection{c}, []Connection{mcpConnection()}) {
		t.Fatal("a changed read_only is the same list, or an equal one is not")
	}
}

// TestTheHeaderTakesTheConnections: session.connections_changed replaces
// the header's connections, an empty list included; a redacted one and a
// fork's copied one do not.
func TestTheHeaderTakesTheConnections(t *testing.T) {
	change := func(seq uint64, cs ...Connection) Event {
		t.Helper()
		if cs == nil {
			cs = []Connection{}
		}
		e, err := NewEvent(TypeConnectionsChanged, ConnectionsChanged{Connections: cs}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		e.Seq = seq
		return e
	}
	if e := change(1); string(e.Payload) != `{"connections":[]}` {
		t.Fatalf("an empty change is %s", e.Payload)
	}
	s := Session{LastSeq: 1}
	ApplyBatch(&s, []Event{change(2, httpConnection())})
	if len(s.Connections) != 1 || s.Connections[0].Name != "code-host" {
		t.Fatalf("the header holds %+v", s.Connections)
	}
	redacted := change(3)
	redacted.Payload = slices.Clone(tombstone)
	ApplyBatch(&s, []Event{redacted})
	if len(s.Connections) != 1 {
		t.Fatal("a redacted change moved the connections")
	}
	ApplyBatch(&s, []Event{change(4)})
	if len(s.Connections) != 0 {
		t.Fatalf("an empty change left %+v", s.Connections)
	}
	fork := Session{Connections: []Connection{mcpConnection()}, Parent: &Parent{SessionID: "ses_parent", Seq: 4}}
	ApplyBatch(&fork, []Event{change(2, httpConnection())})
	if len(fork.Connections) != 1 || fork.Connections[0].Name != "tracker" {
		t.Fatalf("a fork's copied change moved its connections to %+v", fork.Connections)
	}
	ApplyBatch(&fork, []Event{change(5, httpConnection())})
	if len(fork.Connections) != 1 || fork.Connections[0].Name != "code-host" {
		t.Fatalf("a fork's own change left %+v", fork.Connections)
	}
	if Redactable(TypeConnectionsChanged) || !Known[TypeConnectionsChanged] {
		t.Fatal("session.connections_changed is redactable or unknown")
	}
}

// TestAnAvailableConnection: a connection of state available, one the
// installation offers and the person has not connected, takes a name, a
// kind and a title and nothing a connected one is reached by; any other
// state, and a connected one without its address, do not decode; the
// bound counts both; and a session reaches no available connection.
func TestAnAvailableConnection(t *testing.T) {
	gh := Connection{Name: "github", Kind: ConnectionHTTP, Title: "GitHub", State: ConnectionAvailable}
	tr := Connection{Name: "wiki", Kind: ConnectionMCP, Title: "Wiki", State: ConnectionAvailable}
	got, err := CheckConnections([]Connection{httpConnection(), gh, tr})
	if err != nil {
		t.Fatal(err)
	}
	if got[1].Connected() || !got[0].Connected() {
		t.Fatalf("connected: %v, %v", got[0].Connected(), got[1].Connected())
	}
	if h := HTTPConnections(got); len(h) != 1 || h[0].Name != "code-host" {
		t.Fatalf("the http connections are %+v", h)
	}
	for name, change := range map[string]func(*Connection){
		"an audience":     func(c *Connection) { c.Audience = "connections" },
		"an address":      func(c *Connection) { c.Address = "https://connections.example.com/v1/connections/c_1/http" },
		"a git address":   func(c *Connection) { c.GitAddress = "https://connections.example.com/v1/connections/c_1/git" },
		"a git URL":       func(c *Connection) { c.GitURL = "https://git.example.com" },
		"a read_only":     func(c *Connection) { c.ReadOnly = []string{"list_*"} },
		"another state":   func(c *Connection) { c.State = "connected" },
		"an empty title":  func(c *Connection) { c.Title = "" },
		"an unknown kind": func(c *Connection) { c.Kind = "ssh" },
	} {
		c := gh
		change(&c)
		if _, err := CheckConnections([]Connection{c}); err == nil {
			t.Errorf("an available connection with %s was accepted", name)
		}
	}
	bare := httpConnection()
	bare.Address = ""
	if _, err := CheckConnections([]Connection{bare}); err == nil {
		t.Fatal("a connected connection without an address was accepted")
	}
	if _, err := CheckConnections([]Connection{httpConnection(), {Name: "code-host", Kind: ConnectionMCP, Title: "Code host", State: ConnectionAvailable}}); err == nil || !strings.Contains(err.Error(), "second time") {
		t.Fatalf("a name connected and available: %v", err)
	}
	many := make([]Connection, MaxConnections+1)
	for i := range many {
		many[i] = tr
		many[i].Name = fmt.Sprintf("c%d", i)
	}
	many[0] = mcpConnection()
	if _, err := CheckConnections(many); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("%d connections, connected and available: %v", len(many), err)
	}
	if SameConnections([]Connection{gh}, []Connection{{Name: "github", Kind: ConnectionHTTP, Title: "GitHub"}}) {
		t.Fatal("an available connection is the same as a connected one")
	}
	b, err := Marshal(gh)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"name":"github","kind":"http","title":"GitHub","state":"available"}` {
		t.Fatalf("an available connection is %s", b)
	}
}
