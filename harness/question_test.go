// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// twoQuestions is a valid input of the question tool: one question that
// takes one option, with a recommended option and a preview, and one
// that takes several.
const twoQuestions = `{"questions":[` +
	`{"header":"Storage","question":"Which database should the new service keep its records in?","options":[` +
	`{"label":"Postgres","recommended":true,"description":"The cluster the other services use."},` +
	`{"label":"SQLite","description":"A file beside the binary.","preview":["data/","  service.db"]}]},` +
	`{"header":"Regions","multiple":true,"question":"Which regions does the first release serve?","options":[` +
	`{"label":"Europe","description":"Where the current customers are."},` +
	`{"label":"North America","description":"Two prospects asked for it."}]}]}`

// asking sets a harness up whose agent names the question tool, in a
// session a person attends or not.
func asking(t *testing.T, attended bool, muts ...func(*Config)) *env {
	t.Helper()
	return setupSession(t, func(c *Config) {
		c.Question = true
		for _, m := range muts {
			m(c)
		}
	}, func(s *session.Session) { s.Attended = attended })
}

// answer appends a person's user.answer to the call and returns it.
func (e *env) answer(ctx context.Context, id string, entries ...session.AnswerEntry) session.Event {
	e.t.Helper()
	ev, err := session.NewEvent(session.TypeUserAnswer, session.UserAnswer{Sender: e.s.Initiator, ToolUseID: id, Answers: entries}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.appendEvents(ctx, ev)
	return ev
}

// say appends a person's message and returns it.
func (e *env) say(ctx context.Context, words string) session.Event {
	e.t.Helper()
	ev, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: words}}}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.appendEvents(ctx, ev)
	return ev
}

// interrupt appends a person's user.interrupt and returns it.
func (e *env) interrupt(ctx context.Context) session.Event {
	e.t.Helper()
	ev, err := session.NewEvent(session.TypeUserInterrupt, session.UserInterrupt{Sender: e.s.Initiator}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.appendEvents(ctx, ev)
	return ev
}

// resultOf is the tool.result of a call, its event and its meta; it
// fails when the call has none, or more than one.
func (e *env) resultOf(id string) (session.ToolResult, session.Event, session.QuestionMeta) {
	e.t.Helper()
	var found []session.Event
	for _, ev := range e.all() {
		if ev.Answers() == id {
			found = append(found, ev)
		}
	}
	if len(found) != 1 {
		e.t.Fatalf("%s has %d results, want one", id, len(found))
	}
	var res session.ToolResult
	if err := found[0].Decode(&res); err != nil {
		e.t.Fatal(err)
	}
	var meta session.QuestionMeta
	if len(res.Meta) > 0 {
		if err := json.Unmarshal(res.Meta, &meta); err != nil {
			e.t.Fatal(err)
		}
	}
	return res, found[0], meta
}

// offered are the names of the tools a request offers.
func offered(r *ir.Request) []string {
	var names []string
	for _, tool := range r.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// TestQuestionIsOfferedWhenNamed: the harness offers question to the
// session's own thread when the agent's tools name it, and to no thread
// a spawn starts, whatever tools the subagent or the spawn names; an
// agent that does not name it is not offered it, and the built-in set
// stays the eight.
func TestQuestionIsOfferedWhenNamed(t *testing.T) {
	var builtins []string
	for _, b := range tools.Builtins() {
		builtins = append(builtins, b.Definition().Name)
	}
	if want := []string{"read", "write", "edit", "bash", "grep", "glob", "web_fetch", "todo"}; !slices.Equal(builtins, want) {
		t.Fatalf("the built-in set is %v, want %v", builtins, want)
	}

	t.Run("named", func(t *testing.T) {
		e := asking(t, false, withReviewer(func(s *Subagent) { s.Tools = []string{"echo", ToolQuestion} }))
		ctx := t.Context()
		e.stub.Script(model,
			luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{spawnCall("toolu_s", `{"agent":"reviewer","task":"Review it.","tools":["echo","question"]}`)}, StopReason: ir.StopToolUse}, Expect: func(r *ir.Request) error {
				if names := offered(r); !slices.Contains(names, ToolQuestion) {
					return fmt.Errorf("the session's own thread is offered %v", names)
				}
				for _, tool := range r.Tools {
					if tool.Name == ToolQuestion && !strings.Contains(tool.Description, "Puts a decision to the person") {
						return fmt.Errorf("the description is %q", tool.Description)
					}
				}
				return nil
			}},
			reply(ir.StopEndTurn, text("Reviewed.")),
		)
		e.stub.Script(reviewerModel, luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{text("Fine.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			if names := offered(r); !slices.Equal(names, []string{"echo"}) {
				return fmt.Errorf("a spawned thread is offered %v, want echo alone", names)
			}
			return nil
		}})
		e.send(ctx, "Review.")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("outcome %+v", out)
		}
		_, started := e.thread(e.all())
		if slices.Contains(started.Tools, ToolQuestion) {
			t.Fatalf("thread.started records the tools %v", started.Tools)
		}
	})

	t.Run("not named", func(t *testing.T) {
		e := setup(t, nil)
		ctx := t.Context()
		e.stub.Script(model,
			luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{call("toolu_q", ToolQuestion, twoQuestions)}, StopReason: ir.StopToolUse}, Expect: func(r *ir.Request) error {
				if names := offered(r); slices.Contains(names, ToolQuestion) {
					return fmt.Errorf("an agent that does not name the tool is offered %v", names)
				}
				return nil
			}},
			reply(ir.StopEndTurn, text("ok")),
		)
		e.send(ctx, "Go.")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("outcome %+v", out)
		}
		if res, _, _ := e.resultOf("toolu_q"); res.Outcome != tools.OutcomeUnknownTool {
			t.Fatalf("a call of a tool the agent does not hold: %+v", res)
		}
	})
}

