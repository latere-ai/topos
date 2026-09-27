// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	v1 "latere.ai/x/topos/manifest/v1"
)

// store is an in-memory Lookup that keeps every version it is given.
type store struct {
	agents []*v1.Agent
	trigs  []*v1.Trigger
	mems   []*v1.MemoryStore
	conns  []*v1.Connection
	creds  map[string]string
	// fail answers every call with this error when set.
	fail error
	// ids counts the ids minted for resolves against this store, so two
	// resolves never mint the same one.
	ids int
}

func newStore() *store {
	return &store{creds: map[string]string{"github-bot": "cred_" + strings.Repeat("7", 26)}}
}

// apply keeps each resolved object whose version it does not hold yet,
// the way an apply stores what Resolve wrote.
func (s *store) apply(rs []Resolved) {
	for _, r := range rs {
		switch {
		case r.Agent != nil:
			if !s.holds(r.Agent.Status) {
				s.agents = append(s.agents, r.Agent)
			}
		case r.Trigger != nil:
			s.trigs = append(s.trigs, r.Trigger)
		case r.MemoryStore != nil:
			s.mems = append(s.mems, r.MemoryStore)
		case r.Connection != nil:
			s.conns = append(s.conns, r.Connection)
		}
	}
}

func (s *store) holds(st v1.Status) bool {
	for _, a := range s.agents {
		if a.Status.ID == st.ID && a.Status.Version == st.Version {
			return true
		}
	}
	return false
}

func (s *store) Agent(_ context.Context, ref string) (*v1.Agent, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	id, ver, pinned := strings.Cut(ref, "@")
	var found *v1.Agent
	for _, a := range s.agents {
		switch {
		case pinned && a.Status.ID == id && fmt.Sprint(a.Status.Version) == ver:
			return a, nil
		case !pinned && (a.Status.ID == ref || a.Metadata.Name == ref):
			found = a
		}
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

func (s *store) Trigger(_ context.Context, name string) (*v1.Trigger, error) {
	return latest(s, s.trigs, func(t *v1.Trigger) bool { return t.Metadata.Name == name })
}

func (s *store) MemoryStore(_ context.Context, ref string) (*v1.MemoryStore, error) {
	return latest(s, s.mems, func(m *v1.MemoryStore) bool { return m.Metadata.Name == ref || m.Status.ID == ref })
}

func (s *store) Connection(_ context.Context, name string) (*v1.Connection, error) {
	return latest(s, s.conns, func(c *v1.Connection) bool { return c.Metadata.Name == name })
}

func (s *store) Credential(_ context.Context, ref string) (string, error) {
	if s.fail != nil {
		return "", s.fail
	}
	if strings.HasPrefix(ref, "cred_") {
		return ref, nil
	}
	if id, ok := s.creds[ref]; ok {
		return id, nil
	}
	return "", ErrNotFound
}

func latest[T any](s *store, list []*T, match func(*T) bool) (*T, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	var found *T
	for _, x := range list {
		if match(x) {
			found = x
		}
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

// fixed are options that make a resolve reproducible: ids counted from
// one, across every resolve against the same store, and a fixed clock.
func fixed(l Lookup) Options {
	n := new(int)
	if s, ok := l.(*store); ok {
		n = &s.ids
	}
	return Options{
		Lookup: l,
		Files: fstest.MapFS{
			"triager.md": {Data: []byte("Label each new issue by the area it touches and ask for a reproduction\nwhen one is missing.\n")},
		},
		NewID: func(prefix string) string {
			*n++
			return fmt.Sprintf("%s%026d", prefix, *n)
		},
		Now: func() time.Time { return time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC) },
	}
}

// refused resolves a manifest that must be refused with code and returns
// the error.
func refused(t *testing.T, code, body string, o Options) *Error {
	t.Helper()
	_, err := Resolve(t.Context(), []byte(body), o)
	e, ok := errors.AsType[*Error](err)
	if !ok || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	if e.Message != messages[code] {
		t.Fatalf("message %q", e.Message)
	}
	return e
}

// hasProblem reports whether an error lists a problem at path whose
// detail contains want.
func hasProblem(e *Error, path, want string) bool {
	for _, p := range e.Problems {
		if p.Path == path && strings.Contains(p.Detail, want) {
			return true
		}
	}
	return false
}

// agent is a minimal Agent document with the given spec lines, each
// indented under spec.
func agent(name string, spec ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: " + name + "\nspec:\n")
	if len(spec) == 0 {
		spec = []string{"model: {name: claude-haiku-4-5}"}
	}
	for _, l := range spec {
		b.WriteString("  " + l + "\n")
	}
	return b.String()
}

func one(t *testing.T, body string, o Options) Resolved {
	t.Helper()
	rs, err := Resolve(t.Context(), []byte(body), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("%d documents", len(rs))
	}
	return rs[0]
}
