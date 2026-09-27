// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/session"
)

func TestCreateASession(t *testing.T) {
	f := newFixture(t)
	a := f.apply("alice", "reviewer", "Review.")
	resp := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer@1","message":"Review main.go.","title":"main","metadata":{"ticket":"42"},"end_on_idle":true,"capture":{"requests":true},"budget":{"max_cost_usd_micro":500000},"limits":{"turn_timeout":"10m"}}`)
	if resp.status != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.status, resp.body)
	}
	var s session.Session
	resp.decode(t, &s)
	if s.Agent.ID != a.Status.ID || s.Agent.Version != 1 || s.Agent.Bundle == "" || s.Initiator.Subject != alice || s.Runner != session.RunnerHosted ||
		s.Machine.Kind != session.MachineCella || s.Title != "main" || s.Metadata["ticket"] != "42" || !s.EndOnIdle || !s.Capture.Requests ||
		*s.Budget.MaxCostUSDMicro != 500000 || s.Limits.TurnTimeout != "10m0s" || s.LastSeq != 1 {
		t.Fatalf("session %+v", s)
	}
	if link := resp.header.Get("Link"); link != `<https://topos.example/v1/sessions/`+s.ID+`/stream>; rel="stream"` {
		t.Fatalf("Link %q", link)
	}
	evs, err := f.sessions.Events(t.Context(), s.ID, 1, 0)
	if err != nil || len(evs) != 1 || evs[0].Type != session.TypeUserMessage {
		t.Fatalf("the first message: %+v, %v", evs, err)
	}
	for body, code := range map[string]string{
		`{}`: CodeInvalidRequest,
		`{"agent":"reviewer","runner":"external"}`:               CodeInvalidRequest,
		`{"agent":"reviewer","id":"ses_x"}`:                      CodeInvalidRequest,
		`{"agent":"reviewer","machine":{"kind":"host"}}`:         CodeInvalidRequest,
		`{"agent":"reviewer@x"}`:                                 CodeInvalidRequest,
		`{"agent":"reviewer@7"}`:                                 CodeNotFound,
		`{"agent":"nobody"}`:                                     CodeNotFound,
		`{"agent":"reviewer","limits":{"max_age":"soon"}}`:       CodeInvalidRequest,
		`{"agent":"reviewer","metadata":` + manyMetadata() + `}`: CodeInvalidRequest,
	} {
		if got := f.do(http.MethodPost, "/v1/sessions", "alice", body); got.code() != code {
			t.Errorf("%s: %d %s, want %s", body, got.status, got.body, code)
		}
	}
	local := strings.Replace(agentYAML("local", "x"), "  machine: {kind: cella}\n", "", 1)
	if got := f.do(http.MethodPut, "/v1/agents/local", "alice", local); got.status != http.StatusCreated {
		t.Fatalf("a host agent: %d %s", got.status, got.body)
	}
	if got := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"local"}`); got.status != http.StatusUnprocessableEntity || got.code() != CodeMachineUnavailable {
		t.Fatalf("a hosted session of a host agent: %d %s", got.status, got.body)
	}
	if got := f.do(http.MethodPost, "/v1/sessions", "bob", `{"agent":"reviewer"}`); got.code() != auth.CodeForbidden {
		t.Fatalf("a session of another subject's agent: %d %s", got.status, got.body)
	}
}

func manyMetadata() string {
	m := map[string]string{}
	for i := range session.MaxMetadata + 1 {
		m[string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestLimitsApplyAtCreate: an allow's limits lower the session's budget,
// turn timeout and age, set its retention and scope, and limits that do
// not decode refuse the create.
func TestLimitsApplyAtCreate(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	limits := `{"budget_usd_micro":1000,"turn_timeout":"5m","max_age":"1h","retention":"720h","scope":[{"action":"repo.read","resource":"*"}]}`
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		d := authz.Decision{Allow: true}
		if req.Action == authorizer.ActionSessionCreate {
			d.Limits = json.RawMessage(limits)
		}
		return d, nil
	}
	var s session.Session
	f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","budget":{"max_cost_usd_micro":900000},"limits":{"turn_timeout":"30m"}}`).decode(t, &s)
	if *s.Budget.MaxCostUSDMicro != 1000 || s.Limits.TurnTimeout != "5m0s" || s.Limits.MaxAge != "1h0m0s" || s.Limits.Retention != "720h0m0s" ||
		len(s.Scope) != 1 || !s.ExpiresAt.Equal(s.CreatedAt.Add(time.Hour)) {
		t.Fatalf("session %+v", s)
	}
	limits = `{"turn_timeout":"soon"}`
	if a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`); a.code() != auth.CodeAuthorizerUnavailable {
		t.Fatalf("unreadable limits: %d %s", a.status, a.body)
	}
}

// TestSessionCreateCarriesTheAgentsPermissions: session.create names
// the permissions of the agent version the session pins, so the
// authorizer can hold them to what the initiator may do; a pinned older
// version carries its own, and an agent with none carries an empty list.
func TestSessionCreateCarriesTheAgentsPermissions(t *testing.T) {
	f := newFixture(t)
	withPermissions := func(perms string) string {
		return "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: reviewer\nspec:\n  model: {name: claude-haiku-4-5}\n  machine: {kind: cella}\n" + perms
	}
	for _, body := range []string{
		withPermissions("  permissions:\n    - {action: repo.read, resource: \"repo:acme/*\"}\n"),
		withPermissions("  permissions:\n    - {action: repo.read, resource: \"repo:acme/*\"}\n    - {action: repo.write, resource: \"repo:acme/web\"}\n"),
	} {
		if a := f.do(http.MethodPut, "/v1/agents/reviewer", "alice", body); a.status != http.StatusCreated && a.status != http.StatusOK {
			t.Fatalf("apply: %d %s", a.status, a.body)
		}
	}
	f.apply("alice", "plain", "Review.")
	var asked []any
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionCreate {
			asked = append(asked, req.Resource.Fields["permissions"])
		}
		return authz.Decision{Allow: true}, nil
	}
	for _, agent := range []string{"reviewer", "reviewer@1", "plain"} {
		f.create("alice", agent)
	}
	want := []any{
		[]any{map[string]any{"action": "repo.read", "resource": "repo:acme/*"}, map[string]any{"action": "repo.write", "resource": "repo:acme/web"}},
		[]any{map[string]any{"action": "repo.read", "resource": "repo:acme/*"}},
		[]any{},
	}
	if !reflect.DeepEqual(asked, want) {
		t.Fatalf("session.create carried the permissions\n%#v\nwant\n%#v", asked, want)
	}
}

// TestSendSetsSender: the appended event's sender is the verified
// subject whatever the body says; a type a person does not send, a
// malformed payload and a send to an ended session are refused.
func TestSendSetsSender(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", `{"type":"user.message","payload":{"sender":{"subject":"`+root+`","kind":"person"},"content":[{"type":"text","text":"And tests."}]}}`)
	var ev session.Event
	a.decode(t, &ev)
	var p session.UserMessage
	if err := ev.Decode(&p); a.status != http.StatusOK || err != nil || p.Sender.Subject != alice || ev.Seq != 2 {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	for body, code := range map[string]string{
		`{"type":"agent.message","payload":{}}`:                                              CodeInvalidRequest,
		`{"type":"user.message","payload":{"content":[]}}`:                                   CodeInvalidRequest,
		`{"type":"user.message","payload":{"bogus":1}}`:                                      CodeInvalidRequest,
		`{"type":"user.tool_confirmation","payload":{"tool_use_id":"t","decision":"maybe"}}`: CodeInvalidRequest,
		`{"type":"user.tool_result","payload":{}}`:                                           CodeInvalidRequest,
	} {
		if got := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", body); got.code() != code {
			t.Errorf("%s: %d %s", body, got.status, got.body)
		}
	}
	// An ask and a client's call wait for their answers; each takes one,
	// of its own kind.
	for _, use := range []session.AgentToolUse{
		{ToolUseID: "toolu_1", Name: "bash", Input: json.RawMessage(`{}`), Verdict: "ask"},
		{ToolUseID: "toolu_2", Name: "pick", Input: json.RawMessage(`{}`), Verdict: "allow", Client: true},
	} {
		cur, err := f.sessions.Get(t.Context(), s.ID)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := session.NewEvent(session.TypeAgentToolUse, use, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		batch := []session.Event{ev}
		session.Stamp(s.ID, cur.LastSeq, batch)
		if _, err := f.sessions.Append(t.Context(), s.ID, cur.LastSeq, batch); err != nil {
			t.Fatal(err)
		}
	}
	for body, want := range map[string]int{
		`{"type":"user.tool_result","payload":{"tool_use_id":"toolu_1"}}`:                          http.StatusConflict,
		`{"type":"user.tool_confirmation","payload":{"tool_use_id":"toolu_2","decision":"allow"}}`: http.StatusConflict,
		`{"type":"user.tool_confirmation","payload":{"tool_use_id":"toolu_9","decision":"allow"}}`: http.StatusConflict,
	} {
		if got := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", body); got.status != want {
			t.Errorf("%s: %d %s", body, got.status, got.body)
		}
	}
	for _, body := range []string{
		`{"type":"user.tool_confirmation","payload":{"tool_use_id":"toolu_1","decision":"allow"}}`,
		`{"type":"user.tool_result","payload":{"tool_use_id":"toolu_2"}}`,
	} {
		if got := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", body); got.status != http.StatusOK {
			t.Errorf("%s: %d %s", body, got.status, got.body)
		}
		if got := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", body); got.code() != CodeConflict {
			t.Errorf("a second answer %s: %d %s", body, got.status, got.body)
		}
	}
	f.authz.take()
	if got := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", `{"type":"user.interrupt"}`); got.status != http.StatusOK {
		t.Fatalf("an interrupt: %d %s", got.status, got.body)
	}
	if asked := f.authz.take(); len(asked) != 1 || asked[0] != authorizer.ActionSessionInterrupt {
		t.Fatalf("an interrupt asked %v", asked)
	}
	if got := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "bob", `{"type":"user.interrupt"}`); got.status != http.StatusNotFound {
		t.Fatalf("another subject's send: %d %s", got.status, got.body)
	}
	if got := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/end", "alice", `{"reason":"completed"}`); got.status != http.StatusOK {
		t.Fatalf("end: %d %s", got.status, got.body)
	}
	if got := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", `{"type":"user.interrupt"}`); got.code() != CodeConflict {
		t.Fatalf("a send to an ended session: %d %s", got.status, got.body)
	}
}

func TestEndAndDelete(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/end", "alice", `{"reason":"paused"}`); a.code() != CodeInvalidRequest {
		t.Fatalf("a reason that ends nothing: %d %s", a.status, a.body)
	}
	ev, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	batch := []session.Event{ev}
	session.Stamp(s.ID, 1, batch)
	if _, err := f.sessions.Append(t.Context(), s.ID, 1, batch); err != nil {
		t.Fatal(err)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/end", "alice", `{"reason":"canceled"}`); a.code() != CodeConflict {
		t.Fatalf("an end of a running session: %d %s", a.status, a.body)
	}
	other := f.create("alice", "reviewer")
	var ended session.Session
	a := f.do(http.MethodPost, "/v1/sessions/"+other.ID+"/end", "alice", `{"reason":"canceled"}`)
	a.decode(t, &ended)
	if ended.Status != session.StatusEnded || ended.StopReason != session.StopCanceled {
		t.Fatalf("ended %+v", ended)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+other.ID+"/end", "alice", `{"reason":"canceled"}`); a.code() != CodeConflict {
		t.Fatalf("a second end: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodDelete, "/v1/sessions/"+other.ID, "alice", ""); a.status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodGet, "/v1/sessions/"+other.ID, "alice", ""); a.status != http.StatusNotFound {
		t.Fatalf("a deleted session: %d", a.status)
	}
}

// TestDeniedReadAnswersAsMissing: another subject's read of a session
// answers exactly what a read of no session does.
func TestDeniedReadAnswersAsMissing(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	missing := f.do(http.MethodGet, "/v1/sessions/"+session.NewID(session.PrefixSession), "bob", "")
	for _, path := range []string{"/v1/sessions/" + s.ID, "/v1/sessions/" + s.ID + "/events", "/v1/sessions/" + s.ID + "/stream", "/v1/sessions/" + s.ID + "/blobs/" + string(s.Agent.Digest)} {
		denied := f.do(http.MethodGet, path, "bob", "")
		if denied.status != http.StatusNotFound || string(denied.body) != string(missing.body) {
			t.Errorf("GET %s as bob: %d %s, a missing session answers %s", path, denied.status, denied.body, missing.body)
		}
	}
	if a := f.do(http.MethodGet, "/v1/sessions/"+s.ID, "root", ""); a.status != http.StatusOK {
		t.Fatalf("the admin's read: %d", a.status)
	}
}

// TestAuthorizerDownIsRefusal: with the authorizer unreachable every
// route that asks answers authorizer_unavailable, and nothing is written.
func TestAuthorizerDownIsRefusal(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	before, err := f.sessions.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.authz.answer = func(authz.Request) (authz.Decision, error) {
		return authz.Decision{}, &authz.Unavailable{URL: "http://authz", Err: errors.New("connection refused")}
	}
	for _, rt := range table() {
		if rt.public {
			continue
		}
		path := strings.NewReplacer("{name}", "reviewer", "{ref}", "reviewer", "{n}", "1", "{id}", s.ID, "{digest}", string(s.Agent.Digest), "{event_id}", before[0].ID).Replace(rt.path)
		body := map[string]string{
			"applyAgent": agentYAML("reviewer", "Changed."), "createSession": `{"agent":"reviewer"}`, "endSession": `{"reason":"canceled"}`,
			"sendEvent": `{"type":"user.interrupt"}`, "redactEvent": `{"reason":"x"}`, "resumeSession": `{}`,
		}[rt.op]
		if a := f.do(rt.method, "/v1"+path, "alice", body); a.status != http.StatusServiceUnavailable || a.code() != auth.CodeAuthorizerUnavailable {
			t.Errorf("%s: %d %s", rt.op, a.status, a.body)
		}
	}
	after, err := f.sessions.Events(t.Context(), s.ID, 1, 0)
	if err != nil || len(after) != len(before) {
		t.Fatalf("the log moved: %d events, then %d, %v", len(before), len(after), err)
	}
	if a, err := f.objects.Agent(t.Context(), "reviewer"); err != nil || a.Latest != 1 || a.ArchivedAt != nil {
		t.Fatalf("the agent moved: %+v, %v", a, err)
	}
	if _, err := f.sessions.Get(t.Context(), s.ID); err != nil {
		t.Fatalf("the session is gone: %v", err)
	}
}

// TestIdempotencyKey: a repeat answers the first answer and creates
// nothing; the same key with another body is refused; another subject's
// key is its own.
func TestIdempotencyKey(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	body := `{"agent":"reviewer","message":"Once."}`
	first := f.do(http.MethodPost, "/v1/sessions", "alice", body, "Idempotency-Key", "k1")
	again := f.do(http.MethodPost, "/v1/sessions", "alice", body, "Idempotency-Key", "k1")
	if first.status != http.StatusCreated || again.status != http.StatusCreated || string(first.body) != string(again.body) || again.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("first %d %s, again %d %s", first.status, first.body, again.status, again.body)
	}
	all, _, err := f.sessions.List(t.Context(), session.ListOptions{})
	if err != nil || len(all) != 1 {
		t.Fatalf("%d sessions, %v", len(all), err)
	}
	if a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","message":"Twice."}`, "Idempotency-Key", "k1"); a.code() != CodeIdempotencyConflict {
		t.Fatalf("another body: %d %s", a.status, a.body)
	}
	bad := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"nobody"}`, "Idempotency-Key", "k2")
	if again := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"nobody"}`, "Idempotency-Key", "k2"); bad.status != http.StatusNotFound || string(again.body) != string(bad.body) {
		t.Fatalf("a refused answer is kept: %d %s, again %s", bad.status, bad.body, again.body)
	}
	if a := f.do(http.MethodPost, "/v1/sessions", "bob", body, "Idempotency-Key", "k1"); a.code() != auth.CodeForbidden {
		t.Fatalf("bob's key k1 is bob's: %d %s", a.status, a.body)
	}
}

