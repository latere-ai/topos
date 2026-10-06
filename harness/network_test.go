// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// netMachine is a Cella machine that takes networks and reports the
// connections its gateway refused, and records both in trace beside the
// calls the tools ran.
type netMachine struct {
	fakeMachine
	mu      sync.Mutex
	trace   *[]string
	applied []machine.Network
	refuse  error
	refused []machine.Connection
	// unread fails the next read of the refused connections.
	unread error
}

// ApplyNetwork takes a network, and as a Cella machine does, sends nothing
// for the network it took last.
func (m *netMachine) ApplyNetwork(_ context.Context, n machine.Network) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k := len(m.applied); k > 0 && m.applied[k-1].Mode == n.Mode && slices.Equal(m.applied[k-1].Hosts, n.Hosts) {
		return nil
	}
	if m.refuse != nil {
		return m.refuse
	}
	m.applied = append(m.applied, n)
	*m.trace = append(*m.trace, "apply "+n.Mode+" "+strings.Join(n.Hosts, ","))
	return nil
}

func (m *netMachine) Refused(context.Context, time.Time) ([]machine.Connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.unread; err != nil {
		m.unread = nil
		return nil, err
	}
	out := m.refused
	m.refused = nil
	return out, nil
}

// lastApplied is the network the machine took last.
func (m *netMachine) lastApplied() machine.Network {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.applied) == 0 {
		return machine.Network{}
	}
	return m.applied[len(m.applied)-1]
}

// netEnv is a session on a networked Cella machine with a web_fetch and a
// bash that record when they run, its header changed by header.
type netEnv struct {
	*env
	m     *netMachine
	fetch *fakeTool
	trace []string
}

func newNetEnv(t *testing.T, mut func(*Config), header func(*session.Session)) *netEnv {
	t.Helper()
	ne := &netEnv{}
	ne.m = &netMachine{kind: machine.KindCella, trace: &ne.trace}
	ne.fetch = &fakeTool{name: tools.NameWebFetch, props: tools.Properties{Parallel: true, Effect: tools.EffectExternal},
		schema: `{"type":"object","properties":{"url":{"type":"string"}},"additionalProperties":false}`,
		run: func(c tools.Call) (tools.Result, error) {
			ne.m.mu.Lock()
			defer ne.m.mu.Unlock()
			ne.trace = append(ne.trace, "run "+c.ID)
			return tools.Text(tools.OutcomeOK, "page"), nil
		}}
	ne.env = setupSession(t, func(c *Config) {
		c.Machine = ne.m
		if err := c.Tools.AddBuiltin(ne.fetch); err != nil {
			t.Fatal(err)
		}
		if mut != nil {
			mut(c)
		}
	}, header)
	return ne
}

// use is the agent.tool_use of a call.
func (ne *netEnv) use(ctx context.Context, id string) session.AgentToolUse {
	ne.t.Helper()
	for _, e := range ne.events(ctx, session.TypeAgentToolUse) {
		var u session.AgentToolUse
		if err := e.Decode(&u); err != nil {
			ne.t.Fatal(err)
		}
		if u.ToolUseID == id {
			return u
		}
	}
	ne.t.Fatalf("no agent.tool_use %s", id)
	return session.AgentToolUse{}
}

// result is the tool.result of a call.
func (ne *netEnv) result(ctx context.Context, id string) session.ToolResult {
	ne.t.Helper()
	for _, e := range ne.events(ctx, session.TypeToolResult) {
		var r session.ToolResult
		if err := e.Decode(&r); err != nil {
			ne.t.Fatal(err)
		}
		if r.ToolUseID == id {
			return r
		}
	}
	ne.t.Fatalf("no tool.result %s", id)
	return session.ToolResult{}
}

func (ne *netEnv) confirm(ctx context.Context, c session.UserToolConfirmation) {
	ne.t.Helper()
	c.Sender = ne.s.Initiator
	e, err := session.NewEvent(session.TypeUserToolConfirmation, c, t0)
	if err != nil {
		ne.t.Fatal(err)
	}
	ne.appendEvents(ctx, e)
	ne.running(ctx)
}

func fetchCall(id, url string) ir.Block {
	return call(id, tools.NameWebFetch, `{"url":"`+url+`"}`)
}

