// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// logLines is Config.Log's output, one JSON object a line.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// lines decodes every line written so far.
func (l *logLines) lines(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for line := range strings.Lines(l.buf.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// actions are the action of each line about a call written as text.
func (l *logLines) actions(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, m := range l.lines(t) {
		if m["msg"] == "a response wrote a tool call as text" {
			out = append(out, fmt.Sprint(m["action"]))
		}
	}
	return out
}

// textEnv is a session a person attends whose agent is offered the
// question tool and whose Config.Log is recorded; with a router, the
// session runs on via, and the router answers its failover questions.
func textEnv(t *testing.T, r *router, muts ...func(*Config)) (*env, *logLines) {
	t.Helper()
	logs := &logLines{}
	e := setupSession(t, func(c *Config) {
		c.Question = true
		c.Log = slog.New(slog.NewJSONHandler(logs, nil))
		if r != nil {
			c.Failover = r.failover
			c.Connect = func(_ context.Context, name string) (models.Model, models.Connection, models.Entry, error) {
				return &dialect.Model{}, models.Connection{BaseURL: c.Connection.BaseURL, Model: name, Family: models.FamilyAnthropic},
					models.Entry{Name: name, InputWindow: 30_000, MaxOutputTokens: 2_000}, nil
			}
		}
		for _, m := range muts {
			m(c)
		}
	}, func(s *session.Session) {
		s.Attended = true
		if r != nil {
			s.Model = &session.ModelRef{Name: model, Via: via}
		}
	})
	return e, logs
}

// written is a response of the model name that ends its turn with text
// alone.
func written(name, text string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: name, Blocks: []ir.Block{{Type: ir.BlockText, Text: text}}, StopReason: ir.StopEndTurn, Usage: ir.Usage{InputTokens: 100, OutputTokens: 10}}}
}

// questionCall is a response of the model name that asks the person
// through the question tool.
func questionCall(name string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: name, Blocks: []ir.Block{text("Two things first."), call("toolu_q", ToolQuestion, twoQuestions)}, StopReason: ir.StopToolUse, Usage: ir.Usage{InputTokens: 100, OutputTokens: 10}}}
}

// endsWith checks that a request's last message is the person's and ends
// with text, or with anything but text when text is empty.
func endsWith(req *ir.Request, text string) error {
	last := req.Messages[len(req.Messages)-1]
	b := last.Blocks[len(last.Blocks)-1]
	switch {
	case text == "" && b.Type == ir.BlockText && strings.HasPrefix(b.Text, "Your last answer was not shown"):
		return errors.New("the request carries a reminder")
	case text != "" && (last.Role != ir.RoleUser || b.Type != ir.BlockText || b.Text != text):
		return fmt.Errorf("the request ends with %+v, not the reminder", b)
	}
	return nil
}

func reminded(tool string) string {
	return prompts.Render(prompts.ReminderToolAsText, prompts.Data{"Tool": tool})
}

// messages are the agent.message payloads of the log, with their events.
func (e *env) messages(ctx context.Context) ([]session.AgentMessage, []session.Event) {
	e.t.Helper()
	evs := e.events(ctx, session.TypeAgentMessage)
	out := make([]session.AgentMessage, len(evs))
	for i, ev := range evs {
		if err := ev.Decode(&out[i]); err != nil {
			e.t.Fatal(err)
		}
	}
	return out, evs
}

