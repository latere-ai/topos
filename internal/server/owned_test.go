// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/test/stubs/idpstub"
)

// service is a subject the tests' authorizer lets list one named owner's
// objects and delete them, as an installation's authorizer may let a
// service account clean up after a closed account.
const service = issuer + "|svc"

// listQuestions keeps the resource of every list question the authorizer
// was asked, and answers svc's lists with a filter naming bob alone.
type listQuestions struct {
	mu    sync.Mutex
	asked []authz.Request
}

func (l *listQuestions) take() []authz.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.asked
	l.asked = nil
	return out
}

// pageOf reads the names of a list page's items.
func pageOf(t *testing.T, a answer) []string {
	t.Helper()
	if a.status != http.StatusOK {
		t.Fatalf("a list answered %d %s", a.status, a.body)
	}
	var page struct {
		Items []struct {
			Metadata v1.ObjectMeta `json:"metadata"`
		} `json:"items"`
	}
	a.decode(t, &page)
	names := []string{}
	for _, it := range page.Items {
		names = append(names, it.Metadata.Name)
	}
	slices.Sort(names)
	return names
}

// TestTheListsNameAnOwner is spec 074's first two criteria: a trigger
// and an agent list that name an owner hold that owner's objects when the
// allow names no filter or names the owner, in place of the caller's
// context for agents, and are empty otherwise; the question names the
// owner; a list that names none is as before.
func TestTheListsNameAnOwner(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	f.apply("bob", "helper", "Help.")
	f.apply("bob", "writer", "Write.")
	for token, agent := range map[string]string{"alice": "reviewer", "bob": "helper"} {
		if a := f.do(http.MethodPut, "/v1/triggers/nightly", token, triggerOf(agent, "nightly", "schedule: '@daily', session: {message: x}")); a.status != http.StatusCreated {
			t.Fatalf("%s's trigger: %d %s", token, a.status, a.body)
		}
	}
	lists := &listQuestions{}
	policy := f.authz.next
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if authz.IsList(req.Action) {
			lists.mu.Lock()
			lists.asked = append(lists.asked, req)
			lists.mu.Unlock()
			if req.Subject == service {
				return authz.Decision{Allow: true, Filter: &authz.Filter{Owners: []string{bob}}}, nil
			}
		}
		return policy.Authorize(context.Background(), req)
	}
	query := func(owner string) string { return "?owner=" + url.QueryEscape(owner) }
	for _, c := range []struct {
		name, token, path string
		want              []string
		owner             string
	}{
		// The owner policy narrows a person's list to their own.
		{"a person's own agents", "alice", "/v1/agents", []string{"reviewer"}, ""},
		{"a person's agents by their own name", "alice", "/v1/agents" + query(alice), []string{"reviewer"}, alice},
		{"another person's agents", "alice", "/v1/agents" + query(bob), []string{}, bob},
		{"a person's own triggers", "alice", "/v1/triggers", []string{"nightly"}, ""},
		{"another person's triggers", "alice", "/v1/triggers" + query(bob), []string{}, bob},
		// An allow whose filter names bob lists bob's agents in place of
		// the caller's context, and none of anyone else's.
		{"the named owner's agents", "svc", "/v1/agents" + query(bob), []string{"helper", "writer"}, bob},
		{"an owner the filter leaves out", "svc", "/v1/agents" + query(alice), []string{}, alice},
		{"the caller's own context", "svc", "/v1/agents", []string{}, ""},
		{"the named owner's triggers", "svc", "/v1/triggers" + query(bob), []string{"nightly"}, bob},
		{"a trigger owner the filter leaves out", "svc", "/v1/triggers" + query(alice), []string{}, alice},
		// An allow with no filter, the admin's, lists the named owner's
		// alone.
		{"every trigger", "root", "/v1/triggers", []string{"nightly", "nightly"}, ""},
		{"one owner's triggers for the admin", "root", "/v1/triggers" + query(alice), []string{"nightly"}, alice},
		{"one owner's agents for the admin", "root", "/v1/agents" + query(bob), []string{"helper", "writer"}, bob},
		{"an empty owner is no filter", "alice", "/v1/agents?owner=", []string{"reviewer"}, ""},
	} {
		lists.take()
		if got := pageOf(t, f.do(http.MethodGet, c.path, c.token, "")); !slices.Equal(got, c.want) {
			t.Errorf("%s: listed %v, want %v", c.name, got, c.want)
		}
		asked := lists.take()
		if len(asked) != 1 {
			t.Fatalf("%s: asked %d list questions", c.name, len(asked))
		}
		owner, named := asked[0].Resource.Fields["owner"]
		if c.owner == "" && named || c.owner != "" && owner != c.owner {
			t.Errorf("%s: the question named the owner %v (%v)", c.name, owner, named)
		}
	}
	// bob's own list pages through the named owner's agents as it pages
	// through its context's.
	a := f.do(http.MethodGet, "/v1/agents"+query(bob)+"&limit=1", "svc", "")
	if got := pageOf(t, a); len(got) != 1 || !strings.Contains(a.header.Get("Link"), "owner=") {
		t.Fatalf("the first page of bob's agents: %v, Link %q", got, a.header.Get("Link"))
	}
}

