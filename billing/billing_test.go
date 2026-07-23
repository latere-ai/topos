// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package billing_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/topos/billing"
	"latere.ai/x/topos/models"
)

func TestBudgetCheckAxes(t *testing.T) {
	b := billing.Budget{LimitUSD: 10, LimitTokens: 1000, LimitWallTime: time.Minute}
	if breached, _ := b.Check(billing.Usage{USD: 5, Tokens: 500, WallTime: 30 * time.Second}); breached {
		t.Fatal("under budget should not breach")
	}
	if breached, br := b.Check(billing.Usage{USD: 10}); !breached || br.Leg != "usd" {
		t.Fatalf("usd breach not detected: %v %+v", breached, br)
	}
	if breached, br := b.Check(billing.Usage{Tokens: 1000}); !breached || br.Leg != "tokens" {
		t.Fatalf("tokens breach not detected: %+v", br)
	}
	if breached, br := b.Check(billing.Usage{WallTime: time.Hour}); !breached || br.Leg != "wall_time" {
		t.Fatalf("wall_time breach not detected: %+v", br)
	}
}

func TestBudgetNoLimitsNeverBreach(t *testing.T) {
	var b billing.Budget // all zero = unlimited
	if breached, _ := b.Check(billing.Usage{USD: 1e9, Tokens: 1e9, WallTime: time.Hour}); breached {
		t.Fatal("zero budget = unlimited; must not breach")
	}
}

// recordingNotifier counts breach notifications.
type recordingNotifier struct{ count atomic.Int32 }

func (n *recordingNotifier) NotifyBudgetBreach(_ context.Context, _, _, _ string, _ billing.Breach) error {
	n.count.Add(1)
	return nil
}

func TestEnforcerPausesAndNotifiesOnce(t *testing.T) {
	notifier := &recordingNotifier{}
	e := billing.NewEnforcer(billing.Budget{LimitUSD: 10}, "sess_1", "a1", "alice", notifier)
	ctx := context.Background()

	if paused, _, _ := e.OnUsage(ctx, billing.Usage{USD: 5}); paused {
		t.Fatal("under budget should not pause")
	}
	paused, br, _ := e.OnUsage(ctx, billing.Usage{USD: 12})
	if !paused || br.Leg != "usd" {
		t.Fatalf("breach should pause: paused=%v br=%+v", paused, br)
	}
	// Staying over budget keeps paused but does not re-notify.
	_, _, _ = e.OnUsage(ctx, billing.Usage{USD: 15})
	if notifier.count.Load() != 1 {
		t.Fatalf("notify count = %d, want exactly 1", notifier.count.Load())
	}
}

// flakyNotifier fails the first n deliveries, then succeeds.
type flakyNotifier struct {
	count   atomic.Int32
	failFor int32
	failErr error
}

func (n *flakyNotifier) NotifyBudgetBreach(_ context.Context, _, _, _ string, _ billing.Breach) error {
	c := n.count.Add(1)
	if c <= n.failFor {
		return n.failErr
	}
	return nil
}

// TestEnforcerRetriesNotifyAfterTransientFailure confirms a failed breach
// notification is retried on the next OnUsage rather than permanently swallowed.
func TestEnforcerRetriesNotifyAfterTransientFailure(t *testing.T) {
	notifier := &flakyNotifier{failFor: 1, failErr: errors.New("inbox unreachable")}
	e := billing.NewEnforcer(billing.Budget{LimitUSD: 10}, "sess_1", "a1", "alice", notifier)
	ctx := context.Background()

	// First breach: notify fails, so OnUsage surfaces the error but stays paused.
	paused, _, err := e.OnUsage(ctx, billing.Usage{USD: 12})
	if !paused || err == nil {
		t.Fatalf("first breach: paused=%v err=%v, want paused with error", paused, err)
	}
	// Next OnUsage must retry the notification (it was never marked delivered).
	if _, _, err := e.OnUsage(ctx, billing.Usage{USD: 13}); err != nil {
		t.Fatalf("retry should succeed, got %v", err)
	}
	// Now delivered: further breaches must not re-notify.
	if _, _, err := e.OnUsage(ctx, billing.Usage{USD: 14}); err != nil {
		t.Fatalf("after delivery: %v", err)
	}
	if got := notifier.count.Load(); got != 2 {
		t.Fatalf("notify count = %d, want 2 (one failed attempt + one successful retry)", got)
	}
}

