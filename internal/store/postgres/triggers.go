// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

const triggerColumns = `id, name, owner, claims, agent_id, version, digest, doc, suspended, next_fire_at, last_fired_at, last_session_id, counts, created_at, updated_at, spend`

func scanTrigger(row pgx.Row) (store.Trigger, error) {
	var t store.Trigger
	var doc, counts, claims, spend string
	if err := row.Scan(&t.ID, &t.Name, &t.Owner, &claims, &t.AgentID, &t.Version, &t.Digest, &doc, &t.Suspended,
		&t.NextFireAt, &t.LastFiredAt, &t.LastSessionID, &counts, &t.CreatedAt, &t.UpdatedAt, &spend); err != nil {
		return store.Trigger{}, err
	}
	if spend != "" {
		t.Spend = &v1.TriggerSpend{}
		if err := json.Unmarshal([]byte(spend), t.Spend); err != nil {
			return store.Trigger{}, fmt.Errorf("%w: trigger %s's spend: %w", session.ErrCorrupt, t.ID, err)
		}
	}
	if err := json.Unmarshal([]byte(counts), &t.Counts); err != nil {
		return store.Trigger{}, fmt.Errorf("%w: trigger %s's counts: %w", session.ErrCorrupt, t.ID, err)
	}
	if err := json.Unmarshal([]byte(claims), &t.Claims); err != nil {
		return store.Trigger{}, fmt.Errorf("%w: trigger %s's claims: %w", session.ErrCorrupt, t.ID, err)
	}
	t.Doc, t.CreatedAt, t.UpdatedAt = []byte(doc), t.CreatedAt.UTC(), t.UpdatedAt.UTC()
	t.NextFireAt, t.LastFiredAt = utc(t.NextFireAt), utc(t.LastFiredAt)
	return t, nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func encodeCounts(c v1.TriggerCounts) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("postgres: encode a trigger's counts: %w", err)
	}
	return string(b), nil
}

func readTrigger(q pgx.Row, what string) (store.Trigger, error) {
	t, err := scanTrigger(q)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Trigger{}, fmt.Errorf("%w: trigger %s", store.ErrNotFound, what)
	}
	if err != nil {
		return store.Trigger{}, fmt.Errorf("postgres: read trigger %s: %w", what, err)
	}
	return t, nil
}

func (s *Store) Trigger(ctx context.Context, id string) (store.Trigger, error) {
	return readTrigger(s.pool.QueryRow(ctx, `SELECT `+triggerColumns+` FROM triggers WHERE id = $1`, id), id)
}

func (s *Store) TriggerByName(ctx context.Context, owner, name string) (store.Trigger, error) {
	return readTrigger(s.pool.QueryRow(ctx, `SELECT `+triggerColumns+` FROM triggers WHERE owner = $1 AND name = $2`, owner, name), name)
}

func (s *Store) ListTriggers(ctx context.Context, o store.TriggerList) ([]store.Trigger, string, error) {
	after, err := store.Uncursor(o.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := o.Limit
	if limit <= 0 {
		limit = session.DefaultListLimit
	}
	owners := o.Owners
	if owners == nil {
		owners = []string{}
	}
	rows, err := s.pool.Query(ctx, `SELECT `+triggerColumns+` FROM triggers
		WHERE id > $1 AND (cardinality($2::text[]) = 0 OR owner = ANY($2::text[]))
		ORDER BY id LIMIT $3`, after, owners, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list triggers: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.Trigger, error) { return scanTrigger(row) })
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list triggers: %w", err)
	}
	if len(out) > limit {
		return out[:limit], store.Cursor(out[limit-1].ID), nil
	}
	return out, "", nil
}

