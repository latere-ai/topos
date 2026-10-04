// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

// qlog builds a log event by event, each with the next sequence.
type qlog struct {
	t   *testing.T
	evs []Event
}

func (l *qlog) add(typ Type, payload any) Event {
	l.t.Helper()
	e, err := NewEvent(typ, payload, t0)
	if err != nil {
		l.t.Fatal(err)
	}
	e.Seq = uint64(len(l.evs)) + 1
	l.evs = append(l.evs, e)
	return e
}

// redact replaces an event of the log by its tombstone, as a store does.
func (l *qlog) redact(id string) {
	l.t.Helper()
	i := slices.IndexFunc(l.evs, func(e Event) bool { return e.ID == id })
	tomb, _, err := Tombstone(l.evs[i], uint64(len(l.evs)), Sender{Subject: "usr_ada", Kind: SenderPerson}, "", t0)
	if err != nil {
		l.t.Fatal(err)
	}
	l.evs[i] = tomb
}

var (
	ada   = Sender{Subject: "usr_ada", Name: "Ada", Kind: SenderPerson}
	cron  = Sender{Subject: TriggerSubjectPrefix + "trg_1", Kind: SenderTrigger}
	twoQs = json.RawMessage(`{"questions":[` +
		`{"header":"Storage","question":"Which database?","options":[{"label":"Postgres","description":"The shared cluster.","recommended":true},{"label":"SQLite","description":"A file."}]},` +
		`{"header":"Regions","question":"Which regions?","multiple":true,"options":[{"label":"Europe","description":"Current customers."},{"label":"North America","description":"Two prospects."}]}]}`)
)

func said(words string) UserMessage {
	return UserMessage{Sender: ada, Content: []lux.Block{{Type: ir.BlockText, Text: words}}}
}

func asked(id string) AgentToolUse {
	return AgentToolUse{ToolUseID: id, Name: ToolQuestion, Input: twoQs, Verdict: "allow"}
}

// TestASessionIsNotAttendedByDefault: a Session a program builds is not
// attended unless the program says so, and the field is absent from a
// header that does not set it, so a header written before the field
// existed reads the same.
func TestASessionIsNotAttendedByDefault(t *testing.T) {
	s := New(AgentRef{ID: "agent_x", Version: 1}, ada, RunnerHosted, Machine{Kind: MachineCella}, t0)
	if s.Attended {
		t.Fatal("a new session is attended")
	}
	b, err := Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "attended") {
		t.Fatalf("a header that does not set attended names it: %s", b)
	}
	var old Session
	if err := json.Unmarshal(b, &old); err != nil || old.Attended {
		t.Fatalf("a header without the field: %+v, %v", old, err)
	}
	s.Attended = true
	if b, err = Marshal(s); err != nil || !strings.Contains(string(b), `"attended":true`) {
		t.Fatalf("an attended header: %s, %v", b, err)
	}
}

