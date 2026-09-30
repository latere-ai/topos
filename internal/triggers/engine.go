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
)

// ErrEnded is an Actor's Send to a session that has ended, which the
// engine answers by starting the key's next session.
var ErrEnded = errors.New("triggers: the session has ended")

// ErrSuspended answers a fire of a suspended trigger, which fires
// nothing and records nothing.
var ErrSuspended = errors.New("triggers: the trigger is suspended")

// Start is a session a firing starts: the trigger, its spec, the firing,
// and the rendered message, title and repositories.
type Start struct {
	Trigger   store.Trigger
	Spec      v1.TriggerSpec
	FiringID  string
	Message   string
	Title     string
	Resources []session.Resource
}

// Send is a message a firing sends to the open session its key names.
type Send struct {
	Trigger   store.Trigger
	SessionID string
	FiringID  string
	Message   string
}

// Actor starts and continues a trigger's sessions as the server's create
// and send routes do, asked of the authorizer as the trigger's owner.
type Actor interface {
	Start(ctx context.Context, s Start) (session.Session, error)
	// Send appends the message, and answers ErrEnded when the session
	// has ended.
	Send(ctx context.Context, s Send) error
	// Refusal names what an error of Start or Send was: the reason its
	// firing records, and whether it was transient, a failed firing that
	// a redelivery runs again, or a refusal.
	Refusal(err error) (reason string, transient bool)
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
func (e *Engine) Fire(ctx context.Context, t store.Trigger, env *trigger.Envelope) (store.Firing, error) {
	// A caller that hangs up does not stop a firing midway.
	ctx = context.WithoutCancel(ctx)
	spec, err := Spec(t)
	if err != nil {
		return store.Firing{}, err
	}
	if spec.Suspend {
		return store.Firing{}, ErrSuspended
	}
	now := e.o.Now().UTC()
	f := store.Firing{ID: session.NewID(session.PrefixFiring), TriggerID: t.ID, Replica: e.o.Replica, Time: now, ReceivedAt: now}
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
// firing whose session no longer waits for a person is sent. It tries
// every trigger and returns what it could not do.
func (e *Engine) Tick(ctx context.Context) error {
	now := e.o.Now().UTC()
	var errs []error
	due, err := e.o.Store.DueTriggers(ctx, now)
	if err != nil {
		errs = append(errs, err)
	}
	for _, t := range due {
		if err := e.locked(ctx, t.ID, func() error { return e.schedule(ctx, t.ID, now) }); err != nil {
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
	for _, id := range ids {
		if err := e.locked(ctx, id, func() error { return e.sweep(ctx, id) }); err != nil {
			errs = append(errs, fmt.Errorf("trigger %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// NextFire is when a schedule trigger fires next after at, nil for one
// that never does or fires on events. A zone the build does not know is
// an error.
func NextFire(spec v1.TriggerSpec, at time.Time) (*time.Time, error) {
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
		f := store.Firing{ID: session.NewID(session.PrefixFiring), TriggerID: t.ID, Origin: store.OriginSchedule, Dedupe: ScheduleDedupe(*at),
			Replica: e.o.Replica, Time: *at, ReceivedAt: now}
		if _, err := e.run(ctx, t, spec, f, nil); err != nil {
			return err
		}
		if at, err = NextFire(spec, *at); err != nil {
			return err
		}
	}
	return e.o.Store.SetNextFire(ctx, t.ID, at)
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
	out, err := e.decide(ctx, t, spec, claimed, env)
	if err != nil {
		return store.Firing{}, err
	}
	if err := e.o.Store.RecordFiring(ctx, out, ""); err != nil {
		return store.Firing{}, err
	}
	return out, nil
}

// decide is a claimed firing's outcome: skipped when late, refused when
// its message is past the cap, and otherwise what the session policy
// does with it.
func (e *Engine) decide(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing, env *trigger.Envelope) (store.Firing, error) {
	late, err := e.late(spec, f, env)
	if err != nil {
		return store.Firing{}, err
	}
	if late {
		f.Outcome = store.OutcomeSkippedLate
		return f, nil
	}
	r, err := render(t, spec, f, env)
	if err != nil {
		return store.Firing{}, err
	}
	f.Key = r.key
	if len(r.message) > trigger.MaxMessageBytes {
		f.Outcome, f.Reason = store.OutcomeRefused, ReasonMessageTooLarge
		return f, nil
	}
	if trigger.Policy(spec) == v1.PolicyContinue {
		return e.continueKey(ctx, t, spec, f, r, false)
	}
	return e.startNew(ctx, t, spec, f, r)
}

// late reports whether the firing is more than maxAge after its event's
// time or its scheduled time. A manual firing is never late.
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
	return e.o.Now().Sub(at) > maxAge, nil
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
	err = e.o.Actor.Send(ctx, Send{Trigger: t, SessionID: s.ID, FiringID: f.ID, Message: r.message})
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

// start starts the firing's session unless maxActive of the trigger's
// sessions are active, and maps the key to it.
func (e *Engine) start(ctx context.Context, t store.Trigger, spec v1.TriggerSpec, f store.Firing, r rendered) (store.Firing, error) {
	n, err := e.activeSessions(ctx, t.ID)
	if err != nil {
		return store.Firing{}, err
	}
	if n >= trigger.MaxActive(spec) {
		f.Outcome = store.OutcomeSkippedBusy
		return f, nil
	}
	s, err := e.o.Actor.Start(ctx, Start{Trigger: t, Spec: spec, FiringID: f.ID, Message: r.message, Title: r.title, Resources: r.resources})
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
// transient, refused with its reason otherwise.
func (e *Engine) refused(f store.Firing, err error) store.Firing {
	reason, transient := e.o.Actor.Refusal(err)
	f.Outcome, f.Reason = store.OutcomeRefused, reason
	if transient {
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
		out, err := e.continueKey(ctx, t, spec, f, r, true)
		if err != nil {
			return false, err
		}
		if out.Outcome == store.OutcomeHeld {
			return true, nil
		}
		if err := e.o.Store.RecordFiring(ctx, out, store.OutcomeHeld); err != nil {
			return false, err
		}
	}
	return false, nil
}
