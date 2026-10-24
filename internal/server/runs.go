// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/internal/triggers"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// The read of an owner's runs (spec 088) gives a client that forecasts a
// person's month the facts of each trigger: its runs since a time with
// their cost, the mean cost of its latest runs, and the times it fires up
// to a time. The arithmetic of a forecast is the client's.

// The ways a trigger fires, as the read names them.
const (
	firesOnSchedule = "schedule"
	firesOnEvent    = "event"
	firesOnAt       = "at"
)

// triggerRuns is one trigger as GET /triggers/runs answers it.
type triggerRuns struct {
	TriggerID string `json:"trigger_id"`
	Name      string `json:"name"`
	// CreatedAt is the trigger's first apply.
	CreatedAt time.Time `json:"created_at"`
	// FiresOn is schedule, event or at.
	FiresOn   string `json:"fires_on"`
	Suspended bool   `json:"suspended"`
	// PerWeekUSDMicro is spec.budget.perWeek, absent without one.
	PerWeekUSDMicro *int64 `json:"per_week_usd_micro,omitempty"`
	// MaxAge is spec.maxAge: how late a firing may run after its time, so
	// how long a one-shot that waits may wait.
	MaxAge string `json:"max_age"`
	// Wake reports a wake a session set (spec 086).
	Wake bool `json:"wake"`
	// Target is the one session every firing continues, a wake's or the
	// one session.target names.
	Target *runTarget `json:"target,omitempty"`
	// Runs are the runs from since, oldest first.
	Runs []triggerRun `json:"runs"`
	// MoreRuns reports older runs from since left out.
	MoreRuns bool `json:"more_runs"`
	// PerRunUSDMicro is the mean cost of the latest trigger.PaceLooks runs
	// of its own firings that ran and ended.
	PerRunUSDMicro *int64 `json:"per_run_usd_micro,omitempty"`
	// Fires are the times it fires after now and before until.
	Fires []time.Time `json:"fires"`
	// MoreFires reports fire times before until left out.
	MoreFires bool `json:"more_fires"`
}

// runTarget is the one session a trigger's firings continue.
type runTarget struct {
	SessionID string `json:"session_id"`
	// TriggerID is the trigger that started the session, absent for a
	// session a person started.
	TriggerID string `json:"trigger_id,omitempty"`
}

