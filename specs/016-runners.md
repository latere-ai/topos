---
title: "Runners: driving a session, recovery of a step without results, the queue, claim, renew and release"
status: drafted
track: core
depends_on: [001-architecture.md, 002-scaffold-and-configuration.md, 004-session-log.md, 005-harness-loop.md, 008-tools.md, 009-machines.md]
affects: [runner/, internal/queue/, internal/runnerrole/, internal/runnerapi/, internal/serve/, internal/store/postgres/, session/]
effort: large
created: 2026-09-27
updated: 2026-10-02
author: changkun
---

# Runners

## Overview

A runner is the process that holds a session's lease and drives the
harness. It claims a session from a store, appends `session.status`
`running`, attaches the machine, recovers any step left without results,
runs turns until the session is idle or ended, and releases the lease.
In phase 1 the runner runs in process, in `topos` or an embedding
application, over the directory store, whose lock is the lease. In
phase 2 toposd keeps a queue: runners claim sessions over its internal
listener, renew a 60 second lease every 15 seconds, and release it when
the session goes idle; a runner that dies loses its leases and another
resumes each session from its log. At most one runner appends to a
session at a time (invariant 3 of [[001-architecture]]), and toposd
never dials a runner (invariant 10).

## Current state

v0.7.0 had no runner: the loop ran inside the caller's function call.
The retired hosted service ran the loop as a goroutine inside its API
process, as a synchronous request behind a 60 second ingress timeout,
and live attach had to reach the one replica that owned the session.
Borrowed from it: the row lease with a 60 second claim renewed every 15
seconds, and attach as replay then live tail ([[015-api]]).

## Design

### The package

```go
type Options struct {
	Store   session.Store
	Harness func(ctx context.Context, s session.Session) (harness.Config, error) // model, machine, tools per session
	ID      string // unique per process
	Kind    string // local, serve or runner
	// The person's own instruction file and skills folder, read on the host only.
	PersonalInstructions, PersonalSkills string
	CheckpointDir string // spec 034's session repositories
	Clock         func() time.Time
	Capacity      int // phase 2
}

func New(o Options) (*Runner, error)
func (r *Runner) Drive(ctx context.Context, sessionID string) (harness.Outcome, error)              // one session, in process
func (r *Runner) Rewind(ctx context.Context, sessionID string, turn int, by session.Sender) (session.SessionRewound, error) // spec 034
func (r *Runner) Serve(ctx context.Context, c Claimer) error                                        // claim loop, phase 2

func NewLog(st session.Store, sessionID string, last uint64) *Log // the harness.Log over a store
```

`Drive` and `Serve` run the same procedure per session; they differ
only in where the lease comes from. `Log` is the `harness.Log` of
[[005-harness-loop]] over a `session.Store`: it appends after the last
sequence it saw and returns the events others appended since, and
reads and writes the session's blobs. `Drive` refuses an ended session
(`ErrEnded`), a session another holder has (`ErrLocked`) and a missing
one (`ErrNotFound`).

### Driving a session

1. Take the lease: `Store.Acquire` in process, a claim in the queue.
2. Append `session.status` `running` with the runner's `id` and `kind`
   (`local`, `serve` or `runner`), before anything else.
3. Attach the machine ([[009-machines]]); append `session.machine`
   when it is the first attachment or the machine changed, and
   `memory.attached` for each store ([[020-memory-stores]]). A
   session's first machine gets its repositories first ([[019-git]]).
   A Cella machine of a session whose log records none is opened on
   demand instead: the runner hands the harness a machine that opens at
   the first tool that acts on it, and does this step then, appending
   `session.machine` beside the running turn through its log, so the
   turn's next request carries the machine's context. A session whose
   log records a machine has it opened here, by name. Once the machine
   is recorded, the runner writes the files of the session's messages
   the machine has not been given and appends `attachments.delivered`
   ([[015-api]]); while a turn runs on an open machine the harness does
   the same before each step's request, so a file sent during the turn
   is there before the model reads its path.
4. Recover the open step, below.
5. Run turns ([[005-harness-loop]]). A `user.message` that arrives
   during a turn waits for that turn's end: it is appended before the
   turn's closing status, so the harness reports it as
   `Outcome.Pending`, and the runner starts the next turn from it
   without releasing. The runner watches the store while a turn runs
   and cancels the turn's context on a `user.interrupt`, which brings
   the step boundary forward ([[005-harness-loop]]).
