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

// TestASessionChangesItsEffort: an effort change alone keeps the model,
// is asked of the authorizer with effort and without model, and records
// the old and the new effort, which the header carries; a switch of the
// model alone keeps the effort, "" returns to the agent's own, and a
// change to what the session runs appends nothing.
func TestASessionChangesItsEffort(t *testing.T) {
	f := newFixture(t)
	doc := strings.Replace(agentYAML("thinker", "Think."), "model: {name: claude-haiku-4-5}", "model: {name: claude-haiku-4-5, effort: low}", 1)
	if a := f.do(http.MethodPut, "/v1/agents/thinker", "alice", doc); a.status != http.StatusCreated {
		t.Fatalf("apply: %d %s", a.status, a.body)
	}
	s := f.create("alice", "thinker")
	var asked []map[string]any
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionUpdate {
			asked = append(asked, req.Resource.Fields)
		}
		return (&auth.OwnerPolicy{}).Authorize(t.Context(), req)
	}
	patch := func(body string) session.Session {
		t.Helper()
		a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", body)
		if a.status != http.StatusOK {
			t.Fatalf("%s: %d %s", body, a.status, a.body)
		}
		var got session.Session
		a.decode(t, &got)
		return got
	}
	const sonnet = "anthropic/claude-sonnet-4-5"
	// ref is a model as the log and the header store it, answered as the
	// API answers it, its level under reasoning.
	ref := func(name, effort string) session.ModelRef { return session.ModelRef{Name: name, Effort: effort} }
	answered := func(name, level string) session.ModelRef { return session.ModelRef{Name: name, Reasoning: level} }

	got := patch(`{"model":{"effort":"high"}}`)
	if got.Model == nil || *got.Model != answered("claude-haiku-4-5", "high") {
		t.Fatalf("an effort change answers the model %+v", got.Model)
	}
	if len(asked) != 1 || asked[0]["effort"] != "high" || asked[0]["session_id"] != s.ID {
		t.Fatalf("session.update asked about %v", asked)
	}
	if _, named := asked[0]["model"]; named {
		t.Fatalf("an effort change named the model: %v", asked[0])
	}
	got = patch(`{"model":{"name":"` + sonnet + `"}}`)
	if *got.Model != answered(sonnet, "high") {
		t.Fatalf("a switch of the model alone answers %+v", got.Model)
	}
	if _, named := asked[1]["effort"]; named || asked[1]["model"] != sonnet {
		t.Fatalf("a switch of the model asked about %v", asked[1])
	}
	got = patch(`{"model":{"effort":""}}`)
	if *got.Model != answered(sonnet, "low") || asked[2]["effort"] != "low" {
		t.Fatalf("a return to the agent's effort answers %+v, asked %v", got.Model, asked[2])
	}
	got = patch(`{"model":{"name":"claude-haiku-4-5","effort":"minimal"}}`)
	if *got.Model != answered("claude-haiku-4-5", "minimal") {
		t.Fatalf("a change of both answers %+v", got.Model)
	}
	changes := f.modelEvents(s.ID)
	want := []session.ModelChanged{
		{Old: ref("claude-haiku-4-5", "low"), New: ref("claude-haiku-4-5", "high")},
		{Old: ref("claude-haiku-4-5", "high"), New: ref(sonnet, "high")},
		{Old: ref(sonnet, "high"), New: ref(sonnet, "low")},
		{Old: ref(sonnet, "low"), New: ref("claude-haiku-4-5", "minimal")},
	}
	if len(changes) != len(want) {
		t.Fatalf("session.model_changed %+v", changes)
	}
	for i, c := range changes {
		if c.Old != want[i].Old || c.New != want[i].New || c.By.Subject != alice {
			t.Fatalf("change %d is %+v, want %+v", i, c, want[i])
		}
	}
	patch(`{"model":{"effort":"minimal"}}`)
	patch(`{"model":{"name":"claude-haiku-4-5","effort":"minimal"}}`)
	if n := len(f.modelEvents(s.ID)); n != len(want) {
		t.Fatalf("a change to what the session runs appended: %d events", n)
	}
	if stored, err := f.sessions.Get(t.Context(), s.ID); err != nil || stored.Model == nil || *stored.Model != ref("claude-haiku-4-5", "minimal") {
		t.Fatalf("the stored header's model is %+v, %v", stored.Model, err)
	}
}

