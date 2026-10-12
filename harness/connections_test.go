// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// conMachine is a Cella machine that takes a session's connections.
type conMachine struct {
	fakeMachine
	mu      sync.Mutex
	applied [][]machine.HTTPConnection
	refuse  error
}

func (m *conMachine) ApplyConnections(_ context.Context, cs []machine.HTTPConnection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refuse != nil {
		return m.refuse
	}
	m.applied = append(m.applied, cs)
	return nil
}

var (
	codeHost = session.Connection{Name: "code-host", Kind: session.ConnectionHTTP, Title: "Code host", Audience: "connections",
		Address:    "https://connections.example.com/v1/connections/c_1/http",
		GitAddress: "https://connections.example.com/v1/connections/c_1/git", GitURL: "https://git.example.com"}
	tracker = session.Connection{Name: "tracker", Kind: session.ConnectionMCP, Title: "Issue tracker", Audience: "connections",
		Address: "https://connections.example.com/v1/connections/c_2/mcp"}
	wiki = session.Connection{Name: "wiki", Kind: session.ConnectionMCP, Title: "Wiki", Audience: "connections",
		Address: "https://connections.example.com/v1/connections/c_3/mcp"}
)

// unescaped is a request body with the angle brackets JSON escapes
// written as they are.
func unescaped(b []byte) string {
	return strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(b))
}

// connected is a session on a Cella machine with the three connections.
func connected(kind string) func(*session.Session) {
	return func(s *session.Session) {
		s.Machine.Kind = kind
		s.Connections = []session.Connection{codeHost, tracker, wiki}
	}
}

// TestTheConnectionsBlock: every request names the session's http
// connections on a Cella machine with how to clone, push and call their
// APIs, and the mcp connections the drive listed, by their tools' prefix
// or why they are not connected, on the session's thread and a
// subagent's alike; the machine takes the http connections at each
// turn's start, and a send that replaced the list with none leaves no
// block and gives the machine none (spec 069).
func TestTheConnectionsBlock(t *testing.T) {
	ctx := t.Context()
	m := &conMachine{kind: machine.KindCella}
	e := setupSession(t, func(c *Config) {
		c.Machine = m
		c.MCP = map[string]MCPListing{"tracker": {}, "wiki": {NotConnected: true, Reason: "revoked"}}
	}, connected(session.MachineCella))
	e.stub.Script(model, reply(ir.StopEndTurn, text("hi")), reply(ir.StopEndTurn, text("hi")))
	e.send(ctx, "Hello.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	body := unescaped(e.stub.Requests()[0].Body)
	for _, want := range []string{
		"<connections>", "$CODE_HOST_API_URL", "Bearer $CONNECTIONS_TOKEN", "git clone https://git.example.com/<owner>/<repository>",
		"push your own branch, " + session.Branch(e.s), "Issue tracker: its tools are named mcp__tracker__<tool>", "Wiki: not connected (revoked)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the request lacks %q", want)
		}
	}
	want := machine.HTTPConnection{Env: "CODE_HOST_API_URL", Address: codeHost.Address, GitAddress: codeHost.GitAddress, GitURL: codeHost.GitURL}
	if len(m.applied) != 1 || len(m.applied[0]) != 1 || m.applied[0][0] != want {
		t.Fatalf("the machine took %+v", m.applied)
	}
	if own, sub := headerBlocks(e.s, true, e.cfg.MCP), headerBlocks(e.s, false, e.cfg.MCP); len(own) != 1 || len(sub) != 1 || own[0] != sub[0] {
		t.Fatalf("own %q, subagent %q", own, sub)
	}

	none, err := session.NewEvent(session.TypeConnectionsChanged, session.ConnectionsChanged{Connections: []session.Connection{}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, none)
	e.send(ctx, "Again.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if body := unescaped(e.stub.Requests()[1].Body); strings.Contains(body, "<connections>") {
		t.Fatal("a session with no connections still names them")
	}
	if len(m.applied) != 2 || m.applied[1] != nil {
		t.Fatalf("the machine took %+v after the change", m.applied)
	}
}

// TestConnectionsOnAMachineThatCannotTakeThem: a machine that refuses the
// connections appends a retryable session.error connections_unavailable
// and the turn goes on; a host machine takes none and its block names no
// http connection; a session without connections renders no block.
func TestConnectionsOnAMachineThatCannotTakeThem(t *testing.T) {
	ctx := t.Context()
	m := &conMachine{kind: machine.KindCella, refuse: errors.New("the sandbox is gone")}
	e := setupSession(t, func(c *Config) { c.Machine = m }, connected(session.MachineCella))
	e.stub.Script(model, reply(ir.StopEndTurn, text("hi")))
	e.send(ctx, "Hello.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn || len(e.stub.Requests()) != 1 {
		t.Fatalf("outcome %+v", out)
	}
	var se session.SessionError
	if errs := e.events(ctx, session.TypeSessionError); len(errs) != 1 || errs[0].Decode(&se) != nil || se.Code != CodeConnectionsUnavailable || !se.Retryable || se.Detail != "the sandbox is gone" {
		t.Fatalf("session.error %+v", se)
	}

	host := &conMachine{kind: machine.KindHost}
	h := setupSession(t, func(c *Config) { c.Machine = host }, connected(session.MachineHost))
	h.stub.Script(model, reply(ir.StopEndTurn, text("hi")))
	h.send(ctx, "Hello.")
	h.turn(ctx)
	if len(host.applied) != 0 || strings.Contains(unescaped(h.stub.Requests()[0].Body), "<connections>") {
		t.Fatalf("a host machine took %+v", host.applied)
	}
	if got := connectionsBlock(session.Session{Machine: session.Machine{Kind: session.MachineCella}}, nil); got != "" {
		t.Fatalf("a session with none renders %q", got)
	}
}

