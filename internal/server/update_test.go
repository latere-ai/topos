// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/hosted"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// modelEvents are a session's session.model_changed events.
func (f *fixture) modelEvents(id string) []session.ModelChanged {
	f.t.Helper()
	evs, err := f.sessions.Events(f.t.Context(), id, 1, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []session.ModelChanged
	for _, e := range evs {
		if e.Type != session.TypeModelChanged {
			continue
		}
		var p session.ModelChanged
		if err := e.Decode(&p); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// TestASessionSwitchesItsModel: a switch is checked by the rule a
// session's create checks its agent's model by, asked of the authorizer as
// session.update with the session and the model, and recorded as
// session.model_changed from the agent's model, which the answer's
// model carries; a switch back names the switched model as the old one,
// and a switch to the model the session runs appends nothing.
func TestASessionSwitchesItsModel(t *testing.T) {
	var checked []v1.AgentModel
	f := newFixture(t, func(o *Options) {
		cat, err := models.Embedded()
		if err != nil {
			t.Fatal(err)
		}
		o.Runnable = func(_ context.Context, m v1.AgentModel, overlay models.Entry) error {
			checked = append(checked, m)
			_, err := cat.Resolve(m.Name, overlay)
			return err
		}
	})
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	var fields map[string]any
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionUpdate {
			fields = map[string]any{"id": req.Resource.ID, "session_id": req.Resource.String("session_id"), "model": req.Resource.String("model"), "owner": req.Resource.String("owner")}
		}
		return (&auth.OwnerPolicy{}).Authorize(t.Context(), req)
	}
	const sonnet = "anthropic/claude-sonnet-4-5"
	a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+sonnet+`"}}`)
	var got session.Session
	if a.status != http.StatusOK {
		t.Fatalf("switch: %d %s", a.status, a.body)
	}
	a.decode(t, &got)
	if got.Model == nil || got.Model.Name != sonnet {
		t.Fatalf("the answer's model is %+v", got.Model)
	}
	if fields["id"] != s.ID || fields["session_id"] != s.ID || fields["model"] != sonnet || fields["owner"] != alice {
		t.Fatalf("session.update asked about %v", fields)
	}
	if len(checked) != 2 || checked[0].Name != "claude-haiku-4-5" || checked[1].Name != sonnet {
		t.Fatalf("the create and the switch checked %+v", checked)
	}
	changes := f.modelEvents(s.ID)
	if len(changes) != 1 || changes[0].Old.Name != "claude-haiku-4-5" || changes[0].New.Name != sonnet || changes[0].By.Subject != alice {
		t.Fatalf("session.model_changed %+v", changes)
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+sonnet+`"}}`); a.status != http.StatusOK || len(f.modelEvents(s.ID)) != 1 {
		t.Fatalf("a switch to the model the session runs: %d %s, %d events", a.status, a.body, len(f.modelEvents(s.ID)))
	}
	// A switch back to the agent's own model checks it as the agent names
	// it, figures and all.
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"claude-haiku-4-5"}}`); a.status != http.StatusOK {
		t.Fatalf("switch back: %d %s", a.status, a.body)
	}
	if changes := f.modelEvents(s.ID); len(changes) != 2 || changes[1].Old.Name != sonnet || changes[1].New.Name != "claude-haiku-4-5" {
		t.Fatalf("session.model_changed %+v", changes)
	}
	if got, err := f.sessions.Get(t.Context(), s.ID); err != nil || got.Model == nil || got.Model.Name != "claude-haiku-4-5" {
		t.Fatalf("the stored header's model is %+v, %v", got.Model, err)
	}
}

// TestAModelSwitchIsRefused: an unknown model is model_unknown and a
// denied session.update forbidden with the authorizer's reason, both
// leaving the session's model and log as they were; a caller who may
// not read the session hears not_found, an ended session is conflict,
// and a body that names no model or another field is invalid_request.
func TestAModelSwitchIsRefused(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	before, err := f.sessions.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func(what string) {
		t.Helper()
		after, err := f.sessions.Get(t.Context(), s.ID)
		if err != nil || after.Model != nil || after.LastSeq != before.LastSeq || len(f.modelEvents(s.ID)) != 0 {
			t.Fatalf("%s changed the session: model %+v, last seq %d from %d, %v", what, after.Model, after.LastSeq, before.LastSeq, err)
		}
	}
	a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"no-such-model"}}`)
	if a.status != http.StatusUnprocessableEntity || a.code() != models.CodeUnknown {
		t.Fatalf("an unknown model: %d %s", a.status, a.body)
	}
	unchanged("an unknown model")

	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionUpdate {
			return authz.Decision{Reason: "model_not_offered"}, nil
		}
		return (&auth.OwnerPolicy{}).Authorize(t.Context(), req)
	}
	a = f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"anthropic/claude-sonnet-4-5"}}`)
	var refusal struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	a.decode(t, &refusal)
	if a.status != http.StatusForbidden || refusal.Error.Code != auth.CodeForbidden || refusal.Error.Details.Reason != "model_not_offered" {
		t.Fatalf("a denied switch: %d %s", a.status, a.body)
	}
	unchanged("a denied switch")
	f.authz.answer = nil

	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "bob", `{"model":{"name":"no-such-model"}}`); a.status != http.StatusNotFound {
		t.Fatalf("another subject's switch: %d %s", a.status, a.body)
	}
	for _, body := range []string{`{}`, `{"model":{"name":""}}`, `{"model":{"name":" x"}}`, `{"model":{"name":"x"},"title":"t"}`} {
		if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", body); a.code() != CodeInvalidRequest {
			t.Fatalf("%s: %d %s", body, a.status, a.body)
		}
	}
	unchanged("an invalid body")
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/end", "alice", `{"reason":"completed"}`); a.status != http.StatusOK {
		t.Fatalf("end: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"anthropic/claude-sonnet-4-5"}}`); a.code() != CodeConflict {
		t.Fatalf("a switch of an ended session: %d %s", a.status, a.body)
	}
}

