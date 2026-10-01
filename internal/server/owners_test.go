// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// acme is the organization the tests' tokens name as name@acme, and its
// rendered subject, the owner of what its context holds.
const acme = issuer + "|acme"

// orgFixture is the API behind an authorizer that knows acme's members:
// it lets them act in acme's context and names the organization as the
// owner of an agent created there, and answers every other question as
// the owner policy does. Every question it is asked is kept.
type orgFixture struct {
	*fixture
	mu    sync.Mutex
	asked []authz.Request
	// filter, when set, is the owners every list in acme's context is
	// narrowed to.
	filter *authz.Filter
}

func newOrgFixture(t *testing.T) *orgFixture {
	t.Helper()
	f := &orgFixture{fixture: newFixture(t)}
	policy := f.authz.next
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		f.mu.Lock()
		f.asked = append(f.asked, req)
		filter := f.filter
		f.mu.Unlock()
		if org, _ := req.Claims["org_id"].(string); org == "acme" {
			d := authz.Decision{Allow: true}
			switch {
			case req.Action == authorizer.ActionAgentCreate:
				d.Limits = json.RawMessage(`{"owner":{"type":"organization","id":"acme"}}`)
			case authz.IsList(req.Action):
				d.Filter = filter
			}
			return d, nil
		}
		return policy.Authorize(context.Background(), req)
	}
	return f
}

// last is the last question of action asked.
func (f *orgFixture) last(action string) authz.Request {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range slices.Backward(f.asked) {
		if req.Action == action {
			return req
		}
	}
	f.t.Fatalf("%s was never asked", action)
	return authz.Request{}
}

// names lists the names of the agents a token lists.
func (f *orgFixture) names(token string) []string {
	f.t.Helper()
	var page struct {
		Items []v1.Agent `json:"items"`
	}
	a := f.do(http.MethodGet, "/v1/agents", token, "")
	if a.status != http.StatusOK {
		f.t.Fatalf("list: %d %s", a.status, a.body)
	}
	a.decode(f.t, &page)
	var out []string
	for _, ag := range page.Items {
		out = append(out, ag.Metadata.Name+"@"+ag.Status.ID)
	}
	slices.Sort(out)
	return out
}

// sessions lists the ids of the sessions a token lists with query q.
func (f *orgFixture) sessions(token, q string) []string {
	f.t.Helper()
	var page struct {
		Items []session.Session `json:"items"`
	}
	a := f.do(http.MethodGet, "/v1/sessions"+q, token, "")
	if a.status != http.StatusOK {
		f.t.Fatalf("list sessions: %d %s", a.status, a.body)
	}
	a.decode(f.t, &page)
	var out []string
	for _, s := range page.Items {
		out = append(out, s.ID)
	}
	slices.Sort(out)
	return out
}

// TestAnOrganizationHoldsItsAgents: an apply in acme's context creates
// acme's agent, held under acme's subject as an organization's; another
// member's apply of the name updates it; the person's own agent of the
// name stays apart, and each context lists and reads its own.
func TestAnOrganizationHoldsItsAgents(t *testing.T) {
	f := newOrgFixture(t)
	ours := f.apply("carol@acme", "reviewer", "Review for acme.")
	stored, err := f.objects.Agent(t.Context(), ours.Status.ID)
	if err != nil || stored.Owner != acme || stored.OwnerType != store.OwnerOrganization {
		t.Fatalf("acme's agent is held as %+v, %v", stored, err)
	}
	if again := f.apply("dave@acme", "reviewer", "Review for acme, closer."); again.Status.ID != ours.Status.ID || again.Status.Version != 2 {
		t.Fatalf("another member's apply: %+v", again.Status)
	}
	mine := f.apply("carol", "reviewer", "Review for me.")
	if mine.Status.ID == ours.Status.ID {
		t.Fatal("carol's own agent is acme's")
	}
	if a, err := f.objects.Agent(t.Context(), mine.Status.ID); err != nil || a.Owner != issuer+"|carol" || a.OwnerType != store.OwnerUser {
		t.Fatalf("carol's own agent is held as %+v, %v", a, err)
	}
	if got := f.names("carol@acme"); !slices.Equal(got, []string{"reviewer@" + ours.Status.ID}) {
		t.Fatalf("acme's context lists %v", got)
	}
	if got := f.names("carol"); !slices.Equal(got, []string{"reviewer@" + mine.Status.ID}) {
		t.Fatalf("carol's context lists %v", got)
	}
	for token, id := range map[string]string{"dave@acme": ours.Status.ID, "carol": mine.Status.ID} {
		var a v1.Agent
		f.do(http.MethodGet, "/v1/agents/reviewer", token, "").decode(t, &a)
		if a.Status.ID != id {
			t.Fatalf("%s reads reviewer as %s, want %s", token, a.Status.ID, id)
		}
	}
}

