// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/session"
)

// resumeBody is the body of POST /sessions/{id}/resume.
type resumeBody struct {
	Reason          string `json:"reason"`
	MaxCostUSDMicro *int64 `json:"max_cost_usd_micro,omitempty"`
}

// resumeSession is POST /sessions/{id}/resume: a session idle on its
// budget resumes once its cap is raised or its payer's credit restored
// (spec 007). The new budget is the lowest of the one asked, the agent's
// and the authorizer's; a budget the session has already spent resumes
// nothing, so it is refused. A session waiting for its machine, a
// person's own computer, resumes as it stands, once the computer is back
// or its owner allowed the session: the resume adds no message and keeps
// the budget it has, and a resume that asks for another budget is
// refused (spec 080). session.resumed is pending input, so a runner
// claims the session and continues, opening its machine again.
func (c *call) resumeSession() error {
	var b resumeBody
	if err := c.decode(&b); err != nil {
		return err
	}
	if b.MaxCostUSDMicro != nil && *b.MaxCostUSDMicro <= 0 {
		return refuse(CodeInvalidRequest, "max_cost_usd_micro is %d; a budget is above zero", *b.MaxCostUSDMicro)
	}
	ctx := c.r.Context()
	s, err := c.s.o.Sessions.Get(ctx, c.r.PathValue("id"))
	if err != nil {
		return err
	}
	fields := map[string]any{"stop_reason": string(s.StopReason)}
	if b.MaxCostUSDMicro != nil {
		fields["max_cost_usd_micro"] = *b.MaxCostUSDMicro
	}
	limits, err := c.askLimits(ctx, authorizer.ActionSessionResume, sessionResource(s, fields))
	if err != nil {
		return err
	}
	waiting := s.StopReason == session.StopMachineWaiting
	switch {
	case s.Status != session.StatusIdle || (s.StopReason != session.StopBudget && !waiting):
		return refuse(CodeConflict, "the session is %s with stop reason %q; only a session idle on its budget or waiting for its machine resumes", s.Status, s.StopReason)
	case waiting && b.MaxCostUSDMicro != nil:
		return refuse(CodeInvalidRequest, "max_cost_usd_micro raises a budget, and this session waits for its machine, not its budget")
	case waiting:
		return c.resumeWaiting(s, b.Reason, limits.BudgetUSDMicro)
	}
	v, err := c.s.o.Objects.Version(ctx, s.Agent.ID, s.Agent.Version)
	if err != nil {
		return err
	}
	r, err := manifest.ReadBundle(v.Bundle)
	if err != nil {
		return err
	}
	cfg, err := r.AgentConfig(nil)
	if err != nil {
		return err
	}
	max := lowestCost(b.MaxCostUSDMicro, cfg.MaxCostUSDMicro, limits.BudgetUSDMicro)
	evs, err := c.s.o.Sessions.Events(ctx, s.ID, 1, 0)
	if err != nil {
		return err
	}
	if spent := session.Spent(evs); max != nil && spent >= *max {
		return refuse(CodeConflict, "the budget of %d micro-USD is spent (%d); resume with a higher max_cost_usd_micro or after the cap is raised", *max, spent)
	}
	return c.resumed(s, b.Reason, max)
}

// resumeWaiting resumes a session waiting for its machine with the budget
// it has, held to the ceiling the authorizer's allow names, limit; a
// budget that ceiling leaves spent resumes nothing, so it is refused.
func (c *call) resumeWaiting(s session.Session, reason string, limit *int64) error {
	max := lowestCost(s.Budget.MaxCostUSDMicro, limit)
	evs, err := c.s.o.Sessions.Events(c.r.Context(), s.ID, 1, 0)
	if err != nil {
		return err
	}
	if spent := session.Spent(evs); max != nil && spent >= *max {
		return refuse(CodeConflict, "the budget of %d micro-USD is spent (%d); resume after the cap is raised", *max, spent)
	}
	return c.resumed(s, reason, max)
}

// resumed appends the caller's session.resumed with the session's budget
// from then on, max, and answers the session. The event sets the budget
// whatever it holds, so a resume that raises none carries the one the
// session has.
func (c *call) resumed(s session.Session, reason string, max *int64) error {
	ctx := c.r.Context()
	sender := personSender(c.caller)
	ev, err := session.NewEvent(session.TypeSessionResumed, session.SessionResumed{By: sender, Reason: reason, MaxCostUSDMicro: max}, c.s.o.Now())
	if err != nil {
		return err
	}
	if _, err := c.s.append(ctx, s.ID, ev); err != nil {
		return err
	}
	c.s.o.Notify()
	if s, err = c.s.o.Sessions.Get(ctx, s.ID); err != nil {
		return err
	}
	return c.replySession(http.StatusOK, s)
}
