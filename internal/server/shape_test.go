// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// The names of spec 061's tests: a routed name that stands for a choice of
// other routed names, and two of those.
const (
	auto     = "tier/auto"
	quickWay = "tier/quick"
	careWay  = "tier/thorough"
)

// pick is one answer of a routing authorizer: the model it names and the
// route it chose on the way, "" for none.
type pick struct{ model, route string }

// routes is an authorizer that routes by two steps: the owner policy, whose
// allows of the questions a session's model is read at name the model and
// the route answer gives, and no model when it gives none. It keeps those
// questions.
type routes struct {
	mu     sync.Mutex
	answer func(authz.Request) pick
	asked  []authz.Request
}

// routeBy makes f's authorizer one that routes by answer.
func (f *fixture) routeBy(answer func(authz.Request) pick) *routes {
	r := &routes{answer: answer}
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
		p := r.answer(req)
		r.mu.Unlock()
		if p.model == "" && p.route == "" {
			return d, nil
		}
		d.Limits, err = json.Marshal(authorizer.WireLimits{Model: p.model, Route: p.route})
		return d, err
	}
	return r
}

// to answers every question with p from here on.
func (r *routes) to(p pick) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answer = func(authz.Request) pick { return p }
}

// last is the last question of action asked.
func (r *routes) last(t *testing.T, action string) authz.Request {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range slices.Backward(r.asked) {
		if v.Action == action {
			return v
		}
	}
	t.Fatalf("%s was never asked", action)
	return authz.Request{}
}

// shapeFields are the four fields of a message's shape a question
// carries, as the endpoint reads them.
func shapeFields(t *testing.T, req authz.Request) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, name := range []string{"message_chars", "attachments", "links", "tools_last_turn"} {
		if v, ok := req.Resource.Fields[name]; ok {
			out[name] = v
		}
	}
	return out
}

// toolTurn appends turn n of session id with a tool call on thread, ""
// for the session's own, and the request that answered it.
func (f *fixture) toolTurn(id string, n int, thread string) {
	f.t.Helper()
	s := f.header(id)
	var batch []session.Event
	for _, p := range []struct {
		typ     session.Type
		payload any
	}{
		{session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning}},
		{session.TypeModelRequest, session.ModelRequest{Model: haiku, Outcome: "ok"}},
		{session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_1", Name: "read", Input: json.RawMessage(`{}`), Verdict: "allow"}},
		{session.TypeModelRequest, session.ModelRequest{Model: haiku, Outcome: "ok"}},
		{session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn}},
	} {
		e, err := session.NewEvent(p.typ, p.payload, time.Now())
		if err != nil {
			f.t.Fatal(err)
		}
		e.Turn = n
		if p.typ == session.TypeAgentToolUse {
			e.Thread = thread
		}
		batch = append(batch, e)
	}
	session.Stamp(id, s.LastSeq, batch)
	if _, err := f.sessions.Append(f.t.Context(), id, s.LastSeq, batch); err != nil {
		f.t.Fatal(err)
	}
}

