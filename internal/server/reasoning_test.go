// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// storedBefore is an agent version exactly as Topos v0.16.0 stored it,
// each model's reasoning level under effort, read from testdata.
type storedBefore struct {
	Manifest string `json:"manifest"`
	Agent    struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		Owner     string    `json:"owner"`
		OwnerType string    `json:"owner_type"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"agent"`
	Version struct {
		Version   int       `json:"version"`
		Digest    string    `json:"digest"`
		CreatedBy string    `json:"created_by"`
		CreatedAt time.Time `json:"created_at"`
		Doc       string    `json:"doc"`
		Bundle    string    `json:"bundle"`
	} `json:"version"`
}

// putStoredBefore stores the version v0.16.0 stored into f's objects, as
// the store an upgraded server opens holds it.
func (f *fixture) putStoredBefore() storedBefore {
	f.t.Helper()
	b, err := os.ReadFile("testdata/agent-stored-by-v0.16.0.json")
	if err != nil {
		f.t.Fatal(err)
	}
	var s storedBefore
	if err := json.Unmarshal(b, &s); err != nil {
		f.t.Fatal(err)
	}
	a := store.Agent{ID: s.Agent.ID, Name: s.Agent.Name, Owner: s.Agent.Owner, OwnerType: s.Agent.OwnerType, CreatedAt: s.Agent.CreatedAt}
	v := store.AgentVersion{AgentID: s.Agent.ID, Version: s.Version.Version, Digest: s.Version.Digest, Doc: []byte(s.Version.Doc), Bundle: []byte(s.Version.Bundle),
		CreatedBy: s.Version.CreatedBy, CreatedAt: s.Version.CreatedAt}
	if err := f.objects.PutVersion(f.t.Context(), a, v); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// leveling is an authorizer that answers the questions a session's model
// and level are read at with the owner policy's allow and the limits
// answer returns for each, none for nil, and keeps those questions. A deny
// stays a deny.
type leveling struct {
	mu     sync.Mutex
	answer func(authz.Request) *authorizer.WireLimits
	asked  []authz.Request
}

// level makes f's authorizer a leveling one.
func (f *fixture) level(answer func(authz.Request) *authorizer.WireLimits) *leveling {
	l := &leveling{answer: answer}
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
		l.mu.Lock()
		l.asked = append(l.asked, req)
		w := l.answer(req)
		l.mu.Unlock()
		if w == nil {
			return d, nil
		}
		d.Limits, err = json.Marshal(*w)
		return d, err
	}
	return l
}

// to answers every question with w from here on.
func (l *leveling) to(w *authorizer.WireLimits) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.answer = func(authz.Request) *authorizer.WireLimits { return w }
}

// last is the last question of action the authorizer answered.
func (l *leveling) last(t *testing.T, action string) authz.Request {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, req := range slices.Backward(l.asked) {
		if req.Action == action {
			return req
		}
	}
	t.Fatalf("%s was never asked", action)
	return authz.Request{}
}

// reasoning is an allow's limits that name the level r.
func reasoning(r string) *authorizer.WireLimits { return &authorizer.WireLimits{Reasoning: &r} }

// noEffort fails when an answer of the API names effort anywhere: from
// spec 048 every answer names the level reasoning.
func noEffort(t *testing.T, what string, body []byte) {
	t.Helper()
	if strings.Contains(string(body), `"effort"`) {
		t.Fatalf("%s answers effort: %s", what, body)
	}
}

