// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package postgres is the Postgres store of spec 014. As a
// session.Store it keeps sessions, their logs and their blobs in three
// tables, an append as one transaction under the session row's lock,
// Watch over the store's one LISTEN connection with a poll as the
// fallback, live deltas between replicas as notifications on that same
// connection, and the one-writer lease on the row's lease columns. As a
// store.Store it keeps agents, their versions and idempotency records,
// each write one transaction that any number of replicas may race.
package postgres

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // the pgx5:// migration driver
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"latere.ai/x/pkg/pgxmigrate"

	"latere.ai/x/topos/session"
)

//go:embed migrations/*.sql
var migrations embed.FS

// channel is the notification channel appends signal on.
const channel = "topos_events"

// Options tune a store; the zero value is spec 014's.
type Options struct {
	// PoolDSN serves queries through a transaction-pooling proxy when
	// set; LISTEN and the migrations always use the direct DSN.
	PoolDSN string
	// Poll is how often a watcher looks for events a lost notification
	// did not announce: 2 s by default.
	Poll time.Duration
	// LeaseTTL is how long a lease holds without a renew: 60 s by
	// default, renewed every quarter of it.
	LeaseTTL time.Duration
	// Now is the clock idempotency records expire on: time.Now by
	// default.
	Now func() time.Time
	// Log receives the listener connection's failures, and the dropped
	// live deltas at debug level; none by default.
	Log *slog.Logger
	// Blobs keeps the blob bodies outside the database (spec 014), each
	// with a blobs row of location object; nil keeps them in blobs.body.
	Blobs session.Blobs
}

// Store is the Postgres store.
type Store struct {
	pool     *pgxpool.Pool
	listen   *pgx.ConnConfig
	listener *listener
	// hub hands live deltas to this process's subscribers, and sender
	// sends the ones this process publishes to every replica.
	hub    *session.DeltaHub
	sender *sender
	poll   time.Duration
	ttl    time.Duration
	now    func() time.Time
	blobs  session.Blobs
	// stopIndex ends the background indexing of searched events no
	// server has indexed, and indexed is closed once it has ended.
	stopIndex context.CancelFunc
	indexed   chan struct{}
}

// Open applies the migrations on dsn and connects.
func Open(ctx context.Context, dsn string, o Options) (*Store, error) {
	serve := dsn
	if o.PoolDSN != "" {
		serve = o.PoolDSN
	}
	cfg, err := pgxpool.ParseConfig(serve)
	if err != nil {
		return nil, fmt.Errorf("postgres: the serving DSN: %w", err)
	}
	// A transaction-pooling proxy accepts described statements but not
	// prepared ones held across transactions.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	listen, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: the DSN: %w", err)
	}
	// A DSN that does not parse fails above at once; the migration is the
	// first step that dials.
	if err := pgxmigrate.Up(migrationDSN(dsn), migrations, "migrations"); err != nil {
		return nil, fmt.Errorf("postgres: migrate: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	hub := &session.DeltaHub{Log: log}
	s := &Store{pool: pool, listen: listen, listener: newListener(listen, log, hub), hub: hub, sender: &sender{pool: pool, hub: hub},
		poll: o.Poll, ttl: o.LeaseTTL, now: o.Now, blobs: o.Blobs}
	if s.poll <= 0 {
		s.poll = 2 * time.Second
	}
	if s.ttl <= 0 {
		s.ttl = 60 * time.Second
	}
	if s.now == nil {
		s.now = time.Now
	}
	// The indexing outlives the open's context, which may be a start's
	// deadline, and ends with Close.
	index, stop := context.WithCancel(context.WithoutCancel(ctx))
	s.stopIndex, s.indexed = stop, make(chan struct{})
	go s.keepIndexed(index, log)
	return s, nil
}

// Close sends the deltas already queued, then closes the listener
// connection and the pool. A watcher still open wakes, finds the pool
// closed, and closes its channel.
func (s *Store) Close() {
	s.stopIndex()
	<-s.indexed
	s.sender.close()
	s.listener.stop()
	s.pool.Close()
	s.listener.wakeAll()
}

// Ping reports whether the serving pool reaches the database; toposd's
// readiness asks it.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// migrationDSN names the pgx migration driver for a postgres:// DSN.
func migrationDSN(dsn string) string {
	for _, p := range []string{"postgresql://", "postgres://"} {
		if rest, ok := strings.CutPrefix(dsn, p); ok {
			return "pgx5://" + rest
		}
	}
	return dsn
}

func encode(v any) (string, error) {
	b, err := session.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("postgres: encode: %w", err)
	}
	return string(b), nil
}

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func isForeignKey(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503"
}

