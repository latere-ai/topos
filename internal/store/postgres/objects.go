// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/session"
)

const agentColumns = `id, name, owner, latest_version, archived_at, created_at`

func scanAgent(row pgx.Row) (store.Agent, error) {
	var a store.Agent
	var archived *time.Time
	if err := row.Scan(&a.ID, &a.Name, &a.Owner, &a.Latest, &archived, &a.CreatedAt); err != nil {
		return store.Agent{}, err
	}
	a.CreatedAt = a.CreatedAt.UTC()
	if archived != nil {
		at := archived.UTC()
		a.ArchivedAt = &at
	}
	return a, nil
}

func (s *Store) Agent(ctx context.Context, id string) (store.Agent, error) {
	a, err := scanAgent(s.pool.QueryRow(ctx, `SELECT `+agentColumns+` FROM agents WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Agent{}, fmt.Errorf("%w: agent %s", store.ErrNotFound, id)
	}
	if err != nil {
		return store.Agent{}, fmt.Errorf("postgres: read agent %s: %w", id, err)
	}
	return a, nil
}

// AgentByName reads the row the unique (owner, name) constraint keys.
func (s *Store) AgentByName(ctx context.Context, owner, name string) (store.Agent, error) {
	a, err := scanAgent(s.pool.QueryRow(ctx, `SELECT `+agentColumns+` FROM agents WHERE owner = $1 AND name = $2`, owner, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Agent{}, fmt.Errorf("%w: agent %s", store.ErrNotFound, name)
	}
	if err != nil {
		return store.Agent{}, fmt.Errorf("postgres: read agent %s: %w", name, err)
	}
	return a, nil
}

func (s *Store) ListAgents(ctx context.Context, o store.AgentList) ([]store.Agent, string, error) {
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
	rows, err := s.pool.Query(ctx, `SELECT `+agentColumns+` FROM agents
		WHERE id > $1 AND (cardinality($2::text[]) = 0 OR owner = ANY($2::text[]))
		ORDER BY id LIMIT $3`, after, owners, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list agents: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.Agent, error) { return scanAgent(row) })
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list agents: %w", err)
	}
	if len(out) > limit {
		return out[:limit], store.Cursor(out[limit-1].ID), nil
	}
	return out, "", nil
}

const versionColumns = `agent_id, version, digest, doc, bundle, created_by, created_at`

func scanVersion(row pgx.Row) (store.AgentVersion, error) {
	var v store.AgentVersion
	var doc, bundle string
	if err := row.Scan(&v.AgentID, &v.Version, &v.Digest, &doc, &bundle, &v.CreatedBy, &v.CreatedAt); err != nil {
		return store.AgentVersion{}, err
	}
	v.Doc, v.Bundle, v.CreatedAt = []byte(doc), []byte(bundle), v.CreatedAt.UTC()
	return v, nil
}

func (s *Store) Version(ctx context.Context, id string, version int) (store.AgentVersion, error) {
	v, err := scanVersion(s.pool.QueryRow(ctx, `SELECT `+versionColumns+` FROM agent_versions WHERE agent_id = $1 AND version = $2`, id, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.AgentVersion{}, fmt.Errorf("%w: agent %s version %d", store.ErrNotFound, id, version)
	}
	if err != nil {
		return store.AgentVersion{}, fmt.Errorf("postgres: read agent %s version %d: %w", id, version, err)
	}
	return v, nil
}

func (s *Store) Versions(ctx context.Context, id string, limit int, cursor string) ([]store.AgentVersion, string, error) {
	after, err := store.UncursorVersion(cursor)
	if err != nil {
		return nil, "", err
	}
	var one int
	err = s.pool.QueryRow(ctx, `SELECT 1 FROM agents WHERE id = $1`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", fmt.Errorf("%w: agent %s", store.ErrNotFound, id)
	}
	if err != nil {
		return nil, "", fmt.Errorf("postgres: read agent %s: %w", id, err)
	}
	if limit <= 0 {
		limit = session.DefaultListLimit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+versionColumns+` FROM agent_versions
		WHERE agent_id = $1 AND version > $2 ORDER BY version LIMIT $3`, id, after, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list the versions of %s: %w", id, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.AgentVersion, error) { return scanVersion(row) })
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list the versions of %s: %w", id, err)
	}
	if len(out) > limit {
		return out[:limit], store.Cursor(strconv.Itoa(out[limit-1].Version)), nil
	}
	return out, "", nil
}

// PutVersion stores a version in one transaction: version 1 inserts the
// agent and the version, a unique violation on the id or on the owner's
// name being ErrConflict; a later version locks the agent's row, checks it follows
// the latest, inserts the version and moves the latest.
func (s *Store) PutVersion(ctx context.Context, a store.Agent, v store.AgentVersion) error {
	if err := store.CheckVersion(a, v); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if v.Version == 1 {
			_, err := tx.Exec(ctx, `INSERT INTO agents (`+agentColumns+`) VALUES ($1, $2, $3, 1, $4, $5)`,
				a.ID, a.Name, a.Owner, a.ArchivedAt, a.CreatedAt)
			if isUnique(err) {
				return fmt.Errorf("%w: agent %s exists", store.ErrConflict, a.Name)
			}
			if err != nil {
				return fmt.Errorf("postgres: create agent %s: %w", a.Name, err)
			}
		} else {
			var latest int
			err := tx.QueryRow(ctx, `SELECT latest_version FROM agents WHERE id = $1 FOR UPDATE`, v.AgentID).Scan(&latest)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && latest+1 != v.Version {
				return fmt.Errorf("%w: agent %s version %d does not follow the stored latest", store.ErrConflict, v.AgentID, v.Version)
			}
			if err != nil {
				return fmt.Errorf("postgres: lock agent %s: %w", v.AgentID, err)
			}
			if _, err := tx.Exec(ctx, `UPDATE agents SET latest_version = $2 WHERE id = $1`, v.AgentID, v.Version); err != nil {
				return fmt.Errorf("postgres: move the latest version of %s: %w", v.AgentID, err)
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO agent_versions (`+versionColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			v.AgentID, v.Version, v.Digest, string(v.Doc), string(v.Bundle), v.CreatedBy, v.CreatedAt)
		if isUnique(err) {
			return fmt.Errorf("%w: agent %s version %d is stored", store.ErrConflict, v.AgentID, v.Version)
		}
		if err != nil {
			return fmt.Errorf("postgres: store agent %s version %d: %w", v.AgentID, v.Version, err)
		}
		return nil
	})
}

