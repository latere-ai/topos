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
// nothing, so it is refused. session.resumed is pending input, so a
// runner claims the session and continues.
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
	if s.Status != session.StatusIdle || s.StopReason != session.StopBudget {
		return refuse(CodeConflict, "the session is %s with stop reason %q; only a session idle on its budget resumes", s.Status, s.StopReason)
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
	sender := session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson}
	ev, err := session.NewEvent(session.TypeSessionResumed, session.SessionResumed{By: sender, Reason: b.Reason, MaxCostUSDMicro: max}, c.s.o.Now())
	if err != nil {
		return err
	}
	if _, err := c.s.append(c.r.Context(), s.ID, ev); err != nil {
		return err
	}
	c.s.o.Notify()
	if s, err = c.s.o.Sessions.Get(ctx, s.ID); err != nil {
		return err
	}
	return c.replySession(http.StatusOK, s)
}
