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
// may not read the session hears not_found first; a model the body names
// is checked by the rule a session's create checks its agent's by before
// session.update is asked, so the authorizer decides on a model that
// exists and may widen the session's model key to it. The question
// carries what the body changes: model when it names one, and effort,
// resolved to what the next turn runs, when it names one. An allowed
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
	// What the session runs now: its agent's model and effort until a
	// first change. A header written before changes carried an effort
	// names none, and its turns run at the agent's.
	old := session.ModelRef{Name: cfg.Model.Name, Effort: cfg.Effort}
	if s.Model != nil {
		old = session.ModelRef{Name: s.Model.Name, Effort: cmp.Or(s.Model.Effort, cfg.Effort)}
	}
	next := old
	fields := map[string]any{"session_id": s.ID}
	if b.Model.Name != nil {
		next.Name = *b.Model.Name
		m, overlay := cfg.SessionModel(next.Name)
		if err := c.s.runnable(ctx, m, overlay); err != nil {
			return err
		}
		fields["model"] = next.Name
	}
	if b.Model.Effort != nil {
		next.Effort = cmp.Or(*b.Model.Effort, cfg.Effort)
		fields["effort"] = next.Effort
	}
	if _, err := c.ask(ctx, authorizer.ActionSessionUpdate, sessionResource(s, fields)); err != nil {
		return err
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
