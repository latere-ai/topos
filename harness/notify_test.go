// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// fire appends a trigger's message, judged or always, and the running
// status, as a firing and the runner do before a turn.
func (e *env) fire(ctx context.Context, judged bool) {
	e.t.Helper()
	msg := session.UserMessage{Sender: session.Sender{Subject: "trigger:trg_1", Kind: session.SenderTrigger}, Content: []lux.Block{{Type: ir.BlockText, Text: "Look at the fare."}}, FiringID: "frg_1"}
	if judged {
		msg.Notify = session.NotifyJudged
	}
	ev, err := session.NewEvent(session.TypeUserMessage, msg, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.appendEvents(ctx, ev)
	e.running(ctx)
}

// lastOutcome is the outcome of the session's last status.
func (e *env) lastOutcome(ctx context.Context) string {
	e.t.Helper()
	sts := e.events(ctx, session.TypeSessionStatus)
	var p session.SessionStatus
	if err := sts[len(sts)-1].Decode(&p); err != nil {
		e.t.Fatal(err)
	}
	return p.Outcome
}

// TestTheOutcomeOfATriggersTurn is Topos spec 086's outcome criterion: a
// turn a trigger's message began records notified when its trigger always
// brings its result, when the agent called notify, when it waited for the
// person or asked them on the way, and when the agent holds no notify;
// quiet when a judged turn or a wake's ends with none of those, on the
// idle status or on the end end_on_idle makes; a turn a person began
// records none; and the header folds the latest outcome and notice.
func TestTheOutcomeOfATriggersTurn(t *testing.T) {
	asks := &fakeTool{name: "deploy", props: tools.Properties{Effect: tools.EffectWrite}}
	connect := &fakeTool{name: "fetch", props: tools.Properties{Effect: tools.EffectRead}, run: func(tools.Call) (tools.Result, error) {
		res := tools.Text(tools.OutcomeError, "not connected")
		res.Requested = &session.ConnectionRequested{Name: "github", Title: "GitHub", Kind: "http"}
		return res, nil
	}}
	withTools := func(notify bool) func(*Config) {
		return func(c *Config) {
			c.Notify = notify
			c.Policy = Policy{Mode: ModeConfirm, AlwaysConfirm: []string{"deploy"}}
			for _, f := range []*fakeTool{asks, connect} {
				if err := c.Tools.Add(f); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	cases := []struct {
		name   string
		notify bool
		start  func(e *env, ctx context.Context)
		script []ir.Block
		end    ir.StopReason
		want   string
	}{
		{"always", true, func(e *env, ctx context.Context) { e.fire(ctx, false) }, []ir.Block{text("The fare is the same.")}, ir.StopEndTurn, session.OutcomeNotified},
		{"judged and quiet", true, func(e *env, ctx context.Context) { e.fire(ctx, true) }, []ir.Block{text("The fare is the same.")}, ir.StopEndTurn, session.OutcomeQuiet},
		{"judged with notify", true, func(e *env, ctx context.Context) { e.fire(ctx, true) }, []ir.Block{call("toolu_n", session.ToolNotify, `{"title":"The fare dropped"}`)}, ir.StopToolUse, session.OutcomeNotified},
		{"judged with a held call", true, func(e *env, ctx context.Context) { e.fire(ctx, true) }, []ir.Block{call("toolu_d", "deploy", `{"text":"x"}`)}, ir.StopToolUse, session.OutcomeNotified},
		{"judged with a connection asked", true, func(e *env, ctx context.Context) { e.fire(ctx, true) }, []ir.Block{call("toolu_f", "fetch", `{"text":"x"}`)}, ir.StopToolUse, session.OutcomeNotified},
		{"judged without the tool", false, func(e *env, ctx context.Context) { e.fire(ctx, true) }, []ir.Block{text("The fare is the same.")}, ir.StopEndTurn, session.OutcomeNotified},
		{"a wake's", true, func(e *env, ctx context.Context) { e.appendEvents(ctx, wakeMessage(t, 1)); e.running(ctx) }, []ir.Block{text("Nothing new.")}, ir.StopEndTurn, session.OutcomeQuiet},
		{"a person's", true, func(e *env, ctx context.Context) { e.send(ctx, "How is the fare?") }, []ir.Block{text("The same.")}, ir.StopEndTurn, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, withTools(c.notify))
			ctx := t.Context()
			if c.end == ir.StopToolUse {
				e.stub.Script(model, reply(c.end, c.script...), reply(ir.StopEndTurn, text("Done.")))
			} else {
				e.stub.Script(model, reply(c.end, c.script...))
			}
			c.start(e, ctx)
			e.turn(ctx)
			if got := e.lastOutcome(ctx); got != c.want {
				t.Fatalf("the outcome is %q, want %q", got, c.want)
			}
			s, err := e.store.Get(ctx, e.s.ID)
			if err != nil || s.Outcome != c.want {
				t.Fatalf("the header's outcome is %q (%v), want %q", s.Outcome, err, c.want)
			}
			if c.name == "judged with notify" && (s.Notice == nil || s.Notice.Title != "The fare dropped" || s.Notice.Seq == 0) {
				t.Fatalf("the header's notice is %+v", s.Notice)
			}
		})
	}

	// A session that ends on idle records the outcome on its end.
	e := setupSession(t, withTools(true), func(s *session.Session) { s.EndOnIdle = true })
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopEndTurn, text("Nothing new.")))
	e.fire(ctx, true)
	if out := e.turn(ctx); out.Status != session.StatusEnded || e.lastOutcome(ctx) != session.OutcomeQuiet {
		t.Fatalf("the end of a quiet run: %+v, outcome %q", out, e.lastOutcome(ctx))
	}
}

