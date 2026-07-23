// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package loop_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/topos/billing"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/runtime/loop"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
)

// meteredModel emits usage and a tool call on every turn, so a run that is not
// stopped by something else exhausts loop.MaxIterations. It is the shape that
// separates a cap that fires from a cap that only claims to. It counts its
// calls so a test can assert a run that must not spend at all did not.
type meteredModel struct {
	usage models.Usage
	calls atomic.Int32
}

func (m *meteredModel) Stream(_ context.Context, _ models.Request) (models.Stream, error) {
	m.calls.Add(1)
	u := m.usage
	return &cannedStream{events: []models.Event{
		{Kind: models.KindTextDelta, TextDelta: "working"},
		{Kind: models.KindToolCallDone, ToolCall: &models.ToolCall{
			ID: "call_1", Name: "bash", Input: []byte(`{"command":"echo hi"}`),
		}},
		{Kind: models.KindUsage, Usage: &u},
		{Kind: models.KindDone, StopReason: models.StopToolUse},
	}}, nil
}

// flatRate prices every turn's accumulated input tokens at one USD each, so a
// budget test states its arithmetic in turns rather than in a rate card.
type flatRate struct{}

func (flatRate) CostUSD(_ string, u models.Usage) (float64, error) {
	return float64(u.InputTokens), nil
}

// unpriceable refuses to price anything, standing in for a model no rate card
// covers.
type unpriceable struct{}

func (unpriceable) CostUSD(model string, _ models.Usage) (float64, error) {
	return 0, errUnpriceable{model}
}

type errUnpriceable struct{ model string }

func (e errUnpriceable) Error() string { return "no price for model " + e.model }

func budgetCfg(t *testing.T, model models.Model) loop.Config {
	t.Helper()
	p := local.New()
	sb, err := p.Create(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = p.Destroy(context.Background(), sb.ID) })

	cfg := baseCfg(model, tools.Builtins())
	cfg.Sandbox = p
	cfg.SandboxID = sb.ID
	return cfg
}

func meter(t *testing.T, limitUSD float64, cost billing.CostSource) *billing.Meter {
	t.Helper()
	return billing.NewMeter("test-model", cost,
		billing.NewEnforcer(billing.Budget{LimitUSD: limitUSD}, "budget-test", "agent", "owner", nil))
}

// TestLoopStopsOnBudgetBreach asserts the spend cap actually stops the run. The
// model would otherwise loop to MaxIterations, so the assertion is that the run
// ends early, on a budget-specific stop reason, carrying the breach — and that
// the stop is reported as an error rather than passed off as a completed run.
func TestLoopStopsOnBudgetBreach(t *testing.T) {
	// One USD per turn against a three USD cap: turns 1 and 2 are under, turn 3
	// reaches it.
	cfg := budgetCfg(t, &meteredModel{usage: models.Usage{InputTokens: 1, OutputTokens: 1}})

	res, err := loop.Run(context.Background(), cfg, meter(t, 3, flatRate{}))
	if !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("Run error = %v, want billing.ErrBudgetExceeded", err)
	}
	if res == nil {
		t.Fatal("Run = nil result, want the partial transcript alongside the error")
	}
	if res.FinalText == "" || len(res.Transcript) == 0 {
		t.Fatalf("partial result discarded: final=%q transcript=%d", res.FinalText, len(res.Transcript))
	}
	if res.StopReason != models.StopBudgetExceeded {
		t.Fatalf("stop reason = %q, want %q (ran %d turns of %d)",
			res.StopReason, models.StopBudgetExceeded, res.TotalUsage.InputTokens, loop.MaxIterations)
	}
	if res.TotalUsage.InputTokens != 3 {
		t.Fatalf("ran %d turns, want to stop on the 3rd", res.TotalUsage.InputTokens)
	}
	if res.BudgetBreach == nil {
		t.Fatal("BudgetBreach = nil, want the breach that stopped the run")
	}
	if res.BudgetBreach.Leg != "usd" || res.BudgetBreach.Limit != 3 || res.BudgetBreach.Actual != 3 {
		t.Fatalf("breach = %+v, want usd 3/3", *res.BudgetBreach)
	}
}

// TestLoopWithoutBreachRunsToCompletion asserts the cap is not always-on: a run
// that stays under its budget ends on the model's own stop reason with no
// breach recorded.
func TestLoopWithoutBreachRunsToCompletion(t *testing.T) {
	cfg := budgetCfg(t, &fakeModel{prompt: "hi"})

	// The fake model spends 15 input tokens across its two turns, well under a
	// 1000 USD cap at one USD per token.
	res, err := loop.Run(context.Background(), cfg, meter(t, 1000, flatRate{}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != models.StopEndTurn {
		t.Fatalf("stop reason = %q, want %q", res.StopReason, models.StopEndTurn)
	}
	if res.BudgetBreach != nil {
		t.Fatalf("BudgetBreach = %+v on an under-budget run, want nil", *res.BudgetBreach)
	}
}

// TestLoopUnmeteredRunIsUnaffected asserts a nil meter — a run with no
// configured budget — never stops early and never records a breach.
func TestLoopUnmeteredRunIsUnaffected(t *testing.T) {
	cfg := budgetCfg(t, &fakeModel{prompt: "hi"})

	res, err := loop.Run(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != models.StopEndTurn || res.BudgetBreach != nil {
		t.Fatalf("unmetered run: stop=%q breach=%v", res.StopReason, res.BudgetBreach)
	}
}

// TestLoopStopsBeforeSpendingOnABreachedMeter asserts a run that joins a region
// whose shared cap is already reached does not take a turn at all. Without the
// pre-turn check every remaining agent in the region would spend one more turn
// past the cap before its own usage fold noticed.
func TestLoopStopsBeforeSpendingOnABreachedMeter(t *testing.T) {
	m := meter(t, 1, flatRate{})
	if _, _, err := m.OnUsage(context.Background(), models.Usage{InputTokens: 1}); err != nil {
		t.Fatalf("pre-breach the meter: %v", err)
	}

	model := &meteredModel{usage: models.Usage{InputTokens: 1}}
	res, err := loop.Run(context.Background(), budgetCfg(t, model), m)
	if !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("Run error = %v, want billing.ErrBudgetExceeded", err)
	}
	if got := model.calls.Load(); got != 0 {
		t.Fatalf("model called %d times on an already-capped region, want 0", got)
	}
	if res.StopReason != models.StopBudgetExceeded || res.BudgetBreach == nil {
		t.Fatalf("stop=%q breach=%v, want a budget stop carrying the breach", res.StopReason, res.BudgetBreach)
	}
}

// TestLoopStopsWhenATurnCannotBePriced asserts the turn-boundary backstop: a
// model the cost source cannot price leaves the cap unenforceable, so the run
// stops on its first turn with an error naming the model rather than continuing
// unmetered.
func TestLoopStopsWhenATurnCannotBePriced(t *testing.T) {
	cfg := budgetCfg(t, &meteredModel{usage: models.Usage{InputTokens: 1}})

	res, err := loop.Run(context.Background(), cfg, meter(t, 3, unpriceable{}))
	if err == nil {
		t.Fatal("Run on an unpriceable model = nil error, want a failure")
	}
	if !strings.Contains(err.Error(), "test-model") {
		t.Fatalf("error does not name the model: %v", err)
	}
	if res == nil {
		t.Fatal("Run = nil result, want the partial transcript")
	}
	if res.TotalUsage.InputTokens != 1 {
		t.Fatalf("ran %d turns, want to stop after the 1st", res.TotalUsage.InputTokens)
	}
}
