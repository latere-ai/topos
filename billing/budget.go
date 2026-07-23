// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package billing implements per-session budget enforcement for the SDK.
// Budgets are independent of permissions — permission says *can*, budget says
// *how much*. The model-spend ceiling is not re-implemented here: it is the
// per-session Lux virtual key's spend_cap, enforced by Lux and observed by the
// host's cost accounting.
package billing

import (
	"context"
	"fmt"
	"time"

	"latere.ai/x/topos/models"
)

// Budget is a per-agent / per-session spend envelope. A zero field means "no
// limit on that axis".
type Budget struct {
	LimitUSD      float64
	LimitTokens   int
	LimitWallTime time.Duration
}

// Usage is accumulated consumption for a session.
type Usage struct {
	USD      float64
	Tokens   int
	WallTime time.Duration
}

// Breach describes which budget axis was exceeded.
type Breach struct {
	Leg    string  // "usd", "tokens", or "wall_time"
	Limit  float64 // the configured limit (seconds for wall_time)
	Actual float64 // the observed value
}

// Check reports the first budget axis the usage breaches, or breached=false.
func (b Budget) Check(u Usage) (breached bool, br Breach) {
	if b.LimitUSD > 0 && u.USD >= b.LimitUSD {
		return true, Breach{Leg: "usd", Limit: b.LimitUSD, Actual: u.USD}
	}
	if b.LimitTokens > 0 && u.Tokens >= b.LimitTokens {
		return true, Breach{Leg: "tokens", Limit: float64(b.LimitTokens), Actual: float64(u.Tokens)}
	}
	if b.LimitWallTime > 0 && u.WallTime >= b.LimitWallTime {
		return true, Breach{Leg: "wall_time", Limit: b.LimitWallTime.Seconds(), Actual: u.WallTime.Seconds()}
	}
	return false, Breach{}
}

// Notifier is informed when a session is paused on a budget breach (the
// escalation inbox in production; a fake in tests).
type Notifier interface {
	NotifyBudgetBreach(ctx context.Context, sessionID, agentID, ownerSub string, br Breach) error
}

// Enforcer pauses a session when its usage breaches the budget and notifies the
// owner. It is independent of the permission system.
type Enforcer struct {
	budget    Budget
	notify    Notifier
	sessionID string
	agentID   string
	ownerSub  string
	notified  bool // guards single notification on breach
}

// NewEnforcer returns a budget Enforcer for one session. notify may be nil.
func NewEnforcer(budget Budget, sessionID, agentID, ownerSub string, notify Notifier) *Enforcer {
	return &Enforcer{budget: budget, notify: notify, sessionID: sessionID, agentID: agentID, ownerSub: ownerSub}
}

// OnUsage evaluates accumulated usage. On breach it notifies the owner (once)
// and returns paused=true so the runtime halts the session cleanly. Subsequent
// calls after a breach stay paused without re-notifying.
func (e *Enforcer) OnUsage(ctx context.Context, u Usage) (paused bool, br Breach, err error) {
	breached, b := e.budget.Check(u)
	if !breached {
		return false, Breach{}, nil
	}
	if !e.notified {
		// Mark notified only after a successful delivery, so a transient notify
		// failure is retried on the next OnUsage rather than permanently
		// swallowing the breach notification while the session stays paused.
		if e.notify != nil {
			if nerr := e.notify.NotifyBudgetBreach(ctx, e.sessionID, e.agentID, e.ownerSub, b); nerr != nil {
				return true, b, nerr
			}
		}
		e.notified = true
	}
	return true, b, nil
}

// Meter joins pricing to enforcement: it prices a run's accumulated token usage
// through a [CostSource] and evaluates the result against an [Enforcer]. It is
// what the agentic loop consults at each turn boundary, and the only place a
// token count becomes a USD figure the spend cap can be checked against.
//
// One Meter belongs to one run. The Enforcer it wraps latches on breach, so
// agents running concurrently must not share one.
type Meter struct {
	model string
	cost  CostSource
	enf   *Enforcer
}

// NewMeter returns a Meter that prices usage as model through cost and enforces
// enf's budget against the result. model is the id the run's turns are billed
// under; cost and enf must both be non-nil.
func NewMeter(model string, cost CostSource, enf *Enforcer) *Meter {
	return &Meter{model: model, cost: cost, enf: enf}
}

// OnUsage prices the accumulated usage and reports whether it breaches the
// budget. A nil Meter is the unmetered case and never breaches, which is what a
// run with no configured spend cap passes.
//
// It prices the running total rather than the turn, so a gateway-reported cost
// and a rate-card estimate compose the same way: pricing is linear in tokens,
// and [models.Usage.Add] already folds reported costs into the total (or marks
// it unknown when any turn went unreported).
//
// A pricing failure is returned, not swallowed. A model the source cannot price
// leaves the cap unenforceable, and the caller stops the run rather than
// continue spending against a limit that can no longer fire.
func (m *Meter) OnUsage(ctx context.Context, total models.Usage) (paused bool, br Breach, err error) {
	if m == nil {
		return false, Breach{}, nil
	}
	usd, err := m.cost.CostUSD(m.model, total)
	if err != nil {
		return false, Breach{}, fmt.Errorf("billing: price usage for model %q: %w", m.model, err)
	}
	// Only the USD leg is metered here. The token and wall-time legs the Budget
	// declares are wired separately; leaving them zero keeps Check from firing
	// on an axis this Meter does not measure.
	return m.enf.OnUsage(ctx, Usage{USD: usd})
}
