// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// confirmBody is a user.tool_confirmation as a client sends it.
func confirmBody(payload string) string {
	return `{"type":"user.tool_confirmation","payload":` + payload + `}`
}

// TestNetworkAtCreateAndSend: a create records the network its allow
// names, or its agent's with ask false; a send's allow that names another
// network appends session.network_changed by the authorizer straight
// before the event and the header takes it, one that names the same
// network or none appends nothing, and one toposd cannot read refuses the
// send (spec 052).
func TestNetworkAtCreateAndSend(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	plain := f.create("alice", "reviewer")
	if plain.Network == nil || plain.Network.Mode != "allowlist" || plain.Network.Source != "agent" || len(plain.Network.Hosts) != 0 || plain.Network.Ask {
		t.Fatalf("a create no allow names a network for: %+v", plain.Network)
	}

	l := f.level(func(req authz.Request) *authorizer.WireLimits {
		if req.Action == authorizer.ActionSessionCreate {
			return &authorizer.WireLimits{Network: &authorizer.Network{Mode: "allowlist", Hosts: []string{"Docs.Example.com"}, Ask: true}}
		}
		return nil
	})
	s := f.create("alice", "reviewer")
	want := session.Network{Mode: "allowlist", Hosts: []string{"docs.example.com"}, Ask: true, Source: "authorizer"}
	if s.Network == nil || s.Network.Mode != want.Mode || !slices.Equal(s.Network.Hosts, want.Hosts) || !s.Network.Ask || s.Network.Source != want.Source {
		t.Fatalf("the created network %+v", s.Network)
	}

	l.to(&authorizer.WireLimits{Network: &authorizer.Network{Mode: "allowlist", Hosts: []string{"api.example.com", "docs.example.com"}, Ask: true}})
	if a := f.send(s.ID, "Read the API too."); a.status != http.StatusOK {
		t.Fatalf("a send: %d %s", a.status, a.body)
	}
	log := f.log(s.ID)
	n := len(log)
	var c session.NetworkChanged
	if log[n-2].Type != session.TypeNetworkChanged || log[n-2].Decode(&c) != nil || log[n-1].Type != session.TypeUserMessage ||
		!slices.Equal(c.Added, []string{"api.example.com"}) || len(c.Removed) != 0 || c.Mode != "" || c.Ask != nil || c.Source != "authorizer" {
		t.Fatalf("the send appended %s %+v then %s", log[n-2].Type, c, log[n-1].Type)
	}
	if h := f.header(s.ID); !slices.Equal(h.Network.Hosts, []string{"api.example.com", "docs.example.com"}) {
		t.Fatalf("the header's network %+v", h.Network)
	}
	if a := f.send(s.ID, "Again."); a.status != http.StatusOK || len(f.log(s.ID)) != n+1 {
		t.Fatalf("the same network: %d, %d events, want %d", a.status, len(f.log(s.ID)), n+1)
	}
	l.to(nil)
	if a := f.send(s.ID, "Once more."); a.status != http.StatusOK || len(f.log(s.ID)) != n+2 {
		t.Fatalf("no network: %d, %d events", a.status, len(f.log(s.ID)))
	}
	l.to(&authorizer.WireLimits{Network: &authorizer.Network{Mode: "none"}})
	if a := f.send(s.ID, "Stop."); a.status != http.StatusOK {
		t.Fatalf("none: %d %s", a.status, a.body)
	}
	if h := f.header(s.ID); h.Network.Mode != "none" || len(h.Network.Hosts) != 0 || h.Network.Ask {
		t.Fatalf("narrowed to none: %+v", h.Network)
	}
	l.to(&authorizer.WireLimits{Network: &authorizer.Network{Mode: "open", Hosts: []string{"example.com"}}})
	before := len(f.log(s.ID))
	if a := f.send(s.ID, "Open."); a.status == http.StatusOK || !strings.Contains(string(a.body), "authorizer_unavailable") || len(f.log(s.ID)) != before {
		t.Fatalf("an unreadable network: %d %s", a.status, a.body)
	}
}

