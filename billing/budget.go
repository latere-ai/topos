// Package billing implements budget enforcement and the per-session cost join.
// Budgets are independent of permissions — permission says *can*, budget says
// *how much*. The model-spend ceiling is not re-implemented here: it is the
// per-session Lux virtual key's spend_cap (trust-plane), enforced by Lux and
// observed in the cost join. Topos emits its pod-time leg to Auth's metered
// usage contract; Auth owns Stripe — Topos never touches payment.
package billing

import (
	"context"
	"time"
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
		e.notified = true
		if e.notify != nil {
			if nerr := e.notify.NotifyBudgetBreach(ctx, e.sessionID, e.agentID, e.ownerSub, b); nerr != nil {
				return true, b, nerr
			}
		}
	}
	return true, b, nil
}