// blob is a blob of the session as text.
func (e *env) blob(ctx context.Context, d session.Digest) string {
	e.t.Helper()
	rc, err := e.log.Blob(ctx, d)
	if err != nil {
		e.t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// noWords fails when a log line holds a word of the response.
func noWords(t *testing.T, logs *logLines, words ...string) {
	t.Helper()
	logs.mu.Lock()
	defer logs.mu.Unlock()
	for _, w := range words {
		if strings.Contains(logs.buf.String(), w) {
			t.Fatalf("the log holds %q:\n%s", w, logs.buf.String())
		}
	}
}

// TestAQuestionWrittenAsTextIsAskedAgainAsACall: an answer that writes the
// question tool's call as nested markup instead of calling it is not
// kept. Its request is recorded tool_as_text with the response in its
// blob and no agent.message, the Observer discards the step's output, and
// the same request goes again with the reminder after its fold; the
// model's call there is asked of the person. Both requests are spent, and
// the log says what happened in one line that holds none of the answer's
// words.
func TestAQuestionWrittenAsTextIsAskedAgainAsACall(t *testing.T) {
	rec := &recorder{}
	e, logs := textEnv(t, nil, func(c *Config) { c.Observer = rec })
	ctx := t.Context()
	again := questionCall(model)
	again.Expect = func(req *ir.Request) error { return endsWith(req, reminded(ToolQuestion)) }
	e.stub.Script(model, written(model, questionAsMarkup), again)
	e.send(ctx, "I want to build a to-do app. Can you help me create it?")
	if out := e.turn(ctx); out.StopReason != session.StopQuestion {
		t.Fatalf("outcome %+v, want the question asked", out)
	}
	reqs := e.stub.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	first, second := reqs[0].Request.Messages, reqs[1].Request.Messages
	n := len(first) - 1
	if len(second) != len(first) || !reflect.DeepEqual(first[:n], second[:n]) || !reflect.DeepEqual(first[n].Blocks, second[n].Blocks[:len(second[n].Blocks)-1]) {
		t.Fatalf("the request sent again is not the first with the reminder after it:\n%+v\n%+v", first, second)
	}
	mrs := e.requests(ctx)
	if len(mrs) != 2 || mrs[0].Outcome != session.OutcomeToolAsText || mrs[0].Reminder != nil || mrs[0].StopReason != ir.StopEndTurn ||
		mrs[1].Outcome != "ok" || mrs[1].Reminder == nil || *mrs[1].Reminder != (session.Reminder{Prompt: "reminders/tool-as-text-v1", Tool: ToolQuestion}) ||
		mrs[0].RequestSHA256 == mrs[1].RequestSHA256 {
		t.Fatalf("model.requests %+v", mrs)
	}
	if body := e.blob(ctx, mrs[0].ResponseBlob); !strings.Contains(body, "Tech Stack") {
		t.Fatalf("the response written as text is not kept in its blob: %s", body)
	}
	msgs, msgEvents := e.messages(ctx)
	reqEvents := e.events(ctx, session.TypeModelRequest)
	if len(msgs) != 1 || msgs[0].Request != reqEvents[1].ID {
		t.Fatalf("agent.messages %+v", msgs)
	}
	for _, b := range msgs[0].Message.Blocks {
		if b.Type == ir.BlockText && strings.Contains(b.Text, "<question>") {
			t.Fatalf("the markup reached an agent.message: %+v", msgEvents)
		}
	}
	if uses := e.uses(ctx); len(uses) != 1 || uses["toolu_q"].Name != ToolQuestion {
		t.Fatalf("agent.tool_use %+v", uses)
	}
	if rec.resets != 1 {
		t.Fatalf("resets %d, want one for the response written as text", rec.resets)
	}
	if got := session.Spent(e.all()); got != 2*150 {
		t.Fatalf("spent %d, want both requests' cost", got)
	}
	lines := logs.lines(t)
	if len(lines) != 1 || lines[0]["level"] != "INFO" || lines[0]["action"] != actionReminded || lines[0]["tool"] != ToolQuestion ||
		lines[0]["model"] != model || lines[0]["session"] != e.s.ID || lines[0]["turn"] != float64(1) || lines[0]["step"] != float64(1) || lines[0]["thread"] != "" {
		t.Fatalf("log %+v", lines)
	}
	noWords(t, logs, "Tech Stack", "to-do", "React")
}

// TestAPlanWrittenAsMarkupIsTheAnswerUnlessPlanIsATool: a plan element is
// markup named after a tool only where a tool is named plan. A chat agent
// is offered none, plan being an approval mode and not a tool, so its
// answer stands as written; an agent offered a tool named plan is reminded
// to call it.
func TestAPlanWrittenAsMarkupIsTheAnswerUnlessPlanIsATool(t *testing.T) {
	const plan = "Here is the plan.\n<plan>\n1. Model the data.\n2. Build the list view.\n</plan>"
	t.Run("no tool is named plan", func(t *testing.T) {
		e, logs := textEnv(t, nil)
		ctx := t.Context()
		e.stub.Script(model, written(model, plan))
		e.send(ctx, "Plan it.")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("outcome %+v", out)
		}
		msgs, _ := e.messages(ctx)
		if len(e.stub.Requests()) != 1 || len(msgs) != 1 || msgs[0].Message.Blocks[0].Text != plan {
			t.Fatalf("%d requests, messages %+v", len(e.stub.Requests()), msgs)
		}
		if a := logs.actions(t); len(a) != 0 {
			t.Fatalf("log %v", a)
		}
	})
	t.Run("a tool is named plan", func(t *testing.T) {
		planner := &fakeTool{name: "plan", props: tools.Properties{Parallel: true, Effect: tools.EffectNone}}
		e, logs := textEnv(t, nil, func(c *Config) {
			if err := c.Tools.AddBuiltin(planner); err != nil {
				t.Fatal(err)
			}
		})
		ctx := t.Context()
		again := reply(ir.StopToolUse, call("toolu_p", "plan", `{"text":"Model the data, then build the list view."}`))
		again.Expect = func(req *ir.Request) error { return endsWith(req, reminded("plan")) }
		e.stub.Script(model, written(model, plan), again, reply(ir.StopEndTurn, text("Planned.")))
		e.send(ctx, "Plan it.")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("outcome %+v", out)
		}
		if len(e.stub.Requests()) != 3 || len(planner.ran()) != 1 {
			t.Fatalf("%d requests, the tool ran %d times", len(e.stub.Requests()), len(planner.ran()))
		}
		if a := logs.actions(t); !slices.Equal(a, []string{actionReminded}) {
			t.Fatalf("log %v", a)
		}
	})
}

