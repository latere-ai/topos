// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

// sender is a tool that appends a message through the store while it
// runs, as a client beside the turn does, and then waits for its context
// when wait is set.
type sender struct {
	st   session.Store
	id   string
	msg  session.UserMessage
	wait bool
}

func (sender) Definition() tools.Definition {
	return tools.Definition{Name: "send", Description: "sends while it runs", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (sender) Properties() tools.Properties { return tools.Properties{Effect: tools.EffectRead} }
func (s sender) Run(ctx context.Context, c tools.Call) (tools.Result, error) {
	h, err := s.st.Get(context.WithoutCancel(ctx), s.id)
	if err != nil {
		return tools.Result{}, err
	}
	e, err := session.NewEvent(session.TypeUserMessage, s.msg, t0)
	if err != nil {
		return tools.Result{}, err
	}
	evs := []session.Event{e}
	session.Stamp(s.id, h.LastSeq, evs)
	if _, err := s.st.Append(context.WithoutCancel(ctx), s.id, h.LastSeq, evs); err != nil {
		return tools.Result{}, err
	}
	if !s.wait {
		return tools.Text(tools.OutcomeOK, "sent"), nil
	}
	select {
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	case <-time.After(10 * time.Second):
		return tools.Text(tools.OutcomeOK, "never cut"), nil
	}
}

// withSender adds a sender of msg to every harness the runner builds.
func withSender(f *fixture, msg session.UserMessage, wait bool) {
	base := f.r.o.Harness
	f.r.o.Harness = func(ctx context.Context, s session.Session) (harness.Config, error) {
		c, err := base(ctx, s)
		if err != nil {
			return c, err
		}
		return c, c.Tools.AddBuiltin(sender{st: f.store, id: s.ID, msg: msg, wait: wait})
	}
}

func words(s string) []lux.Block { return []lux.Block{{Type: ir.BlockText, Text: s}} }

var sendCall = ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_s", Name: "send", Args: json.RawMessage(`{}`)}}

func (f *fixture) statuses(ctx context.Context) []session.SessionStatus {
	f.t.Helper()
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []session.SessionStatus
	for _, e := range evs {
		var p session.SessionStatus
		if e.Type == session.TypeSessionStatus && e.Decode(&p) == nil {
			out = append(out, p)
		}
	}
	return out
}

func TestAWaitingFiringStartsTheNextTurn(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	withSender(f, session.UserMessage{Sender: session.Sender{Subject: session.TriggerSubjectPrefix + "trg_1", Kind: session.SenderTrigger}, Content: words("Check the deploy."), FiringID: "fir_1"}, false)
	f.stub.Script(model, reply(sendCall), reply(ir.Block{Type: ir.BlockText, Text: "Started."}), reply(ir.Block{Type: ir.BlockText, Text: "The deploy is fine."}))
	f.message(ctx, "Start the server.")
	out, err := f.r.Drive(ctx, f.s.ID)
	if err != nil || out.StopReason != session.StopEndTurn || out.Pending {
		t.Fatalf("drive %+v, %v", out, err)
	}
	reqs := f.stub.Requests()
	if len(reqs) != 3 || strings.Contains(string(reqs[1].Body), "Check the deploy.") || !strings.Contains(string(reqs[2].Body), "Check the deploy.") {
		t.Fatalf("%d requests; the firing is read by the turn it arrived in, or never", len(reqs))
	}
	running := 0
	for _, p := range f.statuses(ctx) {
		if p.Status == session.StatusRunning {
			running++
		}
	}
	if running != 2 {
		t.Fatalf("%d turns, want the firing's own", running)
	}
}

func TestSendNowStartsTheNextTurn(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	withSender(f, session.UserMessage{Sender: f.s.Initiator, Content: words("Stop, use 9000."), SendNow: true}, true)
	f.stub.Script(model, reply(sendCall), reply(ir.Block{Type: ir.BlockText, Text: "On 9000."}))
	f.message(ctx, "Start the server.")
	start := time.Now()
	out, err := f.r.Drive(ctx, f.s.ID)
	if err != nil || out.StopReason != session.StopEndTurn || time.Since(start) > 8*time.Second {
		t.Fatalf("drive %+v, %v after %s", out, err, time.Since(start))
	}
	st := f.statuses(ctx)
	cut := false
	for _, p := range st {
		cut = cut || (p.StopReason == session.StopInterrupted && p.Detail == session.DetailSendNow)
	}
	if !cut {
		t.Fatalf("statuses %+v, none cut for the message", st)
	}
	reqs := f.stub.Requests()
	if len(reqs) != 2 || !strings.Contains(string(reqs[1].Body), "Ada stopped your work to send this:") {
		t.Fatalf("%d requests, the last %s", len(reqs), reqs[len(reqs)-1].Body)
	}
}
