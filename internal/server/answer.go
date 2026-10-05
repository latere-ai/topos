// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"

	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// The reasoning level is stored under effort and answered under reasoning
// (spec 048). Every stored agent version's digest covers the level under
// effort, every header and every session.model_changed stored before the
// rename names it so, and an earlier release reads what this one stores,
// so the stored spelling stays and the API renames at its edge: asAnswer
// is applied to every value a route of the API answers, and to nothing a
// store, a runner or the harness reads.

// asAnswer is v as the API answers it: a session's model, an agent's
// models and a session.model_changed's two models with their level under
// reasoning. A value of any other type is answered as it is. The values v
// holds are not changed.
func asAnswer(v any) (any, error) {
	switch v := v.(type) {
	case session.Session:
		return sessionAnswer(v), nil
	case []session.Session:
		out := make([]session.Session, len(v))
		for i, s := range v {
			out[i] = sessionAnswer(s)
		}
		return out, nil
	case session.Event:
		return eventAnswer(v)
	case []session.Event:
		out := make([]session.Event, len(v))
		for i, e := range v {
			a, err := eventAnswer(e)
			if err != nil {
				return nil, err
			}
			out[i] = a
		}
		return out, nil
	case *v1.Agent:
		return agentAnswer(v), nil
	case []*v1.Agent:
		out := make([]*v1.Agent, len(v))
		for i, a := range v {
			out[i] = agentAnswer(a)
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			a, err := asAnswer(item)
			if err != nil {
				return nil, err
			}
			out[i] = a
		}
		return out, nil
	case page:
		items, err := asAnswer(v.Items)
		if err != nil {
			return nil, err
		}
		v.Items = items
		return v, nil
	}
	return v, nil
}

// sessionAnswer is a session with its model's level under reasoning.
func sessionAnswer(s session.Session) session.Session {
	if s.Model != nil {
		m := s.Model.Answered()
		s.Model = &m
	}
	return s
}

// agentAnswer is an agent document with every model of its spec
// answered: its own, its advisor's and each inline subagent's.
func agentAnswer(a *v1.Agent) *v1.Agent {
	if a == nil {
		return nil
	}
	out := *a
	out.Spec = a.Spec.Answered()
	return &out
}

// eventAnswer is an event with a session.model_changed's two models
// answered, its old model and its new one; any other event, and a
// redacted change, is answered as stored. A change whose payload does not
// decode is a log this release cannot read, and refuses the read.
func eventAnswer(e session.Event) (session.Event, error) {
	if e.Type != session.TypeModelChanged || e.Redacted() {
		return e, nil
	}
	var m session.ModelChanged
	if err := e.Decode(&m); err != nil {
		return session.Event{}, err
	}
	m.Old, m.New = m.Old.Answered(), m.New.Answered()
	b, err := session.Marshal(m)
	if err != nil {
		return session.Event{}, fmt.Errorf("server: answer the session.model_changed %s: %w", e.ID, err)
	}
	e.Payload = b
	return e, nil
}

// AnsweredSpec is how an agent's answer names its models' reasoning
// level, which the document states on every route that answers an agent.
const AnsweredSpec = "Every model of the spec, spec.model, spec.advisor.model and each inline subagent's, answers its reasoning level as reasoning. " +
	"A version is stored, and its digest computed, with the level under effort, its name before reasoning, " +
	"so the spec answered is not byte for byte the spec its digest covers."