// TestProseAndCodeThatNameAToolAreTheAnswer: an answer that names a tool
// in prose, or shows the tool's markup in a code block or a code span, is
// the answer as written, with one request and nothing logged.
func TestProseAndCodeThatNameAToolAreTheAnswer(t *testing.T) {
	for name, answer := range map[string]string{
		"prose":      "I can ask you a question with the question tool, or run bash to scaffold it. Which do you prefer?",
		"code fence": "The question tool takes input like this:\n\n```xml\n<question>\n  <header>Stack</header>\n</question>\n```\n\nI will call it once you say go.",
		"code span":  "A call written as `<question>Which?</question>` would not run, so I ask in prose: which stack?",
	} {
		t.Run(name, func(t *testing.T) {
			e, logs := textEnv(t, nil)
			ctx := t.Context()
			e.stub.Script(model, written(model, answer))
			e.send(ctx, "Help me build it.")
			if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
				t.Fatalf("outcome %+v", out)
			}
			msgs, _ := e.messages(ctx)
			mrs := e.requests(ctx)
			if len(e.stub.Requests()) != 1 || len(msgs) != 1 || msgs[0].Message.Blocks[0].Text != answer || mrs[0].Outcome != "ok" || mrs[0].Reminder != nil {
				t.Fatalf("%d requests, messages %+v, model.requests %+v", len(e.stub.Requests()), msgs, mrs)
			}
			if a := logs.actions(t); len(a) != 0 {
				t.Fatalf("log %v", a)
			}
		})
	}
}

