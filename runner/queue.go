// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"sync"
	"time"

	"latere.ai/x/topos/session"
)

// Claim is one session a runner drives, with the lease it holds on it.
type Claim struct {
	ID    string
	Lease session.Lease
}

// Claimer hands a runner the sessions that have work (spec 016).
type Claimer interface {
	// Claim takes up to n sessions that have work for holder, waiting up
	// to wait for the first one, and returns their leases. An empty
	// answer is no work within the wait.
	Claim(ctx context.Context, holder session.Holder, n int, wait time.Duration) ([]Claim, error)
}

// DefaultPoll is how often a Queue looks for work when nothing woke it.
const DefaultPoll = 2 * time.Second

// Queue is the claim queue of a server that runs its hosted sessions in
// process, over any session.Store: a session is claimable when its
// writer is hosted, it has not ended, and it is idle with pending input
// or running with no live lease. Taking the claim is taking the store's
// lease, so a session another runner holds is never handed out.
type Queue struct {
	st   session.Store
	poll time.Duration
	wake chan struct{}

	mu sync.Mutex
	// quiet is the last sequence of each idle session found without
	// pending input, so an unchanged session is not read again.
	quiet map[string]uint64
}

// NewQueue returns the queue over st, polling every poll (DefaultPoll
// when zero) and at once when Notify is called.
func NewQueue(st session.Store, poll time.Duration) *Queue {
	if poll <= 0 {
		poll = DefaultPoll
	}
	return &Queue{st: st, poll: poll, wake: make(chan struct{}, 1), quiet: map[string]uint64{}}
}

// Notify wakes a waiting Claim: a session was created or gained input.
func (q *Queue) Notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Claim takes up to n sessions with work.
func (q *Queue) Claim(ctx context.Context, holder session.Holder, n int, wait time.Duration) ([]Claim, error) {
	deadline := time.Now().Add(wait)
	for {
		select {
		case <-ctx.Done():
			// A claim its runner stopped waiting for has no work to hand.
			return nil, nil
		default:
		}
		claims, err := q.scan(ctx, holder, n)
		if err != nil || len(claims) > 0 {
			return claims, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, nil
		}
		t := time.NewTimer(min(left, q.poll))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, nil
		case <-q.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// scan walks the hosted sessions that are idle or running and leases the
// ones with work, up to n.
func (q *Queue) scan(ctx context.Context, holder session.Holder, n int) ([]Claim, error) {
	var claims []Claim
	for _, status := range []session.Status{session.StatusRunning, session.StatusIdle} {
		cursor := ""
		for len(claims) < n {
			page, next, err := q.st.List(ctx, session.ListOptions{Status: status, Runner: session.RunnerHosted, Cursor: cursor, Limit: 200})
			if err != nil {
				return claims, errors.Join(err, release(claims))
			}
			for _, s := range page {
				if len(claims) == n {
					break
				}
				c, ok, err := q.take(ctx, holder, s)
				if err != nil {
					return claims, errors.Join(err, release(claims))
				}
				if ok {
					claims = append(claims, c)
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	return claims, nil
}

// take leases one session when it has work. The check is made once
// before the lease, cheaply, and again under it, since the session may
// have moved in between.
func (q *Queue) take(ctx context.Context, holder session.Holder, s session.Session) (Claim, bool, error) {
	if s.Writer != nil && s.Writer.Kind != session.RunnerHosted {
		return Claim{}, false, nil
	}
	if s.Status == session.StatusIdle {
		q.mu.Lock()
		seen, known := q.quiet[s.ID]
		q.mu.Unlock()
		if known && seen == s.LastSeq {
			return Claim{}, false, nil
		}
	}
	lease, err := q.st.Acquire(ctx, s.ID, holder)
	if errors.Is(err, session.ErrLocked) || errors.Is(err, session.ErrNotFound) {
		return Claim{}, false, nil
	}
	if err != nil {
		return Claim{}, false, err
	}
	work, err := q.hasWork(ctx, s.ID)
	if err != nil || !work {
		return Claim{}, false, errors.Join(err, lease.Release())
	}
	q.mu.Lock()
	delete(q.quiet, s.ID)
	q.mu.Unlock()
	return Claim{ID: s.ID, Lease: lease}, true, nil
}

// hasWork reads the session under its lease: a running session the
// lease was free for is one whose runner went away, and an idle one has
// work when input follows its last status.
func (q *Queue) hasWork(ctx context.Context, id string) (bool, error) {
	s, err := q.st.Get(ctx, id)
	if err != nil {
		return false, err
	}
	switch s.Status {
	case session.StatusRunning:
		return true, nil
	case session.StatusIdle:
		evs, err := q.st.Events(ctx, id, 1, 0)
		if err != nil {
			return false, err
		}
		if session.HasPendingInput(evs) {
			return true, nil
		}
		q.mu.Lock()
		q.quiet[id] = s.LastSeq
		q.mu.Unlock()
	}
	return false, nil
}

func release(claims []Claim) error {
	var errs []error
	for _, c := range claims {
		errs = append(errs, c.Lease.Release())
	}
	return errors.Join(errs...)
}
