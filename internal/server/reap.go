// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"time"

	"latere.ai/x/topos/session"
)

// ReapInterval is how often serve runs Reap (spec 014).
const ReapInterval = 10 * time.Minute

// Reap is spec 014's reaper. Every idle session past its expires_at is
// ended expired, every ended session past its retention is deleted, and
// the blob bodies a store keeps outside whose session is gone are
// removed: retention runs from the session's last event, its end. A session with
// no retention is kept until someone deletes it. A running session is
// left for a later pass, since its runner holds it and its turn ends
// first, and so is one another writer holds or appends to meanwhile.
// Reap tries every session and returns the errors it met.
func (s *Server) Reap(ctx context.Context) error {
	now := s.o.Now()
	var errs []error
	errs = append(errs, s.eachSession(ctx, session.StatusIdle, func(sess session.Session) error {
		if sess.ExpiresAt.IsZero() || now.Before(sess.ExpiresAt) {
			return nil
		}
		ev, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopExpired}, now)
		if err != nil {
			return err
		}
		batch := []session.Event{ev}
		session.Stamp(sess.ID, sess.LastSeq, batch)
		_, err = s.o.Sessions.Append(ctx, sess.ID, sess.LastSeq, batch)
		return skipHeld(err)
	}))
	errs = append(errs, s.eachSession(ctx, session.StatusEnded, func(sess session.Session) error {
		if sess.Limits.Retention == "" {
			return nil
		}
		keep, err := time.ParseDuration(sess.Limits.Retention)
		if err != nil {
			return err
		}
		if now.Before(sess.UpdatedAt.Add(keep)) {
			return nil
		}
		return skipHeld(s.o.Sessions.Delete(ctx, sess.ID))
	}))
	// A store that keeps blob bodies outside sweeps the ones a crash left
	// after their session's delete.
	if sw, ok := s.o.Sessions.(interface {
		SweepBlobs(context.Context, time.Time) error
	}); ok {
		errs = append(errs, sw.SweepBlobs(ctx, now))
	}
	return errors.Join(errs...)
}

// eachSession calls fn with every session of one status, page by page.
func (s *Server) eachSession(ctx context.Context, status session.Status, fn func(session.Session) error) error {
	var errs []error
	cursor := ""
	for {
		page, next, err := s.o.Sessions.List(ctx, session.ListOptions{Status: status, Cursor: cursor})
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, sess := range page {
			errs = append(errs, fn(sess))
		}
		if next == "" {
			return errors.Join(errs...)
		}
		cursor = next
	}
}

// skipHeld is no error for a session another writer holds or appended to
// meanwhile, which the next pass takes up again.
func skipHeld(err error) error {
	if errors.Is(err, session.ErrLocked) || errors.Is(err, session.ErrSequenceConflict) || errors.Is(err, session.ErrNotFound) {
		return nil
	}
	return err
}
