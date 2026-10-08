// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// The names of spec 038's tests: a name that stands for a choice, which
// no door lists, and the two models of the embedded catalog it stands
// for.
const (
	quick  = "tier/quick"
	haiku  = "claude-haiku-4-5"
	sonnet = "anthropic/claude-sonnet-4-5"
)

// router is an authorizer that routes: the owner policy, whose allows of
// the questions a session's model is read at name the model resolve
// answers, and no model when it answers "". It keeps those questions.
type router struct {
	mu      sync.Mutex
	resolve func(authz.Request) string
	asked   []authz.Request
}

// route makes f's authorizer a router that resolves by resolve.
func (f *fixture) route(resolve func(authz.Request) string) *router {
	r := &router{resolve: resolve}
	next := f.authz.next
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		d, err := next.Authorize(context.Background(), req)
		if err != nil || !d.Allow {
			return d, err
		}
		switch req.Action {
		case authorizer.ActionSessionCreate, authorizer.ActionSessionFork, authorizer.ActionSessionUpdate, authorizer.ActionSessionSend:
		default:
			return d, nil
		}
		r.mu.Lock()
		r.asked = append(r.asked, req)
		model := r.resolve(req)
		r.mu.Unlock()
		if model == "" {
			return d, nil
		}
		d.Limits, err = json.Marshal(authorizer.WireLimits{Model: model})
		return d, err
	}
	return r
}

// to routes every question to model from here on.
func (r *router) to(model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolve = func(authz.Request) string { return model }
}

// last is the last question of action the router answered.
func (r *router) last(t *testing.T, action string) authz.Request {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, req := range slices.Backward(r.asked) {
		if req.Action == action {
			return req
		}
	}
	t.Fatalf("%s was never asked", action)
	return authz.Request{}
}

// wire is a question's resource as the authorizer's endpoint reads it,
// with the ids a run mints replaced by their names.
func wire(t *testing.T, req authz.Request, ids ...string) string {
	t.Helper()
	b, err := json.Marshal(req.Resource)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer(ids...).Replace(string(b))
}

// checking is a fixture whose model check keeps the names it was asked,
// and answers from the embedded catalog as the fixture's own does.
func checking(t *testing.T) (*fixture, *[]string) {
	t.Helper()
	cat, err := models.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	f := newFixture(t, func(o *Options) {
		o.Runnable = func(_ context.Context, m v1.AgentModel, overlay models.Entry) error {
			names = append(names, m.Name)
			_, err := cat.Resolve(m.Name, overlay)
			return err
		}
	})
	return f, &names
}

// applyModel applies an agent as alice whose spec.model is model, a YAML
// flow mapping's inside.
func (f *fixture) applyModel(name, model string) {
	f.t.Helper()
	doc := strings.Replace(agentYAML(name, "Answer."), "model: {name: claude-haiku-4-5}", "model: {"+model+"}", 1)
	if a := f.do(http.MethodPut, "/v1/agents/"+name, "alice", doc); a.status != http.StatusCreated {
		f.t.Fatalf("apply %s: %d %s", name, a.status, a.body)
	}
}

// send sends a user.message to a session as alice.
func (f *fixture) send(id, text string) answer {
	f.t.Helper()
	return f.do(http.MethodPost, "/v1/sessions/"+id+"/events", "alice", `{"type":"user.message","payload":{"content":[{"type":"text","text":"`+text+`"}]}}`)
}

