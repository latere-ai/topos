---
title: "Deleting a session: the route asks only about a session it would delete, refuses a running one, and states what a delete removes, when, and what it leaves to the installation"
status: complete
track: core
depends_on: [004-session-log.md, 006-identity.md, 009-machines.md, 014-store.md, 015-api.md, 016-runners.md, 035-hosted-checkpoints-at-the-git-host.md]
affects: [internal/server/, api/]
effort: small
created: 2026-10-05
updated: 2026-10-05
author: changkun
---

# Deleting a session

## Overview

`DELETE /v1/sessions/{id}` removes a session, its log and its blobs
([[014-store]]). A person who deletes a session expects it gone: from
every list and every read, and with it what the session wrote. This
spec fixes the order the route asks the authorizer in, so a decider
that acts on an allowed delete never acts for a delete that does not
happen, and refuses a running session the way an end refuses one. It
then states, for each thing a session holds, whether a delete removes
it, when, and what it leaves to the installation, so a client can say
what deleting means and an installation knows what it must clean.

## Current state

The route asks `session.delete` first and only then calls the store,
whose delete refuses a session whose lease is live with `locked`, which
the API answers as `conflict`. An authorizer may act on the allow
before it answers, as one acts on `session.end` by revoking the
session's credentials ([[015-api]]). For a running session the order is
wrong: the authorizer has cut off the session's credentials, the store
then refuses the delete, and the session still exists and still runs,
now meeting refusals at every call it makes. `POST /v1/sessions/{id}/end`
has the right order: it reads the session with `session.read`, refuses a
running or ended session, and asks `session.end` only of a session it
would end.

What a delete reaches today:

| What | Removed by a delete |
|---|---|
| the session's header, its events, and the blobs kept in the database | yes, in one transaction |
| blob bodies kept outside the database, in an object store or a directory | yes, after the rows; a failure leaves them, and the reaper removes them |
| the working, spill and stage directories of a session on a server's host ([[009-machines]]) | yes, by the `Deleted` hook, when the serving host holds them |
| the session's Cella sandbox, with its workspace, and the Cella Secrets the runner applied for it, its Lux key and its git host token | no |
| the checkpoint ref `refs/topos/checkpoints/<session>/latest` at the git host ([[035-hosted-checkpoints-at-the-git-host]]) | no, by that spec's decision |
| a fork's copy of the session's events and blobs ([[017-external-runners-handoff-fork]]) | no: a fork copies its parent's blobs into its own session, so it reads whole after its parent's delete |

## Design

### The route

`DELETE /v1/sessions/{id}`:

1. reads the session and asks `session.read`; a caller who may not read
   it hears `not_found`, and nothing else is asked;
2. refuses a running session with `conflict`, detail "the session is
   running; interrupt it first", and asks nothing further;
3. asks `session.delete`, with the fields every session action carries
   (`agent`, `owner`, `runner`) and the session's id as the resource's;
   a deny is `forbidden` with the authorizer's reason, and the session
   stays;
4. deletes the session through the store, whose lease check still
   holds: a runner that claimed the session between the read and the
   delete, to drive a message that arrived meanwhile, makes the store
   refuse with `locked`, answered `conflict`, and the session stays;
5. calls the `Deleted` hook, whose failure is logged for the operator
   and does not fail the delete, since the session is gone;
6. answers 204.

An idle session and an ended one are deleted alike. A session whose
status is `running` with no runner holding it, a runner that was lost,
is refused until recovery claims it and closes its turn
([[016-runners]]), which happens within one lease; a client that
interrupts it first sees it idle as soon as a runner reads the
interrupt.

A client deleting a session that runs sends `user.interrupt`, waits
for the session to leave `running`, and sends the delete; a `conflict`
in step 4 is answered the same way, after the session goes idle again.
The route does not interrupt on the client's behalf: an interrupt is
`session.interrupt`, a decision of its own, and a delete that waited
for a runner inside one request would hold the request for as long as
the runner takes to stop a command.

Only the route's order changes. The authorizer is asked `session.delete`
only of a session the route would delete, as `session.end` is asked only
of a session the route would end; between the question and the store,
step 4's race is the only way the allow and the delete part, and an
authorizer that acted on the allow has then acted in the safe
direction, refusing a session that a retry deletes.

### What a delete removes, and when

| What | When it is gone |
|---|---|
| the header, the events and the blobs kept in the database | at the delete, in one transaction; every read of the session answers `not_found` from then on, and the list and the summary leave it out |
| blob bodies kept outside the database | at the delete, after the rows; when that removal fails, by the reaper's sweep, which runs every `ReapInterval` (10 minutes) and removes the bodies of a session that is gone once its id is `SweepGrace` (1 hour) old ([[014-store]]); a crash between the rows and the bodies therefore leaves bodies with no session, never a session without its bodies |
| the directories of a session on a server's host | at the delete, by the serving host; on an installation of several hosts, a host that did not serve the delete keeps the directories of a session it ran and had not ended until an operator removes them, since the directories are removed on the host that holds them |
| the Lux keys the minter holds in memory | never written anywhere; each expires within its lease, and the authorizer's revocation on an allowed delete makes the gateway refuse it at once ([[018-credentials-and-secrets]]) |

