// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package triggers fires triggers (spec 022): one pipeline, match, claim,
// skip, render, act by the session policy and record, serves a trigger's
// schedule, the events its fire route delivers, a manual firing, and the
// held firings the minute loop sends once their session stops waiting
// for a person. A trigger's firings act one at a time across replicas,
// under the trigger's lease in the store, so two firings of one key
// never start two sessions and maxActive holds.
//
// The engine starts and continues sessions through an Actor, which the
// server implements with the code of its create and send routes, so a
// firing's session is created and asked of the authorizer as any other.
package triggers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	// The zones a schedule is read in come with the build, so a server
	// on an image without zone data reads them.
	_ "time/tzdata"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// LeaseTTL is how long a trigger's lease holds without a renew; the
// holder renews it every quarter of it while a firing acts.
const LeaseTTL = 30 * time.Second

// The reasons the engine itself records on a refused or failed firing;
// every other reason is the Actor's.
const (
	ReasonMessageTooLarge = "message_too_large"
	ReasonInternal        = "internal"
	// ReasonRunsInvalid refuses a firing whose answer of trigger.runs it
	// cannot act on (spec 095).
	ReasonRunsInvalid = "runs_invalid"
)

// ErrEnded is an Actor's Send to a session that has ended, which the
// engine answers by starting the key's next session.
var ErrEnded = errors.New("triggers: the session has ended")

// ErrSuspended answers a fire of a suspended trigger, which fires
// nothing and records nothing.
var ErrSuspended = errors.New("triggers: the trigger is suspended")

// ErrHeld answers a fire of a trigger whose owner holds every trigger
// they own (spec 086), which fires nothing and records nothing.
var ErrHeld = errors.New("triggers: the trigger's owner holds every trigger")

// ErrRunsInvalid is an answer of an Actor's Runs that names no runs the
// engine can start (spec 095): limits that do not read, more than
// trigger.MaxRunsPerFiring runs, a scope outside its pattern or one named
// twice. Its firing is refused as ReasonRunsInvalid.
var ErrRunsInvalid = errors.New("triggers: the answer of trigger.runs is invalid")

// Start is a session a firing starts: the trigger, its spec, the firing,
// the rendered message, title and repositories, and the key that fired
// it.
type Start struct {
	Trigger   store.Trigger
	Spec      v1.TriggerSpec
	FiringID  string
	Message   string
	Title     string
	Resources []session.Resource
	// FiredBy is the firing's FiredBy decoded: what the token of the fire
	// that recorded the firing says of the key it was minted from, which
	// the session's create carries to the authorizer (spec 078); nil for
	// a firing no key's token fired. The engine reads nothing in it.
	FiredBy map[string]any
	// Spend is the week of the trigger the firing counts toward, which its
	// message carries (spec 087); nil for a firing that counts toward none.
	Spend *session.MessageSpend
	// Reach is the trigger's reach as the session's create carries it
	// (spec 095): its kind, and for a run of one scope of a trigger whose
	// reach splits by scope, the scope and the time since which it reads;
	// nil for a trigger with no reach.
	Reach *StartReach
}

// StartReach is the reach a session a firing starts carries.
type StartReach struct {
	Kind  string
	Scope string
	Since *time.Time
}

// RunsAsk is what a firing of a trigger whose reach splits by scope asks
// before it starts anything (spec 095): the trigger, its spec, the
// firing, and the time of each scope's latest run of the trigger that
// started a session.
type RunsAsk struct {
	Trigger store.Trigger
	Spec    v1.TriggerSpec
	Firing  store.Firing
	Last    map[string]time.Time
}

// DueRun is one scope an answer to a RunsAsk names due, in the order the
// firing starts them: the scope, which keys its session, and the time
// since which its run reads, nil for none.
type DueRun struct {
	Scope string
	Since *time.Time
}

// Send is a message a firing sends to the open session its key or its
// session.target names, with the trigger's spec and the firing's time,
// the time a one-shot was due.
type Send struct {
	Trigger   store.Trigger
	SessionID string
	FiringID  string
	Message   string
	Spec      v1.TriggerSpec
	Due       time.Time
	// Spend is the week of the trigger the firing counts toward, which its
	// message carries (spec 087); nil for a firing that counts toward none.
	Spend *session.MessageSpend
}

// Actor starts and continues a trigger's sessions as the server's create
// and send routes do, asked of the authorizer as the trigger's owner.
type Actor interface {
	Start(ctx context.Context, s Start) (session.Session, error)
	// Send appends the message, and answers ErrEnded when the session
	// has ended.
	Send(ctx context.Context, s Send) error
	// Runs asks which scopes are due at a firing of a trigger whose reach
	// splits by scope (spec 095). An answer it cannot read wraps
	// ErrRunsInvalid; a refusal is an error Refusal names.
	Runs(ctx context.Context, a RunsAsk) ([]DueRun, error)
	// Refusal names what an error of Start or Send was: the reason its
	// firing records, when the limit that refused it resets for a deny
	// that names it (nil otherwise), and whether it was transient, a
	// failed firing that a redelivery runs again, or a refusal.
	Refusal(err error) (reason string, until *time.Time, transient bool)
	// Spent is told of the firing that spent the one-shot trigger t, which
	// the store deleted with it (spec 086), so the server records in the
	// session a wake continued that the wake sent nothing.
	Spent(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing) error
}