// TestRateCardCacheTokensPriceAtTheirOwnMultiples asserts the card is not flat:
// the same token count priced as a cache read, a cache write, and plain input
// yields three different figures. Agentic loops are cache-heavy, so a flat
// table would misprice every real run.
func TestRateCardCacheTokensPriceAtTheirOwnMultiples(t *testing.T) {
	card := billing.DefaultRateCard()
	const model = "claude-opus-4-8"
	const n = 1_000_000

	plain, err := card.CostUSD(model, models.Usage{InputTokens: n})
	if err != nil {
		t.Fatalf("price plain input: %v", err)
	}
	read, err := card.CostUSD(model, models.Usage{CacheReadTokens: n})
	if err != nil {
		t.Fatalf("price cache reads: %v", err)
	}
	write, err := card.CostUSD(model, models.Usage{CacheWriteTokens: n})
	if err != nil {
		t.Fatalf("price cache writes: %v", err)
	}

	if !nearly(read, plain*0.1) {
		t.Errorf("cache read = %g, want %g (0.1x input)", read, plain*0.1)
	}
	if !nearly(write, plain*1.25) {
		t.Errorf("cache write = %g, want %g (1.25x input)", write, plain*1.25)
	}
	if read >= plain || write <= plain {
		t.Errorf("card prices cache tokens flat: plain=%g read=%g write=%g", plain, read, write)
	}
}

// TestRateCardPricesOutputSeparately asserts output tokens bill at the output
// rate, not the input one, and that a mixed usage sums the legs.
func TestRateCardPricesOutputSeparately(t *testing.T) {
	card := billing.DefaultRateCard()
	// claude-opus-4-8: $5/MTok input, $25/MTok output.
	got, err := card.CostUSD("claude-opus-4-8", models.Usage{
		InputTokens: 200_000, OutputTokens: 100_000, CacheReadTokens: 1_000_000,
	})
	if err != nil {
		t.Fatalf("CostUSD: %v", err)
	}
	want := 1.0 + 2.5 + 0.5
	if !nearly(got, want) {
		t.Fatalf("CostUSD = %g, want %g", got, want)
	}
}

// TestRateCardUnknownModelFailsClosed asserts an uncovered model is an error
// rather than a free turn, and that the error names the model.
func TestRateCardUnknownModelFailsClosed(t *testing.T) {
	_, err := billing.DefaultRateCard().CostUSD("no-such-model", models.Usage{InputTokens: 1})
	if err == nil {
		t.Fatal("CostUSD on an uncovered model = nil error, want a failure")
	}
	if !strings.Contains(err.Error(), "no-such-model") {
		t.Fatalf("error does not name the model: %v", err)
	}
}

// TestRateCardCopyIsIndependent asserts each DefaultRateCard call returns a
// fresh map, so a host amending its copy cannot mutate the runtime's.
func TestRateCardCopyIsIndependent(t *testing.T) {
	amended := billing.DefaultRateCard()
	amended["house-model"] = billing.Rate{InputPerMTok: 1, OutputPerMTok: 1}
	if _, ok := billing.DefaultRateCard()["house-model"]; ok {
		t.Fatal("amending a card mutated the pinned table")
	}
}

// TestGatewayFirstPrefersReportedCost asserts a gateway-reported figure wins
// over the rate card, and that a reported zero is a real cost rather than an
// unreported one.
func TestGatewayFirstPrefersReportedCost(t *testing.T) {
	src := billing.DefaultCostSource()
	// 1M input tokens on claude-opus-4-8 cards at $5; the gateway says $2.
	reported := int64(2_000_000)
	got, err := src.CostUSD("claude-opus-4-8", models.Usage{InputTokens: 1_000_000, CostUSDMicro: &reported})
	if err != nil {
		t.Fatalf("CostUSD: %v", err)
	}
	if !nearly(got, 2) {
		t.Fatalf("CostUSD = %g, want the reported 2 (not the carded 5)", got)
	}

	// A reported zero is honored, so an uncardable model still prices.
	zero := int64(0)
	got, err = src.CostUSD("no-such-model", models.Usage{InputTokens: 1_000_000, CostUSDMicro: &zero})
	if err != nil {
		t.Fatalf("reported zero on an uncarded model: %v", err)
	}
	if got != 0 {
		t.Fatalf("CostUSD = %g, want 0", got)
	}
}

