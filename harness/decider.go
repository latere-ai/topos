// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json/jsontext"
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
	Input     jsontext.Value
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
	// Reach is Topos's own claims about what the call reaches (spec 101),
	// filled from what the harness knows without reading the input as
	// text; Holds are the limits the call's machine enforces whatever the
	// call does, of HoldEgress, HoldProtectedRefs and HoldCheckpoints; and
	// Repositories is the visibility of the session's repositories,
	// RepositoriesPrivate, RepositoriesPublic or RepositoriesMixed, "" for
	// a session with none or one whose visibility is not known.
	Reach        tools.Reach
	Holds        []string
	Repositories string
	// Depth is the subagent depth of the thread that made the call: 0 for
	// the session's own conversation, 1 for a subagent it started.
	Depth int
	// Handoff is what an agent or send_message call hands its subagent,
	// nil for every other call.
	Handoff *Handoff
	// Fallback is set while the session asks for every call that changes
	// something, after repeated refusals (spec 101): a decider applies
	// InFallback to its decision.
	Fallback bool
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
// shown verdict has review probability 1 and any other 0. The decision
// records the call's scope from Topos's own claims (spec 101), and in the
// fallback of repeated refusals a change asks.
func (Rules) Decide(_ context.Context, c Call) (session.Risk, Decision, error) {
	named := func(host string) bool { return slices.Contains(c.Policy.Egress, host) }
	if n := c.Policy.Network; n != nil {
		named = n.Inside
	}
	scope := ClaimedScope(c.Reach)
	if c.Memory {
		d := held(c.Policy.decideMemory(c.Name, c.Props, c.Input, c.MachineKind), c.Confirm, c.Name)
		_, d.ReviewProbability = verdict.Decide(d.Verdict, VerdictAllow, 0, 0)
		d.Scope = &scope
		return memoryRisk(), d, nil
	}
	risk := score(c.Name, c.Props, c.Input, c.MachineKind, named)
	d := held(c.Policy.Decide(c.Name, c.Props, c.Input, risk, c.MachineKind, c.Remembered), c.Confirm, c.Name)
	_, d.ReviewProbability = verdict.Decide(d.Verdict, VerdictAllow, 0, 0)
	d.Scope = &scope
	return risk, InFallback(c, d), nil
}

// held is d for a call its tool holds for a person, why: a verdict that
// would run the call without one becomes an ask with that reason, and an
// ask or a block stays as it is. An empty why holds nothing.
func held(d Decision, why, tool string) Decision {
	if why == "" || Stricter(d.Verdict, VerdictAsk) == d.Verdict {
		return d
	}
	return Decision{Verdict: VerdictAsk, Reason: why, ReviewProbability: 1, Suggestion: d.Suggestion, DecidedBy: session.DecidedTool, Rule: tool, Scope: d.Scope}
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
		d := held(p.decideMemory(c.Name, c.Props, c.Input, c.MachineKind), c.Confirm, c.Name)
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
	if rule, ok := matchPattern(p.AlwaysConfirm, c.Name, subject); ok {
		return Decision{Verdict: VerdictAsk, Reason: "on the organization's always-confirm list", ReviewProbability: 1, DecidedBy: session.DecidedAlwaysConfirm, Rule: rule}, true
	}
	switch {
	case c.Confirm != "":
		return Decision{Verdict: VerdictAsk, Reason: c.Confirm, ReviewProbability: 1, DecidedBy: session.DecidedTool, Rule: c.Name}, true
	case risk.Score >= t.BlockAt:
		return Decision{Verdict: VerdictBlock, Reason: prompts.Text(prompts.CallAboveBlock), DecidedBy: session.DecidedScore, Rule: RuleBlockAt}, true
	case c.Props.Effect == tools.EffectNone || c.Props.Effect == tools.EffectRead:
		return Decision{Verdict: VerdictAllow, Reason: "read-only", DecidedBy: session.DecidedReadOnly}, true
	}
	if rule, ok := matchPattern(p.AlwaysAllow, c.Name, subject); ok {
		return Decision{Verdict: VerdictAllow, Reason: "on the always-allow list", DecidedBy: session.DecidedAlwaysAllow, Rule: rule}, true
	}
	if rule, ok := matchPattern(c.Remembered, c.Name, subject); ok {
		return Decision{Verdict: VerdictAllow, Reason: "allowed earlier in this session", DecidedBy: session.DecidedRemembered, Rule: rule}, true
	}
	return Decision{}, false
}

// Learner is a Decider that also learns from the person's answers to the
// calls it decided (spec 037). Resume tells it each confirmation it
// settles, of an asked call or of a network approval, which answers the
// call whose connection raised it, with how the person answered and how
// long the call waited (spec 101); an error is not the session's, and the
// call goes on as it would.
type Learner interface {
	Answered(ctx context.Context, s session.Session, toolUseID string, a Answer) error
}
