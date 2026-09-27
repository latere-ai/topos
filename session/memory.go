// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// NewMemoryStore returns a Store that keeps everything in memory, for
// tests and embedders that keep nothing.
func NewMemoryStore() Store {
	return &memoryStore{sessions: map[string]*memSession{}, now: time.Now}
}

type memoryStore struct {
	mu       sync.Mutex
	sessions map[string]*memSession
	now      func() time.Time
}

type memSession struct {
	s      Session
	events []Event
	blobs  map[Digest][]byte
	lease  *memLease
	signal chan struct{} // closed and replaced on every change
}

func (m *memoryStore) get(id string) (*memSession, error) {
	ms, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return ms, nil
}

func (ms *memSession) notify() {
	close(ms.signal)
	ms.signal = make(chan struct{})
}

func (m *memoryStore) Create(ctx context.Context, s Session, blobs map[Digest][]byte) error {
	if err := CheckCreate(s, blobs); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[s.ID]; ok {
		return fmt.Errorf("%w: %s", ErrExists, s.ID)
	}
	ms := &memSession{s: cloneSession(s), blobs: map[Digest][]byte{}, signal: make(chan struct{})}
	for d, b := range blobs {
		ms.blobs[d] = slices.Clone(b)
	}
	m.sessions[s.ID] = ms
	return nil
}

func (m *memoryStore) Get(ctx context.Context, id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, err := m.get(id)
	if err != nil {
		return Session{}, err
	}
	return cloneSession(ms.s), nil
}

func (m *memoryStore) List(ctx context.Context, o ListOptions) ([]Session, string, error) {
	m.mu.Lock()
	all := make([]Session, 0, len(m.sessions))
	for _, ms := range m.sessions {
		all = append(all, cloneSession(ms.s))
	}
	m.mu.Unlock()
	page, next := ListPage(all, o)
	return page, next, nil
}

// ListPage filters sessions by o and returns one page, newest first, and
// the cursor of the next page ("" on the last). Stores that list by
// reading every header share it.
func ListPage(all []Session, o ListOptions) ([]Session, string) {
	slices.SortFunc(all, func(a, b Session) int { return strings.Compare(b.ID, a.ID) })
	limit := o.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	var page []Session
	for _, s := range all {
		if o.Cursor != "" && s.ID >= o.Cursor {
			continue
		}
		if o.Status != "" && s.Status != o.Status {
			continue
		}
		if o.AgentID != "" && s.Agent.ID != o.AgentID {
			continue
		}
		if len(page) == limit {
			return page, page[len(page)-1].ID
		}
		page = append(page, s)
	}
	return page, ""
}

func (m *memoryStore) Append(ctx context.Context, id string, afterSeq uint64, events []Event) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, err := m.get(id)
	if err != nil {
		return 0, err
	}
	retried, err := CheckBatch(id, ms.s.LastSeq, afterSeq, events, func(from uint64) ([]Event, error) {
		return cloneEvents(ms.events[from-1:]), nil
	})
	if err != nil {
		return 0, err
	}
	last := afterSeq + uint64(len(events))
	if retried {
		return last, nil
	}
	ms.events = append(ms.events, cloneEvents(events)...)
	ApplyBatch(&ms.s, events)
	ms.notify()
	return last, nil
}

func (m *memoryStore) Events(ctx context.Context, id string, fromSeq uint64, limit int) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, err := m.get(id)
	if err != nil {
		return nil, err
	}
	return window(ms.events, fromSeq, limit), nil
}

