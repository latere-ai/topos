// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package dir is the directory store of spec 014 for the objects other
// than sessions. Each object is one JSON file under the data directory's
// objects/, replaced atomically (a temporary file, fsync, rename, fsync
// of the directory):
//
//	objects/agent/<agent_id>.json              an agent: name, owner, latest version, archive time
//	objects/agent_version/<agent_id>/<n>.json  version n of the agent: digest, document, bundle
//	objects/idempotency/<hex>.json             an idempotency record; hex is the SHA-256 of its subject and key
//
// An agent's file is the commit record of its versions: a version's file
// is written first, and it is visible once the agent's latest version
// counts it. A crash between the two writes leaves a version file no
// reader opens, which the next PutVersion of that number replaces, and
// never an agent whose latest version is missing. The agents and the
// name index are read from the agent files at Open and kept in the
// process; versions and idempotency records are read from their files.
//
// The directory holds one process, toposd serve's (spec 014), and a
// mutex serializes its calls.
package dir

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/atomicfile"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/session"
)

// The kinds of object, each a directory under objects/.
const (
	kindAgent       = "agent"
	kindVersion     = "agent_version"
	kindIdempotency = "idempotency"
)

// writeFile replaces a file durably. Tests replace it to fail a write at
// a chosen file.
var writeFile = atomicfile.WriteSync

// Store is the directory store of agents, their versions and
// idempotency records.
type Store struct {
	root string
	now  func() time.Time

	mu     sync.Mutex
	agents map[string]store.Agent
	names  map[string]string
}

// agentFile is an agent's file.
type agentFile struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Owner      string     `json:"owner"`
	Latest     int        `json:"latest_version"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// versionFile is a version's file. The document and the bundle are JSON
// text and kept as strings, so the file reads as text.
type versionFile struct {
	AgentID   string    `json:"agent_id"`
	Version   int       `json:"version"`
	Digest    string    `json:"digest"`
	Doc       string    `json:"doc"`
	Bundle    string    `json:"bundle"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// idempotencyFile is an idempotency record's file. The body is the
