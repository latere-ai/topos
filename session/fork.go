// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
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
// turn boundary, a session.status idle of the session's own thread, and
// the last such boundary when at is nil, which for an ended session is
// the one before its end. A log is dense from 1, so the copy is
// evs[:seq].
func ForkPoint(evs []Event, at *uint64) (uint64, error) {
	if at != nil {
		if *at == 0 || *at > uint64(len(evs)) || !boundary(evs[*at-1]) {
			return 0, fmt.Errorf("%w: at_seq %d is not the sequence of a session.status idle of the session's own thread", ErrInvalidForkPoint, *at)
		}
		return *at, nil
	}
	for _, e := range slices.Backward(evs) {
		if boundary(e) {
			return e.Seq, nil
		}
	}
	return 0, fmt.Errorf("%w: the session has not finished a turn, so it has no turn boundary to fork at", ErrInvalidForkPoint)
}

// boundary reports a turn boundary: a session.status idle of the
// session's own thread.
func boundary(e Event) bool {
	if e.Type != TypeSessionStatus || e.Thread != "" || e.Redacted() {
		return false
	}
	var p SessionStatus
	return e.Decode(&p) == nil && p.Status == StatusIdle
}

// Fork writes child as a fork of the session parent at the end of
// events, that session's log from 1 to the fork point: child with its
// parent link, the blobs given (the agent's) and every blob of parent's
// the events name, then the events as one batch, ids, times, turns and
// steps kept and the session id child's. The store's header takes the
// fork point's status, the copied spend and the last model switch from
// the batch. A copy that fails deletes child, so no half-copied session
// remains.
func Fork(ctx context.Context, st Store, child Session, blobs map[Digest][]byte, parent string, events []Event) (Session, error) {
	child.Parent = &Parent{SessionID: parent, Seq: uint64(len(events))}
	all := maps.Clone(blobs)
	if all == nil {
		all = map[Digest][]byte{}
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