// TestQuestionSchemaFollowsTheConstants: every bound of the tool's
// schema is its constant of package session, and the registry refuses a
// call one past each bound and takes a call at each.
func TestQuestionSchemaFollowsTheConstants(t *testing.T) {
	var s struct {
		Properties struct {
			Questions struct {
				MinItems, MaxItems int
				Items              struct {
					Properties struct {
						Question, Header struct{ MaxLength int }
						Options          struct {
							MinItems, MaxItems int
							Items              struct {
								Properties struct {
									Label, Description struct{ MaxLength int }
									Preview            struct {
										MaxItems int
										Items    struct{ MaxLength int }
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(QuestionSchema, &s); err != nil {
		t.Fatal(err)
	}
	q, o := s.Properties.Questions, s.Properties.Questions.Items.Properties.Options
	got := map[string]int{
		"questions min": q.MinItems, "questions max": q.MaxItems,
		"question": q.Items.Properties.Question.MaxLength, "header": q.Items.Properties.Header.MaxLength,
		"options min": o.MinItems, "options max": o.MaxItems,
		"label": o.Items.Properties.Label.MaxLength, "description": o.Items.Properties.Description.MaxLength,
		"preview lines": o.Items.Properties.Preview.MaxItems, "preview columns": o.Items.Properties.Preview.Items.MaxLength,
	}
	want := map[string]int{
		"questions min": 1, "questions max": session.MaxQuestions,
		"question": session.MaxQuestionLength, "header": session.MaxQuestionHeaderLength,
		"options min": session.MinQuestionOptions, "options max": session.MaxQuestionOptions,
		"label": session.MaxOptionLabelLength, "description": session.MaxOptionDescriptionLength,
		"preview lines": session.MaxOptionPreviewLines, "preview columns": session.MaxOptionPreviewColumns,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("the schema's %s is %d, the constant %d", k, got[k], v)
		}
	}

	reg := tools.NewRegistry()
	if err := reg.AddBuiltin(questionTool{}); err != nil {
		t.Fatal(err)
	}
	// build renders a call of n questions, each of m options, with texts
	// of the given lengths, in a script that takes several bytes a
	// character, since the bounds count characters.
	type sizes struct{ questions, options, header, question, label, description, lines, columns int }
	build := func(z sizes) json.RawMessage {
		rep := func(n int) string { return strings.Repeat("é", n) }
		in := session.QuestionInput{Questions: []session.Question{}}
		for i := range z.questions {
			q := session.Question{Header: rep(z.header), Question: rep(z.question)}
			for j := range z.options {
				o := session.QuestionOption{Label: fmt.Sprintf("%d%d", i, j) + rep(z.label-2), Description: rep(z.description)}
				for range z.lines {
					o.Preview = append(o.Preview, rep(z.columns))
				}
				q.Options = append(q.Options, o)
			}
			in.Questions = append(in.Questions, q)
		}
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	atBounds := sizes{session.MaxQuestions, session.MaxQuestionOptions, session.MaxQuestionHeaderLength, session.MaxQuestionLength,
		session.MaxOptionLabelLength, session.MaxOptionDescriptionLength, session.MaxOptionPreviewLines, session.MaxOptionPreviewColumns}
	if _, bad := reg.Validate(ToolQuestion, build(atBounds)); bad != nil {
		t.Fatalf("a call at every bound is refused: %s", bad.Content[0].Text)
	}
	if _, bad := reg.Validate(ToolQuestion, json.RawMessage(twoQuestions)); bad != nil {
		t.Fatalf("the example is refused: %s", bad.Content[0].Text)
	}
	for name, c := range map[string]struct {
		mut  func(*sizes)
		want string
	}{
		"too many questions":   {func(z *sizes) { z.questions++ }, fmt.Sprintf("/questions: more than %d items", session.MaxQuestions)},
		"no question":          {func(z *sizes) { z.questions = 0 }, "/questions: fewer than 1 items"},
		"too many options":     {func(z *sizes) { z.options++ }, fmt.Sprintf("/questions/0/options: more than %d items", session.MaxQuestionOptions)},
		"one option":           {func(z *sizes) { z.options = 1 }, fmt.Sprintf("/questions/0/options: fewer than %d items", session.MinQuestionOptions)},
		"a long header":        {func(z *sizes) { z.header++ }, fmt.Sprintf("/questions/0/header: longer than %d characters", session.MaxQuestionHeaderLength)},
		"a long question":      {func(z *sizes) { z.question++ }, fmt.Sprintf("/questions/0/question: longer than %d characters", session.MaxQuestionLength)},
		"a long label":         {func(z *sizes) { z.label++ }, fmt.Sprintf("/questions/0/options/0/label: longer than %d characters", session.MaxOptionLabelLength)},
		"a long description":   {func(z *sizes) { z.description++ }, fmt.Sprintf("/questions/0/options/0/description: longer than %d characters", session.MaxOptionDescriptionLength)},
		"too many lines":       {func(z *sizes) { z.lines++ }, fmt.Sprintf("/questions/0/options/0/preview: more than %d items", session.MaxOptionPreviewLines)},
		"a long line":          {func(z *sizes) { z.columns++ }, fmt.Sprintf("/questions/0/options/0/preview/0: longer than %d characters", session.MaxOptionPreviewColumns)},
		"an empty description": {func(z *sizes) { z.description = 0 }, "/questions/0/options/0/description: shorter than 1 characters"},
	} {
		z := atBounds
		c.mut(&z)
		_, bad := reg.Validate(ToolQuestion, build(z))
		if bad == nil || bad.Outcome != tools.OutcomeInvalidInput || !strings.Contains(bad.Content[0].Text, c.want) {
			t.Errorf("%s: %+v, want %q", name, bad, c.want)
		}
	}
	if _, bad := reg.Validate(ToolQuestion, json.RawMessage(`{"questions":[{"header":"A","question":"B?","options":[{"label":"x","description":"d"},{"label":"y","description":"d","other":true}]}]}`)); bad == nil {
		t.Error("an option with a field the schema does not name is accepted")
	}
}

// TestAQuestionCallIsChecked: a call that uses a label twice in one
// question is answered invalid_input and leaves no agent.tool_use; of
// two question calls in one step the first is asked and the second is
// answered invalid_input; a call past a count is refused by the schema.
func TestAQuestionCallIsChecked(t *testing.T) {
	const twice = `{"questions":[{"header":"Storage","question":"Which database?","options":[{"label":"Postgres","description":"The cluster."},{"label":"Postgres","description":"The same name again."}]}]}`
	const one = `{"questions":[{"header":"Storage","question":"Which database?","options":[{"label":"Postgres","description":"The cluster."}]}]}`
	const other = `{"questions":[{"header":"Queue","question":"Which queue?","options":[{"label":"Kafka","description":"A cluster to run."},{"label":"None","description":"A table."}]}]}`

	e := asking(t, false)
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_twice", ToolQuestion, twice), call("toolu_short", ToolQuestion, one)),
		reply(ir.StopToolUse, call("toolu_first", ToolQuestion, twoQuestions), call("toolu_second", ToolQuestion, other)),
		reply(ir.StopEndTurn, text("Decided.")),
	)
	e.send(ctx, "Build the service.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	res, _, _ := e.resultOf("toolu_twice")
	if res.Outcome != tools.OutcomeInvalidInput || !res.IsError || !strings.Contains(res.Content[0].Text, `Question 1 uses the label "Postgres" twice`) {
		t.Fatalf("a label used twice: %+v", res)
	}
	res, _, _ = e.resultOf("toolu_short")
	if res.Outcome != tools.OutcomeInvalidInput || !strings.Contains(res.Content[0].Text, fmt.Sprintf("/questions/0/options: fewer than %d items", session.MinQuestionOptions)) {
		t.Fatalf("one option: %+v", res)
	}
	res, _, meta := e.resultOf("toolu_first")
	if res.Outcome != tools.OutcomeUnanswered || meta.ClosedBy != session.ClosedByUnattended {
		t.Fatalf("the first call of the step: %+v, %+v", res, meta)
	}
	res, _, _ = e.resultOf("toolu_second")
	if res.Outcome != tools.OutcomeInvalidInput || !res.IsError || !strings.Contains(res.Content[0].Text, "A step takes one question call") ||
		!strings.Contains(res.Content[0].Text, fmt.Sprintf("at most %d questions", session.MaxQuestions)) {
		t.Fatalf("the second call of the step: %+v", res)
	}
	var recorded []string
	for _, ev := range e.events(ctx, session.TypeAgentToolUse) {
		var use session.AgentToolUse
		if err := ev.Decode(&use); err != nil {
			t.Fatal(err)
		}
		recorded = append(recorded, use.ToolUseID)
	}
	if !slices.Equal(recorded, []string{"toolu_first"}) {
		t.Fatalf("agent.tool_use was recorded for %v, want the one call that was asked", recorded)
	}

	// A refused first call does not use up the step's one call.
	a := asking(t, false)
	a.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_twice", ToolQuestion, twice), call("toolu_valid", ToolQuestion, other)),
		reply(ir.StopEndTurn, text("Decided.")),
	)
	a.send(ctx, "Build the service.")
	if out := a.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if res, _, _ := a.resultOf("toolu_valid"); res.Outcome != tools.OutcomeUnanswered {
		t.Fatalf("a valid call after a refused one: %+v", res)
	}
}

// TestAQuestionNobodyAttendsIsAnsweredAtOnce: in a session that is not
// attended a valid call is recorded and answered in the same step, with
// the outcome unanswered, which is not an error, and the cause in its
// meta; the model reads the instruction to decide, and the session never
// goes idle on the question.
func TestAQuestionNobodyAttendsIsAnsweredAtOnce(t *testing.T) {
	e := asking(t, false)
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_q", ToolQuestion, twoQuestions), call("toolu_e", "echo", `{"text":"beside"}`)),
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("I assumed Postgres and Europe.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			if len(last.Blocks) != 2 || last.Blocks[0].ToolResult == nil || last.Blocks[0].ToolResult.IsError {
				return fmt.Errorf("the model did not read an unanswered result that is no error: %+v", last)
			}
			if got := last.Blocks[0].ToolResult.Blocks[0].Text; !strings.Contains(got, "Nobody attends this session") || !strings.Contains(got, "do not ask again in this session") {
				return fmt.Errorf("the result says %q", got)
			}
			return nil
		}},
	)
	e.send(ctx, "Build the service.")
	out := e.turn(ctx)
	if out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	res, _, meta := e.resultOf("toolu_q")
	if res.Outcome != tools.OutcomeUnanswered || res.IsError || meta.ClosedBy != session.ClosedByUnattended || meta.EventID != "" {
		t.Fatalf("result %+v, meta %+v", res, meta)
	}
	var use session.AgentToolUse
	if err := e.events(ctx, session.TypeAgentToolUse)[0].Decode(&use); err != nil || use.Name != ToolQuestion || use.Verdict != string(VerdictAllow) || use.Risk == nil || use.Risk.Score != 0 {
		t.Fatalf("agent.tool_use %+v, %v", use, err)
	}
	for _, ev := range e.events(ctx, session.TypeSessionStatus) {
		var st session.SessionStatus
		if err := ev.Decode(&st); err != nil || st.StopReason == session.StopQuestion {
			t.Fatalf("the session went idle on a question nobody attends: %+v, %v", st, err)
		}
	}
	if c, ok := session.Closed(e.all(), "toolu_q"); !ok || !c.Settled || c.By != session.ClosedByUnattended {
		t.Fatalf("closing %+v, %v", c, ok)
	}
}

// TestAQuestionWaitsForItsAnswer: in an attended session a valid call
// scores 0.0 and is allowed in every mode, an always_confirm pattern
// that names it does not make it ask, the step's other calls run and
// their results are appended, the session goes idle question, and a
// claim with nothing that closed the call keeps it waiting and sends no
// request.
func TestAQuestionWaitsForItsAnswer(t *testing.T) {
	for _, mode := range []Mode{ModePlan, ModeConfirm, ModeProgressive} {
		t.Run(string(mode), func(t *testing.T) {
			e := asking(t, true, func(c *Config) {
				c.Policy = Policy{Mode: mode, AlwaysConfirm: []string{ToolQuestion, "question(*)", "bash(rm*)"}}
			})
			ctx := t.Context()
			e.stub.Script(model, reply(ir.StopToolUse, call("toolu_q", ToolQuestion, twoQuestions), call("toolu_e", "echo", `{"text":"beside"}`)))
			e.send(ctx, "Build the service.")
			out := e.turn(ctx)
			if out.Status != session.StatusIdle || out.StopReason != session.StopQuestion || out.Pending {
				t.Fatalf("outcome %+v", out)
			}
			var use session.AgentToolUse
			if err := e.events(ctx, session.TypeAgentToolUse)[0].Decode(&use); err != nil {
				t.Fatal(err)
			}
			if use.Name != ToolQuestion || use.Verdict != string(VerdictAllow) || use.Risk == nil || use.Risk.Score != 0 || use.Mode != string(mode) || !json.Valid(use.Input) {
				t.Fatalf("agent.tool_use %+v", use)
			}
			var in session.QuestionInput
			if err := json.Unmarshal(use.Input, &in); err != nil || len(in.Questions) != 2 || !in.Questions[0].Options[0].Recommended || len(in.Questions[0].Options[1].Preview) != 2 || !in.Questions[1].Multiple {
				t.Fatalf("the recorded input %+v, %v", in, err)
			}
			if got := e.echo.ran(); !slices.Equal(got, []string{"toolu_e"}) {
				t.Fatalf("the step's other call ran %v", got)
			}
			if got := e.outcomes(); len(got) != 1 || got["toolu_e"] != tools.OutcomeOK {
				t.Fatalf("results %v; the question keeps none while it waits", got)
			}
			q, open := session.OpenQuestion(e.all())
			if !open || q.ToolUseID != "toolu_q" || session.Awaiting(e.all())["toolu_q"] != session.AnswerQuestion {
				t.Fatalf("the open question %+v, %v", q, open)
			}
			if h, err := e.store.Get(ctx, e.s.ID); err != nil || h.Status != session.StatusIdle || h.StopReason != session.StopQuestion {
				t.Fatalf("header %+v, %v", h, err)
			}
			// A claim with nothing that closed the call.
			e.running(ctx)
			if out := e.turn(ctx); out.StopReason != session.StopQuestion || len(e.stub.Requests()) != 1 || len(e.outcomes()) != 1 {
				t.Fatalf("a claim with no answer: %+v, %d requests, results %v", out, len(e.stub.Requests()), e.outcomes())
			}
		})
	}

	// A decider that asks all the same is overruled.
	t.Run("a decider that asks", func(t *testing.T) {
		e := asking(t, true, func(c *Config) { c.Decider = asker{} })
		ctx := t.Context()
		e.stub.Script(model, reply(ir.StopToolUse, call("toolu_q", ToolQuestion, twoQuestions)))
		e.send(ctx, "Build the service.")
		if out := e.turn(ctx); out.StopReason != session.StopQuestion {
			t.Fatalf("outcome %+v", out)
		}
		var use session.AgentToolUse
		if err := e.events(ctx, session.TypeAgentToolUse)[0].Decode(&use); err != nil || use.Verdict != string(VerdictAllow) {
			t.Fatalf("agent.tool_use %+v, %v", use, err)
		}
	})

	// A block still blocks.
	t.Run("a decider that blocks", func(t *testing.T) {
		e := asking(t, true, func(c *Config) { c.Decider = blocker{} })
		ctx := t.Context()
		e.stub.Script(model, reply(ir.StopToolUse, call("toolu_q", ToolQuestion, twoQuestions)), reply(ir.StopEndTurn, text("ok")))
		e.send(ctx, "Build the service.")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("outcome %+v", out)
		}
		if res, _, _ := e.resultOf("toolu_q"); res.Outcome != tools.OutcomeBlocked {
			t.Fatalf("result %+v", res)
		}
		if _, open := session.OpenQuestion(e.all()); open {
			t.Fatal("a blocked question is open")
		}
	})
}

// asker and blocker are deciders that answer every call with one
// verdict.
type asker struct{}

func (asker) Decide(context.Context, Call) (session.Risk, Decision, error) {
	return session.Risk{Source: "test"}, Decision{Verdict: VerdictAsk, Reason: "always"}, nil
}

type blocker struct{}

func (blocker) Decide(context.Context, Call) (session.Risk, Decision, error) {
	return session.Risk{Source: "test"}, Decision{Verdict: VerdictBlock, Reason: "never"}, nil
}

// asked runs the first turn of an attended session to the open question.
func asked(t *testing.T, e *env, replies ...luxstub.Reply) {
	t.Helper()
	ctx := t.Context()
	e.stub.Script(model, append([]luxstub.Reply{reply(ir.StopToolUse, call("toolu_q", ToolQuestion, twoQuestions))}, replies...)...)
	e.send(ctx, "Build the service.")
	if out := e.turn(ctx); out.StopReason != session.StopQuestion {
		t.Fatalf("outcome %+v", out)
	}
}

// TestAnAnswerIsRenderedOnce: a user.answer is rendered into one
// tool.result with the outcome ok that holds each chosen label and the
// person's words and marks an empty entry as left to the agent, with the
// cause and the answer's id in its meta; an answer of empty entries is
// unanswered; a fresh harness renders the same bytes from the log; and a
// runner that stops between the answer and the result leaves one result.
func TestAnAnswerIsRenderedOnce(t *testing.T) {
	const rendered = "The person answered.\n" +
		"\n1. Storage: Which database should the new service keep its records in?\n   Chosen: SQLite\n   In their words: we have nobody to run a second schema" +
		"\n2. Regions: Which regions does the first release serve?\n   Left to you. Decide, and state what you assumed."
	chosen := []session.AnswerEntry{{Selected: []string{"SQLite"}, Text: "we have nobody to run a second schema"}, {}}

	t.Run("an answer", func(t *testing.T) {
		e := asking(t, true)
		ctx := t.Context()
		asked(t, e, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("SQLite it is; I assumed Europe.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			if len(last.Blocks) != 1 || last.Blocks[0].ToolResult == nil || last.Blocks[0].ToolResult.IsError || last.Blocks[0].ToolResult.Blocks[0].Text != rendered {
				return fmt.Errorf("the model read %+v", last)
			}
			return nil
		}})
		ans := e.answer(ctx, "toolu_q", chosen...)
		if !session.HasPendingInput(e.all()) {
			t.Fatal("an answer is not pending input")
		}
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the answer %+v", out)
		}
		res, _, meta := e.resultOf("toolu_q")
		if res.Outcome != tools.OutcomeOK || res.IsError || res.Content[0].Text != rendered || meta.ClosedBy != session.ClosedByAnswer || meta.EventID != ans.ID {
			t.Fatalf("result %+v, meta %+v", res, meta)
		}
		if strings.Contains(res.Content[0].Text, e.s.Initiator.Name) || strings.Contains(res.Content[0].Text, e.s.Initiator.Subject) {
			t.Fatalf("the result names the sender: %s", res.Content[0].Text)
		}
	})

	t.Run("every entry empty", func(t *testing.T) {
		e := asking(t, true)
		ctx := t.Context()
		asked(t, e, reply(ir.StopEndTurn, text("I assumed Postgres and Europe.")))
		ans := e.answer(ctx, "toolu_q", session.AnswerEntry{}, session.AnswerEntry{})
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the answer %+v", out)
		}
		res, _, meta := e.resultOf("toolu_q")
		if res.Outcome != tools.OutcomeUnanswered || res.IsError || !strings.Contains(res.Content[0].Text, "left every question to you") || meta.ClosedBy != session.ClosedByAnswer || meta.EventID != ans.ID {
			t.Fatalf("result %+v, meta %+v", res, meta)
		}
	})

	// Words alone, and several labels where the question takes several.
	t.Run("words and several labels", func(t *testing.T) {
		e := asking(t, true)
		ctx := t.Context()
		asked(t, e, reply(ir.StopEndTurn, text("Done.")))
		e.answer(ctx, "toolu_q", session.AnswerEntry{Text: "whatever the platform team runs"}, session.AnswerEntry{Selected: []string{"Europe", "North America"}})
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the answer %+v", out)
		}
		res, _, _ := e.resultOf("toolu_q")
		want := "The person answered.\n" +
			"\n1. Storage: Which database should the new service keep its records in?\n   In their words: whatever the platform team runs" +
			"\n2. Regions: Which regions does the first release serve?\n   Chosen: Europe, North America"
		if res.Outcome != tools.OutcomeOK || res.Content[0].Text != want {
			t.Fatalf("result %+v", res)
		}
	})

	// A runner that appended its claim and stopped before the result: the
	// next runner, a fresh harness over the same log, renders the result,
	// the same bytes, once. The call has no effect outside the log, so a
	// second claim after the answer closes nothing as unknown_effect.
	t.Run("a runner that stops before the result", func(t *testing.T) {
		e := asking(t, true)
		ctx := t.Context()
		asked(t, e, reply(ir.StopEndTurn, text("SQLite it is.")))
		e.answer(ctx, "toolu_q", chosen...)
		e.running(ctx)
		e.running(ctx)
		fresh, err := New(e.cfg)
		if err != nil {
			t.Fatal(err)
		}
		e.h = fresh
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the second claim %+v", out)
		}
		res, _, _ := e.resultOf("toolu_q")
		if res.Outcome != tools.OutcomeOK || res.Content[0].Text != rendered {
			t.Fatalf("result %+v", res)
		}
		// A third claim, on a message, renders nothing more.
		e.stub.Script(model, reply(ir.StopEndTurn, text("Yes.")))
		e.say(ctx, "Is that all?")
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the message %+v", out)
		}
		e.resultOf("toolu_q")
	})

	// An answer redacted before a runner read it still closes the call.
	t.Run("an answer redacted before it was read", func(t *testing.T) {
		e := asking(t, true)
		ctx := t.Context()
		asked(t, e, reply(ir.StopEndTurn, text("I assumed Postgres and Europe.")))
		ans := e.answer(ctx, "toolu_q", session.AnswerEntry{Text: "the root password is hunter2"}, session.AnswerEntry{})
		if err := e.store.Redact(ctx, e.s.ID, ans.ID, e.s.Initiator, "it held a password"); err != nil {
			t.Fatal(err)
		}
		e.log.last = 0
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the redacted answer %+v", out)
		}
		res, _, meta := e.resultOf("toolu_q")
		if res.Outcome != tools.OutcomeUnanswered || !strings.Contains(res.Content[0].Text, "the answer was removed") || meta.ClosedBy != session.ClosedByAnswer || meta.EventID != ans.ID {
			t.Fatalf("result %+v, meta %+v", res, meta)
		}
		for _, r := range e.stub.Requests() {
			if b, err := json.Marshal(r.Request); err != nil || strings.Contains(string(b), "hunter2") {
				t.Fatalf("a request holds the redacted words: %v", err)
			}
		}
	})
}