// TestAVersionStoredBeforeTheRenameRunsWithItsDigest: an agent version
// v0.16.0 stored, each level under effort, is read with its digest
// unchanged: a session of it starts, takes a message, changes its level
// and resumes; the version reads answer its levels as reasoning; and the
// manifest applied again unchanged, under either name, keeps the version
// and writes nothing (spec 048).
func TestAVersionStoredBeforeTheRenameRunsWithItsDigest(t *testing.T) {
	f := newFixture(t)
	stored := f.putStoredBefore()

	read := f.do(http.MethodGet, "/v1/agents/careful/versions/1", "alice", "")
	if read.status != http.StatusOK {
		t.Fatalf("a version read: %d %s", read.status, read.body)
	}
	noEffort(t, "a version read", read.body)
	var doc v1.Agent
	read.decode(t, &doc)
	if doc.Status.Digest != stored.Version.Digest || doc.Spec.Model.Reasoning != "high" || doc.Spec.Advisor.Model.Reasoning != "medium" ||
		doc.Spec.Subagents[0].Spec.Model.Reasoning != "minimal" {
		t.Fatalf("the version reads %s", read.body)
	}

	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"careful","message":"Think."}`)
	if a.status != http.StatusCreated {
		t.Fatalf("a session of the stored version: %d %s", a.status, a.body)
	}
	var s session.Session
	a.decode(t, &s)
	if string(s.Agent.Digest) != stored.Version.Digest || s.Agent.Version != 1 || s.Model != nil {
		t.Fatalf("the session runs %+v, model %+v", s.Agent, s.Model)
	}
	if a := f.send(s.ID, "Again."); a.status != http.StatusOK {
		t.Fatalf("a send: %d %s", a.status, a.body)
	}
	a = f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"reasoning":"low"}}`)
	if a.status != http.StatusOK {
		t.Fatalf("a change of the level: %d %s", a.status, a.body)
	}
	a.decode(t, &s)
	if s.Model == nil || *s.Model != (session.ModelRef{Name: haiku, Reasoning: "low"}) {
		t.Fatalf("the change answers %+v", s.Model)
	}
	cost := int64(500)
	f.appendTo(s.ID, 1, session.SessionStatus{Status: session.StatusRunning}, session.ModelRequest{Model: haiku, CostUSDMicro: &cost},
		session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopBudget})
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/resume", "alice", `{"reason":"budget_raised","max_cost_usd_micro":5000000}`); a.status != http.StatusOK {
		t.Fatalf("a resume: %d %s", a.status, a.body)
	}

	reapply := map[string]string{
		"effort, as stored": stored.Manifest,
		"reasoning":         strings.ReplaceAll(stored.Manifest, "effort:", "reasoning:"),
	}
	for name, manifest := range reapply {
		a := f.do(http.MethodPut, "/v1/agents/careful", "alice", manifest)
		if a.status != http.StatusOK {
			t.Fatalf("%s: applied again: %d %s", name, a.status, a.body)
		}
		noEffort(t, name+": an apply", a.body)
		var got v1.Agent
		a.decode(t, &got)
		if got.Status.Version != 1 || got.Status.Digest != stored.Version.Digest {
			t.Fatalf("%s: applied again as %+v", name, got.Status)
		}
	}
	v, err := f.objects.Version(t.Context(), stored.Agent.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Doc) != stored.Version.Doc || string(v.Bundle) != stored.Version.Bundle {
		t.Fatal("applying the agent again rewrote the stored version")
	}
	if _, err := f.objects.Version(t.Context(), stored.Agent.ID, 2); err == nil {
		t.Fatal("applying the agent again made a second version")
	}
}

