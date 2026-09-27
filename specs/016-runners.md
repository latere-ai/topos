---
title: "Runners: driving a session, recovery of a step without results, the queue, claim, renew and release"
status: drafted
track: core
depends_on: [001-architecture.md, 002-scaffold-and-configuration.md, 004-session-log.md, 005-harness-loop.md, 008-tools.md, 009-machines.md]
affects: [runner/, internal/queue/, internal/runnerrole/, internal/serve/]
effort: large
created: 2026-09-27
updated: 2026-09-27
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
   `memory.attached` for each store ([[020-memory-stores]]).
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
`Authorization: Bearer <token>`.

| Method and path | Body | Answer |
|---|---|---|
| `POST /internal/v1/claims` | `runner`, `capacity`, `wait` | a list of `{session_id, generation, expires_at}` |
| `POST /internal/v1/leases/{session}/renew` | `generation` | `expires_at`, or 409 `lease_lost` |
| `POST /internal/v1/leases/{session}/release` | `generation` | 204 |
| `POST /internal/v1/sessions/{session}/events` | `generation`, `after_seq`, `events` | `last_seq`, or 409 `lease_lost` or `sequence_conflict` |
| `GET /internal/v1/sessions/{session}` and `.../events?from_seq=` and `.../stream` | none | the Session, a page of events, replay then live |
| `PUT` and `GET /internal/v1/sessions/{session}/blobs/{digest}` | bytes | the blob |
| `POST /internal/v1/sessions/{session}/tokens` | `generation`, `audience` | `{token, expires_at}`: a token for one core audience, traded from the session's key by toposd, for the `TokenSource` of [[018-credentials-and-secrets]]; 409 `lease_lost` for any other generation |
| `POST /internal/v1/sessions/{session}/connections/{name}` | `generation` | the value of one connection's credential for a runner-held call, held in memory and never appended; 409 `lease_lost` for any other generation |

`internal/runnerrole` implements `session.Store` and `Claimer` over
these routes, so the `runner` package is the same in every role. A
session's credentials reach a runner only through the last two routes,
only for the current lease holder, and never through the log; the
`runner` role never holds an agent's long-lived key, only the tokens
traded from it.

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
| An append with a stale generation gets `lease_lost` | `TestSecondWriterIsRefused` | not built |
| A runner that stops renewing loses the session within 60 seconds, and another runner resumes it within 75 seconds of the last renew | `TestLostLeaseResumes` with a fake clock | not built |
| Two runners claiming twenty sessions each hold distinct sessions and never more than their capacity | `TestClaimsAreExclusiveAndBounded` on Postgres | not built |
| A `user.message` appended during a turn starts the next turn without a release | `runner.TestDriveContinuesWhileInputIsPending` | built |
| Driving a session appends `session.status` `running` before anything else, attaches the machine once with its `session.machine`, and a moved machine is recorded with reason `handoff` | `runner.TestDriveAttachesTheMachineAndRunsATurn`, `runner.TestAMovedMachineIsAHandoff` | built |
| An `end_on_idle` session ends and its machine is released for good; a harness configuration that cannot start is reported | `runner.TestAnEndOnIdleSessionEndsAndReleasesTheMachine`, `runner.TestDriveReportsAHarnessThatCannotStart` | built |
| `Log.Append` reports the store's errors, including a log ahead of the store and a deleted session | `runner.TestLogAppendReportsTheStore` | built |
| The internal routes refuse a request without the runner token | `TestRunnerRoutesNeedTheToken` | not built |
| toposd opens no connection to a runner: every runner connection is outbound from the runner | `TestServerOpensNoConnectionToARunner` | not built |
| The `topos` CLI drives a local session over the directory store with no server | `internal/toposcli.TestRunATurnInTheWorkingDirectory`, `runner.TestDriveAttachesTheMachineAndRunsATurn` | built |
