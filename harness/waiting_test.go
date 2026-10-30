// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

// says appends a message of sender while the turn runs.
func (e *env) says(sender session.Sender, words string, now bool) session.Event {
	e.t.Helper()
	return e.alongside(context.Background(), session.TypeUserMessage, session.UserMessage{Sender: sender, Content: []lux.Block{{Type: ir.BlockText, Text: words}}, SendNow: now})
}

var aFiring = session.Sender{Subject: session.TriggerSubjectPrefix + "trg_1", Kind: session.SenderTrigger}

// bodies are the bodies of the requests the stub received.
func (e *env) bodies() []string {
	var out []string
	for _, r := range e.stub.Requests() {
		out = append(out, string(r.Body))
	}
	return out
}

// before reports whether a precedes b in s, both present.
func before(s, a, b string) bool {
	i, j := strings.Index(s, a), strings.Index(s, b)
	return i >= 0 && j >= 0 && i < j
}

func (e *env) reads(ctx context.Context) [][]string {
	var out [][]string
	for _, ev := range e.events(ctx, session.TypeMessagesRead) {
		var p session.MessagesRead
		if err := ev.Decode(&p); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p.EventIDs)
	}
	return out
}

func TestAMessageWhileCallsRunIsReadAtTheNextStep(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	var mid session.Event
	e.echo.run = func(tools.Call) (tools.Result, error) {
		mid = e.says(e.s.Initiator, "Use port 9000.", false)
		return tools.Text(tools.OutcomeOK, "started on 8000"), nil
	}
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("Moved to 9000.")))
	e.send(ctx, "Start the server.")
	out := e.turn(ctx)
	if out.StopReason != session.StopEndTurn || out.Pending {
		t.Fatalf("outcome %+v", out)
	}
	b := e.bodies()
	if len(b) != 2 || strings.Contains(b[0], "port 9000") {
		t.Fatalf("%d requests, the first %q", len(b), b[0])
	}
	if !before(b[1], "started on 8000", "Ada sent this while you were working:") || !before(b[1], "while you were working:", "Use port 9000.") {
		t.Fatalf("the second request does not read the message after the result, led by its line: %s", b[1])
	}
	if got := e.reads(ctx); len(got) != 1 || len(got[0]) != 1 || got[0][0] != mid.ID {
		t.Fatalf("messages.read %v, want [[%s]]", got, mid.ID)
	}
	var mr session.ModelRequest
	if err := e.events(ctx, session.TypeModelRequest)[1].Decode(&mr); err != nil || !mr.NamesReads {
		t.Fatalf("model.request %+v, %v", mr, err)
	}
}

