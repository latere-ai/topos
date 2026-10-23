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

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/proposal"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// The trigger tool's reach (spec 086). A session's call lists the
// triggers the person who started the session holds of the session's
// agent, proposes a new one, and changes, pauses, resumes or deletes one
// of them; the harness holds every call but a list and a pause until that
// person allows it. The server asks every question of it as the session's
// initiator in the session's context, with the call named in the
// question's origin, and applies the trigger as theirs through the
// trigger route's own code. A proposal is written in the one shape every
// client reads (latere.ai/x/sdk-go/agents/routine), named by the session
// and the call, so a retry of the call applies the same trigger.

// SessionTriggers is the session id's reach to the triggers this server
// keeps, which the trigger tool of the session's drive asks: the runners
// this server drives read it directly, and a runner process's lease asks
// it over the internal listener.
func (s *Server) SessionTriggers(id string) runner.Triggers { return sessionTriggers{s: s, id: id} }

type sessionTriggers struct {
	s  *Server
	id string
}

// listPage is how many triggers one read of the owner's list asks for,
// and listPages how many pages a list reads at most.
const (
	listPage  = 100
	listPages = 20
)

// ListTriggers asks trigger.list as the session's initiator, naming the
// owner and the agent, and answers the owner's triggers of the session's
// agent newest first, at most trigger.MaxListed.
func (t sessionTriggers) ListTriggers(ctx context.Context) ([]runner.ListedTrigger, error) {
	sess, q, err := t.asker(ctx)
	if err != nil {
		return nil, err
	}
	owner := q.caller.Subject
	d, err := q.ask(ctx, authorizer.ActionTriggerList, authz.NewResource(authorizer.KindTrigger, "", map[string]any{"owner": owner, "agent": sess.Agent.ID}))
	if err != nil {
		return nil, refusal(err)
	}
	if !admits(d, owner) {
		return []runner.ListedTrigger{}, nil
	}
	var all []store.Trigger
	cursor := ""
	for range listPages {
		page, next, err := t.s.o.Objects.ListTriggers(ctx, store.TriggerList{Owners: []string{owner}, Limit: listPage, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, tr := range page {
			if tr.AgentID == sess.Agent.ID {
				all = append(all, tr)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	slices.SortStableFunc(all, func(a, b store.Trigger) int { return b.CreatedAt.Compare(a.CreatedAt) })
	out := make([]runner.ListedTrigger, 0, min(len(all), trigger.MaxListed))
	for _, tr := range all[:min(len(all), trigger.MaxListed)] {
		l, err := t.s.listed(ctx, tr)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// CheckProposal asks the questions of p as the session's initiator,
// before the call is held, and stores nothing.
func (t sessionTriggers) CheckProposal(ctx context.Context, p runner.Proposal) error {
	_, err := t.act(ctx, p, false)
	return err
}

// ApplyProposal does p as the session's initiator: a pause at once, and
// every other action once they allowed its call, which the log must hold.
func (t sessionTriggers) ApplyProposal(ctx context.Context, p runner.Proposal) (runner.ListedTrigger, error) {
	tr, err := t.act(ctx, p, true)
	if err != nil {
		return runner.ListedTrigger{}, err
	}
	return t.s.listed(ctx, tr)
}

// asker is the session and its initiator as an asker in the session's
// context; an ended session asks nothing.
func (t sessionTriggers) asker(ctx context.Context) (session.Session, asker, error) {
	sess, err := t.s.o.Sessions.Get(ctx, t.id)
	if err != nil {
		return session.Session{}, asker{}, err
	}
	if sess.Status == session.StatusEnded {
		return session.Session{}, asker{}, &runner.TriggerRefusal{Code: CodeConflict, Message: "the session ended"}
	}
	q, err := t.s.initiator(ctx, sess)
	return sess, q, err
}

// act asks the questions of p as the session's initiator and, when apply
// is set, does it: a propose writes a new trigger of the session's agent
// in the routine's shape, a change, a pause and a resume apply the
// trigger p names again with its fields changed, and a delete removes it.
// An action held for the person names who allowed the call, which must be
// the initiator, read from the session's log; a pause, which only stops
// work, names nobody.
func (t sessionTriggers) act(ctx context.Context, p runner.Proposal, apply bool) (store.Trigger, error) {
	sess, q, err := t.asker(ctx)
	if err != nil {
		return store.Trigger{}, err
	}
	if p.ToolUseID == "" {
		return store.Trigger{}, &runner.TriggerRefusal{Code: CodeInvalidRequest, Message: "a trigger call names its call"}
	}
	if err := proposal.Check(p); err != nil {
		return store.Trigger{}, &runner.TriggerRefusal{Code: CodeInvalidRequest, Message: err.Error()}
	}
	a, err := t.s.o.Objects.Agent(ctx, sess.Agent.ID)
	if err != nil {
		return store.Trigger{}, err
	}
	origin := v1.TriggerOrigin{Kind: v1.OriginProposal, SessionID: sess.ID, ToolUseID: p.ToolUseID}
	if apply && proposal.Held(p.Verb()) != "" {
		by, err := t.allowedBy(ctx, sess, p.ToolUseID)
		if err != nil {
			return store.Trigger{}, err
		}
		origin.ConfirmedBy = by
	}
	if p.Verb() == runner.TriggerPropose {
		name := proposal.TriggerName(sess.ID, p.ToolUseID, p)
		body, err := proposal.Manifest(a.Name, name, p)
		if err != nil {
			return store.Trigger{}, &runner.TriggerRefusal{Code: CodeInvalidRequest, Message: err.Error()}
		}
		tr, _, err := t.s.putTrigger(ctx, q, name, body, triggerApply{origin: &origin, made: true, agent: sess.Agent.ID, check: !apply})
		if err != nil {
			return store.Trigger{}, refusal(err)
		}
		return tr, nil
	}
	tr, doc, err := t.own(ctx, sess, p.ID)
	if err != nil {
		return store.Trigger{}, err
	}
	if p.Verb() == runner.TriggerDelete {
		res := triggerResource(tr)
		res.Fields["origin"] = originField(origin)
		if _, err := q.ask(ctx, authorizer.ActionTriggerDelete, res); err != nil {
			return store.Trigger{}, refusal(err)
		}
		if apply {
			if err := t.s.removeTrigger(ctx, tr, nil); err != nil {
				return store.Trigger{}, err
			}
		}
		return tr, nil
	}
	body, err := proposal.Changed(a.Name, doc, p)
	if err != nil {
		return store.Trigger{}, &runner.TriggerRefusal{Code: CodeInvalidRequest, Message: err.Error()}
	}
	how := triggerApply{origin: &origin, agent: sess.Agent.ID, check: !apply}
	if p.Verb() == runner.TriggerPause {
		how.why = strings.TrimSpace(p.Why)
	}
	out, _, err := t.s.putTrigger(ctx, q, tr.Name, body, how)
	if err != nil {
		return store.Trigger{}, refusal(err)
	}
	if !apply {
		return tr, nil
	}
	return out, nil
}

// own is the trigger id names as the session's call may act on it: one
// the session's initiator owns, that runs the session's agent, and that
// is no session's wake, which only its own session sets and cancels. Any
// other id answers not_found, as a trigger the caller cannot see does.
func (t sessionTriggers) own(ctx context.Context, sess session.Session, id string) (store.Trigger, *v1.Trigger, error) {
	missing := &runner.TriggerRefusal{Code: auth.CodeNotFound, Message: "the person holds no trigger " + id + " of this agent that a conversation may change"}
	tr, err := t.s.o.Objects.Trigger(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Trigger{}, nil, missing
	}
	if err != nil {
		return store.Trigger{}, nil, err
	}
	doc, err := store.DecodeTrigger(tr.Doc)
	if err != nil {
		return store.Trigger{}, nil, err
	}
	if tr.Owner != sess.Initiator.Subject || tr.AgentID != sess.Agent.ID || isWake(doc) {
		return store.Trigger{}, nil, missing
	}
	return tr, doc, nil
}

// allowedBy is who allowed the call toolUseID: the sender of its
// user.tool_confirmation allow, which must be the session's initiator,
// the trigger's owner to be. The send route refuses another sender's
// confirmation of a trigger call; reading it again here keeps a trigger
// from being applied on anyone else's word.
func (t sessionTriggers) allowedBy(ctx context.Context, sess session.Session, toolUseID string) (string, error) {
	evs, err := t.s.o.Sessions.Events(ctx, sess.ID, 1, 0)
	if err != nil {
		return "", err
	}
	for _, e := range slices.Backward(evs) {
		if e.Type != session.TypeUserToolConfirmation {
			continue
		}
		var c session.UserToolConfirmation
		if err := e.Decode(&c); err != nil || c.ToolUseID != toolUseID {
			continue
		}
		if c.Decision != session.DecisionAllow || c.Sender.Subject != sess.Initiator.Subject {
			break
		}
		return c.Sender.Subject, nil
	}
	return "", &runner.TriggerRefusal{Code: auth.CodeForbidden, Message: "the session's log holds no allow of this call by the person who started the session"}
}

// refusal is an answer the API would give a caller as a refusal the tool
// tells the model, its authorizer's reason when the deny carries one; a
// server's own failure stays an error.
func refusal(err error) error {
	e := classify(err)
	row, ok := codes[e.code]
	if !ok || row.status >= http.StatusInternalServerError {
		return err
	}
	code := e.code
	if r, ok := e.details["reason"].(string); ok && r != "" {
		code = r
	}
	return &runner.TriggerRefusal{Code: code, Message: cmp.Or(e.detail, row.message)}
}

// listed is a stored trigger as the trigger tool reads it: a wake's with
// the title of the session it continues, when that session is there.
func (s *Server) listed(ctx context.Context, t store.Trigger) (runner.ListedTrigger, error) {
	doc, err := store.DecodeTrigger(t.Doc)
	if err != nil {
		return runner.ListedTrigger{}, err
	}
	l := runner.ListedTrigger{ID: t.ID, Name: t.Name, Title: doc.Metadata.DisplayName, Schedule: doc.Spec.Schedule, TimeZone: doc.Spec.TimeZone,
		Ask: doc.Spec.Session.Message, At: doc.Spec.At, Target: doc.Spec.Session.Target, Suspended: t.Suspended, NextFireAt: t.NextFireAt, Origin: doc.Status.Origin}
	if doc.Spec.On != nil {
		l.Product = doc.Spec.On.Product
	}
	if l.Target != "" {
		target, err := s.o.Sessions.Get(ctx, l.Target)
		switch {
		case err == nil:
			l.TargetTitle = target.Title
		case !errors.Is(err, session.ErrNotFound):
			return runner.ListedTrigger{}, err
		}
	}
	return l, nil
}
