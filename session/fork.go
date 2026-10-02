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
// boundary, or of a session that has none (spec 017).
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

// Fork writes child as a fork of the session parent at the end of
// events, that session's log from 1 to the fork point: child with its
// parent link, the blobs given (the agent's) and every blob of parent's
// the events name, then the events as one batch, ids, times, turns and
// steps kept and the session id child's, a fork point that is an end
// restated as idle end_turn. The store's header takes the fork point's
// status, the copied spend and the last model switch from the batch. A
// copy that fails deletes child, so no half-copied session remains.
func Fork(ctx context.Context, st Store, child Session, blobs map[Digest][]byte, parent string, events []Event) (Session, error) {
	child.Parent = &Parent{SessionID: parent, Seq: uint64(len(events))}
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
	copied := make([]Event, len(events))
	for i, e := range events {
		for _, d := range e.Blobs() {
			if _, ok := all[d]; ok {
				continue
			}
			b, err := readBlob(ctx, st, parent, d)
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
		copied[i] = e
	}
	if err := st.Create(ctx, child, all); err != nil {
		return Session{}, err
	}
	if len(copied) > 0 {
		if _, err := st.Append(ctx, child.ID, 0, copied); err != nil {
			return Session{}, errors.Join(fmt.Errorf("session: copy the log of %s: %w", parent, err), st.Delete(context.WithoutCancel(ctx), child.ID))
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
