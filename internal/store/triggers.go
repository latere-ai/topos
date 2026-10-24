// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// The origins of a firing.
const (
	OriginSchedule = "schedule"
	OriginManual   = "manual"
	OriginEvent    = "event"
)

// The outcomes of a firing (spec 022). A firing claimed and not yet
// recorded has none. OutcomeFiltered is counted and never stored.
const (
	OutcomeStarted       = "started"
	OutcomeContinued     = "continued"
	OutcomeHeld          = "held"
	OutcomeFiltered      = "filtered"
	OutcomeSkippedActive = "skipped_active"
	OutcomeSkippedBusy   = "skipped_busy"
	OutcomeSkippedLate   = "skipped_late"
	// OutcomeSkippedEnded is a firing whose session.target has ended or
	// is gone (spec 086): it starts nothing, since a new session would
	// not hold the conversation its message was written for.
	OutcomeSkippedEnded = "skipped_ended"
	// OutcomeSkippedBudget is a firing that started nothing because the
	// spend of the trigger it counts toward was at that trigger's
	// allowance (spec 087). A later claim of the same firing takes it
	// again, as it takes a failed one, so a one-shot fires once the spend
	// allows.
	OutcomeSkippedBudget = "skipped_budget"
	OutcomeRefused       = "refused"
	OutcomeFailed        = "failed"
)

