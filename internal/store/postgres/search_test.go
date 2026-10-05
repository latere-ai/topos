// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

// searchIDs is the ids a search of st for q finds.
func searchIDs(t *testing.T, st *Store, q string) []string {
	t.Helper()
	query, err := session.ParseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	hits, _, err := st.Search(t.Context(), session.SearchOptions{Query: query})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.Session.ID)
	}
	return ids
}

// TestSearchIndexesWhatAnEarlierReleaseWrote writes a session and its
// log as a release before the search column did, then opens the store,
// which migrates and indexes them.
func TestSearchIndexesWhatAnEarlierReleaseWrote(t *testing.T) {
	ctx := t.Context()
	dsn := database(t)
	migrateTo(t, dsn, "0006")
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	sess := storetest.NewSession()
	body, err := encode(sess)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO sessions (id, agent_id, agent_version, owner, runner, status, created_at, updated_at, expires_at, body)
		VALUES ($1, $2, 1, $3, $4, $5, $6, $6, $6, $7)`,
		sess.ID, sess.Agent.ID, sess.Initiator.Subject, sess.Runner, string(sess.Status), sess.CreatedAt, body); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	evs := []session.Event{storetest.Message(t, "the old lighthouse keeper", now), storetest.Reply(t, "a reply about the lighthouse", now), storetest.Message(t, "a secret ferry", now)}
	session.Stamp(sess.ID, 0, evs)
	for i, e := range evs {
		if _, err := conn.Exec(ctx, `INSERT INTO events (session_id, seq, id, type, time, payload, redacted) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			e.SessionID, int64(e.Seq), e.ID, string(e.Type), e.Time, string(e.Payload), i == 2); err != nil {
			t.Fatal(err)
		}
	}

	st, err := Open(ctx, dsn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The store indexes at its open in the background; whatever that run
	// left is indexed here, and a second run finds nothing left.
	if _, err := st.IndexSearch(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := st.IndexSearch(ctx); err != nil || n != 0 {
		t.Fatalf("a second run indexed %d, %v", n, err)
	}
	if ids := searchIDs(t, st, "lighthouse keeper"); len(ids) != 1 || ids[0] != sess.ID {
		t.Fatalf("the earlier release's session is not found: %q", ids)
	}
	if ids := searchIDs(t, st, "ferry"); len(ids) != 0 {
		t.Fatalf("an event redacted before the column was indexed: %q", ids)
	}
	var unindexed bool
	if err := conn.QueryRow(ctx, `SELECT search IS NULL FROM events WHERE id = $1`, evs[2].ID).Scan(&unindexed); err != nil || !unindexed {
		t.Fatalf("a redacted event has an entry: %v, %v", unindexed, err)
	}
}

// TestSearchPassesOverARedactedEventsStaleEntry holds the search to the
// event's redaction when its entry is still there, as a replica of an
// earlier release leaves it.
func TestSearchPassesOverARedactedEventsStaleEntry(t *testing.T) {
	ctx := t.Context()
	st := fresh(t, Options{})
	sess := storetest.NewSession()
	if err := st.Create(ctx, sess, nil); err != nil {
		t.Fatal(err)
	}
	evs := []session.Event{storetest.Message(t, "the vault combination", time.Now())}
	session.Stamp(sess.ID, 0, evs)
	if _, err := st.Append(ctx, sess.ID, 0, evs); err != nil {
		t.Fatal(err)
	}
	e := evs[0]
	if ids := searchIDs(t, st, "vault"); len(ids) != 1 {
		t.Fatalf("an indexed message is not found: %q", ids)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE events SET payload = '{"tombstone":true}', redacted = true WHERE id = $1`, e.ID); err != nil {
		t.Fatal(err)
	}
	if ids := searchIDs(t, st, "vault"); len(ids) != 0 {
		t.Fatalf("a redacted event's stale entry was found: %q", ids)
	}
	if err := st.Redact(ctx, sess.ID, e.ID, session.Sender{Subject: "usr_1", Kind: session.SenderPerson}, "again"); err != nil {
		t.Fatal(err)
	}
}

// TestRedactClearsTheEntry holds a redaction to removing what the index
// keeps of the event.
func TestRedactClearsTheEntry(t *testing.T) {
	ctx := t.Context()
	st := fresh(t, Options{})
	sess := storetest.NewSession()
	if err := st.Create(ctx, sess, nil); err != nil {
		t.Fatal(err)
	}
	evs := []session.Event{storetest.Message(t, "a password", time.Now())}
	session.Stamp(sess.ID, 0, evs)
	if _, err := st.Append(ctx, sess.ID, 0, evs); err != nil {
		t.Fatal(err)
	}
	e := evs[0]
	var indexed bool
	if err := st.pool.QueryRow(ctx, `SELECT search IS NOT NULL FROM events WHERE id = $1`, e.ID).Scan(&indexed); err != nil || !indexed {
		t.Fatalf("an appended message has no entry: %v, %v", indexed, err)
	}
	if err := st.Redact(ctx, sess.ID, e.ID, session.Sender{Subject: "usr_1", Kind: session.SenderPerson}, "secret"); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT search IS NOT NULL FROM events WHERE id = $1`, e.ID).Scan(&indexed); err != nil || indexed {
		t.Fatalf("a redacted message keeps its entry: %v, %v", indexed, err)
	}
}
