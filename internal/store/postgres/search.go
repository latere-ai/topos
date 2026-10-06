// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/topos/session"
)

// arg appends v to args and answers its placeholder.
func arg(args *[]any, v any) string {
	*args = append(*args, v)
	return "$" + strconv.Itoa(len(*args))
}

// filters is the condition of a list's filters over the sessions table
// under alias, its name and a dot or "", with its parameters appended to
// args: what List pages through, Summarize counts and Search looks in. A
// filter o leaves unset adds no condition.
func filters(alias string, o session.ListOptions, args *[]any) string {
	conds := []string{"TRUE"}
	if o.Status != "" {
		conds = append(conds, alias+"status = "+arg(args, string(o.Status)))
	}
	if o.AgentID != "" {
		conds = append(conds, alias+"agent_id = "+arg(args, o.AgentID))
	}
	if len(o.Agents) > 0 {
		conds = append(conds, alias+"agent_id = ANY("+arg(args, o.Agents)+"::text[])")
	}
	if len(o.Owners) > 0 {
		conds = append(conds, alias+"owner = ANY("+arg(args, o.Owners)+"::text[])")
	}
	if o.Runner != "" {
		conds = append(conds, alias+"runner = "+arg(args, o.Runner))
	}
	switch o.Archived {
	case session.ArchivedExclude:
		conds = append(conds, alias+"archived_at IS NULL")
	case session.ArchivedOnly:
		conds = append(conds, alias+"archived_at IS NOT NULL")
	}
	if o.Root != "" {
		root := arg(args, o.Root)
		conds = append(conds, "("+alias+"id = "+root+" OR "+alias+"root_id = "+root+")")
	}
	if o.Parent != "" {
		conds = append(conds, alias+"parent_id = "+arg(args, o.Parent))
	}
	return strings.Join(conds, " AND ")
}

// lexemes escapes a word for a tsquery: quoted, with its quotes and
// backslashes doubled.
var lexemes = strings.NewReplacer(`\`, `\\`, `'`, `''`)

// tsquery is q as Postgres reads it against the documents
// session.SearchDocument writes: a word as the start of a lexeme, a
// phrase as its characters at positions in a row, every term required.
func tsquery(q session.Query) string {
	var parts []string
	for _, t := range q.Terms() {
		quoted := make([]string, len(t.Words))
		for i, w := range t.Words {
			quoted[i] = "'" + lexemes.Replace(w) + "'"
		}
		switch {
		case !t.Phrase:
			parts = append(parts, quoted[0]+":*")
		case len(quoted) == 1:
			parts = append(parts, quoted[0])
		default:
			parts = append(parts, "("+strings.Join(quoted, " <-> ")+")")
		}
	}
	return strings.Join(parts, " & ")
}

// eventColumns are the columns an event is read from, in readEvent's
// order.
const eventColumns = `session_id, seq, id, type, time, thread, turn, step, payload`

// readEvent reads one row of eventColumns.
func readEvent(rows pgx.Rows) (session.Event, error) {
	var e session.Event
	var seq int64
	var typ, payload string
	if err := rows.Scan(&e.SessionID, &seq, &e.ID, &typ, &e.Time, &e.Thread, &e.Turn, &e.Step, &payload); err != nil {
		return session.Event{}, err
	}
	e.Seq, e.Type, e.Payload, e.Time = uint64(seq), session.Type(typ), json.RawMessage(payload), e.Time.UTC()
	return e, nil
}

// Search finds the sessions of o's scope with an event whose index entry
// holds o's query (spec 050), newest first by id as List pages them, and
// for each its newest MaxSearchMatches such events. A redacted event is
// passed over whatever its entry holds, since a replica of an earlier
// release redacts without clearing it.
func (s *Store) Search(ctx context.Context, o session.SearchOptions) ([]session.SearchHit, string, error) {
	limit := session.SearchLimit(o.Limit)
	query := tsquery(o.Query)
	if query == "" {
		return []session.SearchHit{}, "", nil
	}
	args := []any{query}
	where := filters("s.", o.ListOptions, &args)
	if o.Cursor != "" {
		where += " AND s.id < " + arg(&args, o.Cursor)
	}
	rows, err := s.pool.Query(ctx, `SELECT s.body, s.root_id FROM sessions s WHERE `+where+`
		AND EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id AND NOT e.redacted AND e.search @@ to_tsquery('simple', $1))
		ORDER BY s.id DESC LIMIT `+arg(&args, limit+1), args...)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: search sessions: %w", err)
	}
	found, err := decodeRows(rows)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: search sessions: %w", err)
	}
	next := ""
	if len(found) > limit {
		found, next = found[:limit], found[limit-1].ID
	}
	if len(found) == 0 {
		return []session.SearchHit{}, "", nil
	}
	ids := make([]string, len(found))
	for i, sess := range found {
		ids[i] = sess.ID
	}
	rows, err = s.pool.Query(ctx, `SELECT `+eventColumns+` FROM (
		SELECT e.session_id, e.seq, e.id, e.type, e.time, e.thread, e.turn, e.step, e.payload,
			row_number() OVER (PARTITION BY e.session_id ORDER BY e.seq DESC) AS nth
		FROM events e WHERE e.session_id = ANY($1::text[]) AND NOT e.redacted AND e.search @@ to_tsquery('simple', $2)
	) m WHERE nth <= $3 ORDER BY session_id, seq DESC`, ids, query, session.MaxSearchMatches)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: read the matches of a search: %w", err)
	}
	matched := map[string][]session.Event{}
	for rows.Next() {
		e, err := readEvent(rows)
		if err != nil {
			rows.Close()
			return nil, "", fmt.Errorf("postgres: read a match of a search: %w", err)
		}
		matched[e.SessionID] = append(matched[e.SessionID], e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("postgres: read the matches of a search: %w", err)
	}
	hits := []session.SearchHit{}
	for _, sess := range found {
		// A session deleted between the two reads has no events left.
		if evs := matched[sess.ID]; len(evs) > 0 {
			hits = append(hits, session.SearchHit{Session: sess, Events: evs})
		}
	}
	return hits, next, nil
}

