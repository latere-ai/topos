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

// ClaimWait is how long one claim waits for work before it answers
// empty (spec 016).
const ClaimWait = 20 * time.Second

// claimBackoff is the pause after a claim that failed, so a store that
// is down is not asked in a tight loop.
const claimBackoff = time.Second

// Serve claims sessions with work and drives each, at most capacity at a
// time, until ctx ends; then it waits for the sessions it is driving,
// whose turns the canceled context brings to their step boundary. report
// receives every session a drive ended with an error, a setup failure
// included; nil discards them. A session another holder took first is
// not an error.
func (r *Runner) Serve(ctx context.Context, c Claimer, capacity int, report func(id string, err error)) error {
	if capacity < 1 {
		return errors.New("runner: a capacity of at least one session")
	}
	if report == nil {
		report = func(string, error) {}
	}
	var (
		mu     sync.Mutex
		active = map[string]bool{}
		wg     sync.WaitGroup
		freed  = make(chan struct{}, capacity)
	)
	defer wg.Wait()
	for ctx.Err() == nil {
		mu.Lock()
		free := capacity - len(active)
		mu.Unlock()
		if free == 0 {
			select {
			case <-freed:
			case <-ctx.Done():
			}
			continue
		}
		claims, err := c.Claim(ctx, r.holder(), free, ClaimWait)
		if err != nil {
			report("", err)
			sleep(ctx, claimBackoff)
			continue
		}
		for _, cl := range claims {
			mu.Lock()
			if active[cl.ID] {
				mu.Unlock()
				report(cl.ID, cl.Lease.Release())
				continue
			}
			active[cl.ID] = true
			mu.Unlock()
			wg.Add(1)
			go func(cl Claim) {
				defer wg.Done()
				_, err := r.drive(ctx, cl.ID, cl.Lease, true)
				if err != nil && ctx.Err() == nil && !errors.Is(err, session.ErrLocked) && !errors.Is(err, ErrEnded) {
					report(cl.ID, err)
				}
				mu.Lock()
				delete(active, cl.ID)
				mu.Unlock()
				select {
				case freed <- struct{}{}:
				default:
				}
			}(cl)
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
