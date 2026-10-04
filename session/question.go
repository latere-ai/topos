// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"
)

// ToolQuestion is the name of the tool that puts a decision to a person
// (spec 039). The log is read by it: an agent.tool_use of this name that
// no client runs is a question.
const ToolQuestion = "question"

// The bounds of a question call and of its answer (spec 039). The tool's
// schema, the server's check of an answer, the API document and the
// tool's description are rendered from them or held to them by a test.
// A length is in characters, Unicode code points.
const (
	// MaxQuestions is the most questions of one call; a call holds at
	// least one.
	MaxQuestions = 4
	// MinQuestionOptions and MaxQuestionOptions bound the options of one
	// question.
	MinQuestionOptions = 2
	MaxQuestionOptions = 4
	// MaxQuestionHeaderLength bounds a question's header, a short label
	// such as a tab's title.
	MaxQuestionHeaderLength = 12
	// MaxQuestionLength bounds a question's text.
	MaxQuestionLength = 300
	// MaxOptionLabelLength bounds an option's label.
	MaxOptionLabelLength = 40
	// MaxOptionDescriptionLength bounds an option's description.
	MaxOptionDescriptionLength = 200
	// MaxOptionPreviewLines bounds the lines of an option's preview, and
	// MaxOptionPreviewColumns the length of one of them.
	MaxOptionPreviewLines   = 15
	MaxOptionPreviewColumns = 40
	// MaxAnswerTextLength bounds the person's own words in one entry of
	// an answer.
	MaxAnswerTextLength = 2000
)

// QuestionInput is the input of a question call, as its agent.tool_use
// holds it once the harness has checked it.
type QuestionInput struct {
	Questions []Question `json:"questions"`
}

// Question is one question of a call.
type Question struct {
	// Question is the full question, one a person can answer without
	// reading the transcript.
	Question string `json:"question"`
	// Header is a short label for the question.
	Header string `json:"header"`
	// Options are the choices, in the order they are shown.
	Options []QuestionOption `json:"options"`
	// Multiple lets the person choose several options.
	Multiple bool `json:"multiple,omitempty"`
}

// QuestionOption is one choice of a question.
type QuestionOption struct {
	// Label names the option, unique in its question; an answer names an
	// option by it.
	Label string `json:"label"`
	// Description says what choosing the option means.
	Description string `json:"description"`
	// Preview is a mockup or a snippet as lines of plain text.
	Preview []string `json:"preview,omitempty"`
	// Recommended marks the option the agent would take.
	Recommended bool `json:"recommended,omitempty"`
}

// UserAnswer is the payload of user.answer: a person's answer to the
// open question call, one entry per question in the questions' order.
type UserAnswer struct {
	Sender    Sender        `json:"sender"`
	ToolUseID string        `json:"tool_use_id"`
	Answers   []AnswerEntry `json:"answers"`
}

// AnswerEntry answers one question. Selected alone is a choice, Text
// alone an answer in the person's own words, both a choice and their
// note on it, and neither leaves the question to the agent.
type AnswerEntry struct {
	Selected []string `json:"selected,omitempty"`
	Text     string   `json:"text,omitempty"`
}

// Empty reports whether the entry leaves its question to the agent.
func (a AnswerEntry) Empty() bool { return len(a.Selected) == 0 && a.Text == "" }

// Answered reports whether the answer holds an entry that is not empty.
// An answer whose entries are all empty leaves every question to the
// agent.
func (a UserAnswer) Answered() bool {
	return slices.ContainsFunc(a.Answers, func(e AnswerEntry) bool { return !e.Empty() })
}

// CheckShape checks what an answer holds on its own, before anything of
// the session is read: the call it names, its entries, and each entry's
// text within MaxAnswerTextLength.
func (a UserAnswer) CheckShape() error {
	switch {
	case a.ToolUseID == "":
		return errors.New("a user.answer names its tool_use_id")
	case a.Answers == nil:
		return errors.New("a user.answer holds answers, one entry per question")
	}
	for i, e := range a.Answers {
		if n := utf8.RuneCountInString(e.Text); n > MaxAnswerTextLength {
			return fmt.Errorf("answers[%d].text is %d characters, at most %d", i, n, MaxAnswerTextLength)
		}
	}
	return nil
}

// Fits checks an answer against the questions it answers: one entry per
// question, each selected label a label of that question and none twice,
// and more than one label only where the question takes several.
func (a UserAnswer) Fits(in QuestionInput) error {
	if len(a.Answers) != len(in.Questions) {
		return fmt.Errorf("answers holds %d entries for %d questions; send one entry per question, an empty one for a question left to the agent", len(a.Answers), len(in.Questions))
	}
	for i, e := range a.Answers {
		q := in.Questions[i]
		if len(e.Selected) > 1 && !q.Multiple {
			return fmt.Errorf("answers[%d].selected holds %d labels, and question %d takes one", i, len(e.Selected), i+1)
		}
		seen := map[string]bool{}
		for _, label := range e.Selected {
			switch {
			case !slices.ContainsFunc(q.Options, func(o QuestionOption) bool { return o.Label == label }):
				return fmt.Errorf("answers[%d].selected names %q, which is no option of question %d", i, label, i+1)
			case seen[label]:
				return fmt.Errorf("answers[%d].selected names %q twice", i, label)
			}
			seen[label] = true
		}
	}
	return nil
}

// What closed a question call, as Closing.By and a result's meta name it.
const (
	// ClosedByAnswer is a user.answer.
	ClosedByAnswer = "answer"
	// ClosedByMessage is a person's user.message sent in place of an
	// answer.
	ClosedByMessage = "message"
	// ClosedByInterrupt is a user.interrupt, which dismisses the
	// question.
	ClosedByInterrupt = "interrupt"
	// ClosedByUnattended is nobody: the session is not attended, and the
	// call was answered in the step that made it.
	ClosedByUnattended = "unattended"
)