// triggerRun is one run as the read answers it.
type triggerRun struct {
	FiringID     string    `json:"firing_id"`
	At           time.Time `json:"at"`
	Outcome      string    `json:"outcome"`
	Reason       string    `json:"reason,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
	Wake         bool      `json:"wake"`
	CostUSDMicro int64     `json:"cost_usd_micro"`
	Ended        bool      `json:"ended"`
}

// runsWindow is the read's since and until, checked against now.
func (c *call) runsWindow(now time.Time) (since, until time.Time, err error) {
	q := c.r.URL.Query()
	since, until = now.Add(-trigger.RunsSinceDefault), now.Add(trigger.SpendWindow)
	read := func(name string, at *time.Time, lo, hi time.Time) error {
		raw := q.Get(name)
		if raw == "" {
			return nil
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return refuse(CodeInvalidRequest, "%s is %q, not an RFC 3339 time", name, raw)
		}
		if t.Before(lo) || t.After(hi) {
			return refuse(CodeInvalidRequest, "%s is %s, not between %s and %s", name, raw, lo.Format(time.RFC3339), hi.Format(time.RFC3339))
		}
		*at = t.UTC()
		return nil
	}
	if err := read("since", &since, now.Add(-trigger.RunsKept), now); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if err := read("until", &until, now, now.Add(trigger.RunsAhead)); err != nil {
		return time.Time{}, time.Time{}, err
	}
	return since, until, nil
}

// listTriggerRuns is GET /triggers/runs (spec 088): the triggers the list
// would answer, each with its runs since since, the mean cost of its
// latest runs, and its fire times up to until. Before it answers, it
// records each firing since since that has no run, and completes each
// run whose turn had not ended from its session's log.
func (c *call) listTriggerRuns() error {
	limit, cursor, err := c.pageParams()
	if err != nil {
		return err
	}
	now := c.s.o.Now().UTC()
	since, until, err := c.runsWindow(now)
	if err != nil {
		return err
	}
	list, next, err := c.triggerPage(limit, cursor)
	if err != nil {
		return err
	}
	ctx := c.r.Context()
	items := make([]triggerRuns, 0, len(list))
	byOwner := map[string]map[string]store.Run{}
	for _, t := range list {
		if byOwner[t.Owner] != nil {
			continue
		}
		rs, err := c.s.o.Objects.Runs(ctx, t.Owner, now.Add(-trigger.RunsKept))
		if err != nil {
			return err
		}
		m := make(map[string]store.Run, len(rs))
		for _, r := range rs {
			m[r.FiringID] = r
		}
		byOwner[t.Owner] = m
	}
	docs := make([]*v1.Trigger, len(list))
	for i, t := range list {
		if docs[i], err = store.DecodeTrigger(t.Doc); err != nil {
			return err
		}
		if !isWake(docs[i]) {
			if err := c.s.recordMissingRuns(ctx, t, docs[i].Spec, since, byOwner[t.Owner]); err != nil {
				return err
			}
		}
	}
	for _, runs := range byOwner {
		if err := c.s.completeRuns(ctx, runs); err != nil {
			return err
		}
	}
	for i, t := range list {
		item, err := c.s.runsOf(ctx, t, docs[i], byOwner[t.Owner], now, since, until)
		if err != nil {
			return err
		}
		items = append(items, item)
	}
	return c.replyPage(items, next)
}

// recordMissingRuns records each firing of t since since that is a run
// and has none in runs: a firing from before spec 088, or one whose run's
// write failed. A wake's firings are deleted with it, so only a trigger
// that is no wake is read this way.
func (s *Server) recordMissingRuns(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, since time.Time, runs map[string]store.Run) error {
	cursor := ""
	for {
		fs, next, err := s.o.Objects.Firings(ctx, t.ID, MaxLimit, cursor)
		if err != nil {
			return err
		}
		for _, f := range fs {
			// Firings list newest first, so the first one before since ends
			// the walk.
			if f.Time.Before(since) {
				return nil
			}
			if _, ok := runs[f.ID]; ok || !store.IsRun(f.Outcome) {
				continue
			}
			r := triggers.RunOf(t, spec, f)
			if err := s.o.Objects.PutRun(ctx, r); err != nil {
				return err
			}
			runs[f.ID] = r
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// completeRuns writes the cost of each run in runs that ran and has not
// ended, read from its session's log once per session, into the store and
// into runs. A session that is gone holds nothing more to count, so its
// runs end with the cost they hold.
func (s *Server) completeRuns(ctx context.Context, runs map[string]store.Run) error {
	var sessions []string
	for _, r := range runs {
		if r.Ran() && !r.Ended && r.SessionID != "" && !slices.Contains(sessions, r.SessionID) {
			sessions = append(sessions, r.SessionID)
		}
	}
	slices.Sort(sessions)
	var costs []store.RunCost
	for _, id := range sessions {
		read, err := s.lookCosts(ctx, id)
		if err != nil {
			return err
		}
		for _, r := range runs {
			if r.SessionID != id || !r.Ran() || r.Ended {
				continue
			}
			c, ok := read[r.FiringID]
			if !ok {
				// A session gone, or a log that no longer holds the look,
				// leaves nothing more to count.
				c = store.RunCost{FiringID: r.FiringID, CostUSDMicro: r.CostUSDMicro, Ended: read == nil}
			}
			if next := r.Costed(c); next != r {
				runs[r.FiringID] = next
				costs = append(costs, c)
			}
		}
	}
	return s.o.Objects.SetRunCosts(ctx, costs)
}

// lookCosts are the costs of the looks session id's log holds, by firing
// (spec 087's count); nil for a session that is gone.
func (s *Server) lookCosts(ctx context.Context, id string) (map[string]store.RunCost, error) {
	sess, err := s.o.Sessions.Get(ctx, id)
	if errors.Is(err, session.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	evs, err := s.o.Sessions.Events(ctx, id, 1, 0)
	if err != nil {
		return nil, fmt.Errorf("server: the log of %s: %w", id, err)
	}
	out := map[string]store.RunCost{}
	for _, l := range session.Looks(sess, evs) {
		out[l.FiringID] = store.RunCost{FiringID: l.FiringID, CostUSDMicro: l.CostUSDMicro, Ended: l.Ended}
	}
	return out, nil
}

// runsOf is trigger t's item of the read: its fields, its runs from since
// among the owner's runs, the mean of its latest own runs, and its fire
// times after now and before until.
func (s *Server) runsOf(ctx context.Context, t store.Trigger, doc *v1.Trigger, owned map[string]store.Run, now, since, until time.Time) (triggerRuns, error) {
	spec := doc.Spec
	item := triggerRuns{TriggerID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt.UTC(), FiresOn: firesOnSchedule, Suspended: spec.Suspend, Wake: isWake(doc),
		MaxAge: spec.MaxAge, Runs: []triggerRun{}, Fires: []time.Time{}}
	switch {
	case spec.At != nil:
		item.FiresOn = firesOnAt
	case spec.On != nil:
		item.FiresOn = firesOnEvent
	}
	if b := spec.Budget; b != nil {
		item.PerWeekUSDMicro = new(b.PerWeek)
	}
	if target := spec.Session.Target; target != "" {
		item.Target = &runTarget{SessionID: target}
		sess, err := s.o.Sessions.Get(ctx, target)
		switch {
		case errors.Is(err, session.ErrNotFound):
		case err != nil:
			return triggerRuns{}, err
		default:
			item.Target.TriggerID = sess.TriggerID
		}
	}
	// A wake's runs are its session's wakes, each once its own trigger; a
	// trigger's are its own and those of the wakes in the sessions it
	// started, which count toward it (spec 087).
	counts, own := func(r store.Run) bool { return r.TriggerID == t.ID || r.Wake && r.SessionTriggerID == t.ID },
		func(r store.Run) bool { return r.TriggerID == t.ID && !r.Wake }
	if item.Wake {
		target := spec.Session.Target
		counts = func(r store.Run) bool { return r.Wake && r.SessionID == target }
		own = counts
	}
	var mine, ran []store.Run
	for _, r := range owned {
		if counts(r) && !r.At.Before(since) {
			mine = append(mine, r)
		}
		if own(r) && r.Ran() && r.Ended {
			ran = append(ran, r)
		}
	}
	slices.SortFunc(mine, compareRuns)
	if n := len(mine); n > trigger.MaxRunsListed {
		mine, item.MoreRuns = mine[n-trigger.MaxRunsListed:], true
	}
	for _, r := range mine {
		item.Runs = append(item.Runs, triggerRun{FiringID: r.FiringID, At: r.At, Outcome: r.Outcome, Reason: r.Reason, SessionID: r.SessionID, Wake: r.Wake,
			CostUSDMicro: r.CostUSDMicro, Ended: r.Ended})
	}
	slices.SortFunc(ran, compareRuns)
	if n := len(ran); n > 0 {
		recent := ran[max(n-trigger.PaceLooks, 0):]
		var sum int64
		for _, r := range recent {
			sum += r.CostUSDMicro
		}
		item.PerRunUSDMicro = new(sum / int64(len(recent)))
	}
	// A one-shot past its time that is still stored waits: for its
	// allowance, tried at the next pass of the minute loop, or for a limit
	// that refused it to reset, tried then, its next fire time (spec 087).
	if spec.At != nil && !spec.Suspend && !spec.At.After(now) {
		next := now
		if t.NextFireAt != nil && t.NextFireAt.After(now) {
			next = t.NextFireAt.UTC()
		}
		item.Fires = append(item.Fires, next)
		return item, nil
	}
	fires, more, err := triggers.FireTimes(spec, now, until, trigger.MaxRunFires)
	if err != nil {
		return triggerRuns{}, err
	}
	item.Fires, item.MoreFires = append(item.Fires, fires...), more
	return item, nil
}

// compareRuns orders runs by time, those of one time by firing id.
func compareRuns(a, b store.Run) int {
	return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.FiringID, b.FiringID))
}
