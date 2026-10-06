// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"time"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// SinkEvent is one mutation as spec 023's sink envelope carries it,
// content-free: its action as type, who did it, the object it changed,
// and outcome, ok or the code of a refusal the authorizer allowed the
// mutation before. Attributes are named figures and ids, never a
// message, an input or a value.
type SinkEvent struct {
	Type       string         `json:"type"`
	OccurredAt time.Time      `json:"occurred_at"`
	Subject    string         `json:"subject"`
	Object     SinkObject     `json:"object"`
	SessionID  string         `json:"session_id,omitempty"`
	Agent      *SinkAgent     `json:"agent,omitempty"`
	Outcome    string         `json:"outcome"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// SinkObject is the kind and id of what a mutation changed.
type SinkObject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// SinkAgent is the agent version a session runs.
type SinkAgent struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// refusedFork reports a fork refused after the authorizer allowed its
// session.fork, err being the refusal of its message's session.send
// (spec 054): the event names the fork the allow named, with the
// refusal's code as its outcome, the session forked and the fork point,
// and the authorizer's reason where the refusal carries one. Nothing of
// the fork was written. A sink that fails is logged, since the refusal
// is the answer either way.
func (s *Server) refusedFork(ctx context.Context, q asker, fork session.Session, f *forkOrigin, err error) {
	refusal := classify(err)
	e := SinkEvent{
		Type: authorizer.ActionSessionFork, OccurredAt: s.o.Now().UTC(), Subject: q.caller.Subject,
		Object: SinkObject{Kind: authorizer.KindSession, ID: fork.ID}, SessionID: fork.ID, Agent: &SinkAgent{ID: fork.Agent.ID, Version: fork.Agent.Version},
		Outcome:    refusal.code,
		Attributes: map[string]any{"parent": f.parent.ID, "seq": f.seq, "refused": authorizer.ActionSessionSend},
	}
	if reason, ok := refusal.details["reason"].(string); ok {
		e.Attributes["reason"] = reason
	}
	if s.o.Sink == nil {
		s.o.Log.InfoContext(ctx, "a fork refused after its allow", "type", e.Type, "session", e.SessionID, "parent", f.parent.ID, "outcome", e.Outcome)
		return
	}
	if err := s.o.Sink(ctx, e); err != nil {
		s.o.Log.ErrorContext(ctx, "report a fork refused after its allow to the sink", "session", e.SessionID, "outcome", e.Outcome, "err", err)
	}
}
