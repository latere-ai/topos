// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

const via = "tier/quick"

// downReply fails every request with the gateway's answer that no target
// of the model is available, as Lux answers once each upstream failed or
// its circuit opened.
func downReply(name string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: name}, Fail: &luxstub.Failure{Status: 503, Times: 99,
		Body: `{"type":"error","error":{"type":"provider_unavailable","message":"No provider for this model is available right now."}}`}}
}

// limitedReply fails every request with the gateway's upstream_error over
// an upstream's 429, its status and body in the developer detail, as Lux
// answers a provider's rate limit.
func limitedReply(name string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: name}, Fail: &luxstub.Failure{Status: 502, Times: 99, Detail: upstreamLimit,
		Body: `{"type":"error","error":{"type":"upstream_error","message":"The provider returned an error."}}`}}
}

const upstreamLimit = `upstream status 429: {"error":{"message":"Rate limit exceeded: free-models-per-day.","code":429}}`

// answerFrom is a reply of the model name.
func answerFrom(name, say string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: name, Blocks: []ir.Block{text(say)}, StopReason: ir.StopEndTurn, Usage: ir.Usage{InputTokens: 10, OutputTokens: 2}}}
}

// rejectedReply fails every request with the gateway's upstream_rejected
// over a provider's own 4xx, its status and body in the developer detail,
// as Lux answers a provider that refused the request.
func rejectedReply(name string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: name}, Fail: &luxstub.Failure{Status: 400, Times: 99, Detail: upstreamRejection,
		Body: `{"type":"error","error":{"type":"upstream_rejected","message":"The provider rejected this request."}}`}}
}

const upstreamRejection = `upstream status 404: {"error":{"message":"No endpoints found for this model.","code":404}}`

// router is a Failover that answers the models of next in turn, and the
// model the turn stands on once they run out, and records what it was
// asked.
type router struct {
	mu       sync.Mutex
	next     []session.ModelRef
	err      error
	standing []session.ModelRef
	asked    []session.ModelRef
	reasons  []string
	details  []string
}

func (r *router) failover(_ context.Context, standing, failed session.ModelRef, reason, detail string) (session.ModelRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.standing = append(r.standing, standing)
	r.asked = append(r.asked, failed)
	r.reasons = append(r.reasons, reason)
	r.details = append(r.details, detail)
	if r.err != nil {
		return session.ModelRef{}, r.err
	}
	if len(r.next) == 0 {
		return standing, nil
	}
	n := r.next[0]
	r.next = r.next[1:]
	return n, nil
}

// failoverEnv is a session on via standing on model, with a router and a
// clock that records every wait.
func failoverEnv(t *testing.T, r *router) (*env, *[]time.Duration) {
	t.Helper()
	var slept []time.Duration
	e := setupSession(t, func(c *Config) {
		c.Sleep = func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}
		c.Connect = func(_ context.Context, name string) (models.Model, models.Connection, models.Entry, error) {
			if strings.HasPrefix(name, "unknown-") {
				return nil, models.Connection{}, models.Entry{}, &models.Coded{Code: models.CodeUnknown, Message: "no figures for " + name}
			}
			return &dialect.Model{}, models.Connection{BaseURL: c.Connection.BaseURL, Model: name, Family: models.FamilyAnthropic},
				models.Entry{Name: name, InputWindow: 30_000, MaxOutputTokens: 2_000}, nil
		}
		if r != nil {
			c.Failover = r.failover
		}
	}, func(s *session.Session) { s.Model = &session.ModelRef{Name: model, Via: via} })
	return e, &slept
}

