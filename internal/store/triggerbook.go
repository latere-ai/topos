// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	v1 "latere.ai/x/topos/manifest/v1"
)

// TriggerSaver writes a TriggerBook's objects where they persist. The
// book calls it before it changes its own state, so a write that fails
// leaves the book as it was. A nil saver keeps the book in the process.
type TriggerSaver interface {
	SaveTrigger(t Trigger) error
	// RemoveTrigger removes the trigger with its firings and keys.
	RemoveTrigger(id string) error
	SaveFiring(f Firing) error
	// SaveKey maps a trigger's key to its session; an empty session
	// removes the mapping.
	SaveKey(triggerID, key, sessionID string) error
}

// KeySession is one stored mapping of a trigger's key to its session.
type KeySession struct {
	TriggerID string
	Key       string
	SessionID string
}

// TriggerBook is Triggers kept in the process: the memory store's, and
// the directory store's index of its files, which it writes through a
// TriggerSaver. Leases live in the process alone, since the book serves
// one process.
type TriggerBook struct {
	mu       sync.Mutex
	now      func() time.Time
	save     TriggerSaver
	triggers map[string]Trigger
	names    map[ownedName]string
	// firings holds each trigger's firings by dedupe string.
	firings map[string]map[string]Firing
	keys    map[string]map[string]string
	leases  map[string]triggerLease
}

type triggerLease struct {
	holder string
	until  time.Time
}

// NewTriggerBook returns an empty book on the clock now, writing through
// save when it is not nil.
func NewTriggerBook(now func() time.Time, save TriggerSaver) *TriggerBook {
	if now == nil {
		now = time.Now
	}
	return &TriggerBook{now: now, save: save, triggers: map[string]Trigger{}, names: map[ownedName]string{},
		firings: map[string]map[string]Firing{}, keys: map[string]map[string]string{}, leases: map[string]triggerLease{}}
}

// Load fills the book with stored objects, without writing them again.
// A second trigger of one owner's name, and a firing or a key of no
// trigger, are refused.
func (b *TriggerBook) Load(ts []Trigger, fs []Firing, ks []KeySession) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range ts {
		key := ownedName{t.Owner, t.Name}
		if other := b.names[key]; other != "" {
			return fmt.Errorf("triggers %s and %s of one owner are both named %q", other, t.ID, t.Name)
		}
		b.triggers[t.ID], b.names[key] = t, t.ID
	}
	for _, f := range fs {
		if _, ok := b.triggers[f.TriggerID]; !ok {
			return fmt.Errorf("firing %s names trigger %s, which is not stored", f.ID, f.TriggerID)
		}
		b.firingsOf(f.TriggerID)[f.Dedupe] = f
	}
	for _, k := range ks {
		if _, ok := b.triggers[k.TriggerID]; !ok {
			return fmt.Errorf("a key names trigger %s, which is not stored", k.TriggerID)
		}
		b.keysOf(k.TriggerID)[k.Key] = k.SessionID
	}
	return nil
}

func (b *TriggerBook) firingsOf(id string) map[string]Firing {
	m := b.firings[id]
	if m == nil {
		m = map[string]Firing{}
		b.firings[id] = m
	}
	return m
}

func (b *TriggerBook) keysOf(id string) map[string]string {
	m := b.keys[id]
	if m == nil {
		m = map[string]string{}
		b.keys[id] = m
	}
	return m
}

func (b *TriggerBook) saveTrigger(t Trigger) error {
	if b.save == nil {
		return nil
	}
	return b.save.SaveTrigger(t)
}

func (b *TriggerBook) saveFiring(f Firing) error {
	if b.save == nil {
		return nil
	}
	return b.save.SaveFiring(f)
}

func (b *TriggerBook) trigger(id string) (Trigger, error) {
	t, ok := b.triggers[id]
	if !ok {
		return Trigger{}, fmt.Errorf("%w: trigger %s", ErrNotFound, id)
	}
	return t, nil
}

func (b *TriggerBook) Trigger(_ context.Context, id string) (Trigger, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.trigger(id)
}

func (b *TriggerBook) TriggerByName(_ context.Context, owner, name string) (Trigger, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.triggers[b.names[ownedName{owner, name}]]
	if !ok {
		return Trigger{}, fmt.Errorf("%w: trigger %s", ErrNotFound, name)
	}
	return t, nil
}

