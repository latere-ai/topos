// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// TestConfirmationAnswerFields (spec 101): a confirmation that says how
// the person answered is recorded as sent; a field of another type, an
// answer_ms outside a day and a surface outside its list are
// invalid_request; and a confirmation without them is accepted as before.
func TestConfirmationAnswerFields(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	path := "/v1/sessions/" + s.ID + "/events"
	asks := func(ids ...string) {
		t.Helper()
		var evs []session.Event
		for _, id := range ids {
			evs = append(evs, ev(t, session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: id, Name: "bash", Input: json.RawMessage(`{}`), Verdict: "ask"}))
		}
		f.put(s.ID, evs...)
	}
	asks("toolu_1", "toolu_2")
	for name, payload := range map[string]string{
		"answer_ms a string":       `{"tool_use_id":"toolu_1","decision":"allow","answer_ms":"fast"}`,
		"answer_ms below zero":     `{"tool_use_id":"toolu_1","decision":"allow","answer_ms":-1}`,
		"answer_ms past a day":     `{"tool_use_id":"toolu_1","decision":"allow","answer_ms":86400001}`,
		"details_opened a string":  `{"tool_use_id":"toolu_1","decision":"allow","details_opened":"yes"}`,
		"suggestion_shown a count": `{"tool_use_id":"toolu_1","decision":"allow","suggestion_shown":1}`,
		"a surface not listed":     `{"tool_use_id":"toolu_1","decision":"allow","surface":"chat"}`,
		"an unknown field":         `{"tool_use_id":"toolu_1","decision":"allow","answered_at":"now"}`,
	} {
		a := f.do(http.MethodPost, path, "alice", confirmBody(payload))
		if a.code() != CodeInvalidRequest || !strings.Contains(string(a.body), "This confirmation has a field the server cannot read") {
			t.Errorf("%s: %d %s", name, a.status, a.body)
		}
	}
	all := `{"tool_use_id":"toolu_1","decision":"deny","answer_ms":86400000,"details_opened":false,"suggestion_shown":true,"surface":"notification"}`
	if a := f.do(http.MethodPost, path, "alice", confirmBody(all)); a.status != http.StatusOK {
		t.Fatalf("every field: %d %s", a.status, a.body)
	}
	log := f.log(s.ID)
	var p session.UserToolConfirmation
	if err := log[len(log)-1].Decode(&p); err != nil || p.AnswerMS == nil || *p.AnswerMS != 86400000 || p.DetailsOpened == nil || *p.DetailsOpened ||
		p.SuggestionShown == nil || !*p.SuggestionShown || p.Surface != session.SurfaceNotification {
		t.Fatalf("recorded %+v, %v", p, err)
	}
	if a := f.do(http.MethodPost, path, "alice", confirmBody(`{"tool_use_id":"toolu_2","decision":"allow"}`)); a.status != http.StatusOK {
		t.Fatalf("none of them: %d %s", a.status, a.body)
	}
	log = f.log(s.ID)
	p = session.UserToolConfirmation{}
	if err := log[len(log)-1].Decode(&p); err != nil || p.AnswerMS != nil || p.DetailsOpened != nil || p.SuggestionShown != nil || p.Surface != "" {
		t.Fatalf("recorded %+v, %v", p, err)
	}
}

