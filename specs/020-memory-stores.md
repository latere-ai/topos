---
title: "Memory stores: the resource, attachment, the directory and Arca backends, sync into the machine, preconditions and conflicts"
status: drafted
track: core
depends_on: [002-scaffold-and-configuration.md, 003-manifest.md, 004-session-log.md, 008-tools.md, 009-machines.md, 018-credentials-and-secrets.md]
affects: [memory/, memory/dir/, memory/arca/, runner/, harness/tools/]
effort: medium
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Memory stores

## Overview

Memory is documents an agent reads and writes across sessions. A
memory store is a resource of the API with an id, a name, a
description written for the model, and the versions of each document.
An agent names the stores it uses; a session attaches them, read-write
or read-only. The runner syncs each attached store into a directory on
the session's machine, so the model reads and writes memory with the
ordinary file tools and no machine holds a storage credential. The
bytes live in a directory for a self-hoster, or under a prefix of
Arca's files plane. Every write carries a version precondition, so a
sync never overwrites a change it has not seen, and memory sync is the
one built-in operation a resumed runner repeats.

## Current state

v0.7.0 had no memory, and the retired hosted service had none. The
storage placement follows Arca's own design, which puts agent memory
under the files plane's `files/memory/` prefix with a version on every
file and a checksum precondition on every write. An Arca workspace is
not used: it admits one writer at a time and keeps no versions. A
retrieval plane over memory is a separate component and not this spec.
Arca does not yet export a client package; `latere.ai/x/arca/client`
is a prerequisite of the Arca backend, and the directory backend does
not wait for it.

## Design

### The resource

| Field | Meaning |
|---|---|
| `id` | `mem_…` ([[004-session-log]]) |
| `name`, `description` | from the MemoryStore kind ([[003-manifest]]); the description is what the model is told the store holds |
| `owner`, `created_at` | bookkeeping |
| documents | each has a `path` (relative, `/` separated, no `..` segment, at most 512 bytes), a `version`, a `sha256`, a `size` (at most 1 MiB), `updated_at` and `updated_by` (the session that wrote it) |
| versions | every earlier version of a document stays readable until the store is deleted |

A store holds at most 64 MiB of current documents. The routes that list
stores, documents and versions are [[015-api]]'s.

### Partitions

An organization agent serves many people, and each session is capped
by the person who started it. One memory shared by all of them would
carry what one person's session read into another's, past that cap,
and would keep an instruction injected into one session for every
later one. A store is therefore partitioned by initiator unless it is
shared.

| `sharing` | What a session reads and writes |
|---|---|
| `initiator` (default) | the partition of the session's initiator: its own documents, invisible to any other initiator's sessions. A triggered session's initiator is the trigger's owner ([[022-triggers]]) |
| `shared` | one partition for the store's declared `audience`. Setting it is `memory_store.update` with the `sharing` and `audience` in the resource, which the authorizer allows, by default, to an organization admin; the admin accepts that any member's session may write what every member's reads. A session whose initiator is outside the audience cannot attach the store |

A personal agent's sessions all have its owner as initiator, so its
store has one partition in effect. A partition is keyed by the
lowercase hex SHA-256 of the initiator's subject, so no subject's
characters reach a path. Every write to a shared store carries the
session and its initiator in `updated_by`, and every earlier version
stays readable, so a bad note is found by its writer and reverted by
writing an earlier version back. The note rendered for a shared store
says its documents come from other people's sessions
([[011-instructions-and-skills]]); that is a hint to the model, not a
boundary. The routes over documents answer the caller's own partition,
and any partition to a caller the authorizer allows
`memory_store.read` with the `partition` in the resource.

### Attachment

An agent's `spec.memoryStores` names stores and their access
([[003-manifest]]); a session attaches them as resource entries
`{"type":"memory_store","memory_store_id","access"}` with `access`
`read_write` or `read_only` ([[004-session-log]]). An attachment has no
id of its own. A session may narrow an attachment to `read_only` and
never widen it past the agent's. The runner appends `memory.attached`
when it mirrors a store into the machine, and
[[011-instructions-and-skills]] renders what the model is told about it.

### Backends

| Backend | `TOPOS_MEMORY_BACKEND` | Where documents live |
|---|---|---|
| directory | `dir` (default) | `TOPOS_MEMORY_DIR/<mem_id>/<partition>/`, a document per file and its versions under `.versions/<path>/<version>` |
| Arca | `arca` | the files plane at `TOPOS_MEMORY_ARCA_URL`, under the prefix `files/memory/<mem_id>/<partition>/`, with Arca's own version per file and its checksum precondition per write |