// changes are the session.model_changed events of the log.
func (e *env) changes(ctx context.Context) []session.ModelChanged {
	e.t.Helper()
	var out []session.ModelChanged
	for _, ev := range e.events(ctx, session.TypeModelChanged) {
		var m session.ModelChanged
		if err := ev.Decode(&m); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// requests are the model.request events of the log.
func (e *env) requests(ctx context.Context) []session.ModelRequest {
	e.t.Helper()
	var out []session.ModelRequest
	for _, ev := range e.events(ctx, session.TypeModelRequest) {
		var p session.ModelRequest
		if err := ev.Decode(&p); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// sessionErrors are the session.error events of the log.
func (e *env) sessionErrors(ctx context.Context) []session.SessionError {
	e.t.Helper()
	var out []session.SessionError
	for _, ev := range e.events(ctx, session.TypeSessionError) {
		var se session.SessionError
		if err := ev.Decode(&se); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, se)
	}
	return out
}

// TestATurnMovesOffAModelThatCannotServe: a routed turn whose model the
// gateway answers provider_unavailable is not retried on it: the router
// is asked at once, the failed request and the service's
// session.model_changed with the reason are recorded in one batch, and
// the same step is sent again on the model named, at its level, within
// the turn. The next turn stays on that model.
func TestATurnMovesOffAModelThatCannotServe(t *testing.T) {
	const other = "other-model"
	r := &router{next: []session.ModelRef{{Name: other, Via: via, Effort: "low"}}}
	e, slept := failoverEnv(t, r)
	ctx := t.Context()
	low := answerFrom(other, "Answered.")
	low.Expect = func(req *ir.Request) error {
		if req.Reasoning == nil || req.Reasoning.Effort != "low" {
			return errors.New("the moved request is not at the level the router named")
		}
		return nil
	}
	e.stub.Script(model, limitedReply(model))
	e.stub.Script(other, low, answerFrom(other, "Again."))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if len(*slept) != 0 {
		t.Fatalf("the turn waited %v on a model it could move off", *slept)
	}
	if len(r.asked) != 1 || r.asked[0] != (session.ModelRef{Name: model, Via: via}) || r.standing[0] != r.asked[0] || r.reasons[0] != "" || r.details[0] != upstreamLimit {
		t.Fatalf("the router was asked %+v standing on %+v for %q with %q", r.asked, r.standing, r.reasons, r.details)
	}
	reqs := e.requests(ctx)
	if len(reqs) != 2 || reqs[0].Model != model || reqs[0].Outcome != "error" || reqs[0].Attempts != 1 ||
		!strings.Contains(reqs[0].Error, "upstream_error") || !strings.Contains(reqs[0].Error, "free-models-per-day") || reqs[1].Model != other || reqs[1].Outcome != "ok" {
		t.Fatalf("model requests %+v", reqs)
	}
	ch := e.changes(ctx)
	if len(ch) != 1 || ch[0].By.Kind != session.SenderService || ch[0].By.Subject != session.AuthorizerSubject ||
		ch[0].Old.Name != model || ch[0].New.Name != other || ch[0].New.Via != via ||
		ch[0].Reason != session.ReasonModelBusy || !strings.Contains(ch[0].Detail, "upstream status 429") {
		t.Fatalf("session.model_changed %+v", ch)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range evs {
		if ev.Type == session.TypeModelChanged && (i == 0 || evs[i-1].Type != session.TypeModelRequest || evs[i+1].Type != session.TypeModelRequest) {
			t.Fatalf("the change is not between the failed request and the one that answered: %v", evs)
		}
	}
	s, err := e.store.Get(ctx, e.s.ID)
	if err != nil || s.Model == nil || s.Model.Name != other || s.Model.Via != via {
		t.Fatalf("the header's model %+v, %v", s.Model, err)
	}
	if n := len(e.sessionErrors(ctx)); n != 0 {
		t.Fatalf("%d session errors on a turn that answered", n)
	}

	e.send(ctx, "Again.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("the next turn %+v", out)
	}
	if reqs := e.stub.Requests(); len(reqs) != 3 || reqs[2].Request.Model != other {
		t.Fatalf("the next turn's request went to %d requests", len(reqs))
	}
}

// TestATurnThatRunsOutOfMovesEndsBusy: a turn moves at most
// MaxModelSwitches times; the model it stands on then gets DownRetry's
// one quick retry, and the turn ends with model_busy, its sentence for a
// person and the gateway's answer for a developer.
func TestATurnThatRunsOutOfMovesEndsBusy(t *testing.T) {
	pool := []string{model, "second-model", "third-model", "fourth-model", "fifth-model"}
	r := &router{}
	for _, m := range pool[1:] {
		r.next = append(r.next, session.ModelRef{Name: m, Via: via})
	}
	e, slept := failoverEnv(t, r)
	ctx := t.Context()
	for _, m := range pool {
		e.stub.Script(m, downReply(m))
	}
	e.send(ctx, "Go.")
	out := e.turn(ctx)
	if out.StopReason != session.StopError || out.Detail != CodeModelBusy {
		t.Fatalf("outcome %+v", out)
	}
	if len(r.asked) != MaxModelSwitches || len(e.changes(ctx)) != MaxModelSwitches {
		t.Fatalf("the router was asked %d times and the turn moved %d times; at most %d", len(r.asked), len(e.changes(ctx)), MaxModelSwitches)
	}
	if want := []time.Duration{DownRetry.Delay(1)}; len(*slept) != 1 || (*slept)[0] != want[0] || want[0] != time.Second {
		t.Fatalf("waits %v, want the one quick retry %v", *slept, want)
	}
	var ran []string
	for _, p := range e.requests(ctx) {
		ran = append(ran, p.Model)
	}
	if want := strings.Join(pool[:MaxModelSwitches+1], ","); strings.Join(ran, ",") != want {
		t.Fatalf("model requests ran %v, want %s", ran, want)
	}
	if n := len(e.stub.Requests()); n != MaxModelSwitches+2 {
		t.Fatalf("%d requests: one per model moved off, two on the last", n)
	}
	errs := e.sessionErrors(ctx)
	if len(errs) != 1 || errs[0].Code != CodeModelBusy || errs[0].Message != MessageModelBusy || !errs[0].Retryable ||
		!strings.Contains(errs[0].Detail, "provider_unavailable") {
		t.Fatalf("session.error %+v", errs)
	}
}

// TestATurnTheRouterCannotMoveEndsAtOnce: a router that names no other
// model, cannot be asked, or names one that cannot be connected and then
// none other ends the turn with model_busy at once, with no retry and
// nothing recorded but the failed request; the detail says what kept the
// turn where it was.
func TestATurnTheRouterCannotMoveEndsAtOnce(t *testing.T) {
	for _, c := range []struct {
		name string
		r    *router
		why  string
	}{
		{"the failed model", &router{}, "no other model was named"},
		{"an error", &router{err: errors.New("authorizer_unavailable")}, "authorizer_unavailable"},
		{"a model that cannot be connected", &router{next: []session.ModelRef{{Name: "unknown-model", Via: via}}}, "unknown-model was named and could not be connected: models: model_unknown: no figures for unknown-model; no other model was named"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, slept := failoverEnv(t, c.r)
			ctx := t.Context()
			e.stub.Script(model, downReply(model))
			e.send(ctx, "Go.")
			if out := e.turn(ctx); out.StopReason != session.StopError || out.Detail != CodeModelBusy {
				t.Fatalf("outcome %+v", out)
			}
			if len(*slept) != 0 || len(e.stub.Requests()) != 1 || len(e.changes(ctx)) != 0 {
				t.Fatalf("waits %v, %d requests, %d changes", *slept, len(e.stub.Requests()), len(e.changes(ctx)))
			}
			if errs := e.sessionErrors(ctx); len(errs) != 1 || errs[0].Code != CodeModelBusy || !strings.Contains(errs[0].Detail, c.why) {
				t.Fatalf("session.error %+v", errs)
			}
		})
	}
}

// TestAModelNamedThatCannotBeConnectedIsPassedOver: a model the router
// names that cannot be connected is a move that failed: the next question
// stands on the model the turn runs and names the one that could not be
// connected as the failed model, with why as the detail, and the turn
// moves to the model that answer names; the change's detail says which
// model was passed over and why. Each such model counts against
// MaxModelSwitches, so a router that names only such models is asked that
// many times, and the turn then ends model_busy with every one of them in
// the detail and nothing recorded but the failed request.
func TestAModelNamedThatCannotBeConnectedIsPassedOver(t *testing.T) {
	const other = "other-model"
	r := &router{next: []session.ModelRef{{Name: "unknown-model", Via: via}, {Name: other, Via: via}}}
	e, slept := failoverEnv(t, r)
	ctx := t.Context()
	e.stub.Script(model, limitedReply(model))
	e.stub.Script(other, answerFrom(other, "Answered."))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	standing := session.ModelRef{Name: model, Via: via}
	if len(r.asked) != 2 || r.standing[0] != standing || r.standing[1] != standing || r.asked[0] != standing ||
		r.asked[1] != (session.ModelRef{Name: "unknown-model", Via: via}) || r.details[0] != upstreamLimit ||
		!strings.HasPrefix(r.details[1], "unknown-model was named and could not be connected: models: model_unknown") {
		t.Fatalf("the router was asked %+v standing on %+v with %q", r.asked, r.standing, r.details)
	}
	ch := e.changes(ctx)
	if len(ch) != 1 || ch[0].Old.Name != model || ch[0].New.Name != other || ch[0].Reason != session.ReasonModelBusy ||
		!strings.Contains(ch[0].Detail, "upstream status 429") || !strings.Contains(ch[0].Detail, "unknown-model was named and could not be connected") {
		t.Fatalf("session.model_changed %+v", ch)
	}
	if reqs := e.requests(ctx); len(reqs) != 2 || reqs[0].Model != model || reqs[1].Model != other || reqs[1].Outcome != "ok" || len(*slept) != 0 {
		t.Fatalf("model requests %+v, waits %v", reqs, *slept)
	}

	unknown := &router{}
	for i := range MaxModelSwitches + 1 {
		unknown.next = append(unknown.next, session.ModelRef{Name: fmt.Sprintf("unknown-%d", i), Via: via})
	}
	e, slept = failoverEnv(t, unknown)
	e.stub.Script(model, limitedReply(model))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopError || out.Detail != CodeModelBusy {
		t.Fatalf("outcome %+v", out)
	}
	if len(unknown.asked) != MaxModelSwitches || len(e.changes(ctx)) != 0 || len(e.stub.Requests()) != 1 || len(*slept) != 0 {
		t.Fatalf("the router was asked %d times, %d changes, %d requests, waits %v", len(unknown.asked), len(e.changes(ctx)), len(e.stub.Requests()), *slept)
	}
	errs := e.sessionErrors(ctx)
	if len(errs) != 1 || errs[0].Code != CodeModelBusy {
		t.Fatalf("session.error %+v", errs)
	}
	for i := range MaxModelSwitches {
		if !strings.Contains(errs[0].Detail, fmt.Sprintf("unknown-%d was named and could not be connected", i)) {
			t.Fatalf("the detail lacks unknown-%d: %s", i, errs[0].Detail)
		}
	}
}

// TestATurnInterruptedWhileConnectingAsksNothingMore: a turn interrupted
// while it connects the model the router named does not pass that model to
// another question, since the connection failed for the turn's own end:
// the router is asked once, and the turn stops interrupted.
func TestATurnInterruptedWhileConnectingAsksNothingMore(t *testing.T) {
	const slow = "slow-model"
	r := &router{next: []session.ModelRef{{Name: slow, Via: via}, {Name: "other-model", Via: via}}}
	interrupt := make(chan struct{})
	e := setupSession(t, func(c *Config) {
		c.Sleep = func(context.Context, time.Duration) error { return nil }
		c.Interrupt = func() <-chan struct{} { return interrupt }
		c.Connect = func(ctx context.Context, name string) (models.Model, models.Connection, models.Entry, error) {
			if name == slow {
				close(interrupt)
				<-ctx.Done()
				return nil, models.Connection{}, models.Entry{}, ctx.Err()
			}
			return &dialect.Model{}, models.Connection{BaseURL: c.Connection.BaseURL, Model: name, Family: models.FamilyAnthropic},
				models.Entry{Name: name, InputWindow: 30_000, MaxOutputTokens: 2_000}, nil
		}
		c.Failover = r.failover
	}, func(s *session.Session) { s.Model = &session.ModelRef{Name: model, Via: via} })
	ctx := t.Context()
	e.stub.Script(model, limitedReply(model))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopInterrupted {
		t.Fatalf("outcome %+v", out)
	}
	if len(r.asked) != 1 || len(e.changes(ctx)) != 0 {
		t.Fatalf("the router was asked %d times, %d changes", len(r.asked), len(e.changes(ctx)))
	}
}

// TestAModelThatCannotServeTakesOneQuickRetry: a session that cannot move,
// on a model named itself or with no router, retries a model that cannot
// serve once, a second later, where spec 005's policy waited 2, 4, 8, 16
// and 32 seconds less up to a fifth for jitter over six attempts; a
// Retry-After longer than that second takes no retry. A gateway's own
// failure and a provider's overload keep spec 005's policy.
func TestAModelThatCannotServeTakesOneQuickRetry(t *testing.T) {
	ctx := t.Context()
	for _, header := range []func(*session.Session){nil, func(s *session.Session) { s.Model = &session.ModelRef{Name: model, Via: via} }} {
		var slept []time.Duration
		e := setupSession(t, func(c *Config) {
			c.Sleep = func(_ context.Context, d time.Duration) error {
				slept = append(slept, d)
				return nil
			}
		}, header)
		e.stub.Script(model, downReply(model))
		e.send(ctx, "Go.")
		if out := e.turn(ctx); out.Detail != CodeModelBusy {
			t.Fatalf("outcome %+v", out)
		}
		if len(slept) != 1 || slept[0] != time.Second {
			t.Fatalf("waits %v, want one of a second", slept)
		}
		if reqs := e.requests(ctx); len(reqs) != 1 || reqs[0].Attempts != DownRetry.Attempts() {
			t.Fatalf("model requests %+v", reqs)
		}
	}

	var slept []time.Duration
	later := setup(t, func(c *Config) {
		c.Sleep = func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}
	})
	r := downReply(model)
	r.Fail.RetryAfter = "30"
	later.stub.Script(model, r)
	later.send(ctx, "Go.")
	if out := later.turn(ctx); out.Detail != CodeModelBusy || len(slept) != 0 || len(later.stub.Requests()) != 1 {
		t.Fatalf("a Retry-After past the quick retry: %+v, waits %v", out, slept)
	}

	for _, typ := range []string{"store_unavailable", "overloaded_error"} {
		var slept []time.Duration
		g := setup(t, func(c *Config) {
			c.Sleep = func(_ context.Context, d time.Duration) error {
				slept = append(slept, d)
				return nil
			}
		})
		g.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model}, Fail: &luxstub.Failure{Status: 503, Times: 99,
			Body: `{"type":"error","error":{"type":"` + typ + `","message":"try later"}}`}})
		g.send(ctx, "Go.")
		if out := g.turn(ctx); out.Detail != CodeModelError {
			t.Fatalf("%s: outcome %+v", typ, out)
		}
		if len(slept) != DefaultRetry.Attempts()-1 {
			t.Fatalf("%s: waits %v, want spec 005's %d", typ, slept, DefaultRetry.Attempts()-1)
		}
		for i, d := range slept {
			full := DefaultRetry.Base << i
			if d > full || d < full*4/5 {
				t.Fatalf("%s: wait %d is %v, want %v less up to a fifth", typ, i+1, d, full)
			}
		}
	}
}