// TestASendTellsTheShapeOfItsMessage: a send of a message carries the
// characters of its text as code points, its files and images, the web
// addresses in its text, and whether the last turn called a tool; a send
// of an event that is no message carries none of them, and the message's
// words reach the authorizer nowhere.
func TestASendTellsTheShapeOfItsMessage(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	r := f.routeBy(func(authz.Request) pick { return pick{} })
	s := f.create("alice", "reviewer")
	text := `Lies https://example.com/a und HTTP://example.org, nicht "http:// " – grüß dich`
	body := `{"type":"user.message","payload":{"content":[{"type":"text","text":` + string(must(json.Marshal(text))) + `},` +
		`{"type":"text","text":"and https://x.y"},{"type":"image","image":{"media_type":"image/png","data":"` + b64(pngBytes) + `"}}],` +
		`"attachments":[{"name":"notes.txt","data":"aGk="},{"name":"more.txt","data":"aGk="}]}}`
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", body); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	asked := r.last(t, authorizer.ActionSessionSend)
	got, _ := json.Marshal(shapeFields(t, asked))
	chars := len([]rune(text)) + len([]rune("and https://x.y"))
	if want := `{"attachments":3,"links":3,"message_chars":` + strconv.Itoa(chars) + `,"tools_last_turn":false}`; string(got) != want {
		t.Fatalf("the send carried %s, want %s", got, want)
	}
	if raw, _ := json.Marshal(asked.Resource); strings.Contains(string(raw), "grüß") || strings.Contains(string(raw), "example.com") {
		t.Fatalf("the question carried the message's words: %s", raw)
	}
	// A confirmation continues a turn: it is asked, and carries no shape.
	f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", `{"type":"user.tool_confirmation","payload":{"tool_use_id":"toolu_9","decision":"allow"}}`)
	if confirm := r.last(t, authorizer.ActionSessionSend); confirm.Resource.String("event_type") != string(session.TypeUserToolConfirmation) || len(shapeFields(t, confirm)) != 0 {
		t.Fatalf("a confirmation carried %v", confirm.Resource.Fields)
	}
}

// TestEveryQuestionAboutAMessageCarriesItsShape: a create that carries a
// first message asks session.create with its shape, and a fork sent its
// message asks session.send with it, tools_last_turn read from the turn
// it copied last.
func TestEveryQuestionAboutAMessageCarriesItsShape(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	r := f.routeBy(func(authz.Request) pick { return pick{} })
	s := f.create("alice", "reviewer")
	if got, _ := json.Marshal(shapeFields(t, r.last(t, authorizer.ActionSessionCreate))); string(got) != `{"attachments":0,"links":0,"message_chars":15,"tools_last_turn":false}` {
		t.Fatalf("a create with its first message carried %s", got)
	}
	if a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`); a.status != http.StatusCreated {
		t.Fatalf("create: %d %s", a.status, a.body)
	}
	if got := shapeFields(t, r.last(t, authorizer.ActionSessionCreate)); len(got) != 0 {
		t.Fatalf("a create with no message carried %v", got)
	}
	f.toolTurn(s.ID, 1, "thr_sub")
	f.forked(s.ID, `{"message":{"content":[{"type":"text","text":"See https://a.b"}]}}`)
	if got, _ := json.Marshal(shapeFields(t, r.last(t, authorizer.ActionSessionSend))); string(got) != `{"attachments":0,"links":1,"message_chars":15,"tools_last_turn":true}` {
		t.Fatalf("a fork's message carried %s", got)
	}
}

// TestToolsLastTurnReadsTheLastTurn: tools_last_turn is true after a turn
// that called a tool, on a thread of its own as well, and false after a
// turn that only talked, however far back in the log the tool was.
func TestToolsLastTurnReadsTheLastTurn(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	r := f.routeBy(func(authz.Request) pick { return pick{} })
	s := f.create("alice", "reviewer")
	tools := func() any {
		t.Helper()
		if a := f.send(s.ID, "Go on."); a.status != http.StatusOK {
			t.Fatalf("send: %d %s", a.status, a.body)
		}
		return r.last(t, authorizer.ActionSessionSend).Resource.Fields["tools_last_turn"]
	}
	if got := tools(); got != false {
		t.Fatalf("before any turn tools_last_turn is %v", got)
	}
	f.toolTurn(s.ID, 1, "")
	if got := tools(); got != true {
		t.Fatalf("after a turn that called a tool it is %v", got)
	}
	f.toolTurn(s.ID, 2, "thr_sub")
	if got := tools(); got != true {
		t.Fatalf("after a turn whose subagent called a tool it is %v", got)
	}
	f.turn(s.ID, 3, "Talked.", 1)
	if got := tools(); got != false {
		t.Fatalf("after a turn that only talked it is %v", got)
	}
	// A long turn that called a tool early is read past the first window.
	f.toolTurn(s.ID, 4, "")
	for range requestWindow + 4 {
		f.appendTo(s.ID, 4, session.SessionStatus{Status: session.StatusRunning})
	}
	if got := tools(); got != true {
		t.Fatalf("a tool %d events back reads %v", requestWindow+4, got)
	}
}

// TestARouteIsKeptBesideTheModel: an allow's route is kept beside the
// model it names at a create, a PATCH and a send, in the header and on
// both sides of session.model_changed, and answered by a read; an allow
// that names a model and no route clears it, and one that names a route
// and no model changes nothing.
func TestARouteIsKeptBesideTheModel(t *testing.T) {
	f, _ := checking(t)
	f.applyModel("auto", "name: "+auto)
	r := f.routeBy(func(authz.Request) pick { return pick{haiku, quickWay} })
	s := f.create("alice", "auto")
	if want := (session.ModelRef{Name: haiku, Via: auto, Route: quickWay}); s.Model == nil || *s.Model != want || *f.header(s.ID).Model != want {
		t.Fatalf("a create answered %+v", s.Model)
	}
	f.turn(s.ID, 1, "Answered.", 1)
	r.to(pick{sonnet, careWay})
	if a := f.send(s.ID, "A long task."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	changes := f.modelEvents(s.ID)
	if len(changes) != 1 || changes[0].Old != (session.ModelRef{Name: haiku, Via: auto, Route: quickWay}) ||
		changes[0].New != (session.ModelRef{Name: sonnet, Via: auto, Route: careWay}) || changes[0].By.Kind != session.SenderService {
		t.Fatalf("session.model_changed %+v", changes)
	}
	var read session.Session
	f.do(http.MethodGet, "/v1/sessions/"+s.ID, "alice", "").decode(t, &read)
	if read.Model == nil || read.Model.Route != careWay {
		t.Fatalf("a read answers %+v", read.Model)
	}
	// A route alone, with no model, moves nothing.
	r.to(pick{route: quickWay})
	if a := f.send(s.ID, "Short."); a.status != http.StatusOK || len(f.modelEvents(s.ID)) != 1 {
		t.Fatalf("a route with no model: %d, %d changes", a.status, len(f.modelEvents(s.ID)))
	}
	// A PATCH whose allow names a model and a route keeps both.
	r.to(pick{haiku, quickWay})
	var patched session.Session
	f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+auto+`"}}`).decode(t, &patched)
	if patched.Model == nil || *patched.Model != (session.ModelRef{Name: haiku, Via: auto, Route: quickWay}) {
		t.Fatalf("a PATCH answered %+v", patched.Model)
	}
	// One that names a model and no route clears it.
	r.to(pick{model: sonnet})
	var cleared session.Session
	f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+quick+`"}}`).decode(t, &cleared)
	if cleared.Model == nil || *cleared.Model != (session.ModelRef{Name: sonnet, Via: quick}) || f.header(s.ID).Model.Route != "" {
		t.Fatalf("a PATCH with no route answered %+v", cleared.Model)
	}
}

