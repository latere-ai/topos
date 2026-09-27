// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package store keeps toposd's objects other than sessions (spec 014):
// agents with their versions, and the idempotency records of the API's
// POST routes (spec 015). Sessions are session.Store's. NewMemory keeps
// them in the process; the directory and Postgres stores keep them on
// disk, and storetest is the suite every implementation passes.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// Errors every implementation returns.
var (
	// ErrNotFound is an object the store does not hold. It is
	// manifest.ErrNotFound, so the store answers the resolver's lookups
	// as they are.
	ErrNotFound = manifest.ErrNotFound
	// ErrConflict is a write the stored state refuses: a version that
	// does not follow the latest, a second agent of one name, or an
	// archive of an archived agent.
	ErrConflict = errors.New("store: the stored state refuses the write")
)

// Agent is one stored agent.
type Agent struct {
	ID         string
	Name       string
	Owner      string
	Latest     int
	ArchivedAt *time.Time
	CreatedAt  time.Time
}

// AgentVersion is one version of an agent: its resolved manifest and the
// bundle a session keeps (spec 003).
type AgentVersion struct {
	AgentID string
	Version int
	Digest  string
	// Doc is the resolved Agent, status included, as JSON.
	Doc []byte
	// Bundle is the agent and the agents it pins, as manifest.Bundle
	// renders them.
	Bundle    []byte
	CreatedBy string
	CreatedAt time.Time
}

// AgentList narrows ListAgents. Owners, when set, keeps the agents of
// those owners; Limit is the page size, session.DefaultListLimit when
// zero; Cursor is the previous page's next cursor.
type AgentList struct {
	Owners []string
	Limit  int
	Cursor string
}

// Agents keeps agents and their versions.
type Agents interface {
	// Agent returns an agent by name or agent_ id.
	Agent(ctx context.Context, ref string) (Agent, error)
	// ListAgents lists agents oldest first and returns the next page's
	// cursor, empty on the last page.
	ListAgents(ctx context.Context, o AgentList) ([]Agent, string, error)
	// Version returns one version of the agent id.
	Version(ctx context.Context, id string, version int) (AgentVersion, error)
	// Versions lists the agent's versions oldest first, paged like
	// ListAgents.
	Versions(ctx context.Context, id string, limit int, cursor string) ([]AgentVersion, string, error)
	// PutVersion stores a version. Version 1 creates the agent a names,
	// and ErrConflict answers a name or id already held; a later version
	// must follow the stored latest, and ErrConflict answers any other.
	PutVersion(ctx context.Context, a Agent, v AgentVersion) error
	// Archive archives an agent; ErrConflict answers an archived one.
	Archive(ctx context.Context, id string, at time.Time) error
}

// Idempotency is one idempotency record: the answer the first request
// with a subject's key got, kept until it expires (spec 015).
type Idempotency struct {
	Subject  string
	Key      string
	Route    string
	BodyHash string
	// Done is false while the first request is still being answered.
	Done        bool
	Status      int
	ContentType string
	Body        []byte
	ExpiresAt   time.Time
}

// Idempotencies keeps idempotency records.
type Idempotencies interface {
	// Begin reserves a record for (r.Subject, r.Key) and reports true, or
	// returns the record already held, unexpired, and false.
	Begin(ctx context.Context, r Idempotency) (Idempotency, bool, error)
	// Finish stores the answer of a reserved record.
	Finish(ctx context.Context, r Idempotency) error
	// Abandon drops a reserved record whose request failed, so a retry
	// runs again.
	Abandon(ctx context.Context, subject, key string) error
}

// Store is every object store toposd needs beside session.Store.
type Store interface {
	Agents
	Idempotencies
}

// Lookup answers the resolver from the agents a store keeps. A ref of
// agent_<id>@<n> is that version, any other the latest. The kinds whose
// stores are not built yet hold nothing.
func Lookup(a Agents) manifest.Lookup { return lookup{a} }

type lookup struct{ a Agents }

func (l lookup) Agent(ctx context.Context, ref string) (*v1.Agent, error) {
	name, n, pinned := strings.Cut(ref, "@")
	stored, err := l.a.Agent(ctx, name)
	if err != nil {
		return nil, err
	}
	version := stored.Latest
	if pinned {
		if _, err := fmt.Sscanf(n, "%d", &version); err != nil || fmt.Sprint(version) != n {
			return nil, fmt.Errorf("%w: %s names no version", ErrNotFound, ref)
		}
	}
	v, err := l.a.Version(ctx, stored.ID, version)
	if err != nil {
		return nil, err
	}
	return DecodeAgent(v.Doc)
}

func (lookup) Trigger(context.Context, string) (*v1.Trigger, error) { return nil, ErrNotFound }

func (lookup) MemoryStore(context.Context, string) (*v1.MemoryStore, error) {
	return nil, ErrNotFound
}

func (lookup) Connection(context.Context, string) (*v1.Connection, error) {
	return nil, ErrNotFound
}

func (lookup) Credential(context.Context, string) (string, error) { return "", ErrNotFound }

// DecodeAgent reads a stored version's Doc.
func DecodeAgent(doc []byte) (*v1.Agent, error) {
	r, err := manifest.ReadBundle(doc)
	if err != nil {
		return nil, fmt.Errorf("store: a stored agent does not read: %w", err)
	}
	return r.Agent, nil
}

// IsAgentID reports whether ref is an agent_ id rather than a name.
func IsAgentID(ref string) bool { return session.CheckID(session.PrefixAgent, ref) == nil }