// Options configure an Engine.
type Options struct {
	Store    store.Triggers
	Sessions session.Store
	Actor    Actor
	// Now is the clock firings are timed and judged late by; time.Now
	// when nil.
	Now func() time.Time
	// Log receives what a pass could not do; none when nil.
	Log *slog.Logger
	// Replica names this engine as a lease holder; a random name when
	// empty.
	Replica string
}

// Engine fires triggers.
type Engine struct {
	o Options
	// local holds one mutex per trigger, so one goroutine of this
	// replica at a time waits on the store's lease.
	mu    sync.Mutex
	local map[string]*sync.Mutex
}

// New returns an engine over its stores and actor.
func New(o Options) (*Engine, error) {
	if o.Store == nil || o.Sessions == nil || o.Actor == nil {
		return nil, errors.New("triggers: a store or the actor is missing")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Replica == "" {
		o.Replica = "replica_" + rand.Text()
	}
	return &Engine{o: o, local: map[string]*sync.Mutex{}}, nil
}

// Spec reads a stored trigger's spec.
func Spec(t store.Trigger) (v1.TriggerSpec, error) {
	doc, err := store.DecodeTrigger(t.Doc)
	if err != nil {
		return v1.TriggerSpec{}, err
	}
	return doc.Spec, nil
}

// EventDedupe is the dedupe string of a delivered event: its product
// and its id, length-prefixed so no two pairs share one.
func EventDedupe(e trigger.Envelope) string {
	return "event:" + strconv.Itoa(len(e.Product)) + ":" + e.Product + ":" + e.ID
}

// ScheduleDedupe is the dedupe string of a scheduled firing: its
// scheduled instant.
func ScheduleDedupe(at time.Time) string { return "schedule:" + at.UTC().Format(time.RFC3339) }

// Fire runs one firing of t in the request: the envelope e of an event
// trigger, or, with e nil, a schedule trigger fired now. A filtered
// event answers a firing of outcome filtered and no id, stored nowhere;
// every other firing answers as recorded, a redelivery's as first
// recorded.
//
// by is what the request's own token says of the key it was minted
// from, nil for a token no key minted. It is kept with the firing as its
// FiredBy (spec 078), so the session the firing starts names that key
// whenever it starts: now, or later for a firing held while the session
// it continues waits for a person. A failed firing fired again keeps the
// key it was first recorded with, and takes by only when it has none.
func (e *Engine) Fire(ctx context.Context, t store.Trigger, env *trigger.Envelope, by map[string]any) (store.Firing, error) {
	// A caller that hangs up does not stop a firing midway.
	ctx = context.WithoutCancel(ctx)
	spec, err := Spec(t)
	if err != nil {
		return store.Firing{}, err
	}
	if spec.Suspend {
		return store.Firing{}, ErrSuspended
	}
	held, err := e.o.Store.Hold(ctx, t.Owner)
	if err != nil {
		return store.Firing{}, err
	}
	if held != nil {
		return store.Firing{}, ErrHeld
	}
	now := e.o.Now().UTC()
	f := store.Firing{ID: session.NewID(session.PrefixFiring), TriggerID: t.ID, Replica: e.o.Replica, Time: now, ReceivedAt: now}
	if by != nil {
		raw, err := json.Marshal(by)
		if err != nil {
			return store.Firing{}, fmt.Errorf("triggers: the key that fired %s: %w", t.ID, err)
		}
		f.FiredBy = raw
	}
	switch {
	case env == nil:
		f.Origin, f.Dedupe = store.OriginManual, "manual:"+f.ID
	case spec.On != nil:
		raw, err := session.Marshal(*env)
		if err != nil {
			return store.Firing{}, err
		}
		ok, err := trigger.Matches(*spec.On, *env)
		if err != nil {
			return store.Firing{}, err
		}
		if !ok {
			if err := e.o.Store.CountFiltered(ctx, t.ID); err != nil {
				return store.Firing{}, err
			}
			return store.Firing{TriggerID: t.ID, Origin: store.OriginEvent, Envelope: raw, Outcome: store.OutcomeFiltered, ReceivedAt: now}, nil
		}
		f.Origin, f.Dedupe, f.Envelope = store.OriginEvent, EventDedupe(*env), raw
	default:
		return store.Firing{}, errors.New("triggers: a schedule trigger takes no event")
	}
	var out store.Firing
	err = e.locked(ctx, t.ID, func() error {
		fresh, err := e.o.Store.Trigger(ctx, t.ID)
		if err != nil {
			return err
		}
		out, err = e.run(ctx, fresh, spec, f, env)
		return err
	})
	return out, err
}

// Tick is one pass of the minute loop: every due schedule fires each
// scheduled time up to now, in its trigger's zone, and every held
// firing whose session no longer waits for a person is sent. A trigger
// whose owner holds every trigger they own fires nothing and records
// nothing, its next fire time kept for its release (spec 086). It tries
// every trigger and returns what it could not do.
func (e *Engine) Tick(ctx context.Context) error {
	now := e.o.Now().UTC()
	var errs []error
	owners, err := e.o.Store.HeldOwners(ctx)
	if err != nil {
		return err
	}
	due, err := e.o.Store.DueTriggers(ctx, now)
	if err != nil {
		errs = append(errs, err)
	}
	for _, t := range due {
		if slices.Contains(owners, t.Owner) {
			continue
		}
		// A trigger deleted since the list, a one-shot another replica
		// spent among them, has nothing left to fire.
		if err := e.locked(ctx, t.ID, func() error { return e.schedule(ctx, t.ID, now) }); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, fmt.Errorf("trigger %s: %w", t.ID, err))
		}
	}
	held, err := e.o.Store.HeldFirings(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	var ids []string
	for _, f := range held {
		if !slices.Contains(ids, f.TriggerID) {
			ids = append(ids, f.TriggerID)
		}
	}
	// A held owner's held firings wait for the release as their triggers
	// do.
	ids = slices.DeleteFunc(ids, func(id string) bool {
		t, err := e.o.Store.Trigger(ctx, id)
		return err == nil && slices.Contains(owners, t.Owner)
	})
	for _, id := range ids {
		if err := e.locked(ctx, id, func() error { return e.sweep(ctx, id) }); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, fmt.Errorf("trigger %s: %w", id, err))
		}
	}
	// The runs older than the record keeps are deleted (spec 088).
	if err := e.o.Store.PruneRuns(ctx, now.Add(-trigger.RunsKept)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// NextFire is when a schedule trigger fires next after at, nil for one
// that never does or fires on events; a one-shot fires at its time when
// that is after at, and never again, and a trigger of listed times at the
// soonest of them after at (spec 090). A zone the build does not know is
// an error.
func NextFire(spec v1.TriggerSpec, at time.Time) (*time.Time, error) {
	if spec.At != nil {
		if spec.At.After(at) {
			return new(spec.At.UTC()), nil
		}
		return nil, nil
	}
	if spec.Times != nil {
		return trigger.NextTime(spec, at), nil
	}
	if spec.On != nil || spec.Schedule == "" {
		return nil, nil
	}
	sc, err := trigger.ParseSchedule(spec.Schedule)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(spec.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("the time zone %q: %w", spec.TimeZone, err)
	}
	next := sc.Next(at, loc)
	if next.IsZero() {
		return nil, nil
	}
	u := next.UTC()
	return &u, nil
}

// schedule fires every scheduled time of the trigger up to now, each
// once, and moves its next fire time past now. A scheduled time more
// than maxAge before now is recorded skipped_late.
func (e *Engine) schedule(ctx context.Context, id string, now time.Time) error {
	t, err := e.o.Store.Trigger(ctx, id)
	if err != nil {
		return err
	}
	if t.Suspended || t.NextFireAt == nil || t.NextFireAt.After(now) {
		return nil
	}
	spec, err := Spec(t)
	if err != nil {
		return err
	}
	at := t.NextFireAt
	for at != nil && !at.After(now) {
		// A one-shot's firing is its time, whenever it is tried, so a
		// firing that waited is judged late by its time and takes its first
		// firing again; a refused one it asks for again, since a one-shot
		// still stored after a refusal is one that waits (spec 087). A
		// listed time is fired as a one-shot is, the listed time at or
		// before the next fire time being the one tried (spec 090).
		due := *at
		switch {
		case spec.At != nil:
			due = *spec.At
		case spec.Times != nil:
			due = trigger.DueTime(spec, *at)
		}
		once := spec.At != nil || spec.Times != nil
		f := store.Firing{ID: session.NewID(session.PrefixFiring), TriggerID: t.ID, Origin: store.OriginSchedule, Dedupe: ScheduleDedupe(due),
			Replica: e.o.Replica, Time: due, ReceivedAt: now, RetakeRefused: once}
		out, err := e.run(ctx, t, spec, f, nil)
		if err != nil {
			return err
		}
		if once {
			// A spent one-shot is gone with its firing, and so is a trigger
			// of listed times its latest time spent; a failed one, and one
			// skipped at its allowance, run again at the next pass, at its
			// time; one refused for a limit that resets runs again when it
			// does, its next fire time set by the record; a held one waits
			// for its message to be sent. An earlier listed time that is
			// spent, or held, gives way to the next.
			switch {
			case spends(out) && (spec.At != nil || trigger.LastTime(spec, due)):
				return nil
			case out.Outcome == store.OutcomeFailed, out.Outcome == store.OutcomeSkippedBudget, out.Waits():
				return nil
			}
			at = &due
		}
		if at, err = NextFire(spec, *at); err != nil {
			return err
		}
	}
	return e.o.Store.SetNextFire(ctx, t.ID, at)
}

// spends reports whether firing f spends a one-shot trigger (spec 086):
// every firing but a held one, which keeps it until its message is sent,
// a failed one, which runs again, one skipped_budget, which runs once the
// spend allows it, one refused for a limit that resets, which runs once
// it has (spec 087), and a filtered one, which no one-shot records.
func spends(f store.Firing) bool {
	switch f.Outcome {
	case store.OutcomeHeld, store.OutcomeFailed, store.OutcomeFiltered, store.OutcomeSkippedBudget, "":
		return false
	}
	return !f.Waits()
}

// record records a decided firing whose stored outcome is from, and its
// runs when it is one (spec 088): the runs of its scopes when it has them
// (spec 095), and otherwise the firing's own. A one-shot's that spends it
// deletes the trigger with it, and tells the actor.
func (e *Engine) record(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing, from string, scoped []store.Run) error {
	if err := e.recordFiring(ctx, t, spec, f, from); err != nil {
		return err
	}
	e.keepRun(ctx, t, spec, f, scoped)
	return nil
}

// keepRun stores the runs of a recorded firing of t, one that started or
// continued a turn or that an allowance or the authorizer stopped (spec
// 088): the runs of its scopes when it has them, and otherwise its own. A
// run whose write fails is logged and the firing keeps its answer: the
// read of an owner's runs records a firing with no run again from the
// firing, and only a wake's run, whose firing went with its trigger, is
// lost.
func (e *Engine) keepRun(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing, scoped []store.Run) {
	if !store.IsRun(f.Outcome) {
		return
	}
	for _, r := range scoped {
		if err := e.o.Store.PutRun(ctx, r); err != nil {
			e.o.Log.ErrorContext(ctx, "keep a scope's run", "trigger", t.ID, "firing", f.ID, "scope", r.Scope, "err", err)
		}
	}
	if len(scoped) > 0 {
		return
	}
	r, err := e.runOf(ctx, t, spec, f)
	if err == nil {
		err = e.o.Store.PutRun(ctx, r)
	}
	if err != nil {
		e.o.Log.ErrorContext(ctx, "keep a firing's run", "trigger", t.ID, "firing", f.ID, "err", err)
	}
}

// RunOf is the run of a recorded firing f of t of spec (spec 088), as far
// as the firing and the trigger tell it: with no cost yet, ended when it
// is skipped, and for a skipped run of a trigger that continues one
// session, naming that session as a run that went ahead would have. A
// wake's run also names the trigger that started its session, which
// only the session tells.
func RunOf(t store.Trigger, spec v1.TriggerSpec, f store.Firing) store.Run {
	r := store.Run{FiringID: f.ID, Owner: t.Owner, TriggerID: t.ID, SessionID: f.SessionID, At: f.Time, Outcome: f.Outcome, Reason: f.Reason}
	r.Ended = !r.Ran()
	if r.SessionID == "" {
		r.SessionID = spec.Session.Target
	}
	return r
}

// runOf is RunOf with a wake's part: the trigger that started its
// session, which its runs count toward (spec 087).
func (e *Engine) runOf(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing) (store.Run, error) {
	r := RunOf(t, spec, f)
	doc, err := store.DecodeTrigger(t.Doc)
	if err != nil {
		return store.Run{}, err
	}
	if o := doc.Status.Origin; o == nil || o.Kind != v1.OriginWake {
		return r, nil
	}
	r.Wake = true
	s, err := e.o.Sessions.Get(ctx, spec.Session.Target)
	switch {
	case errors.Is(err, session.ErrNotFound):
	case err != nil:
		return store.Run{}, err
	default:
		r.SessionTriggerID = s.TriggerID
	}
	return r, nil
}

// recordFiring is record without the run.
func (e *Engine) recordFiring(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing, from string) error {
	// A listed time fires once, as a one-shot does (spec 090); a manual
	// firing of a trigger of listed times is none of them.
	once := spec.At != nil || spec.Times != nil && f.Origin == store.OriginSchedule
	if once && (f.Outcome == store.OutcomeSkippedBudget || f.Waits()) {
		// A one-shot skipped at its allowance keeps its time, so the minute
		// loop takes its firing again at each pass, a held one's too, until
		// it fires or maxAge passes (spec 087). One a deny refused for a
		// limit that resets is tried again once, when it resets, so the
		// authorizer is not asked at every pass; its firing keeps the
		// one-shot's time, and maxAge still bounds it. A listed time waits
		// no later than the next listed time, which then fires (spec 090).
		if err := e.o.Store.RecordFiring(ctx, f, from); err != nil {
			return err
		}
		next := spec.At
		if spec.Times != nil {
			next = new(f.Time.UTC())
		}
		if f.Waits() {
			next = new(f.Until.UTC())
			if now := e.o.Now().UTC(); next.Before(now) {
				next = &now
			}
		}
		if after := trigger.NextTime(spec, f.Time); spec.Times != nil && after != nil && after.Before(*next) {
			next = after
		}
		return e.o.Store.SetNextFire(ctx, t.ID, next)
	}
	// A trigger of listed times is spent by its latest time's firing alone.
	if !once || !spends(f) || spec.Times != nil && !trigger.LastTime(spec, f.Time) {
		return e.o.Store.RecordFiring(ctx, f, from)
	}
	if err := e.o.Store.SpendFiring(ctx, f, from); err != nil {
		return err
	}
	return e.o.Actor.Spent(ctx, t, spec, f)
}

// locked runs fn holding the trigger's lease: this replica's mutex of
// the trigger first, then the store's lease, renewed while fn runs.
func (e *Engine) locked(ctx context.Context, id string, fn func() error) error {
	e.mu.Lock()
	m := e.local[id]
	if m == nil {
		m = &sync.Mutex{}
		e.local[id] = m
	}
	e.mu.Unlock()
	m.Lock()
	defer m.Unlock()
	wait := 10 * time.Millisecond
	for {
		ok, err := e.o.Store.LeaseTrigger(ctx, id, e.o.Replica, e.o.Now().Add(LeaseTTL))
		if err != nil {
			return err
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, 250*time.Millisecond)
	}
	stop := make(chan struct{})
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		tick := time.NewTicker(LeaseTTL / 4)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if _, err := e.o.Store.LeaseTrigger(ctx, id, e.o.Replica, e.o.Now().Add(LeaseTTL)); err != nil {
					e.o.Log.ErrorContext(ctx, "renew a trigger's lease", "trigger", id, "err", err)
				}
			}
		}
	}()
	err := fn()
	close(stop)
	<-renewed
	if rerr := e.o.Store.ReleaseTrigger(context.WithoutCancel(ctx), id, e.o.Replica); rerr != nil {
		e.o.Log.ErrorContext(ctx, "release a trigger's lease", "trigger", id, "err", rerr)
	}
	return err
}