// TestDenialLimits (spec 101): the denials an allow of a create or a fork
// names are the session's, which lower the installation's limits and
// cannot raise them; a limit below 1 is one toposd cannot apply.
func TestDenialLimits(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	l := f.level(func(req authz.Request) *authorizer.WireLimits {
		switch req.Action {
		case authorizer.ActionSessionCreate:
			return &authorizer.WireLimits{Denials: &authorizer.WireDenials{Consecutive: new(2), Total: new(50)}}
		case authorizer.ActionSessionFork:
			return &authorizer.WireLimits{Denials: &authorizer.WireDenials{Total: new(5)}}
		}
		return &authorizer.WireLimits{Denials: &authorizer.WireDenials{Consecutive: new(1)}}
	})
	s := f.create("alice", "reviewer")
	if s.Denials == nil || *s.Denials != (session.DenialLimits{Consecutive: 2, Total: 50}) {
		t.Fatalf("created with %+v", s.Denials)
	}
	installation := session.DenialLimits{Consecutive: session.DefaultDenialsConsecutive, Total: session.DefaultDenialsTotal}
	if got := installation.Lower(s.Denials); got != (session.DenialLimits{Consecutive: 2, Total: 20}) {
		t.Fatalf("the limits that apply %+v", got)
	}
	if got := (session.DenialLimits{Consecutive: 0, Total: 20}).Lower(s.Denials); got != (session.DenialLimits{Consecutive: 2, Total: 20}) {
		t.Fatalf("over a limit the installation turned off %+v", got)
	}
	// A send's denials are not read.
	if a := f.send(s.ID, "Go on."); a.status != http.StatusOK {
		t.Fatalf("a send: %d %s", a.status, a.body)
	}
	if h := f.header(s.ID); *h.Denials != *s.Denials {
		t.Fatalf("a send changed the denials to %+v", h.Denials)
	}
	end := f.turn(s.ID, 1, "Done.", 1)
	a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", `{"at_seq":`+strconv.FormatUint(end, 10)+`}`)
	var fork session.Session
	a.decode(t, &fork)
	if a.status != http.StatusCreated || fork.Denials == nil || *fork.Denials != (session.DenialLimits{Total: 5}) {
		t.Fatalf("a fork: %d %+v", a.status, fork.Denials)
	}
	l.to(&authorizer.WireLimits{Denials: &authorizer.WireDenials{Consecutive: new(0)}})
	if a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`); a.status == http.StatusCreated || !strings.Contains(string(a.body), "authorizer_unavailable") {
		t.Fatalf("a limit turned off: %d %s", a.status, a.body)
	}
}

// TestTheBoundaryFollowsTheLatestAllow (spec 101): a create records the
// boundary its allow names; a send's allow that names another appends it
// on session.network_changed by the authorizer, moving no network, and the
// header takes it; one that names the same or none appends nothing.
func TestTheBoundaryFollowsTheLatestAllow(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	l := f.level(func(req authz.Request) *authorizer.WireLimits {
		return &authorizer.WireLimits{Boundary: &session.Boundary{Hosts: []string{"Example.com"}}}
	})
	s := f.create("alice", "reviewer")
	if s.Boundary == nil || !slices.Equal(s.Boundary.Hosts, []string{"example.com"}) {
		t.Fatalf("created with %+v", s.Boundary)
	}
	n := len(f.log(s.ID))
	if a := f.send(s.ID, "Same."); a.status != http.StatusOK || len(f.log(s.ID)) != n+1 {
		t.Fatalf("the same boundary: %d", a.status)
	}
	network := f.header(s.ID).Network
	l.to(&authorizer.WireLimits{Boundary: &session.Boundary{Hosts: []string{"*.example.org"}}})
	if a := f.send(s.ID, "Another."); a.status != http.StatusOK {
		t.Fatalf("another boundary: %d %s", a.status, a.body)
	}
	log := f.log(s.ID)
	var c session.NetworkChanged
	if m := len(log); log[m-2].Type != session.TypeNetworkChanged || log[m-2].Decode(&c) != nil || c.Moves() || c.Boundary == nil || !slices.Equal(c.Boundary.Hosts, []string{"*.example.org"}) {
		t.Fatalf("the send appended %s %+v", log[len(log)-2].Type, c)
	}
	h := f.header(s.ID)
	if !slices.Equal(h.Boundary.Hosts, []string{"*.example.org"}) || h.Network.Mode != network.Mode || !slices.Equal(h.Network.Hosts, network.Hosts) {
		t.Fatalf("the header's boundary %+v, network %+v", h.Boundary, h.Network)
	}
	l.to(nil)
	n = len(f.log(s.ID))
	if a := f.send(s.ID, "None."); a.status != http.StatusOK || len(f.log(s.ID)) != n+1 || !slices.Equal(f.header(s.ID).Boundary.Hosts, []string{"*.example.org"}) {
		t.Fatalf("no boundary: %d", a.status)
	}
	// One with the network's change rides on the same event.
	l.to(&authorizer.WireLimits{Network: &authorizer.Network{Mode: "none"}, Boundary: &session.Boundary{Hosts: []string{}}})
	if a := f.send(s.ID, "Both."); a.status != http.StatusOK {
		t.Fatalf("both: %d %s", a.status, a.body)
	}
	if h := f.header(s.ID); h.Network.Mode != "none" || h.Boundary == nil || len(h.Boundary.Hosts) != 0 {
		t.Fatalf("both: %+v %+v", h.Network, h.Boundary)
	}
}