func (s *Store) Archive(ctx context.Context, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE agents SET archived_at = $2 WHERE id = $1 AND archived_at IS NULL`, id, at.UTC())
	if err != nil {
		return fmt.Errorf("postgres: archive agent %s: %w", id, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Agents are never deleted, so a row that exists now existed at the
	// update, archived.
	if _, err := s.Agent(ctx, id); err != nil {
		return err
	}
	return fmt.Errorf("%w: agent %s is archived", store.ErrConflict, id)
}

const idempotencyColumns = `subject, key, route, body_hash, done, status, content_type, body, expires_at`

func scanIdempotency(row pgx.Row) (store.Idempotency, error) {
	var r store.Idempotency
	if err := row.Scan(&r.Subject, &r.Key, &r.Route, &r.BodyHash, &r.Done, &r.Status, &r.ContentType, &r.Body, &r.ExpiresAt); err != nil {
		return store.Idempotency{}, err
	}
	r.ExpiresAt = r.ExpiresAt.UTC()
	return r, nil
}

// reserveAttempts bounds how often Begin inserts again after the held
// record it lost the insert to was abandoned before it could read it.
const reserveAttempts = 3

// Begin reserves the key in one transaction: an insert that does nothing
// when the key is held, then a read of the held record under its row
// lock, replaced when it has expired. Replicas that race on one key
// reserve it once.
func (s *Store) Begin(ctx context.Context, r store.Idempotency) (store.Idempotency, bool, error) {
	r.Done = false
	var out store.Idempotency
	var fresh bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for range reserveAttempts {
			tag, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (`+idempotencyColumns+`)
				VALUES ($1, $2, $3, $4, false, $5, $6, $7, $8) ON CONFLICT (subject, key) DO NOTHING`,
				r.Subject, r.Key, r.Route, r.BodyHash, r.Status, r.ContentType, r.Body, r.ExpiresAt)
			if err != nil {
				return fmt.Errorf("postgres: reserve an idempotency key: %w", err)
			}
			if tag.RowsAffected() == 1 {
				out, fresh = r, true
				return nil
			}
			held, err := scanIdempotency(tx.QueryRow(ctx, `SELECT `+idempotencyColumns+` FROM idempotency_keys
				WHERE subject = $1 AND key = $2 FOR UPDATE`, r.Subject, r.Key))
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("postgres: read an idempotency record: %w", err)
			}
			if s.now().Before(held.ExpiresAt) {
				out = held
				return nil
			}
			if _, err := tx.Exec(ctx, `UPDATE idempotency_keys SET route = $3, body_hash = $4, done = false, status = $5,
				content_type = $6, body = $7, expires_at = $8 WHERE subject = $1 AND key = $2`,
				r.Subject, r.Key, r.Route, r.BodyHash, r.Status, r.ContentType, r.Body, r.ExpiresAt); err != nil {
				return fmt.Errorf("postgres: replace an expired idempotency record: %w", err)
			}
			out, fresh = r, true
			return nil
		}
		return fmt.Errorf("postgres: the idempotency record was abandoned %d times while it was reserved", reserveAttempts)
	})
	if err != nil {
		return store.Idempotency{}, false, err
	}
	return out, fresh, nil
}

func (s *Store) Finish(ctx context.Context, r store.Idempotency) error {
	tag, err := s.pool.Exec(ctx, `UPDATE idempotency_keys SET route = $3, body_hash = $4, done = true, status = $5,
		content_type = $6, body = $7, expires_at = $8 WHERE subject = $1 AND key = $2`,
		r.Subject, r.Key, r.Route, r.BodyHash, r.Status, r.ContentType, r.Body, r.ExpiresAt)
	if err != nil {
		return fmt.Errorf("postgres: finish an idempotency record: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no reserved record for the key", store.ErrNotFound)
	}
	return nil
}

func (s *Store) Abandon(ctx context.Context, subject, key string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE subject = $1 AND key = $2`, subject, key); err != nil {
		return fmt.Errorf("postgres: abandon an idempotency record: %w", err)
	}
	return nil
}

var _ store.Store = (*Store)(nil)