func (s *Store) Create(ctx context.Context, sess session.Session, blobs map[session.Digest][]byte) error {
	if err := session.CheckCreate(sess, blobs); err != nil {
		return err
	}
	body, err := encode(sess)
	if err != nil {
		return err
	}
	// A body kept outside is durable before its row, and before the
	// session appears.
	if s.blobs != nil {
		for d, b := range blobs {
			if err := s.blobs.PutBlob(ctx, sess.ID, d, b); err != nil {
				return err
			}
		}
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var writerKind, writerSubject string
		if sess.Writer != nil {
			writerKind, writerSubject = sess.Writer.Kind, sess.Writer.Subject
		}
		parentID, parentSeq, rootID := treeColumns(sess)
		_, err := tx.Exec(ctx, `INSERT INTO sessions (id, agent_id, agent_version, owner, runner, status, stop_reason, turn, last_seq,
			created_at, updated_at, expires_at, body, writer_kind, writer_subject, parent_id, parent_seq, root_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
			sess.ID, sess.Agent.ID, sess.Agent.Version, sess.Initiator.Subject, sess.Runner, string(sess.Status), string(sess.StopReason),
			sess.Turn, int64(sess.LastSeq), sess.CreatedAt, sess.UpdatedAt, sess.ExpiresAt, body, writerKind, writerSubject, parentID, parentSeq, rootID)
		if isUnique(err) {
			return fmt.Errorf("%w: %s", session.ErrExists, sess.ID)
		}
		if err != nil {
			return fmt.Errorf("postgres: create %s: %w", sess.ID, err)
		}
		for d, b := range blobs {
			if err := s.insertBlob(ctx, tx, sess.ID, d, b, false); err != nil {
				return err
			}
		}
		return nil
	})
}

// insertBlob writes a blob's row: its body in the row, or location
// object when the body is kept outside.
func (s *Store) insertBlob(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, id string, d session.Digest, b []byte, tolerate bool) error {
	location, body := "db", b
	if s.blobs != nil {
		location, body = "object", nil
	}
	query := `INSERT INTO blobs (session_id, digest, size, location, body) VALUES ($1, $2, $3, $4, $5)`
	if tolerate {
		query += ` ON CONFLICT DO NOTHING`
	}
	_, err := q.Exec(ctx, query, id, string(d), len(b), location, body)
	if isForeignKey(err) {
		return fmt.Errorf("%w: %s", session.ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("postgres: store blob %s: %w", d, err)
	}
	return nil
}

// treeColumns are a session's tree columns as its insert writes them,
// each nil for a session no fork made (spec 056).
func treeColumns(sess session.Session) (parentID *string, parentSeq *int64, rootID *string) {
	if p := sess.Parent; p != nil {
		id, seq := p.SessionID, int64(p.Seq)
		parentID, parentSeq = &id, &seq
	}
	if sess.Root != "" {
		root := sess.Root
		rootID = &root
	}
	return parentID, parentSeq, rootID
}

func (s *Store) Get(ctx context.Context, id string) (session.Session, error) {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return session.Session{}, err
	}
	var row sessionRow
	err := s.pool.QueryRow(ctx, `SELECT body, root_id FROM sessions WHERE id = $1`, id).Scan(&row.Body, &row.Root)
	if errors.Is(err, pgx.ErrNoRows) {
		return session.Session{}, fmt.Errorf("%w: %s", session.ErrNotFound, id)
	}
	if err != nil {
		return session.Session{}, fmt.Errorf("postgres: read session %s: %w", id, err)
	}
	return row.decode()
}

// sessionRow is a session's body with its root column.
type sessionRow struct {
	Body string
	Root *string
}

// decode reads the body, its root filled from the column where a
// replica of an earlier release rewrote the body without it (spec 056).
func (r sessionRow) decode() (session.Session, error) {
	var sess session.Session
	if err := json.Unmarshal([]byte(r.Body), &sess); err != nil {
		return session.Session{}, fmt.Errorf("%w: a session row: %w", session.ErrCorrupt, err)
	}
	if sess.Root == "" && r.Root != nil {
		sess.Root = *r.Root
	}
	return sess, nil
}

// decodeRows reads the bodies and root columns of a query's rows.
func decodeRows(rows pgx.Rows) ([]session.Session, error) {
	read, err := pgx.CollectRows(rows, pgx.RowToStructByPos[sessionRow])
	if err != nil {
		return nil, err
	}
	out := make([]session.Session, 0, len(read))
	for _, r := range read {
		sess, err := r.decode()
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, nil
}

// List pages the filtered sessions newest first. Grouped by tree, it
// keeps of each tree's filtered sessions the newest, counting them, and
// pages those by their own id (spec 056).
func (s *Store) List(ctx context.Context, o session.ListOptions) ([]session.Session, string, error) {
	limit := o.Limit
	if limit <= 0 {
		limit = session.DefaultListLimit
	}
	if o.Group == session.GroupTree {
		return s.listTrees(ctx, o, limit)
	}
	var args []any
	where := filters("", o, &args)
	if o.Cursor != "" {
		where += " AND id < " + arg(&args, o.Cursor)
	}
	rows, err := s.pool.Query(ctx, `SELECT body, root_id FROM sessions WHERE `+where+` ORDER BY id DESC LIMIT `+arg(&args, limit+1), args...)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list sessions: %w", err)
	}
	out, err := decodeRows(rows)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list sessions: %w", err)
	}
	if len(out) > limit {
		return out[:limit], out[limit-1].ID, nil
	}
	return out, "", nil
}

// listTrees is List grouped by tree: the filtered rows are grouped
// before the cursor applies, so a page starts at the tree after the one
// the cursor names.
func (s *Store) listTrees(ctx context.Context, o session.ListOptions, limit int) ([]session.Session, string, error) {
	var args []any
	where := filters("", o, &args)
	outer := "TRUE"
	if o.Cursor != "" {
		outer = "id < " + arg(&args, o.Cursor)
	}
	rows, err := s.pool.Query(ctx, `SELECT body, root_id, tree_root, tree_sessions FROM (
		SELECT DISTINCT ON (COALESCE(root_id, id)) id, body, root_id, COALESCE(root_id, id) AS tree_root,
			count(*) OVER (PARTITION BY COALESCE(root_id, id)) AS tree_sessions
		FROM sessions WHERE `+where+` ORDER BY COALESCE(root_id, id), id DESC
	) trees WHERE `+outer+` ORDER BY id DESC LIMIT `+arg(&args, limit+1), args...)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list session trees: %w", err)
	}
	type treeRow struct {
		Body     string
		Root     *string
		TreeRoot string
		Sessions int64
	}
	read, err := pgx.CollectRows(rows, pgx.RowToStructByPos[treeRow])
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list session trees: %w", err)
	}
	out := make([]session.Session, 0, len(read))
	for _, r := range read {
		sess, err := sessionRow{Body: r.Body, Root: r.Root}.decode()
		if err != nil {
			return nil, "", err
		}
		sess.Tree = &session.Tree{Root: r.TreeRoot, Sessions: int(r.Sessions)}
		out = append(out, sess)
	}
	if len(out) > limit {
		return out[:limit], out[limit-1].ID, nil
	}
	return out, "", nil
}

// Summarize counts in one query over the columns List filters on, so no
// session's body is read.
func (s *Store) Summarize(ctx context.Context, o session.ListOptions) (session.Summary, error) {
	o.Status = ""
	var args []any
	where := filters("", o, &args)
	running, idle := arg(&args, string(session.StatusRunning)), arg(&args, string(session.StatusIdle))
	ended, waiting := arg(&args, string(session.StatusEnded)), arg(&args, string(session.StopToolConfirmation))
	var nRunning, nWaiting, nIdle, nEnded, agents int64
	err := s.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status = `+running+`),
		count(*) FILTER (WHERE status = `+idle+` AND stop_reason = `+waiting+`),
		count(*) FILTER (WHERE status = `+idle+` AND stop_reason <> `+waiting+`),
		count(*) FILTER (WHERE status = `+ended+`),
		count(DISTINCT agent_id)
		FROM sessions WHERE `+where, args...,
	).Scan(&nRunning, &nWaiting, &nIdle, &nEnded, &agents)
	if err != nil {
		return session.Summary{}, fmt.Errorf("postgres: summarize sessions: %w", err)
	}
	return session.Summary{
		Sessions: session.Counts{Running: int(nRunning), WaitingForApproval: int(nWaiting), Idle: int(nIdle), Ended: int(nEnded)},
		Agents:   int(agents),
	}, nil
}

// locked reads a session under its row lock for the rest of tx.
func locked(ctx context.Context, tx pgx.Tx, id string) (session.Session, error) {
	var row sessionRow
	err := tx.QueryRow(ctx, `SELECT body, root_id FROM sessions WHERE id = $1 FOR UPDATE`, id).Scan(&row.Body, &row.Root)
	if errors.Is(err, pgx.ErrNoRows) {
		return session.Session{}, fmt.Errorf("%w: %s", session.ErrNotFound, id)
	}
	if err != nil {
		return session.Session{}, fmt.Errorf("postgres: lock session %s: %w", id, err)
	}
	return row.decode()
}

// saveHeader writes a session's header back with its filter columns. A
// tree column is set where the header names it and never cleared, so a
// fork an earlier replica inserted without them gains its parent here
// (spec 056).
func saveHeader(ctx context.Context, tx pgx.Tx, sess session.Session) error {
	body, err := encode(sess)
	if err != nil {
		return err
	}
	var ended any
	if sess.Status == session.StatusEnded {
		ended = sess.UpdatedAt
	}
	parentID, parentSeq, rootID := treeColumns(sess)
	_, err = tx.Exec(ctx, `UPDATE sessions SET body = $2, status = $3, stop_reason = $4, turn = $5, last_seq = $6, updated_at = $7,
		ended_at = COALESCE(ended_at, $8), parent_id = COALESCE($9, parent_id), parent_seq = COALESCE($10, parent_seq),
		root_id = COALESCE($11, root_id) WHERE id = $1`,
		sess.ID, body, string(sess.Status), string(sess.StopReason), sess.Turn, int64(sess.LastSeq), sess.UpdatedAt, ended, parentID, parentSeq, rootID)
	if err != nil {
		return fmt.Errorf("postgres: save session %s: %w", sess.ID, err)
	}
	return nil
}

// SetArchived sets or clears a session's archived_at, in its body and its
// filter column, under the row lock; the log is untouched.
func (s *Store) SetArchived(ctx context.Context, id string, at *time.Time) (session.Session, error) {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return session.Session{}, err
	}
	var out session.Session
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		sess, err := locked(ctx, tx, id)
		if err != nil {
			return err
		}
		out = sess
		if !session.Archive(&out, at) {
			return nil
		}
		body, err := encode(out)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET body = $2, archived_at = $3 WHERE id = $1`, id, body, out.ArchivedAt); err != nil {
			return fmt.Errorf("postgres: archive session %s: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return session.Session{}, err
	}
	return out, nil
}

func insertEvents(ctx context.Context, tx pgx.Tx, events []session.Event) error {
	for _, e := range events {
		// A searched event is indexed by its words as the search reads
		// them (spec 050); every other event has no entry.
		var search *string
		if doc, ok := session.SearchDocument(e); ok {
			search = &doc
		}
		_, err := tx.Exec(ctx, `INSERT INTO events (session_id, seq, id, type, time, thread, turn, step, payload, redacted, search)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, to_tsvector('simple', $11::text))`,
			e.SessionID, int64(e.Seq), e.ID, string(e.Type), e.Time, e.Thread, e.Turn, e.Step, string(e.Payload), e.Redacted(), search)
		if isUnique(err) {
			return fmt.Errorf("%w: event %s is already in the log", session.ErrSequenceConflict, e.ID)
		}
		if err != nil {
			return fmt.Errorf("postgres: append event %s: %w", e.ID, err)
		}
	}
	return nil
}

func (s *Store) Append(ctx context.Context, id string, afterSeq uint64, events []session.Event) (uint64, error) {
	return s.append(ctx, id, afterSeq, events, 0)
}

// append writes one batch in one transaction. A generation above zero is
// the lease the writer holds: the batch is refused with ErrLeaseLost
// unless the session's lease is still that one.
func (s *Store) append(ctx context.Context, id string, afterSeq uint64, events []session.Event, gen int64) (uint64, error) {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return 0, err
	}
	last := afterSeq + uint64(len(events))
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		sess, err := locked(ctx, tx, id)
		if err != nil {
			return err
		}
		if gen > 0 {
			var current int64
			var live bool
			if err := tx.QueryRow(ctx, `SELECT lease_generation, lease_holder IS NOT NULL AND lease_expires_at >= now() FROM sessions WHERE id = $1`, id).Scan(&current, &live); err != nil {
				return fmt.Errorf("postgres: read the lease of %s: %w", id, err)
			}
			if current != gen || !live {
				return fmt.Errorf("%w: %s is at lease generation %d, the writer holds %d", session.ErrLeaseLost, id, current, gen)
			}
		}
		retried, err := session.CheckBatch(id, sess.LastSeq, afterSeq, events, func(from uint64) ([]session.Event, error) {
			return queryEvents(ctx, tx, id, from, len(events))
		})
		if err != nil || retried {
			return err
		}
		if err := insertEvents(ctx, tx, events); err != nil {
			return err
		}
		session.ApplyBatch(&sess, events)
		if err := saveHeader(ctx, tx, sess); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, id)
		return err
	})
	if err != nil {
		return 0, err
	}
	return last, nil
}

// querier is the pool or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func queryEvents(ctx context.Context, q querier, id string, from uint64, limit int) ([]session.Event, error) {
	var lim any
	if limit > 0 {
		lim = limit
	}
	rows, err := q.Query(ctx, `SELECT seq, id, type, time, thread, turn, step, payload FROM events
		WHERE session_id = $1 AND seq >= $2 ORDER BY seq LIMIT $3`, id, int64(max(from, 1)), lim)
	if err != nil {
		return nil, fmt.Errorf("postgres: read events of %s: %w", id, err)
	}
	out := []session.Event{}
	for rows.Next() {
		var e session.Event
		var seq int64
		var typ, payload string
		if err := rows.Scan(&seq, &e.ID, &typ, &e.Time, &e.Thread, &e.Turn, &e.Step, &payload); err != nil {
			rows.Close()
			return nil, fmt.Errorf("postgres: read an event of %s: %w", id, err)
		}
		e.Seq, e.SessionID, e.Type, e.Payload = uint64(seq), id, session.Type(typ), json.RawMessage(payload)
		e.Time = e.Time.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: read events of %s: %w", id, err)
	}
	return out, nil
}

// exists reports ErrNotFound for a session that is not there.
func (s *Store) exists(ctx context.Context, id string) error {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return err
	}
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM sessions WHERE id = $1`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", session.ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("postgres: read session %s: %w", id, err)
	}
	return nil
}