// TestStreamReplayThenLive: a stream from sequence 1 on a replica other
// than the one that took the writes replays the log, then delivers each
// new event in order, and closes after the event that ends the session.
// A reconnect with Last-Event-ID loses and repeats nothing.
func TestStreamReplayThenLive(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	other, err := New(Options{Sessions: f.sessions, Objects: f.objects, Verifier: tokens{}, Guard: auth.Guard{Authorizer: &auth.OwnerPolicy{}}, PublicURL: "https://topos.example", Heartbeat: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	replica := httptest.NewServer(other.Handler())
	t.Cleanup(replica.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, replica.URL+"/v1/sessions/"+s.ID+"/stream?from_seq=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := replica.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	frames := make(chan sseFrame, 16)
	go readFrames(resp.Body, frames)
	if fr := <-frames; fr.id != "1" || fr.event != string(session.TypeUserMessage) {
		t.Fatalf("the replay: %+v", fr)
	}
	f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", `{"type":"user.message","payload":{"content":[{"type":"text","text":"More."}]}}`)
	if fr := <-frames; fr.id != "2" {
		t.Fatalf("the live event: %+v", fr)
	}
	f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/end", "alice", `{"reason":"completed"}`)
	if fr := <-frames; fr.id != "3" || fr.event != string(session.TypeSessionStatus) {
		t.Fatalf("the ending event: %+v", fr)
	}
	if fr, open := <-frames; open {
		t.Fatalf("the stream went on after the session ended: %+v", fr)
	}

	a := f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/stream", "alice", "", "Last-Event-ID", "1")
	var ids []string
	for _, fr := range parseFrames(string(a.body)) {
		ids = append(ids, fr.id)
	}
	if strings.Join(ids, ",") != "2,3" {
		t.Fatalf("a resumed stream held %v", ids)
	}
	for _, h := range [][]string{{"Last-Event-ID", "x"}} {
		if a := f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/stream", "alice", "", h...); a.code() != CodeInvalidRequest {
			t.Errorf("%v: %d %s", h, a.status, a.body)
		}
	}
	for _, q := range []string{"?from_seq=0", "?deltas=2"} {
		if a := f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/stream"+q, "alice", ""); a.code() != CodeInvalidRequest {
			t.Errorf("%s: %d %s", q, a.status, a.body)
		}
	}
}

// TestStreamKeepsAlive: an idle stream carries a comment line.
func TestStreamKeepsAlive(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.srv.URL+"/v1/sessions/"+s.ID+"/stream?from_seq=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer alice")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != ": keepalive\n" {
		t.Fatalf("first line %q, %v", line, err)
	}
}

