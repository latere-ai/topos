// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// conversation is a session of two finished turns as alice holds it: the
// opening message at 1 and its turn to the boundary at 5, a change of
// model alice made at 6, her second message at 7 and its turn to the
// boundary at 11.
func (f *fixture) conversation() session.Session {
	f.t.Helper()
	s := f.create("alice", "reviewer")
	f.turn(s.ID, 1, "One finding.", 1200)
	if a := f.do(http.MethodPatch, "/v1/sessions/"+s.ID, "alice", `{"model":{"name":"`+sonnet+`"}}`); a.status != http.StatusOK {
		f.t.Fatalf("change the model: %d %s", a.status, a.body)
	}
	f.appendTo(s.ID, 2, session.UserMessage{Sender: s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "And main_test.go?"}}})
	f.turn(s.ID, 2, "Two findings.", 800)
	if h := f.header(s.ID); h.LastSeq != 11 {
		f.t.Fatalf("the conversation ends at %d, want 11", h.LastSeq)
	}
	return f.header(s.ID)
}

// count is how many sessions the store holds.
func (f *fixture) count() int {
	f.t.Helper()
	all, _, err := f.sessions.List(f.t.Context(), session.ListOptions{Limit: MaxLimit})
	if err != nil {
		f.t.Fatal(err)
	}
	return len(all)
}

// forked forks id as alice with body and answers the fork.
func (f *fixture) forked(id, body string) session.Session {
	f.t.Helper()
	a := f.do(http.MethodPost, "/v1/sessions/"+id+"/fork", "alice", body)
	if a.status != http.StatusCreated {
		f.t.Fatalf("fork %s: %d %s", body, a.status, a.body)
	}
	var s session.Session
	a.decode(f.t, &s)
	return s
}

// said is the text of a user.message.
func said(t *testing.T, e session.Event) string {
	t.Helper()
	var m session.UserMessage
	if e.Type != session.TypeUserMessage || e.Decode(&m) != nil || len(m.Content) == 0 {
		t.Fatalf("event %d is a %s, not a user.message with text", e.Seq, e.Type)
	}
	return m.Content[0].Text
}

// TestAForkBeforeAMessageCopiesUpToIt: before_seq naming a person's
// message that opened a turn forks just before it: the copy holds what
// lay between the turn's end and the message, the person's model change
// included, the fork stands on that model, waits idle on the boundary's
// status, names its parent's copy's end and its tree's root, and is asked
// session.read and session.fork alone.
func TestAForkBeforeAMessageCopiesUpToIt(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	parent := f.conversation()
	f.authz.take()
	child := f.forked(parent.ID, `{"before_seq":7}`)
	if got := f.authz.take(); !slices.Equal(got, []string{authorizer.ActionSessionRead, authorizer.ActionSessionFork}) {
		t.Fatalf("the fork asked %v", got)
	}
	switch {
	case child.Parent == nil || *child.Parent != (session.Parent{SessionID: parent.ID, Seq: 6}) || child.Root != parent.ID:
		t.Fatalf("parent %+v, root %q", child.Parent, child.Root)
	case child.LastSeq != 6 || child.Status != session.StatusIdle || child.StopReason != session.StopEndTurn || child.Turn != 1:
		t.Fatalf("the fork is %s %s at %d, turn %d", child.Status, child.StopReason, child.LastSeq, child.Turn)
	case child.Model == nil || child.Model.Name != sonnet:
		t.Fatalf("the fork stands on %+v, want the model alice changed to before her message", child.Model)
	case child.Budget.SpentCostUSDMicro != 1200 || child.Budget.CarriedCostUSDMicro != 1200:
		t.Fatalf("spent %d, carried %d, want the copied 1200", child.Budget.SpentCostUSDMicro, child.Budget.CarriedCostUSDMicro)
	}
	pe, ce := f.log(parent.ID), f.log(child.ID)
	if len(ce) != 6 {
		t.Fatalf("the fork holds %d events, want 6", len(ce))
	}
	for i := range ce {
		want := pe[i]
		want.SessionID = child.ID
		if !session.SameEvent(ce[i], want) {
			t.Fatalf("event %d: %+v, want the parent's %+v", i+1, ce[i], pe[i])
		}
	}
	if ce[5].Type != session.TypeModelChanged {
		t.Fatalf("the copy ends with a %s, want the model change", ce[5].Type)
	}
}

