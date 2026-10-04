// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/dir"
)

const twoLanguages = `{"questions":[` +
	`{"header":"Greeting","question":"In which language should greeting.txt greet the readers?","options":[{"label":"English","recommended":true,"description":"hello"},{"label":"German (Deutsch)","description":"hallo"}]},` +
	`{"header":"Farewell","question":"In which language should farewell.txt say goodbye?","options":[{"label":"English","recommended":true,"description":"goodbye"},{"label":"German","description":"auf Wiedersehen"}]},` +
	`{"header":"Signature","question":"Who signs it?","options":[{"label":"The team","description":"x"},{"label":"Nobody","description":"y"}]}]}`

// TestADriverAnswersAQuestion: a task that names its answers runs in an
// attended session, and its driver answers the open question as a person
// does: each question by the answer whose question its header or text
// holds, the option whose label holds the choice, the choice in the
// person's words when no label holds it, and an empty entry for a
// question no answer names. The scripted solution of question-answered
// runs through it to the end of its turn and passes.
func TestADriverAnswersAQuestion(t *testing.T) {
	var in session.QuestionInput
	if err := json.Unmarshal([]byte(twoLanguages), &in); err != nil {
		t.Fatal(err)
	}
	q := session.Asked{ToolUseID: "toolu_q", Input: in}
	ada := session.Sender{Subject: "local:tasks", Kind: session.SenderPerson}
	got := AnswerTo(q, ada, []Answer{{Question: "GREETING", Choose: "german"}, {Question: "goodbye", Choose: "Klingon", Text: "if you can"}})
	want := []session.AnswerEntry{{Selected: []string{"German (Deutsch)"}}, {Text: "Klingon if you can"}, {}}
	if got.ToolUseID != "toolu_q" || got.Sender != ada || len(got.Answers) != 3 {
		t.Fatalf("answer %+v", got)
	}
	for i := range want {
		if !slices.Equal(got.Answers[i].Selected, want[i].Selected) || got.Answers[i].Text != want[i].Text {
			t.Fatalf("entry %d is %+v, want %+v", i, got.Answers[i], want[i])
		}
	}
	if err := got.Fits(in); err != nil {
		t.Fatalf("the driver's answer does not fit its question: %v", err)
	}

	task, err := LoadTask(filepath.Join("instructions", "question-answered"))
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Answers) != 1 || task.Answers[0].Choose != "German" {
		t.Fatalf("the task names %+v", task.Answers)
	}
	res, err := RunTask(t.Context(), task, Options{Work: t.TempDir(), Script: FileSolution}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Fatalf("the solution failed: %s\nlog %s", res.Reason, res.Log)
	}
	st, err := dir.Open(filepath.Dir(filepath.Dir(filepath.Dir(res.Log))))
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Get(t.Context(), res.Session)
	if err != nil || !s.Attended || s.Status != session.StatusEnded {
		t.Fatalf("the session %+v, %v", s, err)
	}
	evs, err := st.Events(t.Context(), res.Session, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var answers []session.UserAnswer
	idled := false
	for _, e := range evs {
		var st session.SessionStatus
		if e.Type == session.TypeSessionStatus && e.Decode(&st) == nil && st.StopReason == session.StopQuestion {
			idled = true
		}
		var a session.UserAnswer
		if e.Type == session.TypeUserAnswer && e.Decode(&a) == nil {
			answers = append(answers, a)
		}
	}
	if !idled || len(answers) != 1 || !slices.Equal(answers[0].Answers[0].Selected, []string{"German"}) || !answers[0].Answers[1].Empty() {
		t.Fatalf("the session went idle on the question %v and was answered %+v", idled, answers)
	}

	// A task that names answers holds the question tool, and each answer
	// names its question and what to say.
	for name, files := range map[string]map[string]string{
		"no question tool": {"task.yaml": "name: t\ncategory: instructions\nprompt: Go.\nanswers: [{question: a, choose: b}]\n"},
		"no question":      {"task.yaml": "name: t\ncategory: instructions\nprompt: Go.\nagent: {tools: [question]}\nanswers: [{choose: b}]\n"},
		"nothing to say":   {"task.yaml": "name: t\ncategory: instructions\nprompt: Go.\nagent: {tools: [question]}\nanswers: [{question: a}]\n"},
	} {
		root := filepath.Join(t.TempDir(), "instructions", "t")
		files["check.yaml"] = minimalCheck
		writeFiles(t, root, files)
		if _, err := LoadTask(root); err == nil || !strings.Contains(err.Error(), "answers") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestQuestionAssertions: question counts the root thread's question
// calls and holds each to described options, a recommended option
// first, and a text that does not match not_matching; final_message
// matches the root thread's last agent.message.
func TestQuestionAssertions(t *testing.T) {
	f := newFixture(t)
	good := `{"questions":[{"header":"A","question":"Which store?","options":[{"label":"x","recommended":true,"description":"d"},{"label":"y","description":"e"}]}]}`
	late := `{"questions":[{"header":"A","question":"Which store?","options":[{"label":"x","description":"d"},{"label":"y","recommended":true,"description":"e"}]}]}`
	bare := `{"questions":[{"header":"A","question":"Which store?","options":[{"label":"x","description":" "},{"label":"y","description":"e"}]}]}`
	asks := `{"questions":[{"header":"A","question":"May I delete the build directory?","options":[{"label":"x","description":"d"},{"label":"y","description":"e"}]}]}`
	said := func(thread, text string) session.Event {
		return event(t, session.TypeAgentMessage, thread, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: text}}}, StopReason: ir.StopEndTurn})
	}
	for _, c := range []struct {
		calls []string
		src   string
		pass  bool
		want  string
	}{
		{[]string{good}, "- question: {}\n", true, ""},
		{[]string{good, good}, "- question: {}\n", false, "2 question calls, want 1"},
		{nil, "- question: {calls: 0}\n", true, ""},
		{[]string{late}, "- question: {}\n", false, `the recommended option "y" of question 1 is not first`},
		{[]string{bare}, "- question: {}\n", false, `option "x" of question 1 has no description`},
		{[]string{asks}, "- question: {not_matching: '(?i)may i'}\n", false, "matches (?i)may i"},
		{[]string{`"text"`}, "- question: {}\n", false, "does not decode"},
	} {
		var evs []session.Event
		for i, in := range c.calls {
			evs = append(evs, call(t, "", "q"+string(rune('0'+i)), session.ToolQuestion, in, "unanswered")...)
		}
		evs = append(evs, call(t, "thr_1", "qs", session.ToolQuestion, good, "unanswered")...)
		f.in.Events = evs
		if v := f.judge(c.src); v.Pass != c.pass || !strings.Contains(v.Reason, c.want) {
			t.Errorf("%s with %d calls: %+v, want pass %v naming %q", c.src, len(c.calls), v, c.pass, c.want)
		}
	}
	f.in.Events = []session.Event{said("", "First."), said("thr_1", "A thread's words."), said("", "I assumed English for the farewell.")}
	if v := f.judge("- final_message: {matches: '(?i)assumed english'}\n"); !v.Pass {
		t.Fatalf("final_message: %+v", v)
	}
	if v := f.judge("- final_message: {matches: 'thread'}\n"); v.Pass || !strings.Contains(v.Reason, "does not match") {
		t.Fatalf("final_message reads the root thread's last message: %+v", v)
	}
	for src, want := range map[string]string{"- final_message: {}\n": "no pattern", "- question: {not_matching: '('}\n": "missing closing"} {
		if _, err := ParseCheck([]byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", src, err)
		}
	}
}
