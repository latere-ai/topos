// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
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
	Model *session.ModelRef `json:"model"`
}

// updateSession is PATCH /sessions/{id}: the model the session's next
// turn runs (spec 015). A caller who may not read the session hears
// not_found first; the model is checked by the rule a session's create
// checks its agent's by before session.update is asked, so the authorizer decides on a model
// that exists and may widen the session's model key to it. An allowed
// switch appends session.model_changed, which the header takes; a
// switch to the model the session runs appends nothing.
func (c *call) updateSession() error {
	var b updateBody
	if err := c.decode(&b); err != nil {
		return err
	}
	if b.Model == nil {
		return refuse(CodeInvalidRequest, `the body names what changes, and a session's model is the one field it changes: {"model": {"name": "..."}}`)
	}
	name := b.Model.Name
	if name == "" || strings.TrimSpace(name) != name {
		return refuse(CodeInvalidRequest, "model.name is %q, not a model's name", name)
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
	m, overlay := cfg.SessionModel(name)
	if err := c.s.runnable(ctx, m, overlay); err != nil {
		return err
	}
	if _, err := c.ask(ctx, authorizer.ActionSessionUpdate, sessionResource(s, map[string]any{"session_id": s.ID, "model": name})); err != nil {
		return err
	}
	old := cfg.Model.Name
	if s.Model != nil {
		old = s.Model.Name
	}
	if old == name {
		return c.replySession(http.StatusOK, s)
	}
	sender := session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson}
	ev, err := session.NewEvent(session.TypeModelChanged, session.ModelChanged{By: sender, Old: session.ModelRef{Name: old}, New: session.ModelRef{Name: name}}, c.s.o.Now())
	if err != nil {
		return err
	}
	if _, err := c.s.append(c.r.Context(), s.ID, ev); err != nil {
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
