// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package triggers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// A trigger's allowance and pace (spec 087). A look is a turn a
// trigger's message began (session.Looks). The looks counted toward a
// trigger are the ones its firings began, in the sessions it started and
// the ones it continued, and the ones wakes began in the sessions it
// started. Its spend is their cost over the rolling trigger.SpendWindow
// before a firing; a firing while the spend of the trigger it counts
// toward is at that trigger's allowance starts nothing.

// maxScheduleLooks bounds the fire times a schedule is walked through in
// one window: a schedule that fires every minute fires this often in
// seven days.
const maxScheduleLooks = 7 * 24 * 60

// pageSize is the page the firings and the sessions are read in.
const pageSize = 100

// counting is the trigger a firing of t counts toward, with its spec: t
// itself, or, for a wake, the trigger that started the session the wake
// continues. A wake of a session no trigger started, or whose trigger is
// gone, counts toward none, and ok is false.
func (e *Engine) counting(ctx context.Context, t store.Trigger, spec v1.TriggerSpec) (store.Trigger, v1.TriggerSpec, bool, error) {
	doc, err := store.DecodeTrigger(t.Doc)
	if err != nil {
		return store.Trigger{}, v1.TriggerSpec{}, false, err
	}
	if o := doc.Status.Origin; o == nil || o.Kind != v1.OriginWake {
		return t, spec, true, nil
	}
	s, err := e.o.Sessions.Get(ctx, spec.Session.Target)
	switch {
	case errors.Is(err, session.ErrNotFound):
		return store.Trigger{}, v1.TriggerSpec{}, false, nil
	case err != nil:
		return store.Trigger{}, v1.TriggerSpec{}, false, err
	case s.TriggerID == "":
		return store.Trigger{}, v1.TriggerSpec{}, false, nil
	}
	starter, err := e.o.Store.Trigger(ctx, s.TriggerID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Trigger{}, v1.TriggerSpec{}, false, nil
	}
	if err != nil {
		return store.Trigger{}, v1.TriggerSpec{}, false, err
	}
	sspec, err := Spec(starter)
	if err != nil {
		return store.Trigger{}, v1.TriggerSpec{}, false, err
	}
	return starter, sspec, true, nil
}

// allowance counts the week of the trigger a firing of t counts toward,
// keeps it as that trigger's status.spend, and answers it as the firing's
// message carries it, and whether the spend is at or past the allowance.
// A firing that counts toward no trigger answers nil and false.
func (e *Engine) allowance(ctx context.Context, t store.Trigger, spec v1.TriggerSpec) (*session.MessageSpend, bool, error) {
	ct, cspec, ok, err := e.counting(ctx, t, spec)
	if err != nil || !ok {
		return nil, false, err
	}
	sp, err := e.Spend(ctx, ct, cspec, e.o.Now().UTC())
	if err != nil {
		return nil, false, err
	}
	// A trigger deleted since it was read keeps nothing; the firing is
	// decided on what was counted.
	if err := e.o.Store.SetSpend(ctx, ct.ID, sp); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	m := &session.MessageSpend{TriggerID: ct.ID, WindowDays: sp.WindowDays, SpentUSDMicro: sp.SpentUSDMicro, PerWeekUSDMicro: sp.PerWeekUSDMicro,
		PerLookUSDMicro: sp.PerLookUSDMicro, ExpectedLooks: sp.ExpectedLooks, ProjectedUSDMicro: sp.ProjectedUSDMicro}
	over := sp.PerWeekUSDMicro != nil && sp.SpentUSDMicro >= *sp.PerWeekUSDMicro
	return m, over, nil
}