// TestNamesAreTheContexts: every name a route reads is read in the
// caller's context: a version, an archive, a session's create and a
// session list's agent filter, and an agent's subagent reference.
func TestNamesAreTheContexts(t *testing.T) {
	f := newOrgFixture(t)
	ours := f.apply("carol@acme", "helper", "Help acme.")
	mine := f.apply("carol", "helper", "Help me.")
	var lead v1.Agent
	a := f.do(http.MethodPut, "/v1/agents/lead", "carol@acme", agentYAML("lead", "Lead.")+"  subagents: [{name: h, agent: helper}]\n")
	if a.status != http.StatusCreated {
		t.Fatalf("lead: %d %s", a.status, a.body)
	}
	a.decode(t, &lead)
	if got := lead.Spec.Subagents[0].Agent; got != ours.Status.ID+"@1" {
		t.Fatalf("acme's lead pins %s, want acme's helper", got)
	}
	var v v1.Agent
	f.do(http.MethodGet, "/v1/agents/helper/versions/1", "carol@acme", "").decode(t, &v)
	if v.Spec.Instructions != "Help acme." {
		t.Fatalf("acme's version reads %q", v.Spec.Instructions)
	}
	s := f.create("carol@acme", "helper")
	if s.Agent.ID != ours.Status.ID {
		t.Fatalf("a session in acme's context runs %s, want acme's helper", s.Agent.ID)
	}
	own := f.create("carol", "helper")
	if own.Agent.ID != mine.Status.ID {
		t.Fatalf("a session in carol's context runs %s, want her helper", own.Agent.ID)
	}
	if got := f.sessions("carol@acme", "?agent=helper"); !slices.Equal(got, []string{s.ID}) {
		t.Fatalf("acme's helper's sessions %v", got)
	}
	if got := f.sessions("carol", "?agent=helper"); !slices.Equal(got, []string{own.ID}) {
		t.Fatalf("carol's helper's sessions %v", got)
	}
	if got := f.sessions("carol@acme", "?agent="+mine.Status.ID); len(got) != 0 {
		t.Fatalf("acme's context lists the sessions of carol's own agent by its id: %v", got)
	}
	if a := f.do(http.MethodPost, "/v1/agents/helper/archive", "carol@acme", `{"permanent":true}`); a.status != http.StatusOK {
		t.Fatalf("archive: %d %s", a.status, a.body)
	}
	for id, archived := range map[string]bool{ours.Status.ID: true, mine.Status.ID: false} {
		if a, err := f.objects.Agent(t.Context(), id); err != nil || (a.ArchivedAt != nil) != archived {
			t.Fatalf("agent %s archived %v: %+v, %v", id, archived, a, err)
		}
	}
}