func allowlist(ask bool, hosts ...string) func(*session.Session) {
	return func(s *session.Session) {
		s.Network = &session.Network{Mode: session.NetworkAllowlist, Hosts: hosts, Ask: ask, Source: session.NetworkFromAuthorizer}
		s.Attended = true
	}
}

// TestFetchInsideTheNetwork: a fetch of a host inside the session's
// network runs in confirm without asking and records the reason a client
// renders, whether the host is the network's own, matched by a "*."
// pattern, the agent's, or any host under open; always_confirm still asks
// for it (spec 052).
func TestFetchInsideTheNetwork(t *testing.T) {
	ctx := t.Context()
	ne := newNetEnv(t, func(c *Config) { c.Policy.Egress = []string{"agent.example.net"} }, allowlist(false, "docs.example.com", "*.example.org"))
	ne.stub.Script(model,
		reply(ir.StopToolUse, fetchCall("toolu_1", "https://docs.example.com/a"), fetchCall("toolu_2", "https://api.example.org/b"), fetchCall("toolu_3", "https://agent.example.net/c")),
		reply(ir.StopEndTurn, text("read")))
	ne.send(ctx, "Read the docs.")
	if out := ne.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	for _, id := range []string{"toolu_1", "toolu_2", "toolu_3"} {
		if u := ne.use(ctx, id); u.Verdict != string(VerdictAllow) || u.Reason != "inside the session's network" {
			t.Errorf("%s: %s %q", id, u.Verdict, u.Reason)
		}
	}
	if got := ne.fetch.ran(); len(got) != 3 {
		t.Fatalf("ran %v", got)
	}

	confirm := newNetEnv(t, func(c *Config) { c.Policy.AlwaysConfirm = []string{"web_fetch"} }, allowlist(false, "docs.example.com"))
	confirm.stub.Script(model, reply(ir.StopToolUse, fetchCall("toolu_1", "https://docs.example.com/a")))
	confirm.send(ctx, "Read.")
	if out := confirm.turn(ctx); out.StopReason != session.StopToolConfirmation || len(confirm.fetch.ran()) != 0 {
		t.Fatalf("always_confirm: %+v, ran %v", out, confirm.fetch.ran())
	}

	open := newNetEnv(t, nil, func(s *session.Session) { s.Network = &session.Network{Mode: session.NetworkOpen} })
	open.stub.Script(model, reply(ir.StopToolUse, fetchCall("toolu_1", "https://anywhere.example/")), reply(ir.StopEndTurn, text("ok")))
	open.send(ctx, "Read.")
	if out := open.turn(ctx); out.StopReason != session.StopEndTurn || open.use(ctx, "toolu_1").Reason != "inside the session's network" {
		t.Fatalf("open: %+v %+v", out, open.use(ctx, "toolu_1"))
	}

	// In progressive a host inside scores as a named host.
	prog := newNetEnv(t, func(c *Config) { c.Policy.Mode = ModeProgressive }, allowlist(false, "docs.example.com"))
	prog.stub.Script(model, reply(ir.StopToolUse, fetchCall("toolu_1", "https://docs.example.com/a")), reply(ir.StopEndTurn, text("ok")))
	prog.send(ctx, "Read.")
	prog.turn(ctx)
	if u := prog.use(ctx, "toolu_1"); u.Risk == nil || u.Risk.Score != 0.4 || !slices.Contains(u.Risk.Features, "egress:named") {
		t.Fatalf("progressive: %+v", u.Risk)
	}
}