// log is a session's events.
func (f *fixture) log(id string) []session.Event {
	f.t.Helper()
	evs, err := f.sessions.Events(f.t.Context(), id, 1, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return evs
}

// header is a session's stored header.
func (f *fixture) header(id string) session.Session {
	f.t.Helper()
	s, err := f.sessions.Get(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// TestARoutedSessionStartsOnTheModelItsAllowNames: an agent names a
// choice no door lists, session.create carries that name as model, and
// the allow's model is the one the session's header holds, with the
// agent's name as via and the agent's effort; the model checked is the
// one the allow names, never the name asked, and nothing is appended
// for it.
func TestARoutedSessionStartsOnTheModelItsAllowNames(t *testing.T) {
	f, names := checking(t)
	f.applyModel("quick", "name: "+quick)
	f.applyModel("careful", "name: "+quick+", effort: high")
	r := f.route(func(req authz.Request) string {
		if req.Resource.String("model") == quick {
			return haiku
		}
		return ""
	})
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"quick","message":"Answer."}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create: %d %s", a.status, a.body)
	}
	var answered struct {
		ID    string          `json:"id"`
		Model json.RawMessage `json:"model"`
	}
	a.decode(t, &answered)
	if want := `{"name":"claude-haiku-4-5","via":"tier/quick"}`; string(answered.Model) != want {
		t.Fatalf("the session's model is %s, want %s", answered.Model, want)
	}
	asked := r.last(t, authorizer.ActionSessionCreate)
	if _, via := asked.Resource.Fields["model_via"]; via || asked.Resource.String("model") != quick || asked.Resource.String("session_id") != answered.ID {
		t.Fatalf("session.create asked about %v", asked.Resource.Fields)
	}
	if !slices.Equal(*names, []string{haiku}) {
		t.Fatalf("the create checked %v, not the model its allow named alone", *names)
	}
	if s := f.header(answered.ID); s.Model == nil || *s.Model != (session.ModelRef{Name: haiku, Via: quick}) {
		t.Fatalf("the stored header's model is %+v", s.Model)
	}
	if evs := f.log(answered.ID); len(evs) != 1 || evs[0].Type != session.TypeUserMessage {
		t.Fatalf("the routed create appended %d events", len(evs))
	}
	var got session.Session
	if a := f.do(http.MethodGet, "/v1/sessions/"+answered.ID, "alice", ""); a.status != http.StatusOK {
		t.Fatalf("get: %d %s", a.status, a.body)
	} else {
		a.decode(t, &got)
	}
	if got.Model == nil || got.Model.Name != haiku || got.Model.Via != quick {
		t.Fatalf("a read answers the model %+v", got.Model)
	}
	// The agent's level holds on the model the allow named, answered as
	// reasoning.
	a = f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"careful"}`)
	a.decode(t, &got)
	if a.status != http.StatusCreated || got.Model == nil || *got.Model != (session.ModelRef{Name: haiku, Via: quick, Reasoning: "high"}) {
		t.Fatalf("a routed create of an agent with an effort: %d, model %+v", a.status, got.Model)
	}
	// An allow that names the agent's own model routes nothing.
	f.apply("alice", "reviewer", "Review.")
	r.to(haiku)
	if s := f.create("alice", "reviewer"); s.Model != nil {
		t.Fatalf("an allow that names the agent's model set %+v", s.Model)
	}
}

// TestAPatchToANameTheAuthorizerResolves: a PATCH to a name the
// authorizer resolves answers the session with the resolved model and
// the name asked as via, and appends one session.model_changed made by
// the person; the question carries the name asked and the model the
// session stands on, the model checked is the resolved one, a repeat the
// authorizer answers alike appends nothing, and a PATCH to a model by
// its own name drops via.
func TestAPatchToANameTheAuthorizerResolves(t *testing.T) {
	f, names := checking(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	*names = nil
	r := f.route(func(req authz.Request) string {
		if req.Action == authorizer.ActionSessionUpdate && req.Resource.String("model") == quick {
			return sonnet
		}
		return ""
	})
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
	got := patch(`{"model":{"name":"` + quick + `"}}`)
	if got.Model == nil || *got.Model != (session.ModelRef{Name: sonnet, Via: quick}) {
		t.Fatalf("the answer's model is %+v", got.Model)
	}
	if !slices.Equal(*names, []string{sonnet}) {
		t.Fatalf("the switch checked %v, not the model its allow named alone", *names)
	}
	asked := r.last(t, authorizer.ActionSessionUpdate)
	want := `{"agent":"AGENT","current_model":"claude-haiku-4-5","id":"SESSION","kind":"session","model":"tier/quick","owner":"https://login.example|alice","runner":"hosted","session_id":"SESSION"}`
	if got := wire(t, asked, s.ID, "SESSION", s.Agent.ID, "AGENT"); got != want {
		t.Fatalf("session.update asked about\n%s, want\n%s", got, want)
	}
	changes := f.modelEvents(s.ID)
	if len(changes) != 1 || changes[0].Old != (session.ModelRef{Name: haiku}) || changes[0].New != (session.ModelRef{Name: sonnet, Via: quick}) ||
		changes[0].By != (session.Sender{Subject: alice, Kind: session.SenderPerson}) {
		t.Fatalf("session.model_changed %+v", changes)
	}
	// The same name again, answered alike, changes nothing, and the
	// question names what the session stands on with its via.
	patch(`{"model":{"name":"` + quick + `"}}`)
	asked = r.last(t, authorizer.ActionSessionUpdate)
	if n := len(f.modelEvents(s.ID)); n != 1 || asked.Resource.String("current_model") != sonnet || asked.Resource.String("current_model_via") != quick {
		t.Fatalf("a repeat appended %d events and asked about %v", n-1, asked.Resource.Fields)
	}
	// An effort change alone names no model, and an allow that names one
	// for it moves nothing.
	r.to(haiku)
	if got := patch(`{"model":{"effort":"high"}}`); *got.Model != (session.ModelRef{Name: sonnet, Via: quick, Reasoning: "high"}) {
		t.Fatalf("an effort change on a routed session answers %+v", got.Model)
	}
	// The same name answered with another model moves the session and
	// keeps the name asked and the effort.
	if got := patch(`{"model":{"name":"` + quick + `"}}`); *got.Model != (session.ModelRef{Name: haiku, Via: quick, Reasoning: "high"}) {
		t.Fatalf("a name answered with another model answers %+v", got.Model)
	}
	// A model asked by its own name, which the allow leaves as asked,
	// runs with no via.
	r.to("")
	if got := patch(`{"model":{"name":"` + haiku + `"}}`); *got.Model != (session.ModelRef{Name: haiku, Reasoning: "high"}) {
		t.Fatalf("a model asked by its own name answers %+v", got.Model)
	}
	if n := len(f.modelEvents(s.ID)); n != 4 {
		t.Fatalf("%d session.model_changed events, want 4", n)
	}
}

// TestASendMovesTheSessionToTheModelItsAllowNames: a send whose allow
// names a model other than the one the session stands on appends
// session.model_changed made by the service straight before the message
// that starts the turn, with the name asked and the effort kept, and
// the header takes it; a send whose allow names the model the session
// stands on, or none, appends the message alone, and an interrupt asks
// nothing about the model.
func TestASendMovesTheSessionToTheModelItsAllowNames(t *testing.T) {
	f, names := checking(t)
	f.applyModel("quick", "name: "+quick+", effort: low")
	r := f.route(func(authz.Request) string { return haiku })
	var s session.Session
	f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"quick","message":"Answer."}`).decode(t, &s)
	f.turn(s.ID, 1, "Answered.", 1)
	before := f.header(s.ID).LastSeq
	*names = nil

	r.to(sonnet)
	a := f.send(s.ID, "And again.")
	if a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	var sent session.Event
	a.decode(t, &sent)
	evs := f.log(s.ID)
	if len(evs) != int(before)+2 || evs[before].Type != session.TypeModelChanged || evs[before+1].Type != session.TypeUserMessage || sent.Seq != before+2 || sent.ID != evs[before+1].ID {
		t.Fatalf("the send appended %d events and answered seq %d", len(evs)-int(before), sent.Seq)
	}
	want := `{"by":{"subject":"service:authorizer","kind":"service"},"old":{"name":"claude-haiku-4-5","via":"tier/quick","effort":"low"},"new":{"name":"anthropic/claude-sonnet-4-5","via":"tier/quick","effort":"low"}}`
	if got := string(evs[before].Payload); got != want {
		t.Fatalf("session.model_changed is\n%s, want\n%s", got, want)
	}
	if h := f.header(s.ID); h.Model == nil || *h.Model != (session.ModelRef{Name: sonnet, Via: quick, Effort: "low"}) {
		t.Fatalf("the header's model is %+v", h.Model)
	}
	if !slices.Equal(*names, []string{sonnet}) {
		t.Fatalf("the send checked %v", *names)
	}
	// The same model again, and no model at all, append the message alone.
	for _, model := range []string{sonnet, ""} {
		r.to(model)
		if a := f.send(s.ID, "Once more."); a.status != http.StatusOK {
			t.Fatalf("send: %d %s", a.status, a.body)
		}
	}
	if evs := f.log(s.ID); len(evs) != int(before)+4 || len(f.modelEvents(s.ID)) != 1 {
		t.Fatalf("two sends that move nothing left %d events, %d of them model changes", len(evs), len(f.modelEvents(s.ID)))
	}
	// A session that runs a model by its own name keeps that name as via.
	f.apply("alice", "reviewer", "Review.")
	r.to("")
	plain := f.create("alice", "reviewer")
	r.to(sonnet)
	if a := f.send(plain.ID, "Review this too."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if changes := f.modelEvents(plain.ID); len(changes) != 1 || changes[0].Old != (session.ModelRef{Name: haiku}) || changes[0].New != (session.ModelRef{Name: sonnet, Via: haiku}) ||
		changes[0].By.Kind != session.SenderService {
		t.Fatalf("session.model_changed %+v", changes)
	}
	// An allow that names the name the session was asked by returns it
	// to that name, with no via.
	r.to(haiku)
	if a := f.send(plain.ID, "And this."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if h := f.header(plain.ID); h.Model == nil || *h.Model != (session.ModelRef{Name: haiku}) {
		t.Fatalf("the header's model is %+v", h.Model)
	}
	// An interrupt starts no turn: it asks session.interrupt, which the
	// router does not answer, and carries nothing about the model.
	asked := len(r.asked)
	if a := f.do(http.MethodPost, "/v1/sessions/"+plain.ID+"/events", "alice", `{"type":"user.interrupt","payload":{}}`); a.status != http.StatusOK || len(r.asked) != asked {
		t.Fatalf("an interrupt: %d %s, %d routed questions", a.status, a.body, len(r.asked)-asked)
	}
}

// TestASendTellsHowLongTheSessionHasBeenQuiet: session.send carries the
// model the session stands on, the name it was asked by, and the whole
// seconds since the session's last model.request ended, however far back
// in the log that request is; a session that has made no request
// carries no idle_seconds, and a clock behind the request reads zero.
func TestASendTellsHowLongTheSessionHasBeenQuiet(t *testing.T) {
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	f := newFixture(t, func(o *Options) {
		o.Now = func() time.Time { return now }
		o.Objects = store.NewMemory(o.Now)
	})
	f.applyModel("quick", "name: "+quick)
	r := f.route(func(authz.Request) string { return haiku })
	var s session.Session
	f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"quick"}`).decode(t, &s)
	resource := func() string {
		t.Helper()
		return wire(t, r.last(t, authorizer.ActionSessionSend), s.ID, "SESSION", s.Agent.ID, "AGENT")
	}
	if a := f.send(s.ID, "Answer."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	want := `{"agent":"AGENT","attachments":0,"event_type":"user.message","id":"SESSION","kind":"session","links":0,"message_chars":7,"model":"claude-haiku-4-5","model_via":"tier/quick","owner":"https://login.example|alice","runner":"hosted","sender":"https://login.example|alice","tools_last_turn":false}`
	if got := resource(); got != want {
		t.Fatalf("a session that has made no request asked\n%s, want\n%s", got, want)
	}
	// The turn's request ended at now, and a thread's ended after it.
	request := func(thread string, at time.Time) session.Event {
		t.Helper()
		e, err := session.NewEvent(session.TypeModelRequest, session.ModelRequest{Model: haiku, Outcome: "ok"}, at)
		if err != nil {
			t.Fatal(err)
		}
		e.Turn, e.Thread = 1, thread
		return e
	}
	batch := []session.Event{request("", now), request("thr_1", now.Add(30*time.Second))}
	last := f.header(s.ID).LastSeq
	session.Stamp(s.ID, last, batch)
	if _, err := f.sessions.Append(t.Context(), s.ID, last, batch); err != nil {
		t.Fatal(err)
	}
	now = now.Add(8*time.Minute + 900*time.Millisecond)
	if a := f.send(s.ID, "And again."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	want = `{"agent":"AGENT","attachments":0,"event_type":"user.message","id":"SESSION","idle_seconds":450,"kind":"session","links":0,"message_chars":10,"model":"claude-haiku-4-5","model_via":"tier/quick","owner":"https://login.example|alice","runner":"hosted","sender":"https://login.example|alice","tools_last_turn":false}`
	if got := resource(); got != want {
		t.Fatalf("a session quiet for seven and a half minutes asked\n%s, want\n%s", got, want)
	}
	// The request is found past the first window of the log's end.
	for range requestWindow + 6 {
		if a := f.send(s.ID, "More."); a.status != http.StatusOK {
			t.Fatalf("send: %d %s", a.status, a.body)
		}
	}
	if got := r.last(t, authorizer.ActionSessionSend).Resource.Int("idle_seconds"); got != 450 {
		t.Fatalf("a request %d events back reads idle_seconds %d", requestWindow+8, got)
	}
	now = now.Add(-time.Hour)
	if a := f.send(s.ID, "Earlier."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if idle, carried := r.last(t, authorizer.ActionSessionSend).Resource.Fields["idle_seconds"]; !carried || idle != 0 {
		t.Fatalf("a clock behind the request reads idle_seconds %v", idle)
	}
}

// TestAnAllowWithoutAModelRoutesNothing: under an authorizer that never
// names a model, and under one whose limits name other members alone, a
// create leaves the header's model absent, a PATCH runs the name asked
// with no via, and a send appends its message alone.
func TestAnAllowWithoutAModelRoutesNothing(t *testing.T) {
	for name, answer := range map[string]func(*fixture) func(authz.Request) (authz.Decision, error){
		"the owner policy": func(*fixture) func(authz.Request) (authz.Decision, error) { return nil },
		"limits of other members": func(f *fixture) func(authz.Request) (authz.Decision, error) {
			return func(req authz.Request) (authz.Decision, error) {
				d, err := f.authz.next.Authorize(context.Background(), req)
				if d.Allow && !authz.IsList(req.Action) {
					d.Limits = json.RawMessage(`{"budget_usd_micro":5000000,"a_member_from_later":true}`)
				}
				return d, err
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.authz.answer = answer(f)
			f.apply("alice", "reviewer", "Review.")
			a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","message":"Review main.go."}`)
			var s session.Session
			a.decode(t, &s)
			if a.status != http.StatusCreated || s.Model != nil || strings.Contains(string(a.body), `"via"`) {
				t.Fatalf("create: %d %s", a.status, a.body)
			}
			a = f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+sonnet+`"}}`)
			a.decode(t, &s)
			if a.status != http.StatusOK || s.Model == nil || *s.Model != (session.ModelRef{Name: sonnet}) || strings.Contains(string(a.body), `"via"`) {
				t.Fatalf("switch: %d %s", a.status, a.body)
			}
			if a := f.send(s.ID, "And again."); a.status != http.StatusOK {
				t.Fatalf("send: %d %s", a.status, a.body)
			}
			changes := f.modelEvents(s.ID)
			if evs := f.log(s.ID); len(evs) != 3 || len(changes) != 1 || changes[0].By.Kind != session.SenderPerson || changes[0].New.Via != "" {
				t.Fatalf("%d events, model changes %+v", len(evs), changes)
			}
		})
	}
}