// TestAForkBeforeTheOpeningMessage: before_seq 1 copies nothing: the fork
// names seq 0 of its parent, holds no event, is idle with no stop reason
// at turn 0, and with a message holds that message alone.
func TestAForkBeforeTheOpeningMessage(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	parent := f.conversation()
	empty := f.forked(parent.ID, `{"before_seq":1}`)
	if empty.Parent == nil || *empty.Parent != (session.Parent{SessionID: parent.ID, Seq: 0}) || empty.Root != parent.ID ||
		empty.LastSeq != 0 || empty.Status != session.StatusIdle || empty.StopReason != "" || empty.Turn != 0 || empty.Budget.CarriedCostUSDMicro != 0 {
		t.Fatalf("a fork before the opening message: %+v", empty)
	}
	if evs := f.log(empty.ID); len(evs) != 0 {
		t.Fatalf("it holds %d events", len(evs))
	}
	sent := f.forked(parent.ID, `{"before_seq":1,"message":{"content":[{"type":"text","text":"Review main.go and its tests."}]}}`)
	evs := f.log(sent.ID)
	if sent.Parent.Seq != 0 || sent.LastSeq != 1 || len(evs) != 1 || said(t, evs[0]) != "Review main.go and its tests." {
		t.Fatalf("a fork before the opening message with its message: %+v, %d events", sent, len(evs))
	}
}