// TestAModelThatKeepsWritingCallsAsTextIsRemindedOnce: a model that
// writes the call as text again after the reminder, on a session that
// cannot move, is not asked a third time: the second response is the
// step's answer, as an answer was before the rule, and the turn ends.
func TestAModelThatKeepsWritingCallsAsTextIsRemindedOnce(t *testing.T) {
	e, logs := textEnv(t, nil)
	ctx := t.Context()
	e.stub.Script(model, written(model, questionAsMarkup), written(model, questionAsMarkup))
	e.send(ctx, "Help me build it.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if n := len(e.stub.Requests()); n != 2 {
		t.Fatalf("%d requests, want the first and one reminded", n)
	}
	mrs := e.requests(ctx)
	reqEvents := e.events(ctx, session.TypeModelRequest)
	msgs, _ := e.messages(ctx)
	if len(mrs) != 2 || mrs[0].Outcome != session.OutcomeToolAsText || mrs[1].Outcome != "ok" || mrs[1].Reminder == nil ||
		len(msgs) != 1 || msgs[0].Request != reqEvents[1].ID || msgs[0].Message.Blocks[0].Text != questionAsMarkup {
		t.Fatalf("model.requests %+v, messages %+v", mrs, msgs)
	}
	if ch := e.changes(ctx); len(ch) != 0 {
		t.Fatalf("changes %+v", ch)
	}
	lines := logs.lines(t)
	if a := logs.actions(t); !slices.Equal(a, []string{actionReminded, actionKept}) || lines[1]["level"] != "WARN" || lines[1]["why"] != "the session runs a model named itself" {
		t.Fatalf("log %+v", lines)
	}
}

// TestARoutedTurnMovesOffAModelThatWritesCallsAsText: on a routed
// session, a model that writes the call as text again after the reminder
// is passed over: the router is asked with failed_reason tool_as_text and
// the tool in the detail, the reminded request and the service's change
// with reason tool_as_text are recorded in one batch, and the step is sent
// on the model named, with no reminder, whose call is asked of the person.
// The next turn stays on that model.
func TestARoutedTurnMovesOffAModelThatWritesCallsAsText(t *testing.T) {
	const other = "other-model"
	r := &router{next: []session.ModelRef{{Name: other, Via: via, Effort: "low"}}}
	e, logs := textEnv(t, r)
	ctx := t.Context()
	again := written(model, questionAsMarkup)
	again.Expect = func(req *ir.Request) error { return endsWith(req, reminded(ToolQuestion)) }
	moved := questionCall(other)
	moved.Expect = func(req *ir.Request) error { return endsWith(req, "") }
	e.stub.Script(model, written(model, questionAsMarkup), again)
	e.stub.Script(other, moved)
	e.send(ctx, "Help me build it.")
	if out := e.turn(ctx); out.StopReason != session.StopQuestion {
		t.Fatalf("outcome %+v", out)
	}
	if len(r.asked) != 1 || r.asked[0] != (session.ModelRef{Name: model, Via: via}) || r.standing[0] != r.asked[0] ||
		r.reasons[0] != FailedToolAsText || !strings.Contains(r.details[0], "question") {
		t.Fatalf("the router was asked %+v standing on %+v for %q with %q", r.asked, r.standing, r.reasons, r.details)
	}
	mrs := e.requests(ctx)
	if len(mrs) != 3 || mrs[0].Model != model || mrs[0].Outcome != session.OutcomeToolAsText ||
		mrs[1].Model != model || mrs[1].Outcome != session.OutcomeToolAsText || mrs[1].Reminder == nil ||
		mrs[2].Model != other || mrs[2].Outcome != "ok" || mrs[2].Reminder != nil {
		t.Fatalf("model.requests %+v", mrs)
	}
	ch := e.changes(ctx)
	if len(ch) != 1 || ch[0].Reason != session.ReasonToolAsText || ch[0].By.Kind != session.SenderService ||
		ch[0].Old.Name != model || ch[0].New.Name != other || ch[0].New.Via != via || !strings.Contains(ch[0].Detail, "question") {
		t.Fatalf("session.model_changed %+v", ch)
	}
	evs := e.all()
	at := slices.IndexFunc(evs, func(ev session.Event) bool { return ev.Type == session.TypeModelChanged })
	if evs[at-1].Type != session.TypeModelRequest || evs[at+1].Type != session.TypeModelRequest {
		t.Fatalf("the change is not between the reminded request and the one that answers: %v %v", evs[at-1].Type, evs[at+1].Type)
	}
	lines := logs.lines(t)
	if a := logs.actions(t); !slices.Equal(a, []string{actionReminded, actionMoved}) || lines[1]["to"] != other || lines[1]["model"] != model {
		t.Fatalf("log %+v", lines)
	}
	s, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.Model == nil || s.Model.Name != other {
		t.Fatalf("the session stands on %+v", s.Model)
	}
}

