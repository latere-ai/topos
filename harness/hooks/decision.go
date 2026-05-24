package hooks

import (
	"encoding/json"

	"latere.ai/x/agents/internal/models"
)

// Verdict is the outcome of a hook decision.
type Verdict string

const (
	// VerdictAllow means the operation is permitted by this consumer.
	VerdictAllow Verdict = "allow"
	// VerdictDeny means the operation is denied. A deny from any consumer
	// (or from a policy deny-rule) is final and cannot be overridden.
	VerdictDeny Verdict = "deny"
	// VerdictModify means the consumer wants to allow the operation but with
	// a modified payload (e.g. rewritten tool input).
	VerdictModify Verdict = "modify"
)

// Decision is the result of a consumer or policy evaluation.
type Decision struct {
	// Verdict is the consumer's verdict.
	Verdict Verdict
	// Reason is a human-readable explanation for the decision (optional).
	Reason string
	// ModifiedPayload is the replacement payload when Verdict == VerdictModify.
	// Nil means "no modification".
	ModifiedPayload any
}

// Allow returns a Decision that permits the operation.
func Allow() Decision { return Decision{Verdict: VerdictAllow} }

// Deny returns a Decision that rejects the operation with a reason.
func Deny(reason string) Decision { return Decision{Verdict: VerdictDeny, Reason: reason} }

// Modify returns a Decision that permits the operation with a modified payload.
func Modify(payload any) Decision { return Decision{Verdict: VerdictModify, ModifiedPayload: payload} }

// DenyRule is a static policy rule that denies a tool call unconditionally
// when its predicate matches. Unlike hook consumers, deny-rules are checked
// independently and CANNOT be overridden by a hook allow.
//
// Invariant (spec: harness-hook-bus.md):
//
//	net_allow = hook_allow AND NOT deny_rule_matched
//
// A compromised or misbehaving consumer cannot widen permissions.
type DenyRule struct {
	// Name is a human-readable label for the rule.
	Name string
	// Predicate returns true when the rule applies to the given tool name and
	// raw input. The predicate MUST NOT mutate the input.
	Predicate func(toolName string, input json.RawMessage) bool
}

// ToolPath is the three-phase decision engine for tool calls:
//
//  1. Validate + normalise input (backfill).
//  2. Permission resolution: run hook consumers (may allow/deny/modify)
//     AND policy deny-rules. Both must pass.
//  3. Execute + post-hooks (caller's responsibility after ToolPath returns
//     a net-allow).
//
// ToolPath is stateless and safe for concurrent use.
type ToolPath struct {
	bus       *Bus
	denyRules []DenyRule
}

// NewToolPath constructs a ToolPath backed by the given Bus. denyRules is the
// static policy; nil means no rules (tools open in trusted sandbox).
func NewToolPath(bus *Bus, denyRules []DenyRule) *ToolPath {
	return &ToolPath{bus: bus, denyRules: denyRules}
}

// PhaseResult is the outcome of ToolPath.Resolve.
type PhaseResult struct {
	// Allowed is true when the net decision is to execute the tool.
	Allowed bool
	// DeniedBy names the consumer or rule that issued the deny (empty on allow).
	DeniedBy string
	// Reason is the human-readable denial reason (empty on allow).
	Reason string
	// ModifiedInput is the normalised/modified input to pass to the tool
	// executor. Always non-nil on allow; equals the original if no modification
	// was requested.
	ModifiedInput json.RawMessage
}

// Resolve runs the two-phase permission check (hook consumers + deny-rules)
// for a tool call and returns the net PhaseResult.
//
// Phase 1: hook consumers via bus.Dispatch(EventPreToolUse, ...).
// Phase 2: deny-rules (independent; cannot be overridden by hook allow).
//
// The caller is responsible for:
//   - Phase 3 execution (tool invoke) if Allowed.
//   - Dispatching EventPostToolUse / EventPostToolUseFailure after execution.
func (tp *ToolPath) Resolve(sessionID string, call models.ToolCall) PhaseResult {
	normalised := call.Input
	if normalised == nil {
		normalised = json.RawMessage("{}")
	}

	// Carry the full call identity (ID + name + normalised input) on the
	// PreToolUse event so the durable event log can pair a tool-use with its
	// result on replay — including detecting an orphan whose result never
	// arrived (harness crash mid-execution).
	payload := &PreToolUsePayload{
		Version:         "1",
		SessionID:       sessionID,
		ToolCall:        models.ToolCall{ID: call.ID, Name: call.Name, Input: normalised},
		NormalisedInput: normalised,
	}

	// Phase 1: hook consumers.
	d := tp.bus.Dispatch(EventPreToolUse, payload)
	if d.Verdict == VerdictDeny {
		return PhaseResult{
			Allowed:  false,
			DeniedBy: "hook:PreToolUse",
			Reason:   d.Reason,
		}
	}
	if d.Verdict == VerdictModify && d.ModifiedPayload != nil {
		// Accept modified payload if it includes normalised_input.
		if mp, ok := d.ModifiedPayload.(*PreToolUsePayload); ok {
			normalised = mp.NormalisedInput
		}
	}

	// Phase 2: policy deny-rules.  Independent of hook outcomes.
	for _, rule := range tp.denyRules {
		if rule.Predicate(call.Name, normalised) {
			return PhaseResult{
				Allowed:  false,
				DeniedBy: "rule:" + rule.Name,
				Reason:   "denied by policy rule " + rule.Name,
			}
		}
	}

	return PhaseResult{
		Allowed:       true,
		ModifiedInput: normalised,
	}
}