// TestEveryAnswerNamesTheLevelReasoning: no answer of the API names
// effort: an agent's apply, read, list and version read; a session's
// create, read, list, change, fork, end, archive, unarchive and resume;
// and session.model_changed in the events list and the stream, a change
// stored before the rename among them. What is stored keeps effort.
func TestEveryAnswerNamesTheLevelReasoning(t *testing.T) {
	f := newFixture(t)
	stored := f.putStoredBefore()
	l := f.level(func(req authz.Request) *authorizer.WireLimits {
		if req.Action == authorizer.ActionSessionCreate {
			return reasoning("low")
		}
		return nil
	})
	answers := map[string][]byte{}
	keep := func(what string, a answer, status int) answer {
		t.Helper()
		if a.status != status {
			t.Fatalf("%s: %d %s", what, a.status, a.body)
		}
		answers[what] = a.body
		return a
	}
	keep("an apply", f.do(http.MethodPut, "/v1/agents/careful", "alice", stored.Manifest), http.StatusOK)
	keep("an agent read", f.do(http.MethodGet, "/v1/agents/careful", "alice", ""), http.StatusOK)
	keep("an agent list", f.do(http.MethodGet, "/v1/agents", "alice", ""), http.StatusOK)
	keep("a version read", f.do(http.MethodGet, "/v1/agents/careful/versions/1", "alice", ""), http.StatusOK)

	var s session.Session
	keep("a create", f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"careful","message":"Think."}`), http.StatusCreated).decode(t, &s)
	if s.Model == nil || *s.Model != (session.ModelRef{Name: haiku, Reasoning: "low"}) {
		t.Fatalf("the create answers the model %+v", s.Model)
	}
	keep("a session read", f.do(http.MethodGet, "/v1/sessions/"+s.ID, "alice", ""), http.StatusOK)
	keep("a session list", f.do(http.MethodGet, "/v1/sessions", "alice", ""), http.StatusOK)
	keep("a change", f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"reasoning":"medium"}}`), http.StatusOK)

	// A change stored before the rename, as v0.16.0 wrote it.
	before := f.header(s.ID).LastSeq
	old := session.Event{ID: session.NewID(session.PrefixEvent), Type: session.TypeModelChanged, Time: time.Now().UTC(),
		Payload: json.RawMessage(`{"by":{"subject":"service:authorizer","kind":"service"},"old":{"name":"claude-haiku-4-5","effort":"medium"},"new":{"name":"claude-haiku-4-5","effort":"minimal"}}`)}
	batch := []session.Event{old}
	session.Stamp(s.ID, before, batch)
	if _, err := f.sessions.Append(t.Context(), s.ID, before, batch); err != nil {
		t.Fatal(err)
	}
	l.to(reasoning("high"))
	keep("a send", f.send(s.ID, "Think harder."), http.StatusOK)
	events := keep("an events list", f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/events", "alice", ""), http.StatusOK)
	var page struct {
		Items []session.Event `json:"items"`
	}
	events.decode(t, &page)
	var changes []session.ModelChanged
	for _, e := range page.Items {
		if e.Type == session.TypeModelChanged {
			var m session.ModelChanged
			if err := e.Decode(&m); err != nil {
				t.Fatal(err)
			}
			changes = append(changes, m)
		}
	}
	// The create appends no change: its level is the header's from the
	// start. The person's change, the stored one and the send's follow.
	want := [][2]string{{"low", "medium"}, {"medium", "minimal"}, {"minimal", "high"}}
	if len(changes) != len(want) {
		t.Fatalf("the list answers %d changes: %s", len(changes), events.body)
	}
	for i, c := range changes {
		if c.Old.Reasoning != want[i][0] || c.New.Reasoning != want[i][1] || c.Old.Effort != "" || c.New.Effort != "" {
			t.Fatalf("change %d answers %+v", i, c)
		}
	}
	l.to(nil)
	f.turn(s.ID, 1, "Thought.", 100)
	keep("a fork", f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", `{}`), http.StatusCreated)
	keep("an end", f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/end", "alice", `{"reason":"completed"}`), http.StatusOK)
	keep("an archive", f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/archive", "alice", ""), http.StatusOK)
	keep("an unarchive", f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/unarchive", "alice", ""), http.StatusOK)

	// The stream replays the log and closes after the end.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/v1/sessions/"+s.ID+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var frames strings.Builder
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 0, 1<<20), 1<<20)
	streamed := 0
	for lines.Scan() {
		line := lines.Text()
		if strings.HasPrefix(line, "event: "+string(session.TypeModelChanged)) {
			streamed++
		}
		frames.WriteString(line + "\n")
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	if streamed != len(want) || strings.Count(frames.String(), `"reasoning"`) < 2*len(want) {
		t.Fatalf("the stream carried %d changes: %s", streamed, frames.String())
	}
	answers["the stream"] = []byte(frames.String())

	// Another session of the agent stops on its budget and resumes.
	l.to(reasoning("minimal"))
	var r session.Session
	keep("a second create", f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"careful"}`), http.StatusCreated).decode(t, &r)
	cost := int64(500)
	f.appendTo(r.ID, 1, session.SessionStatus{Status: session.StatusRunning}, session.ModelRequest{Model: haiku, CostUSDMicro: &cost},
		session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopBudget})
	keep("a resume", f.do(http.MethodPost, "/v1/sessions/"+r.ID+"/resume", "alice", `{"reason":"budget_raised","max_cost_usd_micro":5000000}`), http.StatusOK)

	for what, body := range answers {
		noEffort(t, what, body)
		if what != "a send" && !strings.Contains(string(body), `"reasoning"`) {
			t.Errorf("%s answers no reasoning: %s", what, body)
		}
	}

	// What is stored keeps effort.
	if h := f.header(s.ID); h.Model == nil || h.Model.Effort != "high" || h.Model.Reasoning != "" {
		t.Fatalf("the stored header's model is %+v", h.Model)
	}
	for _, e := range f.log(s.ID) {
		if e.Type == session.TypeModelChanged && (!strings.Contains(string(e.Payload), `"effort"`) || strings.Contains(string(e.Payload), `"reasoning"`)) {
			t.Fatalf("a change is stored as %s", e.Payload)
		}
	}
}

// TestALevelIsReadUnderEitherName: a PATCH names the level reasoning, or
// effort as before; both with one level is one level, and both with two
// is invalid_request, as is a level outside the four under either name. A
// manifest that names both with two levels is invalid_manifest.
func TestALevelIsReadUnderEitherName(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	for body, level := range map[string]string{
		`{"model":{"reasoning":"high"}}`:                     "high",
		`{"model":{"effort":"low"}}`:                         "low",
		`{"model":{"reasoning":"medium","effort":"medium"}}`: "medium",
	} {
		a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", body)
		if a.status != http.StatusOK {
			t.Fatalf("%s: %d %s", body, a.status, a.body)
		}
		var got session.Session
		a.decode(t, &got)
		if got.Model == nil || got.Model.Reasoning != level || f.header(s.ID).Model.Effort != level {
			t.Fatalf("%s: answers %+v, stores %+v", body, got.Model, f.header(s.ID).Model)
		}
	}
	for _, body := range []string{`{"model":{"reasoning":"high","effort":"low"}}`, `{"model":{"reasoning":"max"}}`, `{"model":{"reasoning":null}}`, `{"model":{"effort":"High"}}`} {
		if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", body); a.code() != CodeInvalidRequest {
			t.Fatalf("%s: %d %s", body, a.status, a.body)
		}
	}
	doc := strings.Replace(agentYAML("both", "Answer."), "model: {name: claude-haiku-4-5}", "model: {name: claude-haiku-4-5, reasoning: high, effort: low}", 1)
	if a := f.do(http.MethodPut, "/v1/agents/both", "alice", doc); a.code() != "invalid_manifest" || !strings.Contains(string(a.body), "spec.model.reasoning") {
		t.Fatalf("a manifest of two levels: %d %s", a.status, a.body)
	}
}

// TestTheUpdateQuestionNamesTheLevelUnderBothNames: a change of the level
// asks session.update with the resolved level as effort and as reasoning,
// whichever name the body used, so an authorizer that reads either name
// decides it; a change of the model alone names neither.
func TestTheUpdateQuestionNamesTheLevelUnderBothNames(t *testing.T) {
	f := newFixture(t)
	f.applyModel("thinker", "name: "+haiku+", effort: low")
	s := f.create("alice", "thinker")
	l := f.level(func(authz.Request) *authorizer.WireLimits { return nil })
	for body, level := range map[string]string{
		`{"model":{"reasoning":"high"}}`: "high",
		`{"model":{"effort":"minimal"}}`: "minimal",
		`{"model":{"reasoning":""}}`:     "low",
	} {
		if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", body); a.status != http.StatusOK {
			t.Fatalf("%s: %d %s", body, a.status, a.body)
		}
		asked := l.last(t, authorizer.ActionSessionUpdate).Resource.Fields
		if asked["effort"] != level || asked["reasoning"] != level {
			t.Fatalf("%s asked about %v", body, asked)
		}
	}
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+sonnet+`"}}`); a.status != http.StatusOK {
		t.Fatalf("a switch: %d %s", a.status, a.body)
	}
	asked := l.last(t, authorizer.ActionSessionUpdate).Resource.Fields
	if _, e := asked["effort"]; e {
		t.Fatalf("a switch of the model alone asked about %v", asked)
	}
	if _, r := asked["reasoning"]; r {
		t.Fatalf("a switch of the model alone asked about %v", asked)
	}
}

// TestAnAuthorizersLevelMovesTheSession: an allow's limits.reasoning sets
// the level the session runs at: at a create, in the header with no
// event; at a send, with one session.model_changed by the service before
// the message, keeping the model; at a change, in place of the level the
// body names, "" being the agent's own. An allow without it keeps the
// level, a fork starts at its parent's level whatever its allow names,
// and a level outside the four is authorizer_unavailable.
func TestAnAuthorizersLevelMovesTheSession(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	l := f.level(func(authz.Request) *authorizer.WireLimits { return reasoning("high") })
	s := f.create("alice", "reviewer")
	if h := f.header(s.ID); h.Model == nil || *h.Model != (session.ModelRef{Name: haiku, Effort: "high"}) || len(f.modelEvents(s.ID)) != 0 {
		t.Fatalf("the create stored the model %+v and %d changes", h.Model, len(f.modelEvents(s.ID)))
	}

	l.to(reasoning("low"))
	before := f.header(s.ID).LastSeq
	if a := f.send(s.ID, "Quicker."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	evs := f.log(s.ID)
	if len(evs) != int(before)+2 || evs[before].Type != session.TypeModelChanged || evs[before+1].Type != session.TypeUserMessage {
		t.Fatalf("the send appended %d events", len(evs)-int(before))
	}
	changes := f.modelEvents(s.ID)
	if len(changes) != 1 || changes[0].By != (session.Sender{Subject: session.AuthorizerSubject, Kind: session.SenderService}) ||
		changes[0].Old != (session.ModelRef{Name: haiku, Effort: "high"}) || changes[0].New != (session.ModelRef{Name: haiku, Effort: "low"}) {
		t.Fatalf("session.model_changed %+v", changes)
	}
	// The same level again, and no level at all, append nothing.
	for _, w := range []*authorizer.WireLimits{reasoning("low"), nil} {
		l.to(w)
		if a := f.send(s.ID, "Again."); a.status != http.StatusOK {
			t.Fatalf("send: %d %s", a.status, a.body)
		}
	}
	if n := len(f.modelEvents(s.ID)); n != 1 {
		t.Fatalf("%d changes after a send that changed nothing", n)
	}
	// "" returns the session to its agent's own, the model's default.
	l.to(reasoning(""))
	if a := f.send(s.ID, "Your own pace."); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if changes := f.modelEvents(s.ID); len(changes) != 2 || changes[1].New != (session.ModelRef{Name: haiku}) {
		t.Fatalf("a return to the agent's own: %+v", changes)
	}
	// A change runs at the level its allow names in place of the one asked.
	l.to(reasoning("medium"))
	a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"reasoning":"high"}}`)
	var got session.Session
	if a.decode(t, &got); a.status != http.StatusOK || got.Model == nil || got.Model.Reasoning != "medium" {
		t.Fatalf("a change answered %d %s", a.status, a.body)
	}
	// A fork starts at its parent's level.
	f.turn(s.ID, 1, "Reviewed.", 100)
	l.to(reasoning("minimal"))
	var fork session.Session
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", `{}`); a.status != http.StatusCreated {
		t.Fatalf("a fork: %d %s", a.status, a.body)
	} else {
		a.decode(t, &fork)
	}
	if h := f.header(fork.ID); h.Model == nil || h.Model.Effort != "medium" {
		t.Fatalf("the fork stands on %+v", h.Model)
	}
	// A level the core cannot read is no limit it can apply.
	l.to(reasoning("max"))
	if a := f.send(s.ID, "Harder."); a.code() != "authorizer_unavailable" {
		t.Fatalf("a level outside the four: %d %s", a.status, a.body)
	}
}

// TestAnAllowOfTheAgentsOwnLevelAppendsNothing: a session whose agent
// names a level, answered "" or the agent's own level at its create and
// at every message, stands on the agent's own and appends no change.
func TestAnAllowOfTheAgentsOwnLevelAppendsNothing(t *testing.T) {
	f := newFixture(t)
	f.applyModel("thinker", "name: "+haiku+", effort: high")
	l := f.level(func(authz.Request) *authorizer.WireLimits { return reasoning("") })
	s := f.create("alice", "thinker")
	if s.Model != nil {
		t.Fatalf("a create at the agent's own level answers the model %+v", s.Model)
	}
	for _, w := range []*authorizer.WireLimits{reasoning(""), reasoning("high"), reasoning("")} {
		l.to(w)
		if a := f.send(s.ID, "Go on."); a.status != http.StatusOK {
			t.Fatalf("send: %d %s", a.status, a.body)
		}
	}
	if n := len(f.modelEvents(s.ID)); n != 0 || f.header(s.ID).Model != nil {
		t.Fatalf("%d changes, the header's model %+v", n, f.header(s.ID).Model)
	}
}

// TestARefusedSendSaysWhenItOpensAgain: a deny of a send for a bound that
// resets reaches the client with the deny's limits beside its reason, so
// the client reads when the send can be made again; a deny whose reason is
// not shown shows no limits either.
func TestARefusedSendSaysWhenItOpensAgain(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	reason := "rate_limited"
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionSend {
			return authz.Decision{Reason: reason, Limits: json.RawMessage(`{"resets_at":"2026-10-06T00:00:00Z"}`)}, nil
		}
		return (&auth.OwnerPolicy{}).Authorize(t.Context(), req)
	}
	a := f.send(s.ID, "One more.")
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Reason string `json:"reason"`
				Limits struct {
					ResetsAt time.Time `json:"resets_at"`
				} `json:"limits"`
			} `json:"details"`
		} `json:"error"`
	}
	a.decode(t, &e)
	if a.status != http.StatusForbidden || e.Error.Code != "forbidden" || e.Error.Details.Reason != reason ||
		!e.Error.Details.Limits.ResetsAt.Equal(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("a refused send: %d %s", a.status, a.body)
	}
	reason = "Not Today"
	if a := f.send(s.ID, "One more."); a.status != http.StatusForbidden || strings.Contains(string(a.body), "resets_at") || strings.Contains(string(a.body), `"reason"`) {
		t.Fatalf("a deny whose reason is not shown: %d %s", a.status, a.body)
	}
}