// TestATurnMovesOffWrittenCallsOnce: a turn moves for calls written as
// text once. The model moved to is reminded once too, and when it writes
// the call as text again its response is the answer: four requests, one
// question, one change, and no loop.
func TestATurnMovesOffWrittenCallsOnce(t *testing.T) {
	const other, third = "other-model", "third-model"
	r := &router{next: []session.ModelRef{{Name: other, Via: via}, {Name: third, Via: via}}}
	e, logs := textEnv(t, r)
	ctx := t.Context()
	e.stub.Script(model, written(model, questionAsMarkup), written(model, questionAsMarkup))
	e.stub.Script(other, written(other, questionAsMarkup), written(other, questionAsMarkup))
	e.send(ctx, "Help me build it.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if n := len(e.stub.Requests()); n != 4 || len(r.asked) != 1 {
		t.Fatalf("%d requests, %d questions to the router", n, len(r.asked))
	}
	mrs := e.requests(ctx)
	got := make([]string, len(mrs))
	for i, mr := range mrs {
		got[i] = mr.Model + ":" + mr.Outcome
	}
	if want := []string{model + ":tool_as_text", model + ":tool_as_text", other + ":tool_as_text", other + ":ok"}; !slices.Equal(got, want) {
		t.Fatalf("model.requests %v, want %v", got, want)
	}
	if ch := e.changes(ctx); len(ch) != 1 {
		t.Fatalf("changes %+v", ch)
	}
	lines := logs.lines(t)
	if a := logs.actions(t); !slices.Equal(a, []string{actionReminded, actionMoved, actionReminded, actionKept}) || lines[3]["why"] != "the turn moved off a model for this once already" || lines[3]["model"] != other {
		t.Fatalf("log %+v", lines)
	}
}

