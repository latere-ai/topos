// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
)

// ErrInvalidForkPoint is a fork at a sequence that is not a turn
// boundary, or of a session that has none (spec 017), or before an event
// that is no person's message that opened a turn (spec 054).
var ErrInvalidForkPoint = errors.New("session: invalid_fork_point")

// ForkPoint is the sequence a fork copies a log to: at when it names a
// turn boundary, and the last boundary when at is nil. A log is dense
// from 1, so the copy is evs[:seq].
func ForkPoint(evs []Event, at *uint64) (uint64, error) {
	marks := boundaries(evs)
	if at != nil {
		if *at == 0 || *at > uint64(len(evs)) || !marks[*at-1] {
			return 0, fmt.Errorf("%w: at_seq %d is not a turn boundary, a session.status of the session's own thread that is idle or an end that closed a finished turn", ErrInvalidForkPoint, *at)
		}
		return *at, nil
	}
	for i, e := range slices.Backward(evs) {
		if marks[i] {
			return e.Seq, nil
		}
	}
	return 0, fmt.Errorf("%w: the session has not finished a turn, so it has no turn boundary to fork at", ErrInvalidForkPoint)
}

// ForkBefore is the sequence a fork that replaces the person's message
// at before copies a log to: before - 1 (spec 054). before names a
// user.message of the session's own thread, not redacted, whose sender
// is a person, and which opened a turn: the last session.status of the
// session's own thread before it is idle, whatever its stop reason, or
// the log holds none, as before the session's opening message, which
// copies what precedes it, nothing at all when it is the first event. A
// message sent while a turn ran steered that turn and opened none; a
// trigger's message is its owner's text, and a service's is no person's
// to edit; a redacted message's sender is gone with its content.
func ForkBefore(evs []Event, before uint64) (uint64, error) {
	if before == 0 || before > uint64(len(evs)) {
		return 0, fmt.Errorf("%w: before_seq %d names no event of the log, which ends at %d", ErrInvalidForkPoint, before, len(evs))
	}
	e := evs[before-1]
	if e.Type != TypeUserMessage || e.Thread != "" {
		return 0, fmt.Errorf("%w: before_seq %d is a %s, not a user.message of the session's own thread", ErrInvalidForkPoint, before, e.Type)
	}
	if e.Redacted() {
		return 0, fmt.Errorf("%w: the user.message at before_seq %d is redacted", ErrInvalidForkPoint, before)
	}
	var m UserMessage
	if err := e.Decode(&m); err != nil {
		return 0, fmt.Errorf("%w: the user.message at before_seq %d: %w", ErrInvalidForkPoint, before, err)
	}
	switch m.Sender.Kind {
	case SenderPerson:
	case SenderTrigger:
		return 0, fmt.Errorf("%w: the user.message at before_seq %d is a trigger's, whose text is its owner's and is not edited", ErrInvalidForkPoint, before)
	default:
		return 0, fmt.Errorf("%w: the user.message at before_seq %d is from a %q sender, not a person", ErrInvalidForkPoint, before, m.Sender.Kind)
	}
	for _, prev := range slices.Backward(evs[:before-1]) {
		if p, ok := ownStatus(prev); ok {
			if p.Status != StatusIdle {
				return 0, fmt.Errorf("%w: the user.message at before_seq %d was sent while the session was %s, so it opened no turn", ErrInvalidForkPoint, before, p.Status)
			}
			break
		}
	}
	return before - 1, nil
}

// boundaries marks the turn boundaries of a log (spec 017): each
// session.status of the session's own thread that is idle, whatever its
// stop reason, since the session takes its next input there, and each
// that is ended completed straight after the thread's running, the end a
// session created with end_on_idle writes in place of idle end_turn. An
// end after idle closes no turn, as the end route and the reaper end
// only an idle session, and an end failed, canceled or expired after
// running closes a turn that did not finish.
func boundaries(evs []Event) []bool {
	marks := make([]bool, len(evs))
	var prev Status
	for i, e := range evs {
		p, ok := ownStatus(e)
		if !ok {
			continue
		}
		switch p.Status {
		case StatusIdle:
			marks[i] = true
		case StatusEnded:
			marks[i] = p.StopReason == StopCompleted && prev == StatusRunning
		}
		prev = p.Status
	}
	return marks
}