// PutTrigger inserts version 1, a unique violation on the id or on the
// owner's name being ErrConflict; a later apply updates the row where its
// stored version is the one named or the one before, and keeps the
// firing record and the lease.
func (s *Store) PutTrigger(ctx context.Context, t store.Trigger) error {
	if err := store.CheckTrigger(t); err != nil {
		return err
	}
	counts, err := encodeCounts(v1.TriggerCounts{})
	if err != nil {
		return err
	}
	claims, err := json.Marshal(t.Claims)
	if err != nil {
		return fmt.Errorf("postgres: encode a trigger's claims: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE triggers SET name = $2, owner = $3, claims = $4, agent_id = $5, version = $6, digest = $7, doc = $8,
		suspended = $9, next_fire_at = $10, updated_at = $11 WHERE id = $1 AND (version = $6 OR version = $6 - 1)`,
		t.ID, t.Name, t.Owner, string(claims), t.AgentID, t.Version, t.Digest, string(t.Doc), t.Suspended, utc(t.NextFireAt), t.UpdatedAt.UTC())
	switch {
	case isUnique(err):
		return fmt.Errorf("%w: trigger %s exists", store.ErrConflict, t.Name)
	case err != nil:
		return fmt.Errorf("postgres: update trigger %s: %w", t.ID, err)
	case tag.RowsAffected() == 1:
		return nil
	case t.Version != 1:
		return fmt.Errorf("%w: trigger %s version %d does not follow the stored version", store.ErrConflict, t.ID, t.Version)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO triggers (`+triggerColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULL, '', $11, $12, $13, '')`,
		t.ID, t.Name, t.Owner, string(claims), t.AgentID, t.Version, t.Digest, string(t.Doc), t.Suspended, utc(t.NextFireAt), counts, t.CreatedAt.UTC(), t.UpdatedAt.UTC())
	if isUnique(err) {
		return fmt.Errorf("%w: trigger %s exists", store.ErrConflict, t.Name)
	}
	if err != nil {
		return fmt.Errorf("postgres: create trigger %s: %w", t.Name, err)
	}
	return nil
}

// DeleteTrigger removes the row, and its firings and keys with it.
func (s *Store) DeleteTrigger(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM triggers WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("postgres: delete trigger %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: trigger %s", store.ErrNotFound, id)
	}
	return nil
}

func (s *Store) DueTriggers(ctx context.Context, at time.Time) ([]store.Trigger, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+triggerColumns+` FROM triggers
		WHERE next_fire_at IS NOT NULL AND NOT suspended AND next_fire_at <= $1 ORDER BY next_fire_at, id`, at.UTC())
	if err != nil {
		return nil, fmt.Errorf("postgres: list the due triggers: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.Trigger, error) { return scanTrigger(row) })
	if err != nil {
		return nil, fmt.Errorf("postgres: list the due triggers: %w", err)
	}
	return out, nil
}

// exec runs a statement on one trigger's row and answers ErrNotFound when
// no row is the trigger's.
func (s *Store) exec(ctx context.Context, id, what, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("postgres: %s of trigger %s: %w", what, id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: trigger %s", store.ErrNotFound, id)
	}
	return nil
}

func (s *Store) SetNextFire(ctx context.Context, id string, at *time.Time) error {
	return s.exec(ctx, id, "move the next fire time", `UPDATE triggers SET next_fire_at = $2 WHERE id = $1`, id, utc(at))
}

// LeaseTrigger takes the lease in one statement: the row is updated only
// when its lease is free, expired on this replica's clock, or holder's
// own, so replicas racing a free lease take it once.
func (s *Store) LeaseTrigger(ctx context.Context, id, holder string, until time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE triggers SET lease_holder = $2, lease_expires_at = $3
		WHERE id = $1 AND (lease_holder = '' OR lease_holder = $2 OR lease_expires_at IS NULL OR lease_expires_at <= $4)`,
		id, holder, until.UTC(), s.now().UTC())
	if err != nil {
		return false, fmt.Errorf("postgres: lease trigger %s: %w", id, err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	if _, err := s.Trigger(ctx, id); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) ReleaseTrigger(ctx context.Context, id, holder string) error {
	if _, err := s.pool.Exec(ctx, `UPDATE triggers SET lease_holder = '', lease_expires_at = NULL WHERE id = $1 AND lease_holder = $2`, id, holder); err != nil {
		return fmt.Errorf("postgres: release trigger %s: %w", id, err)
	}
	return nil
}

const firingColumns = `id, trigger_id, dedupe, origin, envelope, key, outcome, reason, session_id, session_open, replica, fired_at, received_at, fired_by`

func scanFiring(row pgx.Row) (store.Firing, error) {
	var f store.Firing
	var envelope, firedBy string
	if err := row.Scan(&f.ID, &f.TriggerID, &f.Dedupe, &f.Origin, &envelope, &f.Key, &f.Outcome, &f.Reason, &f.SessionID, &f.Open,
		&f.Replica, &f.Time, &f.ReceivedAt, &firedBy); err != nil {
		return store.Firing{}, err
	}
	f.Time, f.ReceivedAt = f.Time.UTC(), f.ReceivedAt.UTC()
	if envelope != "" {
		f.Envelope = []byte(envelope)
	}
	if firedBy != "" {
		f.FiredBy = []byte(firedBy)
	}
	return f, nil
}

func collectFirings(rows pgx.Rows, err error, what string) ([]store.Firing, error) {
	if err != nil {
		return nil, fmt.Errorf("postgres: %s: %w", what, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.Firing, error) { return scanFiring(row) })
	if err != nil {
		return nil, fmt.Errorf("postgres: %s: %w", what, err)
	}
	return out, nil
}

// addCount changes the trigger's firing record by apply under its row
// lock, inside tx.
func addCount(ctx context.Context, tx pgx.Tx, id string, apply func(t *store.Trigger)) error {
	var raw string
	var t store.Trigger
	err := tx.QueryRow(ctx, `SELECT counts, last_fired_at, last_session_id FROM triggers WHERE id = $1 FOR UPDATE`, id).Scan(&raw, &t.LastFiredAt, &t.LastSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: trigger %s", store.ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("postgres: lock trigger %s: %w", id, err)
	}
	if err := json.Unmarshal([]byte(raw), &t.Counts); err != nil {
		return fmt.Errorf("%w: trigger %s's counts: %w", session.ErrCorrupt, id, err)
	}
	apply(&t)
	counts, err := encodeCounts(t.Counts)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE triggers SET counts = $2, last_fired_at = $3, last_session_id = $4 WHERE id = $1`,
		id, counts, utc(t.LastFiredAt), t.LastSessionID); err != nil {
		return fmt.Errorf("postgres: count a firing of %s: %w", id, err)
	}
	return nil
}

// ClaimFiring is one transaction: an insert that does nothing when the
// firing is held, then a read of the held row under its lock, taken
// again when its outcome is failed or unrecorded.
func (s *Store) ClaimFiring(ctx context.Context, f store.Firing) (store.Firing, bool, error) {
	if err := store.CheckFiring(f); err != nil {
		return store.Firing{}, false, err
	}
	f.Outcome, f.Reason, f.SessionID, f.Open = "", "", "", false
	var out store.Firing
	var fresh bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO trigger_firings (`+firingColumns+`) VALUES ($1, $2, $3, $4, $5, $6, '', '', '', false, $7, $8, $9, $10)
			ON CONFLICT (trigger_id, dedupe) DO NOTHING`,
			f.ID, f.TriggerID, f.Dedupe, f.Origin, string(f.Envelope), f.Key, f.Replica, f.Time.UTC(), f.ReceivedAt.UTC(), string(f.FiredBy))
		if isForeignKey(err) {
			return fmt.Errorf("%w: trigger %s", store.ErrNotFound, f.TriggerID)
		}
		if err != nil {
			return fmt.Errorf("postgres: claim a firing of %s: %w", f.TriggerID, err)
		}
		if tag.RowsAffected() == 1 {
			out, fresh = f, true
			return nil
		}
		held, err := scanFiring(tx.QueryRow(ctx, `SELECT `+firingColumns+` FROM trigger_firings WHERE trigger_id = $1 AND dedupe = $2 FOR UPDATE`, f.TriggerID, f.Dedupe))
		if err != nil {
			return fmt.Errorf("postgres: read a firing of %s: %w", f.TriggerID, err)
		}
		if !store.Retaken(held, f) {
			out = held
			return nil
		}
		if held.Outcome != "" {
			if err := addCount(ctx, tx, f.TriggerID, func(t *store.Trigger) { store.Count(&t.Counts, held.Outcome, -1) }); err != nil {
				return err
			}
		}
		// The firing runs again under its first id, and with the key it
		// was first recorded with when it has one.
		f.ID = held.ID
		if len(held.FiredBy) > 0 {
			f.FiredBy = held.FiredBy
		}
		if _, err := tx.Exec(ctx, `UPDATE trigger_firings SET origin = $3, envelope = $4, key = $5, outcome = '', reason = '', session_id = '',
			session_open = false, replica = $6, fired_at = $7, received_at = $8, fired_by = $9 WHERE trigger_id = $1 AND dedupe = $2`,
			f.TriggerID, f.Dedupe, f.Origin, string(f.Envelope), f.Key, f.Replica, f.Time.UTC(), f.ReceivedAt.UTC(), string(f.FiredBy)); err != nil {
			return fmt.Errorf("postgres: take a firing of %s again: %w", f.TriggerID, err)
		}
		out, fresh = f, true
		return nil
	})
	if err != nil {
		return store.Firing{}, false, err
	}
	return out, fresh, nil
}