// window returns the events from fromSeq, at most limit of them (no
// bound when limit is 0 or less), from a log dense from 1.
func window(events []Event, fromSeq uint64, limit int) []Event {
	start := int(max(fromSeq, 1)) - 1
	if start >= len(events) {
		return []Event{}
	}
	out := events[start:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return cloneEvents(out)
}

func (m *memoryStore) Watch(ctx context.Context, id string, fromSeq uint64) (<-chan Event, error) {
	m.mu.Lock()
	if _, err := m.get(id); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	m.mu.Unlock()
	out := make(chan Event)
	go func() {
		defer close(out)
		next := max(fromSeq, 1)
		for {
			m.mu.Lock()
			ms, ok := m.sessions[id]
			if !ok {
				m.mu.Unlock()
				return
			}
			evs := window(ms.events, next, 0)
			sig := ms.signal
			m.mu.Unlock()
			for _, e := range evs {
				select {
				case out <- e:
					next = e.Seq + 1
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-sig:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (m *memoryStore) PutBlob(ctx context.Context, id string, r io.Reader) (Digest, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("session: read blob: %w", err)
	}
	d := DigestOf(b)
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, err := m.get(id)
	if err != nil {
		return "", err
	}
	ms.blobs[d] = b
	return d, nil
}

func (m *memoryStore) Blob(ctx context.Context, id string, d Digest) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, err := m.get(id)
	if err != nil {
		return nil, err
	}
	b, ok := ms.blobs[d]
	if !ok {
		return nil, fmt.Errorf("%w: blob %s", ErrNotFound, d)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memoryStore) Redact(ctx context.Context, id, eventID string, by Sender, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, err := m.get(id)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(ms.events, func(e Event) bool { return e.ID == eventID })
	if i < 0 {
		return fmt.Errorf("%w: event %s", ErrNotFound, eventID)
	}
	if ms.events[i].Redacted() {
		return nil
	}
	orphans := OrphanBlobs(ms.events[i], ms.events)
	tomb, red, err := Tombstone(ms.events[i], ms.s.LastSeq, by, reason, m.now())
	if err != nil {
		return err
	}
	ms.events[i] = tomb
	ms.events = append(ms.events, red)
	ApplyBatch(&ms.s, []Event{red})
	for _, d := range orphans {
		delete(ms.blobs, d)
	}
	ms.notify()
	return nil
}

func (m *memoryStore) Acquire(ctx context.Context, id string, holder Holder) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, err := m.get(id)
	if err != nil {
		return nil, err
	}
	if ms.lease != nil {
		return nil, &LockedError{Holder: ms.lease.holder}
	}
	if holder.AcquiredAt.IsZero() {
		holder.AcquiredAt = m.now().UTC()
	}
	l := &memLease{store: m, ms: ms, holder: holder, lost: make(chan struct{})}
	ms.lease = l
	return l, nil
}

func (m *memoryStore) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, err := m.get(id)
	if err != nil {
		return err
	}
	if ms.lease != nil {
		return &LockedError{Holder: ms.lease.holder}
	}
	delete(m.sessions, id)
	ms.notify()
	return nil
}

type memLease struct {
	store  *memoryStore
	ms     *memSession
	holder Holder
	lost   chan struct{}
	done   bool
}

func (l *memLease) Renew(ctx context.Context) error {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	if l.done {
		return fmt.Errorf("%w: lease released", ErrLocked)
	}
	return nil
}

func (l *memLease) Release() error {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()
	if l.done {
		return nil
	}
	l.done = true
	if l.ms.lease == l {
		l.ms.lease = nil
	}
	close(l.lost)
	return nil
}

func (l *memLease) Lost() <-chan struct{} { return l.lost }

func cloneEvents(in []Event) []Event {
	out := make([]Event, len(in))
	for i, e := range in {
		e.Payload = slices.Clone(e.Payload)
		out[i] = e
	}
	return out
}

func cloneSession(s Session) Session {
	s.Resources = slices.Clone(s.Resources)
	s.Scope = slices.Clone(s.Scope)
	if s.Writer != nil {
		w := *s.Writer
		s.Writer = &w
	}
	if s.Parent != nil {
		p := *s.Parent
		s.Parent = &p
	}
	if s.Budget.MaxCostUSDMicro != nil {
		v := *s.Budget.MaxCostUSDMicro
		s.Budget.MaxCostUSDMicro = &v
	}
	s.Metadata = maps.Clone(s.Metadata)
	return s
}