// TestWhatClosedAQuestion: one function says what closed a question
// call, and Awaiting and HasPendingInput follow it.
func TestWhatClosedAQuestion(t *testing.T) {
	idle := SessionStatus{Status: StatusIdle, StopReason: StopQuestion}
	base := func(t *testing.T) *qlog {
		l := &qlog{t: t}
		l.add(TypeUserMessage, said("Build the service."))
		l.add(TypeSessionStatus, SessionStatus{Status: StatusRunning})
		l.add(TypeAgentToolUse, asked("toolu_q"))
		l.add(TypeSessionStatus, idle)
		return l
	}

	t.Run("open", func(t *testing.T) {
		l := base(t)
		c, ok := Closed(l.evs, "toolu_q")
		if !ok || !c.Open() || c.Owed() {
			t.Fatalf("closing %+v, %v", c, ok)
		}
		q, open := OpenQuestion(l.evs)
		if !open || q.ToolUseID != "toolu_q" || q.Seq != 3 || len(q.Input.Questions) != 2 || q.Input.Questions[1].Options[1].Label != "North America" {
			t.Fatalf("the open question: %+v, %v", q, open)
		}
		if got := Awaiting(l.evs); len(got) != 1 || got["toolu_q"] != AnswerQuestion {
			t.Fatalf("awaiting %v", got)
		}
		if HasPendingInput(l.evs) || Dismissed(l.evs) {
			t.Fatal("an open question is pending input")
		}
		if _, ok := Closed(l.evs, "toolu_other"); ok {
			t.Fatal("a call that was never asked is found")
		}
	})

	t.Run("a message before the call closes nothing", func(t *testing.T) {
		l := &qlog{t: t}
		l.add(TypeUserMessage, said("Build the service."))
		l.add(TypeUserMessage, said("And keep it small."))
		l.add(TypeAgentToolUse, asked("toolu_q"))
		if c, _ := Closed(l.evs, "toolu_q"); !c.Open() {
			t.Fatalf("closing %+v", c)
		}
	})

	for name, c := range map[string]struct {
		typ     Type
		payload any
		by      string
	}{
		"an answer":    {TypeUserAnswer, UserAnswer{Sender: ada, ToolUseID: "toolu_q", Answers: []AnswerEntry{{Selected: []string{"SQLite"}}, {}}}, ClosedByAnswer},
		"a message":    {TypeUserMessage, said("Use whatever is simplest."), ClosedByMessage},
		"an interrupt": {TypeUserInterrupt, UserInterrupt{Sender: ada}, ClosedByInterrupt},
	} {
		t.Run(name, func(t *testing.T) {
			l := base(t)
			first := l.add(c.typ, c.payload)
			// A later event changes nothing: the first closing stands.
			l.add(TypeUserInterrupt, UserInterrupt{Sender: ada})
			l.add(TypeUserMessage, said("Never mind."))
			l.add(TypeUserAnswer, UserAnswer{Sender: ada, ToolUseID: "toolu_q", Answers: []AnswerEntry{{}, {}}})
			got, ok := Closed(l.evs, "toolu_q")
			if !ok || got.By != c.by || got.EventID != first.ID || got.Event.ID != first.ID || got.Settled || !got.Owed() || got.Open() {
				t.Fatalf("closing %+v, %v, want %s by %s", got, ok, c.by, first.ID)
			}
			if _, open := OpenQuestion(l.evs); open {
				t.Fatal("a closed question is open")
			}
			if got := Awaiting(l.evs); len(got) != 0 {
				t.Fatalf("awaiting %v", got)
			}
			if Dismissed(l.evs) != (c.by == ClosedByInterrupt) {
				t.Fatalf("dismissed %v", Dismissed(l.evs))
			}
			// The runner's result settles the call, and its meta says by
			// what.
			meta, err := Marshal(QuestionMeta{ClosedBy: c.by, EventID: first.ID})
			if err != nil {
				t.Fatal(err)
			}
			result := l.add(TypeToolResult, ToolResult{ToolUseID: "toolu_q", Meta: meta})
			got, _ = Closed(l.evs, "toolu_q")
			if !got.Settled || got.ResultID != result.ID || got.By != c.by || got.EventID != first.ID || got.Owed() || got.Open() || got.Event.ID != "" {
				t.Fatalf("settled closing %+v", got)
			}
			if Dismissed(l.evs) {
				t.Fatal("a settled call is still dismissed")
			}
		})
	}

	t.Run("a trigger's message closes nothing", func(t *testing.T) {
		l := base(t)
		l.add(TypeUserMessage, UserMessage{Sender: cron})
		if c, _ := Closed(l.evs, "toolu_q"); !c.Open() {
			t.Fatalf("closing %+v", c)
		}
	})

	t.Run("a redacted answer still closes", func(t *testing.T) {
		l := base(t)
		ans := l.add(TypeUserAnswer, UserAnswer{Sender: ada, ToolUseID: "toolu_q", Answers: []AnswerEntry{{Text: "a secret"}, {}}})
		l.redact(ans.ID)
		c, _ := Closed(l.evs, "toolu_q")
		if c.By != ClosedByAnswer || c.EventID != ans.ID || !c.Event.Redacted() || !c.Owed() {
			t.Fatalf("closing %+v", c)
		}
		if !HasPendingInput(l.evs) {
			t.Fatal("a redacted answer is not pending input")
		}
	})

	t.Run("a redacted result still settles", func(t *testing.T) {
		l := base(t)
		ans := l.add(TypeUserAnswer, UserAnswer{Sender: ada, ToolUseID: "toolu_q", Answers: []AnswerEntry{{Text: "a secret"}, {}}})
		meta, err := Marshal(QuestionMeta{ClosedBy: ClosedByAnswer, EventID: ans.ID})
		if err != nil {
			t.Fatal(err)
		}
		result := l.add(TypeToolResult, ToolResult{ToolUseID: "toolu_q", Meta: meta})
		l.redact(ans.ID)
		l.redact(result.ID)
		c, _ := Closed(l.evs, "toolu_q")
		if !c.Settled || c.ResultID != result.ID || c.By != ClosedByAnswer || c.EventID != ans.ID || c.Owed() {
			t.Fatalf("closing %+v", c)
		}
	})

	t.Run("answered at once, and a result that names no cause", func(t *testing.T) {
		l := &qlog{t: t}
		l.add(TypeUserMessage, said("Build the service."))
		l.add(TypeAgentToolUse, asked("toolu_1"))
		meta, err := Marshal(QuestionMeta{ClosedBy: ClosedByUnattended})
		if err != nil {
			t.Fatal(err)
		}
		l.add(TypeToolResult, ToolResult{ToolUseID: "toolu_1", Outcome: "unanswered", Meta: meta})
		l.add(TypeAgentToolUse, AgentToolUse{ToolUseID: "toolu_2", Name: ToolQuestion, Input: twoQs, Verdict: "block"})
		l.add(TypeToolResult, ToolResult{ToolUseID: "toolu_2", Outcome: "blocked"})
		// A message after a settled call is not what closed it.
		l.add(TypeUserMessage, said("Go on."))
		qs := Questions(l.evs)
		if len(qs) != 2 || qs[0].Closing.By != ClosedByUnattended || qs[0].Closing.EventID != "" || !qs[0].Closing.Settled ||
			qs[1].Closing.By != "" || !qs[1].Closing.Settled || qs[1].Closing.Open() {
			t.Fatalf("questions %+v", qs)
		}
		if _, open := OpenQuestion(l.evs); open {
			t.Fatal("a settled call is open")
		}
	})

	t.Run("an interrupt is pending input only when it closed a question", func(t *testing.T) {
		l := base(t)
		in := l.add(TypeUserInterrupt, UserInterrupt{Sender: ada})
		if !HasPendingInput(l.evs) {
			t.Fatal("an interrupt on an open question is not pending input")
		}
		// Once a runner closed the call and went idle, the same interrupt
		// starts nothing more.
		meta, err := Marshal(QuestionMeta{ClosedBy: ClosedByInterrupt, EventID: in.ID})
		if err != nil {
			t.Fatal(err)
		}
		l.add(TypeSessionStatus, SessionStatus{Status: StatusRunning})
		l.add(TypeToolResult, ToolResult{ToolUseID: "toolu_q", Outcome: "canceled", Meta: meta})
		l.add(TypeSessionStatus, SessionStatus{Status: StatusIdle, StopReason: StopInterrupted})
		if HasPendingInput(l.evs) {
			t.Fatal("a dismissed question is still pending input")
		}
		l.add(TypeUserInterrupt, UserInterrupt{Sender: ada})
		if HasPendingInput(l.evs) {
			t.Fatal("an interrupt on a session idle interrupted is pending input")
		}
		// A session that asked nothing: an interrupt on idle end_turn.
		plain := &qlog{t: t}
		plain.add(TypeUserMessage, said("Hello."))
		plain.add(TypeSessionStatus, SessionStatus{Status: StatusIdle, StopReason: StopEndTurn})
		plain.add(TypeUserInterrupt, UserInterrupt{Sender: ada})
		if HasPendingInput(plain.evs) {
			t.Fatal("an interrupt on a session idle end_turn is pending input")
		}
	})

	t.Run("what is not a question", func(t *testing.T) {
		l := &qlog{t: t}
		// A client tool of the name, as an agent version stored before the
		// name was reserved may declare, is that client's call.
		l.add(TypeAgentToolUse, AgentToolUse{ToolUseID: "toolu_c", Name: ToolQuestion, Client: true})
		l.add(TypeAgentToolUse, AgentToolUse{ToolUseID: "toolu_b", Name: "bash", Verdict: "ask"})
		gone := l.add(TypeAgentToolUse, asked("toolu_gone"))
		l.add(TypeToolResult, ToolResult{ToolUseID: "toolu_gone"})
		l.redact(gone.ID)
		l.evs = append(l.evs, Event{Type: TypeAgentToolUse, Payload: json.RawMessage(`[`)}, Event{Type: TypeUserMessage, Payload: json.RawMessage(`[`)})
		// A question whose input does not decode is still a question.
		l.add(TypeAgentToolUse, AgentToolUse{ToolUseID: "toolu_odd", Name: ToolQuestion, Input: json.RawMessage(`"text"`)})
		qs := Questions(l.evs)
		if len(qs) != 1 || qs[0].ToolUseID != "toolu_odd" || len(qs[0].Input.Questions) != 0 {
			t.Fatalf("questions %+v", qs)
		}
		if got := Awaiting(l.evs); got["toolu_c"] != AnswerResult || got["toolu_b"] != AnswerConfirmation || got["toolu_odd"] != AnswerQuestion || len(got) != 3 {
			t.Fatalf("awaiting %v", got)
		}
	})
}