// TestNotifyIsOneLineOnceATurn: a notify's title is required, one line
// and at most MaxNoticeTitle characters; a second call of a turn is
// answered invalid_input and records no notice; and a thread is never
// offered the tool.
func TestNotifyIsOneLineOnceATurn(t *testing.T) {
	e := setup(t, func(c *Config) { c.Notify = true })
	ctx := t.Context()
	long := strings.Repeat("x", session.MaxNoticeTitle+1)
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", session.ToolNotify, `{"title":"`+long+`"}`), call("toolu_2", session.ToolNotify, `{"title":"a\nb"}`)),
		reply(ir.StopToolUse, call("toolu_3", session.ToolNotify, `{"title":"The fare dropped"}`)),
		reply(ir.StopToolUse, call("toolu_4", session.ToolNotify, `{"title":"Again"}`)),
		reply(ir.StopEndTurn, text("Told.")))
	e.fire(ctx, true)
	e.turn(ctx)
	results := map[string]session.ToolResult{}
	for _, ev := range e.events(ctx, session.TypeToolResult) {
		var r session.ToolResult
		if err := ev.Decode(&r); err != nil {
			t.Fatal(err)
		}
		results[r.ToolUseID] = r
	}
	for id, want := range map[string]string{
		"toolu_1": "longer than " + strconv.Itoa(session.MaxNoticeTitle) + " characters",
		"toolu_2": "title is one line",
		"toolu_4": prompts.Text(prompts.NotifyAgain),
	} {
		if r := results[id]; r.Outcome != tools.OutcomeInvalidInput || !strings.Contains(r.Content[0].Text, want) || len(r.Meta) != 0 {
			t.Errorf("%s answered %+v, want invalid_input naming %q", id, r, want)
		}
	}
	var m tools.Meta
	if r := results["toolu_3"]; r.Outcome != tools.OutcomeOK || json.Unmarshal(r.Meta, &m) != nil || m.Notice == nil || m.Notice.Title != "The fare dropped" {
		t.Fatalf("the notice answered %+v", r)
	}
	if s, err := e.store.Get(ctx, e.s.ID); err != nil || s.Notice == nil || s.Notice.Title != "The fare dropped" {
		t.Fatalf("the header's notice is %+v, %v", s.Notice, err)
	}
	p := notifyTool{}.Properties()
	if p.Effect != tools.EffectNone || p.Parallel {
		t.Fatalf("the properties are %+v", p)
	}
	if r := (notifyTool{}).Check(tools.Call{Input: json.RawMessage(`{"title":"x","extra":1}`)}); r == nil || r.Outcome != tools.OutcomeInvalidInput {
		t.Fatalf("an unknown field passed: %+v", r)
	}
	if res, err := (notifyTool{t: &turn{sh: &shared{}}}).Run(ctx, tools.Call{Input: json.RawMessage(`{}`)}); err != nil || res.Outcome != tools.OutcomeInvalidInput {
		t.Fatalf("a call with no title ran: %+v, %v", res, err)
	}
}

// TestNotifyTextReadsItsBound: the notify tool's description and schema
// state the title's bound.
func TestNotifyTextReadsItsBound(t *testing.T) {
	d := notifyTool{}.Definition()
	if d.Description != prompts.Text(prompts.ToolNotify) || !strings.Contains(d.Description, "at most "+strconv.Itoa(session.MaxNoticeTitle)+" characters") {
		t.Fatalf("the description is %q", d.Description)
	}
	var s struct {
		Properties map[string]struct {
			MaxLength int `json:"maxLength"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(d.InputSchema, &s); err != nil || s.Properties["title"].MaxLength != session.MaxNoticeTitle {
		t.Fatalf("the schema bound is %+v, %v", s.Properties, err)
	}
}

// TestAWokenTurnIsToldNobodyWaits: the message that began a judged turn,
// a trigger's or a wake's, is led by what such a turn is, with whether a
// question waits for the person, when the thread holds notify; an always
// trigger's message, and any message to a thread without notify, is not.
func TestAWokenTurnIsToldNobodyWaits(t *testing.T) {
	msg := func(judged bool) session.Event {
		m := session.UserMessage{Sender: session.Sender{Subject: "trigger:trg_1", Kind: session.SenderTrigger}, Content: []lux.Block{{Type: ir.BlockText, Text: "Look."}}, FiringID: "frg_1"}
		if judged {
			m.Notify = session.NotifyJudged
		}
		ev, err := session.NewEvent(session.TypeUserMessage, m, t0)
		if err != nil {
			t.Fatal(err)
		}
		ev.Seq = 1
		return ev
	}
	for _, c := range []struct {
		name             string
		ev               session.Event
		notify, attended bool
		woken            bool
	}{
		{"judged", msg(true), true, false, true},
		{"judged and attended", msg(true), true, true, true},
		{"a wake's", func() session.Event { ev := wakeMessage(t, 1); ev.Seq = 1; return ev }(), true, false, true},
		{"always", msg(false), true, false, false},
		{"without notify", msg(true), false, false, false},
	} {
		tr, err := session.FoldWith([]session.Event{c.ev}, "", session.FoldOptions{Notify: c.notify, Attended: c.attended})
		if err != nil {
			t.Fatal(err)
		}
		lead := tr.Messages[0].Blocks[0].Text
		if strings.HasPrefix(lead, "<woken>") != c.woken {
			t.Errorf("%s: the message is led by %q", c.name, lead)
		}
		if c.woken && strings.Contains(lead, "waits for the person's answer") != c.attended {
			t.Errorf("%s: the woken text says %q", c.name, lead)
		}
	}
}
