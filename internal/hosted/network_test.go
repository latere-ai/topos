// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/cellastub"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// fetcherAgent runs bash and web_fetch in a Cella sandbox that names a
// host of its own.
const fetcherAgent = `apiVersion: topos.latere.ai/v1
kind: Agent
metadata:
  name: fetcher
spec:
  model: {name: anthropic/claude-haiku-4.5}
  tools: [bash, web_fetch]
  machine: {kind: cella, egress: [agent.example.net]}
`

func fetch(id, url string) luxstub.Reply {
	args, err := json.Marshal(map[string]string{"url": url})
	if err != nil {
		panic(err)
	}
	return luxstub.Reply{Response: ir.Response{Model: haiku, Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: "web_fetch", Args: args}}}, StopReason: ir.StopToolUse}}
}

// networked is a session an attended person runs, on an asking network
// of hosts.
func networked(hosts ...string) func(*session.Session) {
	return func(s *session.Session) {
		s.Network = &session.Network{Mode: session.NetworkAllowlist, Hosts: hosts, Ask: true, Source: session.NetworkFromAuthorizer}
		s.Attended = true
	}
}

// event is a client's or a server's event with its payload.
func (c *cloud) event(typ session.Type, p any) session.Event {
	c.t.Helper()
	e, err := session.NewEvent(typ, p, time.Now())
	if err != nil {
		c.t.Fatal(err)
	}
	return e
}

func (c *cloud) message(text string) session.Event {
	return c.event(session.TypeUserMessage, session.UserMessage{Sender: c.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: text}}})
}

func (c *cloud) confirmation(p session.UserToolConfirmation) session.Event {
	p.Sender = c.s.Initiator
	return c.event(session.TypeUserToolConfirmation, p)
}

// egress is the session's sandbox's egress as the stub holds it.
func (c *cloud) egress() []string {
	c.t.Helper()
	sb, ok := c.cella.Sandbox(cella.SandboxName(c.s.ID))
	if !ok {
		c.t.Fatal("the session has no sandbox")
	}
	return sb.Spec.Network.Egress.AllowedHosts
}

// ops are the stub's operations in the order it received them, from the
// n-th request on.
func (c *cloud) ops(from int) []string {
	var out []string
	for _, r := range c.cella.Requests()[from:] {
		out = append(out, r.Op)
	}
	return out
}