// TestARouterThatRefusesTheReasonKeepsTheAnswer: an authorizer that does
// not know failed_reason tool_as_text refuses the question, and the turn
// keeps the reminded response as its answer, with no change and the
// refusal in the log line.
func TestARouterThatRefusesTheReasonKeepsTheAnswer(t *testing.T) {
	r := &router{err: errors.New("forbidden: invalid_resource")}
	e, logs := textEnv(t, r)
	ctx := t.Context()
	e.stub.Script(model, written(model, questionAsMarkup), written(model, questionAsMarkup))
	e.send(ctx, "Help me build it.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	mrs := e.requests(ctx)
	if len(r.asked) != 1 || len(mrs) != 2 || mrs[1].Outcome != "ok" || len(e.changes(ctx)) != 0 {
		t.Fatalf("router asked %d times, model.requests %+v", len(r.asked), mrs)
	}
	lines := logs.lines(t)
	if a := logs.actions(t); !slices.Equal(a, []string{actionReminded, actionKept}) || lines[1]["why"] != "no other model was named: forbidden: invalid_resource" {
		t.Fatalf("log %+v", lines)
	}
}

// TestAReminderIsReplayed: a replay builds the reminded request again with
// the reminder its model.request names, the one escalated at the output
// limit among them, to the bytes whose hash the session recorded; the
// same log with the reminder taken off its records does not match.
func TestAReminderIsReplayed(t *testing.T) {
	e, _ := textEnv(t, nil)
	ctx := t.Context()
	e.stub.Script(model, written(model, questionAsMarkup), reply(ir.StopMaxTokens, text("Half of it")), questionCall(model))
	e.send(ctx, "Help me build it.")
	if out := e.turn(ctx); out.StopReason != session.StopQuestion {
		t.Fatalf("outcome %+v", out)
	}
	mrs := e.requests(ctx)
	if len(mrs) != 3 || mrs[0].Outcome != session.OutcomeToolAsText || mrs[1].Outcome != "escalated" || mrs[1].Reminder == nil || mrs[2].Outcome != "ok" || mrs[2].Reminder == nil {
		t.Fatalf("model.requests %+v", mrs)
	}
	for _, rec := range e.stub.Requests()[1:] {
		if err := endsWith(rec.Request, reminded(ToolQuestion)); err != nil {
			t.Fatal(err)
		}
	}
	log := e.all()
	steps := e.replay(ctx, e.h, log)
	if len(steps) != 3 || slices.ContainsFunc(steps, func(s models.ReplayStep) bool { return s.Outcome != models.ReplayMatch }) {
		t.Fatalf("replay %+v", steps)
	}
	var stripped []session.Event
	for _, ev := range log {
		var p session.ModelRequest
		if ev.Type == session.TypeModelRequest && ev.Decode(&p) == nil && p.Reminder != nil {
			p.Reminder = nil
			b, err := session.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			ev.Payload = b
		}
		stripped = append(stripped, ev)
	}
	if steps := e.replay(ctx, e.h, stripped); steps[1].Outcome != models.ReplayMismatch || steps[2].Outcome != models.ReplayMismatch {
		t.Fatalf("a replay without the reminder %+v", steps)
	}
	if _, err := reminderText(session.Reminder{Prompt: "reminders/unknown-v9", Tool: "question"}); !errors.Is(err, models.ErrNotRebuilt) {
		t.Fatalf("an unknown reminder renders: %v", err)
	}
}

// TestAThreadIsRemindedAndNeverMoves: a subagent's thread that writes a
// call of a tool it is offered as text is reminded as the session's own
// thread is, and when it writes it again its response is its answer: a
// thread's turn asks no router, on a routed session too.
func TestAThreadIsRemindedAndNeverMoves(t *testing.T) {
	r := &router{next: []session.ModelRef{{Name: "other-model", Via: via}}}
	e, logs := textEnv(t, r, withReviewer(nil))
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Review main.go."}`)),
		reply(ir.StopEndTurn, text("Reviewed.")),
	)
	const echoed = "Checking it.\n<echo>main.go</echo>"
	again := written(reviewerModel, echoed)
	again.Expect = func(req *ir.Request) error { return endsWith(req, reminded("echo")) }
	e.stub.Script(reviewerModel, written(reviewerModel, echoed), again)
	e.send(ctx, "Review it.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if len(r.asked) != 0 {
		t.Fatalf("a thread asked the router %+v", r.asked)
	}
	var outcomes []string
	for _, ev := range e.events(ctx, session.TypeModelRequest) {
		var p session.ModelRequest
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if ev.Thread != "" {
			outcomes = append(outcomes, p.Outcome)
		}
	}
	if !slices.Equal(outcomes, []string{session.OutcomeToolAsText, "ok"}) {
		t.Fatalf("the thread's model.requests %v", outcomes)
	}
	lines := logs.lines(t)
	if a := logs.actions(t); !slices.Equal(a, []string{actionReminded, actionKept}) || lines[1]["why"] != "a thread's turn does not move" || lines[1]["thread"] == "" || lines[1]["tool"] != "echo" {
		t.Fatalf("log %+v", lines)
	}
}

// TestAContinuationIsNotReminded: a response that continues one cut at
// the output limit is kept as it is, since the part before it was kept
// and the reminder asks for the answer again in full.
func TestAContinuationIsNotReminded(t *testing.T) {
	const limit = OutputCap / 2
	e, logs := textEnv(t, nil, func(c *Config) { c.Entry.MaxOutputTokens = limit })
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopMaxTokens, text("The first half")), written(model, questionAsMarkup))
	e.send(ctx, "Answer at length.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	msgs, _ := e.messages(ctx)
	if len(e.stub.Requests()) != 2 || len(msgs) != 2 || msgs[1].ContinuationOf == "" || msgs[1].Message.Blocks[0].Text != questionAsMarkup {
		t.Fatalf("%d requests, messages %+v", len(e.stub.Requests()), msgs)
	}
	if a := logs.actions(t); len(a) != 0 {
		t.Fatalf("log %v", a)
	}
}