func (s *Store) Events(ctx context.Context, id string, fromSeq uint64, limit int) ([]session.Event, error) {
	if err := s.exists(ctx, id); err != nil {
		return nil, err
	}
	return queryEvents(ctx, s.pool, id, fromSeq, limit)
}

// Watch replays the events from fromSeq, then reads again each time the
// store's listener announces an append to the session, and every poll
// interval in case a notification was lost. The channel closes when ctx
// ends, when the session is deleted, or when the log cannot be read.
func (s *Store) Watch(ctx context.Context, id string, fromSeq uint64) (<-chan session.Event, error) {
	if err := s.exists(ctx, id); err != nil {
		return nil, err
	}
	// The watcher is registered before the replay reads, so an append
	// that commits after the read began still wakes it.
	w := s.listener.subscribe(ctx, id)
	out := make(chan session.Event)
	go func() {
		defer close(out)
		defer s.listener.unsubscribe(id, w)
		poll := time.NewTimer(s.poll)
		defer poll.Stop()
		next := max(fromSeq, 1)
		for {
			evs, err := queryEvents(ctx, s.pool, id, next, 0)
			if err != nil {
				return
			}
			for _, e := range evs {
				select {
				case out <- e:
					next = e.Seq + 1
				case <-ctx.Done():
					return
				}
			}
			if len(evs) == 0 && s.exists(ctx, id) != nil {
				return
			}
			poll.Reset(s.poll)
			select {
			case <-w.wake:
			case <-poll.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (s *Store) PutBlob(ctx context.Context, id string, r io.Reader) (session.Digest, error) {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return "", err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("postgres: read blob: %w", err)
	}
	d := session.DigestOf(b)
	if s.blobs != nil {
		if err := s.exists(ctx, id); err != nil {
			return "", err
		}
		if err := s.blobs.PutBlob(ctx, id, d, b); err != nil {
			return "", err
		}
	}
	if err := s.insertBlob(ctx, s.pool, id, d, b, true); err != nil {
		return "", err
	}
	return d, nil
}

func (s *Store) Blob(ctx context.Context, id string, d session.Digest) (io.ReadCloser, error) {
	if err := s.exists(ctx, id); err != nil {
		return nil, err
	}
	if !d.Valid() {
		return nil, fmt.Errorf("%w: digest %q", session.ErrInvalid, d)
	}
	var body []byte
	var location string
	err := s.pool.QueryRow(ctx, `SELECT location, body FROM blobs WHERE session_id = $1 AND digest = $2`, id, string(d)).Scan(&location, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: blob %s", session.ErrNotFound, d)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: read blob %s: %w", d, err)
	}
	if location == "object" {
		if s.blobs == nil {
			return nil, fmt.Errorf("%w: blob %s is kept outside the database, and this store is given no blob store", session.ErrCorrupt, d)
		}
		if body, err = s.blobs.GetBlob(ctx, id, d); err != nil {
			return nil, err
		}
	}
	if session.DigestOf(body) != d {
		return nil, fmt.Errorf("%w: blob %s does not match its digest", session.ErrCorrupt, d)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (s *Store) Redact(ctx context.Context, id, eventID string, by session.Sender, reason string) error {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return err
	}
	var removed []session.Digest
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		sess, err := locked(ctx, tx, id)
		if err != nil {
			return err
		}
		log, err := queryEvents(ctx, tx, id, 1, 0)
		if err != nil {
			return err
		}
		r, err := session.Redact(log, eventID, sess.LastSeq, by, reason, time.Now())
		if err != nil || len(r.Records) == 0 {
			return err
		}
		for _, tomb := range r.Tombstones {
			if _, err := tx.Exec(ctx, `UPDATE events SET payload = $3, redacted = true, search = NULL WHERE session_id = $1 AND id = $2`, id, tomb.ID, string(tomb.Payload)); err != nil {
				return fmt.Errorf("postgres: redact %s: %w", tomb.ID, err)
			}
		}
		if err := insertEvents(ctx, tx, r.Records); err != nil {
			return err
		}
		for _, d := range r.Orphans {
			if _, err := tx.Exec(ctx, `DELETE FROM blobs WHERE session_id = $1 AND digest = $2`, id, string(d)); err != nil {
				return fmt.Errorf("postgres: delete blob %s: %w", d, err)
			}
		}
		removed = r.Orphans
		session.ApplyBatch(&sess, r.Records)
		if err := saveHeader(ctx, tx, sess); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, id)
		return err
	})
	if err != nil || s.blobs == nil {
		return err
	}
	// The bodies kept outside go once their rows have.
	var errs []error
	for _, d := range removed {
		errs = append(errs, s.blobs.DeleteBlob(ctx, id, d))
	}
	return errors.Join(errs...)
}

