// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/internal/store"
	objstoretest "latere.ai/x/topos/internal/store/storetest"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

// admin is the DSN of a server the tests create databases on: the
// DATABASE_URL a CI job provides, or a container this process starts.
var admin string

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres tier:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		admin = v
		return m.Run(), nil
	}
	engine, err := containerEngine()
	if err != nil {
		return 0, err
	}
	out, err := exec.Command(engine, "run", "-d", "--rm", "-p", "127.0.0.1::5432", "-e", "POSTGRES_PASSWORD=topos", "-e", "POSTGRES_DB=topos", "postgres:17-alpine").Output()
	if err != nil {
		return 0, fmt.Errorf("start postgres: %w", err)
	}
	id := strings.TrimSpace(string(out))
	defer func() {
		if err := exec.Command(engine, "rm", "-f", id).Run(); err != nil {
			fmt.Fprintln(os.Stderr, "postgres tier: remove the container:", err)
		}
	}()
	port, err := exec.Command(engine, "port", id, "5432").Output()
	if err != nil {
		return 0, fmt.Errorf("find the port: %w", err)
	}
	_, p, err := net.SplitHostPort(strings.TrimSpace(strings.Split(string(port), "\n")[0]))
	if err != nil {
		return 0, fmt.Errorf("read the port %q: %w", port, err)
	}
	admin = "postgres://postgres:topos@127.0.0.1:" + p + "/topos?sslmode=disable"
	if err := ready(admin); err != nil {
		return 0, err
	}
	return m.Run(), nil
}

func containerEngine() (string, error) {
	for _, e := range []string{"podman", "docker"} {
		if p, err := exec.LookPath(e); err == nil {
			return p, nil
		}
	}
	return "", errors.New("no DATABASE_URL and no podman or docker on PATH")
}

func ready(dsn string) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			err = conn.Ping(ctx)
			cerr := conn.Close(ctx)
			cancel()
			if err == nil && cerr == nil {
				return nil
			}
		} else {
			cancel()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres did not come up: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

var databases atomic.Int64

// fresh creates an empty database and opens a store on it.
func fresh(t *testing.T, o Options) *Store {
	t.Helper()
	name := fmt.Sprintf("t%d_%d", os.Getpid(), databases.Add(1))
	conn, err := pgx.Connect(t.Context(), admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	dsn := strings.Replace(admin, "/topos?", "/"+name+"?", 1)
	if !strings.Contains(dsn, "/"+name+"?") {
		t.Fatalf("cannot derive a database DSN from %q", admin)
	}
	st, err := Open(t.Context(), dsn, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestPostgresStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) session.Store { return fresh(t, Options{Poll: 200 * time.Millisecond}) })
}

func TestPostgresObjectStoreConformance(t *testing.T) {
	objstoretest.Run(t, func(t *testing.T, now func() time.Time) store.Store { return fresh(t, Options{Now: now}) })
}

// replicas returns two stores on one fresh database, as two toposd
// replicas would hold.
func replicas(t *testing.T, o Options) [2]*Store {
	t.Helper()
	a := fresh(t, o)
	b, err := Open(t.Context(), a.listen.ConnString(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return [2]*Store{a, b}
}

// race runs fn n times at once, alternating the replicas, and returns
// each call's error.
func race(n int, rs [2]*Store, fn func(i int, st *Store) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			errs[i] = fn(i, rs[i%2])
		})
	}
	close(start)
	wg.Wait()
	return errs
}

func TestBeginReservesAKeyOnceAcrossReplicas(t *testing.T) {
	clock := objstoretest.NewClock()
	rs := replicas(t, Options{Now: clock.Now})
	for _, round := range []string{"a new key", "an expired key"} {
		var reserved atomic.Int64
		errs := race(16, rs, func(i int, st *Store) error {
			r := store.Idempotency{Subject: "alice", Key: "k", Route: "POST /v1/sessions", BodyHash: fmt.Sprint(i), ExpiresAt: clock.Now().Add(time.Hour)}
			got, fresh, err := st.Begin(t.Context(), r)
			if err != nil {
				return err
			}
			if fresh {
				reserved.Add(1)
			} else if got.Done || got.Route != r.Route {
				return fmt.Errorf("held %+v", got)
			}
			return nil
		})
		if err := errors.Join(errs...); err != nil {
			t.Fatalf("%s: %v", round, err)
		}
		if n := reserved.Load(); n != 1 {
			t.Fatalf("%s was reserved %d times", round, n)
		}
		clock.Advance(2 * time.Hour)
	}
}