// TestForkBodyRefusals: before_seq naming no message that opened a turn
// is invalid_fork_point; before_seq beside at_seq, a message holding
// nothing, a file naming both or neither of data and a blob, a blob
// beside a media type, a blob of the send route, and a body past MaxBody
// without a message are refused before anything is written.
func TestForkBodyRefusals(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	parent := f.conversation()
	f.appendTo(parent.ID, 3, session.UserMessage{Sender: session.Sender{Subject: session.TriggerSubjectPrefix + "trg_1", Kind: session.SenderTrigger}, Content: []lux.Block{{Type: ir.BlockText, Text: "Weekly review."}}})
	f.appendTo(parent.ID, 3, session.SessionStatus{Status: session.StatusRunning})
	f.appendTo(parent.ID, 3, session.UserMessage{Sender: parent.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Also the docs."}}})
	sessions := f.count()
	data := base64.StdEncoding.EncodeToString([]byte("a,b\n"))
	blob := session.DigestOf([]byte("a,b\n"))
	for _, c := range []struct {
		name, body, code string
	}{
		{"before nothing", `{"before_seq":0}`, CodeInvalidForkPoint},
		{"past the log", `{"before_seq":99}`, CodeInvalidForkPoint},
		{"an answer", `{"before_seq":3}`, CodeInvalidForkPoint},
		{"a status", `{"before_seq":5}`, CodeInvalidForkPoint},
		{"a model change", `{"before_seq":6}`, CodeInvalidForkPoint},
		{"a trigger's message", `{"before_seq":12,"message":{"content":[{"type":"text","text":"Monthly review."}]}}`, CodeInvalidForkPoint},
		{"a message steered into a turn", `{"before_seq":14}`, CodeInvalidForkPoint},
		{"two fork points", `{"before_seq":7,"at_seq":5}`, CodeInvalidRequest},
		{"a tree that is not new", `{"before_seq":7,"tree":"parent"}`, CodeInvalidRequest},
		{"an empty tree", `{"before_seq":7,"tree":""}`, CodeInvalidRequest},
		{"a tree that is no string", `{"before_seq":7,"tree":true}`, CodeInvalidRequest},
		{"an empty message", `{"before_seq":7,"message":{"content":[]}}`, CodeInvalidRequest},
		{"a file of data and a blob", `{"before_seq":7,"message":{"attachments":[{"name":"a.csv","data":"` + data + `","blob":"` + string(blob) + `"}]}}`, CodeInvalidRequest},
		{"a file of neither", `{"before_seq":7,"message":{"attachments":[{"name":"a.csv"}]}}`, CodeInvalidRequest},
		{"a blob with a media type", `{"before_seq":7,"message":{"attachments":[{"name":"a.csv","media_type":"text/csv","blob":"` + string(blob) + `"}]}}`, CodeInvalidRequest},
		{"a blob no message attached", `{"before_seq":7,"message":{"attachments":[{"name":"a.csv","blob":"` + string(blob) + `"}]}}`, CodeInvalidRequest},
		{"an unknown member", `{"before":7}`, CodeInvalidRequest},
		{"a large body without a message", `{"title":"` + strings.Repeat("x", MaxBody) + `"}`, CodePayloadTooLarge},
	} {
		a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", c.body)
		if a.code() != c.code {
			t.Errorf("%s: %d %s, want %s", c.name, a.status, a.body, c.code)
		}
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/events", "alice", `{"type":"user.message","payload":{"attachments":[{"name":"a.csv","blob":"`+string(blob)+`"}]}}`); a.code() != CodeInvalidRequest {
		t.Errorf("a blob sent to a session: %d %s", a.status, a.body)
	}
	if n := f.count(); n != sessions {
		t.Fatalf("refused forks left %d sessions, want %d", n, sessions)
	}
}

// TestAForkWithAMessage: a fork with a message asks session.read, then
// session.fork with the new session's id, then session.send of that id
// as the person's message, writes the copy and the message as one
// append, wakes the runners, and answers the fork at the message's
// sequence under the title asked.
func TestAForkWithAMessage(t *testing.T) {
	var woken atomic.Int32
	f := newFixture(t, func(o *Options) { o.Notify = func() { woken.Add(1) } })
	f.apply("alice", "reviewer", "Review.")
	parent := f.conversation()
	var fork, send questions
	fork.keep(f, authorizer.ActionSessionFork)
	keepSend := f.authz.answer
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionSend {
			send.mu.Lock()
			send.reqs = append(send.reqs, req)
			send.mu.Unlock()
		}
		return keepSend(req)
	}
	f.authz.take()
	woken.Store(0)
	child := f.forked(parent.ID, `{"before_seq":7,"title":"Review the tests","message":{"content":[{"type":"text","text":"And main_test.go, line by line?"}]}}`)
	if got := f.authz.take(); !slices.Equal(got, []string{authorizer.ActionSessionRead, authorizer.ActionSessionFork, authorizer.ActionSessionSend}) {
		t.Fatalf("the fork asked %v", got)
	}
	f2, s := fork.last(t), send.last(t)
	switch {
	case f2.Resource.String("session_id") != child.ID || fmt.Sprint(f2.Resource.Fields["seq"]) != "6":
		t.Fatalf("session.fork asked about %v", f2.Resource.Fields)
	case s.Resource.ID != child.ID || s.Resource.String("sender") != alice || s.Resource.String("event_type") != string(session.TypeUserMessage) ||
		s.Resource.String("owner") != alice || s.Resource.String("model") != sonnet:
		t.Fatalf("session.send asked about %s with %v", s.Resource.ID, s.Resource.Fields)
	}
	if _, ok := s.Resource.Fields["idle_seconds"]; !ok {
		t.Fatal("session.send carries no idle_seconds though the copy holds a request")
	}
	evs := f.log(child.ID)
	switch {
	case child.LastSeq != 7 || len(evs) != 7 || child.Title != "Review the tests":
		t.Fatalf("the fork is at %d with %d events, titled %q", child.LastSeq, len(evs), child.Title)
	case said(t, evs[6]) != "And main_test.go, line by line?":
		t.Fatalf("the fork's message reads %q", said(t, evs[6]))
	case woken.Load() == 0:
		t.Fatal("the runners were not woken for the fork's message")
	}
	var m session.UserMessage
	if err := evs[6].Decode(&m); err != nil || m.Sender.Subject != alice || m.Sender.Kind != session.SenderPerson {
		t.Fatalf("the message's sender %+v, %v", m.Sender, err)
	}
}

// sinkEvents keeps what a fixture's sink receives.
type sinkEvents struct {
	mu  sync.Mutex
	got []SinkEvent
}

func (s *sinkEvents) all() []SinkEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

// TestAForkWhoseSendIsDeniedWritesNothing: a denied session.fork, and a
// session.send denied after session.fork was allowed, refuse the fork as
// that deny with its reason and write nothing; the send's deny is
// reported to the sink as the fork the allow named, with the deny's code
// as its outcome, and the fork's own deny, which allowed nothing, is not.
func TestAForkWhoseSendIsDeniedWritesNothing(t *testing.T) {
	var sink sinkEvents
	f := newFixture(t, func(o *Options) {
		o.Sink = func(_ context.Context, e SinkEvent) error {
			sink.mu.Lock()
			defer sink.mu.Unlock()
			sink.got = append(sink.got, e)
			return nil
		}
	})
	f.apply("alice", "reviewer", "Review.")
	parent := f.conversation()
	sessions := f.count()
	body := `{"before_seq":7,"message":{"content":[{"type":"text","text":"And main_test.go?"}]}}`
	var forkAsked questions
	forkAsked.keep(f, authorizer.ActionSessionFork)
	keep := f.authz.answer
	deny := func(action, reason string) {
		f.authz.answer = func(req authz.Request) (authz.Decision, error) {
			if req.Action == action {
				return authz.Decision{Reason: reason}, nil
			}
			return keep(req)
		}
	}

	deny(authorizer.ActionSessionFork, "plan_suspended")
	f.authz.take()
	a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", body)
	if a.status != http.StatusForbidden || !strings.Contains(string(a.body), "plan_suspended") {
		t.Fatalf("a denied fork: %d %s", a.status, a.body)
	}
	if asked := f.authz.take(); slices.Contains(asked, authorizer.ActionSessionSend) {
		t.Fatalf("a denied fork asked %v", asked)
	}

	deny(authorizer.ActionSessionSend, "messages_exhausted")
	f.authz.take()
	a = f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", body)
	if a.status != http.StatusForbidden || !strings.Contains(string(a.body), "messages_exhausted") {
		t.Fatalf("a fork whose send is denied: %d %s", a.status, a.body)
	}
	asked := f.authz.take()
	if len(asked) < 3 || !slices.Equal(asked[:3], []string{authorizer.ActionSessionRead, authorizer.ActionSessionFork, authorizer.ActionSessionSend}) {
		t.Fatalf("the fork asked %v", asked)
	}
	if n := f.count(); n != sessions {
		t.Fatalf("denied forks left %d sessions, want %d", n, sessions)
	}
	named := forkAsked.last(t).Resource.String("session_id")
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("the sink received %d events, want the one fork the authorizer allowed: %+v", len(got), got)
	}
	e := got[0]
	switch {
	case e.Type != authorizer.ActionSessionFork || e.Object != (SinkObject{Kind: authorizer.KindSession, ID: named}) || e.SessionID != named || e.Subject != alice:
		t.Fatalf("the sink event names %+v, want the fork %s", e, named)
	case e.Outcome != "forbidden" || e.Attributes["reason"] != "messages_exhausted" || e.Attributes["parent"] != parent.ID || e.Attributes["refused"] != authorizer.ActionSessionSend:
		t.Fatalf("the sink event's outcome %q, attributes %v", e.Outcome, e.Attributes)
	case e.Agent == nil || e.Agent.ID != parent.Agent.ID || e.Agent.Version != parent.Agent.Version || e.OccurredAt.IsZero():
		t.Fatalf("the sink event's agent %+v at %v", e.Agent, e.OccurredAt)
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "main_test.go") {
		t.Fatalf("the sink event carries the message: %s", b)
	}
}

