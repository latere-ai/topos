// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/session"
)

// put appends events to a session as its runner does, after its last.
func (f *fixture) put(id string, events ...session.Event) {
	f.t.Helper()
	s, err := f.sessions.Get(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	session.Stamp(id, s.LastSeq, events)
	if _, err := f.sessions.Append(f.t.Context(), id, s.LastSeq, events); err != nil {
		f.t.Fatal(err)
	}
}

// ev is an event of typ with payload, now.
func ev(t *testing.T, typ session.Type, payload any) session.Event {
	t.Helper()
	e, err := session.NewEvent(typ, payload, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ask has the session's runner record the example's question call as
// toolu_01 and go idle on it.
func (f *fixture) ask(id string) {
	f.t.Helper()
	f.put(id,
		ev(f.t, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning}),
		ev(f.t, session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: exampleQuestionCall, Name: session.ToolQuestion, Input: json.RawMessage(exampleQuestionInput), Verdict: "allow"}),
		ev(f.t, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopQuestion}),
	)
}

// answerBody is a send of a user.answer to the example's question.
func answerBody(entries string) string {
	return `{"type":"user.answer","payload":{"tool_use_id":"` + exampleQuestionCall + `","answers":` + entries + `}}`
}

// detail is a refusal's developer detail.
func (a answer) detail() string {
	var e struct {
		Error struct {
			Details struct {
				Detail string `json:"detail"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(a.body, &e) != nil {
		return ""
	}
	return e.Error.Details.Detail
}

// TestAttendedAtCreate: a session is attended only when its creator says
// so: the create route and the fork route take the body's value, a fork
// declares it again whatever its parent declared, and a trigger's
// session is not attended.
func TestAttendedAtCreate(t *testing.T) {
	f := newTriggerFixture(t)
	var plain, attended session.Session
	f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","message":"Go."}`).decode(t, &plain)
	f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","message":"Go.","attended":true}`).decode(t, &attended)
	if plain.Attended || !attended.Attended || f.header(plain.ID).Attended || !f.header(attended.ID).Attended {
		t.Fatalf("created: %v and %v", plain.Attended, attended.Attended)
	}
	if a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","attended":"yes"}`); a.code() != CodeInvalidRequest {
		t.Fatalf("attended that is not a boolean: %d %s", a.status, a.body)
	}
	for _, c := range []struct {
		parent session.Session
		body   string
		want   bool
	}{
		{plain, `{"attended":true}`, true},
		{attended, ``, false},
		{attended, `{"attended":true}`, true},
	} {
		f.turn(c.parent.ID, f.header(c.parent.ID).Turn+1, "Done.", 1)
		var forked session.Session
		a := f.do(http.MethodPost, "/v1/sessions/"+c.parent.ID+"/fork", "alice", c.body)
		if a.status != http.StatusCreated {
			t.Fatalf("fork: %d %s", a.status, a.body)
		}
		a.decode(t, &forked)
		if forked.Attended != c.want || f.header(forked.ID).Attended != c.want {
			t.Fatalf("a fork of a session attended %v with %q is attended %v", c.parent.Attended, c.body, forked.Attended)
		}
	}
	f.applyTrigger("alice", "triage", eventSpec(""))
	fired := f.fired("alice", "triage", f.envelope("1", "issue.opened", "o/r#1", "Crash"), store.OutcomeStarted)
	if f.header(fired.SessionID).Attended {
		t.Fatal("a trigger's session is attended")
	}
}

// TestAnAnswerIsCheckedAgainstItsQuestion: the send route refuses an
// answer whose shape is wrong before it asks the authorizer, and one
// that does not fit the open question, or names none, after its allow,
// each with nothing appended; a caller the authorizer denies hears
// forbidden whatever the answer holds.
func TestAnAnswerIsCheckedAgainstItsQuestion(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	send := func(body string) answer {
		t.Helper()
		return f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", body)
	}
	if a := send(answerBody(`[{"selected":["SQLite"]},{}]`)); a.code() != CodeConflict || !strings.Contains(a.detail(), "no question is open") {
		t.Fatalf("an answer with no question: %d %s", a.status, a.body)
	}
	f.ask(s.ID)
	before := len(f.log(s.ID))

	// The shape, before the authorizer is asked.
	f.authz.take()
	for body, want := range map[string]string{
		`{"type":"user.answer","payload":{"answers":[{},{}]}}`:                                   "tool_use_id",
		`{"type":"user.answer","payload":{"tool_use_id":"toolu_01"}}`:                            "one entry per question",
		answerBody(`[{"text":"` + strings.Repeat("é", session.MaxAnswerTextLength+1) + `"},{}]`): "at most 2000",
		answerBody(`[{"selected":["SQLite"],"note":"x"},{}]`):                                    "unknown field",
		`{"type":"user.answer","payload":{"tool_use_id":"toolu_01","answers":[],"x":1}}`:         "unknown field",
	} {
		a := send(body)
		if a.code() != CodeInvalidRequest || !strings.Contains(a.detail(), want) {
			t.Errorf("%.80s: %d %s, want %q", body, a.status, a.body, want)
		}
	}
	if asked := f.authz.take(); len(asked) != 0 {
		t.Fatalf("an answer of the wrong shape asked %v", asked)
	}

	// A caller the authorizer denies learns nothing of the question.
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionSend {
			return authz.Decision{Allow: false, Reason: "no_answers"}, nil
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	if a := send(answerBody(`[{"selected":["MySQL"]}]`)); a.status != http.StatusForbidden {
		t.Fatalf("a denied answer that does not fit: %d %s", a.status, a.body)
	}
	f.authz.answer = nil

	// The fit, after the allow.
	for entries, want := range map[string]string{
		`[{"selected":["SQLite"]}]`:                           "1 entries for 2 questions",
		`[{},{},{}]`:                                          "3 entries for 2 questions",
		`[{"selected":["MySQL"]},{}]`:                         `"MySQL", which is no option of question 1`,
		`[{},{"selected":["Europe","Europe"]}]`:               `"Europe" twice`,
		`[{"selected":["Postgres","SQLite"]},{}]`:             "question 1 takes one",
		`[{"selected":["Europe"]},{"selected":["Postgres"]}]`: "no option of question 1",
	} {
		a := send(answerBody(entries))
		if a.code() != CodeInvalidRequest || !strings.Contains(a.detail(), want) {
			t.Errorf("%s: %d %s, want %q", entries, a.status, a.body, want)
		}
	}
	if a := send(`{"type":"user.answer","payload":{"tool_use_id":"toolu_99","answers":[{},{}]}}`); a.code() != CodeConflict || !strings.Contains(a.detail(), "the open question is toolu_01") {
		t.Fatalf("an answer to another call: %d %s", a.status, a.body)
	}
	if n := len(f.log(s.ID)); n != before {
		t.Fatalf("refused answers appended %d events", n-before)
	}
	if q, open := session.OpenQuestion(f.log(s.ID)); !open || q.ToolUseID != exampleQuestionCall {
		t.Fatal("a refused answer closed the question")
	}

	// The answer stands, and the reply and the log carry it.
	a := send(answerBody(`[{"selected":["SQLite"],"text":"we have nobody to run a second schema"},{}]`))
	if a.status != http.StatusOK {
		t.Fatalf("an answer: %d %s", a.status, a.body)
	}
	var sent session.Event
	a.decode(t, &sent)
	var p session.UserAnswer
	if err := sent.Decode(&p); err != nil || sent.Type != session.TypeUserAnswer || p.Sender.Subject != alice || len(p.Answers) != 2 || p.Answers[0].Selected[0] != "SQLite" {
		t.Fatalf("the appended answer %+v, %v", sent, err)
	}
	if again := send(answerBody(`[{},{}]`)); again.code() != CodeConflict || !strings.Contains(again.detail(), "the user.answer "+sent.ID+" already answered") {
		t.Fatalf("a second answer: %d %s", again.status, again.body)
	}

	// A question a message or an interrupt closed refuses an answer and
	// says which.
	for typ, want := range map[string]string{
		`{"type":"user.message","payload":{"content":[{"type":"text","text":"Use SQLite."}]}}`: "the person's user.message",
		`{"type":"user.interrupt"}`: "the user.interrupt",
	} {
		other := f.create("alice", "reviewer")
		f.ask(other.ID)
		closing := f.do(http.MethodPost, "/v1/sessions/"+other.ID+"/events", "alice", typ)
		var closed session.Event
		closing.decode(t, &closed)
		a := f.do(http.MethodPost, "/v1/sessions/"+other.ID+"/events", "alice", answerBody(`[{},{}]`))
		if a.code() != CodeConflict || !strings.Contains(a.detail(), want+" "+closed.ID) {
			t.Errorf("an answer after %s: %d %s", typ, a.status, a.body)
		}
	}
	// A question nobody attends was answered at once.
	quiet := f.create("alice", "reviewer")
	meta, err := session.Marshal(session.QuestionMeta{ClosedBy: session.ClosedByUnattended})
	if err != nil {
		t.Fatal(err)
	}
	f.put(quiet.ID,
		ev(t, session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: exampleQuestionCall, Name: session.ToolQuestion, Input: json.RawMessage(exampleQuestionInput), Verdict: "allow"}),
		ev(t, session.TypeToolResult, session.ToolResult{ToolUseID: exampleQuestionCall, Outcome: tools.OutcomeUnanswered, Meta: meta}))
	if a := f.do(http.MethodPost, "/v1/sessions/"+quiet.ID+"/events", "alice", answerBody(`[{},{}]`)); a.code() != CodeConflict || !strings.Contains(a.detail(), "nobody attends") {
		t.Fatalf("an answer to a question nobody attends: %d %s", a.status, a.body)
	}
}

// racing is a store whose next append to a session first lets another
// writer append its own event, as a second client whose request lands
// between this one's read of the log and its write does.
type racing struct {
	session.Store
	mu    sync.Mutex
	other func(ctx context.Context, id string) error
}

func (r *racing) Append(ctx context.Context, id string, afterSeq uint64, events []session.Event) (uint64, error) {
	r.mu.Lock()
	other := r.other
	r.other = nil
	r.mu.Unlock()
	if other != nil {
		if err := other(ctx, id); err != nil {
			return 0, err
		}
	}
	return r.Store.Append(ctx, id, afterSeq, events)
}

// TestTwoAnswersAtOnce: of two answers sent at once one is appended and
// the other is conflict, and so of two confirmations of one call: the
// second is checked again against the log the first changed, rather than
// appended after it.
func TestTwoAnswersAtOnce(t *testing.T) {
	var race *racing
	f := newFixture(t, func(o *Options) {
		race = &racing{Store: o.Sessions}
		o.Sessions = race
	})
	f.apply("alice", "reviewer", "Review.")
	competing := func(typ session.Type, payload any) (*session.Event, func(ctx context.Context, id string) error) {
		first := ev(t, typ, payload)
		return &first, func(ctx context.Context, id string) error {
			s, err := f.sessions.Get(ctx, id)
			if err != nil {
				return err
			}
			batch := []session.Event{first}
			session.Stamp(id, s.LastSeq, batch)
			_, err = f.sessions.Append(ctx, id, s.LastSeq, batch)
			return err
		}
	}

	s := f.create("alice", "reviewer")
	f.ask(s.ID)
	first, other := competing(session.TypeUserAnswer, session.UserAnswer{Sender: session.Sender{Subject: bob, Kind: session.SenderPerson}, ToolUseID: exampleQuestionCall, Answers: []session.AnswerEntry{{Selected: []string{"Postgres"}}, {}}})
	race.other = other
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", answerBody(`[{"selected":["SQLite"]},{}]`)); a.code() != CodeConflict || !strings.Contains(a.detail(), "the user.answer "+first.ID+" already answered") {
		t.Fatalf("the answer that lost the race: %d %s", a.status, a.body)
	}
	answers := 0
	for _, e := range f.log(s.ID) {
		if e.Type == session.TypeUserAnswer {
			answers++
		}
	}
	if c, _ := session.Closed(f.log(s.ID), exampleQuestionCall); answers != 1 || c.EventID != first.ID {
		t.Fatalf("%d answers appended, the question closed by %+v", answers, c)
	}

	// Two confirmations of one call.
	c := f.create("alice", "reviewer")
	f.put(c.ID, ev(t, session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_b", Name: "bash", Input: json.RawMessage(`{}`), Verdict: "ask"}))
	_, other = competing(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: session.Sender{Subject: bob, Kind: session.SenderPerson}, ToolUseID: "toolu_b", Decision: session.DecisionDeny})
	race.other = other
	if a := f.do(http.MethodPost, "/v1/sessions/"+c.ID+"/events", "alice", `{"type":"user.tool_confirmation","payload":{"tool_use_id":"toolu_b","decision":"allow"}}`); a.code() != CodeConflict {
		t.Fatalf("the confirmation that lost the race: %d %s", a.status, a.body)
	}
	confirmations := 0
	for _, e := range f.log(c.ID) {
		if e.Type == session.TypeUserToolConfirmation {
			confirmations++
		}
	}
	if confirmations != 1 {
		t.Fatalf("%d confirmations of one call appended", confirmations)
	}

	// A competing event that answers nothing leaves the answer to be
	// checked again and appended after it.
	m := f.create("alice", "reviewer")
	f.ask(m.ID)
	_, other = competing(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning})
	race.other = other
	if a := f.do(http.MethodPost, "/v1/sessions/"+m.ID+"/events", "alice", answerBody(`[{},{}]`)); a.status != http.StatusOK {
		t.Fatalf("an answer after an unrelated append: %d %s", a.status, a.body)
	}
}

