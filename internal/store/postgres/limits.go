// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/session"
)

// The rewrite that renames spec.threads to spec.subagentLimits (spec
// 013): its name in data_rewrites, the advisory lock the servers that
// open the database at once take for it, and how many sessions one read
// of the sessions table holds.
const (
	rewriteSubagentLimits = "subagent_limits"
	rewriteLock           = int64(0x746f706f73_0013)
	rewritePage           = 200
)

// renameSubagentLimits renames spec.threads to spec.subagentLimits in
// what the database kept before it, once, in one transaction under an
// advisory lock: every agent version's document and bundle, with its
// digest the renamed spec's, and every session's agent, whose spec and
// bundle are blobs named by their digests, so the session gets the
// renamed spec and bundle as new blobs and its header names them. The
// blobs before are left, and a session whose header names no bundle runs
// no manifest and is passed by. A document the rename cannot read stays
// as it is and is logged, since it did not read before either; a read or
// a write that fails rolls the whole rewrite back for the next start.
func (s *Store) renameSubagentLimits(ctx context.Context, log *slog.Logger) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, rewriteLock); err != nil {
			return fmt.Errorf("postgres: lock the rename of spec.threads: %w", err)
		}
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM data_rewrites WHERE name = $1)`, rewriteSubagentLimits).Scan(&done); err != nil {
			return fmt.Errorf("postgres: read the rewrites done: %w", err)
		}
		if done {
			return nil
		}
		versions, err := renameVersions(ctx, tx, log)
		if err != nil {
			return err
		}
		sessions, err := s.renameSessionAgents(ctx, tx, log)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO data_rewrites (name, done_at) VALUES ($1, now())`, rewriteSubagentLimits); err != nil {
			return fmt.Errorf("postgres: record the rename of spec.threads: %w", err)
		}
		log.InfoContext(ctx, "renamed spec.threads to spec.subagentLimits", slog.Int("versions", versions), slog.Int("sessions", sessions))
		return nil
	})
}