func (b *TriggerBook) ListTriggers(_ context.Context, o TriggerList) ([]Trigger, string, error) {
	after, err := Uncursor(o.Cursor)
	if err != nil {
		return nil, "", err
	}
	b.mu.Lock()
	var all []Trigger
	for _, t := range b.triggers {
		if t.ID > after && (len(o.Owners) == 0 || slices.Contains(o.Owners, t.Owner)) {
			all = append(all, t)
		}
	}
	b.mu.Unlock()
	slices.SortFunc(all, func(a, c Trigger) int { return cmp.Compare(a.ID, c.ID) })
	return page(all, o.Limit, func(t Trigger) string { return t.ID })
}

func (b *TriggerBook) PutTrigger(_ context.Context, t Trigger) error {
	if err := CheckTrigger(t); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	stored, held := b.triggers[t.ID]
	key := ownedName{t.Owner, t.Name}
	switch {
	case t.Version == 1 && !held:
		if b.names[key] != "" {
			return fmt.Errorf("%w: trigger %s exists", ErrConflict, t.Name)
		}
		t.NextFireAt, t.LastFiredAt, t.LastSessionID, t.Counts = utcPtr(t.NextFireAt), nil, "", v1.TriggerCounts{}
		if err := b.saveTrigger(t); err != nil {
			return err
		}
		b.triggers[t.ID], b.names[key] = t, t.ID
		return nil
	case !held || (t.Version != stored.Version && t.Version != stored.Version+1):
		return fmt.Errorf("%w: trigger %s version %d does not follow the stored version", ErrConflict, t.ID, t.Version)
	case b.names[key] != "" && b.names[key] != t.ID:
		return fmt.Errorf("%w: trigger %s exists", ErrConflict, t.Name)
	}
	t.NextFireAt = utcPtr(t.NextFireAt)
	t.CreatedAt, t.LastFiredAt, t.LastSessionID, t.Counts = stored.CreatedAt, stored.LastFiredAt, stored.LastSessionID, stored.Counts
	if err := b.saveTrigger(t); err != nil {
		return err
	}
	delete(b.names, ownedName{stored.Owner, stored.Name})
	b.triggers[t.ID], b.names[key] = t, t.ID
	return nil
}

func (b *TriggerBook) DeleteTrigger(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, err := b.trigger(id)
	if err != nil {
		return err
	}
	if b.save != nil {
		if err := b.save.RemoveTrigger(id); err != nil {
			return err
		}
	}
	delete(b.triggers, id)
	delete(b.names, ownedName{t.Owner, t.Name})
	delete(b.firings, id)
	delete(b.keys, id)
	delete(b.leases, id)
	return nil
}

func (b *TriggerBook) DueTriggers(_ context.Context, at time.Time) ([]Trigger, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Trigger
	for _, t := range b.triggers {
		if !t.Suspended && t.NextFireAt != nil && !t.NextFireAt.After(at) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, c Trigger) int { return a.NextFireAt.Compare(*c.NextFireAt) })
	return out, nil
}

func (b *TriggerBook) SetNextFire(_ context.Context, id string, at *time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, err := b.trigger(id)
	if err != nil {
		return err
	}
	t.NextFireAt = utcPtr(at)
	if err := b.saveTrigger(t); err != nil {
		return err
	}
	b.triggers[id] = t
	return nil
}

func (b *TriggerBook) LeaseTrigger(_ context.Context, id, holder string, until time.Time) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.trigger(id); err != nil {
		return false, err
	}
	if l, ok := b.leases[id]; ok && l.holder != holder && b.now().Before(l.until) {
		return false, nil
	}
	b.leases[id] = triggerLease{holder: holder, until: until}
	return true, nil
}

func (b *TriggerBook) ReleaseTrigger(_ context.Context, id, holder string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if l, ok := b.leases[id]; ok && l.holder == holder {
		delete(b.leases, id)
	}
	return nil
}