`<partition>` is the initiator's key or `shared`.

The prefix is keyed by the store's id, not its name, so a renamed store
keeps its documents. Both backends implement `memory.Backend`: `List`,
`Get(path, version)`, `Put(path, body, ifVersion)` and
`Delete(path, ifVersion)`, where `ifVersion` is the precondition and a
mismatch is `ErrConflict`. The runner reaches the backend with the
session's credential ([[018-credentials-and-secrets]]); no machine
holds one.

### Sync into the machine

The runner mirrors each attached store into a directory on the machine:
`/topos/memory/<name>/` in a Cella sandbox, and
`$TOPOS_DATA_DIR/memory-mounts/<session>/<name>/` on the host, which is
one of the host machine's roots ([[009-machines]]). A manifest of the
versions last synced, the sync base, is kept on the machine outside the
mirrored directory. A sync compares three states per path: the base,
the machine's file, and the backend's current version.

| Path | Sync does |
|---|---|
| changed on the machine only | `Put` with the base version as precondition |
| changed in the backend only | writes the new version to the machine |
| deleted on the machine only | `Delete` with the base version as precondition |
| changed on both | a conflict: the backend's version wins the path; the machine's copy is kept as `<path>.conflict-<session>` and pushed at the next sync; the path is listed in `memory.synced`'s `conflicts` |
| a `read_only` store | machine changes are never pushed; the backend's version wins |

Sync never overwrites a version it has not seen. Each sync appends one
`memory.synced` with the paths pushed, pulled, deleted and in conflict,
and never their content ([[004-session-log]]).

Sync points: when a turn starts, when it ends (before the checkpoint),
every `TOPOS_MEMORY_SYNC_INTERVAL` during a turn, and when the model
calls `memory_sync`.

### The memory_sync tool

| Property | Value |
|---|---|
| input | `store` (optional; every attached store when absent) |
| result | the pushed, pulled and conflicting paths |
| `Parallel` | no |
| `Effect` | `write` |
| `Repeatable` | yes, the one built-in that is ([[008-tools]]) |

It is repeatable because every write carries a precondition: running it
again after a runner stopped mid-sync pushes only what the backend
does not already hold and turns anything else into a conflict. A
resumed runner repeats a `memory_sync` call with no result
([[016-runners]]).

### A client's own copy

A client may keep its own copy of a store for a person's other
sessions, and the `topos` command does, under `TOPOS_MEMORY_DIR`. That
copy is a cache of the resource and syncs by the same rule; the
resource is the record.

### Error codes

| Code | Meaning |
|---|---|
| `memory_unavailable` | the backend could not be reached; the turn continues on the machine's copy and the next sync retries |
| `memory_store_full` | a push would pass the store's 64 MiB or a document's 1 MiB |

## Not in this spec

The routes over stores, documents and versions ([[015-api]]); the
fields of the MemoryStore kind ([[003-manifest]]); the note rendered
for the model ([[011-instructions-and-skills]]); a retrieval plane over
memory, which is a separate component.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A note written in a local session on one device is read by the same agent in a cloud session and in a local session on a second device | `TestMemoryFollowsTheAgent` in the e2e tier, with the Arca tier when the Arca client exists | not built |
| A path changed on both sides keeps both versions, the backend's at the path and the machine's as a conflict copy, and is listed in `conflicts` | `TestSyncConflictKeepsBoth` | not built |
| A write whose precondition fails never overwrites the backend's version | `TestPreconditionPreventsOverwrite` on both backends | not built |
| A memory push the backend refuses for spend, `budget_exhausted` or `spend_exceeded`, is a `models.SpendError` and stops the turn `budget` as [[007-models]]'s refusals do | `TestAMemoryPushRefusedForSpendStopsTheTurn` | not built |
| A `memory_sync` call repeated after a runner stopped mid-sync leaves the store as one sync would | `TestMemorySyncIsRepeatable` | not built |
| A `read_only` attachment never pushes a change | `TestReadOnlyAttachmentNeverPushes` | not built |
| `memory.synced` carries paths and never content | `TestMemorySyncedCarriesNoContent` | not built |
| No machine holds a backend credential | `TestNoMemoryCredentialInMachine` | not built |
| Two initiators' sessions of one organization agent never read each other's documents in an `initiator` store; a triggered session reads its owner's partition; a `shared` store is refused to a session whose initiator is outside its audience | `TestMemoryPartitionsByInitiator` | not built |
| A write to a shared store records the session and the initiator in `updated_by`, and writing an earlier version back restores it | `TestSharedStoreWritesAreAttributed` | not built |
