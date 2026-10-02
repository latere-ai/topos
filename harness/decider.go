// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

// Call is one validated tool call the harness decides (spec 037).
type Call struct {
	Session   session.Session
	ToolUseID string
	Name      string
	Props     tools.Properties
	Input     json.RawMessage
	// MachineKind is the kind of the machine the call runs on.
	MachineKind string
	// Remembered are the session's own allow patterns, from confirmations
	// answered with remember.
	Remembered []string
}

// Decider decides a validated call: its risk score and its verdict. The
// harness asks it for every call it plans. An error fails the step, so a
// decider that wants to fail closed returns a decision instead.
type Decider interface {
	Decide(ctx context.Context, c Call) (session.Risk, Decision, error)
}

// Rules is the built-in decider of spec 012: the rule-feature score and
// the policy's mode and lists. A harness with no Config.Decider uses it
// over its Config.Policy.
type Rules struct {
	Policy Policy
}

// Decide implements [Decider].
func (r Rules) Decide(_ context.Context, c Call) (session.Risk, Decision, error) {
	risk := Score(c.Name, c.Props, c.Input, c.MachineKind, r.Policy.Egress)
	return risk, r.Policy.Decide(c.Name, c.Props, c.Input, risk, c.MachineKind, c.Remembered), nil
}

// decider is the configured decider, or the rules over the policy.
func (h *Harness) decider() Decider {
	if h.c.Decider != nil {
		return h.c.Decider
	}
	return Rules{Policy: h.c.Policy}
}