// unsearched reads the searched events no server has indexed, a page at
// a time after a (session_id, seq), in the order of the
// events_unsearched index, whose condition it repeats word for word so
// the planner reads that index. The types are session.SearchedTypes.
const unsearched = `SELECT ` + eventColumns + ` FROM events
	WHERE search IS NULL AND NOT redacted AND thread = ''
	AND type IN ('user.message', 'agent.message', 'user.answer')
	AND (session_id, seq) > ($1, $2) ORDER BY session_id, seq LIMIT $3`

// indexBatch is how many events one step of IndexSearch indexes.
const indexBatch = 500

// indexEvery is how often the store indexes the searched events no server
// has, after it has once at its open: those a replica of an earlier
// release writes while this one rolls out.
const indexEvery = 10 * time.Minute

// IndexSearch indexes every searched event no server has (spec 050), an
// event written before the search column or by a replica that does not
// write it, and answers how many it indexed. An event with no text is
// indexed as empty, so it is not read again, and one redacted meanwhile
// is left without an entry.
func (s *Store) IndexSearch(ctx context.Context) (int, error) {
	after, afterSeq, total := "", int64(0), 0
	for {
		rows, err := s.pool.Query(ctx, unsearched, after, afterSeq, indexBatch)
		if err != nil {
			return total, fmt.Errorf("postgres: read the events to index: %w", err)
		}
		var sids, docs []string
		var seqs []int64
		for rows.Next() {
			e, err := readEvent(rows)
			if err != nil {
				rows.Close()
				return total, fmt.Errorf("postgres: read an event to index: %w", err)
			}
			doc, _ := session.SearchDocument(e)
			sids, seqs, docs = append(sids, e.SessionID), append(seqs, int64(e.Seq)), append(docs, doc)
		}
		if err := rows.Err(); err != nil {
			return total, fmt.Errorf("postgres: read the events to index: %w", err)
		}
		if len(sids) == 0 {
			return total, nil
		}
		if _, err := s.pool.Exec(ctx, `UPDATE events e SET search = to_tsvector('simple', v.doc)
			FROM unnest($1::text[], $2::bigint[], $3::text[]) AS v(session_id, seq, doc)
			WHERE e.session_id = v.session_id AND e.seq = v.seq AND e.search IS NULL AND NOT e.redacted`, sids, seqs, docs); err != nil {
			return total, fmt.Errorf("postgres: index events: %w", err)
		}
		total += len(sids)
		after, afterSeq = sids[len(sids)-1], seqs[len(seqs)-1]
	}
}

// keepIndexed runs IndexSearch at the store's open and every indexEvery
// after, until ctx ends, and logs what it could not index.
func (s *Store) keepIndexed(ctx context.Context, log *slog.Logger) {
	defer close(s.indexed)
	tick := time.NewTicker(indexEvery)
	defer tick.Stop()
	for {
		n, err := s.IndexSearch(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			log.ErrorContext(ctx, "postgres: the searched events could not be indexed", "err", err)
		case n > 0:
			log.InfoContext(ctx, "postgres: indexed searched events", "events", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
