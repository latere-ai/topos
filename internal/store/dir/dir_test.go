// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/atomicfile"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/internal/store/storetest"
	"latere.ai/x/topos/session"
)

func open(t *testing.T, dataDir string, now func() time.Time) *Store {
	t.Helper()
	s, err := Open(dataDir, now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestObjectStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T, now func() time.Time) store.Store { return open(t, t.TempDir(), now) })
}

// failWrites makes every write of a file whose path contains part fail,
// until the test ends.
func failWrites(t *testing.T, part string) error {
	t.Helper()
	injected := errors.New("injected write failure")
	writeFile = func(path string, b []byte, perm os.FileMode) error {
		if strings.Contains(path, part) {
			return injected
		}
		return atomicfile.WriteSync(path, b, perm)
	}
	t.Cleanup(func() { writeFile = atomicfile.WriteSync })
	return injected
}

func sameAgent(a, b store.Agent) bool {
	archived := a.ArchivedAt == nil && b.ArchivedAt == nil || a.ArchivedAt != nil && b.ArchivedAt != nil && a.ArchivedAt.Equal(*b.ArchivedAt)
	return a.ID == b.ID && a.Name == b.Name && a.Owner == b.Owner && a.Latest == b.Latest && a.CreatedAt.Equal(b.CreatedAt) && archived
}

