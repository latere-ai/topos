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
		Account:    "ada", APIURL: "https://api.git.example.com/", APIExample: "/v3/user",
	}
}

func mcpConnection() Connection {
	return Connection{
		Name: "tracker", Kind: ConnectionMCP, Title: "Issue tracker", Audience: "connections",
		Address: "https://connections.example.com/v1/connections/c_2/mcp", ReadOnly: []string{"list_*", "get_issue"},
	}
}

// TestCheckConnections: each rule of spec 069's member, its bounds, and
// the copy it answers, a git URL and an API URL without their trailing
// "/".
func TestCheckConnections(t *testing.T) {
	in := []Connection{httpConnection(), mcpConnection()}
	got, err := CheckConnections(in)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].GitURL != "https://git.example.com" || in[0].GitURL != "https://git.example.com/" {
		t.Fatalf("the git URL is %q, the input's %q", got[0].GitURL, in[0].GitURL)
	}
	if got[0].APIURL != "https://api.git.example.com" || in[0].APIURL != "https://api.git.example.com/" || got[0].Account != "ada" {
		t.Fatalf("the API URL is %q, the input's %q, the account %q", got[0].APIURL, in[0].APIURL, got[0].Account)
	}
	withAccount := mcpConnection()
	withAccount.Account = strings.Repeat("é", MaxConnectionAccount)
	if _, err := CheckConnections([]Connection{withAccount}); err != nil {
		t.Fatalf("an mcp connection with an account at its bound: %v", err)
	}
	// Two services of one provider share an API base URL.
	sibling := httpConnection()
	sibling.Name, sibling.GitAddress, sibling.GitURL = "drive", "", ""
	if _, err := CheckConnections([]Connection{httpConnection(), sibling}); err != nil {
		t.Fatalf("two connections of one API URL: %v", err)
	}
	got[1].ReadOnly[0] = "changed"
	if in[1].ReadOnly[0] != "list_*" {
		t.Fatal("the answer shares the input's read_only")
	}
	hiding := mcpConnection()
	hiding.Hidden = []string{"delete_*", strings.Repeat("a", MaxReadOnlyGlob)}
	kept, err := CheckConnections([]Connection{hiding})
	if err != nil {
		t.Fatalf("an mcp connection with hidden globs: %v", err)
	}
	kept[0].Hidden[0] = "changed"
	if hiding.Hidden[0] != "delete_*" {
		t.Fatal("the answer shares the input's hidden")
	}
	hiding.Hidden = slices.Repeat([]string{"a"}, MaxReadOnlyTools)
	if _, err := CheckConnections([]Connection{hiding}); err != nil {
		t.Fatalf("%d hidden globs: %v", MaxReadOnlyTools, err)
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
		"an empty hidden glob":         func(c *Connection) { c.Hidden = []string{""} },
		"a broken hidden glob":         func(c *Connection) { c.Hidden = []string{"delete_["} },
		"a long hidden glob":           func(c *Connection) { c.Hidden = []string{strings.Repeat("a", MaxReadOnlyGlob+1)} },
		"too many hidden globs":        func(c *Connection) { c.Hidden = slices.Repeat([]string{"a"}, MaxReadOnlyTools+1) },
		"an mcp api_url":               func(c *Connection) { c.APIURL = "https://api.example.com" },
		"an mcp api_example":           func(c *Connection) { c.APIExample = "/v1/issues" },
		"an mcp api_notes":             func(c *Connection) { c.APINotes = "Issues are numbered." },
		"an account of two lines":      func(c *Connection) { c.Account = "ada\nlovelace" },
		"an account past its bound":    func(c *Connection) { c.Account = strings.Repeat("é", MaxConnectionAccount+1) },
	} {
		c := mcpConnection()
		change(&c)
		if _, err := CheckConnections([]Connection{c}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for name, change := range map[string]func(*Connection){
		"an http read_only":        func(c *Connection) { c.ReadOnly = []string{"*"} },
		"an http hidden":           func(c *Connection) { c.Hidden = []string{"*"} },
		"a git address alone":      func(c *Connection) { c.GitURL = "" },
		"a git URL alone":          func(c *Connection) { c.GitAddress = "" },
		"a plain http git address": func(c *Connection) { c.GitAddress = "http://connections.example.com/git" },
		"a git URL with a query":   func(c *Connection) { c.GitURL = "https://git.example.com/?x" },
		"a plain http api_url":     func(c *Connection) { c.APIURL = "http://api.git.example.com" },
		"an api_url with a query":  func(c *Connection) { c.APIURL = "https://api.git.example.com/?x" },
		"a relative api_url":       func(c *Connection) { c.APIURL = "/v1" },
		"an account with a tab":    func(c *Connection) { c.Account = "ada\tlovelace" },
		"a relative api_example":   func(c *Connection) { c.APIExample = "v3/user" },
		"an api_example query":     func(c *Connection) { c.APIExample = "/v3/user?x=1" },
		"an api_example fragment":  func(c *Connection) { c.APIExample = "/v3/user#x" },
		"an api_example space":     func(c *Connection) { c.APIExample = "/v3/my user" },
		"an api_example $":         func(c *Connection) { c.APIExample = "/v3/$(id)" },
		"an api_example quote":     func(c *Connection) { c.APIExample = `/v3/"user` },
		"an api_example backtick":  func(c *Connection) { c.APIExample = "/v3/`id`" },
		"an api_example backslash": func(c *Connection) { c.APIExample = `/v3/\user` },
		"an api_example newline":   func(c *Connection) { c.APIExample = "/v3/\nuser" },
		"api_notes of two lines":   func(c *Connection) { c.APINotes = "Read a page.\nThen write." },
		"api_notes past its bound": func(c *Connection) { c.APINotes = strings.Repeat("é", MaxConnectionAPINotes+1) },
		"an api_example past bound": func(c *Connection) {
			c.APIExample = "/" + strings.Repeat("a", MaxConnectionAPIExample)
		},
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
	for name, change := range map[string]func(*Connection){
		"account":     func(c *Connection) { c.Account = "grace" },
		"api_url":     func(c *Connection) { c.APIURL = "https://api.other.example.com" },
		"api_example": func(c *Connection) { c.APIExample = "/v3/repos" },
	} {
		e := httpConnection()
		change(&e)
		if SameConnections([]Connection{httpConnection()}, []Connection{e}) {
			t.Errorf("a changed %s is the same list", name)
		}
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
		"a hidden":        func(c *Connection) { c.Hidden = []string{"delete_*"} },
		"an account":      func(c *Connection) { c.Account = "ada" },
		"an api_url":      func(c *Connection) { c.APIURL = "https://api.git.example.com" },
		"an api_example":  func(c *Connection) { c.APIExample = "/v3/user" },
		"an api_notes":    func(c *Connection) { c.APINotes = "Pages read as text." },
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
	notes := httpConnection()
	notes.APINotes = "Read a page's text with GET ${env}/v3/pages/<id>/text?format=plain, and upload to ${{env}%/v3}/upload/v3/pages."
	if _, err := CheckConnections([]Connection{notes}); err != nil {
		t.Fatalf("an http connection's api_notes was refused: %v", err)
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

// TestSameConnectionsReadsTheNotes: two lists that differ only in an http
// connection's api_notes are not the same, so a send that changes the
// notes appends session.connections_changed (spec 069).
func TestSameConnectionsReadsTheNotes(t *testing.T) {
	a, b := httpConnection(), httpConnection()
	b.APINotes = "Read a page's text with GET ${env}/v3/pages/<id>/text."
	if SameConnections([]Connection{a}, []Connection{b}) {
		t.Fatal("lists that differ in api_notes are the same")
	}
	if !SameConnections([]Connection{b}, []Connection{b}) {
		t.Fatal("a list is not the same as itself")
	}
}

// TestAConnectionHidesTools: an mcp connection's hidden globs match a
// tool's whole name with path.Match, as its read_only globs do, and two
// lists that differ only in a connection's hidden are not the same, so a
// send that changes them appends session.connections_changed (spec 069).
func TestAConnectionHidesTools(t *testing.T) {
	c := mcpConnection()
	c.Hidden = []string{"delete_*", "admin"}
	for tool, want := range map[string]bool{"delete_issue": true, "admin": true, "admin_users": false, "list_issues": false, "undelete_issue": false} {
		if c.Hides(tool) != want {
			t.Errorf("%s hidden: %v", tool, !want)
		}
	}
	if mcpConnection().Hides("delete_issue") {
		t.Fatal("a connection with no hidden globs hides a tool")
	}
	if SameConnections([]Connection{mcpConnection()}, []Connection{c}) {
		t.Fatal("lists that differ in hidden are the same")
	}
	if !SameConnections([]Connection{c}, []Connection{c}) {
		t.Fatal("a list is not the same as itself")
	}
}
