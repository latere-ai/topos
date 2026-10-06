// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
)

// forkOf forks e's session before its message that says replaced, the
// fork sent text in that message's place, and answers the fork's header
// and the log it runs on, its running status appended as the runner
// appends it.
func (e *env) forkOf(ctx context.Context, replaced, text string) (session.Session, []session.Event, *storeLog) {
	e.t.Helper()
	parent, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	var before uint64
	for _, ev := range evs {
		var m session.UserMessage
		if ev.Type == session.TypeUserMessage && ev.Decode(&m) == nil && len(m.Content) == 1 && m.Content[0].Text == replaced {
			before = ev.Seq
		}
	}
	at, err := session.ForkBefore(evs, before)
	if err != nil {
		e.t.Fatal(err)
	}
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: text}}}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	child := session.New(parent.Agent, parent.Initiator, parent.Runner, parent.Machine, t0)
	child.Budget.MaxCostUSDMicro = parent.Budget.MaxCostUSDMicro
	if child, err = session.Fork(ctx, e.store, child, nil, parent, evs[:at], msg); err != nil {
		e.t.Fatal(err)
	}
	log := &storeLog{st: e.store, id: child.ID}
	run, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning, Runner: &session.RunnerRef{ID: "run_1", Kind: session.RunnerHosted}}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := log.Append(ctx, []session.Event{run}); err != nil {
		e.t.Fatal(err)
	}
	if child, err = e.store.Get(ctx, child.ID); err != nil {
		e.t.Fatal(err)
	}
	cevs, err := e.store.Events(ctx, child.ID, 1, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return child, cevs, log
}

// TestAForksBudgetCountsItsOwnSpend: a fork of a session that spent past
// the fork's ceiling runs its first request, since what it copied was
// the parent's spend, and its own spend still stops it at its ceiling.
func TestAForksBudgetCountsItsOwnSpend(t *testing.T) {
	limit := int64(200)
	e := setupSession(t, nil, func(s *session.Session) { s.Budget.MaxCostUSDMicro = &limit })
	ctx := t.Context()
	// The parent's turn spent past the ceiling the fork is given.
	e.send(ctx, "Plan it for three people.")
	spent := int64(5000)
	var turn []session.Event
	for _, p := range []struct {
		typ     session.Type
		payload any
	}{
		{session.TypeModelRequest, session.ModelRequest{Model: model, CostUSDMicro: &spent}},
		{session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: "Planned for three."}}}, StopReason: ir.StopEndTurn}},
		{session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn}},
		{session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Plan it for four."}}}},
	} {
		ev, err := session.NewEvent(p.typ, p.payload, t0)
		if err != nil {
			t.Fatal(err)
		}
		ev.Turn = 1
		turn = append(turn, ev)
	}
	e.appendEvents(ctx, turn...)
	parent, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Budget.SpentCostUSDMicro != spent {
		t.Fatalf("the parent spent %d, want %d", parent.Budget.SpentCostUSDMicro, spent)
	}

	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("never")))
	child, evs, log := e.forkOf(ctx, "Plan it for four.", "Plan it for five.")
	if child.Budget.CarriedCostUSDMicro != parent.Budget.SpentCostUSDMicro {
		t.Fatalf("the fork carries %d, the parent spent %d", child.Budget.CarriedCostUSDMicro, parent.Budget.SpentCostUSDMicro)
	}
	before := len(e.stub.Requests())
	out, err := e.h.RunTurn(ctx, child, evs, log)
	if err != nil || out.StopReason != session.StopBudget {
		t.Fatalf("the fork's turn: %+v, %v", out, err)
	}
	if n := len(e.stub.Requests()) - before; n != 1 {
		t.Fatalf("the fork sent %d requests; its first runs on its own budget, and its own spend refuses the second", n)
	}
}

// TestTheCacheKeyIsTheTreesRoot: a fork's request carries its tree's
// root as its cache key, so its prefix and its parent's share one key,
// and a session no fork made carries its own id.
func TestTheCacheKeyIsTheTreesRoot(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.send(ctx, "Plan it for three people.")
	key := func(s session.Session) string {
		t.Helper()
		evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		tu := &turn{h: e.h, s: s, sh: &shared{events: evs}, l: e.log, root: e.h.c.Tools, reg: e.h.c.Tools, num: 1, start: t0}
		tr, err := session.Fold(tu.events(), "")
		if err != nil {
			t.Fatal(err)
		}
		req, _, err := tu.request(ctx, tr)
		if err != nil {
			t.Fatal(err)
		}
		return req.CacheKey
	}
	if got := key(e.s); got != e.s.ID {
		t.Fatalf("a session no fork made is keyed %q, want its id %s", got, e.s.ID)
	}
	fork := e.s
	fork.ID, fork.Root, fork.Parent = session.NewID(session.PrefixSession), session.NewID(session.PrefixSession), &session.Parent{SessionID: e.s.ID, Seq: 1}
	if got := key(fork); got != fork.Root {
		t.Fatalf("a fork is keyed %q, want its tree's root %s", got, fork.Root)
	}
}

// TestAForksPrefixIsItsParents: the first request of a fork before a
// message is its parent's request for that message up to the message,
// byte for byte, when no machine attached between, so a provider that
// matches a cached prefix by its bytes reads it.
func TestAForksPrefixIsItsParents(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopEndTurn, text("Planned for three.")), reply(ir.StopEndTurn, text("About 900 euros.")), reply(ir.StopEndTurn, text("About 1200 euros.")))
	e.send(ctx, "Plan it for three people.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("the parent's first turn: %+v", out)
	}
	e.send(ctx, "And the budget?")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("the parent's second turn: %+v", out)
	}
	child, evs, log := e.forkOf(ctx, "And the budget?", "And the budget for four?")
	if out, err := e.h.RunTurn(ctx, child, evs, log); err != nil || out.StopReason != session.StopEndTurn {
		t.Fatalf("the fork's turn: %+v, %v", out, err)
	}
	reqs := e.stub.Requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want the parent's two and the fork's one", len(reqs))
	}
	type wire struct {
		System   json.RawMessage   `json:"system"`
		Tools    json.RawMessage   `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	var parent, fork wire
	if err := json.Unmarshal(reqs[1].Body, &parent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reqs[2].Body, &fork); err != nil {
		t.Fatal(err)
	}
	n := len(parent.Messages)
	switch {
	case !bytes.Equal(parent.System, fork.System):
		t.Fatalf("the fork's system prompt differs:\n%s\n%s", fork.System, parent.System)
	case !bytes.Equal(parent.Tools, fork.Tools):
		t.Fatal("the fork's tools differ from its parent's")
	case len(fork.Messages) != n || n < 2:
		t.Fatalf("the fork sends %d messages, its parent %d", len(fork.Messages), n)
	}
	for i := range n - 1 {
		if !bytes.Equal(parent.Messages[i], fork.Messages[i]) {
			t.Fatalf("message %d differs:\n%s\n%s", i, fork.Messages[i], parent.Messages[i])
		}
	}
	if bytes.Equal(parent.Messages[n-1], fork.Messages[n-1]) || !bytes.Contains(fork.Messages[n-1], []byte("for four")) {
		t.Fatalf("the fork's last message is not its own: %s", fork.Messages[n-1])
	}
}