// TestAForksSendCarriesTheCopiedIdle: the send of a fork's message
// carries the model the fork starts on and the whole seconds since the
// last request it copied, none for a copy that holds none, and an allow
// that names another model appends session.model_changed straight before
// the message.
func TestAForksSendCarriesTheCopiedIdle(t *testing.T) {
	now := time.Now().Add(90 * time.Second)
	f := newFixture(t, func(o *Options) { o.Now = func() time.Time { return now } })
	f.apply("alice", "reviewer", "Review.")
	parent := f.create("alice", "reviewer")
	f.turn(parent.ID, 1, "One finding.", 1200)
	f.appendTo(parent.ID, 2, session.UserMessage{Sender: parent.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "And main_test.go?"}}})
	r := f.route(func(req authz.Request) string {
		if req.Action == authorizer.ActionSessionSend {
			return sonnet
		}
		return ""
	})
	child := f.forked(parent.ID, `{"before_seq":6,"message":{"content":[{"type":"text","text":"And main_test.go, line by line?"}]}}`)
	asked := r.last(t, authorizer.ActionSessionSend)
	idle := asked.Resource.Int("idle_seconds")
	if asked.Resource.ID != child.ID || asked.Resource.String("model") != haiku || idle < 89 || idle > 95 {
		t.Fatalf("session.send asked about %s with %v", asked.Resource.ID, asked.Resource.Fields)
	}
	if _, via := asked.Resource.Fields["model_via"]; via {
		t.Fatalf("a fork on its agent's model names model_via: %v", asked.Resource.Fields)
	}
	evs := f.log(child.ID)
	if len(evs) != 7 || evs[5].Type != session.TypeModelChanged || said(t, evs[6]) != "And main_test.go, line by line?" {
		t.Fatalf("the fork's log ends %v", evs[len(evs)-2:])
	}
	var m session.ModelChanged
	if err := evs[5].Decode(&m); err != nil || m.New.Name != sonnet || m.New.Via != haiku || m.By.Subject != session.AuthorizerSubject {
		t.Fatalf("the model change %+v, %v", m, err)
	}
	if child.Model == nil || child.Model.Name != sonnet {
		t.Fatalf("the fork stands on %+v", child.Model)
	}

	first := f.forked(parent.ID, `{"before_seq":1,"message":{"content":[{"type":"text","text":"Review main.go closely."}]}}`)
	asked = r.last(t, authorizer.ActionSessionSend)
	if _, ok := asked.Resource.Fields["idle_seconds"]; ok || asked.Resource.ID != first.ID {
		t.Fatalf("a fork that copied no request asked %v", asked.Resource.Fields)
	}
}