// RecordFiring moves the firing's outcome only from the one named and the
// trigger's counts with it, in one transaction.
func (s *Store) RecordFiring(ctx context.Context, f store.Firing, from string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE trigger_firings SET outcome = $4, reason = $5, key = $6, session_id = $7, session_open = $8
			WHERE trigger_id = $1 AND dedupe = $2 AND id = $3 AND outcome = $9`,
			f.TriggerID, f.Dedupe, f.ID, f.Outcome, f.Reason, f.Key, f.SessionID, f.Open, from)
		if err != nil {
			return fmt.Errorf("postgres: record firing %s: %w", f.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: firing %s is not %q", store.ErrConflict, f.ID, from)
		}
		now := s.now()
		return addCount(ctx, tx, f.TriggerID, func(t *store.Trigger) { store.Record(t, f, from, now) })
	})
}

// SpendFiring checks the firing's outcome is the one named and deletes
// the trigger, its firings and keys with it, in one transaction.
func (s *Store) SpendFiring(ctx context.Context, f store.Firing, from string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var outcome string
		err := tx.QueryRow(ctx, `SELECT outcome FROM trigger_firings WHERE trigger_id = $1 AND dedupe = $2 AND id = $3 FOR UPDATE`, f.TriggerID, f.Dedupe, f.ID).Scan(&outcome)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("%w: firing %s is not %q", store.ErrConflict, f.ID, from)
		case err != nil:
			return fmt.Errorf("postgres: read firing %s: %w", f.ID, err)
		case outcome != from:
			return fmt.Errorf("%w: firing %s is not %q", store.ErrConflict, f.ID, from)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM triggers WHERE id = $1`, f.TriggerID)
		if err != nil {
			return fmt.Errorf("postgres: spend trigger %s: %w", f.TriggerID, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: trigger %s", store.ErrNotFound, f.TriggerID)
		}
		return nil
	})
}