// TestAnEffortChangeIsRefused: an effort outside the four values, a model
// that names nothing, and a member it does not take are invalid_request,
// and a denied effort change is forbidden; none changes the session.
func TestAnEffortChangeIsRefused(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	unchanged := func(what string) {
		t.Helper()
		after, err := f.sessions.Get(t.Context(), s.ID)
		if err != nil || after.Model != nil || len(f.modelEvents(s.ID)) != 0 {
			t.Fatalf("%s changed the session: model %+v, %v", what, after.Model, err)
		}
	}
	for _, body := range []string{`{"model":{}}`, `{"model":{"effort":"max"}}`, `{"model":{"effort":"High"}}`, `{"model":{"effort":"high","speed":"fast"}}`, `{"model":{"effort":null}}`} {
		if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", body); a.code() != CodeInvalidRequest {
			t.Fatalf("%s: %d %s", body, a.status, a.body)
		}
	}
	unchanged("an invalid body")
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionUpdate {
			return authz.Decision{Reason: "role_insufficient"}, nil
		}
		return (&auth.OwnerPolicy{}).Authorize(t.Context(), req)
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"effort":"high"}}`); a.status != http.StatusForbidden {
		t.Fatalf("a denied effort change: %d %s", a.status, a.body)
	}
	unchanged("a denied effort change")
}

// policyEvents are a session's session.policy_changed events.
func (f *fixture) policyEvents(id string) []session.PolicyChanged {
	f.t.Helper()
	evs, err := f.sessions.Events(f.t.Context(), id, 1, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []session.PolicyChanged
	for _, e := range evs {
		if e.Type != session.TypePolicyChanged {
			continue
		}
		var p session.PolicyChanged
		if err := e.Decode(&p); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// TestASessionChangesItsMode: a change of the approval mode asks
// session.update with the mode the body names, the mode the session runs
// and its agent's, records session.policy_changed from the old mode to
// the new, and answers the Session whose policy names the new mode with
// its lists and thresholds as they were; a change to the mode the session
// runs is asked and appends nothing.
func TestASessionChangesItsMode(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	var fields map[string]any
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionUpdate {
			fields = req.Resource.Fields
		}
		return (&auth.OwnerPolicy{}).Authorize(t.Context(), req)
	}
	a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"policy":{"mode":"progressive"}}`)
	if a.status != http.StatusOK {
		t.Fatalf("change: %d %s", a.status, a.body)
	}
	var got session.Session
	a.decode(t, &got)
	if got.Policy == nil || got.Policy.Mode != "progressive" || got.Policy.Thresholds != s.Policy.Thresholds {
		t.Fatalf("the answer's policy is %+v, created with %+v", got.Policy, s.Policy)
	}
	if fields["session_id"] != s.ID || fields["approval_mode"] != "progressive" || fields["current_approval_mode"] != "confirm" || fields["agent_approval_mode"] != "confirm" {
		t.Fatalf("session.update asked about %v", fields)
	}
	if _, model := fields["model"]; model {
		t.Fatalf("a change of the mode alone named a model: %v", fields)
	}
	if _, effort := fields["effort"]; effort {
		t.Fatalf("a change of the mode alone named an effort: %v", fields)
	}
	changes := f.policyEvents(s.ID)
	if len(changes) != 1 || changes[0].Old.Mode != "confirm" || changes[0].New.Mode != "progressive" || changes[0].By.Subject != alice || changes[0].By.Kind != session.SenderPerson {
		t.Fatalf("session.policy_changed %+v", changes)
	}
	if len(f.modelEvents(s.ID)) != 0 {
		t.Fatal("a change of the mode alone appended session.model_changed")
	}
	fields = nil
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"policy":{"mode":"progressive"}}`); a.status != http.StatusOK || len(f.policyEvents(s.ID)) != 1 {
		t.Fatalf("a change to the mode the session runs: %d %s, %d events", a.status, a.body, len(f.policyEvents(s.ID)))
	}
	if fields["current_approval_mode"] != "progressive" || fields["agent_approval_mode"] != "confirm" {
		t.Fatalf("a change to the mode the session runs asked about %v", fields)
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"policy":{"mode":"plan"}}`); a.status != http.StatusOK {
		t.Fatalf("change back: %d %s", a.status, a.body)
	}
	if changes := f.policyEvents(s.ID); len(changes) != 2 || changes[1].Old.Mode != "progressive" || changes[1].New.Mode != "plan" {
		t.Fatalf("session.policy_changed %+v", changes)
	}
	if stored, err := f.sessions.Get(t.Context(), s.ID); err != nil || stored.Policy == nil || stored.Policy.Mode != "plan" {
		t.Fatalf("the stored header's policy is %+v, %v", stored.Policy, err)
	}
}