// ownStatus decodes a session.status of the session's own thread.
func ownStatus(e Event) (SessionStatus, bool) {
	if e.Type != TypeSessionStatus || e.Thread != "" || e.Redacted() {
		return SessionStatus{}, false
	}
	var p SessionStatus
	return p, e.Decode(&p) == nil
}

// restate is a fork point's event as the fork keeps it: an end that
// closed a finished turn becomes idle end_turn, the status the turn
// closed with for a session without end_on_idle, so the fork waits for
// its next message where the old session ended. Every other field of the
// payload is kept, those a later schema adds included; any other event
// is e itself.
func restate(e Event) (Event, error) {
	if p, ok := ownStatus(e); !ok || p.Status != StatusEnded {
		return e, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(e.Payload, &fields); err != nil {
		return Event{}, fmt.Errorf("session: restate the fork point %s: %w", e.ID, err)
	}
	for k, v := range map[string]any{"status": StatusIdle, "stop_reason": StopEndTurn} {
		b, err := Marshal(v)
		if err != nil {
			return Event{}, err
		}
		fields[k] = b
	}
	b, err := Marshal(fields)
	if err != nil {
		return Event{}, fmt.Errorf("session: restate the fork point %s: %w", e.ID, err)
	}
	e.Payload = b
	return e, nil
}

// Fork writes child as a fork of parent at the end of events, parent's
// log from 1 to the fork point, followed by then, the events the fork
// starts with, such as a person's message that replaces the one after
// the fork point (spec 054): child with its parent link, its tree's root
// and the copied spend, the blobs given (the agent's, and a new
// message's files) and every blob of parent's the events and then name,
// then the copy and then as one batch, the copy's ids, times, turns and
// steps kept and the session id child's, a fork point that is an end
// restated as idle end_turn. The store's header takes the fork point's
// status, the spend and the last model switch from the batch. A write
// that fails deletes child, so no half-copied session remains, and no
// fork without the message it was made for.
func Fork(ctx context.Context, st Store, child Session, blobs map[Digest][]byte, parent Session, events []Event, then ...Event) (Session, error) {
	child.Parent = &Parent{SessionID: parent.ID, Seq: uint64(len(events))}
	child.Root = parent.TreeRoot()
	child.Budget.CarriedCostUSDMicro = Spent(events)
	all := maps.Clone(blobs)
	if all == nil {
		all = map[Digest][]byte{}
	}
	if n := len(events); n > 0 && boundaries(events)[n-1] {
		last, err := restate(events[n-1])
		if err != nil {
			return Session{}, err
		}
		events = append(events[:n-1:n-1], last)
	}
	batch := make([]Event, 0, len(events)+len(then))
	for _, e := range slices.Concat(events, then) {
		for _, d := range e.Blobs() {
			if _, ok := all[d]; ok {
				continue
			}
			b, err := readBlob(ctx, st, parent.ID, d)
			if errors.Is(err, ErrNotFound) {
				// A digest-shaped string in a payload is not always a blob
				// of the session, as a digest a tool printed is not.
				continue
			}
			if err != nil {
				return Session{}, err
			}
			all[d] = b
		}
		e.SessionID = child.ID
		batch = append(batch, e)
	}
	Stamp(child.ID, uint64(len(events)), batch[len(events):])
	if err := st.Create(ctx, child, all); err != nil {
		return Session{}, err
	}
	if len(batch) > 0 {
		if _, err := st.Append(ctx, child.ID, 0, batch); err != nil {
			return Session{}, errors.Join(fmt.Errorf("session: copy the log of %s: %w", parent.ID, err), st.Delete(context.WithoutCancel(ctx), child.ID))
		}
	}
	return st.Get(ctx, child.ID)
}

// readBlob reads one blob of a session whole.
func readBlob(ctx context.Context, st Store, id string, d Digest) ([]byte, error) {
	rc, err := st.Blob(ctx, id, d)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(rc)
	return b, errors.Join(err, rc.Close())
}