// QuestionMeta is what the tool.result of a question call records in its
// meta: what closed the call, and the id of the event that did, absent
// when nobody attends the session.
type QuestionMeta struct {
	ClosedBy string `json:"closed_by,omitempty"`
	EventID  string `json:"event_id,omitempty"`
}

// Closing is what closed a question call.
type Closing struct {
	// By is ClosedByAnswer, ClosedByMessage or ClosedByInterrupt for a
	// call a person's event closed, ClosedByUnattended for one answered
	// at once, and empty for a call that is open, or that a result closed
	// without naming a cause, as a blocked call's does.
	By string
	// EventID is the id of the event that closed the call.
	EventID string
	// Event is that event while the call's result is owed, the zero Event
	// otherwise. A redacted user.answer is here as its tombstone.
	Event Event
	// Settled reports that the call's tool.result is in the log, so a
	// runner owes it nothing, and ResultID is that result's event id.
	Settled  bool
	ResultID string
}

// Open reports whether nothing closed the call: it waits for the person.
func (c Closing) Open() bool { return c.By == "" && !c.Settled }

// Owed reports whether an event closed the call and its result is not in
// the log yet: the next runner appends it.
func (c Closing) Owed() bool { return c.By != "" && !c.Settled }

// Asked is one question call of a log.
type Asked struct {
	ToolUseID string
	// Seq is the sequence of the call's agent.tool_use.
	Seq uint64
	// Input is the call's input as its agent.tool_use holds it.
	Input   QuestionInput
	Closing Closing
}

// Questions lists the question calls of a log, in the order they were
// asked, each with what closed it. It is the one rule of what closed a
// question (spec 039), which Closed, OpenQuestion, Awaiting,
// HasPendingInput, the server's check of an answer and the harness all
// read:
//
//   - a call whose tool.result is in the log is closed, and the result's
//     meta says by what;
//   - else the first user.answer, person's user.message or
//     user.interrupt after the call's agent.tool_use closed it, in log
//     order, and a later one changes nothing;
//   - else the call is open.
//
// A message before the call's agent.tool_use closes nothing. A redacted
// user.answer still closes its call: at most one call is open at a time,
// so the answer needs no id to be matched. evs are in sequence order.
func Questions(evs []Event) []Asked {
	var out []Asked
	// unclosed are the indexes in out of the calls no event closed yet.
	var unclosed []int
	at := map[string]int{}
	for _, e := range evs {
		if id := e.Answers(); id != "" {
			i, ok := at[id]
			if !ok {
				continue
			}
			c := &out[i].Closing
			c.Settled, c.ResultID, c.Event = true, e.ID, Event{}
			var p ToolResult
			var m QuestionMeta
			if !e.Redacted() && e.Decode(&p) == nil && len(p.Meta) > 0 && json.Unmarshal(p.Meta, &m) == nil && m.ClosedBy != "" {
				c.By, c.EventID = m.ClosedBy, m.EventID
			}
			unclosed = slices.DeleteFunc(unclosed, func(j int) bool { return j == i })
			continue
		}
		var by string
		switch e.Type {
		case TypeAgentToolUse:
			var p AgentToolUse
			if e.Redacted() || e.Decode(&p) != nil || p.Name != ToolQuestion || p.Client {
				continue
			}
			a := Asked{ToolUseID: p.ToolUseID, Seq: e.Seq}
			// An input that does not decode is one no runner of this build
			// recorded; the call is still a question, with no questions.
			if err := json.Unmarshal(p.Input, &a.Input); err != nil {
				a.Input = QuestionInput{}
			}
			at[p.ToolUseID] = len(out)
			unclosed = append(unclosed, len(out))
			out = append(out, a)
			continue
		case TypeUserAnswer:
			by = ClosedByAnswer
		case TypeUserMessage:
			var p UserMessage
			if e.Redacted() || e.Decode(&p) != nil || p.Sender.Kind != SenderPerson {
				continue
			}
			by = ClosedByMessage
		case TypeUserInterrupt:
			if e.Redacted() {
				continue
			}
			by = ClosedByInterrupt
		default:
			continue
		}
		for _, i := range unclosed {
			out[i].Closing = Closing{By: by, EventID: e.ID, Event: e}
		}
		unclosed = nil
	}
	return out
}

// Closed says what closed the question call toolUseID, from the log
// alone; ok is false when the log holds no question call of that id.
func Closed(evs []Event, toolUseID string) (c Closing, ok bool) {
	for _, a := range Questions(evs) {
		if a.ToolUseID == toolUseID {
			return a.Closing, true
		}
	}
	return Closing{}, false
}

// OpenQuestion is the question call of a log that nothing closed, the
// one a person is shown. There is at most one: only the session's own
// thread asks, a step takes one call, and no step follows one whose call
// is open.
func OpenQuestion(evs []Event) (Asked, bool) {
	for _, a := range slices.Backward(Questions(evs)) {
		if a.Closing.Open() {
			return a, true
		}
	}
	return Asked{}, false
}

// owedQuestion reports whether an event closed a question call of the
// log whose result is not in it yet, and that event's id.
func owedQuestion(evs []Event) (Closing, bool) {
	for _, a := range slices.Backward(Questions(evs)) {
		if a.Closing.Owed() {
			return a.Closing, true
		}
	}
	return Closing{}, false
}

// Dismissed reports whether the log holds a question call that a
// user.interrupt closed and whose result no runner appended yet: the
// one interrupt a runner acts on while a session is idle.
func Dismissed(evs []Event) bool {
	c, owed := owedQuestion(evs)
	return owed && c.By == ClosedByInterrupt
}