6. Idle: `Release(false)` on the machine, then release the lease. A
   waiting session holds nothing.
7. Ended: append `thread.ended` for each thread that has not ended,
   `Release(true)` on the machine, then release the lease.

### Recovery of a step without results

A step is committed by its results: `agent.tool_use` is durable before
the call runs and `tool.result` is its commit ([[005-harness-loop]]).
The first two commit points are atomic batches, so the only open work a
runner can find is tool calls with no result, the fold's `Open` list
([[004-session-log]]). For each:

| The call | The resumed runner |
|---|---|
| verdict ask, no confirmation | does nothing; the session is still waiting |
| a client-executed call with no `user.tool_result` | does nothing; the session is still waiting |
| confirmed `allow`, and no `session.status` `running` after the confirmation but the resuming runner's own claim | runs it: no runner started it; one that another claim follows is closed `unknown_effect` |
| a tool whose `repeatable` is true (`memory_sync`, [[020-memory-stores]]) | runs it again |
| anything else | appends `tool.result` with outcome `unknown_effect` and the text `The runner stopped while this call ran. Its effects are unknown; inspect the machine before repeating it.` |

No call without a result is ever run a second time except a repeatable
built-in ([[008-tools]]). The model then inspects the machine.

### Phase 1: in process

`topos run` and an embedding application call `Drive` over the
directory store: the lease is the session's `lock` ([[004-session-log]])
and is never lost while the process lives. A local session's first
claim is the process that created it. The directory store's `Watch`
delivers `user.interrupt` to the running harness.

### The in-process queue

`toposd serve` runs its runners over the same store as its API, through
`runner.Queue`: a claim lists the hosted sessions that are running or
idle, leases each one that has work, checks under the lease that it
still does, and hands out the lease; taking the claim is taking the
store's lease, so a session another runner holds is never handed out.
An idle session found without pending input is remembered by its last
sequence and not read again until it changes. The API calls `Notify`
when a session gains input, which wakes a waiting claim at once; the
poll (2 s) covers anything else. `Serve` drives up to
`TOPOS_RUNNER_CAPACITY` sessions at once.

A turn the runner cannot start, because the agent's bundle, the model
connection or the machine cannot be had, is closed with a
`session.error` naming the code (`agent_missing`, `model_unavailable`,
`model_credential_missing`, `machine_unavailable`, or
`runner_setup_failed`) and `session.status` `idle` `error`, so the
session waits for its next message instead of staying `running` with
nobody driving it. A lost lease cancels the turn and fences the log: an
append after it is refused. A server that stops mid-turn fences the log
the same way and releases the lease, so the session stays `running` and
the next runner's claim resumes it from the log, instead of the turn
being closed as interrupted. On a store whose lease another holder can
take over, Postgres, the runner appends through the lease itself
(`session.Fence`), and the store refuses the batch with `lease_lost` in
the append's own transaction once the session's lease generation is no
longer the writer's; an append outside any lease, a person's event
through the API, is not fenced.

### Phase 2: the queue

A session is claimable when it is not ended, its writer is `hosted`
([[017-external-runners-handoff-fork]]), and either it is `idle` with
pending input (a resuming user event after its last `session.status`,
per the stop reason table of [[004-session-log]]), or it is `running`
with an expired lease. Each session row carries `lease_holder`,
`lease_generation` and `lease_expires_at` ([[014-store]]).

| Operation | Rule |
|---|---|
| claim | picks up to the runner's free capacity of claimable sessions with `FOR UPDATE SKIP LOCKED`, sets the holder, increments the generation, and sets the expiry to now plus 60 seconds; waits up to 20 seconds for work before answering empty |
| renew | every 15 seconds; extends the expiry to now plus 60 seconds when the generation matches; otherwise `lease_lost` |
| release | clears the holder when the generation matches |
| append | carries the generation; an append from any other generation is refused with `lease_lost`, which fences a runner that lost its lease and does not know yet |

A runner whose renew or append answers `lease_lost` stops that session
at once: it cancels the harness, appends nothing more, and lets go of
the machine without deleting it. The session stays `running` with no
lease until another runner claims it, at most 60 seconds after the last
renew, and resumes from the log with recovery. The in-process runners
of `toposd serve` claim through the same queue in process; the `runner`
role claims over the internal listener.

### The internal listener