The database's own copies of a deleted row, in its write-ahead log and
its backups, follow the database's retention, which the installation
sets; the core states none.

### What a delete leaves to the installation

| What | Why the core cannot remove it | Who does |
|---|---|---|
| the session's Cella sandbox, when the session had not ended in a drive: a sandbox Cella stopped after `AutoStop` keeps its workspace, and lives until its `ttl`, the session's remaining age when it was created, at most the session's `max_age` (168 hours by default) | a hosted session's sandbox is created and deleted with the session's own Cella credential ([[018-credentials-and-secrets]]), and an authorizer that acts on an allowed delete stops honoring that credential in the decision; the server holds no Cella credential of its own when it mints session credentials | the installation's authorizer side: it deletes the sandboxes labeled `topos.latere.ai/session: <session id>` and owned by the session's agent identity once it allowed the delete, as Cella's service account |
| the Cella Secrets the runner applied for the sandbox, its Lux key and its git host token, labeled the same way | the same credential; a Secret has no lifetime of its own | the same, after the sandbox, so no sandbox is left mounting a Secret that is gone |
| the checkpoint ref at the git host | [[035-hosted-checkpoints-at-the-git-host]]: no credential of the session outlives it, and the server holds none for the git host | the repository's writers, or an installation that prunes `refs/topos/checkpoints/<session>/` |
| the authorizer's own record of the session | it is the installation's | the installation, by its own retention |

A self-hosted installation that runs no authorizer of its own and gives
toposd an installation credential for Cella (`TOPOS_CELLA_TOKEN_FILE`)
can delete the sandbox and its Secrets by the session label; the core
does not do it for it in this spec. Deleting them from the `Deleted`
hook with that credential is the core's follow-up for such an
installation.

### What a client shows

A delete is permanent: the core keeps no tombstone and has no undo. A
client says so before it sends the request, and says that a fork of the
session keeps its own copy. A client that deletes a running session
interrupts it first, as above.

### API changes

| Route | Change |
|---|---|
| `DELETE /v1/sessions/{id}` | asks `session.read`, then `session.delete`; a running session is `conflict`, refused before `session.delete` is asked; the route's row in the OpenAPI document names both actions |

No field and no code is added. An authorizer sees `session.read` before
every `session.delete`, which it already answers for every session the
caller may see.

## Not in this spec

Deleting a session's sandbox, its Secrets and its checkpoint ref from
the core with an installation credential; an undo or a grace period
before the rows go; the content-free event a delete emits to an events
sink ([[023-events-and-observability]]); deleting every session of an
agent at once.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A delete asks `session.read`, then `session.delete`, and answers 204; the session is then `not_found` on get, events, stream and blob reads, and gone from the list and the summary | `internal/server.TestADeletedSessionIsGoneFromEveryRead` | built |
| A running session's delete is `conflict`, asks no `session.delete`, and leaves the session; after an interrupt and the turn's close the delete removes it | `internal/server.TestDeletingARunningSessionAsksNothing` | built |
| A caller who may not read the session hears `not_found` and `session.delete` is not asked; a denied `session.delete` is `forbidden` with the reason and the session stays | `internal/server.TestADeleteIsRefused` | built |
| A runner that claims the session between the question and the store's delete makes the delete `conflict` and leaves the session, and a retry after the claim ends deletes it | `internal/server.TestADeleteRacedByAClaimIsRetried` | built |
| Deleting a session removes its rows and every blob object; a crash between the two leaves no session without its blobs, and the reaper removes the orphans | `internal/server.TestSessionDeletionOrder` | built |
| Through toposd: a session that ran a turn over the stub model is deleted, and every read of it is `not_found` | `cmd/toposd.TestADeletedSessionIsGone` | built |

## Outcome

Built as designed on 2026-10-05. `DELETE /v1/sessions/{id}` reads the
session with `session.read`, refuses a running one as `conflict`, then
asks `session.delete`, deletes, and calls the `Deleted` hook; the
route's row names both actions, and the API reference says what a
delete keeps out of reach of the core. Every criterion has a passing
test, the end-to-end one through `toposd` over the stub model and the
stub Cella among them.

One point the design left open was settled: a second delete of a
deleted session answers `not_found`, from the `session.read` that
opens the route, so a client that retries a delete whose answer it lost
reads `not_found` as done.

Not built, as the design says: deleting a session's sandbox, its
Secrets and its checkpoint ref from the core with an installation
credential, which stays the core's follow-up for a self-hosted
installation with no authorizer of its own; and the directories a host
that did not serve the delete keeps for a session it ran and had not
ended.
