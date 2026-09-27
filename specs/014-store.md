---
title: "The server's store: the Postgres schema, toposd on the directory store, the blob store, retention and deletion"
status: validated
track: core
depends_on: [002-scaffold-and-configuration.md, 004-session-log.md, 006-identity.md]
affects: [internal/store/postgres/, internal/store/dir/, internal/blob/, internal/serve/]
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
| `agent_versions` | `(agent_id, version)` | `digest`, the resolved document and the bundle of [[003-manifest]] as JSON text, `created_by`, `created_at` |
| `sessions` | `id` | the columns a query filters on (`agent_id`, `agent_version`, `owner`, `runner`, `status`, `stop_reason`, `turn`, `last_seq`, `created_at`, `updated_at`, `expires_at`, `ended_at`), the Session object as JSON text, and the queue columns of [[016-runners]]: `wake`, `lease_holder`, `lease_generation`, `lease_expires_at`, `writer_kind`, `writer_subject` |
| `events` | `(session_id, seq)`, unique `(session_id, id)` | `id`, `type`, `time`, `thread`, `turn`, `step`, `payload` as JSON text, `redacted` |
| `blobs` | `(session_id, digest)` | `size`, `location` (`db` or `object`), and `body` when `location` is `db` |
| `triggers` | `id` | `name`, `owner`, `agent_id`, the resolved spec, `next_fire_at`, `last_fired_at`, `last_session_id`, `suspended` ([[022-triggers]]) |
| `trigger_firings` | `(trigger_id, scheduled_at)` | the replica that claimed the firing, the outcome (`started`, `skipped_active`, `skipped_late`, `refused`) and the session it started, so one firing starts one session across replicas ([[022-triggers]]) |
| `credentials` | `id` | `name`, `owner`, `service`, the wrapped data key, the key index, nonce and ciphertext ([[018-credentials-and-secrets]]) |
| `memory_stores` | `id` | `name`, `owner`, `description`, `created_at`; documents live in the memory backend ([[020-memory-stores]]) |
| `idempotency_keys` | `(subject, key)` | the route, the body's hash, `done`, the stored answer (`status`, `content_type`, `body` as bytes), `expires_at` ([[015-api]]) |
| `sink_outbox` | `id` | a sink event awaiting delivery ([[023-events-and-observability]]) |

