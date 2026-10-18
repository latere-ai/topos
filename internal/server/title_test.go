// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/session"
)

// TestATitleAsksTheInitiator: the title question asks the authorizer
// session.title as the session's initiator in the session's context,
// with session_id, the name asked as model, the model the session stands
// on and its names, and its title, and answers the model and the route
// the allow names, none for an allow that names none; a deny is an
// error, and nothing is appended (spec 083).
func TestATitleAsksTheInitiator(t *testing.T) {
	f, _ := checking(t)
	f.applyModel("quick", "name: "+quick)
	answer := pick{model: "vendor/small", route: "tier/small"}
	r := f.routeBy(func(req authz.Request) pick {
		switch req.Action {
		case authorizer.ActionSessionCreate:
			return pick{model: haiku, route: "tier/thorough"}
		case authorizer.ActionSessionTitle:
			return answer
		}
		return pick{}
	})
	s := f.create("alice", "quick")
	before := len(f.log(s.ID))
	got, err := f.api.Title(t.Context(), s.ID, "tier/titles")
	if err != nil || got != (session.ModelRef{Name: "vendor/small", Route: "tier/small"}) {
		t.Fatalf("the question answered %+v, %v", got, err)
	}
	var asked authz.Request
	for _, req := range slices.Backward(r.asked) {
		if req.Action == authorizer.ActionSessionTitle {
			asked = req
			break
		}
	}
	if claims, _ := json.Marshal(asked.Claims); asked.Subject != alice || string(claims) != `{"org_id":""}` {
		t.Fatalf("the question was asked as %s with claims %s", asked.Subject, claims)
	}
	title, _ := json.Marshal(f.header(s.ID).Title)
	want := `{"agent":"AGENT","current_model":"claude-haiku-4-5","current_model_route":"tier/thorough","current_model_via":"tier/quick","id":"SESSION","kind":"session","model":"tier/titles","owner":"https://login.example|alice","runner":"hosted","session_id":"SESSION","title":` + string(title) + `}`
	if got := wire(t, asked, s.ID, "SESSION", s.Agent.ID, "AGENT"); got != want {
		t.Fatalf("session.title asked about\n%s, want\n%s", got, want)
	}
	answer = pick{}
	if got, err := f.api.Title(t.Context(), s.ID, "tier/titles"); err != nil || got != (session.ModelRef{}) {
		t.Fatalf("an allow that names no model answered %+v, %v", got, err)
	}
	f.authz.answer = func(authz.Request) (authz.Decision, error) { return authz.Decision{Reason: "titles_off"}, nil }
	if _, err := f.api.Title(t.Context(), s.ID, "tier/titles"); err == nil {
		t.Fatal("a deny was answered")
	}
	if n := len(f.log(s.ID)); n != before {
		t.Fatalf("the question appended %d events", n-before)
	}
	if _, err := f.api.Title(t.Context(), "ses_none", "tier/titles"); err == nil {
		t.Fatal("a session that does not exist was answered")
	}
}

// TestTheTitleSettingIsReadFromTheLatestVersion: an agent's spec.title is
// read from its latest version, so a version that turns titles on names
// the sessions an earlier one started, and only agents whose latest
// version sets it are those whose sessions the pass names (spec 083).
func TestTheTitleSettingIsReadFromTheLatestVersion(t *testing.T) {
	f := newFixture(t)
	plain := f.apply("alice", "reviewer", "Review.")
	f.apply("alice", "keeper", "Keep.")
	if set, err := f.api.LatestTitle(t.Context(), plain.Status.ID); err != nil || set != nil {
		t.Fatalf("an agent without spec.title reads %+v, %v", set, err)
	}
	if ids, err := f.api.TitleAgents(t.Context()); err != nil || len(ids) != 0 {
		t.Fatalf("the agents that set a title %v, %v", ids, err)
	}
	doc := strings.Replace(agentYAML("reviewer", "Review."), "  machine:", "  title: {model: tier/quick}\n  machine:", 1)
	if a := f.do(http.MethodPut, "/v1/agents/reviewer", "alice", doc); a.status != http.StatusOK && a.status != http.StatusCreated {
		t.Fatalf("apply: %d %s", a.status, a.body)
	}
	set, err := f.api.LatestTitle(t.Context(), plain.Status.ID)
	if err != nil || set == nil || set.Model == nil || *set.Model != "tier/quick" {
		t.Fatalf("the latest version reads %+v, %v", set, err)
	}
	if ids, err := f.api.TitleAgents(t.Context()); err != nil || !slices.Equal(ids, []string{plain.Status.ID}) {
		t.Fatalf("the agents that set a title %v, %v", ids, err)
	}
	if _, err := f.api.LatestTitle(t.Context(), "agent_none"); err == nil {
		t.Fatal("an agent that does not exist was read")
	}
}

// TestATitlesRequestLeavesTheSessionQuiet: a send measures idle_seconds
// from the last request of the session's turns, not from a title's
// request, which runs on another model and warms no cache of theirs
// (spec 083).
func TestATitlesRequestLeavesTheSessionQuiet(t *testing.T) {
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	f := newFixture(t, func(o *Options) {
		o.Now = func() time.Time { return now }
		o.Objects = store.NewMemory(o.Now)
	})
	f.applyModel("quick", "name: "+quick)
	r := f.route(func(authz.Request) string { return haiku })
	var s session.Session
	f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"quick"}`).decode(t, &s)
	request := func(purpose string, at time.Time) session.Event {
		t.Helper()
		e, err := session.NewEvent(session.TypeModelRequest, session.ModelRequest{Model: haiku, Outcome: "ok", Purpose: purpose}, at)
		if err != nil {
			t.Fatal(err)
		}
		e.Turn = 1
		return e
	}
	batch := []session.Event{request("", now), request(session.PurposeTitle, now.Add(time.Minute))}
	last := f.header(s.ID).LastSeq
	session.Stamp(s.ID, last, batch)
	if _, err := f.sessions.Append(t.Context(), s.ID, last, batch); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	if a := f.send(s.ID, "Again."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if idle := r.last(t, authorizer.ActionSessionSend).Resource.Int("idle_seconds"); idle != 300 {
		t.Fatalf("idle_seconds %d, want 300", idle)
	}
}

// TestTheAPIDocumentShowsATitle: the events list says a title's request
// is a model.request of purpose title and its change is the harness's,
// and the update route says a person's title keeps the harness from it
// (spec 083).
func TestTheAPIDocumentShowsATitle(t *testing.T) {
	raw, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
			Description string `yaml:"description"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	said := map[string]string{}
	for _, methods := range doc.Paths {
		for _, op := range methods {
			said[op.OperationID] = op.Description
		}
	}
	for op, wants := range map[string][]string{
		"listEvents":    {"purpose: title", "prompt_version", "title/title-v1", "session.title_changed {by, old, new}, by the service {subject: service:harness, kind: service}", "updated_at"},
		"updateSession": {"once a person changed the title the harness names the session no more"},
	} {
		for _, want := range wants {
			if !strings.Contains(said[op], want) {
				t.Errorf("%s does not say %q", op, want)
			}
		}
	}
}
