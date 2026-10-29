// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1

import "time"

// Values of ConnectionSpec.Mode.
const (
	ConnectionPerson = "person"
	ConnectionAgent  = "agent"
)

// Values of SessionResource.Type.
const (
	ResourceMemoryStore = "memoryStore"
	ResourceRepository  = "repository"
)

// Values of TriggerSession.Policy (spec 022).
const (
	PolicyNew      = "new"
	PolicyContinue = "continue"
)

// Values of TriggerSession.Notify (spec 086): every turn a firing begins
// brings its result to the person, or the agent decides with its notify
// tool.
const (
	NotifyAlways = "always"
	NotifyJudged = "judged"
)

// Values of TriggerReach.Split (spec 095).
const (
	// SplitScope makes each firing ask the authorizer which scopes are
	// due and start one session for each.
	SplitScope = "scope"
)

// TriggerSpec is what a trigger fires on, a schedule, delivered events,
// one time or a list of times, and the session each firing starts or
// continues (spec 022). The fields specs 022, 086 and 090 added (On, At,
// Times, MaxActive, and the session's Policy, Key and Target) are omitted
// when unset and take their defaults where the trigger fires, so a
// trigger that sets none of them keeps its digest.
type TriggerSpec struct {
	// Agent references the agent the sessions run; the resolved spec
	// holds its id, so the trigger runs the agent's latest version.
	Agent string `json:"agent"`
	// Schedule is a five-field cron expression, or @hourly, @daily or
	// @weekly. A trigger sets exactly one of Schedule, On, At and Times.
	Schedule string `json:"schedule,omitempty"`
	TimeZone string `json:"timeZone"`
	// On selects the delivered events the trigger fires on.
	On *TriggerOn `json:"on,omitempty"`
	// At is the one time a one-shot trigger fires, RFC 3339 with an
	// offset; its firing spends the trigger, which the server then deletes
	// (spec 086).
	At *time.Time `json:"at,omitempty"`
	// Times are the times a trigger fires, each once, RFC 3339 with an
	// offset, at most trigger.MaxTimes and none twice; the firing of the
	// latest spends the trigger, as At's does (spec 090). A time already
	// past stays listed and never fires.
	Times        []time.Time    `json:"times,omitempty"`
	Session      TriggerSession `json:"session"`
	SkipIfActive *bool          `json:"skipIfActive"`
	// MaxActive bounds the trigger's sessions active at once;
	// trigger.DefaultMaxActive when unset.
	MaxActive *int   `json:"maxActive,omitempty"`
	MaxAge    string `json:"maxAge"`
	Suspend   bool   `json:"suspend"`
	// Budget is the trigger's weekly allowance (spec 087); nil for none.
	Budget *TriggerBudget `json:"budget,omitempty"`
	// Reach names what the trigger's runs reach, which the authorizer
	// maps, and whether a firing runs once per due scope (spec 095); nil
	// for a trigger that runs as its agent does.
	Reach *TriggerReach `json:"reach,omitempty"`
}

// TriggerReach is a trigger's reach (spec 095). Kind is a name the
// installation's authorizer maps to what the trigger's runs may read and
// write; the core carries it in its questions and decides nothing from
// it. Split, SplitScope or empty, makes each firing ask trigger.runs
// which scopes are due and start one session for each.
type TriggerReach struct {
	Kind  string `json:"kind"`
	Split string `json:"split,omitempty"`
}

// TriggerBudget is a trigger's allowance (spec 087): PerWeek is the most
// the looks counted toward the trigger may cost in a rolling 7 days, in
// USD micros, above zero. A firing while that spend is at or past it
// starts nothing and records skipped_budget.
type TriggerBudget struct {
	PerWeek int64 `json:"perWeek"`
}

// TriggerOn is an event filter: every field given must match, and a
// list matches when any of its entries does. An entry of Verbs or
// Resources is exact, or ends in * and matches the prefix before it.
type TriggerOn struct {
	Product   string         `json:"product"`
	Verbs     []string       `json:"verbs"`
	Resources []string       `json:"resources,omitempty"`
	Match     []TriggerMatch `json:"match,omitempty"`
}