// TestDeleteAnAgent is spec 074's third criterion: the delete asks
// agent.delete of the agent with its name and owner, removes it and every
// version, answers 204 and then not_found, frees the name for a new
// agent, and deletes an archived agent; a caller the authorizer refuses
// changes nothing.
func TestDeleteAnAgent(t *testing.T) {
	f := newFixture(t)
	first := f.apply("alice", "reviewer", "Review.")
	f.apply("alice", "reviewer", "Review twice.")
	id := first.Status.ID
	if a := f.do(http.MethodDelete, "/v1/agents/"+id, "bob", ""); a.code() != CodeNotFound {
		t.Fatalf("another person's delete: %d %s", a.status, a.body)
	}
	if _, err := f.objects.Agent(t.Context(), id); err != nil {
		t.Fatalf("a refused delete removed the agent: %v", err)
	}
	var asked []authz.Request
	policy := f.authz.next
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		asked = append(asked, req)
		return policy.Authorize(context.Background(), req)
	}
	if a := f.do(http.MethodDelete, "/v1/agents/reviewer", "alice", ""); a.status != http.StatusNoContent || len(a.body) != 0 {
		t.Fatalf("delete: %d %s", a.status, a.body)
	}
	f.authz.answer = nil
	if len(asked) != 1 || asked[0].Action != authorizer.ActionAgentDelete || asked[0].Resource.ID != id ||
		asked[0].Resource.Fields["name"] != "reviewer" || asked[0].Resource.Fields["owner"] != alice {
		t.Fatalf("the delete asked %+v", asked)
	}
	for n := 1; n <= 2; n++ {
		if _, err := f.objects.Version(t.Context(), id, n); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("version %d after the delete: %v", n, err)
		}
	}
	for _, path := range []string{"/v1/agents/" + id, "/v1/agents/reviewer", "/v1/agents/" + id + "/versions"} {
		if a := f.do(http.MethodGet, path, "alice", ""); a.code() != CodeNotFound {
			t.Errorf("GET %s after the delete: %d %s", path, a.status, a.body)
		}
	}
	if a := f.do(http.MethodDelete, "/v1/agents/"+id, "alice", ""); a.code() != CodeNotFound {
		t.Fatalf("a second delete: %d %s", a.status, a.body)
	}
	if again := f.apply("alice", "reviewer", "Review."); again.Status.ID == id || again.Status.Version != 1 {
		t.Fatalf("the freed name applied as %s version %d", again.Status.ID, again.Status.Version)
	}
	f.apply("alice", "retired", "Retire.")
	if a := f.do(http.MethodPost, "/v1/agents/retired/archive", "alice", `{"permanent":true}`); a.status != http.StatusOK {
		t.Fatalf("archive: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodDelete, "/v1/agents/retired", "alice", ""); a.status != http.StatusNoContent {
		t.Fatalf("the delete of an archived agent: %d %s", a.status, a.body)
	}
}

