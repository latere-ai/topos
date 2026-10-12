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
		c.MCP = map[string]string{"tracker": "", "wiki": "revoked"}
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
	if got := HTTPConnections(session.Session{Machine: session.Machine{Kind: session.MachineHost}, Connections: []session.Connection{codeHost}}); got != nil {
		t.Fatalf("a host session's http connections %+v", got)
	}
	if got := connectionsBlock(session.Session{Machine: session.Machine{Kind: session.MachineCella}}, nil); got != "" {
		t.Fatalf("a session with none renders %q", got)
	}
}