// Trigger is one stored trigger: its latest resolved document and its
// firing record.
type Trigger struct {
	ID   string
	Name string
	// Owner is the rendered subject of the person who last applied the
	// trigger, the initiator of every session it starts.
	Owner string
	// Claims are the claims of that apply that the trigger's firings
	// forward to the authorizer as the owner's (auth.TriggerClaims).
	Claims map[string]any
	// AgentID is the agent_ id the trigger's spec runs, without a
	// version.
	AgentID string
	Version int
	Digest  string
	// Doc is the resolved Trigger, the status the resolver wrote
	// included, as JSON.
	Doc []byte
	// Suspended mirrors spec.suspend.
	Suspended bool
	// NextFireAt is when a schedule next fires; nil for an event trigger
	// and while the trigger is suspended.
	NextFireAt    *time.Time
	LastFiredAt   *time.Time
	LastSessionID string
	Counts        v1.TriggerCounts
	// Spend is the trigger's week as the server last counted it (spec
	// 087), nil before any firing counted it. A later apply keeps it.
	Spend     *v1.TriggerSpend
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Firing is one firing of a trigger, keyed by the trigger and its dedupe
// string: the scheduled time of a scheduled firing, the envelope's
// product and id for an event, and the firing's own id for a manual one.
type Firing struct {
	ID        string
	TriggerID string
	Dedupe    string
	Origin    string
	// Envelope is the delivered event as JSON, for an event.
	Envelope []byte
	// FiredBy is the fired_by of the session the firing starts (spec
	// 078), as JSON: the token_use and authorization_details of the
	// token of the fire that recorded the firing, when a key minted that
	// token; empty for none. It is kept with the firing so a session
	// started after that token is gone, a held firing's, names the same
	// key.
	FiredBy []byte
	Key     string
	Outcome string
	Reason  string
	// SessionID is the session the firing started or continued.
	SessionID string
	// Open is set on a started firing until its session is seen ended,
	// so a trigger's active sessions are counted from its open firings.
	Open bool
	// Replica is the engine that claimed the firing.
	Replica string
	// Time is the firing's time: the scheduled time of a scheduled
	// firing, the arrival of an event or of a manual firing.
	Time       time.Time
	ReceivedAt time.Time
}

// Run is one firing a person counts as a run (spec 088): one that started
// or continued a session, so a turn a trigger's message began, or one
// that started nothing because an allowance or the authorizer stopped it.
// It is keyed by its firing and kept apart from the firing's row, which a
// one-shot's firing deletes with its trigger, so a wake's run outlives
// the wake.
type Run struct {
	FiringID string
	// Owner is the trigger's owner, whose read answers the run.
	Owner string
	// TriggerID is the trigger that fired: for a wake, the wake's own
	// trigger, which its firing deletes.
	TriggerID string
	// SessionID is the session the run started or continued; empty for a
	// skipped one.
	SessionID string
	// Wake reports a wake's firing.
	Wake bool
	// SessionTriggerID is, for a wake's run, the trigger that started its
	// session; empty for a session a person started and for every other
	// run.
	SessionTriggerID string
	// At is the firing's time.
	At time.Time
	// Outcome is the firing's: OutcomeStarted, OutcomeContinued,
	// OutcomeSkippedBudget or OutcomeRefused.
	Outcome string
	// Reason is a refused firing's reason.
	Reason string
	// CostUSDMicro is what the turn cost, as a look's cost is counted
	// (spec 087); 0 for a skipped run.
	CostUSDMicro int64
	// Ended reports the turn closed, so its cost is final. A skipped run
	// is ended when it is recorded.
	Ended bool
}

// RunCost is what a log says one run's turn cost so far, and whether it
// has ended.
type RunCost struct {
	FiringID     string
	CostUSDMicro int64
	Ended        bool
}

// IsRun reports whether a firing of outcome is a run (spec 088).
func IsRun(outcome string) bool {
	switch outcome {
	case OutcomeStarted, OutcomeContinued, OutcomeSkippedBudget, OutcomeRefused:
		return true
	}
	return false
}

// Ran reports whether a run started or continued a turn, so it has a cost.
func (r Run) Ran() bool { return r.Outcome == OutcomeStarted || r.Outcome == OutcomeContinued }

// CheckRun is the shape every PutRun refuses.
func CheckRun(r Run) error {
	switch {
	case session.CheckID(session.PrefixFiring, r.FiringID) != nil:
		return fmt.Errorf("%w: %q is not a firing id", session.ErrInvalid, r.FiringID)
	case r.Owner == "" || r.TriggerID == "":
		return fmt.Errorf("%w: a run names its owner and its trigger", session.ErrInvalid)
	case !IsRun(r.Outcome):
		return fmt.Errorf("%w: a firing of outcome %q is no run", session.ErrInvalid, r.Outcome)
	}
	return nil
}

// Costed is the run with cost written over it: a run already ended keeps
// what it holds, since an ended turn's cost is final, and one that has not
// takes the cost and the end the log read.
func (r Run) Costed(c RunCost) Run {
	if r.Ended {
		return r
	}
	r.CostUSDMicro, r.Ended = c.CostUSDMicro, c.Ended
	return r
}

// TriggerList narrows ListTriggers as AgentList narrows ListAgents.
type TriggerList struct {
	Owners []string
	Limit  int
	Cursor string
}

// Triggers keeps triggers, their firings, the open session each key
// names (spec 014's triggers, trigger_firings and trigger_sessions), and
// the lease that serializes a trigger's firings across replicas. A
// trigger's id is unique in the store and its name unique within its
// owner.
type Triggers interface {
	// Trigger returns a trigger by its trg_ id, whoever owns it.
	Trigger(ctx context.Context, id string) (Trigger, error)
	// TriggerByName returns the trigger owner holds under name.
	TriggerByName(ctx context.Context, owner, name string) (Trigger, error)
	// ListTriggers lists triggers oldest first, paged as ListAgents.
	ListTriggers(ctx context.Context, o TriggerList) ([]Trigger, string, error)
	// PutTrigger stores an applied trigger. Version 1 creates it, and
	// ErrConflict answers an id already held or a name its owner holds.
	// A later apply names the stored version, for a change to the
	// metadata alone, or the next, and ErrConflict answers any other; it
	// replaces the document, the owner, the org, the agent, the
	// suspension and the next fire time, and keeps the firing record.
	PutTrigger(ctx context.Context, t Trigger) error
	// DeleteTrigger removes a trigger with its firings and keys.
	DeleteTrigger(ctx context.Context, id string) error
	// DueTriggers lists the unsuspended triggers whose next fire time is
	// at or before at.
	DueTriggers(ctx context.Context, at time.Time) ([]Trigger, error)
	// SetNextFire moves a trigger's next fire time; nil fires nothing.
	SetNextFire(ctx context.Context, id string, at *time.Time) error

	// LeaseTrigger takes the trigger's lease for holder until until, or
	// renews holder's own, and reports false while another holder's
	// lease is unexpired on the store's clock.
	LeaseTrigger(ctx context.Context, id, holder string, until time.Time) (bool, error)
	// ReleaseTrigger gives holder's lease up; another holder's is kept.
	ReleaseTrigger(ctx context.Context, id, holder string) error

	// ClaimFiring takes the firing (f.TriggerID, f.Dedupe) and reports
	// true: a new one is stored as f with no outcome, and a stored one
	// whose outcome is failed, skipped_budget or unrecorded is taken again
	// with f's id kept as stored, and its FiredBy too when it has one, so
	// a firing a key fired keeps that key whoever fires it again; the
	// count a failed or skipped_budget one added is taken back. Any other stored
	// firing is returned with false. A caller claims under the trigger's
	// lease, so an unrecorded firing it finds is one a replica that
	// stopped left.
	ClaimFiring(ctx context.Context, f Firing) (Firing, bool, error)
	// RecordFiring moves a claimed firing whose stored outcome is from to
	// f's outcome, reason, key, session and Open, and the trigger's
	// counts, last fired time and last session with it. ErrConflict
	// answers a stored outcome other than from.
	RecordFiring(ctx context.Context, f Firing, from string) error
	// SpendFiring is RecordFiring for the firing that spends a one-shot
	// trigger (spec 086): in the same transaction as the check of from,
	// it deletes the trigger with its firings and keys. ErrConflict
	// answers a stored outcome other than from.
	SpendFiring(ctx context.Context, f Firing, from string) error
	// CountFiltered counts a filtered event, which stores no firing.
	CountFiltered(ctx context.Context, triggerID string) error
	// Firings lists a trigger's firings newest first, paged.
	Firings(ctx context.Context, triggerID string, limit int, cursor string) ([]Firing, string, error)
	// HeldFirings lists every trigger's held firings, oldest first.
	HeldFirings(ctx context.Context) ([]Firing, error)
	// OpenFirings lists a trigger's started firings whose session has
	// not been seen ended.
	OpenFirings(ctx context.Context, triggerID string) ([]Firing, error)
	// CloseSession records that a session a trigger started has ended:
	// its firings are no longer open, and a key that names it names
	// none.
	CloseSession(ctx context.Context, triggerID, sessionID string) error

	// TriggerSession is the open session key names, ErrNotFound for none.
	TriggerSession(ctx context.Context, triggerID, key string) (string, error)
	// SetTriggerSession maps key to sessionID.
	SetTriggerSession(ctx context.Context, triggerID, key, sessionID string) error

	// Hold is since when owner holds every trigger they own (spec 086),
	// nil while they hold none.
	Hold(ctx context.Context, owner string) (*time.Time, error)
	// SetHold holds every trigger owner owns from since, keeping an
	// earlier since of a hold already set, or releases them for nil.
	SetHold(ctx context.Context, owner string, since *time.Time) error
	// HeldOwners lists every owner who holds their triggers.
	HeldOwners(ctx context.Context) ([]string, error)

	// SetSpend keeps the trigger's week as the server counted it (spec
	// 087).
	SetSpend(ctx context.Context, id string, spend v1.TriggerSpend) error

	// PutRun stores a run (spec 088), replacing the run of its firing
	// whole: a firing taken again writes its new outcome.
	PutRun(ctx context.Context, r Run) error
	// SetRunCosts writes each cost to the run of its firing as Run.Costed
	// does, and passes over a firing that has no run.
	SetRunCosts(ctx context.Context, costs []RunCost) error
	// Runs lists owner's runs at or after since, oldest first, those of
	// one time by firing id.
	Runs(ctx context.Context, owner string, since time.Time) ([]Run, error)
	// PruneRuns deletes every run before before. Runs outlive their
	// triggers until then.
	PruneRuns(ctx context.Context, before time.Time) error
}

// CheckTrigger is the shape every PutTrigger refuses before it looks at
// the stored state.
func CheckTrigger(t Trigger) error {
	switch {
	case session.CheckID(session.PrefixTrigger, t.ID) != nil:
		return fmt.Errorf("%w: %q is not a trigger id", session.ErrInvalid, t.ID)
	case t.Name == "" || t.Owner == "":
		return fmt.Errorf("%w: a trigger has a name and an owner", session.ErrInvalid)
	case t.Version < 1 || t.Digest == "" || len(t.Doc) == 0:
		return fmt.Errorf("%w: a trigger holds its version, digest and document", session.ErrInvalid)
	case !utf8.Valid(t.Doc):
		return fmt.Errorf("%w: a trigger's document is UTF-8 text", session.ErrInvalid)
	}
	return nil
}

// CheckFiring is the shape every ClaimFiring refuses.
func CheckFiring(f Firing) error {
	switch {
	case session.CheckID(session.PrefixFiring, f.ID) != nil:
		return fmt.Errorf("%w: %q is not a firing id", session.ErrInvalid, f.ID)
	case f.TriggerID == "" || f.Dedupe == "":
		return fmt.Errorf("%w: a firing names its trigger and its dedupe string", session.ErrInvalid)
	case f.Origin != OriginSchedule && f.Origin != OriginManual && f.Origin != OriginEvent:
		return fmt.Errorf("%w: origin %q", session.ErrInvalid, f.Origin)
	case !utf8.Valid(f.Envelope):
		return fmt.Errorf("%w: a firing's envelope is UTF-8 text", session.ErrInvalid)
	}
	return nil
}

// Count adds n to the count of outcome, one of the stored outcomes or
// OutcomeFiltered; an unrecorded outcome counts nothing.
func Count(c *v1.TriggerCounts, outcome string, n int64) {
	switch outcome {
	case OutcomeStarted:
		c.Started += n
	case OutcomeContinued:
		c.Continued += n
	case OutcomeHeld:
		c.Held += n
	case OutcomeFiltered:
		c.Filtered += n
	case OutcomeSkippedActive:
		c.SkippedActive += n
	case OutcomeSkippedBusy:
		c.SkippedBusy += n
	case OutcomeSkippedLate:
		c.SkippedLate += n
	case OutcomeSkippedEnded:
		c.SkippedEnded += n
	case OutcomeSkippedBudget:
		c.SkippedBudget += n
	case OutcomeRefused:
		c.Refused += n
	case OutcomeFailed:
		c.Failed += n
	}
}

// Record applies a recorded firing to its trigger's record: the counts
// move from the outcome it held to its new one, and the last fired time
// and the last session follow it.
func Record(t *Trigger, f Firing, from string, now time.Time) {
	Count(&t.Counts, from, -1)
	Count(&t.Counts, f.Outcome, 1)
	at := now.UTC()
	t.LastFiredAt = &at
	if f.SessionID != "" && (f.Outcome == OutcomeStarted || f.Outcome == OutcomeContinued) {
		t.LastSessionID = f.SessionID
	}
}

// DecodeTrigger reads a stored trigger's Doc.
func DecodeTrigger(doc []byte) (*v1.Trigger, error) {
	var t v1.Trigger
	if err := json.Unmarshal(doc, &t); err != nil {
		return nil, fmt.Errorf("store: a stored trigger does not read: %w", err)
	}
	return &t, nil
}

// Retaken reports whether a claim takes a stored firing of outcome again
// (spec 087): one never recorded, one that failed, and one skipped at its
// trigger's allowance, which runs once the spend allows it.
func Retaken(outcome string) bool {
	return outcome == "" || outcome == OutcomeFailed || outcome == OutcomeSkippedBudget
}
