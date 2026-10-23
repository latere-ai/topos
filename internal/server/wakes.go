// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"latere.ai/x/sdk-go/agents/routine"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/proposal"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// A session's wake (spec 086) is a one-shot trigger of the session's
// agent that continues the session with the note the agent left itself.
// It is applied as the session's initiator in the session's context, and
// a session has one: its name is made from the session's id, so a set
// while one is pending applies the same trigger again. Its bounds are
// manifest/trigger's, checked here whatever applies a trigger whose
// origin is a wake.

// wakePrefix begins the name of a session's wake.
const wakePrefix = "wake-"

// wakeName is the name of the wake of session id: wakePrefix and the
// id's ULID in lowercase, a DNS label.
func wakeName(id string) string {
	return wakePrefix + strings.ToLower(strings.TrimPrefix(id, session.PrefixSession))
}

// SetWake applies w as the session's wake, as its initiator, with the
// call's origin, and the wake's bounds held: after MinWakeDelay, before
// MaxWakeAhead and inside the session's life. A set the initiator allowed,
// as the eleventh in a row is, names who allowed it.
func (t sessionTriggers) SetWake(ctx context.Context, w runner.Wake) (runner.ListedTrigger, error) {
	sess, q, err := t.asker(ctx)
	if err != nil {
		return runner.ListedTrigger{}, err
	}
	if w.ToolUseID == "" {
		return runner.ListedTrigger{}, &runner.TriggerRefusal{Code: CodeInvalidRequest, Message: "a wake names its call"}
	}
	origin := v1.TriggerOrigin{Kind: v1.OriginWake, SessionID: sess.ID, ToolUseID: w.ToolUseID}
	if by, err := t.allowedBy(ctx, sess, w.ToolUseID); err == nil {
		origin.ConfirmedBy = by
	} else if _, refused := errors.AsType[*runner.TriggerRefusal](err); !refused {
		return runner.ListedTrigger{}, err
	}
	a, err := t.s.o.Objects.Agent(ctx, sess.Agent.ID)
	if err != nil {
		return runner.ListedTrigger{}, err
	}
	// A wake carries the label a proposal does, naming the session's
	// agent, so a client lists a wake's agent without reading its session.
	doc := v1.Trigger{
		APIVersion: v1.APIVersion, Kind: v1.KindTrigger,
		Metadata: v1.ObjectMeta{Name: wakeName(sess.ID), DisplayName: w.Title, Labels: map[string]string{routine.Label: a.Name}},
		Spec: v1.TriggerSpec{
			Agent: sess.Agent.ID, At: new(w.At.UTC()), TimeZone: "UTC", MaxAge: trigger.WakeMaxAge.String(),
			Session: v1.TriggerSession{Message: escapeTemplate(w.Note), Policy: v1.PolicyContinue, Target: sess.ID},
		},
	}
	body, err := session.Marshal(doc)
	if err != nil {
		return runner.ListedTrigger{}, err
	}
	// A wake whose firing is held, its session waiting for a person when
	// it came due, holds a note the set replaces: its trigger goes with
	// that firing, so the stale note is never sent, and the set applies a
	// new one.
	old, err := t.s.o.Objects.TriggerByName(ctx, sess.Initiator.Subject, doc.Metadata.Name)
	switch {
	case err == nil && old.Counts.Held > 0:
		if err := t.s.o.Objects.DeleteTrigger(ctx, old.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return runner.ListedTrigger{}, err
		}
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return runner.ListedTrigger{}, err
	}
	tr, _, err := t.s.putTrigger(ctx, q, doc.Metadata.Name, body, triggerApply{origin: &origin, action: proposal.WakeSet, made: true, agent: sess.Agent.ID})
	if err != nil {
		return runner.ListedTrigger{}, refusal(err)
	}
	return t.s.listed(ctx, tr)
}