// run claims a firing and, when it is this one's to act on, decides and
// records it. A firing claimed before answers as recorded.
func (e *Engine) run(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing, env *trigger.Envelope) (store.Firing, error) {
	claimed, fresh, err := e.o.Store.ClaimFiring(ctx, f)
	if err != nil || !fresh {
		return claimed, err
	}
	out, scoped, err := e.decide(ctx, t, spec, claimed, env)
	if err != nil {
		return store.Firing{}, err
	}
	if err := e.record(ctx, t, spec, out, "", scoped); err != nil {
		return store.Firing{}, err
	}
	return out, nil
}

// decide is a claimed firing's outcome: skipped when late, refused when
// its message is past the cap, and otherwise what the session policy
// does with it, or, for a trigger whose reach splits by scope, what its
// due scopes do, with the run of each (spec 095).
func (e *Engine) decide(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing, env *trigger.Envelope) (store.Firing, []store.Run, error) {
	late, err := e.late(spec, f, env)
	if err != nil {
		return store.Firing{}, nil, err
	}
	if late {
		f.Outcome = store.OutcomeSkippedLate
		return f, nil, nil
	}
	r, err := render(t, spec, f, env)
	if err != nil {
		return store.Firing{}, nil, err
	}
	f.Key = r.key
	if len(r.message) > trigger.MaxMessageBytes {
		f.Outcome, f.Reason = store.OutcomeRefused, ReasonMessageTooLarge
		return f, nil, nil
	}
	// A firing while the trigger it counts toward is at its allowance
	// starts nothing (spec 087).
	spend, over, err := e.allowance(ctx, t, spec)
	if err != nil {
		return store.Firing{}, nil, err
	}
	if over {
		f.Outcome = store.OutcomeSkippedBudget
		return f, nil, nil
	}
	r.spend = spend
	var out store.Firing
	switch {
	case trigger.Split(spec):
		return e.split(ctx, t, spec, f, r)
	case trigger.Policy(spec) == v1.PolicyContinue:
		out, err = e.continueKey(ctx, t, spec, f, r, false)
	default:
		out, err = e.startNew(ctx, t, spec, f, r)
	}
	return out, nil, err
}