// TestAModeAndAModelChangeTogether: one body that changes the model and
// the mode asks one session.update with both, and appends both events in
// one batch, after the session's last.
func TestAModeAndAModelChangeTogether(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	var questions []map[string]any
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionUpdate {
			questions = append(questions, req.Resource.Fields)
		}
		return (&auth.OwnerPolicy{}).Authorize(t.Context(), req)
	}
	a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"effort":"high"},"policy":{"mode":"plan"}}`)
	if a.status != http.StatusOK {
		t.Fatalf("change: %d %s", a.status, a.body)
	}
	if len(questions) != 1 || questions[0]["effort"] != "high" || questions[0]["approval_mode"] != "plan" {
		t.Fatalf("session.update asked %v", questions)
	}
	evs, err := f.sessions.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := len(evs)
	if n < 2 || evs[n-2].Type != session.TypeModelChanged || evs[n-1].Type != session.TypePolicyChanged || evs[n-2].Seq+1 != evs[n-1].Seq || !evs[n-2].Time.Equal(evs[n-1].Time) {
		t.Fatalf("the log ends %+v", evs[max(0, n-2):])
	}
	var got session.Session
	a.decode(t, &got)
	if got.Model == nil || got.Model.Reasoning != "high" || got.Policy == nil || got.Policy.Mode != "plan" || got.LastSeq != evs[n-1].Seq {
		t.Fatalf("the answer is %+v %+v at %d", got.Model, got.Policy, got.LastSeq)
	}
}

// TestAModeChangeIsRefused: a mode outside the three, a policy without a
// mode or with another member, and a body naming nothing are
// invalid_request; a caller who may not read the session hears not_found;
// an ended session is conflict; a denied change is forbidden with the
// authorizer's reason. Each leaves the mode and the log as they were.
func TestAModeChangeIsRefused(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	before, err := f.sessions.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"policy":{"mode":"auto"}}`, `{"policy":{"mode":""}}`, `{"policy":{}}`, `{"policy":{"mode":"plan","thresholds":{"ask_at":0.1}}}`, `{"policy":null}`, `{}`, `{"model":{},"policy":{"mode":"plan"}}`} {
		if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", body); a.code() != CodeInvalidRequest {
			t.Errorf("%s: %d %s", body, a.status, a.body)
		}
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "bob", `{"policy":{"mode":"plan"}}`); a.status != http.StatusNotFound {
		t.Fatalf("bob changes alice's mode: %d %s", a.status, a.body)
	}
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionUpdate {
			return authz.Decision{Reason: "approval_mode_restricted"}, nil
		}
		return (&auth.OwnerPolicy{}).Authorize(t.Context(), req)
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"policy":{"mode":"progressive"}}`); a.status != http.StatusForbidden || !strings.Contains(string(a.body), "approval_mode_restricted") {
		t.Fatalf("a denied change: %d %s", a.status, a.body)
	}
	f.authz.answer = nil
	after, err := f.sessions.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastSeq != before.LastSeq || after.Policy.Mode != before.Policy.Mode {
		t.Fatalf("a refused change moved the session from %d %s to %d %s", before.LastSeq, before.Policy.Mode, after.LastSeq, after.Policy.Mode)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/end", "alice", `{"reason":"completed"}`); a.status != http.StatusOK {
		t.Fatalf("end: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"policy":{"mode":"plan"}}`); a.code() != CodeConflict {
		t.Fatalf("a change of an ended session: %d %s", a.status, a.body)
	}
	if len(f.policyEvents(s.ID)) != 0 {
		t.Fatal("a refused change appended session.policy_changed")
	}
}
