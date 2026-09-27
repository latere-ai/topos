// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

// TestPostgresTwoReplicasOneWriter: two replicas appending to one session
// after the same sequence each round keep the log dense, and the one
// that did not write gets sequence_conflict.
func TestPostgresTwoReplicasOneWriter(t *testing.T) {
	ctx := t.Context()
	a := fresh(t, Options{})
	b, err := Open(ctx, a.listen.ConnString(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	s := storetest.NewSession()
	if err := a.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	const rounds = 20
	for round := range rounds {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, st := range []*Store{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				evs := []session.Event{message(t, fmt.Sprintf("round %d from replica %d", round, i))}
				session.Stamp(s.ID, uint64(round), evs)
				_, errs[i] = st.Append(ctx, s.ID, uint64(round), evs)
			}()
		}
		wg.Wait()
		won := 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, session.ErrSequenceConflict):
				t.Fatalf("round %d: %v", round, err)
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d replicas wrote, errors %v", round, won, errs)
		}
	}
	evs, err := b.Events(ctx, s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.CheckSequence(evs, 1); err != nil || len(evs) != rounds {
		t.Fatalf("%d events, %v", len(evs), err)
	}
}

// TestPostgresWatchAcrossReplicas: a watch sees an event another replica
// wrote with no notification at all, within the poll interval, the
// fallback for a notification a dropped listener lost.
func TestPostgresWatchAcrossReplicas(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	a := fresh(t, Options{Poll: 200 * time.Millisecond})
	s := storetest.NewSession()
	if err := a.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	ch, err := a.Watch(ctx, s.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The other replica's write, made the way Append makes it but with
	// its NOTIFY lost.
	e := message(t, "no notification")
	evs := []session.Event{e}
	session.Stamp(s.ID, 0, evs)
	e = evs[0]
	conn, err := pgx.Connect(ctx, a.listen.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(context.WithoutCancel(ctx)); err != nil {
			t.Error(err)
		}
	}()
	if _, err := conn.Exec(ctx, `INSERT INTO events (session_id, seq, id, type, time, thread, turn, step, payload, redacted) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, false)`,
		e.SessionID, int64(e.Seq), e.ID, string(e.Type), e.Time, e.Thread, e.Turn, e.Step, string(e.Payload)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE sessions SET last_seq = 1 WHERE id = $1`, s.ID); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	select {
	case got := <-ch:
		if got.ID != e.ID {
			t.Fatalf("watched %s", got.ID)
		}
		if waited := time.Since(start); waited > 2*time.Second {
			t.Fatalf("the watch took %s, past the 200 ms poll", waited)
		}
	case <-ctx.Done():
		t.Fatal("the watch never saw an event that came with no notification")
	}
}

// TestMigrationsAtStart: the store applies its migrations when it opens,
// opens again on its own schema, and refuses a database whose schema is
// newer than the migrations it knows.
func TestMigrationsAtStart(t *testing.T) {
	ctx := t.Context()
	a := fresh(t, Options{})
	dsn := a.listen.ConnString()
	again, err := Open(ctx, dsn, Options{})
	if err != nil {
		t.Fatalf("a second open on its own schema: %v", err)
	}
	again.Close()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE schema_migrations SET version = 9999`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, dsn, Options{})
	if err == nil {
		st.Close()
		t.Fatal("a store opened a database whose schema is newer than it knows")
	}
	if !strings.Contains(err.Error(), "postgres: migrate:") {
		t.Fatalf("refused for another reason: %v", err)
	}
}