// split acts on a firing of a trigger whose reach splits by scope (spec
// 095): it asks the actor which scopes are due, and records
// skipped_nothing_new when none is, runs_invalid for an answer it cannot
// act on, and a refusal's reason. Each due scope whose session is still
// active is passed over when skipIfActive holds; each other starts a
// session keyed by its scope, a run of its own. maxActive bounds no
// scope: the answer, at most trigger.MaxRunsPerFiring, bounds the firing.
// The firing is started with its first session; refused, with the first
// refusal's reason, when no scope started; failed when every start that
// was tried failed; and skipped_active when every scope was passed over.
func (e *Engine) split(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing, r rendered) (store.Firing, []store.Run, error) {
	last, err := e.lastRuns(ctx, t)
	if err != nil {
		return store.Firing{}, nil, err
	}
	due, err := e.o.Actor.Runs(ctx, RunsAsk{Trigger: t, Spec: spec, Firing: f, Last: last})
	if err == nil {
		err = checkDue(due)
	}
	switch {
	case errors.Is(err, ErrRunsInvalid):
		e.o.Log.WarnContext(ctx, "a trigger's firing refused: the answer of trigger.runs is invalid", "trigger", t.ID, "firing", f.ID, "err", err)
		f.Outcome, f.Reason = store.OutcomeRefused, ReasonRunsInvalid
		return f, nil, nil
	case err != nil:
		return e.refused(f, err), nil, nil
	case len(due) == 0:
		f.Outcome = store.OutcomeSkippedNothingNew
		return f, nil, nil
	}
	by, err := firedBy(f)
	if err != nil {
		return store.Firing{}, nil, err
	}
	// Each scope's run starts from the firing's before any session does.
	base := RunOf(t, spec, f)
	var runs []store.Run
	var refusal store.Firing
	failed := false
	for _, d := range due {
		if spec.SkipIfActive == nil || *spec.SkipIfActive {
			s, open, err := e.keySession(ctx, t.ID, d.Scope)
			if err != nil {
				return store.Firing{}, nil, err
			}
			if open {
				busy, err := active(ctx, e.o.Sessions, s)
				if err != nil {
					return store.Firing{}, nil, err
				}
				if busy {
					continue
				}
			}
		}
		run := base
		run.Scope = d.Scope
		s, err := e.o.Actor.Start(ctx, Start{Trigger: t, Spec: spec, FiringID: f.ID, Message: r.message, Title: r.title, Resources: r.resources, FiredBy: by,
			Spend: r.spend, Reach: &StartReach{Kind: spec.Reach.Kind, Scope: d.Scope, Since: d.Since}})
		if err != nil {
			reason, until, transient := e.o.Actor.Refusal(err)
			if transient {
				// The scope's last run does not move, so the next firing asks
				// for it again.
				failed = true
				e.o.Log.WarnContext(ctx, "a scope's session did not start; the next firing asks again", "trigger", t.ID, "firing", f.ID, "scope", d.Scope, "err", err)
				continue
			}
			if refusal.Outcome == "" {
				refusal.Outcome, refusal.Reason, refusal.Until = store.OutcomeRefused, reason, until
			}
			run.Outcome, run.Reason, run.Ended = store.OutcomeRefused, reason, true
			runs = append(runs, run)
			continue
		}
		if err := e.o.Store.SetTriggerSession(ctx, t.ID, d.Scope, s.ID); err != nil {
			return store.Firing{}, nil, err
		}
		run.Outcome, run.SessionID, run.Ended = store.OutcomeStarted, s.ID, false
		runs = append(runs, run)
		if f.SessionID == "" {
			f.Outcome, f.SessionID, f.Open = store.OutcomeStarted, s.ID, true
		}
	}
	switch {
	case f.Outcome == store.OutcomeStarted:
	case refusal.Outcome != "":
		f.Outcome, f.Reason, f.Until = refusal.Outcome, refusal.Reason, refusal.Until
	case failed:
		f.Outcome = store.OutcomeFailed
	default:
		f.Outcome = store.OutcomeSkippedActive
	}
	return f, runs, nil
}