func (s *Store) CountFiltered(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return addCount(ctx, tx, id, func(t *store.Trigger) { store.Count(&t.Counts, store.OutcomeFiltered, 1) })
	})
}

func (s *Store) Firings(ctx context.Context, id string, limit int, cursor string) ([]store.Firing, string, error) {
	before, err := store.Uncursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if _, err := s.Trigger(ctx, id); err != nil {
		return nil, "", err
	}
	if limit <= 0 {
		limit = session.DefaultListLimit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+firingColumns+` FROM trigger_firings
		WHERE trigger_id = $1 AND ($2 = '' OR id < $2) ORDER BY id DESC LIMIT $3`, id, before, limit+1)
	out, err := collectFirings(rows, err, "list the firings of "+id)
	if err != nil {
		return nil, "", err
	}
	if len(out) > limit {
		return out[:limit], store.Cursor(out[limit-1].ID), nil
	}
	return out, "", nil
}

func (s *Store) HeldFirings(ctx context.Context) ([]store.Firing, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+firingColumns+` FROM trigger_firings WHERE outcome = 'held' ORDER BY id`)
	return collectFirings(rows, err, "list the held firings")
}

func (s *Store) OpenFirings(ctx context.Context, id string) ([]store.Firing, error) {
	if _, err := s.Trigger(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+firingColumns+` FROM trigger_firings WHERE trigger_id = $1 AND session_open ORDER BY id`, id)
	return collectFirings(rows, err, "list the open firings of "+id)
}

// CloseSession closes the session's firings and clears the keys that
// name it, in one transaction.
func (s *Store) CloseSession(ctx context.Context, id, sessionID string) error {
	if _, err := s.Trigger(ctx, id); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE trigger_firings SET session_open = false WHERE trigger_id = $1 AND session_id = $2 AND session_open`, id, sessionID); err != nil {
			return fmt.Errorf("postgres: close the firings of %s: %w", sessionID, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM trigger_sessions WHERE trigger_id = $1 AND session_id = $2`, id, sessionID); err != nil {
			return fmt.Errorf("postgres: clear the keys of %s: %w", sessionID, err)
		}
		return nil
	})
}

func (s *Store) TriggerSession(ctx context.Context, id, key string) (string, error) {
	var sessionID string
	err := s.pool.QueryRow(ctx, `SELECT session_id FROM trigger_sessions WHERE trigger_id = $1 AND key = $2`, id, key).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: trigger %s names no session by its key", store.ErrNotFound, id)
	}
	if err != nil {
		return "", fmt.Errorf("postgres: read a key of %s: %w", id, err)
	}
	return sessionID, nil
}

