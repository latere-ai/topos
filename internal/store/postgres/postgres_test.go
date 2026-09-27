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
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

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
