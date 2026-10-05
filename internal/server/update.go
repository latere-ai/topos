// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// updateBody is the body of PATCH /sessions/{id}: the fields of the
// session a request changes, its model and the approval mode of its
// policy, either or both.
type updateBody struct {
	Model  *modelChange  `json:"model"`
	Policy *policyChange `json:"policy"`
}

// policyChange is what a PATCH changes of the session's policy: the
// approval mode its next steps decide calls under (spec 041). The lists
// and the thresholds are the authorizer's and the agent's, so the mode is
// its one member.
type policyChange struct {
	Mode *string `json:"mode"`
}

// modes are the approval modes a session may be set to, strictest first.
var modes = []string{v1.ModePlan, v1.ModeConfirm, v1.ModeProgressive}

// modelChange is what a PATCH changes of the session's model: its name,
// its reasoning level, or both. A member left out keeps what the session
// runs, so each is a pointer; an empty level returns to the agent's own.
// The level is named reasoning, or effort, its name before spec 048,
// which is read through every v0.x release; a body that names both names
// one level.
type modelChange struct {
	Name      *string `json:"name,omitempty"`
	Reasoning *string `json:"reasoning,omitempty"`
	Effort    *string `json:"effort,omitempty"`
}

// level is the reasoning level the change names under either name, nil
// when it names none.
func (m modelChange) level() *string {
	if m.Reasoning != nil {
		return m.Reasoning
	}
	return m.Effort
}

// updateSession is PATCH /sessions/{id}: the model the session's next
// turn runs and the reasoning level it runs at (spec 015), and the
// approval mode its next steps decide calls under (spec 041). A caller
// who may not read the session hears not_found first. One question
// carries what the body changes: model when it names one, and the level,
// resolved to what the next turn runs, when it names one, under both
// effort and reasoning, so an authorizer that reads either name decides
// it (spec 048), beside the model the session stands on; approval_mode
// beside the mode the session runs and its agent's. The allow may answer
// a model the body names with the one to run in its place (spec 038), so
// the model is checked after the question, by the rule a session's create
// checks its own by: the one the allow names, or the one asked when it
// names none. The allow may also answer the level the change runs at
// (spec 048). An allowed change appends session.model_changed and
// session.policy_changed in one batch, which the header takes; a change
// to what the session runs appends nothing.
func (c *call) updateSession() error {
	var b updateBody
	if err := c.decode(&b); err != nil {
		return err
	}
	if err := b.check(); err != nil {
		return err
	}
	ctx := c.r.Context()
	s, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	if s.Status == session.StatusEnded {
		return refuse(CodeConflict, "the session ended %s", s.StopReason)
	}
	cfg, err := c.s.agentConfig(ctx, s)
	if err != nil {
		return err
	}
	old := standing(s, cfg)
	next := old
	fields := map[string]any{"session_id": s.ID}
	if b.Model != nil {
		fields["current_model"] = old.Name
		if old.Via != "" {
			fields["current_model_via"] = old.Via
		}
		if b.Model.Name != nil {
			fields["model"] = *b.Model.Name
		}
		if level := b.Model.level(); level != nil {
			next.Effort = cmp.Or(*level, cfg.Effort)
			fields["effort"], fields["reasoning"] = next.Effort, next.Effort
		}
	}
	agentMode := cmp.Or(cfg.Policy.Mode, harness.ModeConfirm)
	oldMode := standingMode(s, agentMode)
	nextMode := oldMode
	if b.Policy != nil {
		nextMode = harness.Mode(*b.Policy.Mode)
		fields["approval_mode"] = string(nextMode)
		fields["current_approval_mode"] = string(oldMode)
		fields["agent_approval_mode"] = string(agentMode)
	}
	limits, err := c.askLimits(ctx, authorizer.ActionSessionUpdate, sessionResource(s, fields))
	if err != nil {
		return err
	}
	// A change that names a model runs the one the allow names in its
	// place and keeps the name asked beside it. A level change alone
	// names no model, and an allow that names one for it moves nothing.
	if b.Model != nil && b.Model.Name != nil {
		asked := *b.Model.Name
		next.Name, next.Via = cmp.Or(limits.Model, asked), ""
		if next.Name != asked {
			next.Via = asked
		}
		m, overlay := cfg.SessionModel(next.Name)
		if err := c.s.runnable(ctx, m, overlay); err != nil {
			return err
		}
	}
	// A change of the model or the level runs at the level the allow
	// names when it names one, "" being the agent's own; a change of the
	// mode alone moves no model and reads none.
	if r := limits.Reasoning; b.Model != nil && r != nil {
		next.Effort = cmp.Or(*r, cfg.Effort)
	}
	sender := session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson}
	now := c.s.o.Now()
	var batch []session.Event
	if next != old {
		ev, err := session.NewEvent(session.TypeModelChanged, session.ModelChanged{By: sender, Old: old, New: next}, now)
		if err != nil {
			return err
		}
		batch = append(batch, ev)
	}
	if nextMode != oldMode {
		ev, err := session.NewEvent(session.TypePolicyChanged, session.PolicyChanged{By: sender, Old: session.PolicyRef{Mode: string(oldMode)}, New: session.PolicyRef{Mode: string(nextMode)}}, now)
		if err != nil {
			return err
		}
		batch = append(batch, ev)
	}
	if len(batch) == 0 {
		return c.replySession(http.StatusOK, s)
	}
	if err := c.s.appendBatch(ctx, s.ID, batch); err != nil {
		return err
	}
	if s, err = c.s.o.Sessions.Get(ctx, s.ID); err != nil {
		return err
	}
	return c.replySession(http.StatusOK, s)
}