// checkDue holds an answer of trigger.runs to the shape a firing acts on,
// wrapping ErrRunsInvalid when it is not.
func checkDue(due []DueRun) error {
	scopes := make([]string, len(due))
	for i, d := range due {
		scopes[i] = d.Scope
	}
	if err := trigger.CheckRuns(scopes); err != nil {
		return fmt.Errorf("%w: %w", ErrRunsInvalid, err)
	}
	return nil
}

// lastRuns is the time of each scope's latest run of t that started a
// session, among the runs the store keeps (spec 095).
func (e *Engine) lastRuns(ctx context.Context, t store.Trigger) (map[string]time.Time, error) {
	rs, err := e.o.Store.Runs(ctx, t.Owner, e.o.Now().UTC().Add(-trigger.RunsKept))
	if err != nil {
		return nil, err
	}
	last := map[string]time.Time{}
	for _, r := range rs {
		if r.TriggerID == t.ID && r.Scope != "" && r.Outcome == store.OutcomeStarted && r.At.After(last[r.Scope]) {
			last[r.Scope] = r.At
		}
	}
	return last, nil
}

// firedBy is a firing's FiredBy decoded, nil for none.
func firedBy(f store.Firing) (map[string]any, error) {
	if len(f.FiredBy) == 0 {
		return nil, nil
	}
	var by map[string]any
	if err := json.Unmarshal(f.FiredBy, &by); err != nil {
		return nil, fmt.Errorf("triggers: firing %s's key: %w", f.ID, err)
	}
	return by, nil
}

