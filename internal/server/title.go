// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// TitleRule is what the events list says of the title the harness gives
// a session (spec 083).
var TitleRule = fmt.Sprintf("A session whose agent's latest version sets spec.title is named by the harness after a turn a person started, and once for a session such an agent ran before: "+
	"model.request {model, prompt_version, purpose: %s, fold_seq, outcome, cost_usd_micro} records the request that asked a model whether the title still names the conversation, "+
	"its prompt_version the title prompt it was built from, such as %s; the session spends its cost, and it moves no thread and not the session's updated_at. "+
	"A new title follows it in the same batch as session.title_changed {by, old, new}, by the service {subject: %s, kind: service}, which leaves updated_at where it was. "+
	"A model.request without purpose is a step of a thread or a compaction's summary. No delta of a title's request is streamed: a client sees the title, not the model writing it. "+
	"Once a person changed the title, a fork's copied change included, the harness changes it no more.",
	session.PurposeTitle, prompts.Title, session.HarnessSubject)

// Title answers whether the session id may be named now, and on which
// model (spec 083); a hosted runner asks it after a turn and in the pass
// over the sessions an agent ran before. asked is the name the title
// request asks for. It asks the authorizer session.title as the
// session's initiator, in the context the session runs in, as a failover
// does, with session_id, model, the name asked, current_model,
// current_model_via and current_model_route, the model the session stands
// on and its names, the last two while it has them, and title, its title.
// The answer is the model the allow names, with the route it names beside
// it, and none when it names none; the runner connects it, so a model the
// installation does not run is passed over there. A deny, and an
// authorizer that cannot be asked, are errors, and the session keeps its
// title. Nothing is appended.
func (s *Server) Title(ctx context.Context, id, asked string) (session.ModelRef, error) {
	sess, err := s.o.Sessions.Get(ctx, id)
	if err != nil {
		return session.ModelRef{}, err
	}
	cfg, err := s.agentConfig(ctx, sess)
	if err != nil {
		return session.ModelRef{}, err
	}
	old := standing(sess, cfg)
	q, err := s.initiator(ctx, sess)
	if err != nil {
		return session.ModelRef{}, err
	}
	fields := map[string]any{"session_id": sess.ID, "model": asked, "current_model": old.Name, "title": sess.Title}
	if old.Via != "" {
		fields["current_model_via"] = old.Via
	}
	if old.Route != "" {
		fields["current_model_route"] = old.Route
	}
	limits, err := q.limits(ctx, authorizer.ActionSessionTitle, sessionResource(sess, fields))
	if err != nil {
		return session.ModelRef{}, err
	}
	if limits.Model == "" {
		return session.ModelRef{}, nil
	}
	return session.ModelRef{Name: limits.Model, Route: limits.Route}, nil
}

// LatestTitle is spec.title of the latest version of the agent id, nil
// when it sets none (spec 083): naming a session changes nothing its
// agent does, so an agent's latest version turns titles on and off for
// the sessions it already runs.
func (s *Server) LatestTitle(ctx context.Context, agentID string) (*v1.Title, error) {
	a, err := s.o.Objects.Agent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	v, err := s.o.Objects.Version(ctx, agentID, a.Latest)
	if err != nil {
		return nil, err
	}
	r, err := manifest.ReadBundle(v.Bundle)
	if err != nil {
		return nil, err
	}
	if r.Agent == nil {
		return nil, fmt.Errorf("server: version %d of %s holds no agent", v.Version, agentID)
	}
	return r.Agent.Spec.Title, nil
}

// TitleAgents are the ids of the agents, archived ones among them, whose
// latest version sets spec.title (spec 083): those whose sessions the pass
// at start names.
func (s *Server) TitleAgents(ctx context.Context) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		page, next, err := s.o.Objects.ListAgents(ctx, store.AgentList{Cursor: cursor, Limit: 200})
		if err != nil {
			return nil, err
		}
		for _, a := range page {
			set, err := s.LatestTitle(ctx, a.ID)
			if err != nil {
				return nil, err
			}
			if set != nil {
				ids = append(ids, a.ID)
			}
		}
		if next == "" {
			return ids, nil
		}
		cursor = next
	}
}