// TestAForkMessageKeepsAnAttachmentByBlob: a fork's message keeps a file
// a message of the parent attached by its blob, with the parent's record
// of its media type and size, under the new message's id, beside a new
// file sent as data; each is a blob of the fork.
func TestAForkMessageKeepsAnAttachmentByBlob(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	parent := f.create("alice", "reviewer")
	f.turn(parent.ID, 1, "One finding.", 1200)
	prices := []byte("item,price\nhotel,300\n")
	a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/events", "alice",
		`{"type":"user.message","payload":{"content":[{"type":"text","text":"Plan it for three."}],"attachments":[{"name":"prices.csv","media_type":"text/csv","data":"`+base64.StdEncoding.EncodeToString(prices)+`"}]}}`)
	if a.status != http.StatusOK {
		t.Fatalf("send: %d %s", a.status, a.body)
	}
	var sent session.Event
	a.decode(t, &sent)
	f.turn(parent.ID, 2, "Planned.", 300)
	notes := []byte("four people\n")
	child := f.forked(parent.ID, fmt.Sprintf(`{"before_seq":%d,"message":{"content":[{"type":"text","text":"Plan it for four."}],"attachments":[{"name":"prices.csv","blob":%q},{"name":"notes.txt","data":%q}]}}`,
		sent.Seq, session.DigestOf(prices), base64.StdEncoding.EncodeToString(notes)))
	evs := f.log(child.ID)
	var m session.UserMessage
	if err := evs[len(evs)-1].Decode(&m); err != nil {
		t.Fatal(err)
	}
	id := evs[len(evs)-1].ID
	want := []session.Attachment{
		{Name: "prices.csv", MediaType: "text/csv", Size: int64(len(prices)), Blob: session.DigestOf(prices), Path: session.AttachmentPath(id, "prices.csv")},
		{Name: "notes.txt", MediaType: "text/plain; charset=utf-8", Size: int64(len(notes)), Blob: session.DigestOf(notes), Path: session.AttachmentPath(id, "notes.txt")},
	}
	if !slices.Equal(m.Attachments, want) || id == sent.ID {
		t.Fatalf("the fork's message attaches %+v, want %+v", m.Attachments, want)
	}
	for _, b := range [][]byte{prices, notes} {
		got := f.do(http.MethodGet, "/v1/sessions/"+child.ID+"/blobs/"+string(session.DigestOf(b)), "alice", "")
		if got.status != http.StatusOK || string(got.body) != string(b) {
			t.Fatalf("the fork's blob %s: %d %q", session.DigestOf(b), got.status, got.body)
		}
	}
}