// openStream opens the stream of session id as token and returns the
// response and the function that hangs it up.
func (f *fixture) openStream(id, token string) (*http.Response, context.CancelFunc) {
	f.t.Helper()
	ctx, cancel := context.WithCancel(f.t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/v1/sessions/"+id+"/stream?from_seq=2", nil)
	if err != nil {
		cancel()
		f.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		cancel()
		f.t.Fatal(err)
	}
	hangUp := func() {
		cancel()
		if err := resp.Body.Close(); err != nil {
			f.t.Error(err)
		}
	}
	f.t.Cleanup(hangUp)
	return resp, hangUp
}

// TestStreamsPerSubjectAreCapped: a subject holds at most
// StreamsPerSubject streams open at once, the one past it is refused
// rate_limited while another subject still opens one, and a stream that
// ends frees its slot.
func TestStreamsPerSubjectAreCapped(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	var hangUps []context.CancelFunc
	for i := range StreamsPerSubject {
		resp, hangUp := f.openStream(s.ID, "alice")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d within the cap: %d", i, resp.StatusCode)
		}
		hangUps = append(hangUps, hangUp)
	}
	over, _ := f.openStream(s.ID, "alice")
	if over.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the stream past the cap: %d", over.StatusCode)
	}
	body, err := io.ReadAll(over.Body)
	if err != nil {
		t.Fatal(err)
	}
	a := answer{status: over.StatusCode, header: over.Header, body: body}
	var e struct {
		Error struct {
			Details struct {
				Detail string `json:"detail"`
			} `json:"details"`
		} `json:"error"`
	}
	a.decode(t, &e)
	if a.code() != CodeRateLimited || !strings.Contains(e.Error.Details.Detail, fmt.Sprint(StreamsPerSubject)) {
		t.Fatalf("the stream past the cap: %s", a.body)
	}
	if resp, _ := f.openStream(s.ID, "root"); resp.StatusCode != http.StatusOK {
		t.Fatalf("another subject's stream: %d", resp.StatusCode)
	}
	hangUps[0]()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, _ := f.openStream(s.ID, "alice")
		if resp.StatusCode == http.StatusOK {
			break
		}
		if resp.StatusCode != http.StatusTooManyRequests || time.Now().After(deadline) {
			t.Fatalf("the stream after one ended: %d", resp.StatusCode)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if resp, _ := f.openStream(s.ID, "alice"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the freed slot was given twice: %d", resp.StatusCode)
	}
}