// TriggerMatch matches the value at Path, a path into the event's
// payload such as payload.action, against the entries of In.
type TriggerMatch struct {
	Path string   `json:"path"`
	In   []string `json:"in"`
}

// TriggerSession is the session a firing starts, or the message it sends
// to the open session its key names. Message, Title, Key and a
// repository resource's URL and Ref are templates (spec 022). A field
// left out is the agent's.
type TriggerSession struct {
	Message string `json:"message"`
	Title   string `json:"title,omitempty"`
	// Policy is PolicyNew or PolicyContinue; PolicyNew when unset.
	Policy string `json:"policy,omitempty"`
	// Key is the name a firing's session goes by: "" on a schedule and
	// {{event.resource}} on an event when unset.
	Key string `json:"key,omitempty"`
	// Target is the open session, ses_ and the rest, every firing of a
	// trigger under PolicyContinue sends its message to in place of the
	// session its key names (spec 086); a firing whose target has ended
	// or is gone starts nothing.
	Target string `json:"target,omitempty"`
	// Attended is the attended of each session a firing starts: its
	// questions wait for the owner (spec 086). Ignored with Target, whose
	// session keeps its own; false when unset.
	Attended *bool `json:"attended,omitempty"`
	// Notify is NotifyAlways or NotifyJudged; NotifyAlways when unset. A
	// wake's turn is judged whatever it says.
	Notify string `json:"notify,omitempty"`
	// Model is the model and the reasoning level each session a firing
	// starts begins on in place of its agent's (spec 089); a firing that
	// continues an open session leaves that session's model as it is.
	// Ignored with Target; nil for the agent's own.
	Model     *SessionModel     `json:"model,omitempty"`
	Machine   *SessionMachine   `json:"machine,omitempty"`
	Resources []SessionResource `json:"resources,omitempty"`
	Budget    *Budget           `json:"budget,omitempty"`
	Limits    *SessionLimits    `json:"limits,omitempty"`
	EndOnIdle *bool             `json:"endOnIdle"`
}

// SessionModel is the model a triggered session starts on, in the shape a
// session's change of its model takes (spec 015): its name, its reasoning
// level, one of Efforts, or both. A member left out is the agent's. The
// installation's authorizer may run another model in the named one's
// place, as it may for the agent's own (spec 038).
type SessionModel struct {
	Name      string `json:"name,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
}

// SessionMachine is the machine a triggered session runs on. A device,
// a person's own computer, names the Cella environment it serves and the
// folder on it the session works in (spec 080).
type SessionMachine struct {
	Kind        string `json:"kind,omitempty"`
	Image       string `json:"image,omitempty"`
	Environment string `json:"environment,omitempty"`
	Workdir     string `json:"workdir,omitempty"`
}

// SessionResource is one attachment of a triggered session: a memory
// store by name or mem_ id with its access, or a git repository at a
// ref (spec 004).
type SessionResource struct {
	Type        string `json:"type"`
	MemoryStore string `json:"memoryStore,omitempty"`
	Access      string `json:"access,omitempty"`
	URL         string `json:"url,omitempty"`
	Ref         string `json:"ref,omitempty"`
}

// SessionLimits are a triggered session's wall-clock bounds; one left
// out is the agent's.
type SessionLimits struct {
	TurnTimeout string `json:"turnTimeout,omitempty"`
	MaxAge      string `json:"maxAge,omitempty"`
}

// MemoryStoreSpec describes a memory store to the model that reads it.
type MemoryStoreSpec struct {
	Description string `json:"description"`
}

// ConnectionSpec is how a credential reaches a service: the hosts it is
// injected toward and the header it goes in. Credential names a
// Credential by name or cred_ id and is set only in agent mode; in
// person mode the credential is that of the person running the agent.
type ConnectionSpec struct {
	Service    string   `json:"service"`
	Mode       string   `json:"mode"`
	Hosts      []string `json:"hosts"`
	Credential string   `json:"credential,omitempty"`
	Inject     Inject   `json:"inject"`
}

// Inject is the header a credential is injected in. Format contains
// {value}, where the credential goes.
type Inject struct {
	Header string `json:"header"`
	Format string `json:"format"`
}