// TestANameNothingResolvesIsUnknown: a name no door lists that the
// authorizer does not resolve is model_unknown at a create and at a
// PATCH, and so is a model the authorizer names that no door lists, at a
// create, a PATCH and a send; none creates a session or appends.
func TestANameNothingResolvesIsUnknown(t *testing.T) {
	f := newFixture(t)
	f.applyModel("quick", "name: "+quick)
	f.apply("alice", "reviewer", "Review.")
	unknown := func(what string, a answer) {
		t.Helper()
		if a.status != http.StatusUnprocessableEntity || a.code() != models.CodeUnknown {
			t.Fatalf("%s: %d %s", what, a.status, a.body)
		}
	}
	sessions := func() int {
		t.Helper()
		list, _, err := f.sessions.List(t.Context(), session.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	unknown("a create of a name nothing resolves", f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"quick","message":"Answer."}`))
	if n := sessions(); n != 0 {
		t.Fatalf("the refused create left %d sessions", n)
	}
	s := f.create("alice", "reviewer")
	before := f.header(s.ID)
	unchanged := func(what string) {
		t.Helper()
		if after := f.header(s.ID); after.Model != nil || after.LastSeq != before.LastSeq {
			t.Fatalf("%s changed the session: model %+v, last seq %d from %d", what, after.Model, after.LastSeq, before.LastSeq)
		}
	}
	unknown("a switch to a name nothing resolves", f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+quick+`"}}`))
	unchanged("a switch to a name nothing resolves")

	// An authorizer that answers a model no door lists resolves nothing
	// the installation runs.
	f.route(func(authz.Request) string { return "vendor/no-such-model" })
	unknown("a create routed to a model no door lists", f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`))
	if n := sessions(); n != 1 {
		t.Fatalf("the refused create left %d sessions", n)
	}
	unknown("a switch routed to a model no door lists", f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+sonnet+`"}}`))
	unknown("a send routed to a model no door lists", f.send(s.ID, "And again."))
	unchanged("a send routed to a model no door lists")
}

// TestAModelThatDoesNotDecodeRefusesTheRequest: a limits.model that is
// no model's name refuses the create, the PATCH and the send as
// authorizer_unavailable, and changes nothing.
func TestAModelThatDoesNotDecodeRefusesTheRequest(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	before := f.header(s.ID).LastSeq
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		d, err := f.authz.next.Authorize(context.Background(), req)
		if d.Allow && !authz.IsList(req.Action) {
			d.Limits = json.RawMessage(`{"model":{"name":"vendor/model-a"}}`)
		}
		return d, err
	}
	for what, a := range map[string]answer{
		"create": f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`),
		"switch": f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+sonnet+`"}}`),
		"send":   f.send(s.ID, "And again."),
	} {
		if a.status != http.StatusServiceUnavailable || a.code() != "authorizer_unavailable" {
			t.Errorf("%s: %d %s", what, a.status, a.body)
		}
	}
	if after := f.header(s.ID); after.LastSeq != before || after.Model != nil {
		t.Fatalf("the refused requests changed the session: %+v", after)
	}
}

// TestAForkStartsOnTheModelItsParentStoodOn: a fork of a routed session
// starts on the model the session stood on at the fork point, with the
// name it was asked by: the one its create's allow named for a fork
// point before any change, and the new model of the last change before
// a later one. session.fork carries that model, its allow's model is
// not read, and the model checked is the one the fork starts on, so an
// agent whose model no door lists is forked with no answer from the
// authorizer.
func TestAForkStartsOnTheModelItsParentStoodOn(t *testing.T) {
	f, names := checking(t)
	f.applyModel("quick", "name: "+quick)
	r := f.route(func(authz.Request) string { return haiku })
	var s session.Session
	f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"quick","message":"Answer."}`).decode(t, &s)
	first := f.turn(s.ID, 1, "Answered.", 1)
	fork := func(body string) session.Session {
		t.Helper()
		a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", body)
		if a.status != http.StatusCreated {
			t.Fatalf("fork %s: %d %s", body, a.status, a.body)
		}
		var out session.Session
		a.decode(t, &out)
		return out
	}
	started := session.ModelRef{Name: haiku, Via: quick}
	// The log holds no change: the model is the header's, and an allow
	// that names another model moves nothing.
	*names = nil
	r.to(sonnet)
	if got := fork(""); got.Model == nil || *got.Model != started {
		t.Fatalf("a fork of a session routed at its create runs %+v", got.Model)
	}
	asked := r.last(t, authorizer.ActionSessionFork)
	if asked.Resource.String("model") != haiku || asked.Resource.String("model_via") != quick || asked.Resource.ID != s.ID {
		t.Fatalf("session.fork asked about %v", asked.Resource.Fields)
	}
	if !slices.Equal(*names, []string{haiku}) {
		t.Fatalf("the fork checked %v, not the model it starts on alone", *names)
	}
	// A send moves the session, and then a person does.
	if a := f.send(s.ID, "And again."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	second := f.turn(s.ID, 2, "Answered again.", 1)
	r.to("")
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+haiku+`"}}`); a.status != http.StatusOK {
		t.Fatalf("switch: %d %s", a.status, a.body)
	}
	for at, want := range map[uint64]session.ModelRef{first: started, second: {Name: sonnet, Via: quick}} {
		body, err := json.Marshal(forkBody{AtSeq: &at})
		if err != nil {
			t.Fatal(err)
		}
		if got := fork(string(body)); got.Model == nil || *got.Model != want {
			t.Errorf("a fork at %d runs %+v, want %+v", at, got.Model, want)
		}
	}
	// A session that ran its agent's model at the fork point forks with
	// no model of its own, whatever it changed to later.
	f.apply("alice", "reviewer", "Review.")
	plain := f.create("alice", "reviewer")
	boundary := f.turn(plain.ID, 1, "Reviewed.", 1)
	if a := f.do(http.MethodPatch, "/v1/sessions/"+plain.ID, "alice", `{"model":{"name":"`+sonnet+`"}}`); a.status != http.StatusOK {
		t.Fatalf("switch: %d %s", a.status, a.body)
	}
	body, err := json.Marshal(forkBody{AtSeq: &boundary})
	if err != nil {
		t.Fatal(err)
	}
	a := f.do(http.MethodPost, "/v1/sessions/"+plain.ID+"/fork", "alice", string(body))
	var forked session.Session
	a.decode(t, &forked)
	if a.status != http.StatusCreated || forked.Model != nil {
		t.Fatalf("a fork before the session's first change: %d, model %+v", a.status, forked.Model)
	}
	if asked := r.last(t, authorizer.ActionSessionFork); asked.Resource.String("model") != haiku || asked.Resource.Fields["model_via"] != nil {
		t.Fatalf("session.fork asked about %v", asked.Resource.Fields)
	}
}

