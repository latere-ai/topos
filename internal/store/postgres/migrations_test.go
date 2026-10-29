// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/pkg/pgxmigrate"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
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

// TestOwnerTypeMigratesATableThatHoldsAgents: a database at migration
// 0005 holds agents with no owner type; after the migration each reads
// as a person's, with its row and its version kept, an organization's
// agent is stored beside them, and a type that is neither is refused by
// the table itself.
func TestOwnerTypeMigratesATableThatHoldsAgents(t *testing.T) {
	dsn := database(t)
	migrateTo(t, dsn, "0005")
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
	created := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	held := session.NewID(session.PrefixAgent)
	if _, err := conn.Exec(t.Context(), `INSERT INTO agents (id, name, owner, latest_version, created_at) VALUES ($1, 'reviewer', 'alice', 1, $2)`, held, created); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO agent_versions (agent_id, version, digest, doc, bundle, created_by, created_at) VALUES ($1, 1, 'sha256:one', '{}', '{}', 'alice', $2)`, held, created); err != nil {
		t.Fatal(err)
	}

	st, err := Open(t.Context(), dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	a, err := st.AgentByName(t.Context(), "alice", "reviewer")
	if err != nil || a.ID != held || a.OwnerType != store.OwnerUser || a.Latest != 1 {
		t.Fatalf("alice's reviewer after the migration: %+v, %v", a, err)
	}
	if v, err := st.Version(t.Context(), held, 1); err != nil || v.Digest != "sha256:one" {
		t.Fatalf("its version after the migration: %+v, %v", v, err)
	}
	ours := store.Agent{ID: session.NewID(session.PrefixAgent), Name: "reviewer", Owner: "https://issuer.test|org_1", OwnerType: store.OwnerOrganization, CreatedAt: created}
	if err := st.PutVersion(t.Context(), ours, store.AgentVersion{AgentID: ours.ID, Version: 1, Digest: "sha256:two", Doc: []byte(`{}`), Bundle: []byte(`{}`), CreatedBy: "alice", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Agent(t.Context(), ours.ID); err != nil || got.OwnerType != store.OwnerOrganization {
		t.Fatalf("the organization's reviewer: %+v, %v", got, err)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE agents SET owner_type = 'team' WHERE id = $1`, held); err == nil {
		t.Fatal("the table took an owner type that is neither a person nor an organization")
	}
}