func TestPutVersionHasOneWinnerAcrossReplicas(t *testing.T) {
	rs := replicas(t, Options{})
	r := objstoretest.Apply(t, rs[0], "alice", "reviewer", "one")
	v, err := rs[0].Version(t.Context(), r.Agent.Status.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	a, err := rs[1].Agent(t.Context(), "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	for _, round := range []struct {
		name string
		put  func(i int, st *Store) error
	}{
		{"a second version", func(i int, st *Store) error {
			next := v
			next.Version, next.Digest = 2, fmt.Sprintf("sha256:%d", i)
			return st.PutVersion(t.Context(), a, next)
		}},
		{"a new agent of one name", func(i int, st *Store) error {
			fresh := store.Agent{ID: session.NewID(session.PrefixAgent), Name: "builder", Owner: fmt.Sprint(i)}
			first := v
			first.AgentID = fresh.ID
			return st.PutVersion(t.Context(), fresh, first)
		}},
	} {
		won := 0
		for _, err := range race(8, rs, round.put) {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, store.ErrConflict):
				t.Fatalf("%s: %v", round.name, err)
			}
		}
		if won != 1 {
			t.Fatalf("%s was stored %d times", round.name, won)
		}
	}
	if got, err := rs[1].Agent(t.Context(), "reviewer"); err != nil || got.Latest != 2 {
		t.Fatalf("the agent after the race %+v, %v", got, err)
	}
}

func TestObjectCallsOnAClosedStoreReturnTheirErrors(t *testing.T) {
	st := fresh(t, Options{})
	r := objstoretest.Apply(t, st, "alice", "reviewer", "one")
	v, err := st.Version(t.Context(), r.Agent.Status.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.Agent(t.Context(), r.Agent.Status.ID)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	next := v
	next.Version = 2
	fresh := store.Agent{ID: session.NewID(session.PrefixAgent), Name: "builder", Owner: "bob"}
	first := v
	first.AgentID = fresh.ID
	rec := store.Idempotency{Subject: "alice", Key: "k"}
	for name, call := range map[string]func() error{
		"Agent":      func() error { _, err := st.Agent(t.Context(), "reviewer"); return err },
		"ListAgents": func() error { _, _, err := st.ListAgents(t.Context(), store.AgentList{}); return err },
		"Version":    func() error { _, err := st.Version(t.Context(), a.ID, 1); return err },
		"Versions":   func() error { _, _, err := st.Versions(t.Context(), a.ID, 0, ""); return err },
		"PutVersion": func() error { return st.PutVersion(t.Context(), a, next) },
		"create":     func() error { return st.PutVersion(t.Context(), fresh, first) },
		"Archive":    func() error { return st.Archive(t.Context(), a.ID, time.Now()) },
		"Begin":      func() error { _, _, err := st.Begin(t.Context(), rec); return err },
		"Finish":     func() error { return st.Finish(t.Context(), rec) },
		"Abandon":    func() error { return st.Abandon(t.Context(), "alice", "k") },
	} {
		if err := call(); err == nil || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			t.Errorf("%s on a closed store: %v", name, err)
		}
	}
}

