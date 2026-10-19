// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// listedIDs is the ids of the sessions a list answers alice.
func (f *fixture) listedIDs(path string) []string {
	f.t.Helper()
	a := f.do(http.MethodGet, path, "alice", "")
	if a.status != http.StatusOK {
		f.t.Fatalf("GET %s: %d %s", path, a.status, a.body)
	}
	var page struct {
		Items []session.Session `json:"items"`
	}
	a.decode(f.t, &page)
	ids := make([]string, len(page.Items))
	for i, s := range page.Items {
		ids[i] = s.ID
	}
	return ids
}

// TestARolloutsRunsAreLeftOutOfTheList: a create naming a rollout holds
// it, session.create carries it, and the list, the summary and the
// search leave the run out unless their rollout parameter names that
// rollout or any; a rollout id outside the rule is refused at create and
// in a list.
func TestARolloutsRunsAreLeftOutOfTheList(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "helper", "Answer.")
	var asked []authz.Request
	next := f.authz.next
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionCreate {
			asked = append(asked, req)
		}
		return next.Authorize(t.Context(), req)
	}
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"helper","rollout":"cel_01.smoke"}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create of a run: %d %s", a.status, a.body)
	}
	var run session.Session
	a.decode(t, &run)
	if run.Rollout != "cel_01.smoke" || f.header(run.ID).Rollout != "cel_01.smoke" {
		t.Fatalf("the run answers rollout %q and stores %q", run.Rollout, f.header(run.ID).Rollout)
	}
	if len(asked) != 1 || asked[0].Resource.String("rollout") != "cel_01.smoke" {
		t.Fatalf("session.create carried %v", asked)
	}
	own := f.create("alice", "helper")
	for _, c := range []struct {
		path string
		want []string
	}{
		{"/v1/sessions", []string{own.ID}},
		{"/v1/sessions?rollout=cel_01.smoke", []string{run.ID}},
		{"/v1/sessions?rollout=cel_02.smoke", []string{}},
		{"/v1/sessions?rollout=any", []string{own.ID, run.ID}},
	} {
		if got := f.listedIDs(c.path); !slices.Equal(got, c.want) {
			t.Fatalf("GET %s listed %v, want %v", c.path, got, c.want)
		}
	}
	for path, want := range map[string]int{"/v1/sessions/summary": 1, "/v1/sessions/summary?rollout=any": 2} {
		a := f.do(http.MethodGet, path, "alice", "")
		var sum session.Summary
		a.decode(t, &sum)
		if n := sum.Sessions.Idle + sum.Sessions.Running + sum.Sessions.Ended; a.status != http.StatusOK || n != want {
			t.Fatalf("GET %s: %d, %d sessions, want %d", path, a.status, n, want)
		}
	}
	for _, body := range []string{`{"agent":"helper","rollout":"any"}`, `{"agent":"helper","rollout":"none"}`, `{"agent":"helper","rollout":"-x"}`} {
		if a := f.do(http.MethodPost, "/v1/sessions", "alice", body); a.status != http.StatusBadRequest {
			t.Fatalf("create %s: %d %s, want 400", body, a.status, a.body)
		}
	}
	if a := f.do(http.MethodGet, "/v1/sessions?rollout=a%20b", "alice", ""); a.status != http.StatusBadRequest {
		t.Fatalf("a list by a rollout outside the rule: %d, want 400", a.status)
	}
}

// TestACreateRunsTheModelItNames: a create naming a model and a level
// asks session.create with both and starts on them; an allow naming the
// same model keeps it, and an allow naming another model or level is
// model_not_allowed with no session written.
func TestACreateRunsTheModelItNames(t *testing.T) {
	f, names := checking(t)
	f.applyModel("plain", "name: "+haiku)
	r := f.route(func(authz.Request) string { return "" })
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"plain","model":{"name":"`+sonnet+`","reasoning":"low"},"message":"Answer."}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create naming a model: %d %s", a.status, a.body)
	}
	var created struct {
		ID    string          `json:"id"`
		Model json.RawMessage `json:"model"`
	}
	a.decode(t, &created)
	if want := `{"name":"` + sonnet + `","reasoning":"low"}`; string(created.Model) != want {
		t.Fatalf("the session's model is %s, want %s", created.Model, want)
	}
	q := r.last(t, authorizer.ActionSessionCreate)
	if q.Resource.String("model") != sonnet || q.Resource.String("reasoning") != "low" || q.Resource.String("effort") != "low" {
		t.Fatalf("session.create asked about %v", q.Resource.Fields)
	}
	if !slices.Contains(*names, sonnet) {
		t.Fatalf("the create checked %v, not the model it names", *names)
	}
	// A level alone keeps the agent's model.
	a = f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"plain","model":{"reasoning":"high"}}`)
	a.decode(t, &created)
	if want := `{"name":"` + haiku + `","reasoning":"high"}`; a.status != http.StatusCreated || string(created.Model) != want {
		t.Fatalf("create naming a level: %d, model %s, want %s", a.status, created.Model, want)
	}
	// An allow naming the model asked runs it.
	r.to(sonnet)
	if a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"plain","model":{"name":"`+sonnet+`"}}`); a.status != http.StatusCreated {
		t.Fatalf("an allow naming the model asked: %d %s", a.status, a.body)
	}
	// An allow naming another model refuses the create.
	r.to(haiku)
	before := len(f.listedIDs("/v1/sessions?rollout=any"))
	a = f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"plain","model":{"name":"`+sonnet+`"}}`)
	if a.status != http.StatusForbidden || a.code() != CodeModelNotAllowed {
		t.Fatalf("an allow naming another model: %d %s, want 403 model_not_allowed", a.status, a.body)
	}
	if after := len(f.listedIDs("/v1/sessions?rollout=any")); after != before {
		t.Fatalf("a refused create left %d sessions, had %d", after, before)
	}
	for _, body := range []string{`{"agent":"plain","model":{}}`, `{"agent":"plain","model":{"reasoning":"most"}}`, `{"agent":"plain","model":{"name":" x"}}`} {
		if a := f.do(http.MethodPost, "/v1/sessions", "alice", body); a.status != http.StatusBadRequest {
			t.Fatalf("create %s: %d, want 400", body, a.status)
		}
	}
}
