// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

// The layers that decide a tool call, recorded as an agent.tool_use's
// decided_by (spec 012, "Which layer decided"). The set is closed: a
// client that meets a value it does not know shows the reason alone.
const (
	// DecidedNetwork: the session's network decided a web_fetch (spec 052).
	DecidedNetwork = "network"
	// DecidedAlwaysConfirm: a pattern of the organization's always-confirm
	// list matched, so the call asks.
	DecidedAlwaysConfirm = "always_confirm"
	// DecidedTool: the call's tool holds it for a person whatever the
	// lists, the score or a decision service say.
	DecidedTool = "tool"
	// DecidedPlan: plan mode blocked a call that changes something.
	DecidedPlan = "plan"
	// DecidedReadOnly: the call reads and changes nothing, so it runs.
	DecidedReadOnly = "read_only"
	// DecidedAlwaysAllow: a pattern of the always-allow list matched.
	DecidedAlwaysAllow = "always_allow"
	// DecidedRemembered: a pattern the person allowed earlier in this
	// session matched.
	DecidedRemembered = "remembered"
	// DecidedSandbox: in confirm mode, a write that stays inside a Cella
	// sandbox runs.
	DecidedSandbox = "sandbox"
	// DecidedScore: in progressive mode, the rule-based risk score and its
	// thresholds decided.
	DecidedScore = "score"
	// DecidedConfirm: in confirm mode, a call no other rule settles asks.
	DecidedConfirm = "confirm"
	// DecidedMemory: a call that reaches memory folders alone runs, since
	// the person reviews every memory (spec 093).
	DecidedMemory = "memory"
	// DecidedQuestion: a question tool's call is itself put to a person and
	// is never held for a confirmation.
	DecidedQuestion = "question"
	// DecidedLearned: a decision service's suggestion decided the call, in
	// progressive mode and outside shadow (specs 037 and 099).
	DecidedLearned = "learned"
	// DecidedFallback: the decision service did not answer, so the call
	// waits for a person.
	DecidedFallback = "fallback"
	// DecidedDenials: after repeated refusals, the session asks the person
	// for every call that changes something until their next message
	// (spec 101).
	DecidedDenials = "denials"
)

// Decided lists every value of decided_by, in the order the rules apply.
var Decided = []string{
	DecidedNetwork, DecidedAlwaysConfirm, DecidedTool, DecidedPlan, DecidedReadOnly,
	DecidedAlwaysAllow, DecidedRemembered, DecidedSandbox, DecidedScore, DecidedConfirm,
	DecidedMemory, DecidedQuestion, DecidedLearned, DecidedFallback, DecidedDenials,
}
