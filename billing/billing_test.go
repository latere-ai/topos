// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package billing_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/topos/billing"
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