// TestATriggersSendIsRoutedAsAPersons: a firing that continues its
// key's session asks session.send with the model the session stands on,
// and its allow's model moves the session before the firing's message.
func TestATriggersSendIsRoutedAsAPersons(t *testing.T) {
	f := newTriggerFixture(t)
	f.applyTrigger("alice", "triage", eventSpec("policy: continue"))
	first := f.fired("alice", "triage", f.envelope("1", "issue.opened", "o/r#1", "Crash"), store.OutcomeStarted)
	r := f.route(func(authz.Request) string { return sonnet })
	f.fired("alice", "triage", f.envelope("2", "issue.edited", "o/r#1", "Crash on start"), store.OutcomeContinued)
	asked := r.last(t, authorizer.ActionSessionSend)
	if _, idle := asked.Resource.Fields["idle_seconds"]; idle || asked.Resource.String("model") != haiku || !strings.HasPrefix(asked.Resource.String("sender"), session.TriggerSubjectPrefix) {
		t.Fatalf("the firing's session.send asked about %v", asked.Resource.Fields)
	}
	evs := f.log(first.SessionID)
	if len(evs) != 3 || evs[1].Type != session.TypeModelChanged || evs[2].Type != session.TypeUserMessage {
		t.Fatalf("the continued session's log has %d events", len(evs))
	}
	if changes := f.modelEvents(first.SessionID); changes[0].By.Subject != session.AuthorizerSubject || changes[0].New != (session.ModelRef{Name: sonnet, Via: haiku}) {
		t.Fatalf("session.model_changed %+v", changes)
	}
}
