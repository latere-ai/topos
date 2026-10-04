// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// TestASearchCostIsCounted (spec 040): a tool result's cost is recorded
// on its tool.result, the session's header counts it, and the budget
// check before the next request counts it, so a search that passes the
// session's budget stops the turn with budget before the next request.
func TestASearchCostIsCounted(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	charged := int64(10_000)
	e.echo.run = func(c tools.Call) (tools.Result, error) {
		res := tools.Text(tools.OutcomeOK, "1. a result")
		res.CostUSDMicro = &charged
		return res, nil
	}
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_s", "echo", `{"text":"go"}`)), reply(ir.StopEndTurn, text("never")))
	e.send(ctx, "Search.")
	s, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(5_000)
	s.Budget.MaxCostUSDMicro = &limit
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if session.Spent(evs) >= limit {
		t.Fatalf("the request alone spent %d, past the limit", session.Spent(evs))
	}
	out, err := e.h.RunTurn(ctx, s, evs, e.log)
	if err != nil || out.StopReason != session.StopBudget {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	if n := len(e.stub.Requests()); n != 1 {
		t.Fatalf("%d requests; the search's cost stops the turn before the second", n)
	}
	results := e.events(ctx, session.TypeToolResult)
	var p session.ToolResult
	if len(results) != 1 || results[0].Decode(&p) != nil || p.CostUSDMicro == nil || *p.CostUSDMicro != charged {
		t.Fatalf("the result %+v", p)
	}
	after, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	all, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if after.Budget.SpentCostUSDMicro != session.Spent(all) || after.Budget.SpentCostUSDMicro < charged {
		t.Fatalf("the header spent %d, the log %d", after.Budget.SpentCostUSDMicro, session.Spent(all))
	}
}

// TestWebSearchChangesNothing (spec 040): a web_search call scores 0 and
// is allowed in every mode, plan included, an always-confirm pattern
// that names it asks, and a call never opens a machine opened on demand.
func TestWebSearchChangesNothing(t *testing.T) {
	props := tools.WebSearch(nil).Properties()
	input := json.RawMessage(`{"query":"go"}`)
	risk := Score(tools.NameWebSearch, props, input, machine.KindCella, nil)
	if risk.Score != 0 {
		t.Fatalf("risk %+v", risk)
	}
	for _, mode := range []Mode{ModePlan, ModeConfirm, ModeProgressive} {
		if d := (Policy{Mode: mode}).Decide(tools.NameWebSearch, props, input, risk, machine.KindHost, nil); d.Verdict != VerdictAllow {
			t.Errorf("%s: %+v", mode, d)
		}
	}
	if d := (Policy{Mode: ModeConfirm, AlwaysConfirm: []string{tools.NameWebSearch}}).Decide(tools.NameWebSearch, props, input, risk, machine.KindHost, nil); d.Verdict != VerdictAsk {
		t.Fatalf("always-confirm: %+v", d)
	}
	e := setup(t, func(c *Config) {
		c.Machine = machine.Defer(t.Context(), machine.KindCella, func(context.Context) (machine.Machine, error) {
			t.Error("a search opened the machine")
			return nil, errors.New("not opened")
		})
		if err := c.Tools.AddBuiltin(tools.WebSearch(nil)); err != nil {
			t.Fatal(err)
		}
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_s", tools.NameWebSearch, `{"query":"go"}`)), reply(ir.StopEndTurn, text("done")))
	e.send(ctx, "Search.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if n := len(e.events(ctx, session.TypeSessionMachine)); n != 0 {
		t.Fatalf("%d session.machine events", n)
	}
}