// check refuses a body that names nothing to change or a value no
// session takes, before the session is read.
func (b updateBody) check() error {
	if (b.Model == nil || (b.Model.Name == nil && b.Model.level() == nil)) && b.Policy == nil {
		return refuse(CodeInvalidRequest, `the body names what changes: {"model": {"name": "...", "reasoning": "..."}}, either member or both, {"policy": {"mode": "..."}}, or both`)
	}
	if b.Model != nil {
		if b.Model.Name == nil && b.Model.level() == nil {
			return refuse(CodeInvalidRequest, "model names neither name nor reasoning")
		}
		if n := b.Model.Name; n != nil && (*n == "" || strings.TrimSpace(*n) != *n) {
			return refuse(CodeInvalidRequest, "model.name is %q, not a model's name", *n)
		}
		for _, l := range []struct {
			name  string
			level *string
		}{{"reasoning", b.Model.Reasoning}, {"effort", b.Model.Effort}} {
			if l.level != nil && *l.level != "" && !slices.Contains(v1.Efforts, *l.level) {
				return refuse(CodeInvalidRequest, "model.%s is %q, not one of %s, or empty for the agent's own", l.name, *l.level, strings.Join(v1.Efforts, ", "))
			}
		}
		if r, e := b.Model.Reasoning, b.Model.Effort; r != nil && e != nil && *r != *e {
			return refuse(CodeInvalidRequest, "model.reasoning is %q and model.effort %q: name the level once, as reasoning", *r, *e)
		}
	}
	if b.Policy != nil && (b.Policy.Mode == nil || !slices.Contains(modes, *b.Policy.Mode)) {
		got := "absent"
		if b.Policy.Mode != nil {
			got = strconv.Quote(*b.Policy.Mode)
		}
		return refuse(CodeInvalidRequest, "policy.mode is %s, not one of %s", got, strings.Join(modes, ", "))
	}
	return nil
}

// standingMode is the approval mode s runs: its recorded policy's, and
// its agent's, agentMode, for a session that records none (spec 041).
func standingMode(s session.Session, agentMode harness.Mode) harness.Mode {
	if s.Policy != nil && s.Policy.Mode != "" {
		return harness.Mode(s.Policy.Mode)
	}
	return agentMode
}

// agentConfig is the configuration of the agent version sess runs.
func (s *Server) agentConfig(ctx context.Context, sess session.Session) (manifest.AgentConfig, error) {
	v, err := s.o.Objects.Version(ctx, sess.Agent.ID, sess.Agent.Version)
	if err != nil {
		return manifest.AgentConfig{}, err
	}
	r, err := manifest.ReadBundle(v.Bundle)
	if err != nil {
		return manifest.AgentConfig{}, err
	}
	return r.AgentConfig(nil)
}

// standing is the model s runs as it stands, with the name it was asked
// by and the reasoning level its next turn runs at: its header's, or its
// agent's until a first change. A header written before changes carried
// a level names none, and its turns run at the agent's.
func standing(s session.Session, cfg manifest.AgentConfig) session.ModelRef {
	if s.Model == nil {
		return session.ModelRef{Name: cfg.Model.Name, Effort: cfg.Effort}
	}
	return session.ModelRef{Name: s.Model.Name, Via: s.Model.Via, Effort: cmp.Or(s.Model.Effort, cfg.Effort)}
}

// runnable refuses a session's model the installation does not run
// (spec 007): model_unknown for one no source gives figures, and
// model_unavailable for one whose figures could not be read.
func (s *Server) runnable(ctx context.Context, m v1.AgentModel, overlay models.Entry) error {
	err := s.o.Runnable(ctx, m, overlay)
	if err == nil {
		return nil
	}
	if mc, ok := errors.AsType[*models.Coded](err); ok && mc.Code == models.CodeUnknown {
		return &apiError{code: models.CodeUnknown, detail: mc.Message, err: err}
	}
	return &apiError{code: models.CodeUnavailable, detail: "the figures of " + m.Name + " could not be read", err: err}
}
