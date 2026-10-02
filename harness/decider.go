// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"

	"latere.ai/x/pkg/verdict"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// Call is one validated tool call the harness decides (spec 037).
type Call struct {
	// Policy is the policy in force for the call: the session's, narrowed
	// for a subagent's thread.
	Policy    Policy
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
// the call's policy, its mode and its lists. A harness with no
// Config.Decider uses it.
type Rules struct{}

// Decide implements [Decider]. The rules review nothing at random, so a
// shown verdict has review probability 1 and any other 0.
func (Rules) Decide(_ context.Context, c Call) (session.Risk, Decision, error) {
	risk := Score(c.Name, c.Props, c.Input, c.MachineKind, c.Policy.Egress)
	d := c.Policy.Decide(c.Name, c.Props, c.Input, risk, c.MachineKind, c.Remembered)
	_, d.ReviewProbability = verdict.Decide(d.Verdict, VerdictAllow, 0, 0)
	return risk, d, nil
}

// decider is the configured decider, or the rules.
func (h *Harness) decider() Decider {
	if h.c.Decider != nil {
		return h.c.Decider
	}
	return Rules{}
}

// Settle reports whether the rules decide the call whatever a decision
// service would suggest (spec 037), and their decision when they do: a
// call on always_confirm asks, a score at or above block_at is blocked,
// and a read-only call, or one an always_allow or remembered pattern
// matches, is allowed. Every other call is open to the service, under a
// ceiling of allow.
func (p Policy) Settle(c Call, risk session.Risk) (Decision, bool) {
	subject := patternSubject(c.Name, c.Input)
	t := p.Thresholds
	if t == (Thresholds{}) {
		t = DefaultThresholds
	}
	switch {
	case matchAny(p.AlwaysConfirm, c.Name, subject):
		return Decision{Verdict: VerdictAsk, Reason: "on the organization's always-confirm list", ReviewProbability: 1}, true
	case risk.Score >= t.BlockAt:
		return Decision{Verdict: VerdictBlock, Reason: prompts.Text(prompts.CallAboveBlock)}, true
	case c.Props.Effect == tools.EffectNone || c.Props.Effect == tools.EffectRead:
		return Decision{Verdict: VerdictAllow, Reason: "read-only"}, true
	case matchAny(p.AlwaysAllow, c.Name, subject):
		return Decision{Verdict: VerdictAllow, Reason: "on the always-allow list"}, true
	case matchAny(c.Remembered, c.Name, subject):
		return Decision{Verdict: VerdictAllow, Reason: "allowed earlier in this session"}, true
	}
	return Decision{}, false
}

// Learner is a Decider that also learns from the person's answers to the
// calls it decided (spec 037). Resume tells it each confirmation it
// settles; an error is not the session's, and the call goes on as it would.
type Learner interface {
	Answered(ctx context.Context, s session.Session, toolUseID string, approve bool, by string) error
}