// TestARouteAloneMovesAtASend: an allow whose model is the one the session
// stands on and whose route differs appends one session.model_changed made
// by the service; the same route again appends nothing.
func TestARouteAloneMovesAtASend(t *testing.T) {
	f, _ := checking(t)
	f.applyModel("auto", "name: "+auto)
	r := f.routeBy(func(authz.Request) pick { return pick{haiku, quickWay} })
	s := f.create("alice", "auto")
	f.turn(s.ID, 1, "Answered.", 1)
	r.to(pick{haiku, careWay})
	for range 2 {
		if a := f.send(s.ID, "A file this time."); a.status != http.StatusOK {
			t.Fatalf("send: %d %s", a.status, a.body)
		}
	}
	changes := f.modelEvents(s.ID)
	if len(changes) != 1 || changes[0].Old.Route != quickWay || changes[0].New != (session.ModelRef{Name: haiku, Via: auto, Route: careWay}) || changes[0].By.Kind != session.SenderService {
		t.Fatalf("session.model_changed %+v", changes)
	}
}

// TestQuestionsCarryTheRoute: the send question carries model_route, and
// a PATCH's question current_model_route, while the session has one, and
// neither while it has none.
func TestQuestionsCarryTheRoute(t *testing.T) {
	f, _ := checking(t)
	f.applyModel("auto", "name: "+auto)
	r := f.routeBy(func(authz.Request) pick { return pick{haiku, careWay} })
	s := f.create("alice", "auto")
	if a := f.send(s.ID, "Again."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if sent := r.last(t, authorizer.ActionSessionSend); sent.Resource.String("model_route") != careWay || sent.Resource.String("model_via") != auto {
		t.Fatalf("the send asked about %v", sent.Resource.Fields)
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"reasoning":"high"}}`); a.status != http.StatusOK {
		t.Fatalf("patch: %d %s", a.status, a.body)
	}
	if asked := r.last(t, authorizer.ActionSessionUpdate); asked.Resource.String("current_model_route") != careWay {
		t.Fatalf("the update asked about %v", asked.Resource.Fields)
	}
	r.to(pick{model: haiku})
	if a := f.send(s.ID, "Plain."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if a := f.send(s.ID, "Plain again."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if _, carried := r.last(t, authorizer.ActionSessionSend).Resource.Fields["model_route"]; carried {
		t.Fatal("a session with no route asked with one")
	}
}

// TestAFailoverKeepsTheRoute: a failover asks with current_model_route and
// keeps the route the turn began on, whatever the allow names.
func TestAFailoverKeepsTheRoute(t *testing.T) {
	f, _ := checking(t)
	f.applyModel("auto", "name: "+auto)
	r := f.routeBy(func(req authz.Request) pick {
		if req.Action == authorizer.ActionSessionUpdate && req.Resource.String("failed_model") == haiku {
			return pick{sonnet, quickWay}
		}
		return pick{haiku, careWay}
	})
	s := f.create("alice", "auto")
	on := session.ModelRef{Name: haiku, Via: auto, Route: careWay}
	next, err := f.api.Failover(t.Context(), s.ID, on, on, "", "")
	if err != nil || next != (session.ModelRef{Name: sonnet, Via: auto, Route: careWay}) {
		t.Fatalf("the failover answered %+v, %v", next, err)
	}
	if asked := r.last(t, authorizer.ActionSessionUpdate); asked.Resource.String("current_model_route") != careWay {
		t.Fatalf("the failover asked about %v", asked.Resource.Fields)
	}
}

// TestAForkCarriesTheRoute: a fork starts on its parent's model with its
// via and its route, and its question names the route.
func TestAForkCarriesTheRoute(t *testing.T) {
	f, _ := checking(t)
	f.applyModel("auto", "name: "+auto)
	r := f.routeBy(func(authz.Request) pick { return pick{haiku, quickWay} })
	s := f.create("alice", "auto")
	f.turn(s.ID, 1, "Answered.", 1)
	r.to(pick{sonnet, careWay})
	if a := f.send(s.ID, "A long one."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	f.turn(s.ID, 2, "Answered at length.", 1)
	fork := f.forked(s.ID, `{}`)
	if fork.Model == nil || *fork.Model != (session.ModelRef{Name: sonnet, Via: auto, Route: careWay}) {
		t.Fatalf("the fork starts on %+v", fork.Model)
	}
	if asked := r.last(t, authorizer.ActionSessionFork); asked.Resource.String("model_route") != careWay {
		t.Fatalf("the fork asked about %v", asked.Resource.Fields)
	}
}

// TestOpenAPIDocumentsTheRoute: the API document names the message's shape,
// the route an allow names and the fields that carry it back.
func TestOpenAPIDocumentsTheRoute(t *testing.T) {
	doc, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"message_chars", "attachments, its files and images", "links, the http:// and https:// addresses", "tools_last_turn", "model_route", "current_model_route", "{name, via, reasoning, route}"} {
		if !strings.Contains(string(doc), word) {
			t.Errorf("the API document does not name %q", word)
		}
	}
}

// must is v, and panics on err: for a value a test builds that cannot fail.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