// TestFetchOutsideBlocks: a fetch of a host outside a network that does
// not ask, outside an asking network in a session nobody attends, or
// under none, is blocked with the result the model reads, and does not
// run (spec 052).
func TestFetchOutsideBlocks(t *testing.T) {
	ctx := t.Context()
	for name, header := range map[string]func(*session.Session){
		"no ask":     allowlist(false, "docs.example.com"),
		"unattended": func(s *session.Session) { allowlist(true, "docs.example.com")(s); s.Attended = false },
		"none":       func(s *session.Session) { s.Network = &session.Network{Mode: session.NetworkNone} },
	} {
		ne := newNetEnv(t, func(c *Config) { c.Policy.AlwaysConfirm = []string{"web_fetch"} }, header)
		ne.stub.Script(model, reply(ir.StopToolUse, fetchCall("toolu_1", "https://other.example.com/x")), reply(ir.StopEndTurn, text("ok")))
		ne.send(ctx, "Read.")
		if out := ne.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("%s: outcome %+v", name, out)
		}
		want := prompts.Render(prompts.CallOutsideNetwork, prompts.Data{"Host": "other.example.com"})
		r := ne.result(ctx, "toolu_1")
		if u := ne.use(ctx, "toolu_1"); u.Verdict != string(VerdictBlock) || r.Outcome != tools.OutcomeBlocked || r.Content[0].Text != want || len(ne.fetch.ran()) != 0 {
			t.Fatalf("%s: verdict %s, result %+v, ran %v", name, u.Verdict, r, ne.fetch.ran())
		}
	}
	// A session that records no network runs on its agent's mode, with
	// ask false.
	agent := newNetEnv(t, func(c *Config) { c.Policy.EgressMode = "allowlist"; c.Policy.Egress = []string{"agent.example.net"} }, nil)
	agent.stub.Script(model, reply(ir.StopToolUse, fetchCall("toolu_1", "https://other.example.com/x"), fetchCall("toolu_2", "https://agent.example.net/")), reply(ir.StopEndTurn, text("ok")))
	agent.send(ctx, "Read.")
	agent.turn(ctx)
	if agent.use(ctx, "toolu_1").Verdict != string(VerdictBlock) || agent.use(ctx, "toolu_2").Verdict != string(VerdictAllow) {
		t.Fatalf("the agent's network: %+v %+v", agent.use(ctx, "toolu_1"), agent.use(ctx, "toolu_2"))
	}
	// A host machine is decided by the mode alone, as before.
	host := newNetEnv(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} }, allowlist(false, "docs.example.com"))
	host.stub.Script(model, reply(ir.StopToolUse, fetchCall("toolu_1", "https://other.example.com/x")))
	host.send(ctx, "Read.")
	if out := host.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("a host machine: %+v", out)
	}
}

// TestAnAllowedFetchWidensBeforeItRuns: a fetch outside an asking network
// of an attended session asks with the reason a client renders; the
// person's allow widens the machine first, then records the change, then
// runs the call; a widening the machine refuses closes the call
// network_unavailable and changes nothing (spec 052).
func TestAnAllowedFetchWidensBeforeItRuns(t *testing.T) {
	ctx := t.Context()
	ne := newNetEnv(t, nil, allowlist(true, "docs.example.com"))
	ne.stub.Script(model, reply(ir.StopToolUse, fetchCall("toolu_1", "https://other.example.com/x")), reply(ir.StopEndTurn, text("read")))
	ne.send(ctx, "Read.")
	if out := ne.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	if u := ne.use(ctx, "toolu_1"); u.Verdict != string(VerdictAsk) || u.Reason != "outside the session's network" {
		t.Fatalf("asked %+v", u)
	}
	ne.confirm(ctx, session.UserToolConfirmation{ToolUseID: "toolu_1", Decision: session.DecisionAllow})
	if out := ne.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("after the allow %+v", out)
	}
	widened := "apply allowlist docs.example.com,other.example.com"
	if i := slices.Index(ne.trace, widened); i < 0 || slices.Index(ne.trace, "run toolu_1") < i {
		t.Fatalf("trace %q: the machine is widened before the call runs", ne.trace)
	}
	changes := ne.events(ctx, session.TypeNetworkChanged)
	var c session.NetworkChanged
	if len(changes) != 1 || changes[0].Decode(&c) != nil || !slices.Equal(c.Added, []string{"other.example.com"}) || c.Source != "person" || c.ToolUseID != "toolu_1" {
		t.Fatalf("network_changed %+v", c)
	}
	if res := ne.events(ctx, session.TypeToolResult); res[0].Seq < changes[0].Seq {
		t.Fatal("the result precedes the change")
	}

	refused := newNetEnv(t, nil, allowlist(true, "docs.example.com"))
	refused.stub.Script(model, reply(ir.StopToolUse, fetchCall("toolu_1", "https://other.example.com/x")), reply(ir.StopEndTurn, text("no")))
	refused.send(ctx, "Read.")
	refused.turn(ctx)
	refused.m.mu.Lock()
	refused.m.refuse = errors.New("Cella refused to change the network: admission_refused")
	refused.m.mu.Unlock()
	refused.confirm(ctx, session.UserToolConfirmation{ToolUseID: "toolu_1", Decision: session.DecisionAllow})
	refused.turn(ctx)
	r := refused.result(ctx, "toolu_1")
	if r.Outcome != tools.OutcomeError || !strings.Contains(r.Content[0].Text, "could not be widened to other.example.com") || len(refused.fetch.ran()) != 0 {
		t.Fatalf("refused widening: %+v, ran %v", r, refused.fetch.ran())
	}
	var se session.SessionError
	errs := refused.events(ctx, session.TypeSessionError)
	if len(errs) != 1 || errs[0].Decode(&se) != nil || se.Code != CodeNetworkUnavailable || len(refused.events(ctx, session.TypeNetworkChanged)) != 0 {
		t.Fatalf("session.error %+v, %d changes", se, len(refused.events(ctx, session.TypeNetworkChanged)))
	}
}