// late reports whether the firing is more than maxAge after its event's
// time or its scheduled time, or, for a listed time, whether the next
// listed time has come (spec 090). A manual firing is never late.
func (e *Engine) late(spec v1.TriggerSpec, f store.Firing, env *trigger.Envelope) (bool, error) {
	if f.Origin == store.OriginManual {
		return false, nil
	}
	maxAge, err := time.ParseDuration(spec.MaxAge)
	if err != nil {
		return false, fmt.Errorf("triggers: trigger %s's maxAge: %w", f.TriggerID, err)
	}
	at := f.Time
	if env != nil {
		at = env.Time
	}
	now := e.o.Now()
	if next := trigger.NextTime(spec, f.Time); spec.Times != nil && next != nil && !next.After(now) {
		return true, nil
	}
	return now.Sub(at) > maxAge, nil
}

// startNew acts under the new policy: a session of the key that is
// still active skips the firing when skipIfActive holds, and otherwise
// the firing starts a session.
func (e *Engine) startNew(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing, r rendered) (store.Firing, error) {
	if spec.SkipIfActive == nil || *spec.SkipIfActive {
		s, open, err := e.keySession(ctx, t.ID, f.Key)
		if err != nil {
			return store.Firing{}, err
		}
		if open {
			busy, err := active(ctx, e.o.Sessions, s)
			if err != nil {
				return store.Firing{}, err
			}
			if busy {
				f.Outcome = store.OutcomeSkippedActive
				return f, nil
			}
		}
	}
	return e.start(ctx, t, spec, f, r)
}

