// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package triggers

import (
	"context"
	"fmt"
	"time"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// rendered are a firing's templates rendered against what fired it.
type rendered struct {
	message, title, key string
	resources           []session.Resource
}

// render renders the trigger's templates for the firing: the event root
// from the envelope, when an event fired it, the trigger's id and name,
// and the firing's id and time. A stored spec's templates parsed at
// apply, so one that does not parse now is an error.
func render(t store.Trigger, spec v1.TriggerSpec, f store.Firing, env *trigger.Envelope) (rendered, error) {
	v := trigger.Values{
		trigger.RootTrigger: map[string]any{"id": t.ID, "name": t.Name},
		trigger.RootFiring:  map[string]any{"id": f.ID, "time": f.Time.UTC().Format(time.RFC3339Nano)},
	}
	if env != nil {
		ev, err := env.Value()
		if err != nil {
			return rendered{}, err
		}
		v[trigger.RootEvent] = ev
	}
	one := func(field, text string) (string, error) {
		tpl, err := trigger.Parse(text)
		if err != nil {
			return "", fmt.Errorf("triggers: trigger %s's %s: %w", t.ID, field, err)
		}
		return tpl.Render(v), nil
	}
	var r rendered
	var err error
	if r.message, err = one("session.message", spec.Session.Message); err != nil {
		return rendered{}, err
	}
	if r.title, err = one("session.title", spec.Session.Title); err != nil {
		return rendered{}, err
	}
	if r.key, err = one("session.key", trigger.Key(spec)); err != nil {
		return rendered{}, err
	}
	for i, res := range spec.Session.Resources {
		// The server refuses a trigger whose session names a memory store
		// at apply, since a session's memory stores are its agent's.
		if res.Type != v1.ResourceRepository {
			return rendered{}, fmt.Errorf("triggers: trigger %s's session.resources[%d] is of type %q, not a repository", t.ID, i, res.Type)
		}
		out := session.Resource{Type: session.ResourceRepository}
		if out.URL, err = one(fmt.Sprintf("session.resources[%d].url", i), res.URL); err != nil {
			return rendered{}, err
		}
		if out.Ref, err = one(fmt.Sprintf("session.resources[%d].ref", i), res.Ref); err != nil {
			return rendered{}, err
		}
		r.resources = append(r.resources, out)
	}
	return r, nil
}

// active reports whether a session is active (spec 022): running, or
// idle with input waiting for its runner or a call waiting for an
// answer, a confirmation, a client's result or a raised budget. An ended
// session, and an idle one that waits for nothing, is not.
func active(ctx context.Context, st session.Store, s session.Session) (bool, error) {
	switch {
	case s.Status == session.StatusEnded:
		return false, nil
	case s.Status == session.StatusRunning:
		return true, nil
	case s.StopReason == session.StopToolConfirmation, s.StopReason == session.StopToolResult, s.StopReason == session.StopBudget:
		return true, nil
	}
	return pendingInput(ctx, st, s)
}

// waitsForPerson reports whether a session is idle waiting for a person,
// on a confirmation or on its budget, when a message would answer the
// person's question for them: a user.message denies every pending call.
func waitsForPerson(s session.Session) bool {
	return s.Status == session.StatusIdle && (s.StopReason == session.StopToolConfirmation || s.StopReason == session.StopBudget)
}

// pendingInput reports whether an idle session's log holds input after
// its last session.status, reading the log's tail and more of it only
// while the tail holds no status.
func pendingInput(ctx context.Context, st session.Store, s session.Session) (bool, error) {
	for window := uint64(16); ; window *= 4 {
		from := uint64(1)
		if s.LastSeq > window {
			from = s.LastSeq - window + 1
		}
		evs, err := st.Events(ctx, s.ID, from, 0)
		if err != nil {
			return false, err
		}
		if session.HasPendingInput(evs) {
			return true, nil
		}
		if from == 1 || hasStatus(evs) {
			return false, nil
		}
	}
}

func hasStatus(evs []session.Event) bool {
	for _, e := range evs {
		if e.Type == session.TypeSessionStatus {
			return true
		}
	}
	return false
}