func (b *TriggerBook) ClaimFiring(_ context.Context, f Firing) (Firing, bool, error) {
	if err := CheckFiring(f); err != nil {
		return Firing{}, false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t, err := b.trigger(f.TriggerID)
	if err != nil {
		return Firing{}, false, err
	}
	held, ok := b.firingsOf(f.TriggerID)[f.Dedupe]
	switch {
	case !ok:
		f.Outcome, f.Reason, f.SessionID, f.Open = "", "", "", false
	case held.Outcome == OutcomeFailed || held.Outcome == "":
		// The firing runs again under its first id; the failure it
		// counted is taken back.
		f.ID, f.Outcome, f.Reason, f.SessionID, f.Open = held.ID, "", "", "", false
		if held.Outcome == OutcomeFailed {
			Count(&t.Counts, OutcomeFailed, -1)
			if err := b.saveTrigger(t); err != nil {
				return Firing{}, false, err
			}
		}
	default:
		return held, false, nil
	}
	if err := b.saveFiring(f); err != nil {
		return Firing{}, false, err
	}
	b.triggers[t.ID] = t
	b.firingsOf(f.TriggerID)[f.Dedupe] = f
	return f, true, nil
}

func (b *TriggerBook) RecordFiring(_ context.Context, f Firing, from string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, err := b.trigger(f.TriggerID)
	if err != nil {
		return err
	}
	held, ok := b.firingsOf(f.TriggerID)[f.Dedupe]
	if !ok || held.ID != f.ID || held.Outcome != from {
		return fmt.Errorf("%w: firing %s is not %q", ErrConflict, f.ID, from)
	}
	held.Outcome, held.Reason, held.Key, held.SessionID, held.Open = f.Outcome, f.Reason, f.Key, f.SessionID, f.Open
	Record(&t, held, from, b.now())
	if err := b.saveFiring(held); err != nil {
		return err
	}
	if err := b.saveTrigger(t); err != nil {
		return err
	}
	b.firingsOf(f.TriggerID)[f.Dedupe] = held
	b.triggers[t.ID] = t
	return nil
}

func (b *TriggerBook) CountFiltered(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, err := b.trigger(id)
	if err != nil {
		return err
	}
	Count(&t.Counts, OutcomeFiltered, 1)
	if err := b.saveTrigger(t); err != nil {
		return err
	}
	b.triggers[id] = t
	return nil
}

// sortedFirings is a trigger's firings that keep, oldest first.
func (b *TriggerBook) sortedFirings(id string, keep func(Firing) bool) []Firing {
	var out []Firing
	for _, f := range b.firings[id] {
		if keep(f) {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, c Firing) int { return cmp.Compare(a.ID, c.ID) })
	return out
}

func (b *TriggerBook) Firings(_ context.Context, id string, limit int, cursor string) ([]Firing, string, error) {
	before, err := Uncursor(cursor)
	if err != nil {
		return nil, "", err
	}
	b.mu.Lock()
	if _, err := b.trigger(id); err != nil {
		b.mu.Unlock()
		return nil, "", err
	}
	all := b.sortedFirings(id, func(f Firing) bool { return before == "" || f.ID < before })
	b.mu.Unlock()
	slices.Reverse(all)
	return page(all, limit, func(f Firing) string { return f.ID })
}

func (b *TriggerBook) HeldFirings(context.Context) ([]Firing, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Firing
	for id := range b.firings {
		out = append(out, b.sortedFirings(id, func(f Firing) bool { return f.Outcome == OutcomeHeld })...)
	}
	slices.SortFunc(out, func(a, c Firing) int { return cmp.Compare(a.ID, c.ID) })
	return out, nil
}

func (b *TriggerBook) OpenFirings(_ context.Context, id string) ([]Firing, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.trigger(id); err != nil {
		return nil, err
	}
	return b.sortedFirings(id, func(f Firing) bool { return f.Open }), nil
}

func (b *TriggerBook) CloseSession(_ context.Context, id, sessionID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.trigger(id); err != nil {
		return err
	}
	for dedupe, f := range b.firings[id] {
		if f.SessionID != sessionID || !f.Open {
			continue
		}
		f.Open = false
		if err := b.saveFiring(f); err != nil {
			return err
		}
		b.firings[id][dedupe] = f
	}
	for key, s := range b.keys[id] {
		if s != sessionID {
			continue
		}
		if b.save != nil {
			if err := b.save.SaveKey(id, key, ""); err != nil {
				return err
			}
		}
		delete(b.keys[id], key)
	}
	return nil
}

func (b *TriggerBook) TriggerSession(_ context.Context, id, key string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.keys[id][key]
	if !ok {
		return "", fmt.Errorf("%w: trigger %s names no session by its key", ErrNotFound, id)
	}
	return s, nil
}

func (b *TriggerBook) SetTriggerSession(_ context.Context, id, key, sessionID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.trigger(id); err != nil {
		return err
	}
	if b.save != nil {
		if err := b.save.SaveKey(id, key, sessionID); err != nil {
			return err
		}
	}
	b.keysOf(id)[key] = sessionID
	return nil
}

// utcPtr is at in UTC, nil for nil.
func utcPtr(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	u := at.UTC()
	return &u
}
