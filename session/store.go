// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"time"
)

// Digest addresses a blob: "sha256:" and the lowercase hex of its bytes.
type Digest string

// DigestOf returns the digest of b.
func DigestOf(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

var digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Valid reports whether d has the digest form.
func (d Digest) Valid() bool { return digestForm.MatchString(string(d)) }

// Hex returns the hex part of d.
func (d Digest) Hex() string {
	if !d.Valid() {
		return ""
	}
	return string(d[len("sha256:"):])
}

// The errors of spec 004. Codes a client sees are the text after
// "session: ".
var (
	ErrSequenceConflict     = errors.New("session: sequence_conflict")
	ErrRedactionUncompacted = errors.New("session: redaction_uncompacted")
	ErrSchemaTooNew         = errors.New("session: schema_too_new")
	ErrLocked               = errors.New("session: locked")
	ErrCorrupt              = errors.New("session: corrupt")
	ErrNotFound             = errors.New("session: not_found")
	ErrExists               = errors.New("session: exists")
	ErrInvalid              = errors.New("session: invalid")
	ErrBlobMismatch         = errors.New("session: blob_mismatch")
	// ErrLeaseLost is an append through a lease another holder has taken
	// the session from.
	ErrLeaseLost = errors.New("session: lease_lost")
)

// Holder is who holds a session's lease.
type Holder struct {
	Runner     string    `json:"runner"`
	PID        int       `json:"pid,omitempty"`
	Host       string    `json:"host,omitempty"`
	AcquiredAt time.Time `json:"acquired_at"`
}

// LockedError is ErrLocked naming the current holder.
type LockedError struct {
	Holder Holder
}

func (e *LockedError) Error() string {
	h := e.Holder
	if h.Runner == "" && h.PID == 0 {
		return "session: locked by another holder"
	}
	return fmt.Sprintf("session: locked by runner %q (pid %d on %s) since %s",
		h.Runner, h.PID, h.Host, h.AcquiredAt.UTC().Format(time.RFC3339))
}

// Is makes errors.Is(err, ErrLocked) hold.
func (e *LockedError) Is(target error) bool { return target == ErrLocked }

// Lease is a session's one-writer lease.
type Lease interface {
	// Renew extends the lease; it fails once the lease is lost.
	Renew(ctx context.Context) error
	// Release gives the lease up. A second Release is a no-op.
	Release() error
	// Lost is closed when the lease ends for any reason.
	Lost() <-chan struct{}
}

// Fence is the optional interface of a lease another holder can take
// over, as a Postgres lease that expires: its own Append is refused with
// ErrLeaseLost once the session's lease is no longer this one, in the
// same transaction as the write, so a runner that has not noticed its
// loss yet cannot interleave with the one that took over (spec 016). A
// lease that cannot be taken over while held needs no fence.
type Fence interface {
	Append(ctx context.Context, afterSeq uint64, events []Event) (uint64, error)
}

// ListOptions filter and page List. Sessions list newest first; Cursor
// is the value a previous page returned. Owners, when set, keeps the
// sessions whose initiator's subject is one of them, the narrowing an
// authorizer's list decision carries; Runner, when set, keeps the
// sessions of that runner kind. A store filters before it pages, so a
// page holds only matching sessions.
type ListOptions struct {
	Status  Status
	AgentID string
	Owners  []string
	Runner  string
	// Archived keeps sessions by whether they are archived; the zero
	// value keeps both, as every caller inside the server lists.
	Archived Archived
	Limit    int
	Cursor   string
}

// Archived selects sessions by their archived_at.
type Archived string

// The archived filters of a List.
const (
	ArchivedAny     Archived = ""
	ArchivedExclude Archived = "exclude"
	ArchivedOnly    Archived = "only"
)

// Keeps reports whether s passes the filter.
func (a Archived) Keeps(s Session) bool {
	switch a {
	case ArchivedExclude:
		return s.ArchivedAt == nil
	case ArchivedOnly:
		return s.ArchivedAt != nil
	}
	return true
}

// Archiver is the optional interface of a store that files sessions
// away from the lists (spec 015): SetArchived sets a session's
// archived_at to at, or clears it for nil, without an event, and answers
// the session as it is after. Setting what is already set leaves the
// time it was set.
type Archiver interface {
	SetArchived(ctx context.Context, id string, at *time.Time) (Session, error)
}

// Archive sets s's archived_at to at, or clears it for nil, and reports
// whether that changed anything; an archived session keeps the time it
// was first archived.
func Archive(s *Session, at *time.Time) bool {
	switch {
	case at == nil && s.ArchivedAt == nil, at != nil && s.ArchivedAt != nil:
		return false
	case at == nil:
		s.ArchivedAt = nil
	default:
		t := at.UTC()
		s.ArchivedAt = &t
	}
	return true
}

// DefaultListLimit is the page size of a List with no limit.
const DefaultListLimit = 50

// Blobs keeps the bodies of a store's blobs outside it, in a directory
// or an object store (spec 014), each under its session's id and its
// digest. A store handed one writes each body there before the event
// that names it, reads it back verified against its digest, and deletes
// a session's bodies after its rows.
type Blobs interface {
	PutBlob(ctx context.Context, sessionID string, d Digest, body []byte) error
	// GetBlob answers ErrNotFound for a body it does not hold.
	GetBlob(ctx context.Context, sessionID string, d Digest) ([]byte, error)
	DeleteBlob(ctx context.Context, sessionID string, d Digest) error
	DeleteSession(ctx context.Context, sessionID string) error
	// Sessions are the ids that hold at least one body, for the sweep of
	// the bodies a crash left without their session.
	Sessions(ctx context.Context) ([]string, error)
}

// SweepGrace is how long after its id was minted a session's bodies are
// safe from SweepBlobs, since a session being created puts its bodies
// before the session itself appears.
const SweepGrace = time.Hour

// SweepBlobs removes the bodies of every session that gone reports is
// no longer in the store, which a crash between a session's delete and
// its bodies' leaves behind (spec 014). A session minted within
// SweepGrace of now is left alone.
func SweepBlobs(ctx context.Context, b Blobs, gone func(ctx context.Context, id string) (bool, error), now time.Time) error {
	ids, err := b.Sessions(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		minted, err := MintedAt(PrefixSession, id)
		if err != nil || now.Sub(minted) < SweepGrace {
			continue
		}
		missing, err := gone(ctx, id)
		switch {
		case err != nil:
			errs = append(errs, err)
		case missing:
			errs = append(errs, b.DeleteSession(ctx, id))
		}
	}
	return errors.Join(errs...)
}

// Store keeps sessions, their logs and their blobs (spec 004).
type Store interface {
	Create(ctx context.Context, s Session, blobs map[Digest][]byte) error
	Get(ctx context.Context, id string) (Session, error)
	List(ctx context.Context, o ListOptions) ([]Session, string, error)
	Append(ctx context.Context, id string, afterSeq uint64, events []Event) (uint64, error)
	Events(ctx context.Context, id string, fromSeq uint64, limit int) ([]Event, error)
	Watch(ctx context.Context, id string, fromSeq uint64) (<-chan Event, error)
	PutBlob(ctx context.Context, id string, r io.Reader) (Digest, error)
	Blob(ctx context.Context, id string, d Digest) (io.ReadCloser, error)
	Redact(ctx context.Context, id, eventID string, by Sender, reason string) error
	Acquire(ctx context.Context, id string, holder Holder) (Lease, error)
	Delete(ctx context.Context, id string) error
}

// CheckCreate validates a session and its blobs for Create.
func CheckCreate(s Session, blobs map[Digest][]byte) error {
	if err := CheckID(PrefixSession, s.ID); err != nil {
		return err
	}
	if s.Schema != SchemaVersion {
		return fmt.Errorf("%w: schema %d, this build writes %d", ErrInvalid, s.Schema, SchemaVersion)
	}
	if s.LastSeq != 0 {
		return fmt.Errorf("%w: a new session has last_seq 0, got %d", ErrInvalid, s.LastSeq)
	}
	if len(s.Metadata) > MaxMetadata {
		return fmt.Errorf("%w: %d metadata entries, at most %d", ErrInvalid, len(s.Metadata), MaxMetadata)
	}
	for d, b := range blobs {
		if DigestOf(b) != d {
			return fmt.Errorf("%w: %s", ErrBlobMismatch, d)
		}
	}
	return nil
}

// Stamp fills the session id and the sequences of a batch that follows
// afterSeq, in place, and returns the batch's last sequence.
func Stamp(id string, afterSeq uint64, events []Event) uint64 {
	for i := range events {
		events[i].SessionID = id
		events[i].Seq = afterSeq + uint64(i) + 1
	}
	return afterSeq + uint64(len(events))
}

// CheckBatch validates a batch against the stored log. last is the
// session's last sequence; stored returns the stored events from a
// sequence onward, and is called only for a retry. It returns
// retried=true when the batch is an accepted batch sent again, in which
// case the store answers success without writing.
func CheckBatch(id string, last, afterSeq uint64, events []Event, stored func(from uint64) ([]Event, error)) (retried bool, err error) {
	if len(events) == 0 {
		return false, fmt.Errorf("%w: an append carries at least one event", ErrInvalid)
	}
	seen := make(map[string]bool, len(events))
	for i, e := range events {
		if err := CheckID(PrefixEvent, e.ID); err != nil {
			return false, fmt.Errorf("%w: event id %q: %w", ErrInvalid, e.ID, err)
		}
		if seen[e.ID] {
			return false, fmt.Errorf("%w: event %s repeats in the batch", ErrInvalid, e.ID)
		}
		seen[e.ID] = true
		if e.SessionID != id {
			return false, fmt.Errorf("%w: event %s names session %q", ErrInvalid, e.ID, e.SessionID)
		}
		if want := afterSeq + uint64(i) + 1; e.Seq != want {
			return false, fmt.Errorf("%w: event %s has seq %d, want %d", ErrInvalid, e.ID, e.Seq, want)
		}
		if e.Type == "" {
			return false, fmt.Errorf("%w: event %s has no type", ErrInvalid, e.ID)
		}
		if !json.Valid(e.Payload) || !bytes.HasPrefix(bytes.TrimSpace(e.Payload), []byte("{")) {
			return false, fmt.Errorf("%w: event %s payload is not a JSON object", ErrInvalid, e.ID)
		}
		if e.Time.IsZero() {
			return false, fmt.Errorf("%w: event %s has no time", ErrInvalid, e.ID)
		}
	}
	if afterSeq == last {
		return false, nil
	}
	if afterSeq > last {
		return false, fmt.Errorf("%w: after_seq %d, last sequence %d", ErrSequenceConflict, afterSeq, last)
	}
	have, err := stored(afterSeq + 1)
	if err != nil {
		return false, err
	}
	if len(have) < len(events) {
		return false, fmt.Errorf("%w: after_seq %d, last sequence %d", ErrSequenceConflict, afterSeq, last)
	}
	for i, e := range events {
		if !SameEvent(have[i], e) {
			return false, fmt.Errorf("%w: after_seq %d, last sequence %d", ErrSequenceConflict, afterSeq, last)
		}
	}
	return true, nil
}

// SameEvent reports whether a and b are the same event: equal fields and
// the same payload up to insignificant whitespace.
func SameEvent(a, b Event) bool {
	if a.ID != b.ID || a.Seq != b.Seq || a.SessionID != b.SessionID || a.Type != b.Type ||
		!a.Time.Equal(b.Time) || a.Turn != b.Turn || a.Step != b.Step || a.Thread != b.Thread {
		return false
	}
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a.Payload) != nil || json.Compact(&cb, b.Payload) != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// ApplyBatch updates the Session header from appended events: the last
// sequence, the turn, the update time, the status and stop reason of the
// last session.status event, the budget of the last session.resumed, the
// model of the last session.model_changed, and the spend, which each
// model.request's cost adds to as Spent counts it.
func ApplyBatch(s *Session, events []Event) {
	for _, e := range events {
		if e.Seq > s.LastSeq {
			s.LastSeq = e.Seq
		}
		if e.Turn > s.Turn {
			s.Turn = e.Turn
		}
		if t := e.Time.UTC(); t.After(s.UpdatedAt) {
			s.UpdatedAt = t
		}
		if e.Type == TypeModelRequest && !e.Redacted() {
			var p ModelRequest
			if e.Decode(&p) == nil && p.CostUSDMicro != nil {
				s.Budget.SpentCostUSDMicro += *p.CostUSDMicro
			}
			continue
		}
		// A fork's copied session.resumed raised its parent's budget; the
		// fork's own budget is the one it was created with (spec 017).
		if e.Type == TypeSessionResumed && !e.Redacted() && !s.Copied(e) {
			var r SessionResumed
			if e.Decode(&r) == nil {
				s.Budget.MaxCostUSDMicro = r.MaxCostUSDMicro
			}
			continue
		}
		if e.Type == TypeModelChanged && !e.Redacted() {
			var m ModelChanged
			if e.Decode(&m) == nil {
				s.Model = &ModelRef{Name: m.New.Name, Effort: m.New.Effort}
			}
			continue
		}
		if e.Type != TypeSessionStatus || e.Redacted() {
			continue
		}
		var p SessionStatus
		if e.Decode(&p) == nil && p.Status != "" {
			s.Status = p.Status
			s.StopReason = p.StopReason
		}
	}
}

// CheckSequence reports ErrCorrupt when events are not dense from their
// first sequence.
func CheckSequence(events []Event, from uint64) error {
	for i, e := range events {
		if want := from + uint64(i); e.Seq != want {
			return fmt.Errorf("%w: seq %d where %d was expected", ErrCorrupt, e.Seq, want)
		}
	}
	return nil
}

// Tombstone returns e redacted, and the event.redacted event recording
// it, sequenced after last.
func Tombstone(e Event, last uint64, by Sender, reason string, now time.Time) (Event, Event, error) {
	red, err := NewEvent(TypeEventRedacted, EventRedacted{EventID: e.ID, By: by, Reason: reason}, now)
	if err != nil {
		return Event{}, Event{}, err
	}
	red.SessionID = e.SessionID
	red.Seq = last + 1
	e.Payload = slices.Clone(tombstone)
	return e, red, nil
}

// Blobs returns the digests a payload names, in order of appearance,
// each once.
func (e Event) Blobs() []Digest {
	var v any
	if json.Unmarshal(e.Payload, &v) != nil {
		return nil
	}
	var out []Digest
	seen := map[Digest]bool{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if d := Digest(x); d.Valid() && !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		case []any:
			for _, y := range x {
				walk(y)
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				walk(x[k])
			}
		}
	}
	walk(v)
	return out
}

// OrphanBlobs returns the blobs the redacted event named that no other
// event of the log names.
func OrphanBlobs(redacted Event, log []Event) []Digest {
	named := redacted.Blobs()
	if len(named) == 0 {
		return nil
	}
	others := map[Digest]bool{}
	for _, e := range log {
		if e.ID == redacted.ID {
			continue
		}
		for _, d := range e.Blobs() {
			others[d] = true
		}
	}
	var out []Digest
	for _, d := range named {
		if !others[d] {
			out = append(out, d)
		}
	}
	return out
}