// TestMigrationBackfillsTheTree: a database at migration 0007 holds a
// session, its fork and that fork's fork, and two forks below a session
// deleted before the migration. After it each fork reads its tree's
// root, the deleted session's id where the walk up ends at it, from its
// body and its columns; a body an earlier release rewrote without root
// still reads it, and a list by root finds the tree.
func TestMigrationBackfillsTheTree(t *testing.T) {
	dsn := database(t)
	migrateTo(t, dsn, "0007")
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
	gone := session.NewID(session.PrefixSession)
	sessions := map[string]session.Session{}
	for _, c := range []struct{ name, parent string }{{"a", ""}, {"b", "a"}, {"c", "b"}, {"d", "gone"}, {"e", "d"}} {
		s := storetest.NewSession()
		if c.parent == "gone" {
			s.Parent = &session.Parent{SessionID: gone, Seq: 3}
		} else if c.parent != "" {
			s.Parent = &session.Parent{SessionID: sessions[c.parent].ID, Seq: 4}
		}
		body, err := encode(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(t.Context(), `INSERT INTO sessions (id, agent_id, agent_version, owner, runner, status, turn, last_seq, created_at, updated_at, expires_at, body)
			VALUES ($1, $2, 1, $3, $4, $5, 0, 0, $6, $6, $7, $8)`, s.ID, s.Agent.ID, s.Initiator.Subject, s.Runner, string(s.Status), s.CreatedAt, s.ExpiresAt, body); err != nil {
			t.Fatal(err)
		}
		sessions[c.name] = s
	}

	st, err := Open(t.Context(), dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	a := sessions["a"].ID
	for name, want := range map[string]string{"a": "", "b": a, "c": a, "d": gone, "e": gone} {
		id := sessions[name].ID
		got, err := st.Get(t.Context(), id)
		if err != nil || got.Root != want {
			t.Fatalf("%s reads root %q, want %q: %v", name, got.Root, want, err)
		}
		var body string
		var parentID, rootID *string
		var parentSeq *int64
		if err := conn.QueryRow(t.Context(), `SELECT body, parent_id, parent_seq, root_id FROM sessions WHERE id = $1`, id).Scan(&body, &parentID, &parentSeq, &rootID); err != nil {
			t.Fatal(err)
		}
		var stored session.Session
		if err := json.Unmarshal([]byte(body), &stored); err != nil {
			t.Fatal(err)
		}
		p := sessions[name].Parent
		switch {
		case stored.Root != want:
			t.Fatalf("%s's body holds root %q, want %q", name, stored.Root, want)
		case p == nil && (parentID != nil || parentSeq != nil || rootID != nil):
			t.Fatalf("%s, no fork, has tree columns %v %v %v", name, parentID, parentSeq, rootID)
		case p != nil && (parentID == nil || *parentID != p.SessionID || parentSeq == nil || *parentSeq != int64(p.Seq) || rootID == nil || *rootID != want):
			t.Fatalf("%s's tree columns %v %v %v, want %s %d %s", name, parentID, parentSeq, rootID, p.SessionID, p.Seq, want)
		}
	}
	// A replica of the earlier release rewrites a fork's header without
	// root; the column fills it on a read, and an append keeps it.
	b := sessions["b"].ID
	if _, err := conn.Exec(t.Context(), `UPDATE sessions SET body = (body::jsonb - 'root')::text WHERE id = $1`, b); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Get(t.Context(), b); err != nil || got.Root != a {
		t.Fatalf("b rewritten without root reads %q, %v", got.Root, err)
	}
	again := []session.Event{storetest.Message(t, "Again.", time.Now())}
	session.Stamp(b, 0, again)
	if _, err := st.Append(t.Context(), b, 0, again); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Get(t.Context(), b); err != nil || got.Root != a || got.LastSeq != 1 {
		t.Fatalf("b after an append: root %q at %d, %v", got.Root, got.LastSeq, err)
	}
	page, _, err := st.List(t.Context(), session.ListOptions{Root: a})
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, s := range page {
		listed = append(listed, s.ID)
	}
	if want := []string{sessions["c"].ID, b, a}; !slices.Equal(listed, want) {
		t.Fatalf("the tree of a lists %v, want %v", listed, want)
	}
}

// metadataRows is the session_metadata rows of a session as a map.
func metadataRows(t *testing.T, conn *pgx.Conn, id string) map[string]string {
	t.Helper()
	rows, err := conn.Query(t.Context(), `SELECT key, value FROM session_metadata WHERE session_id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	type entry struct{ Key, Value string }
	read, err := pgx.CollectRows(rows, pgx.RowToStructByPos[entry])
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	for _, e := range read {
		if out == nil {
			out = map[string]string{}
		}
		out[e.Key] = e.Value
	}
	return out
}

// TestMetadataTableFollowsTheBody: migration 0009 copies the metadata of
// every header into session_metadata, and a create, a fork's copy and a
// change keep the table equal to the body, so a list filtered by an
// entry finds each session the body labels (spec 057).
func TestMetadataTableFollowsTheBody(t *testing.T) {
	dsn := database(t)
	migrateTo(t, dsn, "0008")
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
	before := map[string]session.Session{}
	for name, meta := range map[string]map[string]string{"labeled": {"project": "prj_1", "pinned": "yes"}, "bare": nil} {
		s := storetest.NewSession()
		s.Metadata = meta
		body, err := encode(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(t.Context(), `INSERT INTO sessions (id, agent_id, agent_version, owner, runner, status, turn, last_seq, created_at, updated_at, expires_at, body)
			VALUES ($1, $2, 1, $3, $4, $5, 0, 0, $6, $6, $7, $8)`, s.ID, s.Agent.ID, s.Initiator.Subject, s.Runner, string(s.Status), s.CreatedAt, s.ExpiresAt, body); err != nil {
			t.Fatal(err)
		}
		before[name] = s
	}
	st, err := Open(t.Context(), dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	follows := func(id string) {
		t.Helper()
		got, err := st.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rows := metadataRows(t, conn, id); !maps.Equal(rows, got.Metadata) {
			t.Fatalf("%s: rows %v, body %v", id, rows, got.Metadata)
		}
	}
	follows(before["labeled"].ID)
	follows(before["bare"].ID)
	if rows := metadataRows(t, conn, before["labeled"].ID); len(rows) != 2 {
		t.Fatalf("the migration copied %v", rows)
	}

	made := storetest.NewSession()
	made.Metadata = map[string]string{"project": "prj_1"}
	if err := st.Create(t.Context(), made, nil); err != nil {
		t.Fatal(err)
	}
	follows(made.ID)
	child := storetest.NewSession()
	child.Metadata = made.Metadata
	if _, err := session.Fork(t.Context(), st, child, nil, made, nil); err != nil {
		t.Fatal(err)
	}
	follows(child.ID)
	if _, err := st.SetMetadata(t.Context(), made.ID, map[string]*string{"project": new("prj_2"), "pinned": new("yes")}); err != nil {
		t.Fatal(err)
	}
	follows(made.ID)
	if _, err := st.SetMetadata(t.Context(), made.ID, map[string]*string{"project": nil, "pinned": nil}); err != nil {
		t.Fatal(err)
	}
	follows(made.ID)
	page, _, err := st.List(t.Context(), session.ListOptions{Metadata: &session.MetadataEntry{Key: "project", Value: "prj_1"}})
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, s := range page {
		listed = append(listed, s.ID)
	}
	if want := []string{child.ID, before["labeled"].ID}; !slices.Equal(listed, want) {
		t.Fatalf("listed by project prj_1: %v, want %v", listed, want)
	}
	// A deleted session leaves no row behind.
	if err := st.Delete(t.Context(), child.ID); err != nil {
		t.Fatal(err)
	}
	if rows := metadataRows(t, conn, child.ID); rows != nil {
		t.Fatalf("a deleted session left %v", rows)
	}
}

// TestTriggerColumnFollowsTheBody: a database at migration 0009 holding a
// session a trigger started and one no trigger started migrates with the
// trigger column read from each body, and a list by the trigger reads it;
// a session a replica of the earlier release inserts afterwards without
// the column gains it when a later batch saves its header; a create
// writes it; and a session no trigger started keeps none (spec 072).
func TestTriggerColumnFollowsTheBody(t *testing.T) {
	dsn := database(t)
	migrateTo(t, dsn, "0009")
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
	trigger := session.NewID(session.PrefixTrigger)
	// insert writes s's row as a replica before the trigger column does.
	insert := func(s session.Session) {
		t.Helper()
		body, err := encode(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(t.Context(), `INSERT INTO sessions (id, agent_id, agent_version, owner, runner, status, turn, last_seq, created_at, updated_at, expires_at, body)
			VALUES ($1, $2, 1, $3, $4, $5, 0, 0, $6, $6, $7, $8)`, s.ID, s.Agent.ID, s.Initiator.Subject, s.Runner, string(s.Status), s.CreatedAt, s.ExpiresAt, body); err != nil {
			t.Fatal(err)
		}
	}
	fired, plain := storetest.NewSession(), storetest.NewSession()
	fired.TriggerID = trigger
	insert(fired)
	insert(plain)
	st, err := Open(t.Context(), dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	column := func(id string) *string {
		t.Helper()
		var v *string
		if err := conn.QueryRow(t.Context(), `SELECT trigger_id FROM sessions WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := column(fired.ID); got == nil || *got != trigger {
		t.Fatalf("the migration filled the trigger column with %v, want %s", got, trigger)
	}
	if got := column(plain.ID); got != nil {
		t.Fatalf("a session no trigger started has the trigger %q", *got)
	}

	late := storetest.NewSession()
	late.TriggerID = trigger
	insert(late)
	if got := column(late.ID); got != nil {
		t.Fatalf("a row written without the column has %q", *got)
	}
	run := []session.Event{storetest.Message(t, "Run.", time.Now())}
	session.Stamp(late.ID, 0, run)
	if _, err := st.Append(t.Context(), late.ID, 0, run); err != nil {
		t.Fatal(err)
	}
	if got := column(late.ID); got == nil || *got != trigger {
		t.Fatalf("a saved header filled the trigger column with %v, want %s", got, trigger)
	}
	made := storetest.NewSession()
	made.TriggerID = trigger
	if err := st.Create(t.Context(), made, nil); err != nil {
		t.Fatal(err)
	}
	page, _, err := st.List(t.Context(), session.ListOptions{Trigger: trigger})
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, s := range page {
		listed = append(listed, s.ID)
	}
	if want := []string{made.ID, late.ID, fired.ID}; !slices.Equal(listed, want) {
		t.Fatalf("listed by the trigger: %v, want %v", listed, want)
	}
}

// TestAFiringBeforeTheKeyColumnNamesNoKey: a database at migration 0011
// holding a held firing migrates with the firing kept and no key that
// fired it, and a firing a replica of the earlier release inserts
// afterwards, naming no fired_by column, reads back with none too, while
// a claim of this release keeps its key (spec 078).
func TestAFiringBeforeTheKeyColumnNamesNoKey(t *testing.T) {
	dsn := database(t)
	migrateTo(t, dsn, "0011")
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
	now := time.Now().UTC()
	tr := session.NewID(session.PrefixTrigger)
	if _, err := conn.Exec(t.Context(), `INSERT INTO triggers (id, name, owner, agent_id, version, digest, doc, created_at, updated_at)
		VALUES ($1, 't', 'alice', 'agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C', 1, 'sha256:1', '{}', $2, $2)`, tr, now); err != nil {
		t.Fatal(err)
	}
	// insert writes a held firing's row as a replica before the column does.
	insert := func(dedupe string) string {
		t.Helper()
		id := session.NewID(session.PrefixFiring)
		if _, err := conn.Exec(t.Context(), `INSERT INTO trigger_firings (id, trigger_id, dedupe, origin, envelope, key, outcome, reason, session_id, session_open, replica, fired_at, received_at)
			VALUES ($1, $2, $3, 'event', '', 'k', 'held', '', '', false, 'r1', $4, $4)`, id, tr, dedupe, now); err != nil {
			t.Fatal(err)
		}
		return id
	}
	before := insert("github:1")
	st, err := Open(t.Context(), dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	after := insert("github:2")
	key := `{"token_use":"pat"}`
	claimed, fresh, err := st.ClaimFiring(t.Context(), store.Firing{ID: session.NewID(session.PrefixFiring), TriggerID: tr, Dedupe: "github:3", Origin: store.OriginEvent,
		FiredBy: []byte(key), Replica: "r2", Time: now, ReceivedAt: now})
	if err != nil || !fresh || string(claimed.FiredBy) != key {
		t.Fatalf("a claim naming a key: %+v, %v, %v", claimed, fresh, err)
	}
	held, err := st.HeldFirings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 2 || held[0].ID != before || held[1].ID != after || len(held[0].FiredBy) != 0 || len(held[1].FiredBy) != 0 {
		t.Fatalf("the held firings written without the column: %+v", held)
	}
	page, _, err := st.Firings(t.Context(), tr, 10, "")
	if err != nil || len(page) != 3 || page[0].ID != claimed.ID || string(page[0].FiredBy) != key {
		t.Fatalf("the firings: %+v, %v", page, err)
	}
}

// TestSubagentEventsMigrateInPlace: a database at migration 0020 holding a
// session whose log has a subagent's events under their names before,
// thread.started, thread.message and thread.ended in a thread column,
// migrates to subagent.started, subagent.message and subagent.ended in a
// subagent column. The store reads the events back under the new names
// with the subagent each belongs to, the session's own events stay its
// own, the fold renders the subagent's conversation apart, and the partial
// indexes on the column name it subagent (spec 013).
func TestSubagentEventsMigrateInPlace(t *testing.T) {
	dsn := database(t)
	migrateTo(t, dsn, "0020")
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
	sess := storetest.NewSession()
	body, err := encode(sess)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), `INSERT INTO sessions (id, agent_id, agent_version, owner, runner, status, turn, last_seq, created_at, updated_at, expires_at, body)
		VALUES ($1, $2, 1, $3, $4, $5, 1, 5, $6, $6, $7, $8)`, sess.ID, sess.Agent.ID, sess.Initiator.Subject, sess.Runner, string(sess.Status), sess.CreatedAt, sess.ExpiresAt, body); err != nil {
		t.Fatal(err)
	}
	sub := session.NewID(session.PrefixEvent)
	at := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	rows := []struct {
		id, typ, thread, payload string
	}{
		{session.NewID(session.PrefixEvent), "user.message", "", `{"sender":{"subject":"usr_ada","kind":"person"},"content":[{"type":"text","text":"Review it."}]}`},
		{sub, "thread.started", sub, `{"agent":{"name":"reviewer","version":0},"task":"Review the diff.","depth":1}`},
		{session.NewID(session.PrefixEvent), "thread.message", sub, `{"to":"` + sub + `","from_name":"main","content":[{"type":"text","text":"Check line 9 too."}]}`},
		{session.NewID(session.PrefixEvent), "agent.message", sub, `{"message":{"role":"assistant","blocks":[{"type":"text","text":"One issue."}]},"stop_reason":"end_turn"}`},
		{session.NewID(session.PrefixEvent), "thread.ended", sub, `{"reason":"completed","final_text":"One issue."}`},
	}
	for i, r := range rows {
		if _, err := conn.Exec(t.Context(), `INSERT INTO events (session_id, seq, id, type, time, thread, turn, step, payload, redacted) VALUES ($1, $2, $3, $4, $5, $6, 1, 0, $7, false)`,
			sess.ID, i+1, r.id, r.typ, at, r.thread, r.payload); err != nil {
			t.Fatal(err)
		}
	}

	st, err := Open(t.Context(), dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	evs, err := st.Events(t.Context(), sess.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		typ      session.Type
		subagent string
	}{
		{session.TypeUserMessage, ""},
		{session.TypeSubagentStarted, sub},
		{session.TypeSubagentMessage, sub},
		{session.TypeAgentMessage, sub},
		{session.TypeSubagentEnded, sub},
	}
	if len(evs) != len(want) {
		t.Fatalf("the migrated log holds %d events, want %d", len(evs), len(want))
	}
	for i, w := range want {
		if evs[i].Type != w.typ || evs[i].Subagent != w.subagent {
			t.Fatalf("event %d migrated to %s in %q, want %s in %q", i+1, evs[i].Type, evs[i].Subagent, w.typ, w.subagent)
		}
		b, err := json.Marshal(evs[i])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"thread`) {
			t.Fatalf("event %d still says thread: %s", i+1, b)
		}
	}
	own, err := session.Fold(evs, "")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := session.Fold(evs, sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Unknown)+len(theirs.Unknown) > 0 {
		t.Fatalf("the fold reports types it does not know: %v, %v", own.Unknown, theirs.Unknown)
	}
	if len(own.Messages) != 1 || len(theirs.Messages) < 2 {
		t.Fatalf("the fold of the session's own conversation has %d messages and the subagent's %d", len(own.Messages), len(theirs.Messages))
	}

	var stale int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE type LIKE 'thread.%'`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("%d events keep a type of before: %v", stale, err)
	}
	idx, err := conn.Query(t.Context(), `SELECT indexdef FROM pg_indexes WHERE tablename = 'events' AND indexname IN ('events_unsearched', 'events_activity') ORDER BY indexname`)
	if err != nil {
		t.Fatal(err)
	}
	defs, err := pgx.CollectRows(idx, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 {
		t.Fatalf("the partial indexes after the migration: %v", defs)
	}
	for _, d := range defs {
		if !strings.Contains(d, "subagent = ''") || strings.Contains(d, "thread") {
			t.Fatalf("an index after the migration: %s", d)
		}
	}
}
