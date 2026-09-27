// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	for _, body := range []string{
		`{"type":"user.tool_confirmation","payload":{"tool_use_id":"toolu_1","decision":"allow"}}`,
		`{"type":"user.tool_result","payload":{"tool_use_id":"toolu_2"}}`,
	} {
		if got := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", body); got.status != http.StatusOK {
			t.Errorf("%s: %d %s", body, got.status, got.body)
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
	running := s
	running.ID = ""
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
			"sendEvent": `{"type":"user.interrupt"}`, "redactEvent": `{"reason":"x"}`,
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
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events/evt_none/redact", "alice", `{"reason":"x"}`); a.status < 400 {
		t.Fatalf("a redaction of no event: %d", a.status)
	}
}