func message(t *testing.T, text string) session.Event {
	t.Helper()
	e, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: session.Sender{Subject: "u", Kind: session.SenderPerson}, Content: []lux.Block{{Type: ir.BlockText, Text: text}}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestWatchSeesAnotherStoresAppends(t *testing.T) {
	a := fresh(t, Options{Poll: time.Hour})
	b, err := Open(t.Context(), a.listen.ConnString(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	s := storetest.NewSession()
	if err := a.Create(t.Context(), s, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ch, err := a.Watch(ctx, s.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	evs := []session.Event{message(t, "through LISTEN")}
	session.Stamp(s.ID, 0, evs)
	if _, err := b.Append(ctx, s.ID, 0, evs); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-ch:
		if e.ID != evs[0].ID {
			t.Fatalf("watched %s", e.ID)
		}
	case <-ctx.Done():
		t.Fatal("no notification reached the watcher; the poll is an hour away")
	}
}

func TestAnExpiredLeaseIsTakenOverAndTheOldHolderLosesIt(t *testing.T) {
	st := fresh(t, Options{LeaseTTL: 400 * time.Millisecond})
	s := storetest.NewSession()
	if err := st.Create(t.Context(), s, nil); err != nil {
		t.Fatal(err)
	}
	first, err := st.Acquire(t.Context(), s.ID, session.Holder{Runner: "run_a"})
	if err != nil {
		t.Fatal(err)
	}
	// Stop the first holder's renewals without releasing, as a runner that
	// died would.
	l := first.(*lease)
	close(l.stop)
	l.stop = make(chan struct{})
	time.Sleep(600 * time.Millisecond)
	second, err := st.Acquire(t.Context(), s.ID, session.Holder{Runner: "run_b"})
	if err != nil {
		t.Fatalf("taking over an expired lease: %v", err)
	}
	if err := first.Renew(t.Context()); !errors.Is(err, session.ErrLocked) {
		t.Fatalf("the old holder renewed: %v", err)
	}
	select {
	case <-first.Lost():
	default:
		t.Fatal("the old holder's Lost is open")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	var le *session.LockedError
	if err := st.Delete(t.Context(), s.ID); !errors.As(err, &le) || le.Holder.Runner != "run_b" {
		t.Fatalf("delete under the new holder: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
}

func TestACorruptBlobIsRefused(t *testing.T) {
	st := fresh(t, Options{})
	s := storetest.NewSession()
	if err := st.Create(t.Context(), s, nil); err != nil {
		t.Fatal(err)
	}
	d, err := st.PutBlob(t.Context(), s.ID, strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(t.Context(), `UPDATE blobs SET body = 'tampered' WHERE digest = $1`, string(d)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Blob(t.Context(), s.ID, d); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("a tampered blob: %v", err)
	}
}

func TestMigrationDSN(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://u:p@h/db":   "pgx5://u:p@h/db",
		"postgresql://u:p@h/db": "pgx5://u:p@h/db",
		"pgx5://u@h/db":         "pgx5://u@h/db",
	} {
		if got := migrationDSN(in); got != want {
			t.Fatalf("migrationDSN(%q) = %q", in, got)
		}
	}
}

// TestAFencedAppendAfterATakeoverIsRefused: once another holder takes an
// expired lease, an append through the old lease is refused and writes
// nothing, the new holder's goes through, and an append outside any
// lease, as the API makes one, still does.
func TestAFencedAppendAfterATakeoverIsRefused(t *testing.T) {
	st := fresh(t, Options{LeaseTTL: 400 * time.Millisecond})
	s := storetest.NewSession()
	if err := st.Create(t.Context(), s, nil); err != nil {
		t.Fatal(err)
	}
	first, err := st.Acquire(t.Context(), s.ID, session.Holder{Runner: "run_a"})
	if err != nil {
		t.Fatal(err)
	}
	old := first.(*lease)
	evs := []session.Event{message(t, "while the lease is held")}
	session.Stamp(s.ID, 0, evs)
	if _, err := old.Append(t.Context(), 0, evs); err != nil {
		t.Fatalf("an append under a live lease: %v", err)
	}
	close(old.stop)
	old.stop = make(chan struct{})
	time.Sleep(600 * time.Millisecond)
	second, err := st.Acquire(t.Context(), s.ID, session.Holder{Runner: "run_b"})
	if err != nil {
		t.Fatal(err)
	}
	stale := []session.Event{message(t, "from the runner that lost it")}
	session.Stamp(s.ID, 1, stale)
	if _, err := old.Append(t.Context(), 1, stale); !errors.Is(err, session.ErrLeaseLost) {
		t.Fatalf("an append through the lost lease: %v", err)
	}
	if got, err := st.Get(t.Context(), s.ID); err != nil || got.LastSeq != 1 {
		t.Fatalf("the refused append wrote: %+v, %v", got, err)
	}
	current := []session.Event{message(t, "from the new holder")}
	session.Stamp(s.ID, 1, current)
	if _, err := second.(session.Fence).Append(t.Context(), 1, current); err != nil {
		t.Fatalf("the new holder's append: %v", err)
	}
	api := []session.Event{message(t, "from a person")}
	session.Stamp(s.ID, 2, api)
	if _, err := st.Append(t.Context(), s.ID, 2, api); err != nil {
		t.Fatalf("an append outside any lease: %v", err)
	}
	if err := errors.Join(first.Release(), second.Release()); err != nil {
		t.Fatal(err)
	}
}
