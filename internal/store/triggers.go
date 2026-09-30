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
	// OrgID is the org_id claim of that apply, the context the trigger
	// was applied in, and "" for a personal one.
	OrgID string
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
	CreatedAt     time.Time
	UpdatedAt     time.Time
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
	Key      string
	Outcome  string
	Reason   string
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
	// whose outcome is failed or unrecorded is taken again with f's id
	// kept as stored. Any other stored firing is returned with false. A
	// caller claims under the trigger's lease, so an unrecorded firing
	// it finds is one a replica that stopped left.
	ClaimFiring(ctx context.Context, f Firing) (Firing, bool, error)
	// RecordFiring moves a claimed firing whose stored outcome is from to
	// f's outcome, reason, key, session and Open, and the trigger's
	// counts, last fired time and last session with it. ErrConflict
	// answers a stored outcome other than from.
	RecordFiring(ctx context.Context, f Firing, from string) error
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