func TestStreamSlots(t *testing.T) {
	two := newSlots(2)
	first, ok := two.take("alice")
	if !ok {
		t.Fatal("the first slot")
	}
	if _, ok := two.take("alice"); !ok {
		t.Fatal("the second slot")
	}
	if _, ok := two.take("alice"); ok {
		t.Fatal("a third slot of two")
	}
	first()
	first()
	if _, ok := two.take("alice"); !ok {
		t.Fatal("the slot a release freed")
	}
	if _, ok := two.take("alice"); ok {
		t.Fatal("a release that ran twice freed two slots")
	}
	none := newSlots(-1)
	for range 3 * StreamsPerSubject {
		if _, ok := none.take("alice"); !ok {
			t.Fatal("a negative limit bounds nothing")
		}
	}
}

type sseFrame struct{ id, event, data string }

func readFrames(r interface{ Read([]byte) (int, error) }, out chan<- sseFrame) {
	defer close(out)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var fr sseFrame
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if fr.id != "" {
				out <- fr
			}
			fr = sseFrame{}
		case strings.HasPrefix(line, "id: "):
			fr.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			fr.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			fr.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func parseFrames(body string) []sseFrame {
	ch := make(chan sseFrame, 64)
	readFrames(strings.NewReader(body), ch)
	var out []sseFrame
	for fr := range ch {
		out = append(out, fr)
	}
	return out
}

func TestEventsBlobsAndRedaction(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	for range 3 {
		f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", `{"type":"user.message","payload":{"content":[{"type":"text","text":"x"}]}}`)
	}
	var seqs []uint64
	path := "/v1/sessions/" + s.ID + "/events?limit=3"
	for range 4 {
		var page struct {
			Items      []session.Event `json:"items"`
			NextCursor string          `json:"next_cursor"`
		}
		f.do(http.MethodGet, path, "alice", "").decode(t, &page)
		for _, e := range page.Items {
			seqs = append(seqs, e.Seq)
		}
		if page.NextCursor == "" {
			break
		}
		path = "/v1/sessions/" + s.ID + "/events?limit=3&cursor=" + page.NextCursor
	}
	if len(seqs) != 4 || seqs[0] != 1 || seqs[3] != 4 {
		t.Fatalf("the pages held %v", seqs)
	}
	var tail struct {
		Items []session.Event `json:"items"`
	}
	f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/events?from_seq=9", "alice", "").decode(t, &tail)
	if tail.Items == nil || len(tail.Items) != 0 {
		t.Fatalf("past the end: %+v", tail)
	}
	for _, q := range []string{"?from_seq=x", "?cursor=%21%21", "?cursor=eA"} {
		if a := f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/events"+q, "alice", ""); a.code() != CodeInvalidRequest {
			t.Errorf("%s: %d %s", q, a.status, a.body)
		}
	}

	b := f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/blobs/"+string(s.Agent.Bundle), "alice", "")
	if b.status != http.StatusOK || session.DigestOf(b.body) != s.Agent.Bundle || b.header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("blob: %d", b.status)
	}
	if a := f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/blobs/nope", "alice", ""); a.code() != CodeInvalidRequest {
		t.Fatalf("a malformed digest: %d", a.status)
	}
	if a := f.do(http.MethodGet, "/v1/sessions/"+s.ID+"/blobs/"+string(session.DigestOf([]byte("none"))), "alice", ""); a.status != http.StatusNotFound {
		t.Fatalf("a missing blob: %d", a.status)
	}

	evs, err := f.sessions.Events(t.Context(), s.ID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events/"+evs[0].ID+"/redact", "alice", `{"reason":"a token"}`); a.status != http.StatusNoContent {
		t.Fatalf("redact: %d %s", a.status, a.body)
	}
	redacted, err := f.sessions.Events(t.Context(), s.ID, 1, 1)
	if err != nil || strings.Contains(string(redacted[0].Payload), "Review main.go.") {
		t.Fatalf("the event still holds its content: %s, %v", redacted[0].Payload, err)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events/evt_none/redact", "alice", `{"reason":"x"}`); a.code() != CodeNotFound {
		t.Fatalf("a redaction of no event: %d %s", a.status, a.body)
	}
	status, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cur, err := f.sessions.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	batch := []session.Event{status}
	session.Stamp(s.ID, cur.LastSeq, batch)
	if _, err := f.sessions.Append(t.Context(), s.ID, cur.LastSeq, batch); err != nil {
		t.Fatal(err)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events/"+batch[0].ID+"/redact", "alice", `{"reason":"x"}`); a.code() != CodeInvalidRequest {
		t.Fatalf("a redaction of a status: %d %s", a.status, a.body)
	}
}