// TestASpentWalletOnTheModelMovedToStopsWithBudget: a turn moved to a
// priced model whose wallet is spent stops with budget at the gateway's
// refusal, as any spent request does, and never asks the router about a
// refusal for spend.
func TestASpentWalletOnTheModelMovedToStopsWithBudget(t *testing.T) {
	const priced = "priced-model"
	r := &router{next: []session.ModelRef{{Name: priced, Via: via}}}
	e, _ := failoverEnv(t, r)
	ctx := t.Context()
	e.stub.Script(model, downReply(model))
	e.stub.Script(priced, luxstub.Reply{Response: ir.Response{Model: priced}, Fail: &luxstub.Failure{Status: 429, Times: 9,
		Body: `{"type":"error","error":{"type":"budget_exhausted","message":"The budget has nothing left for the window."}}`}})
	e.send(ctx, "Go.")
	out := e.turn(ctx)
	if out.StopReason != session.StopBudget || out.Detail != "budget_exhausted" {
		t.Fatalf("outcome %+v", out)
	}
	if len(r.asked) != 1 || len(e.changes(ctx)) != 1 {
		t.Fatalf("the router was asked %d times", len(r.asked))
	}
}

// TestATurnMovesOffAModelItsProviderRejected: a routed turn whose request
// the gateway answers upstream_rejected, the provider's own refusal, asks
// the router at once with the reason FailedRejected and the gateway's code
// and detail; the turn moves to the model named and answers there within
// the turn, with the failed request and the service's change recorded as
// for a model that cannot serve. A model named that cannot be connected is
// asked past with no reason, since it failed to connect, not to serve.
func TestATurnMovesOffAModelItsProviderRejected(t *testing.T) {
	const other = "other-model"
	r := &router{next: []session.ModelRef{{Name: "unknown-model", Via: via}, {Name: other, Via: via}}}
	e, slept := failoverEnv(t, r)
	ctx := t.Context()
	e.stub.Script(model, rejectedReply(model))
	e.stub.Script(other, answerFrom(other, "Answered."))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	standing := session.ModelRef{Name: model, Via: via}
	if len(r.asked) != 2 || r.asked[0] != standing || r.standing[0] != standing || r.reasons[0] != FailedRejected ||
		r.details[0] != "upstream_rejected: "+upstreamRejection {
		t.Fatalf("the router was asked %+v standing on %+v for %q with %q", r.asked, r.standing, r.reasons, r.details)
	}
	if r.asked[1].Name != "unknown-model" || r.standing[1] != standing || r.reasons[1] != "" || !strings.HasPrefix(r.details[1], "unknown-model was named and could not be connected") {
		t.Fatalf("the second question %+v standing on %+v for %q with %q", r.asked[1], r.standing[1], r.reasons[1], r.details[1])
	}
	if len(*slept) != 0 {
		t.Fatalf("the turn waited %v", *slept)
	}
	reqs := e.requests(ctx)
	if len(reqs) != 2 || reqs[0].Model != model || reqs[0].Outcome != "error" || !strings.Contains(reqs[0].Error, "upstream_rejected") ||
		!strings.Contains(reqs[0].Error, "upstream status 404") || reqs[1].Model != other || reqs[1].Outcome != "ok" {
		t.Fatalf("model requests %+v", reqs)
	}
	ch := e.changes(ctx)
	if len(ch) != 1 || ch[0].By.Kind != session.SenderService || ch[0].Old != standing || ch[0].New.Name != other ||
		ch[0].Reason != session.ReasonModelBusy || !strings.Contains(ch[0].Detail, "upstream_rejected") {
		t.Fatalf("session.model_changed %+v", ch)
	}
	if n := len(e.sessionErrors(ctx)); n != 0 {
		t.Fatalf("%d session errors on a turn that answered", n)
	}
}

