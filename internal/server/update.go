// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// updateBody is the body of PATCH /sessions/{id}: the fields of the
// session a request changes, the model alone for now.
type updateBody struct {
	Model *modelChange `json:"model"`
}

// modelChange is what a PATCH changes of the session's model: its name,
// its reasoning effort, or both. A member left out keeps what the session
// runs, so each is a pointer; an empty Effort returns to the agent's own.
type modelChange struct {
	Name   *string `json:"name"`
	Effort *string `json:"effort"`
}

// updateSession is PATCH /sessions/{id}: the model the session's next
// turn runs and the reasoning effort it runs at (spec 015). A caller who
// may not read the session hears not_found first. The question carries
// what the body changes, model when it names one, and effort, resolved
// to what the next turn runs, when it names one, beside the model the
// session stands on. The allow may answer a model the body names with
// the one to run in its place (spec 038), so the model is checked after
// the question, by the rule a session's create checks its own by: the
// one the allow names, or the one asked when it names none. An allowed
// change appends session.model_changed, which the header takes; a change
// to the model and the effort the session runs appends nothing.
func (c *call) updateSession() error {
	var b updateBody
	if err := c.decode(&b); err != nil {
		return err
	}
	if b.Model == nil || (b.Model.Name == nil && b.Model.Effort == nil) {
		return refuse(CodeInvalidRequest, `the body names what changes, and a session's model is the one field it changes: {"model": {"name": "...", "effort": "..."}}, either member or both`)
	}
	if n := b.Model.Name; n != nil && (*n == "" || strings.TrimSpace(*n) != *n) {
		return refuse(CodeInvalidRequest, "model.name is %q, not a model's name", *n)
	}
	if e := b.Model.Effort; e != nil && *e != "" && !slices.Contains(v1.Efforts, *e) {
		return refuse(CodeInvalidRequest, "model.effort is %q, not one of %s, or empty for the agent's own", *e, strings.Join(v1.Efforts, ", "))
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
	fields := map[string]any{"session_id": s.ID, "current_model": old.Name}
	if old.Via != "" {
		fields["current_model_via"] = old.Via
	}
	if b.Model.Name != nil {
		fields["model"] = *b.Model.Name
	}
	if b.Model.Effort != nil {
		next.Effort = cmp.Or(*b.Model.Effort, cfg.Effort)
		fields["effort"] = next.Effort
	}
	limits, err := c.askLimits(ctx, authorizer.ActionSessionUpdate, sessionResource(s, fields))
	if err != nil {
		return err
	}
	// A change that names a model runs the one the allow names in its
	// place and keeps the name asked beside it. An effort change alone
	// names no model, and an allow that names one for it moves nothing.
	if asked := b.Model.Name; asked != nil {
		next.Name, next.Via = cmp.Or(limits.Model, *asked), ""
		if next.Name != *asked {
			next.Via = *asked
		}
		m, overlay := cfg.SessionModel(next.Name)
		if err := c.s.runnable(ctx, m, overlay); err != nil {
			return err
		}
	}
	if next == old {
		return c.replySession(http.StatusOK, s)
	}
	sender := session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson}
	ev, err := session.NewEvent(session.TypeModelChanged, session.ModelChanged{By: sender, Old: old, New: next}, c.s.o.Now())
	if err != nil {
		return err
	}
	if _, err := c.s.append(ctx, s.ID, ev); err != nil {
		return err
	}
	if s, err = c.s.o.Sessions.Get(ctx, s.ID); err != nil {
		return err
	}
	return c.replySession(http.StatusOK, s)
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
// by and the effort its next turn runs at: its header's, or its agent's
// until a first change. A header written before changes carried an
// effort names none, and its turns run at the agent's.
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