// continueKey acts under the continue policy: the key's open session
// takes the message, or holds it while it waits for a person, and a key
// with no open session starts one. A firing of a key with held firings
// waits behind them; resend is set when the firing is a held one the
// sweep sends.
func (e *Engine) continueKey(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing, r rendered, resend bool) (store.Firing, error) {
	if !resend {
		waiting, err := e.flush(ctx, t, spec, f.Key)
		if err != nil {
			return store.Firing{}, err
		}
		if waiting {
			f.Outcome = store.OutcomeHeld
			return f, nil
		}
	}
	if spec.Session.Target != "" {
		return e.continueTarget(ctx, t, spec, f, r)
	}
	s, open, err := e.keySession(ctx, t.ID, f.Key)
	if err != nil {
		return store.Firing{}, err
	}
	if !open {
		return e.start(ctx, t, spec, f, r)
	}
	if waitsForPerson(s) {
		f.Outcome = store.OutcomeHeld
		return f, nil
	}
	err = e.o.Actor.Send(ctx, Send{Trigger: t, SessionID: s.ID, FiringID: f.ID, Message: r.message, Spec: spec, Due: f.Time, Spend: r.spend})
	switch {
	case errors.Is(err, ErrEnded):
		if err := e.o.Store.CloseSession(ctx, t.ID, s.ID); err != nil {
			return store.Firing{}, err
		}
		return e.start(ctx, t, spec, f, r)
	case err != nil:
		return e.refused(f, err), nil
	}
	f.Outcome, f.SessionID = store.OutcomeContinued, s.ID
	return f, nil
}

// continueTarget acts on a firing of a trigger whose session.target names
// the session every firing continues (spec 086): it takes the message,
// or holds it while it waits for a person, and a target that has ended or
// is gone is skipped_ended, since a new session would not hold the
// conversation the message was written for.
func (e *Engine) continueTarget(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing, r rendered) (store.Firing, error) {
	s, err := e.o.Sessions.Get(ctx, spec.Session.Target)
	switch {
	case errors.Is(err, session.ErrNotFound) || err == nil && s.Status == session.StatusEnded:
		f.Outcome = store.OutcomeSkippedEnded
		return f, nil
	case err != nil:
		return store.Firing{}, err
	case waitsForPerson(s):
		f.Outcome = store.OutcomeHeld
		return f, nil
	}
	err = e.o.Actor.Send(ctx, Send{Trigger: t, SessionID: s.ID, FiringID: f.ID, Message: r.message, Spec: spec, Due: f.Time, Spend: r.spend})
	switch {
	case errors.Is(err, ErrEnded):
		f.Outcome = store.OutcomeSkippedEnded
		return f, nil
	case err != nil:
		return e.refused(f, err), nil
	}
	f.Outcome, f.SessionID = store.OutcomeContinued, s.ID
	return f, nil
}

// start starts the firing's session unless maxActive of the trigger's
// sessions are active, and maps the key to it. The session's create
// names the firing's own FiredBy, never that of the fire that happens to
// start it, as one does for a held firing it flushes.
func (e *Engine) start(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing, r rendered) (store.Firing, error) {
	n, err := e.activeSessions(ctx, t.ID)
	if err != nil {
		return store.Firing{}, err
	}
	if n >= trigger.MaxActive(spec) {
		f.Outcome = store.OutcomeSkippedBusy
		return f, nil
	}
	by, err := firedBy(f)
	if err != nil {
		return store.Firing{}, err
	}
	var reach *StartReach
	if spec.Reach != nil {
		reach = &StartReach{Kind: spec.Reach.Kind}
	}
	s, err := e.o.Actor.Start(ctx, Start{Trigger: t, Spec: spec, FiringID: f.ID, Message: r.message, Title: r.title, Resources: r.resources, FiredBy: by,
		Spend: r.spend, Reach: reach})
	if err != nil {
		return e.refused(f, err), nil
	}
	if err := e.o.Store.SetTriggerSession(ctx, t.ID, f.Key, s.ID); err != nil {
		return store.Firing{}, err
	}
	f.Outcome, f.SessionID, f.Open = store.OutcomeStarted, s.ID, true
	return f, nil
}