// TestGatewayFirstTreatsUnknownCostAsUnreported asserts both unknown states —
// nil, and the gateway's negative cannot-price sentinel — fall through to the
// rate card. Reading -1 as a cost would under-count by the whole turn and
// silently defeat the cap.
func TestGatewayFirstTreatsUnknownCostAsUnreported(t *testing.T) {
	src := billing.DefaultCostSource()
	usage := models.Usage{InputTokens: 1_000_000}

	carded, err := src.CostUSD("claude-opus-4-8", usage)
	if err != nil {
		t.Fatalf("nil cost: %v", err)
	}
	if !nearly(carded, 5) {
		t.Fatalf("nil cost priced at %g, want the carded 5", carded)
	}

	sentinel := int64(-1)
	usage.CostUSDMicro = &sentinel
	got, err := src.CostUSD("claude-opus-4-8", usage)
	if err != nil {
		t.Fatalf("sentinel cost: %v", err)
	}
	if !nearly(got, 5) {
		t.Fatalf("sentinel cost priced at %g, want the carded 5", got)
	}

	// With no card behind it, an unknown cost is an error, not a free turn.
	if _, err := (billing.GatewayFirst{}).CostUSD("claude-opus-4-8", usage); err == nil {
		t.Fatal("unknown cost with no fallback = nil error, want a failure")
	}
}

// TestMeterBreachesOnPricedUsage asserts the Meter turns token usage into USD
// and reports a breach only once the cap is reached.
func TestMeterBreachesOnPricedUsage(t *testing.T) {
	m := billing.NewMeter(
		"claude-opus-4-8",
		billing.DefaultCostSource(),
		billing.NewEnforcer(billing.Budget{LimitUSD: 1}, "s", "a", "o", nil),
	)

	// 100k input tokens on claude-opus-4-8 = $0.50, under the cap.
	paused, _, err := m.OnUsage(context.Background(), models.Usage{InputTokens: 100_000})
	if err != nil {
		t.Fatalf("OnUsage: %v", err)
	}
	if paused {
		t.Fatal("paused at $0.50 against a $1 cap")
	}

	// 200k input tokens = $1.00, at the cap.
	paused, br, err := m.OnUsage(context.Background(), models.Usage{InputTokens: 200_000})
	if err != nil {
		t.Fatalf("OnUsage: %v", err)
	}
	if !paused {
		t.Fatal("not paused at $1.00 against a $1 cap")
	}
	if br.Leg != "usd" {
		t.Fatalf("breach leg = %q, want usd", br.Leg)
	}
}

// TestMeterPricingFailureIsReturned asserts an unpriceable model surfaces as an
// error rather than a zero cost, so the caller can fail closed.
func TestMeterPricingFailureIsReturned(t *testing.T) {
	m := billing.NewMeter(
		"no-such-model",
		billing.DefaultCostSource(),
		billing.NewEnforcer(billing.Budget{LimitUSD: 1}, "s", "a", "o", nil),
	)
	_, _, err := m.OnUsage(context.Background(), models.Usage{InputTokens: 1})
	if err == nil {
		t.Fatal("OnUsage on an unpriceable model = nil error, want a failure")
	}
	if !strings.Contains(err.Error(), "no-such-model") {
		t.Fatalf("error does not name the model: %v", err)
	}
}

// TestNilMeterNeverBreaches asserts the unmetered case — a run with no
// configured spend cap — passes through without pricing anything.
func TestNilMeterNeverBreaches(t *testing.T) {
	var m *billing.Meter
	paused, _, err := m.OnUsage(context.Background(), models.Usage{InputTokens: 1_000_000_000})
	if err != nil || paused {
		t.Fatalf("nil meter: paused=%v err=%v, want false/nil", paused, err)
	}
}

// nearly compares two USD figures within float rounding.
func nearly(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