// TestAMessageStandsInForAnAnswer: a person's message appended after the
// question closes it unanswered, with the cause and the message's id in
// its meta, and the next request holds the result and then the message;
// a message appended before the question closes nothing; and a message
// that finds an ask waiting beside the question denies the ask too.
func TestAMessageStandsInForAnAnswer(t *testing.T) {
	t.Run("a message after the question", func(t *testing.T) {
		e := asking(t, true)
		ctx := t.Context()
		asked(t, e, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("SQLite, then.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			if len(last.Blocks) != 2 || last.Blocks[0].ToolResult == nil || last.Blocks[0].ToolResult.IsError ||
				!strings.Contains(last.Blocks[0].ToolResult.Blocks[0].Text, "it may be the answer") || last.Blocks[1].Text != "Keep it to one file, please." {
				return fmt.Errorf("the model did not read the result and then the message: %+v", last)
			}
			return nil
		}})
		msg := e.say(ctx, "Keep it to one file, please.")
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the message %+v", out)
		}
		res, _, meta := e.resultOf("toolu_q")
		if res.Outcome != tools.OutcomeUnanswered || res.IsError || meta.ClosedBy != session.ClosedByMessage || meta.EventID != msg.ID {
			t.Fatalf("result %+v, meta %+v", res, meta)
		}
	})

	// The step's other call appends a person's message while it runs,
	// after the question's agent.tool_use: the question is closed when
	// the step ends, and the session never goes idle on it.
	t.Run("a message while the step's other calls run", func(t *testing.T) {
		e := asking(t, true)
		ctx := t.Context()
		var msg session.Event
		e.echo.run = func(tools.Call) (tools.Result, error) {
			msg = e.alongside(context.WithoutCancel(ctx), session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Use SQLite."}}})
			return tools.Text(tools.OutcomeOK, "done"), nil
		}
		e.stub.Script(model,
			reply(ir.StopToolUse, call("toolu_q", ToolQuestion, twoQuestions), call("toolu_e", "echo", `{"text":"beside"}`)),
			reply(ir.StopEndTurn, text("SQLite it is.")),
		)
		e.send(ctx, "Build the service.")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("outcome %+v", out)
		}
		res, _, meta := e.resultOf("toolu_q")
		if res.Outcome != tools.OutcomeUnanswered || meta.ClosedBy != session.ClosedByMessage || meta.EventID != msg.ID {
			t.Fatalf("result %+v, meta %+v", res, meta)
		}
	})

	// The message that asked for the work is before the question, and so
	// is one sent while the model was still answering: neither closes it.
	t.Run("a message before the question", func(t *testing.T) {
		e := asking(t, true)
		ctx := t.Context()
		e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{call("toolu_q", ToolQuestion, twoQuestions)}, StopReason: ir.StopToolUse}, Respond: func(*ir.Request, *ir.Response) {
			e.alongside(context.WithoutCancel(ctx), session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "And keep it small."}}})
		}})
		e.send(ctx, "Build the service.")
		out := e.turn(ctx)
		if out.StopReason != session.StopQuestion {
			t.Fatalf("outcome %+v", out)
		}
		if _, open := session.OpenQuestion(e.all()); !open || len(e.outcomes()) != 0 {
			t.Fatalf("a message before the question closed it: results %v", e.outcomes())
		}
		// The model has not read that message, so the runner claims again,
		// and the claim keeps the question waiting.
		if !out.Pending {
			t.Fatal("a message the model has not read is not pending")
		}
	})

	t.Run("an ask beside the question", func(t *testing.T) {
		e := asking(t, true, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} })
		ctx := t.Context()
		e.stub.Script(model,
			reply(ir.StopToolUse, call("toolu_q", ToolQuestion, twoQuestions), call("toolu_w", "bash", `{"command":"make"}`)),
			luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("Stopping the build; SQLite, then.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
				last := r.Messages[len(r.Messages)-1]
				if len(last.Blocks) != 3 || last.Blocks[0].ToolResult == nil || last.Blocks[0].ToolResult.ToolUseID != "toolu_q" || last.Blocks[0].ToolResult.IsError ||
					last.Blocks[1].ToolResult == nil || !last.Blocks[1].ToolResult.IsError || last.Blocks[2].Text != "No build. Use SQLite." {
					return fmt.Errorf("the model did not read the unanswered question, the denial and then the message: %+v", last)
				}
				return nil
			}},
		)
		e.send(ctx, "Build the service.")
		if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
			t.Fatalf("outcome %+v; the stop reason names the confirmation before the question", out)
		}
		if got := session.Awaiting(e.all()); got["toolu_q"] != session.AnswerQuestion || got["toolu_w"] != session.AnswerConfirmation {
			t.Fatalf("awaiting %v", got)
		}
		e.say(ctx, "No build. Use SQLite.")
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the message %+v", out)
		}
		got := e.outcomes()
		if got["toolu_q"] != tools.OutcomeUnanswered || got["toolu_w"] != tools.OutcomeDenied || len(e.write.ran()) != 0 {
			t.Fatalf("outcomes %v, shell ran %v", got, e.write.ran())
		}
	})

	// A confirmed ask beside an open question runs in the claim that
	// reads its confirmation, and the session then waits on the question.
	t.Run("a confirmation beside the question", func(t *testing.T) {
		e := asking(t, true, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} })
		ctx := t.Context()
		e.stub.Script(model,
			reply(ir.StopToolUse, call("toolu_q", ToolQuestion, twoQuestions), call("toolu_w", "bash", `{"command":"make"}`)),
			reply(ir.StopEndTurn, text("Built, on SQLite.")),
		)
		e.send(ctx, "Build the service.")
		if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
			t.Fatalf("outcome %+v", out)
		}
		e.confirm(ctx, "toolu_w")
		if out := e.turn(ctx); out.StopReason != session.StopQuestion || !slices.Equal(e.write.ran(), []string{"toolu_w"}) || len(e.stub.Requests()) != 1 {
			t.Fatalf("after the confirmation %+v, shell ran %v, %d requests", out, e.write.ran(), len(e.stub.Requests()))
		}
		e.answer(ctx, "toolu_q", session.AnswerEntry{Selected: []string{"SQLite"}}, session.AnswerEntry{})
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the answer %+v", out)
		}
		if got := e.outcomes(); got["toolu_q"] != tools.OutcomeOK || got["toolu_w"] != tools.OutcomeOK || len(e.write.ran()) != 1 {
			t.Fatalf("outcomes %v, shell ran %v", got, e.write.ran())
		}
	})
}