// TestAnAgentWithSessionsIsNotDeleted is spec 074's fourth criterion: an
// agent the store holds a session of, of any status, archived included,
// is conflict and stays; once its sessions are deleted it is deleted.
func TestAnAgentWithSessionsIsNotDeleted(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	idle := f.create("alice", "reviewer")
	ended := f.create("alice", "reviewer")
	if a := f.do(http.MethodPost, "/v1/sessions/"+ended.ID+"/end", "alice", `{"reason":"completed"}`); a.status != http.StatusOK {
		t.Fatalf("end: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+ended.ID+"/archive", "alice", ""); a.status != http.StatusOK {
		t.Fatalf("archive: %d %s", a.status, a.body)
	}
	for _, id := range []string{idle.ID, ended.ID} {
		if a := f.do(http.MethodDelete, "/v1/agents/reviewer", "alice", ""); a.status != http.StatusConflict || a.code() != CodeConflict {
			t.Fatalf("a delete while a session is left: %d %s", a.status, a.body)
		}
		if _, err := f.objects.AgentByName(t.Context(), alice, "reviewer"); err != nil {
			t.Fatalf("a refused delete removed the agent: %v", err)
		}
		if a := f.do(http.MethodDelete, "/v1/sessions/"+id, "alice", ""); a.status != http.StatusNoContent {
			t.Fatalf("the session's delete: %d %s", a.status, a.body)
		}
	}
	if a := f.do(http.MethodDelete, "/v1/agents/reviewer", "alice", ""); a.status != http.StatusNoContent {
		t.Fatalf("the delete once no session is left: %d %s", a.status, a.body)
	}
}

// TestDeleteAnAgentsIdentity is spec 074's fifth criterion: the identity
// is archived and then disabled, both confirmed permanent; one the
// provider deleted with its owner is gone and the delete succeeds; a
// provider that refuses or does not answer the archive leaves the agent;
// a disable that fails is caught by the reconcile pass.
func TestDeleteAnAgentsIdentity(t *testing.T) {
	f := newIdentityFixture(t)
	a := f.apply("alice", "reviewer", "Review.")
	if d := f.do(http.MethodDelete, "/v1/agents/reviewer", "alice", ""); d.status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", d.status, d.body)
	}
	if got := f.idp.Agents()[0]; got.Subject != a.Status.Identity || got.Status != idpstub.StatusDisabled {
		t.Fatalf("the deleted agent's identity: %+v", got)
	}
	var ops []string
	for _, r := range f.idp.Requests() {
		if r.Op != idpstub.OpArchive && r.Op != idpstub.OpDisable {
			continue
		}
		ops = append(ops, r.Op)
		if !strings.Contains(string(r.Body), `"permanent":true`) {
			t.Fatalf("%s without the confirmation: %s", r.Op, r.Body)
		}
	}
	if !slices.Equal(ops, []string{idpstub.OpArchive, idpstub.OpDisable}) {
		t.Fatalf("the provider was asked %v", ops)
	}

	// The provider deleted alice's identities with her: the agent is
	// deleted all the same.
	f.apply("alice", "helper", "Help.")
	if n := f.idp.DeleteOwner(idpstub.Owner{Type: "user", ID: "alice"}); n != 2 {
		t.Fatalf("the provider deleted %d identities", n)
	}
	if d := f.do(http.MethodDelete, "/v1/agents/helper", "alice", ""); d.status != http.StatusNoContent {
		t.Fatalf("the delete of an agent whose identity is gone: %d %s", d.status, d.body)
	}
	if _, err := f.objects.AgentByName(t.Context(), alice, "helper"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the agent after its delete: %v", err)
	}

	// A refused or unanswered archive leaves the agent as it was.
	f.apply("alice", "linter", "Lint.")
	for _, c := range []struct {
		f    idpstub.Failure
		code string
	}{
		{idpstub.Failure{Status: http.StatusForbidden, Code: "owner_not_served"}, CodeIdentityRefused},
		{idpstub.Failure{Status: http.StatusBadGateway, Code: "bad_gateway"}, CodeIdentityUnavailable},
	} {
		f.idp.Fail(idpstub.OpArchive, c.f)
		if d := f.do(http.MethodDelete, "/v1/agents/linter", "alice", ""); d.code() != c.code {
			t.Fatalf("%+v: %d %s", c.f, d.status, d.body)
		}
		if _, err := f.objects.AgentByName(t.Context(), alice, "linter"); err != nil {
			t.Fatalf("%+v removed the agent: %v", c.f, err)
		}
	}

	// A disable that fails leaves the identity archived for the reconcile
	// pass, which disables it since the store holds no agent of it.
	keeper := f.apply("alice", "keeper", "Keep.")
	f.idp.Fail(idpstub.OpDisable, idpstub.Failure{Status: http.StatusBadGateway, Code: "bad_gateway"})
	if d := f.do(http.MethodDelete, "/v1/agents/keeper", "alice", ""); d.status != http.StatusNoContent {
		t.Fatalf("a delete whose disable failed: %d %s", d.status, d.body)
	}
	status := func() string {
		for _, got := range f.idp.Agents() {
			if got.Subject == keeper.Status.Identity {
				return got.Status
			}
		}
		return ""
	}
	if got := status(); got != idpstub.StatusArchived {
		t.Fatalf("the identity after a failed disable is %q", got)
	}
	if err := f.api.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != idpstub.StatusDisabled {
		t.Fatalf("the identity after the reconcile pass is %q", got)
	}
}