Mounted on toposd's internal listener when `TOPOS_RUNNER_TOKEN` is set
([[002-scaffold-and-configuration]]); every request carries
`Authorization: Bearer <token>`. A remote claim holds the store's lease
on the session in the serving replica for as long as its runner renews
it: the store's own lease may renew itself, so the claim's 60 second
expiry is what a runner that went away loses, and a reaper every 5
seconds frees the expired ones. The generation is the claim's own. The
event stream a runner follows is one JSON event per line, not
Server-Sent Events, and a blob is put under the digest its bytes must
hash to.

| Method and path | Body | Answer |
|---|---|---|
| `POST /internal/v1/claims` | `runner`, `capacity`, `wait` | a list of `{session_id, generation, expires_at}` |
| `POST /internal/v1/leases/{session}/renew` | `generation` | `expires_at`, or 409 `lease_lost` |
| `POST /internal/v1/leases/{session}/release` | `generation` | 204 |
| `POST /internal/v1/leases/{session}/tokens` | `generation`, `audience`, `workload` (`session` or `sandbox`) | `{token, expires_at}`: a token for the session's agent, or the session's Lux key for the audience `lux`; 409 `lease_lost` for a lease the runner does not hold, 404 `not_minted` when the installation mints nothing for the audience ([[018-credentials-and-secrets]]) |
| `POST /internal/v1/sessions/{session}/events` | `generation`, `after_seq`, `events` | `last_seq`, or 409 `lease_lost` or `sequence_conflict` |
| `GET /internal/v1/sessions/{session}` and `.../events?from_seq=` and `.../stream` | none | the Session, a page of events, replay then live |
| `PUT` and `GET /internal/v1/sessions/{session}/blobs/{digest}` | bytes | the blob |
| `POST /internal/v1/sessions/{session}/connections/{name}` | `generation` | the value of one connection's credential for a runner-held call, held in memory and never appended; 409 `lease_lost` for any other generation |
| `POST /internal/v1/deltas` | `deltas`: a list of `{session_id, generation, delta}` | 204; each delta under its session's live claim is published to the server's store, and one under any other generation is dropped; a delta a runner could not have sent is `invalid_request` |

`internal/runnerrole` implements `session.Store` and `Claimer` over
these routes, so the `runner` package is the same in every role. A
session's credentials reach a runner only through the tokens route and
the connections route, only for the current lease holder, and never
through the log; no agent has a long-lived key, and the `runner` role
holds only the tokens and keys toposd obtained for its lease.

### Live deltas

A drive over a store that carries live deltas
(`session.DeltaPublisher`) gives the harness an Observer
([[005-harness-loop]]) that turns its fragments into the deltas of
[[015-api]]'s stream and publishes them; an Observer the harness
configuration names still receives every fragment. Text, thinking and
tool input fragments are held, one pending delta per thread, turn,
step, block and kind, and published every 50 ms (`DeltaInterval`), when
a pending delta reaches 1024 bytes (`session.MaxDeltaText`; a longer
run is cut between runes into several), at the end of a block, and at
the end of the response, so a step's last text goes out before its
`agent.message` is appended. A reset drops the step's held fragments
and publishes the reset. A signature, a block's header and the
response's own events are not published. When the drive ends, what is
held is published and the Observer stops.

Publishing never waits and reports nothing: a delta that cannot be
delivered is dropped, counted (`DroppedDeltas`) and logged at debug
level. A subscriber holds 256 deltas (`session.DeltaBuffer`) and loses
the next until it reads. The carrier is the store's:

| Store | Carrier |
|---|---|
| directory, memory | in process (`session.DeltaHub`): one `toposd serve` holds a data directory alone, so its runners and its streams share the store |
| Postgres | notifications on the channel `topos_deltas`, received by the store's one listener connection, which listens on it beside `topos_events` ([[014-store]]), and handed to the subscribers in that process; a payload is decoded only where a stream subscribes. A replica's deltas wait in a queue of 1024 for one sender, which sends everything waiting in one statement, `SELECT pg_notify('topos_deltas', p) FROM unnest($2::text[]) AS p`, from the serving pool, packed in order into payloads under Postgres's 8000 byte limit, with a 2 second timeout. Deltas therefore hold at most one pooled connection at a time, only while they flow, and no connection of their own |
| runner role | the internal listener's `POST /internal/v1/deltas`: a queue of 1024 and one sender, which sends everything waiting in one request with a 5 second timeout, each delta under its session's claim generation; the server publishes those of a live claim to its own store, which carries them on |

### Capacity