func TestListSessions(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	f.apply("alice", "writer", "Write.")
	f.apply("bob", "helper", "Help.")
	mine := []string{f.create("alice", "reviewer").ID, f.create("alice", "writer").ID, f.create("alice", "reviewer").ID}
	f.create("bob", "helper")
	list := func(token, q string) []string {
		var page struct {
			Items []session.Session `json:"items"`
		}
		a := f.do(http.MethodGet, "/v1/sessions"+q, token, "")
		if a.status != http.StatusOK {
			t.Fatalf("list %s: %d %s", q, a.status, a.body)
		}
		a.decode(t, &page)
		var ids []string
		for _, s := range page.Items {
			ids = append(ids, s.ID)
		}
		return ids
	}
	// A list runs newest first.
	if got := list("alice", ""); len(got) != 3 || got[0] != mine[2] {
		t.Fatalf("alice lists %v", got)
	}
	if got := list("root", ""); len(got) != 4 {
		t.Fatalf("the admin lists %v", got)
	}
	if got := list("alice", "?agent=reviewer"); len(got) != 2 || got[1] != mine[0] {
		t.Fatalf("by agent: %v", got)
	}
	if got := list("alice", "?agent=nobody"); len(got) != 0 {
		t.Fatalf("an unknown agent: %v", got)
	}
	if got := list("alice", "?status=ended&runner=hosted"); len(got) != 0 {
		t.Fatalf("ended: %v", got)
	}
	for _, q := range []string{"?status=gone", "?runner=elsewhere", "?limit=x"} {
		if a := f.do(http.MethodGet, "/v1/sessions"+q, "alice", ""); a.code() != CodeInvalidRequest {
			t.Errorf("%s: %d %s", q, a.status, a.body)
		}
	}
}