// TestForkTitle: a fork's title is the body's, none for "", and the
// continuation title when the body names none.
func TestForkTitle(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","title":"Trip budget","message":"Plan it for three."}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create: %d %s", a.status, a.body)
	}
	var parent session.Session
	a.decode(t, &parent)
	f.turn(parent.ID, 1, "Planned.", 1)
	for body, want := range map[string]string{
		`{"before_seq":1,"title":"Trip budget"}`: "Trip budget",
		`{"before_seq":1,"title":""}`:            "",
		`{"before_seq":1}`:                       "Trip budget (continued)",
		`{}`:                                     "Trip budget (continued)",
	} {
		if got := f.forked(parent.ID, body); got.Title != want {
			t.Errorf("%s titles the fork %q, want %q", body, got.Title, want)
		}
	}
}

// TestForkRoot: a fork's root is its parent's id, a fork of that fork's
// its parent's root, and a session no fork made answers none.
func TestForkRoot(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	top := f.conversation()
	child := f.forked(top.ID, `{"before_seq":7}`)
	grandchild := f.forked(child.ID, `{"before_seq":1}`)
	if child.Root != top.ID || grandchild.Root != top.ID || grandchild.Parent.SessionID != child.ID {
		t.Fatalf("roots %q and %q, want %s", child.Root, grandchild.Root, top.ID)
	}
	a := f.do(http.MethodGet, "/v1/sessions/"+top.ID, "alice", "")
	var fields map[string]json.RawMessage
	a.decode(t, &fields)
	if _, ok := fields["root"]; ok {
		t.Fatalf("a session no fork made answers root %s", fields["root"])
	}
}

// listTree lists sessions as alice under query and answers their ids and
// each one's tree, "" for none.
func (f *fixture) listTree(query string) ([]string, []string) {
	f.t.Helper()
	a := f.do(http.MethodGet, "/v1/sessions?"+query, "alice", "")
	if a.status != http.StatusOK {
		f.t.Fatalf("list %s: %d %s", query, a.status, a.body)
	}
	var page struct {
		Items []session.Session `json:"items"`
	}
	a.decode(f.t, &page)
	var ids, trees []string
	for _, s := range page.Items {
		ids = append(ids, s.ID)
		tree := ""
		if s.Tree != nil {
			tree = fmt.Sprintf("%s %d", s.Tree.Root, s.Tree.Sessions)
		}
		trees = append(trees, tree)
	}
	return ids, trees
}

// TestTheListReadsATree: the list answers a tree by root, a session's
// forks by parent, and one session per tree with group=tree, the newest,
// with its tree; group takes tree alone, and the summary counts sessions
// whatever the tree parameters say.
func TestTheListReadsATree(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	top := f.conversation()
	other := f.create("alice", "reviewer")
	child := f.forked(top.ID, `{"before_seq":7}`)
	grandchild := f.forked(child.ID, `{"before_seq":1}`)
	if ids, _ := f.listTree("root=" + top.ID); !slices.Equal(ids, []string{grandchild.ID, child.ID, top.ID}) {
		t.Fatalf("the tree lists %v", ids)
	}
	if ids, _ := f.listTree("parent=" + top.ID); !slices.Equal(ids, []string{child.ID}) {
		t.Fatalf("the parent's forks list %v", ids)
	}
	ids, trees := f.listTree("group=tree")
	if !slices.Equal(ids, []string{grandchild.ID, other.ID}) || !slices.Equal(trees, []string{top.ID + " 3", other.ID + " 1"}) {
		t.Fatalf("grouped by tree: %v, %v", ids, trees)
	}
	if a := f.do(http.MethodGet, "/v1/sessions?group=parent", "alice", ""); a.code() != CodeInvalidRequest {
		t.Fatalf("group=parent: %d %s", a.status, a.body)
	}
	var sum session.Summary
	f.do(http.MethodGet, "/v1/sessions/summary?root="+top.ID+"&group=tree", "alice", "").decode(t, &sum)
	if sum.Sessions.Idle != 4 {
		t.Fatalf("the summary counts %+v, want every session", sum.Sessions)
	}
}

