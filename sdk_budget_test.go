// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
)

// costlyBrain emits a fixed usage per turn and reports no cost of its own, so
// the run is priced by whatever CostSource the runner resolved. It counts its
// turns so a test can assert how far a run got before the cap stopped it.
type costlyBrain struct {
	usage models.Usage
	turns atomic.Int32
}

func (b *costlyBrain) Stream(_ context.Context, _ models.Request) (models.Stream, error) {
	b.turns.Add(1)
	u := b.usage
	return &budgetStream{events: []models.Event{
		{Kind: models.KindTextDelta, TextDelta: "spending"},
		{Kind: models.KindUsage, Usage: &u},
		{Kind: models.KindDone, StopReason: models.StopEndTurn},
	}}, nil
}

type budgetStream struct {
	events []models.Event
	pos    int
}

func (s *budgetStream) Recv() (models.Event, error) {
	if s.pos >= len(s.events) {
		return models.Event{}, io.EOF
	}
	ev := s.events[s.pos]
	s.pos++
	return ev, nil
}

func (s *budgetStream) Close() error { return nil }

// turnInput builds a one-turn input against a throwaway local sandbox.
func turnInput(t *testing.T) TurnInput {
	t.Helper()
	p := local.New()
	sb, err := p.Create(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = p.Destroy(context.Background(), sb.ID) })
	return TurnInput{Sandbox: p, SandboxID: sb.ID, AgentID: "spender", UserPrompt: "go"}
}

// TestNewRunnerRefusesBudgetOnUnpriceableModel asserts the config-time
// fail-closed path: a budget set against a model the cost source cannot price
// is a configuration error, refused before anything runs.
func TestNewRunnerRefusesBudgetOnUnpriceableModel(t *testing.T) {
	r, err := NewRunner(Options{
		SessionID: "run-1",
		Model:     ModelOptions{Kind: ModelLux, Model: "no-such-model", APIKey: "lux_x"},
		BudgetUSD: 10,
	})
	if err == nil {
		t.Fatal("NewRunner with an unpriceable budgeted model = nil error, want a refusal")
	}
	if r != nil {
		t.Fatal("NewRunner returned a runner alongside the error; no turn may be possible")
	}
	if !strings.Contains(err.Error(), "no-such-model") {
		t.Fatalf("error does not name the model: %v", err)
	}
}

// TestNewRunnerAllowsUnpriceableModelWithoutBudget asserts the check is scoped
// to budgeted runs: with no cap to enforce, pricing is irrelevant.
func TestNewRunnerAllowsUnpriceableModelWithoutBudget(t *testing.T) {
	if _, err := NewRunner(Options{
		Model: ModelOptions{Kind: ModelLux, Model: "no-such-model", APIKey: "lux_x"},
	}); err != nil {
		t.Fatalf("NewRunner without a budget: %v", err)
	}
}

// TestRuntimeResolvedModelStopsFirstTurn asserts the backstop for what
// construction cannot check. A host-supplied brain declares no model id, so
// NewRunner has nothing to price and admits the run; the turn-boundary check
// then stops it on its first turn rather than let it run unenforced.
func TestRuntimeResolvedModelStopsFirstTurn(t *testing.T) {
	brain := &costlyBrain{usage: models.Usage{InputTokens: 1_000}}
	r, err := NewRunner(Options{SessionID: "run-1", Brain: brain, BudgetUSD: 10})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	_, err = r.Turn(context.Background(), turnInput(t))
	if err == nil {
		t.Fatal("Turn on an unpriceable runtime model = nil error, want a failure")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("error does not identify the budget as the cause: %v", err)
	}
	if got := brain.turns.Load(); got != 1 {
		t.Fatalf("model ran %d turns, want to stop after the 1st", got)
	}
}

// TestTurnStopsOnSpendCap asserts the end-to-end cap: a priced turn that
// reaches BudgetUSD ends the run on the budget stop reason, distinguishable
// from the model's own end_turn.
func TestTurnStopsOnSpendCap(t *testing.T) {
	// 1M input tokens on claude-opus-4-8 cards at $5, over a $1 cap.
	brain := &costlyBrain{usage: models.Usage{InputTokens: 1_000_000}}
	r, err := NewRunner(Options{
		SessionID: "run-1",
		Model:     ModelOptions{Kind: ModelFake, Model: "claude-opus-4-8"},
		Brain:     brain,
		BudgetUSD: 1,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	out, err := r.Turn(context.Background(), turnInput(t))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if out.StopReason != models.StopBudgetExceeded {
		t.Fatalf("stop reason = %q, want %q", out.StopReason, models.StopBudgetExceeded)
	}
}

// TestTurnUnderSpendCapCompletesNormally asserts the cap does not fire early:
// the same wiring, priced under the limit, ends on the model's own stop reason.
func TestTurnUnderSpendCapCompletesNormally(t *testing.T) {
	brain := &costlyBrain{usage: models.Usage{InputTokens: 1_000}}
	r, err := NewRunner(Options{
		SessionID: "run-1",
		Model:     ModelOptions{Kind: ModelFake, Model: "claude-opus-4-8"},
		Brain:     brain,
		BudgetUSD: 1,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	out, err := r.Turn(context.Background(), turnInput(t))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if out.StopReason != models.StopEndTurn {
		t.Fatalf("stop reason = %q, want %q", out.StopReason, models.StopEndTurn)
	}
}

// TestHostCostSourceOverridesRateCard asserts Options.CostSource replaces the
// pinned card, both for the construction-time check and for enforcement: a host
// price authority makes an otherwise uncardable model both admissible and
// enforceable.
func TestHostCostSourceOverridesRateCard(t *testing.T) {
	brain := &costlyBrain{usage: models.Usage{InputTokens: 1}}
	r, err := NewRunner(Options{
		SessionID:  "run-1",
		Model:      ModelOptions{Kind: ModelFake, Model: "house-model"},
		Brain:      brain,
		BudgetUSD:  1,
		CostSource: houseRate{usdPerInputToken: 2},
	})
	if err != nil {
		t.Fatalf("NewRunner with a host cost source: %v", err)
	}

	out, err := r.Turn(context.Background(), turnInput(t))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if out.StopReason != models.StopBudgetExceeded {
		t.Fatalf("stop reason = %q, want %q (host price should breach the $1 cap)",
			out.StopReason, models.StopBudgetExceeded)
	}
}

// houseRate is a host's own price authority: a flat per-input-token rate that
// covers every model id.
type houseRate struct{ usdPerInputToken float64 }

func (h houseRate) CostUSD(_ string, u models.Usage) (float64, error) {
	return float64(u.InputTokens) * h.usdPerInputToken, nil
}

// TestFakeModelReportsZeroCost asserts the zero-config path stays runnable
// under a budget: the fake reaches no provider, reports a real zero, and that
// reported zero is preferred over any rate card, so no card entry is needed.
func TestFakeModelReportsZeroCost(t *testing.T) {
	r, err := NewRunner(Options{
		SessionID: "run-1",
		Model:     ModelOptions{Kind: ModelFake},
		BudgetUSD: 0.000001,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	out, err := r.Turn(context.Background(), turnInput(t))
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if out.StopReason == models.StopBudgetExceeded {
		t.Fatal("a zero-cost fake turn breached a spend cap")
	}
	if out.Usage.CostUSDMicro == nil || *out.Usage.CostUSDMicro != 0 {
		t.Fatalf("fake usage cost = %v, want a reported zero", out.Usage.CostUSDMicro)
	}
}