// CancelWake deletes the session's pending wake for the call toolUseID,
// asked as trigger.delete with the call's origin, and answers its id, ""
// when none was pending.
func (t sessionTriggers) CancelWake(ctx context.Context, toolUseID string) (string, error) {
	sess, q, err := t.asker(ctx)
	if err != nil {
		return "", err
	}
	tr, err := t.s.o.Objects.TriggerByName(ctx, sess.Initiator.Subject, wakeName(sess.ID))
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	res := triggerResource(tr)
	res.Fields["origin"] = originField(v1.TriggerOrigin{Kind: v1.OriginWake, SessionID: sess.ID, ToolUseID: toolUseID}, proposal.WakeCancel)
	if _, err := q.ask(ctx, authorizer.ActionTriggerDelete, res); err != nil {
		return "", refusal(err)
	}
	if err := t.s.o.Objects.DeleteTrigger(ctx, tr.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return tr.ID, nil
}

// escapeTemplate makes s literal in a trigger's template, as a routine's
// instruction is.
func escapeTemplate(s string) string { return strings.ReplaceAll(s, "{{", `\{{`) }

// checkWake holds a trigger a wake applies to its bounds, against the
// session the wake continues: a one-shot of WakeMaxAge, after
// MinWakeDelay and before MaxWakeAhead and the session's end.
func (s *Server) checkWake(ctx context.Context, spec v1.TriggerSpec, now time.Time) error {
	if spec.At == nil || spec.Session.Target == "" || spec.MaxAge != trigger.WakeMaxAge.String() {
		return refuse(CodeInvalidRequest, "a wake is a one-shot trigger that continues its session, with maxAge %s", trigger.WakeMaxAge)
	}
	target, err := s.o.Sessions.Get(ctx, spec.Session.Target)
	if err != nil {
		return err
	}
	if err := trigger.CheckWake(*spec.At, now, target.ExpiresAt); err != nil {
		we, _ := errors.AsType[*trigger.WakeError](err)
		e := refuse(CodeInvalidRequest, "spec.at: %v", err)
		e.details = map[string]any{"reason": we.Code, "bound": we.Bound.UTC().Format(time.RFC3339)}
		return e
	}
	return nil
}

// checkTarget holds a trigger's session.target to an open session of the
// trigger's agent, agentID, that q may send to (spec 086).
func (s *Server) checkTarget(ctx context.Context, q asker, name, agentID string, spec v1.TriggerSpec) error {
	id := spec.Session.Target
	target, err := s.o.Sessions.Get(ctx, id)
	switch {
	case errors.Is(err, session.ErrNotFound):
		return manifest.Invalid("spec.session.target", "names no session")
	case err != nil:
		return err
	case target.Status == session.StatusEnded:
		return manifest.Invalid("spec.session.target", "names a session that ended")
	case target.Agent.ID != agentID:
		return manifest.Invalid("spec.session.target", "names a session of another agent than spec.agent")
	}
	if _, err := q.ask(ctx, authorizer.ActionSessionSend, sessionResource(target, map[string]any{"sender": session.TriggerSubjectPrefix + name, "event_type": string(session.TypeUserMessage)})); err != nil {
		return err
	}
	return nil
}

// clearedEvent is the session.wake_cleared of the wake triggerID, for
// reason, and by the sender of a person's or a client's delete.
func (s *Server) clearedEvent(triggerID, reason string, by *session.Sender) (session.Event, error) {
	return session.NewEvent(session.TypeWakeCleared, session.WakeCleared{TriggerID: triggerID, Reason: reason, Sender: by}, s.o.Now())
}

// dropWake deletes the trigger of a wake its session no longer holds;
// one already gone is no error. The session's log said so first, so a
// delete that fails leaves a trigger whose firing finds its session
// ended and spends it.
func (s *Server) dropWake(ctx context.Context, sessionID, triggerID string) {
	if err := s.o.Objects.DeleteTrigger(ctx, triggerID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.o.Log.ErrorContext(ctx, "delete the wake of a session that no longer holds it; its firing spends it", "session", sessionID, "trigger", triggerID, "err", err)
	}
}

// canceledWake appends session.wake_cleared with canceled and the
// deleter to the session a deleted wake continued, while that session is
// open (spec 086).
func (s *Server) canceledWake(ctx context.Context, t store.Trigger, doc *v1.Trigger, by *auth.Caller) error {
	if !isWake(doc) {
		return nil
	}
	from := personSender(*by)
	ev, err := s.clearedEvent(t.ID, session.WakeCanceled, &from)
	if err != nil {
		return err
	}
	err = s.appendWhen(ctx, doc.Status.Origin.SessionID, []session.Event{ev}, unended)
	if errors.Is(err, session.ErrNotFound) || errors.Is(err, errEnded) {
		return nil
	}
	return err
}

// archivedWake records the end of the pending wake of the session id,
// which is being archived, and deletes its trigger: an idle session's
// log takes the session.wake_cleared, read on the session it follows.
func (s *Server) archivedWake(ctx context.Context, id string) error {
	var wake string
	guard := func(cur session.Session) error {
		if err := unended(cur); err != nil {
			return err
		}
		wake = ""
		if cur.Wake != nil {
			wake = cur.Wake.TriggerID
		}
		return nil
	}
	batch := func() ([]session.Event, error) {
		if wake == "" {
			return nil, nil
		}
		ev, err := s.clearedEvent(wake, session.WakeEnded, nil)
		return []session.Event{ev}, err
	}
	err := s.appendBuilt(ctx, id, guard, batch)
	if errors.Is(err, errEnded) {
		return nil
	}
	if err != nil {
		return err
	}
	if wake != "" {
		s.dropWake(ctx, id, wake)
	}
	return nil
}