// TestAnAnswerIsASend: an answer is asked of the authorizer as
// session.send with the event_type user.answer and the fields every send
// carries; a deny is forbidden, appends nothing and leaves the question
// open; an allow that names another model appends session.model_changed
// straight before the answer.
func TestAnAnswerIsASend(t *testing.T) {
	f, _ := checking(t)
	f.applyModel("asker", "name: "+haiku)
	r := f.route(func(authz.Request) string { return "" })
	var s session.Session
	f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"asker","message":"Build it.","attended":true}`).decode(t, &s)
	f.ask(s.ID)
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", answerBody(`[{"selected":["SQLite"]},{}]`)); a.status != http.StatusOK {
		t.Fatalf("an answer: %d %s", a.status, a.body)
	}
	want := `{"agent":"AGENT","event_type":"user.answer","id":"SESSION","kind":"session","model":"claude-haiku-4-5","owner":"https://login.example|alice","runner":"hosted","sender":"https://login.example|alice"}`
	if got := wire(t, r.last(t, authorizer.ActionSessionSend), s.Agent.ID, "AGENT", s.ID, "SESSION"); got != want {
		t.Fatalf("session.send carries\n%s, want\n%s", got, want)
	}

	// A deny.
	d := f.create("alice", "asker")
	f.ask(d.ID)
	before := len(f.log(d.ID))
	next := f.authz.answer
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionSend {
			return authz.Decision{Allow: false, Reason: "answers_not_enabled"}, nil
		}
		return next(req)
	}
	a := f.do(http.MethodPost, "/v1/sessions/"+d.ID+"/events", "alice", answerBody(`[{"selected":["SQLite"]},{}]`))
	if a.status != http.StatusForbidden || !strings.Contains(string(a.body), "answers_not_enabled") {
		t.Fatalf("a denied answer: %d %s", a.status, a.body)
	}
	if _, open := session.OpenQuestion(f.log(d.ID)); !open || len(f.log(d.ID)) != before {
		t.Fatal("a denied answer appended, or closed the question")
	}
	f.authz.answer = next

	// An allow that names another model.
	r.to(sonnet)
	a = f.do(http.MethodPost, "/v1/sessions/"+d.ID+"/events", "alice", answerBody(`[{"selected":["SQLite"]},{}]`))
	if a.status != http.StatusOK {
		t.Fatalf("a routed answer: %d %s", a.status, a.body)
	}
	evs := f.log(d.ID)
	if len(evs) != before+2 || evs[before].Type != session.TypeModelChanged || evs[before+1].Type != session.TypeUserAnswer {
		t.Fatalf("the routed answer appended %d events", len(evs)-before)
	}
}

// TestRedactingAnAnswerTakesItsResult: redacting a user.answer redacts
// the tool.result the runner rendered from it, whose tombstone still
// answers the call, and the same redaction sent again changes nothing;
// the agent.tool_use of a question whose call has no result yet is
// conflict, and once the result is in the log it is redacted.
func TestRedactingAnAnswerTakesItsResult(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	f.ask(s.ID)
	use := f.log(s.ID)[len(f.log(s.ID))-2]
	redact := func(id string) answer {
		t.Helper()
		return f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events/"+id+"/redact", "alice", `{"reason":"it held a password"}`)
	}
	if a := redact(use.ID); a.code() != CodeConflict || !strings.Contains(a.detail(), "dismiss it with user.interrupt first") {
		t.Fatalf("redacting an open question: %d %s", a.status, a.body)
	}
	var sent session.Event
	f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", answerBody(`[{"text":"the root password is hunter2"},{}]`)).decode(t, &sent)
	if a := redact(use.ID); a.code() != CodeConflict {
		t.Fatalf("redacting a question whose result is owed: %d %s", a.status, a.body)
	}
	meta, err := session.Marshal(session.QuestionMeta{ClosedBy: session.ClosedByAnswer, EventID: sent.ID})
	if err != nil {
		t.Fatal(err)
	}
	result := ev(t, session.TypeToolResult, session.ToolResult{ToolUseID: exampleQuestionCall, Outcome: tools.OutcomeOK, Meta: meta,
		Content: []lux.Block{{Type: ir.BlockText, Text: "In their words: the root password is hunter2"}}})
	f.put(s.ID, ev(t, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning}), result)

	for range 2 {
		if a := redact(sent.ID); a.status != http.StatusNoContent {
			t.Fatalf("redact the answer: %d %s", a.status, a.body)
		}
	}
	redactions := 0
	for _, e := range f.log(s.ID) {
		if strings.Contains(string(e.Payload), "hunter2") {
			t.Fatalf("%s still holds the redacted words: %s", e.Type, e.Payload)
		}
		switch {
		case e.ID == sent.ID && !e.Redacted(), e.ID == result.ID && (!e.Redacted() || e.Answers() != exampleQuestionCall):
			t.Fatalf("%s: %s", e.Type, e.Payload)
		case e.Type == session.TypeEventRedacted:
			redactions++
		}
	}
	if redactions != 2 {
		t.Fatalf("%d event.redacted; the answer and its result are each redacted once", redactions)
	}
	if got := session.Awaiting(f.log(s.ID)); len(got) != 0 {
		t.Fatalf("the redacted answer reopened a call: %v", got)
	}
	if c, _ := session.Closed(f.log(s.ID), exampleQuestionCall); !c.Settled || c.EventID != sent.ID {
		t.Fatalf("the closing after the redaction %+v", c)
	}
	if a := redact(use.ID); a.status != http.StatusNoContent {
		t.Fatalf("redacting a settled question's call: %d %s", a.status, a.body)
	}
}

// TestTheSendRouteShowsAnAnswer: the document shows a user.answer the
// send route takes, the question it answers, and both schemas with the
// bounds of the constants.
func TestTheSendRouteShowsAnAnswer(t *testing.T) {
	raw, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Description string `yaml:"description"`
		} `yaml:"paths"`
		Components struct {
			Schemas struct {
				QuestionInput struct {
					Example    any `yaml:"example"`
					Properties struct {
						Questions struct {
							MaxItems int `yaml:"maxItems"`
						} `yaml:"questions"`
					} `yaml:"properties"`
				} `yaml:"QuestionInput"`
				UserAnswer struct {
					Properties struct {
						Answers struct {
							MaxItems int `yaml:"maxItems"`
							Items    struct {
								Properties struct {
									Text struct {
										MaxLength int `yaml:"maxLength"`
									} `yaml:"text"`
								} `yaml:"properties"`
							} `yaml:"items"`
						} `yaml:"answers"`
					} `yaml:"properties"`
				} `yaml:"UserAnswer"`
				QuestionResultMeta struct {
					Properties struct {
						ClosedBy struct {
							Enum []string `yaml:"enum"`
						} `yaml:"closed_by"`
					} `yaml:"properties"`
				} `yaml:"QuestionResultMeta"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	send := doc.Paths["/sessions/{id}/events"]["post"].Description
	for _, want := range []string{exampleAnswerBody, "event_type user.answer", "of two sent at once one is appended and the other is conflict"} {
		if !strings.Contains(send, want) {
			t.Errorf("the send route does not show %s", want)
		}
	}
	for path, want := range map[string]string{"/sessions": AttendedRule, "/sessions/{id}/fork": `"attended": true`, "/sessions/{id}/events/{event_id}/redact": "A user.answer is redactable"} {
		if !strings.Contains(doc.Paths[path]["post"].Description, want) {
			t.Errorf("%s does not state %q", path, want)
		}
	}
	sc := doc.Components.Schemas
	if sc.QuestionInput.Example == nil || sc.QuestionInput.Properties.Questions.MaxItems != session.MaxQuestions ||
		sc.UserAnswer.Properties.Answers.MaxItems != session.MaxQuestions || sc.UserAnswer.Properties.Answers.Items.Properties.Text.MaxLength != session.MaxAnswerTextLength ||
		len(sc.QuestionResultMeta.Properties.ClosedBy.Enum) != 4 {
		t.Fatalf("the schemas %+v", sc)
	}

	// The example is a send the route takes against the example question.
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	s := f.create("alice", "reviewer")
	f.ask(s.ID)
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/events", "alice", exampleAnswerBody); a.status != http.StatusOK {
		t.Fatalf("the example: %d %s", a.status, a.body)
	}
}

// TestTheAuthorizerDocNamesEveryEventType: the wire documentation an
// authorizer's author reads names each event_type a session.send
// carries, the answer among them, and the interrupt's own action, since
// an authorizer that lists the event types it allows must accept a new
// one before a server that sends it rolls out.
func TestTheAuthorizerDocNamesEveryEventType(t *testing.T) {
	b, err := os.ReadFile("../../authorizer/doc.go")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.Join(strings.Fields(strings.ReplaceAll(string(b), "//", "")), " ")
	for _, typ := range sentTypes {
		if typ == string(session.TypeUserInterrupt) {
			continue
		}
		if !strings.Contains(doc, typ) {
			t.Errorf("the authorizer's documentation does not name the event_type %s", typ)
		}
	}
	for _, want := range []string{"user.interrupt is asked as session.interrupt", "before a server that sends it"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the authorizer's documentation does not say %q", want)
		}
	}
}