A runner holds at most `TOPOS_RUNNER_CAPACITY` sessions (default 16)
and claims only up to its free capacity. `toposd serve` with capacity
0 runs no runner of its own, for an installation whose runners are a
separate deployment of the `runner` role.

### Error codes

| Code | Status | Meaning |
|---|---|---|
| `lease_lost` | 409 | the generation is not the session's current one |
| `runner_unauthorized` | 401 | no bearer, or one not in `TOPOS_RUNNER_TOKEN` |
| `not_minted` | 404 | the installation mints nothing for the audience asked, so the runner uses its own credential ([[018-credentials-and-secrets]]) |

## Not in this spec

The loop ([[005-harness-loop]]); the external runner's write and the
append route ([[017-external-runners-handoff-fork]]); the Postgres
columns and migrations ([[014-store]]); credential resolution
([[018-credentials-and-secrets]]); shutdown timing
([[002-scaffold-and-configuration]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Killing a runner at each commit point of a turn, then resuming on another runner, loses no event, duplicates none, and runs no call a second time except `memory_sync` | `TestKillRunnerMidTurnLosesNoEvent`, one subtest per commit point, in process and in the queue | not built |
| An open call is closed `unknown_effect`, a repeatable built-in runs again, a confirmed call no runner started runs once and one a runner may have started is closed `unknown_effect`, and a waiting ask or client call is left waiting | `harness.TestResumeClosesACallWithNoResult`, `harness.TestResumeRunsARepeatableCallAgain`, `harness.TestConfirmationsAndDenials`, `harness.TestAConfirmedCallAnEarlierRunnerMayHaveStarted`, `harness.TestAnUnansweredAskKeepsWaiting`, `harness.TestClientToolsWaitForTheirResult` | built |
| A second `Drive` on a session another holder has gets `ErrLocked`; an ended or missing session is refused; a second process on a locked directory session is locked out | `runner.TestDriveRefusesWhatItCannotDrive`, `session/dir.TestDirStoreSingleWriterLock` | built |
| An append with a stale generation gets `lease_lost` and writes nothing; the new holder's and a person's unleased append go through; a runner whose lease fence refuses stops writing | `internal/store/postgres.TestAFencedAppendAfterATakeoverIsRefused` (postgres tier), `runner.TestTheRunnerAppendsThroughItsLeasesFence` | built |
| A runner that stops renewing loses the session within 60 seconds, and another runner resumes it within 75 seconds of the last renew | `TestLostLeaseResumes` with a fake clock | not built |
| Two runners claiming twenty sessions each hold distinct sessions and never more than their capacity | `TestClaimsAreExclusiveAndBounded` on Postgres | not built |
| A `user.message` appended during a turn starts the next turn without a release | `runner.TestDriveContinuesWhileInputIsPending` | built |
| Driving a session appends `session.status` `running` before anything else, attaches the machine once with its `session.machine`, and a moved machine is recorded with reason `handoff` | `runner.TestDriveAttachesTheMachineAndRunsATurn`, `runner.TestAMovedMachineIsAHandoff` | built |
| A machine opened on demand is attached when a tool first acts on it, its `session.machine` reaches the turn's next request, and a later drive of the session opens it at once and records it no second time | `runner.TestAMachineOnDemandIsRecordedWhenAToolFirstActsOnIt`, `internal/hosted.TestARestartedRunnerReattachesTheSandboxByName` | built |
| A message's files are written into the machine when it is recorded and before each step of a turn on an open machine, once per machine | `runner.TestAMessagesFilesReachTheMachine` | built |
| An `end_on_idle` session ends and its machine is released for good; a harness configuration that cannot start is reported | `runner.TestAnEndOnIdleSessionEndsAndReleasesTheMachine`, `runner.TestDriveReportsAHarnessThatCannotStart` | built |
| `Log.Append` reports the store's errors, including a log ahead of the store and a deleted session | `runner.TestLogAppendReportsTheStore` | built |
| The in-process queue claims a hosted session idle with pending input or running with no live lease, with its lease, and never one another runner holds, an answered one or an external one; `Notify` wakes a waiting claim | `runner.TestQueueClaimsHostedSessionsWithWork`, `runner.TestQueueWakesOnNotify` | built |
| `Serve` drives the sessions it claims up to its capacity and reports a failed claim | `runner.TestServeDrivesTheSessionsItClaims` | built |
| A turn that cannot start closes with its setup code and `idle` `error`, and is not claimed again until a new message | `runner.TestASetupFailureClosesTheTurn` | built |
| A lost lease stops the turn and refuses every later append; a server stopping mid-turn leaves the session running for the next claim | `runner.TestALostLeaseStopsTheDrive`, `runner.TestAServedDriveLeavesItsSessionToTheNextRunner` | built |
| A session created over the API is run by `toposd serve`'s own runner on a Cella machine against the model URL | `cmd/toposd.TestServeRunsAHostedSession` | built |
| The internal routes refuse a request without the runner token, and accept every token `TOPOS_RUNNER_TOKEN` lists | `internal/runnerrole.TestTheRoutesNeedTheRunnerToken` | built |
| A remote claim holds the store's lease while its runner renews it; a renew or an append at another generation is `lease_lost`; the reaper frees a claim whose runner stopped renewing; a store that fences carries the runner's appends | `internal/runnerrole.TestTheProtocolsLeases`, `internal/runnerrole.TestTheServerFollowsTheStoresLease`, `internal/runnerrole.TestALeaseRenewsItself` | built |
| The runner role claims from a toposd's internal listener and runs a session created over the API, writing only through its claims | `internal/runnerrole.TestARemoteRunnerRunsASession`, `cmd/toposd.TestTheRunnerRoleRunsAHostedSession` | built |
| A drive joins the fragments of each block into deltas published every 50 ms, at the end of a block and of the response, and when one reaches 1024 bytes, cut between runes; a retried request publishes a reset between the cut stream's deltas and the next attempt's | `runner.TestTheForwarderJoinsFragments`, `runner.TestTheForwarderPublishesEachInterval`, `runner.TestTheForwarderCutsLongTextBetweenRunes`, `runner.TestTheForwarderResetsAStep`, `runner.TestAResetFollowsARetriedRequest` | built |
| A subscriber that does not read loses the deltas past its buffer, counted, and neither it nor the absence of one delays the turn; on Postgres a full send queue, a failed send, a closed store and a notification that does not decode drop and count | `runner.TestASlowSubscriberNeverDelaysTheTurn`, `session.TestAFullSubscriberDropsAndCounts`, `internal/store/postgres.TestADeltaNeverWaitsOnTheDatabase` (postgres tier) | built |
| A runner's deltas on one replica reach a stream with `deltas=1` on another through Postgres, in the order published, packed into notifications under 8000 bytes; every store that carries deltas passes the conformance suite's deltas | `internal/store/postgres.TestARunnersDeltasReachAStreamOnAnotherReplica`, `internal/store/postgres.TestDeltasCrossReplicas`, `internal/store/postgres.TestDeltasPackIntoNotifications` (postgres tier), `session/storetest` `Deltas` | built |
| The runner role's deltas reach the server's streams over the internal listener under a live claim only; a delta without a claim, past the queue or of a failed send is dropped and counted | `internal/runnerrole.TestARemoteRunnersDeltasReachTheServersStreams`, `internal/runnerrole.TestTheDeltasRoute` | built |
| toposd opens no connection to a runner: every runner connection is outbound from the runner | `TestServerOpensNoConnectionToARunner` | not built |
| A runner that finds its session's sandbox gone, with no checkpoint to restore, creates a new one and appends `session.machine` with reason `replaced` and a `session.error` `machine_lost` ([[009-machines]]) | `TestALostSandboxIsReplaced` | not built |
| The `topos` CLI drives a local session over the directory store with no server | `internal/toposcli.TestRunATurnInTheWorkingDirectory`, `runner.TestDriveAttachesTheMachineAndRunsATurn` | built |

## Outcome

Shipped in v0.9.0 (2026-09-29): `Drive` in process under the directory
store's lock, the queue and `Serve` of `toposd serve` up to
`TOPOS_RUNNER_CAPACITY`, the `toposd runner` role that claims over the
internal listener and writes only through its lease, `lease_lost`, the
recovery of a step without results, and machines opened when a tool
first acts on them. v0.9.2 (2026-09-29) leaves a running session to the
next claim when a server or runner stops. v0.9.7 (2026-10-01) publishes
a drive's deltas to streams on any replica.

Open: the kill test at each commit point, the lease's timing under a
fake clock, exclusive and bounded claims on Postgres, the test that
toposd opens no connection to a runner, and a lost sandbox replaced with
`machine_lost`.

The status stays `drafted` while [[005-harness-loop]] and [[008-tools]]
are open, since the gate starts no spec before its dependencies close.