// TestAnAnswerIsCheckedByShapeAndFit: the checks the send route makes of
// an answer, on its own and against the questions it answers.
func TestAnAnswerIsCheckedByShapeAndFit(t *testing.T) {
	var in QuestionInput
	if err := json.Unmarshal(twoQs, &in); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("é", MaxAnswerTextLength)
	for name, c := range map[string]struct {
		a     UserAnswer
		shape string
		fit   string
	}{
		"a choice and words":   {a: UserAnswer{ToolUseID: "t", Answers: []AnswerEntry{{Selected: []string{"SQLite"}, Text: long}, {Selected: []string{"Europe", "North America"}}}}},
		"left to the agent":    {a: UserAnswer{ToolUseID: "t", Answers: []AnswerEntry{{}, {}}}},
		"no id":                {a: UserAnswer{Answers: []AnswerEntry{{}, {}}}, shape: "tool_use_id"},
		"no answers":           {a: UserAnswer{ToolUseID: "t"}, shape: "one entry per question"},
		"a long text":          {a: UserAnswer{ToolUseID: "t", Answers: []AnswerEntry{{Text: long + "é"}, {}}}, shape: "at most 2000"},
		"too few entries":      {a: UserAnswer{ToolUseID: "t", Answers: []AnswerEntry{{}}}, fit: "1 entries for 2 questions"},
		"too many entries":     {a: UserAnswer{ToolUseID: "t", Answers: []AnswerEntry{{}, {}, {}}}, fit: "3 entries for 2 questions"},
		"an unknown label":     {a: UserAnswer{ToolUseID: "t", Answers: []AnswerEntry{{Selected: []string{"MySQL"}}, {}}}, fit: `"MySQL", which is no option of question 1`},
		"a label twice":        {a: UserAnswer{ToolUseID: "t", Answers: []AnswerEntry{{}, {Selected: []string{"Europe", "Europe"}}}}, fit: `"Europe" twice`},
		"several where one":    {a: UserAnswer{ToolUseID: "t", Answers: []AnswerEntry{{Selected: []string{"Postgres", "SQLite"}}, {}}}, fit: "question 1 takes one"},
		"a label of the other": {a: UserAnswer{ToolUseID: "t", Answers: []AnswerEntry{{Selected: []string{"Europe"}}, {}}}, fit: "no option of question 1"},
	} {
		shape := c.a.CheckShape()
		if (shape == nil) != (c.shape == "") || (shape != nil && !strings.Contains(shape.Error(), c.shape)) {
			t.Errorf("%s: shape %v, want %q", name, shape, c.shape)
		}
		if shape != nil {
			continue
		}
		fit := c.a.Fits(in)
		if (fit == nil) != (c.fit == "") || (fit != nil && !strings.Contains(fit.Error(), c.fit)) {
			t.Errorf("%s: fit %v, want %q", name, fit, c.fit)
		}
	}
	if (UserAnswer{Answers: []AnswerEntry{{}, {}}}).Answered() || !(UserAnswer{Answers: []AnswerEntry{{}, {Text: "x"}}}).Answered() {
		t.Fatal("Answered")
	}
}