// alongside appends an event as another writer does, a client beside the
// running turn, straight to the store.
func (e *env) alongside(ctx context.Context, typ session.Type, payload any) session.Event {
	e.t.Helper()
	ev, err := session.NewEvent(typ, payload, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	for range 8 {
		s, err := e.store.Get(ctx, e.s.ID)
		if err != nil {
			e.t.Fatal(err)
		}
		batch := []session.Event{ev}
		session.Stamp(e.s.ID, s.LastSeq, batch)
		_, err = e.store.Append(ctx, e.s.ID, s.LastSeq, batch)
		if err == nil {
			return batch[0]
		}
		if !errors.Is(err, session.ErrSequenceConflict) {
			e.t.Fatal(err)
		}
	}
	e.t.Fatal("the append beside the turn never landed")
	return session.Event{}
}

// TestAnInterruptDismissesAQuestion: an interrupt in the step that holds
// the call closes it canceled and ends the turn interrupted; an
// interrupt on a session idle on the question is claimed, the call is
// closed canceled, the session goes idle interrupted, and no request is
// sent; the person's next message continues from there.
func TestAnInterruptDismissesAQuestion(t *testing.T) {
	t.Run("in the step that holds the call", func(t *testing.T) {
		e := asking(t, true)
		ctx := t.Context()
		var in session.Event
		e.echo.run = func(tools.Call) (tools.Result, error) {
			in = e.alongside(context.WithoutCancel(ctx), session.TypeUserInterrupt, session.UserInterrupt{Sender: e.s.Initiator})
			return tools.Text(tools.OutcomeOK, "done"), nil
		}
		e.stub.Script(model, reply(ir.StopToolUse, call("toolu_q", ToolQuestion, twoQuestions), call("toolu_e", "echo", `{"text":"beside"}`)), reply(ir.StopEndTurn, text("never")))
		e.send(ctx, "Build the service.")
		out := e.turn(ctx)
		if out.StopReason != session.StopInterrupted || out.Pending || len(e.stub.Requests()) != 1 {
			t.Fatalf("outcome %+v, %d requests", out, len(e.stub.Requests()))
		}
		res, _, meta := e.resultOf("toolu_q")
		if res.Outcome != tools.OutcomeCanceled || !res.IsError || meta.ClosedBy != session.ClosedByInterrupt || meta.EventID != in.ID {
			t.Fatalf("result %+v, meta %+v", res, meta)
		}
		if session.HasPendingInput(e.all()) {
			t.Fatal("a dismissed question whose result is in the log is pending input")
		}
	})

	t.Run("on a session idle on the question", func(t *testing.T) {
		e := asking(t, true)
		ctx := t.Context()
		asked(t, e, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("Understood, a file it is.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			var texts []string
			for _, b := range last.Blocks {
				if b.ToolResult != nil {
					texts = append(texts, b.ToolResult.Blocks[0].Text)
				} else {
					texts = append(texts, b.Text)
				}
			}
			if len(texts) != 3 || !strings.Contains(texts[0], "stopped the work before answering") || !strings.Contains(texts[1], "interrupted the previous turn") || texts[2] != "Just use a file." {
				return fmt.Errorf("the model read %q", texts)
			}
			return nil
		}})
		in := e.interrupt(ctx)
		if !session.HasPendingInput(e.all()) {
			t.Fatal("an interrupt on an open question is not pending input")
		}
		e.running(ctx)
		out := e.turn(ctx)
		if out.StopReason != session.StopInterrupted || out.Pending || len(e.stub.Requests()) != 1 {
			t.Fatalf("outcome %+v, %d requests", out, len(e.stub.Requests()))
		}
		res, _, meta := e.resultOf("toolu_q")
		if res.Outcome != tools.OutcomeCanceled || meta.ClosedBy != session.ClosedByInterrupt || meta.EventID != in.ID {
			t.Fatalf("result %+v, meta %+v", res, meta)
		}
		if h, err := e.store.Get(ctx, e.s.ID); err != nil || h.Status != session.StatusIdle || h.StopReason != session.StopInterrupted {
			t.Fatalf("header %+v, %v", h, err)
		}
		if session.HasPendingInput(e.all()) {
			t.Fatal("the dismissed question is still pending input")
		}
		// Nothing ran; the person's next message continues the session.
		e.say(ctx, "Just use a file.")
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("after the message %+v", out)
		}
		e.resultOf("toolu_q")
	})

	// An interrupt the turn learns of only as it goes idle: the turn
	// reports input pending, so the runner claims again at once.
	t.Run("an interrupt the turn sees as it goes idle", func(t *testing.T) {
		e := asking(t, true)
		ctx := t.Context()
		e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{call("toolu_q", ToolQuestion, twoQuestions)}, StopReason: ir.StopToolUse}})
		e.send(ctx, "Build the service.")
		s, err := e.store.Get(ctx, e.s.ID)
		if err != nil {
			t.Fatal(err)
		}
		late := &interrupting{storeLog: e.log, e: e}
		out, err := e.h.RunTurn(ctx, s, e.all(), late)
		if err != nil {
			t.Fatal(err)
		}
		if out.StopReason != session.StopQuestion || !out.Pending {
			t.Fatalf("outcome %+v; the interrupt closed the question, so the runner must claim again", out)
		}
		e.running(ctx)
		if out := e.turn(ctx); out.StopReason != session.StopInterrupted || out.Pending {
			t.Fatalf("the next claim %+v", out)
		}
		if res, _, meta := e.resultOf("toolu_q"); res.Outcome != tools.OutcomeCanceled || meta.EventID != late.id {
			t.Fatalf("result %+v, meta %+v", res, meta)
		}
	})
}

