// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// conMachine is a Cella machine that takes a session's connections, and
// reaches the connection service unless unreached.
type conMachine struct {
	fakeMachine
	mu        sync.Mutex
	applied   [][]machine.HTTPConnection
	refuse    error
	unreached bool
}

func (m *conMachine) ReachesConnections() bool { return !m.unreached }

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
		GitAddress: "https://connections.example.com/v1/connections/c_1/git", GitURL: "https://git.example.com",
		Account: "ada", APIURL: "https://api.git.example.com"}
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

// TestTheConnectionsBlock: every request says the connected services can
// be used now, and names the session's http connections on a Cella
// machine with the account each acts as, the API base URL its variable
// stands in for, a curl call, and how to clone and push, and the mcp
// connections the drive listed, by their tools' prefix or why they are
// not connected, on the session's thread and a subagent's alike; the
// machine takes the http connections at each turn's start, and a send
// that replaced the list with none leaves no block and gives the machine
// none (spec 069).
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
		"<connections>", "you can use them now", "Code host, connected as ada: $CODE_HOST_API_URL stands in for https://api.git.example.com",
		`curl -s -H \"Authorization: Bearer $CONNECTIONS_TOKEN\" \"$CODE_HOST_API_URL/<path>\"`, "git clone https://git.example.com/<owner>/<repository>",
		"push your own branch, " + session.Branch(e.s), "Issue tracker: its tools are named mcp__tracker__<tool>", "Wiki: not connected (revoked)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the request lacks %q", want)
		}
	}
	if strings.Contains(body, "holds none of them") {
		t.Error("the block still says the machine holds none of the credentials")
	}
	want := machine.HTTPConnection{Env: "CODE_HOST_API_URL", Address: codeHost.Address, GitAddress: codeHost.GitAddress, GitURL: codeHost.GitURL}
	if len(m.applied) != 1 || len(m.applied[0]) != 1 || m.applied[0][0] != want {
		t.Fatalf("the machine took %+v", m.applied)
	}
	if own, sub := headerBlocks(e.s, true, true, e.cfg.MCP), headerBlocks(e.s, false, true, e.cfg.MCP); len(own) != 1 || len(sub) != 1 || own[0] != sub[0] {
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
	if got := connectionsBlock(session.Session{Machine: session.Machine{Kind: session.MachineCella}}, true, true, nil); got != "" {
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

// TestRequestConnection: a session with an available connection is
// offered request_connection on its own thread, and its block names each
// service the person may be asked to connect, apart from the connected
// ones; a call for one appends connection.requested with the call's id,
// the service's title and kind, and the reason on one line, answers the
// model that the person was asked and the turn should end, and opens no
// machine; a call for another name is an error result naming the ones it
// may ask for and appends nothing; an available http connection on a
// host machine is neither named nor asked for, a thread a spawn started
// is offered no request_connection and is told of no available service,
// and a session with none is offered no tool (spec 069).
func TestRequestConnection(t *testing.T) {
	ctx := t.Context()
	github := session.Connection{Name: "github", Kind: session.ConnectionHTTP, Title: "GitHub", State: session.ConnectionAvailable}
	notes := session.Connection{Name: "notes", Kind: session.ConnectionMCP, Title: "Notes", State: session.ConnectionAvailable}
	e := setupSession(t, func(c *Config) {
		c.Machine = machine.Defer(ctx, machine.KindCella, func(context.Context) (machine.Machine, error) {
			t.Error("request_connection opened the machine")
			return &conMachine{kind: machine.KindCella}, nil
		})
		c.MCP = map[string]MCPListing{"tracker": {}}
	}, func(s *session.Session) {
		s.Machine.Kind = session.MachineCella
		s.Connections = []session.Connection{tracker, github, notes}
	})
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", ToolRequestConnection, `{"name":"gitlab","reason":"to clone the app"}`)),
		reply(ir.StopToolUse, call("toolu_2", ToolRequestConnection, `{"name":"github","reason":"to clone acme/app\nand open a pull request"}`)),
		reply(ir.StopEndTurn, text("Once GitHub is connected, I will clone acme/app and open the pull request.")))
	e.send(ctx, "Fix the bug in acme/app and open a pull request.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	body := unescaped(e.stub.Requests()[0].Body)
	for _, want := range []string{
		`"name":"` + ToolRequestConnection + `"`, "Issue tracker: its tools are named mcp__tracker__<tool>",
		"Services the installation offers that the person has not connected.", "- GitHub, named github", "- Notes, named notes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the request lacks %q", want)
		}
	}
	var results []session.ToolResult
	for _, ev := range e.events(ctx, session.TypeToolResult) {
		var r session.ToolResult
		if err := ev.Decode(&r); err != nil {
			t.Fatal(err)
		}
		results = append(results, r)
	}
	if len(results) != 2 || !results[0].IsError || results[0].Content[0].Text != prompts.Render(prompts.RequestConnectionNotAvailable, prompts.Data{"Name": "gitlab", "Names": "github, notes"}) ||
		results[1].IsError || results[1].Content[0].Text != prompts.Render(prompts.RequestConnectionAsked, prompts.Data{"Title": "GitHub"}) {
		t.Fatalf("the results %+v", results)
	}
	var req session.ConnectionRequested
	if reqs := e.events(ctx, session.TypeConnectionRequested); len(reqs) != 1 || reqs[0].Decode(&req) != nil ||
		req.Name != "github" || req.Title != "GitHub" || req.Kind != session.ConnectionHTTP || req.Reason != "to clone acme/app and open a pull request" || req.ToolUseID != "toolu_2" || req.Scopes != nil {
		t.Fatalf("connection.requested %+v", req)
	}
	var use session.AgentToolUse
	if uses := e.events(ctx, session.TypeAgentToolUse); len(uses) != 2 || uses[1].Decode(&use) != nil || use.Verdict != string(VerdictAllow) {
		t.Fatalf("the call's verdict %+v", use)
	}
	if !strings.Contains(prompts.Text(prompts.ToolRequestConnection), fmt.Sprintf("at most %d characters", session.MaxConnectionReason)) {
		t.Error("the description does not state the bound of a reason")
	}

	host := session.Session{Machine: session.Machine{Kind: session.MachineHost}, Connections: []session.Connection{github, notes}}
	if got := connectionsBlock(host, true, reachesHTTP(host, nil), nil); strings.Contains(got, "GitHub") || !strings.Contains(got, "- Notes, named notes") {
		t.Fatalf("a host machine's block %q", got)
	}
	if got := connectionsBlock(e.s, false, true, e.cfg.MCP); strings.Contains(got, "GitHub") || strings.Contains(got, ToolRequestConnection) {
		t.Fatalf("a subagent's block %q", got)
	}
	registry := func(s session.Session, thread string) []string {
		t.Helper()
		r, err := (&turn{h: e.h, s: s, root: e.cfg.Tools, thread: thread}).registry(e.cfg.Tools.Names())
		if err != nil {
			t.Fatal(err)
		}
		return r.Names()
	}
	only := session.Session{Machine: session.Machine{Kind: session.MachineHost}, Connections: []session.Connection{github}}
	for name, names := range map[string][]string{
		"a thread a spawn started": registry(e.s, "evt_thread"),
		"a host machine's session": registry(only, ""),
		"a session with none":      registry(session.Session{}, ""),
	} {
		if strings.Contains(strings.Join(names, " "), ToolRequestConnection) {
			t.Errorf("%s is offered %v", name, names)
		}
	}
	if names := registry(e.s, ""); !strings.Contains(strings.Join(names, " "), ToolRequestConnection) {
		t.Fatalf("the session's own thread is offered %v", names)
	}
}