// TestDeletingAParentLeavesItsForks: a session deleted in the middle of a
// tree leaves its fork readable, its root and parent still naming what
// they named, and listed under the tree's root, as it is after the root
// itself is deleted.
func TestDeletingAParentLeavesItsForks(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	top := f.conversation()
	child := f.forked(top.ID, `{"before_seq":7}`)
	grandchild := f.forked(child.ID, `{"before_seq":1}`)
	for _, gone := range []string{child.ID, top.ID} {
		if a := f.do(http.MethodDelete, "/v1/sessions/"+gone, "alice", ""); a.status != http.StatusNoContent {
			t.Fatalf("delete %s: %d %s", gone, a.status, a.body)
		}
		a := f.do(http.MethodGet, "/v1/sessions/"+grandchild.ID, "alice", "")
		var got session.Session
		a.decode(t, &got)
		if a.status != http.StatusOK || got.Root != top.ID || got.Parent == nil || got.Parent.SessionID != child.ID {
			t.Fatalf("after deleting %s the fork reads %d %+v", gone, a.status, got)
		}
		if ids, _ := f.listTree("root=" + top.ID); !slices.Contains(ids, grandchild.ID) {
			t.Fatalf("after deleting %s the tree lists %v", gone, ids)
		}
	}
}

// TestAForkThatStartsANewConversation: a fork with tree new is the root
// of a tree of its own: its root is its own id and its parent stays the
// session it came from, session.fork names that root, the list by the
// source's root leaves it out while the list by its parent keeps it, a
// list grouped by tree shows it as a conversation of its own, and a fork
// of it joins its tree.
func TestAForkThatStartsANewConversation(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	source := f.conversation()
	version := f.forked(source.ID, `{"before_seq":7}`)
	var fork questions
	fork.keep(f, authorizer.ActionSessionFork)
	fresh := f.forked(source.ID, `{"tree":"new","title":"Tests","message":{"content":[{"type":"text","text":"Now write the tests."}]}}`)
	switch {
	case fresh.Root != fresh.ID || fresh.Parent == nil || *fresh.Parent != (session.Parent{SessionID: source.ID, Seq: 11}):
		t.Fatalf("a new conversation: root %q, parent %+v", fresh.Root, fresh.Parent)
	case fresh.Title != "Tests" || fresh.LastSeq != 12 || fresh.Budget.CarriedCostUSDMicro != 2000:
		t.Fatalf("a new conversation: %+v", fresh)
	}
	asked := fork.last(t)
	if asked.Resource.String("root") != fresh.ID || asked.Resource.String("session_id") != fresh.ID || asked.Resource.String("parent") != source.ID {
		t.Fatalf("session.fork of a new conversation asked %v", asked.Resource.Fields)
	}
	opening := f.forked(source.ID, `{"before_seq":1}`)
	if q := fork.last(t); q.Resource.String("root") != source.ID {
		t.Fatalf("session.fork of a version asked root %q, want the source's %s", q.Resource.String("root"), source.ID)
	}
	if ids, _ := f.listTree("root=" + source.ID); slices.Contains(ids, fresh.ID) || !slices.Contains(ids, version.ID) {
		t.Fatalf("the source's tree lists %v", ids)
	}
	if ids, _ := f.listTree("parent=" + source.ID); !slices.Contains(ids, fresh.ID) {
		t.Fatalf("the source's forks list %v", ids)
	}
	ids, trees := f.listTree("group=tree")
	if !slices.Equal(ids, []string{opening.ID, fresh.ID}) || !slices.Equal(trees, []string{source.ID + " 3", fresh.ID + " 1"}) {
		t.Fatalf("grouped by tree: %v, %v", ids, trees)
	}
	next := f.forked(fresh.ID, `{"before_seq":12}`)
	if next.Root != fresh.ID {
		t.Fatalf("a version in the new conversation has root %q, want %s", next.Root, fresh.ID)
	}
	if ids, _ := f.listTree("root=" + fresh.ID); !slices.Equal(ids, []string{next.ID, fresh.ID}) {
		t.Fatalf("the new conversation lists %v", ids)
	}
}