// refused is the firing an Actor's error ends: failed when it was
// transient, refused with its reason otherwise, and with when the limit
// that refused it resets, for a deny that says.
func (e *Engine) refused(f store.Firing, err error) store.Firing {
	reason, until, transient := e.o.Actor.Refusal(err)
	f.Outcome, f.Reason, f.Until = store.OutcomeRefused, reason, until
	if transient {
		f.Until = nil
		f.Outcome = store.OutcomeFailed
		e.o.Log.Warn("a trigger's firing failed; a redelivery runs it again", "trigger", f.TriggerID, "firing", f.ID, "err", err)
	}
	return f
}

// keySession is the open session key names: false when it names none,
// or one that has ended or is gone, whose mapping it clears.
func (e *Engine) keySession(ctx context.Context, triggerID, key string) (session.Session, bool, error) {
	id, err := e.o.Store.TriggerSession(ctx, triggerID, key)
	if errors.Is(err, store.ErrNotFound) {
		return session.Session{}, false, nil
	}
	if err != nil {
		return session.Session{}, false, err
	}
	s, err := e.o.Sessions.Get(ctx, id)
	switch {
	case errors.Is(err, session.ErrNotFound) || err == nil && s.Status == session.StatusEnded:
		return session.Session{}, false, e.o.Store.CloseSession(ctx, triggerID, id)
	case err != nil:
		return session.Session{}, false, err
	}
	return s, true, nil
}

// activeSessions counts the trigger's active sessions, from its open
// firings; a session seen ended, or gone, is closed.
func (e *Engine) activeSessions(ctx context.Context, triggerID string) (int, error) {
	open, err := e.o.Store.OpenFirings(ctx, triggerID)
	if err != nil {
		return 0, err
	}
	n := 0
	seen := map[string]bool{}
	for _, f := range open {
		if seen[f.SessionID] {
			continue
		}
		seen[f.SessionID] = true
		s, err := e.o.Sessions.Get(ctx, f.SessionID)
		switch {
		case errors.Is(err, session.ErrNotFound) || err == nil && s.Status == session.StatusEnded:
			if err := e.o.Store.CloseSession(ctx, triggerID, f.SessionID); err != nil {
				return 0, err
			}
			continue
		case err != nil:
			return 0, err
		}
		busy, err := active(ctx, e.o.Sessions, s)
		if err != nil {
			return 0, err
		}
		if busy {
			n++
		}
	}
	return n, nil
}

// sweep sends every held firing of the trigger whose session no longer
// waits for a person.
func (e *Engine) sweep(ctx context.Context, id string) error {
	t, err := e.o.Store.Trigger(ctx, id)
	if err != nil {
		return err
	}
	spec, err := Spec(t)
	if err != nil {
		return err
	}
	held, err := e.o.Store.HeldFirings(ctx)
	if err != nil {
		return err
	}
	var keys []string
	for _, f := range held {
		if f.TriggerID == id && !slices.Contains(keys, f.Key) {
			keys = append(keys, f.Key)
		}
	}
	for _, k := range keys {
		if _, err := e.flush(ctx, t, spec, k); err != nil {
			return err
		}
	}
	return nil
}

// flush acts on the key's held firings in the order they arrived, until
// one is held again because the key's session still waits for a person,
// and reports whether any is left held. A held firing whose session has
// ended goes through the policy as a firing of a key with no open
// session: the first starts the key's next session and the others
// continue it.
func (e *Engine) flush(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, key string) (bool, error) {
	held, err := e.o.Store.HeldFirings(ctx)
	if err != nil {
		return false, err
	}
	for _, f := range held {
		if f.TriggerID != t.ID || f.Key != key {
			continue
		}
		var env *trigger.Envelope
		if len(f.Envelope) > 0 {
			env = &trigger.Envelope{}
			if err := json.Unmarshal(f.Envelope, env); err != nil {
				return false, fmt.Errorf("triggers: firing %s's envelope: %w", f.ID, err)
			}
		}
		r, err := render(t, spec, f, env)
		if err != nil {
			return false, err
		}
		// The key the firing was held under is its key, whatever the
		// template renders now.
		r.key = key
		// A held firing is sent only while the trigger it counts toward is
		// under its allowance; one past it is skipped, and a held one-shot
		// goes back to its time (spec 087).
		spend, over, err := e.allowance(ctx, t, spec)
		if err != nil {
			return false, err
		}
		if over {
			f.Outcome = store.OutcomeSkippedBudget
			if err := e.record(ctx, t, spec, f, store.OutcomeHeld, nil); err != nil {
				return false, err
			}
			continue
		}
		r.spend = spend
		out, err := e.continueKey(ctx, t, spec, f, r, true)
		if err != nil {
			return false, err
		}
		if out.Outcome == store.OutcomeHeld {
			return true, nil
		}
		if err := e.record(ctx, t, spec, out, store.OutcomeHeld, nil); err != nil {
			return false, err
		}
	}
	return false, nil
}