// TestARefusedConnectionAsksAfterTheCall: a bash call's connections the
// gateway refused outside the network follow its result as at most
// MaxAsksPerCall approval.requested, the rest counted in the last's more,
// and the session waits; an allow by approval_id widens the network and
// the model reads that it may retry, a deny is not asked again, and a
// session nobody attends records the refusal with nothing waiting (spec
// 052).
func TestARefusedConnectionAsksAfterTheCall(t *testing.T) {
	ctx := t.Context()
	ne := newNetEnv(t, nil, allowlist(true, "docs.example.com"))
	ne.m.refused = []machine.Connection{
		{Host: "a.example.com", Port: 443}, {Host: "docs.example.com", Port: 443}, {Host: "b.example.com", Port: 443},
		{Host: "c.example.com", Port: 80}, {Host: "d.example.com", Port: 443}, {Host: "e.example.com", Port: 443},
	}
	ne.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", "bash", `{"command":"make deps"}`)),
		reply(ir.StopToolUse, call("toolu_2", "bash", `{"command":"make deps"}`)),
		reply(ir.StopEndTurn, text("done")))
	ne.send(ctx, "Install.")
	if out := ne.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	reqs := ne.events(ctx, session.TypeApprovalRequested)
	if len(reqs) != MaxAsksPerCall {
		t.Fatalf("%d requests", len(reqs))
	}
	result := ne.events(ctx, session.TypeToolResult)[0]
	var ps []session.ApprovalRequested
	for _, e := range reqs {
		var p session.ApprovalRequested
		if err := e.Decode(&p); err != nil || e.Seq < result.Seq {
			t.Fatalf("request %+v %v before the result", p, err)
		}
		ps = append(ps, p)
	}
	if ps[0].Destination.Host != "a.example.com" || ps[2].Destination.Host != "c.example.com" || ps[2].Destination.Port != 80 || ps[2].More != 2 || ps[0].More != 0 ||
		ps[0].Verdict != "ask" || ps[0].Reason != "connection outside the session's network" || ps[0].Source != "egress" || ps[0].ToolUseID != "toolu_1" || !strings.HasPrefix(ps[0].ApprovalID, "apr_") {
		t.Fatalf("requests %+v", ps)
	}
	// Two answered, one left: the session keeps waiting.
	ne.confirm(ctx, session.UserToolConfirmation{ApprovalID: ps[0].ApprovalID, Decision: session.DecisionAllow})
	ne.confirm(ctx, session.UserToolConfirmation{ApprovalID: ps[1].ApprovalID, Decision: session.DecisionDeny, Note: "use the mirror"})
	if out := ne.turn(ctx); out.StopReason != session.StopToolConfirmation || len(ne.stub.Requests()) != 1 {
		t.Fatalf("with an approval left: %+v, %d requests", out, len(ne.stub.Requests()))
	}
	if n := ne.m.lastApplied(); !slices.Equal(n.Hosts, []string{"a.example.com", "docs.example.com"}) {
		t.Fatalf("the machine took %+v", n)
	}
	// A message in place of the last answer denies it, and the turn goes
	// on: the model reads each answer, and its retry is not asked about a
	// denied host again.
	ne.m.refused = []machine.Connection{{Host: "b.example.com", Port: 443}, {Host: "c.example.com", Port: 80}}
	ne.send(ctx, "Skip that one.")
	if out := ne.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("after the answers %+v", out)
	}
	var decided []session.ApprovalDecided
	for _, e := range ne.events(ctx, session.TypeApprovalDecided) {
		var d session.ApprovalDecided
		if err := e.Decode(&d); err != nil {
			t.Fatal(err)
		}
		decided = append(decided, d)
	}
	if len(decided) != 3 || decided[0].Decision != "allow" || decided[1].Decision != "deny" || decided[1].Note != "use the mirror" || decided[2].Decision != "deny" || decided[2].By.Subject != "usr_ada" {
		t.Fatalf("decided %+v", decided)
	}
	if n := len(ne.events(ctx, session.TypeApprovalRequested)); n != MaxAsksPerCall {
		t.Fatalf("a denied host was asked again: %d requests", n)
	}
	reqs2 := ne.stub.Requests()
	var seen []string
	for _, m := range reqs2[1].Request.Messages {
		for _, b := range m.Blocks {
			if b.Type == ir.BlockText {
				seen = append(seen, b.Text)
			}
		}
	}
	all := strings.Join(seen, "\n")
	if !strings.Contains(all, "The person allowed connections to a.example.com.") || !strings.Contains(all, "The person denied connections to b.example.com.") {
		t.Fatalf("the model read %q", all)
	}

	// Nobody attends: the refusal is recorded with nothing waiting.
	un := newNetEnv(t, nil, func(s *session.Session) { allowlist(true, "docs.example.com")(s); s.Attended = false })
	un.m.refused = []machine.Connection{{Host: "a.example.com", Port: 443}}
	un.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "bash", `{"command":"make"}`)), reply(ir.StopEndTurn, text("done")))
	un.send(ctx, "Install.")
	if out := un.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("unattended: %+v", out)
	}
	var p session.ApprovalRequested
	if rs := un.events(ctx, session.TypeApprovalRequested); len(rs) != 1 || rs[0].Decode(&p) != nil || p.Verdict != "block" {
		t.Fatalf("unattended request %+v", p)
	}
}