`toposd serve` applies the migrations at start on `TOPOS_DB_URL`
through `pgxmigrate` (golang-migrate's pgx v5 driver, `pgx5://`); the
store in `internal/store/postgres` does so when it opens. The first
migration holds `sessions`, `events` and `blobs`; each other table joins
with the spec that uses it, and a replica that finds a newer schema than it
knows refuses to start. Serving queries use `TOPOS_DB_POOL_URL` when it
is set, in pgx's describe-cached execution mode so a
transaction-pooling proxy accepts them, with JSON bound as text;
`LISTEN` and the migrations use `TOPOS_DB_URL`.

A session's `owner` is its initiator's subject, and `sessions` keeps
an index on `(owner, id)` so a list narrowed to its owners
([[006-identity]]) walks it newest first.

### Agents and idempotency on Postgres

`PutVersion` is one transaction. Version 1 inserts the `agents` row
and the version; a unique violation on the id or the name is
`ErrConflict`. A later version locks the agent's row with
`SELECT ... FOR UPDATE`, is `ErrConflict` unless it follows
`latest_version`, and inserts the version and moves `latest_version`
before the lock is released, so replicas racing one version store it
once. `Begin` is one transaction too: `INSERT ... ON CONFLICT DO
NOTHING` reserves a free key; otherwise the held row is read under
its lock, answered while it is unexpired, and replaced when it has
expired, so replicas racing one key reserve it once. Expiry is judged
on the clock of `toposd`, which set `expires_at`, not the database's.

### Append on Postgres

An append is one transaction: the session row is locked with
`SELECT ... FOR UPDATE`; an append through a lease carries its
generation and is refused `lease_lost` once the row's generation has
moved on ([[016-runners]]), while an append outside any lease, a
person's event through the API, is not fenced; the writer subject of an
external writer is [[017-external-runners-handoff-fork]]'s check; a
batch whose `after_seq` is the row's `last_seq` is inserted, and the row's
`last_seq`, `status`, `stop_reason`, `turn` and `updated_at` are set
from it; then `NOTIFY topos_events` with the session id. A batch whose
`after_seq` is behind and whose events match, by id and content, the
rows at those sequences answers success with the same last sequence;
any other is `sequence_conflict` ([[004-session-log]]).

`Watch` replays from the table, then reads again each time the store's
listener announces an append to the session, with a 2 second poll as
the fallback for a lost notification. A store holds one listener: a
connection on `TOPOS_DB_URL` that runs `LISTEN topos_events` from the
first `Watch` until the store closes, however many watches are open.
It routes each notification to the watches of the session its payload
names, and a watch of another session does not wake. A watch's wake is
a signal of one slot, so a burst of appends while it reads coalesces
into one more read and a slow watch never holds the listener back. A
dropped listener connection is dialed again with backoff from 200 ms
to 10 s for as long as the store is open, and once it listens again
every watch reads once, so none misses an append made while it was
down; meanwhile the watches poll.

### Connections on Postgres

A replica's connections to Postgres are its serving pool, its
listener, and its migration:

| Connection | Endpoint | Count | Held |
|---|---|---|---|
| serving pool | `TOPOS_DB_POOL_URL`, or `TOPOS_DB_URL` without it | the DSN's `pool_max_conns`, or else the greater of 4 and the CPUs the process sees | opened on demand, each for one query or transaction |
| listener | `TOPOS_DB_URL` | 1 | from the first `Watch` until the store closes |
| migration | `TOPOS_DB_URL` | 1 | at start, closed by `pgxmigrate` before the pool opens |

Watches and streams open no connection of their own: fifty watches on
one store with an 18-connection pool hold at most 19 backends. On
`TOPOS_DB_URL` alone a replica holds at most the pool plus one; behind
a transaction-pooling proxy it holds one direct connection, the
listener, and the pool's connections are the proxy's clients. N
replicas hold N times that, and a rolling deploy adds one replica's
share while the old and the new overlap.

### toposd on the directory store

With `TOPOS_DB_URL` unset, sessions live in `session/dir` under
`$TOPOS_DATA_DIR/sessions/`, and the other objects as one JSON file per
object under `$TOPOS_DATA_DIR/objects/<kind>/<id>.json`, each replaced
atomically (write, fsync, rename, fsync the directory):

```
objects/agent/<agent_id>.json              the agent: name, owner, latest version, archive time
objects/agent_version/<agent_id>/<n>.json  version n: digest, the resolved document and the bundle as text
objects/idempotency/<hex>.json             an idempotency record; hex is the SHA-256 of its subject and key
```

An agent's file is the commit record of its versions: a version's
file is written before its agent's, and a reader opens only the
versions the agent's latest version counts. A crash between the two
writes leaves a version file nothing reads, which the next write of
that version replaces, and never an agent whose latest version is
missing. The name index is derived from the agent files when the
store opens, so no index file can disagree with them. A file whose
name is not an object's, such as the temporary file of an interrupted
write, is ignored, and a well-named file that does not decode refuses
the open with `ErrCorrupt`; the open deletes nothing. The queue and
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
| `session/storetest` passes on the Postgres store and on the directory store, the Postgres tier against `DATABASE_URL` or a container it starts | `internal/store/postgres.TestPostgresStoreConformance` (tag `postgres`), `session/dir.TestDirStoreConformance` | built |
| Two replicas appending to one session through Postgres keep a dense sequence, and the one that is not the writer gets `sequence_conflict` | `internal/store/postgres.TestPostgresTwoReplicasOneWriter` (tag `postgres`) | built |
| A `Watch` on one store instance sees an event another instance appends through `NOTIFY`, with the poll an hour away | `internal/store/postgres.TestWatchSeesAnotherStoresAppends` (tag `postgres`) | built |
| A `Watch` sees an event within the poll interval with notifications dropped | `internal/store/postgres.TestPostgresWatchAcrossReplicas` (tag `postgres`), an append written with no `NOTIFY` | built |
| Fifty watches on one store hold one listener connection beside the pool, and each sees an append to its session through it | `internal/store/postgres.TestManyWatchesShareOneListener` (tag `postgres`) | built |
| An append to one session wakes its watch and not the watch of another | `internal/store/postgres.TestANotificationWakesOnlyItsSessionsWatchers` (tag `postgres`) | built |
| A listener whose backend is terminated connects again, and every watch sees the appends made while it was down and after, with the poll an hour away | `internal/store/postgres.TestADroppedListenerReconnectsAndMissesNothing` (tag `postgres`) | built |
| Closing the store closes its listener connection and every open watch | `internal/store/postgres.TestAClosedStoreClosesItsWatches` (tag `postgres`) | built |
| A second `toposd serve` on the same data directory refuses to start | `internal/store/dir.TestASecondServeIsRefusedWithThePidOfTheFirst`, `cmd/toposd.TestServeAnswersTheAPIAsASelfHoster` | built |
| A blob in the database is readable by digest, and one whose bytes no longer match answers `ErrCorrupt` | `internal/store/postgres.TestACorruptBlobIsRefused` (tag `postgres`) | built |
| A blob is readable by digest from the `file://` and `s3://` locations | `TestBlobStoreLocations` | not built |
| Deleting a session removes its rows and every blob object; a crash between the two leaves no session without its blobs, and the reaper removes the orphans | `TestSessionDeletionOrder` | not built |
| A session past `expires_at` is ended `expired`, and an ended one past its retention is deleted | `TestReaperExpiresAndDeletes` | not built |
| Migrations apply on an empty database when the store opens | `internal/store/postgres.TestPostgresStoreConformance` (tag `postgres`, every subtest opens a fresh database) | built |
| A lease that expires is taken over by the next holder, and the old holder's renew fails and its `Lost` closes | `internal/store/postgres.TestAnExpiredLeaseIsTakenOverAndTheOldHolderLosesIt` (tag `postgres`) | built |
| A replica that finds a newer schema than it knows refuses to start | `internal/store/postgres.TestMigrationsAtStart` (tag `postgres`), with `cmd/toposd.TestServeStopsOnAStoreItCannotOpen` for the start | built |
| The suite of `store.Store` (`internal/store/storetest`) passes on the memory store, the directory store and the Postgres store | `internal/store.TestMemoryStoreConformance`, `internal/store/dir.TestObjectStoreConformance`, `internal/store/postgres.TestPostgresObjectStoreConformance` (tag `postgres`) | built |
| Replicas racing one idempotency key reserve it once, new or expired, and replicas racing one agent version or one new agent name store it once | `internal/store/postgres.TestBeginReservesAKeyOnceAcrossReplicas`, `internal/store/postgres.TestPutVersionHasOneWinnerAcrossReplicas` (tag `postgres`) | built |
| The directory store reads back every agent, version, name and idempotency record after a restart, and ignores a torn temporary file | `internal/store/dir.TestReopenReadsEverythingBack`, `internal/store/dir.TestATornTemporaryFileIsIgnoredAtOpen` | built |
| A crash between a version's file and its agent's leaves the version unread and replaceable, and never an agent whose latest version is missing | `internal/store/dir.TestAVersionIsVisibleOnlyOnceItsAgentCountsIt` | built |
| A well-named object file that does not decode, or holds another object, is `ErrCorrupt` | `internal/store/dir.TestOpenRefusesACorruptAgent`, `internal/store/dir.TestACorruptVersionIsRefused`, `internal/store/dir.TestACorruptIdempotencyRecordIsRefused` | built |
