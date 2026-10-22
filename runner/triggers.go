// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"time"

	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// Triggers is a session's reach to the triggers its server keeps, which
// the trigger tool asks (spec 086). Every question is the server's own,
// asked of its authorizer as the person who started the session, in the
// session's context: a runner holds no token of theirs. toposd answers it
// for its own runners, and a runner process's lease asks it over toposd's
// internal listener.
type Triggers interface {
	// ListTriggers answers the session initiator's triggers of the
	// session's agent, newest first, at most trigger.MaxListed.
	ListTriggers(ctx context.Context) ([]ListedTrigger, error)
	// CheckProposal asks whether p could be applied as the session's
	// initiator, before anyone is asked to allow it, and answers a
	// *TriggerRefusal when it could not.
	CheckProposal(ctx context.Context, p Proposal) error
	// ApplyProposal applies p as the session's initiator once they allowed
	// its call, and answers the trigger as stored, or a *TriggerRefusal
	// when it is refused. The same call applies the same trigger however
	// often it is asked.
	ApplyProposal(ctx context.Context, p Proposal) (ListedTrigger, error)
}

// Proposal is a propose of the trigger tool as its call named it: a
// routine on a schedule.
type Proposal struct {
	ToolUseID string `json:"tool_use_id"`
	// Name is the routine's name a person reads, Ask what each run asks,
	// and Why the sentence the person reads of why the agent offers it.
	Name string `json:"name"`
	Ask  string `json:"ask"`
	Why  string `json:"why"`
	// Schedule is a five-field cron of a minute and an hour, every day or
	// on the days of the week it lists, read in TimeZone.
	Schedule string `json:"schedule"`
	TimeZone string `json:"time_zone"`
}

// ListedTrigger is one trigger as the trigger tool reads it.
type ListedTrigger struct {
	ID string `json:"id"`
	// Name is the trigger's name, and Title the name a person reads.
	Name  string `json:"name"`
	Title string `json:"title,omitempty"`
	// Schedule and TimeZone are a schedule trigger's; Product the product
	// of an event trigger's events.
	Schedule string `json:"schedule,omitempty"`
	TimeZone string `json:"time_zone,omitempty"`
	Product  string `json:"product,omitempty"`
	// Ask is what each firing asks, as its template is written.
	Ask        string            `json:"ask,omitempty"`
	Suspended  bool              `json:"suspended,omitempty"`
	NextFireAt *time.Time        `json:"next_fire_at,omitempty"`
	Origin     *v1.TriggerOrigin `json:"origin,omitempty"`
}

// TriggerRefusal is a trigger question the server answered no: its
// authorizer denied it, or the trigger is not one it applies. Code is the
// authorizer's reason or the API's error code, and Message the
// developer's detail; the tool tells the model both.
type TriggerRefusal struct {
	Code, Message string
}

func (e *TriggerRefusal) Error() string { return "trigger refused: " + e.Code + ": " + e.Message }

// triggers is the Triggers of a drive holding lease on the session id: the
// lease's own, Options.Triggers's, or nil.
func (r *Runner) triggers(id string, lease session.Lease) Triggers {
	if t, ok := lease.(Triggers); ok {
		return t
	}
	if r.o.Triggers == nil {
		return nil
	}
	return r.o.Triggers(id)
}

type triggersKey struct{}

// WithTriggers is ctx carrying a drive's Triggers, which the harness
// builder reads with TriggersFrom.
func WithTriggers(ctx context.Context, t Triggers) context.Context {
	return context.WithValue(ctx, triggersKey{}, t)
}

// TriggersFrom is the Triggers of the drive ctx belongs to, nil when the
// runner reaches no server that keeps triggers.
func TriggersFrom(ctx context.Context) Triggers {
	t, _ := ctx.Value(triggersKey{}).(Triggers)
	return t
}