// TestARejectedRequestTheRouterKeepsEndsWithTheModelsError: a request the
// provider rejected on a turn the router keeps where it is, by naming no
// other model, or that an authorizer refuses to answer, as one that does
// not know the reason refuses it, ends the turn at once with the model's
// error, model_error and the gateway's sentence, not model_busy: nothing
// is retried, nothing changes, and the detail says why it did not move.
func TestARejectedRequestTheRouterKeepsEndsWithTheModelsError(t *testing.T) {
	for _, c := range []struct {
		name string
		r    *router
		why  string
	}{
		{"named no other model", &router{}, "no other model was named"},
		{"refused the question", &router{err: errors.New("authz: denied: invalid_resource")}, "invalid_resource"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, slept := failoverEnv(t, c.r)
			ctx := t.Context()
			e.stub.Script(model, rejectedReply(model))
			e.send(ctx, "Go.")
			if out := e.turn(ctx); out.StopReason != session.StopError || out.Detail != CodeModelError {
				t.Fatalf("outcome %+v", out)
			}
			if len(c.r.asked) != 1 || c.r.reasons[0] != FailedRejected {
				t.Fatalf("the router was asked %+v for %q", c.r.asked, c.r.reasons)
			}
			if len(*slept) != 0 || len(e.stub.Requests()) != 1 || len(e.changes(ctx)) != 0 {
				t.Fatalf("waits %v, %d requests, %d changes", *slept, len(e.stub.Requests()), len(e.changes(ctx)))
			}
			errs := e.sessionErrors(ctx)
			if len(errs) != 1 || errs[0].Code != CodeModelError || !strings.Contains(errs[0].Message, "The provider rejected this request.") ||
				errs[0].Retryable || !strings.HasPrefix(errs[0].Detail, "HTTP 400 upstream_rejected ("+upstreamRejection+"); ") || !strings.Contains(errs[0].Detail, c.why) {
				t.Fatalf("session.error %+v", errs)
			}
		})
	}
}

