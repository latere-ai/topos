---
title: "The server's store: the Postgres schema, toposd on the directory store, the blob store, retention and deletion"
status: drafted
track: core
depends_on: [002-scaffold-and-configuration.md, 004-session-log.md, 006-identity.md]
affects: [internal/store/postgres/, internal/store/dirobjects/, internal/blob/, internal/serve/]
effort: medium
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# The server's store

## Overview

`toposd` keeps session logs, agent versions, triggers, credentials,
memory store records, and the blobs beside the logs. With
`TOPOS_DB_URL` set it keeps them in Postgres and any number of replicas
share them; without it, one `toposd` keeps them on the directory store
of [[004-session-log]] under `TOPOS_DATA_DIR`. Raw response bodies and
captured requests go to the blob store: in the store itself by default,
or a directory or S3-compatible object storage named by
`TOPOS_BLOB_URL`. Both stores implement `session.Store` and pass
`session/storetest`. This spec owns the schema, the modes, the blob
store and the rules for retention and deletion.

## Current state

v0.7.0 kept nothing. The retired hosted service kept an append-only
`session_events` table keyed by `(session_id, seq)` beside session
snapshots of untagged Go structs, and a row lease on the session row;
the events table's key and the lease columns are borrowed, the
snapshots are not. Migrations follow the family's shared
`latere.ai/x/pkg/pgxmigrate`.

## Design

### The Postgres schema

| Table | Key | Holds |
|---|---|---|
| `agents` | `id` | `name`, `owner`, `latest_version`, `archived_at`, `created_at` |
| `agent_versions` | `(agent_id, version)` | `digest`, the resolved spec as JSON text, `created_by`, `created_at` |
| `sessions` | `id` | the columns a query filters on (`agent_id`, `agent_version`, `owner`, `runner`, `status`, `stop_reason`, `turn`, `last_seq`, `created_at`, `updated_at`, `expires_at`, `ended_at`), the Session object as JSON text, and the queue columns of [[016-runners]]: `wake`, `lease_holder`, `lease_generation`, `lease_expires_at`, `writer_kind`, `writer_subject` |
| `events` | `(session_id, seq)`, unique `(session_id, id)` | `id`, `type`, `time`, `thread`, `turn`, `step`, `payload` as JSON text, `redacted` |
| `blobs` | `(session_id, digest)` | `size`, `location` (`db` or `object`), and `body` when `location` is `db` |
| `triggers` | `id` | `name`, `owner`, `agent_id`, the resolved spec, `next_fire_at`, `last_fired_at`, `last_session_id`, `suspended` ([[022-triggers]]) |
| `trigger_firings` | `(trigger_id, scheduled_at)` | the replica that claimed the firing, the outcome (`started`, `skipped_active`, `skipped_late`, `refused`) and the session it started, so one firing starts one session across replicas ([[022-triggers]]) |
| `credentials` | `id` | `name`, `owner`, `service`, the wrapped data key, the key index, nonce and ciphertext ([[018-credentials-and-secrets]]) |
| `memory_stores` | `id` | `name`, `owner`, `description`, `created_at`; documents live in the memory backend ([[020-memory-stores]]) |
| `idempotency_keys` | `(subject, key)` | the route, the body's hash, the stored answer, `expires_at` ([[015-api]]) |
| `sink_outbox` | `id` | a sink event awaiting delivery ([[023-events-and-observability]]) |

`toposd serve` applies the migrations at start on `TOPOS_DB_URL`
through `pgxmigrate`, and a replica that finds a newer schema than it
knows refuses to start. Serving queries use `TOPOS_DB_POOL_URL` when it
is set, in pgx's describe-cached execution mode so a
transaction-pooling proxy accepts them, with JSON bound as text;
`LISTEN` and the migrations use `TOPOS_DB_URL`.

### Append on Postgres

