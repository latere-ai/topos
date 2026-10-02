// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"

	"latere.ai/x/topos/session"
)

// pendingCall is an agent.tool_use of the thread with no result yet.
type pendingCall struct {
	use          session.AgentToolUse
	seq          uint64
	confirmation *session.UserToolConfirmation
	// runsAfter counts the session.status running events after the
	// confirmation: the current claim appends one, so a second means an
	// earlier runner may have started the call.
	runsAfter int
}

// openCalls returns the thread's calls without a tool.result or a
// user.tool_result, in the order they were decided.
func openCalls(log []session.Event, thread string) []pendingCall {
	answered := map[string]bool{}
	for _, e := range log {
		if e.Redacted() || e.Thread != thread {
			continue
		}
		switch e.Type {
		case session.TypeToolResult:
			var p session.ToolResult
			if e.Decode(&p) == nil {
				answered[p.ToolUseID] = true
			}
		case session.TypeUserToolResult:
			var p session.UserToolResult
			if e.Decode(&p) == nil {
				answered[p.ToolUseID] = true
			}
		}
	}
	var open []pendingCall
	index := map[string]int{}
	for _, e := range log {
		if e.Redacted() {
			continue
		}
		switch {
		case e.Type == session.TypeAgentToolUse && e.Thread == thread:
			var p session.AgentToolUse
			if e.Decode(&p) != nil || answered[p.ToolUseID] {
				continue
			}
			index[p.ToolUseID] = len(open)
			open = append(open, pendingCall{use: p, seq: e.Seq})
		case e.Type == session.TypeUserToolConfirmation:
			var p session.UserToolConfirmation
			if e.Decode(&p) != nil {
				continue
			}
			if i, ok := index[p.ToolUseID]; ok {
				open[i].confirmation = &p
				open[i].runsAfter = 0
			}
		case e.Type == session.TypeSessionStatus:
			var p session.SessionStatus
			if e.Decode(&p) != nil || p.Status != session.StatusRunning {
				continue
			}
			for i := range open {
				if open[i].confirmation != nil {
					open[i].runsAfter++
				}
			}
		}
	}
	return open
}

// rememberedPatterns are the allow patterns people added with remember
// on their confirmations, in the order they answered.
func rememberedPatterns(log []session.Event) []string {
	var out []string
	for _, e := range log {
		if e.Type != session.TypeUserToolConfirmation || e.Redacted() {
			continue
		}
		var p session.UserToolConfirmation
		if e.Decode(&p) == nil && p.Decision == session.DecisionAllow && p.Remember != "" {
			out = append(out, p.Remember)
		}
	}
	return out
}

// forwardAnswer tells a learning decider the person's answer to an asked
// call (spec 037). It is best effort: the call is settled as it would be
// without a decision service, whatever the decider says.
func (t *turn) forwardAnswer(ctx context.Context, c pendingCall) {
	l, ok := t.h.decider().(Learner)
	if !ok {
		return
	}
	approve := c.confirmation.Decision == session.DecisionAllow
	_ = l.Answered(ctx, t.s, c.use.ToolUseID, approve, c.confirmation.Sender.Subject)
}