// TestTheQuestionsNameTheOrganization: an organization's agent is its
// owner in every question, as {type, id}; a create names the context's
// organization and a person's none; a session's create and fork name it
// as agent_owner; a session list names its context.
func TestTheQuestionsNameTheOrganization(t *testing.T) {
	f := newOrgFixture(t)
	org := map[string]any{"type": "organization", "id": "acme"}
	ours := f.apply("carol@acme", "reviewer", "Review.")
	if got := f.last(authorizer.ActionAgentCreate).Resource.Fields["owner"]; !equalJSON(got, org) {
		t.Fatalf("acme's create names %v", got)
	}
	f.apply("carol", "solo", "Alone.")
	if _, named := f.last(authorizer.ActionAgentCreate).Resource.Fields["owner"]; named {
		t.Fatal("a personal create names an owner")
	}
	f.apply("dave@acme", "reviewer", "Review again.")
	if got := f.last(authorizer.ActionAgentUpdate).Resource.Fields["owner"]; !equalJSON(got, org) {
		t.Fatalf("acme's update names %v", got)
	}
	f.do(http.MethodGet, "/v1/agents/"+ours.Status.ID, "dave@acme", "")
	if got := f.last(authorizer.ActionAgentRead).Resource.Fields["owner"]; !equalJSON(got, org) {
		t.Fatalf("acme's read names %v", got)
	}
	s := f.create("dave@acme", "reviewer")
	if got := f.last(authorizer.ActionSessionCreate).Resource.Fields["agent_owner"]; !equalJSON(got, org) {
		t.Fatalf("acme's session names %v", got)
	}
	f.turn(s.ID, 1, "Done.", 1)
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "dave@acme", ""); a.status != http.StatusCreated {
		t.Fatalf("fork: %d %s", a.status, a.body)
	}
	if got := f.last(authorizer.ActionSessionFork).Resource.Fields["agent_owner"]; !equalJSON(got, org) {
		t.Fatalf("acme's fork names %v", got)
	}
	f.sessions("dave@acme", "")
	if got := f.last(authorizer.ActionSessionList).Resource.Fields["agent_owner"]; !equalJSON(got, org) {
		t.Fatalf("acme's session list names %v", got)
	}
	f.sessions("carol", "")
	if got := f.last(authorizer.ActionSessionList).Resource.Fields["agent_owner"]; got != issuer+"|carol" {
		t.Fatalf("carol's session list names %v", got)
	}
	if a := f.do(http.MethodPost, "/v1/agents/reviewer/archive", "carol@acme", `{"permanent":true}`); a.status != http.StatusOK {
		t.Fatalf("archive: %d %s", a.status, a.body)
	}
	if got := f.last(authorizer.ActionAgentArchive).Resource.Fields["owner"]; !equalJSON(got, org) {
		t.Fatalf("acme's archive names %v", got)
	}
}

// TestListsHoldTheirContext: a list holds what its context holds, its
// agents and their sessions, whatever owners the authorizer names: an
// answer with no narrowing lists the context's alone, and one naming
// owners outside the context lists none of theirs.
func TestListsHoldTheirContext(t *testing.T) {
	f := newOrgFixture(t)
	ours := f.apply("carol@acme", "reviewer", "Review.")
	f.apply("carol", "solo", "Alone.")
	carols := f.create("carol@acme", "reviewer")
	daves := f.create("dave@acme", "reviewer")
	f.create("carol", "solo")
	if got := f.names("dave@acme"); !slices.Equal(got, []string{"reviewer@" + ours.Status.ID}) {
		t.Fatalf("acme's list with no narrowing %v", got)
	}
	want := []string{carols.ID, daves.ID}
	slices.Sort(want)
	if got := f.sessions("dave@acme", ""); !slices.Equal(got, want) {
		t.Fatalf("acme's sessions with no narrowing %v, want %v", got, want)
	}
	// The authorizer narrows a member to their own sessions within it.
	f.mu.Lock()
	f.filter = &authz.Filter{Owners: []string{issuer + "|dave"}}
	f.mu.Unlock()
	if got := f.sessions("dave@acme", ""); !slices.Equal(got, []string{daves.ID}) {
		t.Fatalf("dave's sessions in acme %v", got)
	}
	// Owners outside the context list nothing of theirs.
	if got := f.names("dave@acme"); len(got) != 0 {
		t.Fatalf("a list naming an owner outside acme %v", got)
	}
	f.mu.Lock()
	f.filter = &authz.Filter{Owners: []string{acme, issuer + "|carol"}}
	f.mu.Unlock()
	if got := f.names("carol@acme"); !slices.Equal(got, []string{"reviewer@" + ours.Status.ID}) {
		t.Fatalf("a list naming carol beside acme %v", got)
	}
	// A context that holds no agent lists no session.
	if got := f.sessions("erin@empty", ""); len(got) != 0 {
		t.Fatalf("an empty context lists %v", got)
	}
}

// equalJSON compares two values as JSON renders them.
func equalJSON(a, b any) bool {
	x, err := json.Marshal(a)
	if err != nil {
		return false
	}
	y, err := json.Marshal(b)
	return err == nil && string(x) == string(y)
}

// TestASessionListReadsEveryPageOfTheContextsAgents: a context holding
// more agents than one page of the agent list still lists the sessions
// of each of them.
func TestASessionListReadsEveryPageOfTheContextsAgents(t *testing.T) {
	f := newOrgFixture(t)
	for i := range session.DefaultListLimit + 1 {
		f.apply("carol@acme", "agent-"+strconv.Itoa(i), "Work.")
	}
	last := f.create("carol@acme", "agent-"+strconv.Itoa(session.DefaultListLimit))
	if got := f.sessions("carol@acme", ""); !slices.Equal(got, []string{last.ID}) {
		t.Fatalf("the sessions of the last page's agent %v", got)
	}
}