// interrupting is a log that appends a person's interrupt straight
// before the turn's closing status, as a client racing the turn's end
// does.
type interrupting struct {
	*storeLog
	e  *env
	id string
}

func (l *interrupting) Append(ctx context.Context, batch []session.Event) ([]session.Event, error) {
	if l.id == "" && len(batch) == 1 && batch[0].Type == session.TypeSessionStatus {
		l.id = l.e.alongside(ctx, session.TypeUserInterrupt, session.UserInterrupt{Sender: l.e.s.Initiator}).ID
	}
	return l.storeLog.Append(ctx, batch)
}

// TestAForkAcrossAnOpenQuestion: a fork made while a question is open
// copies the open call, and the fork's first message closes it as a
// message closes any, in a fork that is attended or not.
func TestAForkAcrossAnOpenQuestion(t *testing.T) {
	for _, attended := range []bool{true, false} {
		t.Run(fmt.Sprintf("attended %v", attended), func(t *testing.T) {
			e := asking(t, true)
			ctx := t.Context()
			asked(t, e)
			parent := e.all()
			at, err := session.ForkPoint(parent, nil)
			if err != nil || at != parent[len(parent)-1].Seq {
				t.Fatalf("the fork point %d, %v; the idle question status is a turn boundary", at, err)
			}
			child := session.New(e.s.Agent, e.s.Initiator, session.RunnerHosted, e.s.Machine, t0)
			child.Attended = attended
			forked, err := session.Fork(ctx, e.store, child, nil, e.s.ID, parent[:at])
			if err != nil {
				t.Fatal(err)
			}
			if forked.Status != session.StatusIdle || forked.StopReason != session.StopQuestion || forked.Attended != attended {
				t.Fatalf("the fork's header %+v", forked)
			}
			// The fork is driven as its own session from here.
			e.s, e.log = forked, &storeLog{st: e.store, id: forked.ID}
			if q, open := session.OpenQuestion(e.all()); !open || q.ToolUseID != "toolu_q" {
				t.Fatalf("the fork's open question %+v, %v", q, open)
			}
			e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("A file, then.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
				last := r.Messages[len(r.Messages)-1]
				if len(last.Blocks) != 2 || last.Blocks[0].ToolResult == nil || !strings.Contains(last.Blocks[0].ToolResult.Blocks[0].Text, "it may be the answer") || last.Blocks[1].Text != "Use a file." {
					return fmt.Errorf("the fork's model read %+v", last)
				}
				return nil
			}})
			msg := e.say(ctx, "Use a file.")
			e.running(ctx)
			if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
				t.Fatalf("the fork's turn %+v", out)
			}
			res, _, meta := e.resultOf("toolu_q")
			if res.Outcome != tools.OutcomeUnanswered || meta.ClosedBy != session.ClosedByMessage || meta.EventID != msg.ID {
				t.Fatalf("result %+v, meta %+v", res, meta)
			}
		})
	}

	// A fork that is not attended and is claimed with the call open, as a
	// runner that stopped before the result leaves one, answers at once.
	t.Run("an open call of a session nobody attends", func(t *testing.T) {
		e := asking(t, false)
		ctx := t.Context()
		msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_q", Name: ToolQuestion, Args: json.RawMessage(twoQuestions)}}}}, StopReason: ir.StopToolUse}, t0)
		if err != nil {
			t.Fatal(err)
		}
		use, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_q", Name: ToolQuestion, Input: json.RawMessage(twoQuestions), Verdict: "allow"}, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.send(ctx, "Build the service.")
		e.appendEvents(ctx, msg, use)
		e.running(ctx)
		e.stub.Script(model, reply(ir.StopEndTurn, text("I assumed Postgres.")))
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("outcome %+v", out)
		}
		if res, _, meta := e.resultOf("toolu_q"); res.Outcome != tools.OutcomeUnanswered || meta.ClosedBy != session.ClosedByUnattended {
			t.Fatalf("result %+v, meta %+v", res, meta)
		}
	})
}