// TestConnectionsInASandboxThatReachesNoConnectionService: a sandbox that
// opened without the connection service's Secret, because its host was
// another Secret's, says it reaches no connection service, and the model
// is told of none of the session's http connections: the block names no
// connected one and no available one, request_connection is not offered
// for an http service alone, and a call for one answers that it is not
// available; mcp connections are named and asked for as before. The same
// session in a sandbox that reaches the service is told of them (spec
// 069).
func TestConnectionsInASandboxThatReachesNoConnectionService(t *testing.T) {
	ctx := t.Context()
	github := session.Connection{Name: "github", Kind: session.ConnectionHTTP, Title: "GitHub", State: session.ConnectionAvailable}
	notes := session.Connection{Name: "notes", Kind: session.ConnectionMCP, Title: "Notes", State: session.ConnectionAvailable}
	m := &conMachine{kind: machine.KindCella, unreached: true}
	e := setupSession(t, func(c *Config) {
		c.Machine = m
		c.MCP = map[string]MCPListing{"tracker": {}}
	}, func(s *session.Session) {
		s.Machine.Kind = session.MachineCella
		s.Connections = []session.Connection{codeHost, tracker, github, notes}
	})
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", ToolRequestConnection, `{"name":"github","reason":"to clone acme/app"}`)),
		reply(ir.StopEndTurn, text("GitHub cannot be reached from here.")))
	e.send(ctx, "Clone acme/app.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	body := unescaped(e.stub.Requests()[0].Body)
	for _, want := range []string{"<connections>", "Issue tracker: its tools are named mcp__tracker__<tool>", "- Notes, named notes"} {
		if !strings.Contains(body, want) {
			t.Errorf("the request lacks %q", want)
		}
	}
	for _, unwanted := range []string{"Code host", "$CODE_HOST_API_URL", "git clone", "GitHub"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the request names %q, which the sandbox cannot reach", unwanted)
		}
	}
	var res session.ToolResult
	if results := e.events(ctx, session.TypeToolResult); len(results) != 1 || results[0].Decode(&res) != nil || !res.IsError ||
		res.Content[0].Text != prompts.Render(prompts.RequestConnectionNotAvailable, prompts.Data{"Name": "github", "Names": "notes"}) {
		t.Fatalf("a request for an http service the sandbox cannot reach answered %+v", res)
	}
	if n := len(e.events(ctx, session.TypeConnectionRequested)); n != 0 {
		t.Fatalf("%d connection.requested for a service the sandbox cannot reach", n)
	}

	s := session.Session{Machine: session.Machine{Kind: session.MachineCella}, Connections: []session.Connection{codeHost, github}}
	if got := connectionsBlock(s, true, reachesHTTP(s, m), nil); got != "" {
		t.Fatalf("a session whose connections are http alone renders %q", got)
	}
	r, err := (&turn{h: e.h, s: s, root: e.cfg.Tools}).registry(e.cfg.Tools.Names())
	if err != nil {
		t.Fatal(err)
	}
	if names := r.Names(); strings.Contains(strings.Join(names, " "), ToolRequestConnection) {
		t.Fatalf("an http service alone is offered %v", names)
	}
	m.unreached = false
	if got := connectionsBlock(s, true, reachesHTTP(s, m), nil); !strings.Contains(got, "Code host") || !strings.Contains(got, "- GitHub, named github") {
		t.Fatalf("a sandbox that reaches the service renders %q", got)
	}
}