// page is a site the session fetches and how often it was asked.
func page(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		if _, err := fmt.Fprint(w, "the page's text"); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestAllowedFetchWidensTheMachine: a fetch of a host outside an asking
// network asks; the person's allow updates the running sandbox to the
// host before the call runs, and the log holds the session.network_changed
// that records it (spec 052).
func TestAllowedFetchWidensTheMachine(t *testing.T) {
	srv, hits := page(t)
	c := newCloudOf(t, fetcherAgent, networked("docs.example.com"))
	c.driveAfter(session.StopToolConfirmation, []session.Event{c.message("Read the page.")}, bash("toolu_1", "pwd"), fetch("toolu_2", srv.URL+"/page"))
	if !slices.Equal(c.egress(), []string{"agent.example.net", "docs.example.com"}) {
		t.Fatalf("the sandbox was created with %q", c.egress())
	}
	var use session.AgentToolUse
	if err := c.events(session.TypeAgentToolUse)[1].Decode(&use); err != nil || use.Verdict != "ask" || use.Reason != "outside the session's network" {
		t.Fatalf("the fetch %+v, %v", use, err)
	}
	before := len(c.cella.Requests())
	c.driveAfter(session.StopEndTurn, []session.Event{c.confirmation(session.UserToolConfirmation{ToolUseID: "toolu_2", Decision: session.DecisionAllow})}, said("Read."))
	if !slices.Contains(c.egress(), "127.0.0.1") || hits.Load() != 1 {
		t.Fatalf("egress %q, the page was asked %d times", c.egress(), hits.Load())
	}
	ops := c.ops(before)
	apply := slices.Index(ops, cellastub.OpApply)
	if apply < 0 || !slices.ContainsFunc(ops[apply:], func(op string) bool { return op == cellastub.OpExec || op == cellastub.OpSession }) {
		t.Fatalf("operations %q: the sandbox is widened before the fetch runs in it", ops)
	}
	changes := c.events(session.TypeNetworkChanged)
	var p session.NetworkChanged
	if len(changes) != 1 || changes[0].Decode(&p) != nil || !slices.Equal(p.Added, []string{"127.0.0.1"}) || p.Source != "person" || p.ToolUseID != "toolu_2" {
		t.Fatalf("network_changed %+v", p)
	}
	if got := c.results(); !strings.Contains(got[1], "the page's text") {
		t.Fatalf("results %q", got)
	}
}

// TestWideningRefused: a widening Cella refuses closes the allowed call
// with network_unavailable, appends no session.network_changed, leaves the
// sandbox's egress as it was, and the fetch never runs (spec 052).
func TestWideningRefused(t *testing.T) {
	srv, hits := page(t)
	c := newCloudOf(t, fetcherAgent, networked("docs.example.com"))
	c.driveAfter(session.StopToolConfirmation, []session.Event{c.message("Read the page.")}, bash("toolu_1", "pwd"), fetch("toolu_2", srv.URL+"/page"))
	c.cella.Fail(cellastub.OpApply, cellastub.Failure{Status: http.StatusForbidden, Code: "forbidden", Detail: "sandbox.update: the boundary is the installation's"})
	c.driveAfter(session.StopEndTurn, []session.Event{c.confirmation(session.UserToolConfirmation{ToolUseID: "toolu_2", Decision: session.DecisionAllow})}, said("I could not read it."))
	if hits.Load() != 0 || slices.Contains(c.egress(), "127.0.0.1") || len(c.events(session.TypeNetworkChanged)) != 0 {
		t.Fatalf("the page was asked %d times, egress %q, %d changes", hits.Load(), c.egress(), len(c.events(session.TypeNetworkChanged)))
	}
	var se session.SessionError
	errs := c.events(session.TypeSessionError)
	if len(errs) != 1 || errs[0].Decode(&se) != nil || se.Code != harness.CodeNetworkUnavailable {
		t.Fatalf("session.error %+v", se)
	}
	var r session.ToolResult
	results := c.events(session.TypeToolResult)
	if err := results[len(results)-1].Decode(&r); err != nil || r.ToolUseID != "toolu_2" || r.Outcome != "error" || !strings.Contains(r.Content[0].Text, "could not be widened") {
		t.Fatalf("result %+v, %v", r, err)
	}
}

// TestRefusedConnectionAsks: a command's connections the gateway refused
// append at most three approval.requested after its result, the rest in
// the last one's more, and the session idles on tool_confirmation; an
// allow by approval_id widens the sandbox and appends approval.decided,
// and a denied host is not asked again (spec 052).
func TestRefusedConnectionAsks(t *testing.T) {
	c := newCloudOf(t, fetcherAgent, networked("docs.example.com"))
	for _, h := range []string{"registry.example.com", "cdn.example.com", "mirror.example.org", "extra.example.org"} {
		c.cella.RefuseHost(h, 443)
	}
	c.driveAfter(session.StopToolConfirmation, []session.Event{c.message("Install.")},
		bash("toolu_1", "echo registry.example.com cdn.example.com mirror.example.org extra.example.org docs.example.com"))
	var reqs []session.ApprovalRequested
	for _, e := range c.events(session.TypeApprovalRequested) {
		var p session.ApprovalRequested
		if err := e.Decode(&p); err != nil {
			t.Fatal(err)
		}
		reqs = append(reqs, p)
	}
	if len(reqs) != 3 || reqs[2].More != 1 || reqs[0].Verdict != "ask" || reqs[0].ToolUseID != "toolu_1" || reqs[0].Destination.Port != 443 {
		t.Fatalf("requests %+v", reqs)
	}
	var hosts []string
	for _, r := range reqs {
		hosts = append(hosts, r.Destination.Host)
	}
	if !slices.Equal(hosts, []string{"cdn.example.com", "extra.example.org", "mirror.example.org"}) {
		t.Fatalf("asked about %q", hosts)
	}
	byHost := func(h string) string { return reqs[slices.Index(hosts, h)].ApprovalID }
	c.driveAfter(session.StopEndTurn, []session.Event{
		c.confirmation(session.UserToolConfirmation{ApprovalID: byHost("cdn.example.com"), Decision: session.DecisionAllow}),
		c.confirmation(session.UserToolConfirmation{ApprovalID: byHost("extra.example.org"), Decision: session.DecisionDeny}),
		c.confirmation(session.UserToolConfirmation{ApprovalID: byHost("mirror.example.org"), Decision: session.DecisionDeny, Note: "use the registry"}),
	}, bash("toolu_2", "echo cdn.example.com extra.example.org"), said("Installed."))
	if !slices.Contains(c.egress(), "cdn.example.com") || slices.Contains(c.egress(), "extra.example.org") {
		t.Fatalf("egress %q", c.egress())
	}
	if n := len(c.events(session.TypeApprovalRequested)); n != 3 {
		t.Fatalf("a denied host was asked again: %d requests", n)
	}
	var decided []string
	for _, e := range c.events(session.TypeApprovalDecided) {
		var d session.ApprovalDecided
		if err := e.Decode(&d); err != nil {
			t.Fatal(err)
		}
		decided = append(decided, d.Decision)
	}
	var changed session.NetworkChanged
	changes := c.events(session.TypeNetworkChanged)
	if !slices.Equal(decided, []string{"allow", "deny", "deny"}) || len(changes) != 1 || changes[0].Decode(&changed) != nil || changed.ApprovalID != byHost("cdn.example.com") {
		t.Fatalf("decided %q, changed %+v", decided, changed)
	}
}

// TestSendNarrowsTheNetwork: an authorizer's change at a send that removes
// a host narrows the session's sandbox before any call runs in it, and the
// hosts the person allowed stay (spec 052).
func TestSendNarrowsTheNetwork(t *testing.T) {
	c := newCloudOf(t, fetcherAgent, networked("a.example.com", "b.example.com"))
	c.drive("Start.", bash("toolu_1", "pwd"), said("Started."))
	if !slices.Equal(c.egress(), []string{"a.example.com", "agent.example.net", "b.example.com"}) {
		t.Fatalf("created with %q", c.egress())
	}
	before := len(c.cella.Requests())
	c.driveAfter(session.StopEndTurn, []session.Event{
		c.event(session.TypeNetworkChanged, session.NetworkChanged{Added: []string{"p.example.com"}, Source: session.NetworkFromPerson, ToolUseID: "toolu_0"}),
		c.event(session.TypeNetworkChanged, session.NetworkChanged{Removed: []string{"b.example.com"}, Source: session.NetworkFromAuthorizer}),
		c.message("Go on."),
	}, bash("toolu_2", "pwd"), said("Done."))
	if !slices.Equal(c.egress(), []string{"a.example.com", "agent.example.net", "p.example.com"}) {
		t.Fatalf("narrowed to %q", c.egress())
	}
	ops := c.ops(before)
	apply := slices.Index(ops, cellastub.OpApply)
	if apply < 0 || slices.ContainsFunc(ops[:apply], func(op string) bool { return op == cellastub.OpSession }) {
		t.Fatalf("operations %q: the sandbox is narrowed before a call runs in it", ops)
	}
	s, err := c.st.Get(t.Context(), c.s.ID)
	if err != nil || !slices.Equal(s.Network.Hosts, []string{"a.example.com"}) {
		t.Fatalf("the header's network %+v, %v", s.Network, err)
	}
}

// TestNetworkSurvivesAClaim: a runner that claims the session after its
// sandbox is gone creates the next one with the network the log holds,
// the hosts a person allowed included, as every runner reads the same log
// (spec 052).
func TestNetworkSurvivesAClaim(t *testing.T) {
	c := newCloudOf(t, fetcherAgent, networked("a.example.com"))
	c.drive("Start.", bash("toolu_1", "pwd"), said("Started."))
	if !c.cella.Remove(cella.SandboxName(c.s.ID)) {
		t.Fatal("no sandbox to remove")
	}
	c.driveAfter(session.StopEndTurn, []session.Event{
		c.event(session.TypeNetworkChanged, session.NetworkChanged{Added: []string{"p.example.com"}, Source: session.NetworkFromPerson, ApprovalID: "apr_01J9Z3Q4W8KX6T0M2V5N7R0000"}),
		c.message("Again."),
	}, bash("toolu_2", "pwd"), said("Done."))
	if n := c.cella.Count(cellastub.OpCreate); n != 2 {
		t.Fatalf("%d creates", n)
	}
	if !slices.Equal(c.egress(), []string{"a.example.com", "agent.example.net", "p.example.com"}) {
		t.Fatalf("the sandbox created again runs %q", c.egress())
	}
}

// TestTheSandboxTakesTheSessionsMode: a session whose network is open
// runs a sandbox in mode open that names no host, and one whose network
// is none a sandbox in mode none, whatever hosts the agent names (spec
// 052).
func TestTheSandboxTakesTheSessionsMode(t *testing.T) {
	for _, mode := range []string{session.NetworkOpen, session.NetworkNone} {
		c := newCloudOf(t, fetcherAgent, func(s *session.Session) {
			s.Network = &session.Network{Mode: mode, Source: session.NetworkFromAuthorizer}
		})
		c.drive("Start.", bash("toolu_1", "pwd"), said("Started."))
		sb, ok := c.cella.Sandbox(cella.SandboxName(c.s.ID))
		if !ok || string(sb.Spec.Network.Egress.Mode) != mode || len(sb.Spec.Network.Egress.AllowedHosts) != 0 {
			t.Fatalf("%s: egress %+v", mode, sb.Spec.Network.Egress)
		}
		var m session.SessionMachine
		if err := c.events(session.TypeSessionMachine)[0].Decode(&m); err != nil || m.Machine.Egress != mode {
			t.Fatalf("%s: session.machine records %+v, %v", mode, m.Machine, err)
		}
	}
}