// holderOf reads the recorded holder of a held lease.
func holderOf(raw *string) session.Holder {
	var h session.Holder
	if raw != nil && json.Unmarshal([]byte(*raw), &h) == nil {
		return h
	}
	return session.Holder{}
}

// Acquire takes the session's lease when it is free or expired, and
// renews it in the background every quarter of its time to live.
func (s *Store) Acquire(ctx context.Context, id string, holder session.Holder) (session.Lease, error) {
	if err := s.exists(ctx, id); err != nil {
		return nil, err
	}
	if holder.AcquiredAt.IsZero() {
		holder.AcquiredAt = time.Now().UTC()
	}
	raw, err := encode(holder)
	if err != nil {
		return nil, err
	}
	var gen int64
	err = s.pool.QueryRow(ctx, `UPDATE sessions SET lease_holder = $2, lease_generation = lease_generation + 1,
		lease_expires_at = now() + $3 * interval '1 millisecond'
		WHERE id = $1 AND (lease_holder IS NULL OR lease_expires_at < now()) RETURNING lease_generation`,
		id, raw, s.ttl.Milliseconds()).Scan(&gen)
	if errors.Is(err, pgx.ErrNoRows) {
		var current *string
		if err := s.pool.QueryRow(ctx, `SELECT lease_holder FROM sessions WHERE id = $1`, id).Scan(&current); err != nil {
			return nil, fmt.Errorf("postgres: read the lease of %s: %w", id, err)
		}
		return nil, &session.LockedError{Holder: holderOf(current)}
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: acquire %s: %w", id, err)
	}
	l := &lease{s: s, id: id, gen: gen, lost: make(chan struct{}), stop: make(chan struct{})}
	go l.keep(context.WithoutCancel(ctx), l.stop)
	return l, nil
}

