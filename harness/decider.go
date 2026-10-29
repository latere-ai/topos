// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"slices"

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
	// Confirm is why the call's tool holds it for a person whatever the
	// lists, the patterns and the mode's score allow (tools.Confirmer);
	// empty for a call decided as any other.
	Confirm string
	// Attended is false on a turn a trigger's message began, which nobody
	// watches (spec 087); its Remembered is then empty. A decision
	// service is told it, to weigh a call nobody sees.
	Attended bool
	// Memory is true for a call that runs on the session's memory machine
	// (spec 093), every path of which is under /memory/: it scores with
	// the feature memory in place of its tool's own, and the rules settle
	// it.
	Memory bool
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
	named := func(host string) bool { return slices.Contains(c.Policy.Egress, host) }
	if n := c.Policy.Network; n != nil {
		named = n.Inside
	}
	if c.Memory {
		d := held(c.Policy.decideMemory(c.Name, c.Props, c.Input, c.MachineKind), c.Confirm)
		_, d.ReviewProbability = verdict.Decide(d.Verdict, VerdictAllow, 0, 0)
		return memoryRisk(), d, nil
	}
	risk := score(c.Name, c.Props, c.Input, c.MachineKind, named)
	d := held(c.Policy.Decide(c.Name, c.Props, c.Input, risk, c.MachineKind, c.Remembered), c.Confirm)
	_, d.ReviewProbability = verdict.Decide(d.Verdict, VerdictAllow, 0, 0)
	return risk, d, nil
}

// held is d for a call its tool holds for a person, why: a verdict that
// would run the call without one becomes an ask with that reason, and an
// ask or a block stays as it is. An empty why holds nothing.
func held(d Decision, why string) Decision {
	if why == "" || Stricter(d.Verdict, VerdictAsk) == d.Verdict {
		return d
	}
	return Decision{Verdict: VerdictAsk, Reason: why, ReviewProbability: 1, Suggestion: d.Suggestion}
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
// fetch outside the session's network asks or is blocked as the network
// says, a call on always_confirm and one its tool holds for a person ask,
// a score at or above block_at is blocked, and a read-only call, or one an
// always_allow or remembered pattern matches, is allowed. Every other call
// is open to the service, under a ceiling of allow.
//
// A call that reaches memory folders alone is the rules' to decide (spec
// 093): the person reviews every memory through the installation.
func (p Policy) Settle(c Call, risk session.Risk) (Decision, bool) {
	if c.Memory {
		d := held(p.decideMemory(c.Name, c.Props, c.Input, c.MachineKind), c.Confirm)
		if d.Verdict == VerdictAsk {
			d.ReviewProbability = 1
		}
		return d, true
	}
	// A fetch outside the session's network is the network's to decide,
	// which no service suggests past (spec 052).
	if d, ok := p.fetchDecision(c.Name, c.Input); ok && d.Verdict != VerdictAllow {
		if d.Verdict == VerdictAsk {
			d.ReviewProbability = 1
		}
		return d, true
	}
	subject := patternSubject(c.Name, c.Input)
	t := p.Thresholds
	if t == (Thresholds{}) {
		t = DefaultThresholds
	}
	switch {
	case matchAny(p.AlwaysConfirm, c.Name, subject):
		return Decision{Verdict: VerdictAsk, Reason: "on the organization's always-confirm list", ReviewProbability: 1}, true
	case c.Confirm != "":
		return Decision{Verdict: VerdictAsk, Reason: c.Confirm, ReviewProbability: 1}, true
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
