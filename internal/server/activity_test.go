// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/session"
)

// sendersOf is every sender a session's log records, by event type, in
// log order.
func (f *fixture) sendersOf(id string) map[session.Type][]session.Sender {
	f.t.Helper()
	out := map[session.Type][]session.Sender{}
	for _, e := range f.log(id) {
		var p struct {
			Sender *session.Sender `json:"sender"`
			By     *session.Sender `json:"by"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			f.t.Fatal(err)
		}
		if p.Sender != nil {
			out[e.Type] = append(out[e.Type], *p.Sender)
		}
		if p.By != nil {
			out[e.Type] = append(out[e.Type], *p.By)
		}
	}
	return out
}

// TestSendersCarryTheCallersClient: every sender the API records for a
// caller carries the client of the caller's token (spec 085), the
// session's initiator, a message, an interrupt, a rename and a fork's
// message alike; a token that names no client writes none, and what a
// body says of its sender is not what is recorded.
func TestSendersCarryTheCallersClient(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice+web", "reviewer")
	if s.Initiator.Client != "web" {
		t.Fatalf("the initiator is %+v, want the web client", s.Initiator)
	}
	f.turn(s.ID, 1, "Done.", 1)
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice+desk", `{"type":"user.message","payload":{"content":[{"type":"text","text":"More."}]}}`); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice+desk", `{"type":"user.interrupt","payload":{}}`); a.status != http.StatusOK {
		t.Fatalf("interrupt: %d %s", a.status, a.body)
	}
	f.turn(s.ID, 2, "Stopped.", 1)
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice+web", `{"title":"Review"}`); a.status != http.StatusOK {
		t.Fatalf("rename: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", `{"type":"user.message","payload":{"content":[{"type":"text","text":"Again."}]}}`); a.status != http.StatusOK {
		t.Fatalf("send without a client: %d %s", a.status, a.body)
	}
	got := f.sendersOf(s.ID)
	clients := func(senders []session.Sender) []string {
		out := []string{}
		for _, s := range senders {
			out = append(out, s.Client)
		}
		return out
	}
	for typ, want := range map[session.Type][]string{
		session.TypeUserMessage:   {"web", "desk", ""},
		session.TypeUserInterrupt: {"desk"},
		session.TypeTitleChanged:  {"web"},
	} {
		if c := clients(got[typ]); !reflect.DeepEqual(c, want) {
			t.Errorf("%s senders' clients are %q, want %q", typ, c, want)
		}
	}
	// The message sent with no client has none in its payload.
	evs := f.log(s.ID)
	if last := evs[len(evs)-1]; strings.Contains(string(last.Payload), `"client"`) {
		t.Errorf("a message of a token with no client records one: %s", last.Payload)
	}
	// A fork's message is the forker's, from the forker's client.
	fork := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice+phone", `{"message":{"content":[{"type":"text","text":"Instead."}]}}`)
	if fork.status != http.StatusCreated {
		t.Fatalf("fork: %d %s", fork.status, fork.body)
	}
	var forked session.Session
	fork.decode(t, &forked)
	if forked.Initiator.Client != "phone" {
		t.Errorf("the fork's initiator is %+v, want the phone client", forked.Initiator)
	}
	messages := f.sendersOf(forked.ID)[session.TypeUserMessage]
	if len(messages) == 0 || messages[len(messages)-1].Client != "phone" {
		t.Errorf("the fork's own message's sender is %+v, want the phone client", messages)
	}
	// The sender is the server's to write: a body naming one is recorded
	// with the verified caller and client whatever it said.
	a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice+web", `{"type":"user.message","payload":{"sender":{"subject":"x","kind":"person","client":"forged"},"content":[{"type":"text","text":"x"}]}}`)
	if a.status != http.StatusOK {
		t.Fatalf("a body naming the sender: %d %s", a.status, a.body)
	}
	var sent session.Event
	a.decode(t, &sent)
	var m session.UserMessage
	if err := sent.Decode(&m); err != nil || m.Sender.Subject != issuer+"|alice" || m.Sender.Client != "web" {
		t.Errorf("a body naming the sender recorded %+v, %v; want alice from the web client", m.Sender, err)
	}
}

// TestActivityCountsADay: the route counts the day's messages, sessions
// and finished answers per sender by client, as spec 085 counts them, and
// leaves out a trigger's message and a turn that stopped on an error.
func TestActivityCountsADay(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice+web", "reviewer")
	f.turn(s.ID, 1, "Done.", 1)
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice+desk", `{"type":"user.message","payload":{"content":[{"type":"text","text":"More."}]}}`); a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	f.appendTo(s.ID, 2, session.SessionStatus{Status: session.StatusRunning}, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopError})
	f.appendTo(s.ID, 3,
		session.UserMessage{Sender: session.Sender{Subject: session.TriggerSubjectPrefix + "trg_1", Kind: session.SenderTrigger}, FiringID: "fir_1"},
		session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn})
	today := time.Now().UTC().Format(time.DateOnly)
	alice := issuer + "|alice"
	a := f.do(http.MethodGet, "/v1/sessions/activity?day="+today, "root", "")
	if a.status != http.StatusOK {
		t.Fatalf("activity: %d %s", a.status, a.body)
	}
	var got struct {
		Items []session.SenderActivity `json:"items"`
	}
	a.decode(t, &got)
	want := []session.SenderActivity{{Sender: alice,
		Messages: []session.ClientCount{{Client: "desk", Count: 1}, {Client: "web", Count: 1}},
		Sessions: []session.ClientCount{{Client: "web", Count: 1}}, Answers: 1}}
	if !reflect.DeepEqual(got.Items, want) {
		t.Fatalf("the day\n got %+v\nwant %+v", got.Items, want)
	}
	// A page of one sender after alice is empty, and carries no cursor.
	a = f.do(http.MethodGet, "/v1/sessions/activity?day="+today+"&cursor="+store.Cursor(alice), "root", "")
	if a.status != http.StatusOK || !strings.Contains(string(a.body), `"items":[]`) || strings.Contains(string(a.body), "next_cursor") {
		t.Fatalf("after alice: %d %s", a.status, a.body)
	}
}

// TestActivityPages: a page holds limit senders and names the next one's
// cursor, and the last page names none.
func TestActivityPages(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	f.create("alice+web", "reviewer")
	f.apply("bob", "reviewer", "Review.")
	f.create("bob+web", "reviewer")
	today := time.Now().UTC().Format(time.DateOnly)
	var senders []string
	path := "/v1/sessions/activity?limit=1&day=" + today
	for range 3 {
		a := f.do(http.MethodGet, path, "root", "")
		if a.status != http.StatusOK {
			t.Fatalf("activity: %d %s", a.status, a.body)
		}
		var p struct {
			Items      []session.SenderActivity `json:"items"`
			NextCursor string                   `json:"next_cursor"`
		}
		a.decode(t, &p)
		for _, it := range p.Items {
			senders = append(senders, it.Sender)
		}
		if p.NextCursor == "" {
			break
		}
		path = "/v1/sessions/activity?limit=1&day=" + today + "&cursor=" + url.QueryEscape(p.NextCursor)
	}
	if want := []string{issuer + "|alice", issuer + "|bob"}; !reflect.DeepEqual(senders, want) {
		t.Fatalf("paged %v, want %v", senders, want)
	}
}

// TestActivityAsksOnce: the route asks session.activity once, about the
// collection, with the day and the one sender it names; the owner policy
// allows an admin and a caller naming themselves, and refuses anyone
// else's counts.
func TestActivityAsksOnce(t *testing.T) {
	f := newFixture(t)
	var q questions
	q.keep(f, authorizer.ActionSessionActivity)
	today := time.Now().UTC().Format(time.DateOnly)
	alice := issuer + "|alice"
	if a := f.do(http.MethodGet, "/v1/sessions/activity?day="+today+"&sender="+url.QueryEscape(alice), "alice", ""); a.status != http.StatusOK {
		t.Fatalf("alice's own: %d %s", a.status, a.body)
	}
	if asked := f.authz.take(); !reflect.DeepEqual(asked, []string{authorizer.ActionSessionActivity}) {
		t.Fatalf("asked %v, want session.activity once", asked)
	}
	req := q.last(t)
	if req.Resource.Kind != authorizer.KindSession || req.Resource.ID != "" || req.Resource.String("day") != today || req.Resource.String("sender") != alice {
		t.Fatalf("the question's resource is %+v", req.Resource)
	}
	for _, c := range []struct {
		token, query string
		status       int
	}{
		{"alice", "", http.StatusForbidden},
		{"alice", "&sender=" + url.QueryEscape(issuer+"|bob"), http.StatusForbidden},
		{"root", "", http.StatusOK},
		{"root", "&sender=" + url.QueryEscape(alice), http.StatusOK},
	} {
		if a := f.do(http.MethodGet, "/v1/sessions/activity?day="+today+c.query, c.token, ""); a.status != c.status {
			t.Errorf("%s%s: %d %s, want %d", c.token, c.query, a.status, a.body, c.status)
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.reqs[1].Resource.Fields["sender"]; ok {
		t.Errorf("a question without a sender names one: %+v", q.reqs[1].Resource)
	}
}

// TestActivityRefusesABadDay: a day that is absent, not a date, before
// the first day or after today, a limit out of range and a cursor not
// issued here are invalid_request, and nothing is asked.
func TestActivityRefusesABadDay(t *testing.T) {
	f := newFixture(t)
	tomorrow := time.Now().UTC().Add(24 * time.Hour).Format(time.DateOnly)
	for _, query := range []string{"", "?day=yesterday", "?day=2026-13-01", "?day=1999-12-31", "?day=" + tomorrow,
		"?day=2026-01-02&limit=0", "?day=2026-01-02&limit=501", "?day=2026-01-02&cursor=%25%25"} {
		a := f.do(http.MethodGet, "/v1/sessions/activity"+query, "root", "")
		if a.status != http.StatusBadRequest || a.code() != CodeInvalidRequest {
			t.Errorf("%q: %d %s, want invalid_request", query, a.status, a.body)
		}
	}
	if asked := f.authz.take(); len(asked) != 0 {
		t.Errorf("a refused request asked %v", asked)
	}
}
