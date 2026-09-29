// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"latere.ai/x/topos/session"
)

// Memory keeps the objects in the process, for tests and for a toposd
// whose sessions are also in memory.
type Memory struct {
	mu       sync.Mutex
	now      func() time.Time
	agents   map[string]Agent
	names    map[ownedName]string
	versions map[string][]AgentVersion
	idem     map[[2]string]Idempotency
}

// NewMemory returns an empty store on the clock now, time.Now when nil.
func NewMemory(now func() time.Time) *Memory {
	if now == nil {
		now = time.Now
	}
	return &Memory{now: now, agents: map[string]Agent{}, names: map[ownedName]string{}, versions: map[string][]AgentVersion{}, idem: map[[2]string]Idempotency{}}
}

// ownedName keys the name index: a name is unique within its owner.
type ownedName struct{ owner, name string }

func (m *Memory) Agent(_ context.Context, id string) (Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return Agent{}, fmt.Errorf("%w: agent %s", ErrNotFound, id)
	}
	return a, nil
}

func (m *Memory) AgentByName(_ context.Context, owner, name string) (Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[m.names[ownedName{owner, name}]]
	if !ok {
		return Agent{}, fmt.Errorf("%w: agent %s", ErrNotFound, name)
	}
	return a, nil
}

func (m *Memory) ListAgents(_ context.Context, o AgentList) ([]Agent, string, error) {
	after, err := Uncursor(o.Cursor)
	if err != nil {
		return nil, "", err
	}
	m.mu.Lock()
	var all []Agent
	for _, a := range m.agents {
		if a.ID > after && (len(o.Owners) == 0 || slices.Contains(o.Owners, a.Owner)) {
			all = append(all, a)
		}
	}
	m.mu.Unlock()
	slices.SortFunc(all, func(a, b Agent) int { return cmp.Compare(a.ID, b.ID) })
	return page(all, o.Limit, func(a Agent) string { return a.ID })
}

func (m *Memory) Version(_ context.Context, id string, version int) (AgentVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vs := m.versions[id]
	if version < 1 || version > len(vs) {
		return AgentVersion{}, fmt.Errorf("%w: agent %s version %d", ErrNotFound, id, version)
	}
	return vs[version-1], nil
}

func (m *Memory) Versions(_ context.Context, id string, limit int, cursor string) ([]AgentVersion, string, error) {
	n, err := UncursorVersion(cursor)
	if err != nil {
		return nil, "", err
	}
	m.mu.Lock()
	if _, ok := m.agents[id]; !ok {
		m.mu.Unlock()
		return nil, "", fmt.Errorf("%w: agent %s", ErrNotFound, id)
	}
	vs := slices.Clone(m.versions[id][min(n, len(m.versions[id])):])
	m.mu.Unlock()
	return page(vs, limit, func(v AgentVersion) string { return strconv.Itoa(v.Version) })
}

func (m *Memory) PutVersion(_ context.Context, a Agent, v AgentVersion) error {
	if err := CheckVersion(a, v); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, held := m.agents[v.AgentID]
	if v.Version == 1 {
		key := ownedName{a.Owner, a.Name}
		if held || m.names[key] != "" {
			return fmt.Errorf("%w: agent %s exists", ErrConflict, a.Name)
		}
		a.Latest = 1
		m.agents[a.ID], m.names[key] = a, a.ID
		m.versions[a.ID] = []AgentVersion{v}
		return nil
	}
	if !held || stored.Latest+1 != v.Version {
		return fmt.Errorf("%w: agent %s version %d does not follow the stored latest", ErrConflict, v.AgentID, v.Version)
	}
	stored.Latest = v.Version
	m.agents[stored.ID] = stored
	m.versions[stored.ID] = append(m.versions[stored.ID], v)
	return nil
}

func (m *Memory) Archive(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	switch {
	case !ok:
		return fmt.Errorf("%w: agent %s", ErrNotFound, id)
	case a.ArchivedAt != nil:
		return fmt.Errorf("%w: agent %s is archived", ErrConflict, id)
	}
	at = at.UTC()
	a.ArchivedAt = &at
	m.agents[id] = a
	return nil
}

func (m *Memory) Begin(_ context.Context, r Idempotency) (Idempotency, bool, error) {
	k := [2]string{r.Subject, r.Key}
	m.mu.Lock()
	defer m.mu.Unlock()
	if held, ok := m.idem[k]; ok && m.now().Before(held.ExpiresAt) {
		return held, false, nil
	}
	r.Done = false
	m.idem[k] = r
	return r, true, nil
}

func (m *Memory) Finish(_ context.Context, r Idempotency) error {
	k := [2]string{r.Subject, r.Key}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.idem[k]; !ok {
		return fmt.Errorf("%w: no reserved record for the key", ErrNotFound)
	}
	r.Done = true
	m.idem[k] = r
	return nil
}

func (m *Memory) Abandon(_ context.Context, subject, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.idem, [2]string{subject, key})
	return nil
}

// CheckVersion is the shape every PutVersion refuses before it looks at
// the stored state.
func CheckVersion(a Agent, v AgentVersion) error {
	switch {
	case session.CheckID(session.PrefixAgent, v.AgentID) != nil:
		return fmt.Errorf("%w: %q is not an agent id", session.ErrInvalid, v.AgentID)
	case v.Version < 1:
		return fmt.Errorf("%w: version %d", session.ErrInvalid, v.Version)
	case v.Version == 1 && (a.ID != v.AgentID || a.Name == "" || a.Owner == ""):
		return fmt.Errorf("%w: the first version creates the agent, which needs its id, name and owner", session.ErrInvalid)
	case len(v.Doc) == 0 || len(v.Bundle) == 0 || v.Digest == "":
		return fmt.Errorf("%w: a version holds its document, its bundle and its digest", session.ErrInvalid)
	case !utf8.Valid(v.Doc) || !utf8.Valid(v.Bundle):
		// The document and the bundle are JSON text, and the stores on
		// disk keep them as text.
		return fmt.Errorf("%w: a version's document and bundle are UTF-8 text", session.ErrInvalid)
	}
	return nil
}

// Cursor renders a page's last key as an opaque cursor.
func Cursor(key string) string {
	if key == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(key))
}

// Uncursor reads a cursor Cursor rendered; empty is the first page.
func Uncursor(c string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", fmt.Errorf("%w: the cursor was not issued by this server", session.ErrInvalid)
	}
	return string(b), nil
}

// UncursorVersion reads a cursor Versions rendered: the version the
// previous page ended at, 0 for the first page.
func UncursorVersion(c string) (int, error) {
	after, err := Uncursor(c)
	if err != nil || after == "" {
		return 0, err
	}
	n, err := strconv.Atoi(after)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: the cursor names no version", session.ErrInvalid)
	}
	return n, nil
}

// page cuts a sorted list at limit and returns the cursor after its last
// item, or none when nothing follows.
func page[T any](all []T, limit int, key func(T) string) ([]T, string, error) {
	if limit <= 0 {
		limit = session.DefaultListLimit
	}
	if len(all) <= limit {
		return all, "", nil
	}
	return all[:limit], Cursor(key(all[limit-1])), nil
}
