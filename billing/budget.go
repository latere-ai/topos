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
	"errors"
	"fmt"
	"sync"
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

// ErrBudgetExceeded reports that a run stopped because its accumulated cost
// reached the configured spend cap. It is returned alongside the partial result
// — the work paid for before the cap is still handed back — so a caller that
// ignores errors gets a loud signal and a caller that wants the partial output
// still has it.
//
// It is a sentinel: callers match it with errors.Is through whatever context
// each layer wraps around it.
var ErrBudgetExceeded = errors.New("billing: spend cap reached")

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

// Meter joins pricing to enforcement: it accumulates the token usage of every
// turn handed to it, prices the running total through a [CostSource], and
// evaluates the result against an [Enforcer]. It is what the agentic loop
// consults at each turn boundary, and the only place a token count becomes a
// USD figure the spend cap can be checked against.
//
// One Meter belongs to one region, not one agent. Every agent in the region —
// the entry agent, each step of a pinned chain, each delegated peer — folds its
// turns into the same Meter, so the budget bounds the region's total spend
// rather than being granted afresh to each agent. Which agent trips the cap is
// consequently not part of the contract: whichever folds the crossing turn
// first reports the breach, and under any interleaving of agents that is a
// different one. The guarantee is on the region's total, not on any particular
// agent, which is what a safety bound needs to be.
//
// A Meter is safe for concurrent use. The current dispatcher runs a region's
// agents one at a time, but the cap must not rest on that: a host that fans
// peers out in parallel, or anything that reads the meter off the run's
// goroutine, would otherwise lose a fold and let the cap be overrun. The fold,
// the pricing, the budget check, and the breach latch therefore happen inside
// one mutex rather than as separate atomic operations — the state is a
// multi-field usage total plus the Enforcer's own notification latch, and only
// a single critical section makes the whole sequence indivisible. Per-word
// atomics would still let two agents each observe an under-cap total and both
// proceed. Contention is immaterial: the lock is taken once per turn boundary.
//
// A Meter is a different axis from [DeriveChildBudget]. Sub-allocation decides
// what authority a parent may GRANT a spawned child, an attenuation of the
// permission a child carries; a Meter decides how much may be SPENT in total
// inside one region before the runtime stops it. A child can hold a grant it
// never gets to use because the region's meter tripped first, and a region
// meter is unaware of how the grant was divided.
type Meter struct {
	model string
	cost  CostSource
	enf   *Enforcer

	mu       sync.Mutex
	total    models.Usage // region-wide accumulated usage
	breached bool         // latched once the cap is reached
	breach   Breach       // the breach that latched it
}

// NewMeter returns a Meter that prices usage as model through cost and enforces
// enf's budget against the result. model is the id the region's turns are
// billed under; cost and enf must both be non-nil. The Meter takes ownership of
// enf: it serializes every call into it, so enf must not be shared with another
// Meter or called directly once handed over.
func NewMeter(model string, cost CostSource, enf *Enforcer) *Meter {
	return &Meter{model: model, cost: cost, enf: enf}
}

// OnUsage folds one turn's usage into the region's running total, prices that
// total, and reports whether it breaches the budget. A nil Meter is the
// unmetered case and never breaches, which is what a run with no configured
// spend cap passes.
//
// The argument is the turn, and the Meter holds the total. That is what makes
// the cap region-wide: a caller has no way to accidentally reset the total by
// passing only its own run's accumulation, and several callers add up.
//
// It prices the running total rather than each turn in isolation, so a
// gateway-reported cost and a rate-card estimate compose the same way: pricing
// is linear in tokens, and [models.Usage.Add] already folds reported costs into
// the total (or marks it unknown when any turn went unreported).
//
// A pricing failure is returned, not swallowed. A model the source cannot price
// leaves the cap unenforceable, and the caller stops the run rather than
// continue spending against a limit that can no longer fire.
func (m *Meter) OnUsage(ctx context.Context, turn models.Usage) (paused bool, br Breach, err error) {
	if m == nil {
		return false, Breach{}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	m.total.Add(turn)
	usd, err := m.cost.CostUSD(m.model, m.total)
	if err != nil {
		return false, Breach{}, fmt.Errorf("billing: price usage for model %q: %w", m.model, err)
	}
	// Only the USD leg is metered here. The token and wall-time legs the Budget
	// declares are wired separately; leaving them zero keeps Check from firing
	// on an axis this Meter does not measure.
	paused, br, err = m.enf.OnUsage(ctx, Usage{USD: usd})
	if paused {
		m.breached, m.breach = true, br
	}
	return paused, br, err
}

// Breached reports whether the region has already reached its cap, without
// pricing anything. It is what an agent about to take a turn asks first: once
// any agent in the region trips the cap, the others must stop before spending,
// not after their own next turn. A nil Meter is never breached.
func (m *Meter) Breached() (breached bool, br Breach) {
	if m == nil {
		return false, Breach{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.breached, m.breach
}