// answer's bytes, of any content type, so it is kept as base64.
type idempotencyFile struct {
	Subject     string    `json:"subject"`
	Key         string    `json:"key"`
	Route       string    `json:"route"`
	BodyHash    string    `json:"body_hash"`
	Done        bool      `json:"done"`
	Status      int       `json:"status"`
	ContentType string    `json:"content_type"`
	Body        []byte    `json:"body"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Open returns the store over dataDir/objects, creating its directories,
// on the clock now, time.Now when nil. It reads every agent file; one
// that does not decode is ErrCorrupt. A file whose name is not an
// object's, such as the temporary file of a write a crash interrupted,
// is ignored.
func Open(dataDir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	root := filepath.Join(dataDir, "objects")
	for _, kind := range []string{kindAgent, kindVersion, kindIdempotency} {
		if err := os.MkdirAll(filepath.Join(root, kind), 0o700); err != nil {
			return nil, fmt.Errorf("dir: create %s: %w", kind, err)
		}
	}
	for _, d := range []string{root, dataDir} {
		if err := syncDir(d); err != nil {
			return nil, err
		}
	}
	s := &Store{root: root, now: now, agents: map[string]store.Agent{}, names: map[string]string{}}
	entries, err := os.ReadDir(filepath.Join(root, kindAgent))
	if err != nil {
		return nil, fmt.Errorf("dir: list agents: %w", err)
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !store.IsAgentID(id) {
			continue
		}
		var f agentFile
		if err := readJSON(s.agentPath(id), &f); err != nil {
			return nil, err
		}
		switch {
		case f.ID != id:
			return nil, fmt.Errorf("%w: %s holds agent %q", session.ErrCorrupt, e.Name(), f.ID)
		case s.names[f.Name] != "":
			return nil, fmt.Errorf("%w: agents %s and %s are both named %q", session.ErrCorrupt, s.names[f.Name], id, f.Name)
		}
		s.agents[id], s.names[f.Name] = f.agent(), id
	}
	return s, nil
}

func (s *Store) agentPath(id string) string {
	return filepath.Join(s.root, kindAgent, id+".json")
}

func (s *Store) versionPath(id string, version int) string {
	return filepath.Join(s.root, kindVersion, id, strconv.Itoa(version)+".json")
}

// idempotencyPath names a record's file by the SHA-256 of its subject,
// length-prefixed so no two pairs share an input, and its key.
func (s *Store) idempotencyPath(subject, key string) string {
	sum := sha256.Sum256([]byte(strconv.Itoa(len(subject)) + ":" + subject + key))
	return filepath.Join(s.root, kindIdempotency, hex.EncodeToString(sum[:])+".json")
}

func (f agentFile) agent() store.Agent {
	a := store.Agent{ID: f.ID, Name: f.Name, Owner: f.Owner, Latest: f.Latest, CreatedAt: f.CreatedAt.UTC()}
	if f.ArchivedAt != nil {
		at := f.ArchivedAt.UTC()
		a.ArchivedAt = &at
	}
	return a
}

func (s *Store) Agent(_ context.Context, ref string) (store.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := ref
	if !store.IsAgentID(ref) {
		id = s.names[ref]
	}
	a, ok := s.agents[id]
	if !ok {
		return store.Agent{}, fmt.Errorf("%w: agent %s", store.ErrNotFound, ref)
	}
	return a, nil
}

func (s *Store) ListAgents(_ context.Context, o store.AgentList) ([]store.Agent, string, error) {
	after, err := store.Uncursor(o.Cursor)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	var all []store.Agent
	for _, a := range s.agents {
		if a.ID > after && (len(o.Owners) == 0 || slices.Contains(o.Owners, a.Owner)) {
			all = append(all, a)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(all, func(a, b store.Agent) int { return cmp.Compare(a.ID, b.ID) })
	limit := o.Limit
	if limit <= 0 {
		limit = session.DefaultListLimit
	}
	if len(all) <= limit {
		return all, "", nil
	}
	return all[:limit], store.Cursor(all[limit-1].ID), nil
}

func (s *Store) Version(_ context.Context, id string, version int) (store.AgentVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[id]
	if !ok || version < 1 || version > a.Latest {
		return store.AgentVersion{}, fmt.Errorf("%w: agent %s version %d", store.ErrNotFound, id, version)
	}
	return s.readVersion(id, version)
}

func (s *Store) Versions(_ context.Context, id string, limit int, cursor string) ([]store.AgentVersion, string, error) {
	after, err := store.UncursorVersion(cursor)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[id]
	if !ok {
		return nil, "", fmt.Errorf("%w: agent %s", store.ErrNotFound, id)
	}
	if limit <= 0 {
		limit = session.DefaultListLimit
	}
	out := []store.AgentVersion{}
	if after >= a.Latest {
		return out, "", nil
	}
	last := min(a.Latest, after+limit)
	for n := after + 1; n <= last; n++ {
		v, err := s.readVersion(id, n)
		if err != nil {
			return nil, "", err
		}
		out = append(out, v)
	}
	if last < a.Latest {
		return out, store.Cursor(strconv.Itoa(last)), nil
	}
	return out, "", nil
}

// readVersion reads a version the agent counts; a file that is missing
// or does not hold that version is ErrCorrupt.
func (s *Store) readVersion(id string, version int) (store.AgentVersion, error) {
	var f versionFile
	if err := readJSON(s.versionPath(id, version), &f); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return store.AgentVersion{}, fmt.Errorf("%w: agent %s counts version %d, which has no file", session.ErrCorrupt, id, version)
		}
		return store.AgentVersion{}, err
	}
	if f.AgentID != id || f.Version != version {
		return store.AgentVersion{}, fmt.Errorf("%w: the file of agent %s version %d holds agent %q version %d", session.ErrCorrupt, id, version, f.AgentID, f.Version)
	}
	return store.AgentVersion{
		AgentID: f.AgentID, Version: f.Version, Digest: f.Digest, Doc: []byte(f.Doc), Bundle: []byte(f.Bundle),
		CreatedBy: f.CreatedBy, CreatedAt: f.CreatedAt.UTC(),
	}, nil
}

// PutVersion writes the version's file, then the agent's, whose latest
// version makes the version visible.
func (s *Store) PutVersion(_ context.Context, a store.Agent, v store.AgentVersion) error {
	if err := store.CheckVersion(a, v); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, held := s.agents[v.AgentID]
	if v.Version == 1 {
		if held || s.names[a.Name] != "" {
			return fmt.Errorf("%w: agent %s exists", store.ErrConflict, a.Name)
		}
		stored = a
		stored.Latest = 1
		if err := s.versionDir(a.ID); err != nil {
			return err
		}
	} else {
		if !held || stored.Latest+1 != v.Version {
			return fmt.Errorf("%w: agent %s version %d does not follow the stored latest", store.ErrConflict, v.AgentID, v.Version)
		}
		stored.Latest = v.Version
	}
	f := versionFile{AgentID: v.AgentID, Version: v.Version, Digest: v.Digest, Doc: string(v.Doc), Bundle: string(v.Bundle), CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt}
	if err := write(s.versionPath(v.AgentID, v.Version), f); err != nil {
		return err
	}
	if err := s.writeAgent(stored); err != nil {
		return err
	}
	s.agents[stored.ID], s.names[stored.Name] = stored, stored.ID
	return nil
}

// versionDir creates the directory of an agent's versions and makes its
// entry durable before a version is written into it.
func (s *Store) versionDir(id string) error {
	parent := filepath.Join(s.root, kindVersion)
	if err := os.MkdirAll(filepath.Join(parent, id), 0o700); err != nil {
		return fmt.Errorf("dir: create the versions of %s: %w", id, err)
	}
	return syncDir(parent)
}

func (s *Store) writeAgent(a store.Agent) error {
	return write(s.agentPath(a.ID), agentFile{ID: a.ID, Name: a.Name, Owner: a.Owner, Latest: a.Latest, ArchivedAt: a.ArchivedAt, CreatedAt: a.CreatedAt})
}

func (s *Store) Archive(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agents[id]
	switch {
	case !ok:
		return fmt.Errorf("%w: agent %s", store.ErrNotFound, id)
	case a.ArchivedAt != nil:
		return fmt.Errorf("%w: agent %s is archived", store.ErrConflict, id)
	}
	at = at.UTC()
	a.ArchivedAt = &at
	if err := s.writeAgent(a); err != nil {
		return err
	}
	s.agents[id] = a
	return nil
}

func (s *Store) Begin(_ context.Context, r store.Idempotency) (store.Idempotency, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held, ok, err := s.readIdempotency(r.Subject, r.Key)
	if err != nil {
		return store.Idempotency{}, false, err
	}
	if ok && s.now().Before(held.ExpiresAt) {
		return held, false, nil
	}
	r.Done = false
	if err := s.writeIdempotency(r); err != nil {
		return store.Idempotency{}, false, err
	}
	return r, true, nil
}

func (s *Store) Finish(_ context.Context, r store.Idempotency) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok, err := s.readIdempotency(r.Subject, r.Key)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no reserved record for the key", store.ErrNotFound)
	}
	r.Done = true
	return s.writeIdempotency(r)
}

func (s *Store) Abandon(_ context.Context, subject, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.idempotencyPath(subject, key)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("dir: remove an idempotency record: %w", err)
	}
	return syncDir(filepath.Join(s.root, kindIdempotency))
}

// readIdempotency reads the record of (subject, key) and reports whether
// there is one. A file that holds another pair is ErrCorrupt.
func (s *Store) readIdempotency(subject, key string) (store.Idempotency, bool, error) {
	var f idempotencyFile
	if err := readJSON(s.idempotencyPath(subject, key), &f); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return store.Idempotency{}, false, nil
		}
		return store.Idempotency{}, false, err
	}
	if f.Subject != subject || f.Key != key {
		return store.Idempotency{}, false, fmt.Errorf("%w: the file of the idempotency record of %q holds the record of %q", session.ErrCorrupt, subject, f.Subject)
	}
	return store.Idempotency{
		Subject: f.Subject, Key: f.Key, Route: f.Route, BodyHash: f.BodyHash, Done: f.Done,
		Status: f.Status, ContentType: f.ContentType, Body: f.Body, ExpiresAt: f.ExpiresAt.UTC(),
	}, true, nil
}

func (s *Store) writeIdempotency(r store.Idempotency) error {
	return write(s.idempotencyPath(r.Subject, r.Key), idempotencyFile{
		Subject: r.Subject, Key: r.Key, Route: r.Route, BodyHash: r.BodyHash, Done: r.Done,
		Status: r.Status, ContentType: r.ContentType, Body: r.Body, ExpiresAt: r.ExpiresAt,
	})
}

// readJSON decodes the file at path into v. A missing file is returned
// as fs.ErrNotExist; one that does not decode is ErrCorrupt.
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("dir: read %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%w: %s: %w", session.ErrCorrupt, filepath.Base(path), err)
	}
	return nil
}

// write encodes v and replaces the file at path with it.
func write(path string, v any) error {
	b, err := session.Marshal(v)
	if err != nil {
		return fmt.Errorf("dir: encode %s: %w", filepath.Base(path), err)
	}
	if err := writeFile(path, b, 0o600); err != nil {
		return fmt.Errorf("dir: write %s: %w", filepath.Base(path), err)
	}
	return nil
}

// syncDir makes a directory's entries durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("dir: open directory: %w", err)
	}
	serr := d.Sync()
	cerr := d.Close()
	if serr != nil {
		return fmt.Errorf("dir: sync directory %s: %w", dir, serr)
	}
	if cerr != nil {
		return fmt.Errorf("dir: close directory: %w", cerr)
	}
	return nil
}

var _ store.Store = (*Store)(nil)
