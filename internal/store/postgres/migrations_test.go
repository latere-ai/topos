// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/pkg/pgxmigrate"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/session"
)

// migrateTo applies the embedded migrations up to and including version
// last, as a replica of that release would have left the database.
func migrateTo(t *testing.T, dsn, last string) {
	t.Helper()
	upTo := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if version, _, _ := strings.Cut(e.Name(), "_"); version > last {
			continue
		}
		b, err := migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		upTo["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	if len(upTo) == len(entries) {
		t.Fatalf("migration %s is the latest, so nothing is left to apply after it", last)
	}
	if err := pgxmigrate.Up(migrationDSN(dsn), upTo, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// constraints lists the unique constraints on agents.
func constraints(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(t.Context(), `SELECT conname FROM pg_constraint WHERE conrelid = 'agents'::regclass AND contype = 'u' ORDER BY conname`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// TestNamesPerOwnerMigrateATableThatHoldsAgents: a database at migration
// 0002, whose agent names are unique across the installation and which
// holds agents of two owners, migrates to names unique per owner with
// every row and version kept. Afterwards a second owner may take a name
// the first holds, and the first still may not hold it twice.
func TestNamesPerOwnerMigrateATableThatHoldsAgents(t *testing.T) {
	dsn := database(t)
	migrateTo(t, dsn, "0002")
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
	created := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	insert := func(owner, name string) (string, error) {
		id := session.NewID(session.PrefixAgent)
		_, err := conn.Exec(t.Context(), `INSERT INTO agents (id, name, owner, latest_version, created_at) VALUES ($1, $2, $3, 1, $4)`, id, name, owner, created)
		return id, err
	}
	reviewer, err := insert("alice", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	builder, err := insert("bob", "builder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO agent_versions (agent_id, version, digest, doc, bundle, created_by, created_at) VALUES ($1, 1, 'sha256:one', '{}', '{}', 'alice', $2)`, reviewer, created); err != nil {
		t.Fatal(err)
	}
	if _, err := insert("bob", "reviewer"); !isUnique(err) {
		t.Fatalf("migration 0002 let a second owner take a held name: %v", err)
	}
	if got := constraints(t, conn); !slices.Equal(got, []string{"agents_name_key"}) {
		t.Fatalf("the unique constraints before the migration: %v", got)
	}

	st, err := Open(t.Context(), dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if got := constraints(t, conn); !slices.Equal(got, []string{"agents_owner_name_key"}) {
		t.Fatalf("the unique constraints after the migration: %v", got)
	}
	all, _, err := st.ListAgents(t.Context(), store.AgentList{})
	if err != nil || len(all) != 2 {
		t.Fatalf("the agents after the migration: %+v, %v", all, err)
	}
	for _, r := range []struct{ owner, name, id string }{{"alice", "reviewer", reviewer}, {"bob", "builder", builder}} {
		a, err := st.AgentByName(t.Context(), r.owner, r.name)
		if err != nil || a.ID != r.id || a.Owner != r.owner || a.Latest != 1 || !a.CreatedAt.Equal(created) {
			t.Fatalf("%s's %s after the migration: %+v, %v", r.owner, r.name, a, err)
		}
	}
	v, err := st.Version(t.Context(), reviewer, 1)
	if err != nil || v.Digest != "sha256:one" {
		t.Fatalf("alice's version after the migration: %+v, %v", v, err)
	}
	if _, err := st.AgentByName(t.Context(), "bob", "reviewer"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("alice's name read as bob's: %v", err)
	}
	if _, err := insert("bob", "reviewer"); err != nil {
		t.Fatalf("bob takes a name alice holds: %v", err)
	}
	if _, err := insert("alice", "reviewer"); !isUnique(err) {
		t.Fatalf("alice holds one name twice: %v", err)
	}
}