// TestRejectionsCountAgainstTheMoves: each move off a rejected request
// counts against MaxModelSwitches; a rejection once the turn is out of
// moves asks nothing and ends with the model's error.
func TestRejectionsCountAgainstTheMoves(t *testing.T) {
	pool := []string{model, "second-model", "third-model", "fourth-model", "fifth-model"}
	r := &router{}
	for _, m := range pool[1:] {
		r.next = append(r.next, session.ModelRef{Name: m, Via: via})
	}
	e, slept := failoverEnv(t, r)
	ctx := t.Context()
	for _, m := range pool {
		e.stub.Script(m, rejectedReply(m))
	}
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopError || out.Detail != CodeModelError {
		t.Fatalf("outcome %+v", out)
	}
	if len(r.asked) != MaxModelSwitches || len(e.changes(ctx)) != MaxModelSwitches || len(*slept) != 0 {
		t.Fatalf("the router was asked %d times, the turn moved %d times, waits %v", len(r.asked), len(e.changes(ctx)), *slept)
	}
	if n := len(e.stub.Requests()); n != MaxModelSwitches+1 {
		t.Fatalf("%d requests, want one per model", n)
	}
	if errs := e.sessionErrors(ctx); len(errs) != 1 || errs[0].Code != CodeModelError || errs[0].Detail != "HTTP 400 upstream_rejected ("+upstreamRejection+")" {
		t.Fatalf("session.error %+v", errs)
	}
}