// TestTheFoldSkipsAnAnswer: user.answer is a known type that renders
// nothing, redacted or not, since the model reads the answer as the
// result of the question's call; the fold notes its sender, so a message
// of a second person that follows is led by their name.
func TestTheFoldSkipsAnAnswer(t *testing.T) {
	bob := Sender{Subject: "usr_bob", Name: "Bob", Kind: SenderPerson}
	l := &qlog{t: t}
	l.add(TypeUserMessage, said("Build the service."))
	l.add(TypeAgentMessage, AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{
		{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_q", Name: ToolQuestion, Args: twoQs}}}}, StopReason: ir.StopToolUse})
	l.add(TypeAgentToolUse, asked("toolu_q"))
	ans := l.add(TypeUserAnswer, UserAnswer{Sender: bob, ToolUseID: "toolu_q", Answers: []AnswerEntry{{Text: "the words of the answer"}, {}}})
	l.add(TypeToolResult, ToolResult{ToolUseID: "toolu_q", Content: []lux.Block{{Type: ir.BlockText, Text: "The person answered."}}, Outcome: "ok"})
	l.add(TypeUserMessage, said("And add a health check."))

	if !Known[TypeUserAnswer] || !Redactable(TypeUserAnswer) {
		t.Fatal("user.answer is not a known, redactable type")
	}
	render := func() string {
		t.Helper()
		tr, err := Fold(l.evs, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := tr.Check(); err != nil {
			t.Fatal(err)
		}
		b, err := Marshal(tr.Messages)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	got := render()
	if strings.Contains(got, "the words of the answer") {
		t.Fatalf("the fold rendered the answer: %s", got)
	}
	if !strings.Contains(got, "The person answered.") || !strings.Contains(got, "Ada") {
		t.Fatalf("the fold lacks the result, or the sender of the message after an answer of a second person: %s", got)
	}
	// Redacting the answer leaves a transcript a request can be built
	// from, since an answer is not a visible event. Its sender is gone
	// with its payload, so the fold no longer counts a second person.
	l.redact(ans.ID)
	if after := render(); !strings.Contains(after, "The person answered.") || strings.Contains(after, "Message from") {
		t.Fatalf("the transcript after the answer was redacted: %s", after)
	}
}

// TestAnOldLogReadsAsBefore: the golden logs, written before questions
// existed, hold no question and wait for what they waited for.
func TestAnOldLogReadsAsBefore(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "fold", "*.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("the golden logs: %v, %v", paths, err)
	}
	for _, p := range paths {
		evs := readEvents(t, p)
		if qs := Questions(evs); len(qs) != 0 {
			t.Errorf("%s holds questions %+v", p, qs)
		}
		if _, open := OpenQuestion(evs); open || Dismissed(evs) {
			t.Errorf("%s has an open or a dismissed question", p)
		}
		for id, want := range Awaiting(evs) {
			if want == AnswerQuestion {
				t.Errorf("%s: %s awaits an answer to a question", p, id)
			}
		}
		for _, e := range evs {
			if e.Type == TypeUserAnswer {
				t.Errorf("%s holds a user.answer", p)
			}
		}
	}
}

// readEvents reads a log of one JSON event a line.
func readEvents(t *testing.T, path string) []Event {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for line := range strings.Lines(string(b)) {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}