// TestAConnectionThatIsNotConnected: a call that meets a connection the
// service answers is not connected is followed by connection.requested
// naming the call; a drive's first turn records how each listing that
// offered fewer tools ended, a connection.requested for one not
// connected, a session.error for one that could not be listed and for
// one whose schemas were reduced, and its later turns record none; the
// block names the listed connections, the one not connected as such, and
// not the one that could not be listed; a remote tool opens no machine
// (spec 069).
func TestAConnectionThatIsNotConnected(t *testing.T) {
	ctx := t.Context()
	m := &conMachine{kind: machine.KindCella}
	requested := &session.ConnectionRequested{Name: "tracker", Title: "Issue tracker", Kind: session.ConnectionMCP, Reason: "revoked", Scopes: []string{"issues:write"}}
	remote := &fakeTool{name: "mcp__tracker__create_issue", props: tools.Properties{Parallel: true, Effect: tools.EffectExternal, Remote: true},
		run: func(tools.Call) (tools.Result, error) {
			r := tools.Text(tools.OutcomeError, "Issue tracker is not connected (revoked).")
			r.Requested = requested
			return r, nil
		}}
	broken := session.Connection{Name: "broken", Kind: session.ConnectionMCP, Title: "Broken", Audience: "connections", Address: "https://connections.example.com/b"}
	slow := session.Connection{Name: "slow", Kind: session.ConnectionMCP, Title: "Slow", Audience: "connections", Address: "https://connections.example.com/s"}
	e := setupSession(t, func(c *Config) {
		c.Machine = machine.Defer(ctx, machine.KindCella, func(context.Context) (machine.Machine, error) {
			t.Error("a remote tool opened the machine")
			return m, nil
		})
		c.Policy.AlwaysAllow = []string{"mcp__tracker__create_issue"}
		if err := c.Tools.Add(remote); err != nil {
			t.Fatal(err)
		}
		c.MCP = map[string]MCPListing{"tracker": {}, "wiki": {NotConnected: true, Reason: "revoked", Scopes: []string{"wiki:read"}}, "broken": {Err: "the server answered 502"}, "slow": {Reduced: []string{"search", "fetch"}}}
	}, func(s *session.Session) {
		s.Machine.Kind = session.MachineCella
		s.Connections = []session.Connection{tracker, wiki, broken, slow}
	})
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", "mcp__tracker__create_issue", `{"text":"x"}`)),
		reply(ir.StopEndTurn, text("Connect the issue tracker again.")),
		reply(ir.StopEndTurn, text("hi")))
	e.send(ctx, "Open an issue.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	var reqs []session.ConnectionRequested
	for _, ev := range e.events(ctx, session.TypeConnectionRequested) {
		var p session.ConnectionRequested
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		reqs = append(reqs, p)
	}
	if len(reqs) != 2 || reqs[0].Name != "wiki" || reqs[0].ToolUseID != "" || reqs[0].Reason != "revoked" || reqs[0].Scopes[0] != "wiki:read" ||
		reqs[1].Name != "tracker" || reqs[1].ToolUseID != "toolu_1" || reqs[1].Scopes[0] != "issues:write" {
		t.Fatalf("connection.requested %+v", reqs)
	}
	var codes []string
	for _, ev := range e.events(ctx, session.TypeSessionError) {
		var se session.SessionError
		if err := ev.Decode(&se); err != nil {
			t.Fatal(err)
		}
		codes = append(codes, se.Code+" "+se.Detail)
	}
	if want := []string{CodeMCPUnavailable + " broken: the server answered 502", CodeMCPSchemaReduced + " slow: search, fetch"}; len(codes) != 2 || codes[0] != want[0] || codes[1] != want[1] {
		t.Fatalf("session errors %v", codes)
	}
	body := unescaped(e.stub.Requests()[0].Body)
	for _, want := range []string{"Issue tracker: its tools are named mcp__tracker__<tool>", "Wiki: not connected (revoked)", "Slow: its tools are named mcp__slow__<tool>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the block lacks %q", want)
		}
	}
	if strings.Contains(body, "Broken") {
		t.Error("the block names a connection that could not be listed")
	}
	e.send(ctx, "Again.")
	e.turn(ctx)
	if n, m := len(e.events(ctx, session.TypeConnectionRequested)), len(e.events(ctx, session.TypeSessionError)); n != 2 || m != 2 {
		t.Fatalf("a later turn recorded the listings again: %d, %d", n, m)
	}
}
