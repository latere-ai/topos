// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"

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
	// message is the person's message that denied a call waiting for a
	// confirmation (spec 012): the first one appended after the call's
	// agent.tool_use while no confirmation had answered it.
	message *session.UserMessage
}

// openCalls returns the thread's calls without a tool.result or a
// user.tool_result, in the order they were decided, each with what a
// person answered it by: a confirmation, or a message that denied it.
func openCalls(log []session.Event, thread string) []pendingCall {
	// A result answers its call whether or not it was redacted since: a
	// redaction removes what the result held, not that the call returned.
	answered := map[string]bool{}
	for _, e := range log {
		if id := e.Answers(); id != "" && e.Thread == thread {
			answered[id] = true
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
			// A message that denied the call stands: a confirmation
			// appended after it answers nothing.
			if i, ok := index[p.ToolUseID]; ok && open[i].message == nil {
				open[i].confirmation = &p
				open[i].runsAfter = 0
			}
		case e.Type == session.TypeUserMessage:
			// A person's message is in the session's own thread and denies
			// the waiting calls of every thread, so it is read whatever
			// thread the calls are of.
			var p session.UserMessage
			if e.Decode(&p) != nil || p.Sender.Kind != session.SenderPerson {
				continue
			}
			for i := range open {
				if open[i].use.Verdict == string(VerdictAsk) && open[i].confirmation == nil && open[i].message == nil {
					open[i].message = &p
				}
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

// note is the text of a message that denied a call, which the call's
// result gives the model as the person's note.
func note(m session.UserMessage) string {
	var parts []string
	for _, b := range m.Content {
		if b.Type == ir.BlockText && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n")
}

// waits are the kinds of answer a step still waits for once every call
// that could be settled is.
type waits struct{ confirmation, result bool }

// paused records the wait a paused call left, a thread's.
func (w *waits) paused(err error) {
	switch pauseReason(err) {
	case session.StopToolConfirmation:
		w.confirmation = true
	case session.StopToolResult:
		w.result = true
	}
}

// reason is the stop reason the session goes idle with, and false when
// nothing waits. Both kinds may wait at once and the reason names one of
// them, a confirmation before a client's result; a client finds what is
// open from the log.
func (w waits) reason() (session.StopReason, bool) {
	switch {
	case w.confirmation:
		return session.StopToolConfirmation, true
	case w.result:
		return session.StopToolResult, true
	}
	return "", false
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
