// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package postgres is the Postgres store of spec 014. As a
// session.Store it keeps sessions, their logs and their blobs in three
// tables, an append as one transaction under the session row's lock,
// Watch over LISTEN with a poll as the fallback, and the one-writer
// lease on the row's lease columns. As a store.Store it keeps agents,
// their versions and idempotency records, each write one transaction
// that any number of replicas may race.
package postgres

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
}

// Store is the Postgres store.
type Store struct {
	pool   *pgxpool.Pool
	listen *pgx.ConnConfig
	poll   time.Duration
	ttl    time.Duration
	now    func() time.Time
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
	s := &Store{pool: pool, listen: listen, poll: o.Poll, ttl: o.LeaseTTL, now: o.Now}
	if s.poll <= 0 {
		s.poll = 2 * time.Second
	}
	if s.ttl <= 0 {
		s.ttl = 60 * time.Second
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Close closes the pool.
func (s *Store) Close() { s.pool.Close() }

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
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var writerKind, writerSubject string
		if sess.Writer != nil {
			writerKind, writerSubject = sess.Writer.Kind, sess.Writer.Subject
		}
		_, err := tx.Exec(ctx, `INSERT INTO sessions (id, agent_id, agent_version, owner, runner, status, stop_reason, turn, last_seq,
			created_at, updated_at, expires_at, body, writer_kind, writer_subject)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			sess.ID, sess.Agent.ID, sess.Agent.Version, sess.Initiator.Subject, sess.Runner, string(sess.Status), string(sess.StopReason),
			sess.Turn, int64(sess.LastSeq), sess.CreatedAt, sess.UpdatedAt, sess.ExpiresAt, body, writerKind, writerSubject)
		if isUnique(err) {
			return fmt.Errorf("%w: %s", session.ErrExists, sess.ID)
		}
		if err != nil {
			return fmt.Errorf("postgres: create %s: %w", sess.ID, err)
		}
		for d, b := range blobs {
			if _, err := tx.Exec(ctx, `INSERT INTO blobs (session_id, digest, size, body) VALUES ($1, $2, $3, $4)`, sess.ID, string(d), len(b), b); err != nil {
				return fmt.Errorf("postgres: store blob %s: %w", d, err)
			}
		}
		return nil
	})
}

func (s *Store) Get(ctx context.Context, id string) (session.Session, error) {
	if err := session.CheckID(session.PrefixSession, id); err != nil {
		return session.Session{}, err
	}
	var body string
	err := s.pool.QueryRow(ctx, `SELECT body FROM sessions WHERE id = $1`, id).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return session.Session{}, fmt.Errorf("%w: %s", session.ErrNotFound, id)
	}
	if err != nil {
		return session.Session{}, fmt.Errorf("postgres: read session %s: %w", id, err)
	}
	return decodeSession(body)
}

func decodeSession(body string) (session.Session, error) {
	var sess session.Session
	if err := json.Unmarshal([]byte(body), &sess); err != nil {
		return session.Session{}, fmt.Errorf("%w: a session row: %w", session.ErrCorrupt, err)
	}
	return sess, nil
}

func (s *Store) List(ctx context.Context, o session.ListOptions) ([]session.Session, string, error) {
	limit := o.Limit
	if limit <= 0 {
		limit = session.DefaultListLimit
	}
	owners := o.Owners
	if owners == nil {
		owners = []string{}
	}
	rows, err := s.pool.Query(ctx, `SELECT body FROM sessions
		WHERE ($1 = '' OR status = $1) AND ($2 = '' OR agent_id = $2) AND ($3 = '' OR id < $3)
		AND (cardinality($5::text[]) = 0 OR owner = ANY($5::text[])) AND ($6 = '' OR runner = $6)
		ORDER BY id DESC LIMIT $4`, string(o.Status), o.AgentID, o.Cursor, limit+1, owners, o.Runner)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list sessions: %w", err)
	}
	bodies, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list sessions: %w", err)
	}
	var out []session.Session
	for _, b := range bodies {
		sess, err := decodeSession(b)
		if err != nil {
			return nil, "", err
		}
		out = append(out, sess)
	}
	if len(out) > limit {
		return out[:limit], out[limit-1].ID, nil
	}
	return out, "", nil
}

// locked reads a session under its row lock for the rest of tx.
func locked(ctx context.Context, tx pgx.Tx, id string) (session.Session, error) {
	var body string
	err := tx.QueryRow(ctx, `SELECT body FROM sessions WHERE id = $1 FOR UPDATE`, id).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return session.Session{}, fmt.Errorf("%w: %s", session.ErrNotFound, id)
	}
	if err != nil {
		return session.Session{}, fmt.Errorf("postgres: lock session %s: %w", id, err)
	}
	return decodeSession(body)
}

// saveHeader writes a session's header back with its filter columns.
func saveHeader(ctx context.Context, tx pgx.Tx, sess session.Session) error {
	body, err := encode(sess)
	if err != nil {
		return err
	}
	var ended any
	if sess.Status == session.StatusEnded {
		ended = sess.UpdatedAt
	}
	_, err = tx.Exec(ctx, `UPDATE sessions SET body = $2, status = $3, stop_reason = $4, turn = $5, last_seq = $6, updated_at = $7,
		ended_at = COALESCE(ended_at, $8) WHERE id = $1`,
		sess.ID, body, string(sess.Status), string(sess.StopReason), sess.Turn, int64(sess.LastSeq), sess.UpdatedAt, ended)
	if err != nil {
		return fmt.Errorf("postgres: save session %s: %w", sess.ID, err)
	}
	return nil
}

func insertEvents(ctx context.Context, tx pgx.Tx, events []session.Event) error {
	for _, e := range events {
		_, err := tx.Exec(ctx, `INSERT INTO events (session_id, seq, id, type, time, thread, turn, step, payload, redacted)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			e.SessionID, int64(e.Seq), e.ID, string(e.Type), e.Time, e.Thread, e.Turn, e.Step, string(e.Payload), e.Redacted())
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

// Watch replays the events from fromSeq, then follows LISTEN on a
// connection of its own, with a poll as the fallback for a lost
// notification. The channel closes when ctx ends, when the session is
// deleted, or when the log cannot be read.
func (s *Store) Watch(ctx context.Context, id string, fromSeq uint64) (<-chan session.Event, error) {
	if err := s.exists(ctx, id); err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, s.listen)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect to listen: %w", err)
	}
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return nil, errors.Join(fmt.Errorf("postgres: listen: %w", err), conn.Close(context.WithoutCancel(ctx)))
	}
	out := make(chan session.Event)
	go func() {
		defer close(out)
		defer func() {
			if err := conn.Close(context.WithoutCancel(ctx)); err != nil {
				return
			}
		}()
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
			wctx, cancel := context.WithTimeout(ctx, s.poll)
			_, err = conn.WaitForNotification(wctx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
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
	_, err = s.pool.Exec(ctx, `INSERT INTO blobs (session_id, digest, size, body) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, id, string(d), len(b), b)
	if isForeignKey(err) {
		return "", fmt.Errorf("%w: %s", session.ErrNotFound, id)
	}
	if err != nil {
		return "", fmt.Errorf("postgres: store blob: %w", err)
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
	err := s.pool.QueryRow(ctx, `SELECT body FROM blobs WHERE session_id = $1 AND digest = $2`, id, string(d)).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: blob %s", session.ErrNotFound, d)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: read blob %s: %w", d, err)
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
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		sess, err := locked(ctx, tx, id)
		if err != nil {
			return err
		}
		log, err := queryEvents(ctx, tx, id, 1, 0)
		if err != nil {
			return err
		}
		var target *session.Event
		for i := range log {
			if log[i].ID == eventID {
				target = &log[i]
			}
		}
		if target == nil {
			return fmt.Errorf("%w: event %s", session.ErrNotFound, eventID)
		}
		if target.Redacted() {
			return nil
		}
		orphans := session.OrphanBlobs(*target, log)
		tomb, red, err := session.Tombstone(*target, sess.LastSeq, by, reason, time.Now())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE events SET payload = $3, redacted = true WHERE session_id = $1 AND id = $2`, id, eventID, string(tomb.Payload)); err != nil {
			return fmt.Errorf("postgres: redact %s: %w", eventID, err)
		}
		if err := insertEvents(ctx, tx, []session.Event{red}); err != nil {
			return err
		}
		for _, d := range orphans {
			if _, err := tx.Exec(ctx, `DELETE FROM blobs WHERE session_id = $1 AND digest = $2`, id, string(d)); err != nil {
				return fmt.Errorf("postgres: delete blob %s: %w", d, err)
			}
		}
		session.ApplyBatch(&sess, []session.Event{red})
		if err := saveHeader(ctx, tx, sess); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, id)
		return err
	})
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
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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
}

var _ session.Store = (*Store)(nil)