// renameVersions rewrites every agent version that names spec.threads and
// returns how many it rewrote.
func renameVersions(ctx context.Context, tx pgx.Tx, log *slog.Logger) (int, error) {
	type version struct {
		agent       string
		n           int
		doc, bundle string
	}
	rows, err := tx.Query(ctx, `SELECT agent_id, version, doc, bundle FROM agent_versions ORDER BY agent_id, version`)
	if err != nil {
		return 0, fmt.Errorf("postgres: read the agent versions: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (version, error) {
		var v version
		err := r.Scan(&v.agent, &v.n, &v.doc, &v.bundle)
		return v, err
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: read the agent versions: %w", err)
	}
	renamed := 0
	for _, v := range all {
		doc, derr := manifest.RenameSubagentLimits([]byte(v.doc))
		bundle, berr := manifest.RenameSubagentLimits([]byte(v.bundle))
		if err := errors.Join(derr, berr); err != nil {
			log.WarnContext(ctx, "an agent version does not read, so spec.threads is not renamed in it", slog.String("agent", v.agent), slog.Int("version", v.n), slog.String("error", err.Error()))
			continue
		}
		if !doc.Changed && !bundle.Changed {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_versions SET digest = $3, doc = $4, bundle = $5 WHERE agent_id = $1 AND version = $2`,
			v.agent, v.n, doc.Digest, string(doc.Stream), string(bundle.Stream)); err != nil {
			return 0, fmt.Errorf("postgres: rewrite agent %s version %d: %w", v.agent, v.n, err)
		}
		renamed++
	}
	return renamed, nil
}

// renameSessionAgents gives every session whose agent's bundle names
// spec.threads the renamed spec and bundle, and returns how many it gave
// them. Sessions of one agent version share a bundle, which is renamed
// once. A session whose bundle the database does not hold, or holds
// outside where this store reaches, is logged and passed by, since every
// start would fail it alike.
func (s *Store) renameSessionAgents(ctx context.Context, tx pgx.Tx, log *slog.Logger) (int, error) {
	type header struct{ id, body string }
	seen := map[session.Digest]*manifest.Renamed{}
	renamed, after := 0, ""
	for {
		rows, err := tx.Query(ctx, `SELECT id, body FROM sessions WHERE id > $1 ORDER BY id LIMIT $2`, after, rewritePage)
		if err != nil {
			return 0, fmt.Errorf("postgres: read the sessions: %w", err)
		}
		page, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (header, error) {
			var h header
			err := r.Scan(&h.id, &h.body)
			return h, err
		})
		if err != nil {
			return 0, fmt.Errorf("postgres: read the sessions: %w", err)
		}
		for _, h := range page {
			var sess session.Session
			if err := json.Unmarshal([]byte(h.body), &sess); err != nil {
				return 0, fmt.Errorf("%w: the header of %s: %w", session.ErrCorrupt, h.id, err)
			}
			if sess.Agent.Bundle == "" {
				continue
			}
			r, ok := seen[sess.Agent.Bundle]
			if !ok {
				b, err := s.blobIn(ctx, tx, h.id, sess.Agent.Bundle)
				if errors.Is(err, session.ErrNotFound) || errors.Is(err, session.ErrCorrupt) {
					// The session lost its bundle before the rename,
					// which another start would not find either; another
					// session of the same bundle may hold it.
					log.WarnContext(ctx, "a session's agent cannot be read, so spec.threads is not renamed in it", slog.String("session", h.id), slog.String("error", err.Error()))
					continue
				}
				if err != nil {
					return 0, err
				}
				got, err := manifest.RenameSubagentLimits(b)
				if err != nil {
					log.WarnContext(ctx, "a session's agent does not read, so spec.threads is not renamed in it", slog.String("session", h.id), slog.String("error", err.Error()))
				}
				r = &got
				seen[sess.Agent.Bundle] = r
			}
			if !r.Changed {
				continue
			}
			bundle := session.DigestOf(r.Stream)
			for d, b := range map[session.Digest][]byte{session.Digest(r.Digest): r.Spec, bundle: r.Stream} {
				if err := s.putBlobIn(ctx, tx, h.id, d, b); err != nil {
					return 0, err
				}
			}
			sess.Agent.Digest, sess.Agent.Bundle = session.Digest(r.Digest), bundle
			body, err := encode(sess)
			if err != nil {
				return 0, err
			}
			if _, err := tx.Exec(ctx, `UPDATE sessions SET body = $2 WHERE id = $1`, h.id, body); err != nil {
				return 0, fmt.Errorf("postgres: rewrite the header of %s: %w", h.id, err)
			}
			renamed++
		}
		if len(page) < rewritePage {
			return renamed, nil
		}
		after = page[len(page)-1].id
	}
}

// blobIn reads a session's blob in tx, from the blob store when the row
// says it is kept outside.
func (s *Store) blobIn(ctx context.Context, tx pgx.Tx, id string, d session.Digest) ([]byte, error) {
	var body []byte
	var location string
	err := tx.QueryRow(ctx, `SELECT location, body FROM blobs WHERE session_id = $1 AND digest = $2`, id, string(d)).Scan(&location, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: blob %s of %s", session.ErrNotFound, d, id)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: read blob %s of %s: %w", d, id, err)
	}
	if location == "object" {
		if s.blobs == nil {
			return nil, fmt.Errorf("%w: blob %s is kept outside the database, and this store is given no blob store", session.ErrCorrupt, d)
		}
		if body, err = s.blobs.GetBlob(ctx, id, d); err != nil {
			return nil, err
		}
	}
	return body, nil
}

// putBlobIn writes a session's blob in tx, its body to the blob store
// first when the store keeps bodies outside, as PutBlob does; a blob the
// session already holds is kept.
func (s *Store) putBlobIn(ctx context.Context, tx pgx.Tx, id string, d session.Digest, b []byte) error {
	if s.blobs != nil {
		if err := s.blobs.PutBlob(ctx, id, d, b); err != nil {
			return err
		}
	}
	return s.insertBlob(ctx, tx, id, d, b, true)
}