// Append writes a batch under this lease, fenced by its generation.
func (l *lease) Append(ctx context.Context, afterSeq uint64, events []session.Event) (uint64, error) {
	return l.s.append(ctx, l.id, afterSeq, events, l.gen)
}

var _ session.Fence = (*lease)(nil)

type lease struct {
	s    *Store
	id   string
	gen  int64
	lost chan struct{}
	stop chan struct{}
	once sync.Once
	mu   sync.Mutex
	done bool
}

// keep renews the lease until stop closes: at release or loss. Its
// context outlives the call that acquired the lease.
func (l *lease) keep(ctx context.Context, stop <-chan struct{}) {
	t := time.NewTicker(l.s.ttl / 4)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := l.Renew(ctx); err != nil {
				return
			}
		}
	}
}

func (l *lease) Renew(ctx context.Context) error {
	l.mu.Lock()
	done := l.done
	l.mu.Unlock()
	if done {
		return fmt.Errorf("%w: lease released", session.ErrLocked)
	}
	tag, err := l.s.pool.Exec(ctx, `UPDATE sessions SET lease_expires_at = now() + $3 * interval '1 millisecond'
		WHERE id = $1 AND lease_generation = $2 AND lease_holder IS NOT NULL`, l.id, l.gen, l.s.ttl.Milliseconds())
	if err != nil {
		return fmt.Errorf("postgres: renew the lease of %s: %w", l.id, err)
	}
	if tag.RowsAffected() == 0 {
		l.end()
		return fmt.Errorf("%w: the lease of %s was lost", session.ErrLocked, l.id)
	}
	return nil
}

