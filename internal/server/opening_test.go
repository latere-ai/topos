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

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// allows is an authorizer whose allows of the questions a session's model
// is read at carry the limits answer gives, on top of the owner policy,
// and none when it gives the zero limits. It keeps those questions.
type allows struct {
	mu     sync.Mutex
	answer func(authz.Request) authorizer.WireLimits
	asked  []authz.Request
}

// allowBy makes f's authorizer one whose allows carry what answer gives.
func (f *fixture) allowBy(answer func(authz.Request) authorizer.WireLimits) *allows {
	a := &allows{answer: answer}
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
		a.mu.Lock()
		a.asked = append(a.asked, req)
		l := a.answer(req)
		a.mu.Unlock()
		raw, err := json.Marshal(l)
		if err != nil || string(raw) == "{}" {
			return d, err
		}
		d.Limits = raw
		return d, nil
	}
	return a
}

// by answers every question by answer from here on.
func (a *allows) by(answer func(authz.Request) authorizer.WireLimits) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answer = answer
}

// to answers every question with l from here on.
func (a *allows) to(l authorizer.WireLimits) {
	a.by(func(authz.Request) authorizer.WireLimits { return l })
}

// last is the last question of action asked.
func (a *allows) last(t *testing.T, action string) authz.Request {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, v := range slices.Backward(a.asked) {
		if v.Action == action {
			return v
		}
	}
	t.Fatalf("%s was never asked", action)
	return authz.Request{}
}

// TestAnAllowAsksForTheOpening: an allow's message_text is kept on the
// session's model beside its route at a create, a PATCH and a send, in
// the header, in a read and on both sides of session.model_changed; an
// allow that names a model without it clears it, one that names no model
// changes nothing, a change of it alone at a send appends one change made
// by the service, a failover keeps it and a fork carries it. Keeping it
// does not wait for the operator's ceiling, which is off here (spec 063).
func TestAnAllowAsksForTheOpening(t *testing.T) {
	f, _ := checking(t)
	f.applyModel("auto", "name: "+auto)
	opens := authorizer.WireLimits{Model: haiku, Route: quickWay, MessageText: true}
	a := f.allowBy(func(authz.Request) authorizer.WireLimits { return opens })
	s := f.create("alice", "auto")
	asking := session.ModelRef{Name: haiku, Via: auto, Route: quickWay, MessageText: true}
	if s.Model == nil || *s.Model != asking || *f.header(s.ID).Model != asking {
		t.Fatalf("a create answered %+v", s.Model)
	}
	if read := f.do(http.MethodGet, "/v1/sessions/"+s.ID, "alice", ""); !strings.Contains(string(read.body), `"route":"tier/quick","message_text":true}`) {
		t.Fatalf("a read answers %s", read.body)
	}
	f.turn(s.ID, 1, "Answered.", 1)
	// The same model and route without the ask clears it, a change of its
	// own made by the service; the same allow again changes nothing.
	a.to(authorizer.WireLimits{Model: haiku, Route: quickWay})
	for range 2 {
		if r := f.send(s.ID, "Thanks."); r.status != http.StatusOK {
			t.Fatalf("send: %d %s", r.status, r.body)
		}
	}
	quiet := session.ModelRef{Name: haiku, Via: auto, Route: quickWay}
	changes := f.modelEvents(s.ID)
	if len(changes) != 1 || changes[0].Old != asking || changes[0].New != quiet || changes[0].By.Kind != session.SenderService {
		t.Fatalf("session.model_changed %+v", changes)
	}
	// Asked again, it is set again, another change of its own.
	a.to(opens)
	if r := f.send(s.ID, "A long task."); r.status != http.StatusOK {
		t.Fatalf("send: %d %s", r.status, r.body)
	}
	if changes = f.modelEvents(s.ID); len(changes) != 2 || changes[1].Old != quiet || changes[1].New != asking || *f.header(s.ID).Model != asking {
		t.Fatalf("session.model_changed %+v", changes)
	}
	// An allow that names no model changes nothing, the ask included.
	a.to(authorizer.WireLimits{Route: careWay})
	if r := f.send(s.ID, "Short."); r.status != http.StatusOK || len(f.modelEvents(s.ID)) != 2 || *f.header(s.ID).Model != asking {
		t.Fatalf("an allow with no model: %d, %+v", r.status, f.header(s.ID).Model)
	}
	// A PATCH whose allow names a model without the ask clears it, and
	// one that names it sets it, on both sides of its change.
	a.to(authorizer.WireLimits{Model: sonnet})
	var cleared session.Session
	f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+quick+`"}}`).decode(t, &cleared)
	if cleared.Model == nil || *cleared.Model != (session.ModelRef{Name: sonnet, Via: quick}) || f.header(s.ID).Model.MessageText {
		t.Fatalf("a PATCH without the ask answered %+v", cleared.Model)
	}
	a.to(opens)
	var patched session.Session
	f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+auto+`"}}`).decode(t, &patched)
	if patched.Model == nil || *patched.Model != asking {
		t.Fatalf("a PATCH with the ask answered %+v", patched.Model)
	}
	if changes = f.modelEvents(s.ID); changes[len(changes)-1].Old != (session.ModelRef{Name: sonnet, Via: quick}) || changes[len(changes)-1].New != asking {
		t.Fatalf("the PATCH's session.model_changed %+v", changes[len(changes)-1])
	}
	// A failover moves the model inside the turn and keeps the ask.
	a.by(func(req authz.Request) authorizer.WireLimits {
		if req.Action == authorizer.ActionSessionUpdate && req.Resource.String("failed_model") == haiku {
			return authorizer.WireLimits{Model: sonnet, Route: careWay}
		}
		return opens
	})
	next, err := f.api.Failover(t.Context(), s.ID, asking, asking, "", "")
	if err != nil || next != (session.ModelRef{Name: sonnet, Via: auto, Route: quickWay, MessageText: true}) {
		t.Fatalf("the failover answered %+v, %v", next, err)
	}
	// A fork starts on its parent's model at the fork point, the ask
	// included.
	f.turn(s.ID, 2, "Answered again.", 1)
	if fork := f.forked(s.ID, `{}`); fork.Model == nil || *fork.Model != asking {
		t.Fatalf("the fork starts on %+v", fork.Model)
	}
}