// TestASwitchRunsWhatACreateRuns: on a hosted installation whose runners
// read Lux's doors with each session's own key, and which holds no
// TOPOS_MODELS_KEY, a session is created of an agent whose model the
// embedded catalog does not name, and a session switches to such a
// model: the one check both ask routes it through Lux's OpenAI door, as
// the runner does, rather than refusing it unread.
func TestASwitchRunsWhatACreateRuns(t *testing.T) {
	stub := luxstub.New(t)
	runnable, err := hosted.Runnable(hosted.Options{ModelsURL: stub.URL(), Doors: models.Doors{"anthropic": stub.URL() + "/anthropic", "openai": stub.URL() + "/openai"}})
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, func(o *Options) { o.Runnable = runnable })
	const doorOnly = "deepseek/deepseek-v4-flash-0731"
	cat, err := models.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if _, known := cat.Lookup(doorOnly); known {
		t.Fatalf("the embedded catalog names %s", doorOnly)
	}
	doc := strings.Replace(agentYAML("routed", "Route."), "model: {name: claude-haiku-4-5}", "model: {name: "+doorOnly+"}", 1)
	if a := f.do(http.MethodPut, "/v1/agents/routed", "alice", doc); a.status != http.StatusCreated {
		t.Fatalf("apply: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"routed"}`); a.status != http.StatusCreated {
		t.Fatalf("a create of an agent whose model only the door names: %d %s", a.status, a.body)
	}
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+doorOnly+`"}}`); a.status != http.StatusOK {
		t.Fatalf("a switch to a model only the door names: %d %s", a.status, a.body)
	}
	if changes := f.modelEvents(s.ID); len(changes) != 1 || changes[0].New.Name != doorOnly {
		t.Fatalf("session.model_changed %+v", changes)
	}
}