func (s *Store) SetTriggerSession(ctx context.Context, id, key, sessionID string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO trigger_sessions (trigger_id, key, session_id) VALUES ($1, $2, $3)
		ON CONFLICT (trigger_id, key) DO UPDATE SET session_id = EXCLUDED.session_id`, id, key, sessionID)
	if isForeignKey(err) {
		return fmt.Errorf("%w: trigger %s", store.ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("postgres: map a key of %s: %w", id, err)
	}
	return nil
}

func (s *Store) Hold(ctx context.Context, owner string) (*time.Time, error) {
	var since time.Time
	err := s.pool.QueryRow(ctx, `SELECT since FROM trigger_holds WHERE owner = $1`, owner).Scan(&since)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: read the hold of %s: %w", owner, err)
	}
	since = since.UTC()
	return &since, nil
}

// SetHold inserts the hold, keeping one already set, or deletes it.
func (s *Store) SetHold(ctx context.Context, owner string, since *time.Time) error {
	var err error
	if since == nil {
		_, err = s.pool.Exec(ctx, `DELETE FROM trigger_holds WHERE owner = $1`, owner)
	} else {
		_, err = s.pool.Exec(ctx, `INSERT INTO trigger_holds (owner, since) VALUES ($1, $2) ON CONFLICT (owner) DO NOTHING`, owner, since.UTC())
	}
	if err != nil {
		return fmt.Errorf("postgres: set the hold of %s: %w", owner, err)
	}
	return nil
}

func (s *Store) HeldOwners(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT owner FROM trigger_holds ORDER BY owner`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list the held owners: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("postgres: list the held owners: %w", err)
	}
	return out, nil
}

// SetSpend writes the trigger's week as JSON text (spec 087).
func (s *Store) SetSpend(ctx context.Context, id string, spend v1.TriggerSpend) error {
	b, err := json.Marshal(spend)
	if err != nil {
		return fmt.Errorf("postgres: encode a trigger's spend: %w", err)
	}
	return s.exec(ctx, id, "set the spend", `UPDATE triggers SET spend = $2 WHERE id = $1`, id, string(b))
}

// runColumns are a run's columns in the order scanRun reads them.
const runColumns = `firing_id, owner, trigger_id, session_id, wake, session_trigger_id, at, outcome, reason, cost_usd_micro, ended`

// PutRun writes a run, replacing the row of its firing whole.
func (s *Store) PutRun(ctx context.Context, r store.Run) error {
	if err := store.CheckRun(r); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO trigger_runs (`+runColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (firing_id) DO UPDATE SET owner = $2, trigger_id = $3, session_id = $4, wake = $5, session_trigger_id = $6, at = $7, outcome = $8, reason = $9,
		cost_usd_micro = $10, ended = $11`,
		r.FiringID, r.Owner, r.TriggerID, r.SessionID, r.Wake, r.SessionTriggerID, r.At.UTC(), r.Outcome, r.Reason, r.CostUSDMicro, r.Ended); err != nil {
		return fmt.Errorf("postgres: put the run of %s: %w", r.FiringID, err)
	}
	return nil
}

// SetRunCosts writes every cost in one statement, to the runs that have
// not ended, as store.Run.Costed does.
func (s *Store) SetRunCosts(ctx context.Context, costs []store.RunCost) error {
	if len(costs) == 0 {
		return nil
	}
	ids, cost, ended := make([]string, len(costs)), make([]int64, len(costs)), make([]bool, len(costs))
	for i, c := range costs {
		ids[i], cost[i], ended[i] = c.FiringID, c.CostUSDMicro, c.Ended
	}
	if _, err := s.pool.Exec(ctx, `UPDATE trigger_runs AS r SET cost_usd_micro = c.cost, ended = c.ended
		FROM unnest($1::text[], $2::bigint[], $3::boolean[]) AS c(firing_id, cost, ended)
		WHERE r.firing_id = c.firing_id AND NOT r.ended`, ids, cost, ended); err != nil {
		return fmt.Errorf("postgres: set the cost of %d runs: %w", len(costs), err)
	}
	return nil
}

func (s *Store) Runs(ctx context.Context, owner string, since time.Time) ([]store.Run, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+runColumns+` FROM trigger_runs WHERE owner = $1 AND at >= $2 ORDER BY at, firing_id`, owner, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("postgres: list the runs of %s: %w", owner, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.Run, error) {
		var r store.Run
		if err := row.Scan(&r.FiringID, &r.Owner, &r.TriggerID, &r.SessionID, &r.Wake, &r.SessionTriggerID, &r.At, &r.Outcome, &r.Reason, &r.CostUSDMicro, &r.Ended); err != nil {
			return store.Run{}, err
		}
		r.At = r.At.UTC()
		return r, nil
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: read the runs of %s: %w", owner, err)
	}
	return out, nil
}

func (s *Store) PruneRuns(ctx context.Context, before time.Time) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM trigger_runs WHERE at < $1`, before.UTC()); err != nil {
		return fmt.Errorf("postgres: prune the runs before %s: %w", before.UTC().Format(time.RFC3339), err)
	}
	return nil
}