// Spend is the week of trigger t at now, a firing's time (spec 087): the
// cost of the looks counted toward it in the window before now, its
// allowance, the mean cost of the window's latest trigger.PaceLooks
// looks that ended, the looks expected from the firing to the end of the
// window, the firing's own included, and the projection. The window ends
// trigger.SpendWindow after the oldest look counted, or after now when
// none is, so the projection is the spend of one window, the one whose
// end the ceiling reaches first. The sessions read are the ones its
// firings started or continued in the window, its sessions still open,
// and its session.target.
func (e *Engine) Spend(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, now time.Time) (v1.TriggerSpend, error) {
	since := now.Add(-trigger.SpendWindow)
	ids, err := e.watched(ctx, t, spec, since)
	if err != nil {
		return v1.TriggerSpend{}, err
	}
	var looks []session.Look
	for _, id := range ids {
		s, err := e.o.Sessions.Get(ctx, id)
		if errors.Is(err, session.ErrNotFound) {
			continue
		}
		if err != nil {
			return v1.TriggerSpend{}, err
		}
		evs, err := e.o.Sessions.Events(ctx, id, 1, 0)
		if err != nil {
			return v1.TriggerSpend{}, fmt.Errorf("triggers: the log of %s: %w", id, err)
		}
		for _, l := range session.Looks(s, evs) {
			if !l.At.After(since) || l.At.After(now) {
				continue
			}
			if l.TriggerID == t.ID || l.Wake && s.TriggerID == t.ID {
				looks = append(looks, l)
			}
		}
	}
	slices.SortStableFunc(looks, func(a, b session.Look) int { return a.At.Compare(b.At) })

	sp := v1.TriggerSpend{WindowDays: trigger.SpendWindowDays}
	if b := spec.Budget; b != nil {
		sp.PerWeekUSDMicro = new(b.PerWeek)
	}
	var ended []int64
	for _, l := range looks {
		sp.SpentUSDMicro += l.CostUSDMicro
		if l.Ended {
			ended = append(ended, l.CostUSDMicro)
		}
	}
	end := now.Add(trigger.SpendWindow)
	if len(looks) > 0 {
		end = looks[0].At.Add(trigger.SpendWindow)
	}
	sp.ExpectedLooks, err = expectedLooks(t, spec, len(looks), now, end)
	if err != nil {
		return v1.TriggerSpend{}, err
	}
	if n := len(ended); n > 0 {
		recent := ended[max(n-trigger.PaceLooks, 0):]
		var sum int64
		for _, c := range recent {
			sum += c
		}
		perLook := sum / int64(len(recent))
		sp.PerLookUSDMicro = &perLook
		sp.ProjectedUSDMicro = new(sp.SpentUSDMicro + perLook*int64(sp.ExpectedLooks))
	}
	return sp, nil
}

// watched are the sessions whose logs hold t's looks in the window after
// since: the sessions its firings started or continued since then, its
// sessions still open, whose wakes may fire now, and its session.target.
func (e *Engine) watched(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, since time.Time) ([]string, error) {
	var ids []string
	add := func(id string) {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	cursor := ""
	for {
		fs, next, err := e.o.Store.Firings(ctx, t.ID, pageSize, cursor)
		if err != nil {
			return nil, err
		}
		older := false
		for _, f := range fs {
			// Firings list newest first, so the first one received before
			// the window ends the walk.
			if f.ReceivedAt.Before(since) {
				older = true
				break
			}
			add(f.SessionID)
		}
		if older || next == "" {
			break
		}
		cursor = next
	}
	for _, st := range []session.Status{session.StatusIdle, session.StatusRunning} {
		cursor := ""
		for {
			ss, next, err := e.o.Sessions.List(ctx, session.ListOptions{Trigger: t.ID, Status: st, Limit: pageSize, Cursor: cursor})
			if err != nil {
				return nil, err
			}
			for _, s := range ss {
				add(s.ID)
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	add(spec.Session.Target)
	return ids, nil
}

// expectedLooks are the looks t expects from now to end, the firing's
// own included: then a schedule's fire times before end, none more for
// a one-shot, whose one look is the firing's, and for events the looks of
// the window spread over the time left, their count over seven days or
// the trigger's age when it is younger, at least a day; a suspended
// trigger expects none more.
func expectedLooks(t store.Trigger, spec v1.TriggerSpec, seen int, now, end time.Time) (int, error) {
	n := 1
	switch {
	case spec.Suspend || spec.At != nil || !end.After(now):
	case spec.On != nil:
		span := min(trigger.SpendWindow, max(now.Sub(t.CreatedAt), 24*time.Hour))
		n += int(math.Round(float64(seen) * float64(end.Sub(now)) / float64(span)))
	case spec.Schedule != "":
		at := now
		for range maxScheduleLooks {
			next, err := NextFire(spec, at)
			if err != nil {
				return 0, err
			}
			if next == nil || !next.Before(end) {
				break
			}
			n++
			at = *next
		}
	}
	return n, nil
}