func TestAMessageDuringARequestIsReadAfterItsAnswer(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	first := reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`))
	first.Expect = func(*ir.Request) error {
		e.says(e.s.Initiator, "Use port 9000.", false)
		return nil
	}
	e.stub.Script(model, first, reply(ir.StopEndTurn, text("Moved to 9000.")))
	e.send(ctx, "Start the server.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn || out.Pending {
		t.Fatalf("outcome %+v", out)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := session.Fold(evs, "")
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range tr.Messages {
		roles = append(roles, string(m.Role))
	}
	// The message arrived before the request's answer was appended, and
	// reads after it: the call, then its result and the message.
	if strings.Join(roles, ",") != "user,assistant,user,assistant" {
		t.Fatalf("roles %v", roles)
	}
	third := tr.Messages[2].Blocks
	if third[0].Type != ir.BlockToolResult || third[len(third)-1].Text != "Use port 9000." {
		t.Fatalf("the message after the call %+v", third)
	}
}

func TestAFiringWaitsForTheTurnsEnd(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.echo.run = func(tools.Call) (tools.Result, error) {
		e.alongside(context.Background(), session.TypeUserMessage, session.UserMessage{Sender: aFiring, Content: []lux.Block{{Type: ir.BlockText, Text: "Check the deploy."}}, FiringID: "fir_1"})
		return tools.Text(tools.OutcomeOK, "started"), nil
	}
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("Started.")), reply(ir.StopEndTurn, text("Deploy is fine.")))
	e.send(ctx, "Start the server.")
	out := e.turn(ctx)
	if out.StopReason != session.StopEndTurn || !out.Pending {
		t.Fatalf("a turn that held a firing %+v", out)
	}
	if b := e.bodies(); strings.Contains(b[1], "Check the deploy.") {
		t.Fatalf("the turn the firing arrived in read it: %s", b[1])
	}
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn || out.Pending {
		t.Fatalf("the next turn %+v", out)
	}
	b := e.bodies()
	if len(b) != 3 || !before(b[2], "Started.", "Check the deploy.") || strings.Contains(b[2], "while you were working") {
		t.Fatalf("the next turn's request: %s", b[len(b)-1])
	}
}

func TestEndOnIdleReadsAHeldMessageFirst(t *testing.T) {
	e := setupSession(t, nil, func(s *session.Session) { s.EndOnIdle = true })
	ctx := t.Context()
	first := reply(ir.StopEndTurn, text("Started."))
	first.Expect = func(*ir.Request) error {
		e.says(e.s.Initiator, "Also add a test.", false)
		return nil
	}
	e.stub.Script(model, first, reply(ir.StopEndTurn, text("Added.")))
	e.send(ctx, "Start the server.")
	if out := e.turn(ctx); out.Status != session.StatusIdle || !out.Pending {
		t.Fatalf("a session ended with a message unread: %+v", out)
	}
	e.running(ctx)
	if out := e.turn(ctx); out.Status != session.StatusEnded {
		t.Fatalf("the turn that read it %+v", out)
	}
}

func TestAWithdrawalBeforeTheRequestKeepsItOut(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.echo.run = func(tools.Call) (tools.Result, error) {
		m := e.says(e.s.Initiator, "Use port 9000.", false)
		e.alongside(context.Background(), session.TypeUserWithdraw, session.UserWithdraw{Sender: e.s.Initiator, EventID: m.ID})
		return tools.Text(tools.OutcomeOK, "started"), nil
	}
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("Started.")))
	e.send(ctx, "Start the server.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn || out.Pending {
		t.Fatalf("outcome %+v", out)
	}
	if b := e.bodies(); strings.Contains(b[1], "port 9000") {
		t.Fatalf("a withdrawn message was read: %s", b[1])
	}
	if got := e.reads(ctx); len(got) != 0 {
		t.Fatalf("messages.read %v", got)
	}
}

// sendNow is a SendNow the test signals on, as the runner's watch does.
func sendNow(ch chan session.Event) func(*Config) {
	return func(c *Config) { c.SendNow = func() <-chan session.Event { return ch } }
}

func TestSendNowCutsARequest(t *testing.T) {
	now := make(chan session.Event, 4)
	e := setup(t, sendNow(now))
	ctx := t.Context()
	first := reply(ir.StopEndTurn, text("too late"))
	first.Expect = func(*ir.Request) error {
		now <- e.says(e.s.Initiator, "Stop, use 9000.", true)
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	e.stub.Script(model, first, reply(ir.StopEndTurn, text("On 9000.")))
	e.send(ctx, "Start the server.")
	out := e.turn(ctx)
	if out.StopReason != session.StopInterrupted || out.Detail != session.DetailSendNow || !out.Pending {
		t.Fatalf("outcome %+v", out)
	}
	var mr session.ModelRequest
	if err := e.events(ctx, session.TypeModelRequest)[0].Decode(&mr); err != nil || mr.Outcome != "canceled" {
		t.Fatalf("model.request %+v, %v", mr, err)
	}
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("the next turn %+v", out)
	}
	b := e.bodies()
	if !before(b[len(b)-1], "Ada stopped your work to send this:", "Stop, use 9000.") {
		t.Fatalf("the next turn's request: %s", b[len(b)-1])
	}
}

func TestSendNowWaitsForCallsThatReturn(t *testing.T) {
	now := make(chan session.Event, 4)
	e := setup(t, sendNow(now))
	ctx := t.Context()
	e.echo.run = func(c tools.Call) (tools.Result, error) {
		now <- e.says(e.s.Initiator, "Use port 9000.", true)
		time.Sleep(50 * time.Millisecond)
		return tools.Text(tools.OutcomeOK, "started"), nil
	}
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("On 9000.")))
	e.send(ctx, "Start the server.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn || out.Pending {
		t.Fatalf("outcome %+v", out)
	}
	if b := e.bodies(); len(b) != 2 || !before(b[1], "Ada sent this while you were working:", "Use port 9000.") {
		t.Fatalf("requests %q", b)
	}
}

func TestSendNowCutsACallPastTheGrace(t *testing.T) {
	now := make(chan session.Event, 4)
	e := setup(t, sendNow(now))
	ctx := t.Context()
	e.echo.run = func(c tools.Call) (tools.Result, error) {
		now <- e.says(e.s.Initiator, "Stop, use 9000.", true)
		time.Sleep(SendNowGrace + 300*time.Millisecond)
		return tools.Result{}, nil
	}
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("never")))
	e.send(ctx, "Start the server.")
	out := e.turn(ctx)
	if out.StopReason != session.StopInterrupted || out.Detail != session.DetailSendNow || !out.Pending {
		t.Fatalf("outcome %+v", out)
	}
	var res session.ToolResult
	if err := e.events(ctx, session.TypeToolResult)[0].Decode(&res); err != nil || res.Outcome != tools.OutcomeCanceled {
		t.Fatalf("result %+v, %v", res, err)
	}
	if n := len(e.stub.Requests()); n != 1 {
		t.Fatalf("%d requests in the cut turn", n)
	}
}

func TestSendNowIgnoresAMessageTheStepRead(t *testing.T) {
	now := make(chan session.Event, 4)
	e := setup(t, sendNow(now))
	ctx := t.Context()
	var m session.Event
	e.echo.run = func(c tools.Call) (tools.Result, error) {
		m = e.says(e.s.Initiator, "Use port 9000.", true)
		return tools.Text(tools.OutcomeOK, "started"), nil
	}
	second := reply(ir.StopEndTurn, text("On 9000."))
	second.Expect = func(*ir.Request) error {
		// The signal comes late, after the request that read it was sent.
		now <- m
		time.Sleep(100 * time.Millisecond)
		return nil
	}
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)), second)
	e.send(ctx, "Start the server.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("a read message cut its reader: %+v", out)
	}
}

func TestSendNowOnAnIdleSessionChangesNothing(t *testing.T) {
	now := make(chan session.Event, 4)
	e := setup(t, sendNow(now))
	ctx := t.Context()
	first := reply(ir.StopEndTurn, text("Hi."))
	first.Expect = func(*ir.Request) error {
		time.Sleep(100 * time.Millisecond)
		return nil
	}
	e.stub.Script(model, first)
	m := e.says(e.s.Initiator, "Hello.", true)
	now <- m
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn || out.Pending {
		t.Fatalf("a message sent now to an idle session cut its own turn: %+v", out)
	}
}