// TestInstructionsAtCreateOnly: a create records the initiator's
// instructions its allow carries; a send's allow never changes them; a
// fork takes the ones its own allow carries, and its network is its own
// allow's, not the hosts its parent's person allowed (specs 052 and 053).
func TestInstructionsAtCreateOnly(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	f.level(func(req authz.Request) *authorizer.WireLimits {
		switch req.Action {
		case authorizer.ActionSessionCreate:
			return &authorizer.WireLimits{Instructions: "Call me Ada. I work on the parser."}
		case authorizer.ActionSessionFork:
			return &authorizer.WireLimits{Instructions: "Call me Ada in the fork."}
		}
		return &authorizer.WireLimits{Instructions: "A send's text."}
	})
	s := f.create("alice", "reviewer")
	if s.Instructions != "Call me Ada. I work on the parser." {
		t.Fatalf("created with %q", s.Instructions)
	}
	if a := f.send(s.ID, "Go on."); a.status != http.StatusOK {
		t.Fatalf("a send: %d %s", a.status, a.body)
	}
	if h := f.header(s.ID); h.Instructions != s.Instructions {
		t.Fatalf("a send changed the instructions to %q", h.Instructions)
	}
	f.put(s.ID, ev(t, session.TypeNetworkChanged, session.NetworkChanged{Added: []string{"p.example.com"}, Source: session.NetworkFromPerson, ToolUseID: "toolu_1"}))
	end := f.turn(s.ID, 1, "Done.", 1)
	a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", `{"at_seq":`+strconv.FormatUint(end, 10)+`}`)
	if a.status != http.StatusCreated {
		t.Fatalf("a fork: %d %s", a.status, a.body)
	}
	var fork session.Session
	a.decode(t, &fork)
	if fork.Instructions != "Call me Ada in the fork." {
		t.Fatalf("the fork took %q", fork.Instructions)
	}
	evs, err := f.sessions.Events(t.Context(), fork.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r := session.NetworkOf(fork, evs); len(r.Allowed) != 0 || fork.Network == nil || fork.Network.Source != "agent" {
		t.Fatalf("the fork's reach %+v, network %+v", r, fork.Network)
	}
}

// TestConfirmationByApprovalID: a confirmation names exactly one of
// tool_use_id and approval_id; one of an approval that asks and that
// nothing answered is appended, a second is conflict, as is one of an
// approval nothing waits on; remember with approval_id is refused (spec
// 052).
func TestConfirmationByApprovalID(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	req := func(id, verdict string) session.Event {
		return ev(t, session.TypeApprovalRequested, session.ApprovalRequested{ApprovalID: id, ToolUseID: "toolu_1", Source: session.ApprovalFromEgress,
			Destination: session.Destination{Host: "registry.example.com", Port: 443}, Reason: session.ReasonConnectionOutside, Verdict: verdict})
	}
	f.put(s.ID,
		ev(t, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning}),
		req("apr_01J9Z3Q4W8KX6T0M2V5N7R0001", "ask"),
		req("apr_01J9Z3Q4W8KX6T0M2V5N7R0002", "block"),
		ev(t, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopToolConfirmation}),
	)
	path := "/v1/sessions/" + s.ID + "/events"
	for name, c := range map[string]struct {
		payload string
		status  int
	}{
		"both ids":   {`{"tool_use_id":"toolu_1","approval_id":"apr_01J9Z3Q4W8KX6T0M2V5N7R0001","decision":"allow"}`, http.StatusBadRequest},
		"no id":      {`{"decision":"allow"}`, http.StatusBadRequest},
		"remember":   {`{"approval_id":"apr_01J9Z3Q4W8KX6T0M2V5N7R0001","decision":"allow","remember":"bash(curl*)"}`, http.StatusBadRequest},
		"a block":    {`{"approval_id":"apr_01J9Z3Q4W8KX6T0M2V5N7R0002","decision":"allow"}`, http.StatusConflict},
		"no request": {`{"approval_id":"apr_01J9Z3Q4W8KX6T0M2V5N7R0009","decision":"allow"}`, http.StatusConflict},
	} {
		if a := f.do(http.MethodPost, path, "alice", confirmBody(c.payload)); a.status != c.status {
			t.Errorf("%s: %d %s, want %d", name, a.status, a.body, c.status)
		}
	}
	allow := confirmBody(`{"approval_id":"apr_01J9Z3Q4W8KX6T0M2V5N7R0001","decision":"allow","note":"the registry is fine"}`)
	if a := f.do(http.MethodPost, path, "alice", allow); a.status != http.StatusOK {
		t.Fatalf("the allow: %d %s", a.status, a.body)
	}
	var p session.UserToolConfirmation
	log := f.log(s.ID)
	if err := log[len(log)-1].Decode(&p); err != nil || p.ApprovalID != "apr_01J9Z3Q4W8KX6T0M2V5N7R0001" || p.ToolUseID != "" || !strings.HasSuffix(p.Sender.Subject, "|alice") {
		t.Fatalf("appended %+v, %v", p, err)
	}
	if a := f.do(http.MethodPost, path, "alice", allow); a.status != http.StatusConflict {
		t.Fatalf("a second answer: %d %s", a.status, a.body)
	}
}
