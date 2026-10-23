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
	// CheckProposal asks whether p could be done as the session's
	// initiator, before anyone is asked to allow it, and answers a
	// *TriggerRefusal when it could not.
	CheckProposal(ctx context.Context, p Proposal) error
	// ApplyProposal does p as the session's initiator, once they allowed
	// its call when its action is held for them, and answers the trigger as
	// stored, or a *TriggerRefusal when it is refused. The same propose
	// applies the same trigger however often it is asked.
	ApplyProposal(ctx context.Context, p Proposal) (ListedTrigger, error)
	// SetWake applies w as the session's one wake, as its initiator,
	// replacing a pending one, and answers the trigger as stored, or a
	// *TriggerRefusal when it is refused (spec 086). The same call applies
	// the same trigger however often it is asked.
	SetWake(ctx context.Context, w Wake) (ListedTrigger, error)
	// CancelWake deletes the session's pending wake for the call toolUseID
	// and answers its trigger's id, "" when none was pending.
	CancelWake(ctx context.Context, toolUseID string) (string, error)
}

// Wake is a set of the wake tool as its call named it: the time the wake
// is due, the line a person reads, and the note the agent leaves itself.
type Wake struct {
	ToolUseID string    `json:"tool_use_id"`
	At        time.Time `json:"at"`
	Title     string    `json:"title"`
	Note      string    `json:"note"`
}

// The actions of a Proposal: every action of the trigger tool but its
// list. An empty Action is a propose, as a runner of a release before the
// other actions sends one.
const (
	TriggerPropose = "propose"
	TriggerChange  = "change"
	TriggerPause   = "pause"
	TriggerResume  = "resume"
	TriggerDelete  = "delete"
)

// Proposal is a call of the trigger tool that writes, as the call named
// it: a propose of a new trigger, or a change, pause, resume or delete of
// the trigger ID names.
type Proposal struct {
	ToolUseID string `json:"tool_use_id"`
	// Action is one of the Trigger actions; empty is TriggerPropose.
	Action string `json:"action,omitempty"`
	// ID is the trg_ id of the trigger a change, pause, resume or delete
	// acts on.
	ID string `json:"id,omitempty"`
	// Name is the trigger's name a person reads, Ask what each firing
	// asks, and Why the sentence the person reads of why the agent offers
	// the trigger, or pauses it.
	Name string `json:"name"`
	Ask  string `json:"ask"`
	Why  string `json:"why"`
	// Schedule is a five-field cron of a minute and an hour, every day or
	// on the days of the week it lists, read in TimeZone; On, in its
	// place, the delivered events the trigger fires on.
	Schedule string        `json:"schedule"`
	TimeZone string        `json:"time_zone"`
	On       *v1.TriggerOn `json:"on,omitempty"`
	// Notify is v1.NotifyAlways or v1.NotifyJudged, empty for the
	// default: always for a schedule, and judged for events when the agent
	// names the notify tool (spec 086).
	Notify string `json:"notify,omitempty"`
}

// Verb is p's action, TriggerPropose for none.
func (p Proposal) Verb() string {
	if p.Action == "" {
		return TriggerPropose
	}
	return p.Action
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
	Ask string `json:"ask,omitempty"`
	// At is a one-shot trigger's time; Target and TargetTitle the session
	// a wake continues and its title (spec 086).
	At          *time.Time        `json:"at,omitempty"`
	Target      string            `json:"target,omitempty"`
	TargetTitle string            `json:"target_title,omitempty"`
	Suspended   bool              `json:"suspended,omitempty"`
	NextFireAt  *time.Time        `json:"next_fire_at,omitempty"`
	Origin      *v1.TriggerOrigin `json:"origin,omitempty"`
	// Held is set when the trigger's owner holds every trigger they own
	// (spec 086): it fires nothing until they release them.
	Held bool `json:"held,omitempty"`
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