// TestInputNotifiesTheRunners: a first message and a sent event each
// wake the server's runners; a session with no message does not.
func TestInputNotifiesTheRunners(t *testing.T) {
	var calls atomic.Int32
	f := newFixture(t, func(o *Options) { o.Notify = func() { calls.Add(1) } })
	f.apply("alice", "reviewer", "Review.")
	if a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer"}`); a.status != http.StatusCreated || calls.Load() != 0 {
		t.Fatalf("a session with no message: %d, %d calls", a.status, calls.Load())
	}
	s := f.create("alice", "reviewer")
	f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", `{"type":"user.interrupt"}`)
	if calls.Load() != 2 {
		t.Fatalf("%d calls, want 2", calls.Load())
	}
}

// TestHostSessionsBehindTheSwitch: a session of a host agent is refused
// machine_unavailable unless host sessions are on; with them on it is
// created on the host, still refused when its agent names roots or read
// paths, and its deletion reaches the server's clean-up.
func TestHostSessionsBehindTheSwitch(t *testing.T) {
	hostAgent := func(name, extra string) string {
		return strings.Replace(agentYAML(name, "x"), "  machine: {kind: cella}\n", "  machine: {kind: host"+extra+"}\n", 1)
	}
	off := newFixture(t)
	if got := off.do(http.MethodPut, "/v1/agents/local", "alice", hostAgent("local", "")); got.status != http.StatusCreated {
		t.Fatalf("a host agent: %d %s", got.status, got.body)
	}
	if got := off.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"local"}`); got.code() != CodeMachineUnavailable || !strings.Contains(string(got.body), "machine_unavailable") {
		t.Fatalf("a host session with the switch off: %d %s", got.status, got.body)
	}
	var deleted []string
	on := newFixture(t, func(o *Options) {
		o.HostSessions = true
		o.Deleted = func(id string) error {
			deleted = append(deleted, id)
			return errors.New("the directory is busy")
		}
	})
	for name, extra := range map[string]string{"local": "", "rooted": ", roots: [/srv]", "reading": ", readPaths: [/etc]"} {
		if got := on.do(http.MethodPut, "/v1/agents/"+name, "alice", hostAgent(name, extra)); got.status != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, got.status, got.body)
		}
	}
	for _, name := range []string{"rooted", "reading"} {
		if got := on.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"`+name+`"}`); got.code() != CodeMachineUnavailable {
			t.Fatalf("a host agent that names %s: %d %s", name, got.status, got.body)
		}
	}
	resp := on.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"local"}`)
	var s session.Session
	resp.decode(t, &s)
	if resp.status != http.StatusCreated || s.Machine.Kind != session.MachineHost || s.Machine.Workdir != "" {
		t.Fatalf("a host session with the switch on: %d %s", resp.status, resp.body)
	}
	if a := on.do(http.MethodDelete, "/v1/sessions/"+s.ID, "alice", ""); a.status != http.StatusNoContent || len(deleted) != 1 || deleted[0] != s.ID {
		t.Fatalf("delete: %d %s, cleaned %v", a.status, a.body, deleted)
	}
}

// TestResumeAfterTheCapIsRaised: a session idle on its budget resumes
// with a raised cap, which session.resumed carries into the header and
// makes pending input; any other status, a budget already spent and a
// budget of zero are refused (spec 007).
func TestResumeAfterTheCapIsRaised(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	resume := func(body string) answer {
		return f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/resume", "alice", body)
	}
	if a := resume(`{}`); a.code() != CodeConflict {
		t.Fatalf("a resume of a session not stopped on its budget: %d %s", a.status, a.body)
	}
	cost := int64(500)
	var batch []session.Event
	for _, e := range []struct {
		typ     session.Type
		payload any
	}{
		{session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning}},
		{session.TypeModelRequest, session.ModelRequest{CostUSDMicro: &cost}},
		{session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopBudget}},
	} {
		ev, err := session.NewEvent(e.typ, e.payload, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		batch = append(batch, ev)
	}
	session.Stamp(s.ID, 1, batch)
	if _, err := f.sessions.Append(t.Context(), s.ID, 1, batch); err != nil {
		t.Fatal(err)
	}
	if a := resume(`{"max_cost_usd_micro":0}`); a.code() != CodeInvalidRequest {
		t.Fatalf("a budget of zero: %d %s", a.status, a.body)
	}
	if a := resume(`{"max_cost_usd_micro":400}`); a.code() != CodeConflict {
		t.Fatalf("a budget already spent: %d %s", a.status, a.body)
	}
	var resumed session.Session
	a := resume(`{"reason":"budget_raised","max_cost_usd_micro":1000}`)
	a.decode(t, &resumed)
	if a.status != http.StatusOK || resumed.Budget.MaxCostUSDMicro == nil || *resumed.Budget.MaxCostUSDMicro != 1000 {
		t.Fatalf("resume: %d %s", a.status, a.body)
	}
	evs, err := f.sessions.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	var p session.SessionResumed
	if last.Type != session.TypeSessionResumed || last.Decode(&p) != nil || !strings.HasSuffix(p.By.Subject, "|alice") || p.Reason != "budget_raised" {
		t.Fatalf("the last event %s %s", last.Type, last.Payload)
	}
	if !session.HasPendingInput(evs) {
		t.Fatal("a resumed session holds no pending input")
	}
}