func TestReopenReadsEverythingBack(t *testing.T) {
	root := t.TempDir()
	clock := storetest.NewClock()
	st := open(t, root, clock.Now)
	id := storetest.Apply(t, st, "alice", "reviewer", "one").Agent.Status.ID
	storetest.Apply(t, st, "alice", "reviewer", "two")
	other := storetest.Apply(t, st, "bob", "builder", "x").Agent.Status.ID
	if err := st.Archive(t.Context(), other, clock.Now()); err != nil {
		t.Fatal(err)
	}
	done := store.Idempotency{Subject: "alice", Key: "k1", Route: "POST /v1/sessions", BodyHash: "h1", ExpiresAt: clock.Now().Add(time.Hour)}
	if _, _, err := st.Begin(t.Context(), done); err != nil {
		t.Fatal(err)
	}
	done.Status, done.ContentType, done.Body = 201, "application/json", []byte(`{"id":"ses_1"}`)
	if err := st.Finish(t.Context(), done); err != nil {
		t.Fatal(err)
	}
	running := store.Idempotency{Subject: "bob", Key: "k2", Route: "POST /v1/agents", BodyHash: "h2", ExpiresAt: clock.Now().Add(time.Hour)}
	if _, _, err := st.Begin(t.Context(), running); err != nil {
		t.Fatal(err)
	}

	again := open(t, root, clock.Now)
	for _, ref := range []string{"reviewer", id, "builder", other} {
		before, err := st.Agent(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		after, err := again.Agent(t.Context(), ref)
		if err != nil || !sameAgent(before, after) {
			t.Fatalf("Agent(%s) after reopening = %+v, %v; before %+v", ref, after, err, before)
		}
	}
	listed, next, err := again.ListAgents(t.Context(), store.AgentList{Owners: []string{"bob"}})
	if err != nil || next != "" || len(listed) != 1 || listed[0].ID != other || listed[0].ArchivedAt == nil {
		t.Fatalf("bob's agents after reopening %+v, %q, %v", listed, next, err)
	}
	vs, next, err := again.Versions(t.Context(), id, 0, "")
	if err != nil || next != "" || len(vs) != 2 {
		t.Fatalf("versions after reopening %+v, %q, %v", vs, next, err)
	}
	for _, v := range vs {
		was, err := st.Version(t.Context(), id, v.Version)
		if err != nil {
			t.Fatal(err)
		}
		if v.Digest != was.Digest || !bytes.Equal(v.Doc, was.Doc) || !bytes.Equal(v.Bundle, was.Bundle) || v.CreatedBy != was.CreatedBy || !v.CreatedAt.Equal(was.CreatedAt) {
			t.Fatalf("version %d after reopening %+v, before %+v", v.Version, v, was)
		}
	}
	a, err := store.Lookup(again).Agent(t.Context(), id+"@1")
	if err != nil || a.Status.Version != 1 {
		t.Fatalf("the first version after reopening: %+v, %v", a, err)
	}
	dup := store.Agent{ID: session.NewID(session.PrefixAgent), Name: "reviewer", Owner: "carol"}
	if err := again.PutVersion(t.Context(), dup, store.AgentVersion{AgentID: dup.ID, Version: 1, Digest: vs[0].Digest, Doc: vs[0].Doc, Bundle: vs[0].Bundle}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("the name index after reopening: %v", err)
	}
	held, fresh, err := again.Begin(t.Context(), done)
	if err != nil || fresh || !held.Done || held.Status != 201 || held.ContentType != "application/json" || string(held.Body) != `{"id":"ses_1"}` || held.Route != done.Route || held.BodyHash != "h1" || !held.ExpiresAt.Equal(done.ExpiresAt) {
		t.Fatalf("the finished record after reopening %+v, %v, %v", held, fresh, err)
	}
	held, fresh, err = again.Begin(t.Context(), running)
	if err != nil || fresh || held.Done || held.BodyHash != "h2" {
		t.Fatalf("the reserved record after reopening %+v, %v, %v", held, fresh, err)
	}
}

func TestATornTemporaryFileIsIgnoredAtOpen(t *testing.T) {
	root := t.TempDir()
	st := open(t, root, nil)
	id := storetest.Apply(t, st, "alice", "reviewer", "one").Agent.Status.ID
	objects := filepath.Join(root, "objects")
	torn := []byte(`{"id":"` + id + `","na`)
	for _, dir := range []string{kindAgent, filepath.Join(kindVersion, id), kindIdempotency} {
		if err := os.WriteFile(filepath.Join(objects, dir, ".tmp-1234567"), torn, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(objects, kindAgent, "notes.json"), torn, 0o600); err != nil {
		t.Fatal(err)
	}
	again := open(t, root, nil)
	listed, _, err := again.ListAgents(t.Context(), store.AgentList{})
	if err != nil || len(listed) != 1 || listed[0].ID != id || listed[0].Latest != 1 {
		t.Fatalf("agents after a torn write %+v, %v", listed, err)
	}
	if _, err := again.Version(t.Context(), id, 1); err != nil {
		t.Fatal(err)
	}
	storetest.Apply(t, again, "alice", "reviewer", "two")
	if a, err := again.Agent(t.Context(), "reviewer"); err != nil || a.Latest != 2 {
		t.Fatalf("a write after a torn one: %+v, %v", a, err)
	}
}

// TestAVersionIsVisibleOnlyOnceItsAgentCountsIt stops each PutVersion
// between the version's file and the agent's, as a crash there would,
// and reopens the directory.
func TestAVersionIsVisibleOnlyOnceItsAgentCountsIt(t *testing.T) {
	root := t.TempDir()
	st := open(t, root, nil)
	id := storetest.Apply(t, st, "alice", "reviewer", "one").Agent.Status.ID
	v1, err := st.Version(t.Context(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	injected := failWrites(t, filepath.Join("objects", kindAgent)+string(filepath.Separator))
	a, err := st.Agent(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	v2 := v1
	v2.Version, v2.Digest = 2, "sha256:second"
	if err := st.PutVersion(t.Context(), a, v2); !errors.Is(err, injected) {
		t.Fatalf("a crash before the agent's write: %v", err)
	}
	fresh := store.Agent{ID: session.NewID(session.PrefixAgent), Name: "builder", Owner: "bob"}
	first := store.AgentVersion{AgentID: fresh.ID, Version: 1, Digest: v1.Digest, Doc: v1.Doc, Bundle: v1.Bundle}
	if err := st.PutVersion(t.Context(), fresh, first); !errors.Is(err, injected) {
		t.Fatalf("a crash before a new agent's write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "objects", kindVersion, id, "2.json")); err != nil {
		t.Fatalf("the version's file was not written first: %v", err)
	}
	writeFile = atomicfile.WriteSync

	for name, s := range map[string]*Store{"the same store": st, "a reopened store": open(t, root, nil)} {
		t.Run(name, func(t *testing.T) {
			if a, err := s.Agent(t.Context(), id); err != nil || a.Latest != 1 {
				t.Fatalf("the agent counts %+v, %v", a, err)
			}
			if _, err := s.Version(t.Context(), id, 2); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("a version no agent counts: %v", err)
			}
			if vs, _, err := s.Versions(t.Context(), id, 0, ""); err != nil || len(vs) != 1 {
				t.Fatalf("versions %+v, %v", vs, err)
			}
			if _, err := s.Agent(t.Context(), "builder"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("an agent whose file was never written: %v", err)
			}
		})
	}
	again := open(t, root, nil)
	v2.Digest = "sha256:retried"
	if err := again.PutVersion(t.Context(), a, v2); err != nil {
		t.Fatal(err)
	}
	if got, err := again.Version(t.Context(), id, 2); err != nil || got.Digest != "sha256:retried" {
		t.Fatalf("the retried version %+v, %v", got, err)
	}
	if err := again.PutVersion(t.Context(), fresh, first); err != nil {
		t.Fatalf("the retried new agent: %v", err)
	}
}

func TestWriteFailuresLeaveTheStoreAsItWas(t *testing.T) {
	clock := storetest.NewClock()
	st := open(t, t.TempDir(), clock.Now)
	id := storetest.Apply(t, st, "alice", "reviewer", "one").Agent.Status.ID
	r := store.Idempotency{Subject: "alice", Key: "k", Route: "POST /v1/sessions", BodyHash: "h", ExpiresAt: clock.Now().Add(time.Hour)}
	if _, _, err := st.Begin(t.Context(), r); err != nil {
		t.Fatal(err)
	}

	injected := failWrites(t, "objects")
	if err := st.Archive(t.Context(), id, clock.Now()); !errors.Is(err, injected) {
		t.Fatalf("archive: %v", err)
	}
	if a, err := st.Agent(t.Context(), id); err != nil || a.ArchivedAt != nil {
		t.Fatalf("a failed archive archived %+v, %v", a, err)
	}
	if err := st.Finish(t.Context(), r); !errors.Is(err, injected) {
		t.Fatalf("finish: %v", err)
	}
	other := r
	other.Key = "k2"
	if _, _, err := st.Begin(t.Context(), other); !errors.Is(err, injected) {
		t.Fatalf("begin: %v", err)
	}
	v, err := st.Version(t.Context(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.Agent(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	v.Version = 2
	if err := st.PutVersion(t.Context(), a, v); !errors.Is(err, injected) {
		t.Fatalf("a version: %v", err)
	}
	writeFile = atomicfile.WriteSync
	if a, err := st.Agent(t.Context(), id); err != nil || a.Latest != 1 {
		t.Fatalf("a failed version counted %+v, %v", a, err)
	}
	if err := st.Abandon(t.Context(), "nobody", "none"); err != nil {
		t.Fatalf("abandoning no record: %v", err)
	}
	if held, fresh, err := st.Begin(t.Context(), r); err != nil || fresh || held.Done {
		t.Fatalf("a failed finish answered %+v, %v, %v", held, fresh, err)
	}
	if _, fresh, err := st.Begin(t.Context(), other); err != nil || !fresh {
		t.Fatalf("a failed begin reserved the key: %v, %v", fresh, err)
	}
}

func TestOpenRefusesACorruptAgent(t *testing.T) {
	id := session.NewID(session.PrefixAgent)
	for name, body := range map[string]string{
		"undecodable": `{"id":`,
		"another id":  `{"id":"` + session.NewID(session.PrefixAgent) + `","name":"a","owner":"o","latest_version":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			open(t, root, nil)
			if err := os.WriteFile(filepath.Join(root, "objects", kindAgent, id+".json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(root, nil); !errors.Is(err, session.ErrCorrupt) {
				t.Fatalf("Open: %v", err)
			}
		})
	}
	t.Run("one name twice", func(t *testing.T) {
		root := t.TempDir()
		open(t, root, nil)
		for range 2 {
			id := session.NewID(session.PrefixAgent)
			body := `{"id":"` + id + `","name":"twice","owner":"o","latest_version":1}`
			if err := os.WriteFile(filepath.Join(root, "objects", kindAgent, id+".json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := Open(root, nil); !errors.Is(err, session.ErrCorrupt) {
			t.Fatalf("Open: %v", err)
		}
	})
	t.Run("a directory in an agent's place", func(t *testing.T) {
		root := t.TempDir()
		open(t, root, nil)
		if err := os.Mkdir(filepath.Join(root, "objects", kindAgent, id+".json"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(root, nil); err == nil {
			t.Fatal("Open read a directory as an agent")
		}
	})
}

func TestOpenReturnsWhatTheDirectoryRefuses(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file, nil); err == nil {
		t.Fatal("Open under a file")
	}
	if err := syncDir(filepath.Join(file, "missing")); err == nil {
		t.Fatal("syncDir of no directory")
	}
}

func TestACorruptVersionIsRefused(t *testing.T) {
	root := t.TempDir()
	st := open(t, root, nil)
	id := storetest.Apply(t, st, "alice", "reviewer", "one").Agent.Status.ID
	storetest.Apply(t, st, "alice", "reviewer", "two")
	path := func(n string) string { return filepath.Join(root, "objects", kindVersion, id, n+".json") }
	one, err := os.ReadFile(path("1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path("2"), one, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Version(t.Context(), id, 2); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("a file holding another version: %v", err)
	}
	if _, _, err := st.Versions(t.Context(), id, 0, ""); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("a listing over a file holding another version: %v", err)
	}
	if err := os.WriteFile(path("2"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Version(t.Context(), id, 2); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("an undecodable version: %v", err)
	}
	if err := os.Remove(path("2")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Version(t.Context(), id, 2); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("a counted version with no file: %v", err)
	}
	if err := os.Mkdir(path("2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Version(t.Context(), id, 2); err == nil || errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("an unreadable version: %v", err)
	}
}

func TestACorruptIdempotencyRecordIsRefused(t *testing.T) {
	root := t.TempDir()
	st := open(t, root, nil)
	r := store.Idempotency{Subject: "alice", Key: "k", ExpiresAt: time.Now().Add(time.Hour)}
	path := st.idempotencyPath(r.Subject, r.Key)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Begin(t.Context(), r); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("an undecodable record: %v", err)
	}
	if err := st.Finish(t.Context(), r); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("finishing an undecodable record: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"subject":"bob","key":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Begin(t.Context(), r); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("a record of another subject: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "held"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := st.Abandon(t.Context(), r.Subject, r.Key); err == nil {
		t.Fatal("abandoning a record that cannot be removed")
	}
}

func TestIdempotencyFilesAreNamedByTheirPair(t *testing.T) {
	st := open(t, t.TempDir(), nil)
	if st.idempotencyPath("ab", "c") == st.idempotencyPath("a", "bc") {
		t.Fatal("two pairs share a file")
	}
	if name := filepath.Base(st.idempotencyPath("../x", "/y")); strings.ContainsAny(strings.TrimSuffix(name, ".json"), "./") {
		t.Fatalf("a record's file name %q carries its input", name)
	}
}

func TestAVersionDirectoryThatCannotBeMadeIsReturned(t *testing.T) {
	root := t.TempDir()
	st := open(t, root, nil)
	v := storetest.Apply(t, st, "alice", "reviewer", "one")
	stored, err := st.Version(t.Context(), v.Agent.Status.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	a := store.Agent{ID: session.NewID(session.PrefixAgent), Name: "builder", Owner: "bob"}
	if err := os.WriteFile(filepath.Join(root, "objects", kindVersion, a.ID), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.PutVersion(t.Context(), a, store.AgentVersion{AgentID: a.ID, Version: 1, Digest: stored.Digest, Doc: stored.Doc, Bundle: stored.Bundle}); err == nil {
		t.Fatal("a version written where its directory cannot be")
	}
	if _, err := st.Agent(t.Context(), "builder"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the agent of a failed write: %v", err)
	}
}