// TestTheGatewaysOwnRefusalAsksNothing: the gateway's own refusals of a
// routed turn's request are never a reason to move: an invalid request, a
// model the key may not use, a rate limit on the caller's key and a spent
// budget each keep the policy they had, and the router is asked nothing.
// A rejected request on a session that cannot move, on a model named
// itself, asks nothing either and ends with the model's error at once.
func TestTheGatewaysOwnRefusalAsksNothing(t *testing.T) {
	for _, c := range []struct {
		status int
		typ    string
		stop   session.StopReason
		detail string
	}{
		{400, "invalid_request", session.StopError, CodeModelError},
		{400, "dialect_unsupported", session.StopError, CodeModelError},
		{403, "model_not_allowed", session.StopError, CodeModelError},
		{403, "model_unpriced", session.StopError, CodeModelError},
		{429, "rate_limited", session.StopError, CodeModelError},
		{429, "budget_exhausted", session.StopBudget, "budget_exhausted"},
	} {
		t.Run(c.typ, func(t *testing.T) {
			r := &router{next: []session.ModelRef{{Name: "other-model", Via: via}}}
			e, _ := failoverEnv(t, r)
			ctx := t.Context()
			e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model}, Fail: &luxstub.Failure{Status: c.status, Times: 99,
				Body: `{"type":"error","error":{"type":"` + c.typ + `","message":"refused"}}`}})
			e.send(ctx, "Go.")
			if out := e.turn(ctx); out.StopReason != c.stop || out.Detail != c.detail {
				t.Fatalf("outcome %+v", out)
			}
			if len(r.asked) != 0 || len(e.changes(ctx)) != 0 {
				t.Fatalf("the router was asked %+v", r.asked)
			}
		})
	}
	r := &router{next: []session.ModelRef{{Name: "other-model", Via: via}}}
	var slept []time.Duration
	named := setupSession(t, func(c *Config) {
		c.Sleep = func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}
		c.Failover = r.failover
	}, nil)
	ctx := t.Context()
	named.stub.Script(model, rejectedReply(model))
	named.send(ctx, "Go.")
	if out := named.turn(ctx); out.StopReason != session.StopError || out.Detail != CodeModelError {
		t.Fatalf("a model named itself: outcome %+v", out)
	}
	if len(r.asked) != 0 || len(slept) != 0 || len(named.stub.Requests()) != 1 {
		t.Fatalf("a model named itself: asked %+v, waits %v, %d requests", r.asked, slept, len(named.stub.Requests()))
	}
	if errs := named.sessionErrors(ctx); len(errs) != 1 || errs[0].Detail != "HTTP 400 upstream_rejected ("+upstreamRejection+")" {
		t.Fatalf("a model named itself: session.error %+v", errs)
	}
}