// TestASendsNetworkReachesTheMachineBeforeTheTurn: a change an
// authorizer's send made is given to the running machine before the
// turn's first request, and the hosts a person allowed stay; a network
// the machine cannot take ends the turn network_unavailable before any
// request (spec 052).
func TestASendsNetworkReachesTheMachineBeforeTheTurn(t *testing.T) {
	ctx := t.Context()
	ne := newNetEnv(t, nil, allowlist(true, "a.example.com", "b.example.com"))
	allowed, err := session.NewEvent(session.TypeNetworkChanged, session.NetworkChanged{Added: []string{"p.example.com"}, Source: session.NetworkFromPerson, ToolUseID: "toolu_0"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	narrowed, err := session.NewEvent(session.TypeNetworkChanged, session.NetworkChanged{Removed: []string{"b.example.com"}, Source: session.NetworkFromAuthorizer}, t0)
	if err != nil {
		t.Fatal(err)
	}
	ne.appendEvents(ctx, allowed, narrowed)
	ne.stub.Script(model, reply(ir.StopEndTurn, text("hi")))
	ne.send(ctx, "Hello.")
	if out := ne.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if got := ne.m.lastApplied(); got.Mode != "allowlist" || !slices.Equal(got.Hosts, []string{"a.example.com", "p.example.com"}) {
		t.Fatalf("the machine took %+v", got)
	}

	refused := newNetEnv(t, nil, allowlist(false, "a.example.com"))
	refused.m.refuse = errors.New("Cella is unreachable")
	refused.stub.Script(model, reply(ir.StopEndTurn, text("hi")))
	refused.send(ctx, "Hello.")
	if out := refused.turn(ctx); out.StopReason != session.StopError || out.Detail != CodeNetworkUnavailable || len(refused.stub.Requests()) != 0 {
		t.Fatalf("a refused network: %+v, %d requests", out, len(refused.stub.Requests()))
	}
}

// TestEffectiveNetworkJoinsTheSessionsHosts: under allowlist the network
// holds the base hosts, the person's allows, the agent's hosts and the
// repositories' git hosts; open and none hold none and never ask.
func TestEffectiveNetworkJoinsTheSessionsHosts(t *testing.T) {
	s := session.Session{
		Network:   &session.Network{Mode: session.NetworkAllowlist, Hosts: []string{"base.example.com"}, Ask: true},
		Resources: []session.Resource{{Type: session.ResourceRepository, URL: "https://code.example.com/team/app"}},
	}
	e, err := session.NewEvent(session.TypeNetworkChanged, session.NetworkChanged{Added: []string{"p.example.com"}, Source: session.NetworkFromPerson}, t0)
	if err != nil {
		t.Fatal(err)
	}
	n := EffectiveNetwork(s, []session.Event{e}, "open", []string{"agent.example.net"})
	if n.Mode != "allowlist" || !n.Ask || !slices.Equal(n.Hosts, []string{"agent.example.net", "base.example.com", "code.example.com", "p.example.com"}) {
		t.Fatalf("network %+v", n)
	}
	s.Network = &session.Network{Mode: session.NetworkOpen}
	if n := EffectiveNetwork(s, nil, "", []string{"agent.example.net"}); n.Mode != "open" || len(n.Hosts) != 0 || n.Ask || !n.Inside("any.example") {
		t.Fatalf("open %+v", n)
	}
	s.Network = nil
	if n := EffectiveNetwork(s, nil, "none", nil); n.Mode != "none" || n.Inside("code.example.com") {
		t.Fatalf("the agent's none %+v", n)
	}
}

// unopenable is a networked machine opened on demand whose open fails.
type unopenable struct {
	*netMachine
	err error
}

func (u unopenable) Open(context.Context) error { return u.err }

// TestTheNetworksEdges: the rules settle a fetch outside the network for
// a decision service; an allowed fetch the network stopped asking about
// is blocked, and one whose machine cannot open is answered as any
// tool's; an approval's allow the machine refuses reads as still out of
// reach; and a read of the refused connections that fails is a
// session.error the call stands beside (spec 052).
func TestTheNetworksEdges(t *testing.T) {
	ctx := t.Context()
	fetchOf := func(url string) Call {
		return Call{Name: tools.NameWebFetch, Props: tools.Properties{Effect: tools.EffectExternal}, Input: []byte(`{"url":"` + url + `"}`)}
	}
	p := Policy{Mode: ModeProgressive, Network: &Network{Mode: "allowlist", Hosts: []string{"a.example.com"}, Ask: true}}
	if d, ok := p.Settle(fetchOf("https://b.example.com/"), session.Risk{}); !ok || d.Verdict != VerdictAsk || d.ReviewProbability != 1 || d.Reason != "outside the session's network" {
		t.Fatalf("an asking network settles %+v, %v", d, ok)
	}
	p.Network.Ask = false
	if d, ok := p.Settle(fetchOf("https://b.example.com/"), session.Risk{}); !ok || d.Verdict != VerdictBlock {
		t.Fatalf("a network that does not ask settles %+v, %v", d, ok)
	}
	if _, ok := p.Settle(fetchOf("https://a.example.com/"), session.Risk{Score: 0.4}); ok {
		t.Fatal("a fetch inside the network is settled before the service is asked")
	}

	// The authorizer stopped the network asking between the ask and the
	// allow: the fetch is blocked and nothing widens.
	ne := newNetEnv(t, nil, allowlist(true, "docs.example.com"))
	ne.stub.Script(model, reply(ir.StopToolUse, fetchCall("toolu_1", "https://other.example.com/x")), reply(ir.StopEndTurn, text("no")))
	ne.send(ctx, "Read.")
	ne.turn(ctx)
	off, err := session.NewEvent(session.TypeNetworkChanged, session.NetworkChanged{Ask: new(false), Source: session.NetworkFromAuthorizer}, t0)
	if err != nil {
		t.Fatal(err)
	}
	ne.appendEvents(ctx, off)
	ne.confirm(ctx, session.UserToolConfirmation{ToolUseID: "toolu_1", Decision: session.DecisionAllow})
	ne.turn(ctx)
	if r := ne.result(ctx, "toolu_1"); r.Outcome != tools.OutcomeBlocked || len(ne.fetch.ran()) != 0 || len(ne.events(ctx, session.TypeNetworkChanged)) != 1 {
		t.Fatalf("a network that stopped asking: %+v, ran %v", r, ne.fetch.ran())
	}

	// The machine cannot be had when the allowed fetch would widen it.
	closed := newNetEnv(t, nil, allowlist(true, "docs.example.com"))
	closed.cfg.Machine = unopenable{netMachine: closed.m, err: errors.New("Cella is unreachable")}
	h, err := New(closed.cfg)
	if err != nil {
		t.Fatal(err)
	}
	closed.h = h
	closed.stub.Script(model, reply(ir.StopToolUse, fetchCall("toolu_1", "https://other.example.com/x")), reply(ir.StopEndTurn, text("no")))
	closed.send(ctx, "Read.")
	closed.turn(ctx)
	closed.confirm(ctx, session.UserToolConfirmation{ToolUseID: "toolu_1", Decision: session.DecisionAllow})
	closed.turn(ctx)
	var se session.SessionError
	if r := closed.result(ctx, "toolu_1"); r.Outcome != tools.OutcomeError || !strings.Contains(r.Content[0].Text, "could not be started") ||
		closed.events(ctx, session.TypeSessionError)[0].Decode(&se) != nil || se.Code != machine.CodeUnavailable {
		t.Fatalf("an unopenable machine: %+v, %+v", r, se)
	}

	// An approval's allow the machine refuses: the model reads that the
	// host is still out of reach.
	refused := newNetEnv(t, nil, allowlist(true, "docs.example.com"))
	refused.m.refused = []machine.Connection{{Host: "a.example.com", Port: 443}}
	refused.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "bash", `{"command":"make"}`)), reply(ir.StopEndTurn, text("sorry")))
	refused.send(ctx, "Install.")
	refused.turn(ctx)
	var req session.ApprovalRequested
	if err := refused.events(ctx, session.TypeApprovalRequested)[0].Decode(&req); err != nil {
		t.Fatal(err)
	}
	refused.m.mu.Lock()
	refused.m.refuse = errors.New("Cella refused to change the network: admission_refused")
	refused.m.mu.Unlock()
	refused.confirm(ctx, session.UserToolConfirmation{ApprovalID: req.ApprovalID, Decision: session.DecisionAllow})
	if out := refused.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("after a refused allow: %+v", out)
	}
	if n := len(refused.events(ctx, session.TypeNetworkChanged)); n != 0 {
		t.Fatalf("%d changes after a refused widening", n)
	}
	var d session.ApprovalDecided
	if err := refused.events(ctx, session.TypeApprovalDecided)[0].Decode(&d); err != nil || d.Decision != "allow" {
		t.Fatalf("decided %+v, %v", d, err)
	}
	reqs := refused.stub.Requests()
	last := reqs[len(reqs)-1].Request.Messages
	var said []string
	for _, b := range last[len(last)-1].Blocks {
		said = append(said, b.Text)
	}
	if !strings.Contains(strings.Join(said, "\n"), "still out of reach") {
		t.Fatalf("the model read %q", said)
	}

	// A read of the refused connections that fails is recorded, and the
	// call's result stands.
	unread := newNetEnv(t, nil, allowlist(true, "docs.example.com"))
	unread.m.unread = errors.New("the gateway's records are unavailable")
	unread.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "bash", `{"command":"make"}`)), reply(ir.StopEndTurn, text("done")))
	unread.send(ctx, "Install.")
	if out := unread.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("an unread record: %+v", out)
	}
	errs := unread.events(ctx, session.TypeSessionError)
	if len(errs) != 1 || errs[0].Decode(&se) != nil || se.Code != CodeNetworkUnavailable || len(unread.events(ctx, session.TypeToolResult)) != 1 {
		t.Fatalf("an unread record: %+v", se)
	}
}
