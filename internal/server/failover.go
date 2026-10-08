// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"cmp"
	"context"
	"strings"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// Failover answers which model a session's turn continues on when the
// model it runs could not serve now, or its provider rejected the request
// (spec 051), or it wrote a tool call as text (spec 062); a hosted
// runner's harness asks it in the middle of a turn.
// current is the model the turn runs, and failed that model, or a model an
// earlier answer of the turn named that the runner could not connect, with
// the same via. It asks the authorizer session.update as the session's
// initiator, in the context the session's agent belongs to, which is the
// context the session runs in: model is the routed name the session runs,
// current_model and current_model_via the model the session stands on and
// that name, failed_model the model that failed, failed_reason, when there
// is one, why it failed, harness.FailedRejected for a request the provider
// rejected and harness.FailedToolAsText for a model that wrote a tool call
// as text, and failed_detail, when there is one, the developer detail of
// the failure, at most models.MaxDetail bytes: the gateway's, from which
// an installation tells a provider's rate limit from its outage, the tool
// a call of was written as text, or why the model named could not be
// connected. An authorizer that routes
// passes over the failed model and names another in the allow's model, as
// it answers any switch to a routed name, or keeps the turn on the model a
// rejected request failed on by naming none; one that reads no
// failed_model answers the model the session stands on.
//
// The question carries current_model_route, the route the turn began on,
// while the session has one (spec 061). The model the allow names is
// checked by the rule a switch checks a model by, and its level, "" being
// the agent's own, is resolved as a send resolves it. The answer keeps the
// session's via and its route: the model moves inside the turn, the route
// the turn began on does not. It is the model
// the session stands on, so nothing moves, when the allow names none, the
// same one or the failed one, when current is not the routed model the
// session stands on, since the harness and the header disagree then about
// what ran, and when failed is not a model of the same routed name.
// Recording the change is the harness's, in the turn's own batch. A deny,
// an authorizer that cannot be asked and a model the installation does
// not run are errors.
func (s *Server) Failover(ctx context.Context, id string, current, failed session.ModelRef, reason, detail string) (session.ModelRef, error) {
	sess, err := s.o.Sessions.Get(ctx, id)
	if err != nil {
		return session.ModelRef{}, err
	}
	cfg, err := s.agentConfig(ctx, sess)
	if err != nil {
		return session.ModelRef{}, err
	}
	old := standing(sess, cfg)
	if old.Via == "" || old.Name != current.Name || old.Via != current.Via || failed.Name == "" || failed.Via != old.Via {
		return old, nil
	}
	q, err := s.initiator(ctx, sess)
	if err != nil {
		return session.ModelRef{}, err
	}
	fields := map[string]any{
		"session_id": sess.ID, "model": old.Via, "current_model": old.Name, "current_model_via": old.Via,
		"failed_model": failed.Name,
	}
	if old.Route != "" {
		fields["current_model_route"] = old.Route
	}
	if reason != "" {
		fields["failed_reason"] = reason
	}
	if detail != "" {
		fields["failed_detail"] = cut(detail, models.MaxDetail)
	}
	limits, err := q.limits(ctx, authorizer.ActionSessionUpdate, sessionResource(sess, fields))
	if err != nil {
		return session.ModelRef{}, err
	}
	if limits.Model == "" || limits.Model == old.Name || limits.Model == failed.Name {
		return old, nil
	}
	m, overlay := cfg.SessionModel(limits.Model)
	if err := s.runnable(ctx, m, overlay); err != nil {
		return session.ModelRef{}, err
	}
	next := old
	next.Name = limits.Model
	if r := limits.Reasoning; r != nil {
		next.Effort = cmp.Or(*r, cfg.Effort)
	}
	return next, nil
}

// cut is s at most n bytes long, on a character's boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// initiator is the session's initiator as an asker, in the context the
// session runs in: its agent's, an organization's for an organization's
// agent and the initiator's own otherwise (spec 036). A question asked so
// carries the context alone among the claims, as a trigger's firing does
// for its owner (spec 022), since no request of the initiator's is in
// hand.
func (s *Server) initiator(ctx context.Context, sess session.Session) (asker, error) {
	a, err := s.o.Objects.Agent(ctx, sess.Agent.ID)
	if err != nil {
		return asker{}, err
	}
	org, _ := ownerOf(a).organization()
	issuer, sub, ok := authz.SplitSubject(sess.Initiator.Subject)
	if !ok {
		sub = sess.Initiator.Subject
	}
	return s.as(auth.Caller{Subject: sess.Initiator.Subject, Issuer: issuer, Sub: sub, Claims: auth.ContextClaims(org)}), nil
}

// as is an asker for a caller toposd asks for with no request of theirs
// in hand: a trigger's owner at a firing, a session's initiator at a
// failover.
func (s *Server) as(c auth.Caller) asker {
	q := asker{caller: c}
	q.ask = func(ctx context.Context, action string, res authz.Resource) (authz.Decision, error) {
		return s.o.Guard.Ask(ctx, auth.Envelope(c, action, res, nil))
	}
	q.limits = func(ctx context.Context, action string, res authz.Resource) (authorizer.Limits, error) {
		return s.o.Guard.Limits(ctx, auth.Envelope(c, action, res, nil))
	}
	return q
}