// TestQuestionResultsAreNeverCleared: a question's result stays in the
// context when older results are cleared, since the cleared text tells
// the model to run the tool again, which would ask the person twice.
func TestQuestionResultsAreNeverCleared(t *testing.T) {
	e := setup(t, window(8000))
	ctx := t.Context()
	e.send(ctx, "Plan.")
	id := "toolu_q"
	msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: id, Name: ToolQuestion, Args: json.RawMessage(twoQuestions)}}}}, StopReason: ir.StopToolUse}, t0)
	if err != nil {
		t.Fatal(err)
	}
	use, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: id, Name: ToolQuestion, Input: json.RawMessage(twoQuestions), Verdict: "allow"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := session.NewEvent(session.TypeToolResult, session.ToolResult{ToolUseID: id, Content: []lux.Block{{Type: ir.BlockText, Text: "The person answered.\n" + strings.Repeat("a", 12000)}}, Outcome: tools.OutcomeOK}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, msg, use, res)
	e.history(ctx, 12000, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40)
	if _, err := e.manage(ctx); err != nil {
		var stop *errStop
		if !errors.As(err, &stop) {
			t.Fatal(err)
		}
	}
	cleared := 0
	for _, c := range e.compactions(ctx) {
		if c.Kind != session.CompactClearToolResults {
			continue
		}
		cleared += len(c.ToolUseIDs)
		if slices.Contains(c.ToolUseIDs, id) {
			t.Fatal("a question's result was cleared")
		}
	}
	if cleared == 0 {
		t.Fatal("nothing was cleared, so the test proves nothing")
	}
}