An append is one transaction: the session row is locked with
`SELECT ... FOR UPDATE`; the writer rule is checked (the lease
generation for a hosted writer, the writer subject for an external
one, [[016-runners]], [[017-external-runners-handoff-fork]]); a batch
whose `after_seq` is the row's `last_seq` is inserted, and the row's
`last_seq`, `status`, `stop_reason`, `turn` and `updated_at` are set
from it; then `NOTIFY topos_events` with the session id. A batch whose
`after_seq` is behind and whose events match, by id and content, the
rows at those sequences answers success with the same last sequence;
any other is `sequence_conflict` ([[004-session-log]]). `Watch`
replays from the table, then follows `LISTEN topos_events`, with a
2 second poll as the fallback for a lost notification.

### toposd on the directory store

With `TOPOS_DB_URL` unset, sessions live in `session/dir` under
`$TOPOS_DATA_DIR/sessions/`, and the other objects as one JSON file per
object under `$TOPOS_DATA_DIR/objects/<kind>/<id>.json`, each replaced
atomically (write, fsync, rename, fsync the directory). The queue and
the leases of [[016-runners]] live in the process. The mode is one
replica: `toposd serve` holds `flock` on `$TOPOS_DATA_DIR/serve.lock`,
and a second `serve` on the same directory refuses to start with
`data directory in use by pid <n>`. The start-up log says the store is
a directory and names it.

### The blob store

| `TOPOS_BLOB_URL` | Blobs are kept |
|---|---|
| unset | in the store: the `blobs.body` column on Postgres, `blobs/sha256/` in the session's directory on the directory store |
| `file:///<path>` | as `<path>/<session>/<hex>`, written to a temporary name, fsynced and renamed |
| `s3://<host>/<bucket>/<prefix>` | as the object `<prefix>/<session>/<hex>` through `latere.ai/x/pkg/s3`, path-style over TLS, with `TOPOS_BLOB_ACCESS_KEY` and `TOPOS_BLOB_SECRET_KEY` ([[002-scaffold-and-configuration]]) |

A blob is written and its `blobs` row inserted before the event that
names it is appended. A read verifies the bytes against the digest and
answers `ErrCorrupt` when they differ.

### Retention and deletion

A session is deleted by `DELETE /v1/sessions/{id}` ([[015-api]]) or
at the end of its retention: the `retention` of the authorizer's
`limits` at create ([[006-identity]]), recorded on the session; with
none, an ended session is kept until someone deletes it, which is the
self-hoster's default. A reaper in `serve` runs every 10 minutes: it
ends `expired` every session past `expires_at`, and deletes every
ended session past its retention. Deleting a session removes its row,
its events and its blobs, the objects under `<prefix>/<session>/`
included, in that order, so a crash midway leaves blobs without a
session, which the next reaper run removes, and never a session
without its blobs. Redaction deletes only the blobs the redacted event
alone named ([[004-session-log]]). An agent is archived, never
deleted, while a session pins one of its versions.

## Not in this spec

The Session and Event schema and the directory store's layout
([[004-session-log]]); the queue's claim and lease rules
([[016-runners]]); credential encryption ([[018-credentials-and-secrets]]);
memory documents ([[020-memory-stores]]); the routes ([[015-api]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `session/storetest` passes on the Postgres store and on the directory store, the Postgres tier through Testcontainers | `TestPostgresStoreConformance`, and `TestDirStoreConformance` of [[004-session-log]] | not built |
| Two replicas appending to one session through Postgres keep a dense sequence, and the one that is not the writer gets `sequence_conflict` | `TestPostgresTwoReplicasOneWriter` | not built |
| A `Watch` on one replica sees an event appended on another within one second, and within the poll interval with notifications dropped | `TestPostgresWatchAcrossReplicas` | not built |
| A second `toposd serve` on the same data directory refuses to start | `TestDirModeIsOneReplica` | not built |
| A blob is readable by digest from each of the three blob locations, and a corrupted object answers `ErrCorrupt` | `TestBlobStoreLocations` | not built |
| Deleting a session removes its rows and every blob object; a crash between the two leaves no session without its blobs, and the reaper removes the orphans | `TestSessionDeletionOrder` | not built |
| A session past `expires_at` is ended `expired`, and an ended one past its retention is deleted | `TestReaperExpiresAndDeletes` | not built |
| Migrations apply on an empty database at start, and a replica that finds a newer schema refuses to start | `TestMigrationsAtStart` | not built |