func (l *lease) end() {
	l.once.Do(func() {
		l.mu.Lock()
		l.done = true
		l.mu.Unlock()
		close(l.stop)
		close(l.lost)
	})
}

func (l *lease) Release() error {
	l.mu.Lock()
	done := l.done
	l.mu.Unlock()
	if done {
		return nil
	}
	l.end()
	_, err := l.s.pool.Exec(context.Background(), `UPDATE sessions SET lease_holder = NULL, lease_expires_at = NULL
		WHERE id = $1 AND lease_generation = $2`, l.id, l.gen)
	if err != nil {
		return fmt.Errorf("postgres: release the lease of %s: %w", l.id, err)
	}
	return nil
}

func (l *lease) Lost() <-chan struct{} { return l.lost }

func (s *Store) Delete(ctx context.Context, id string) error {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return err
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var holder *string
		var live bool
		err := tx.QueryRow(ctx, `SELECT lease_holder, lease_holder IS NOT NULL AND lease_expires_at >= now() FROM sessions WHERE id = $1 FOR UPDATE`, id).Scan(&holder, &live)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", session.ErrNotFound, id)
		}
		if err != nil {
			return fmt.Errorf("postgres: lock session %s: %w", id, err)
		}
		if live {
			return &session.LockedError{Holder: holderOf(holder)}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id); err != nil {
			return fmt.Errorf("postgres: delete %s: %w", id, err)
		}
		_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, id)
		return err
	})
	if err != nil || s.blobs == nil {
		return err
	}
	// The bodies kept outside go last, so a crash before leaves bodies
	// with no session, which SweepBlobs removes, and never a session
	// without its bodies.
	return s.blobs.DeleteSession(ctx, id)
}

// SweepBlobs removes the bodies kept outside whose session is gone
// (spec 014), judging the grace of session.SweepBlobs on now.
func (s *Store) SweepBlobs(ctx context.Context, now time.Time) error {
	if s.blobs == nil {
		return nil
	}
	return session.SweepBlobs(ctx, s.blobs, func(ctx context.Context, id string) (bool, error) {
		err := s.exists(ctx, id)
		if errors.Is(err, session.ErrNotFound) {
			return true, nil
		}
		return false, err
	}, now)
}

var _ session.Store = (*Store)(nil)
