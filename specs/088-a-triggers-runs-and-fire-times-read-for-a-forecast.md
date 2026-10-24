---
title: "A trigger's runs and fire times, read for a forecast: every firing a person counts as a run is recorded with its turn's cost where the firing is recorded and where the logs are already read, one read answers an owner's triggers with their runs since a time, the mean cost of a run and the times each fires up to a time, and a trigger's createdAt is its first apply"
status: in-progress
track: core
depends_on: [.archive/022-triggers.md, 074-one-owners-triggers-and-agents-and-an-agent-deleted.md, 086-proactive-agents.md, 087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report.md]
affects: [internal/store/, internal/triggers/, internal/server/, manifest/trigger/, manifest/, api/openapi.yaml, docs/]
effort: medium
created: 2026-10-24
updated: 2026-10-24
author: changkun
---

# A trigger's runs and fire times, read for a forecast

## Overview

A client that shows a person what their standing work will cost by the
end of the month needs, for each of the person's triggers, what it did
and what it will do: every run so far with its time, its cost and how it
ended, skipped ones included; when it fires next, up to a time; for one
that fires on events, how often it ran lately and how old it is; and the
cost of a typical run. The forecast itself is the installation's: it
knows the person's credit and how their own spending goes. This spec
gives it the facts in one read, cheaply.

Three changes:

1. **A run record.** Every firing that started or continued a turn, or
   that an allowance or the authorizer stopped, is recorded as a run when
   the firing is recorded. Its cost is written where Topos already reads
   the logs: at each firing that counts toward a trigger ([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]),
   and at the read for a run whose turn had not ended. A wake's run
   outlives the wake's trigger, which its firing deletes.
2. **One read.** `GET /triggers/runs` answers, for each trigger of the
   owners the list's question admits, its runs since a time, the mean
   cost of its latest runs, and the times it fires up to a time.
3. **A trigger's `status.createdAt` is its first apply.** It was the
   time of the latest version, so a client that says when the person set
   a trigger up gave the day of its last pause or change instead.

## Current state

- A firing is recorded in `trigger_firings` with its outcome, its session
  and its time ([[022-triggers]]). A one-shot's firing, a wake's among
  them, is deleted with its trigger when it spends it ([[086-proactive-agents]]).
- A look's cost is read from the session logs at each firing that counts
  toward a trigger, for the rolling 7 days of the allowance, and nothing
  keeps it ([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]).
  A month read the same way would read every log of every trigger on
  every request.
- `status.spend` holds the week as of the trigger's last firing; nothing
  answers a run's own cost or anything older than 7 days.
- `expectedLooks` walks a schedule's fire times to the end of the
  allowance's window; nothing answers fire times further ahead.
- The resolver writes `status.createdAt` at every new version, so a
  trigger applied again, as a pause, a resume or a change applies it,
  answers the time of that apply. The record keeps the first apply's
  time.

## Design

### Recommendations

| Item | Recommended | Weighed against, and why not |
|---|---|---|
| Where a run's cost lives | its own record, `trigger_runs`, one row per firing, kept apart from the firing rows | a cost column on `trigger_firings`: a wake's firing row is deleted with its spent trigger, so every follow-up's runs would be lost |
| When a run is written | where the engine records the firing, with no cost, and its cost where the logs are already read: at each firing's count of [[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]], and at the read for a run whose turn had not ended | reading every log at every read: a month of every trigger; a hook on every event append: the runner, not the server, writes most events |
| What a run is | a firing that started or continued a turn, or one `skipped_budget` or `refused` stopped | every firing: a firing skipped as late or busy, or still held, is not something a person counts as a run |
| A run's time | the firing's time: the scheduled time, the event's arrival, the wake's time | the message's time in the log: a skipped run has none, and the two differ by the time the firing takes |
| How long runs are kept | `RunsKept`, six weeks, pruned by the minute loop | as long as the firings: a month's forecast reads four weeks back at most, and five weekly runs fit in six weeks |
| The read's shape | one route for the owner's triggers, paged as the list is, each with its runs, its mean and its fire times | a route per trigger: a client reads a person's month with one request per trigger |
| The read's question | `trigger.list`, with the list's `owner` and filter ([[074-one-owners-triggers-and-agents-and-an-agent-deleted]]) | a new action: every installation's authorizer would have to learn it before the release reaches it; the read answers what the list answers and the runs of those triggers alone |
| The route's path | `/triggers/runs`, a name no trigger takes, as `/triggers/hold` ([[086-proactive-agents]]) | a top-level `/runs`: a run is a trigger's, and Topos keeps every trigger route under `/triggers` |
| A run's cost before this spec | the read records a firing of the trigger since `since` that has no run, from the firing, and completes it from its log | none: a forecast in the month of the release would count only the runs after it |
| The hold | not applied: the fire times are what would fire once the hold is released | the hold applied: a client that shows each item's month as if it ran, while everything is held, would read nothing ahead |
| `status.createdAt` | the record's first apply, in every answer | a new field beside it: the SDK already documents `createdAt` as the first apply, and the version's own time is the digest's business, not the person's |

### The run record

A run is a firing a person counts: one that started or continued a
session, so a turn a trigger's message began (a look of [[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]),
or one that started nothing because the allowance of the trigger it
counts toward was reached (`skipped_budget`) or the authorizer refused it
(`refused`, with its reason). One row per firing, keyed by the firing's
id:

| Field | Meaning |
|---|---|
| `firing_id` | the firing's `frg_` id |
| `owner` | the trigger's owner, whose read answers the run |
| `trigger_id` | the trigger that fired; for a wake, the wake's own trigger, which its firing deletes |
| `session_id` | the session the run started or continued; empty for a skipped one |
| `wake` | the firing was a wake's |
| `session_trigger_id` | for a wake's run, the trigger that started its session (the session's `trigger_id`); empty for a session a person started, and for every other run |
| `at` | the firing's time: a schedule's scheduled time, an event's or a manual firing's arrival, a one-shot's time |
| `outcome` | `started`, `continued`, `skipped_budget` or `refused`, the firing's |
| `reason` | the firing's reason, for a refused one |
| `cost_usd_micro` | what the turn cost, counted as a look's cost is counted ([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]); 0 for a skipped run |
| `ended` | the turn closed, so its cost is final; a skipped run is ended when it is recorded |

The engine writes the run where it records the firing, for those four
outcomes, with no cost and `ended` false for one that started or
continued. A firing taken again, a one-shot's `skipped_budget` that later
fires, writes the same row again with its new outcome. A run whose write
fails is logged and the firing keeps its answer: the read records it
again from the firing, and only a wake's run, whose firing is gone with
its trigger, is lost.

The row is not deleted with its trigger. The minute loop deletes the runs
older than `RunsKept` at each pass.

### Where a run's cost is written

| Where | What it writes |
|---|---|
| each firing that counts toward a trigger | 087's count reads the logs of the trigger's sessions in the window and finds each look by its firing; it writes each look's cost, and `ended` once its turn closed, to the look's run |
| the read | a run it answers that started or continued and has not ended is completed from its session's log, read once per session per read; a session gone ends the run with the cost it holds |

A log is so read at the read only while a run in it has not ended, and
once it ends its row is final. A person's month is answered from rows,
whose number is bounded by `RunsKept`, and the logs of the turns still
running, or ended since the trigger last fired.

### The read

`GET /triggers/runs`, asked as `trigger.list` with the list's fields and
filter and the list's `owner` ([[074-one-owners-triggers-and-agents-and-an-agent-deleted]]):
the triggers it answers are the ones `GET /triggers` would, paged the same
way by `limit` and `cursor`.

| Parameter | Default | Bounds | Meaning |
|---|---|---|---|
| `since` | now less `RunsSinceDefault` (28 days) | from now less `RunsKept` to now | the earliest run answered |
| `until` | now plus `SpendWindow` (7 days) | from now to now plus `RunsAhead` | the latest fire time answered |
| `owner`, `limit`, `cursor` | as `GET /triggers` | as `GET /triggers` | as `GET /triggers` |

A time outside its bounds, or one that is no RFC 3339 time, is
`invalid_request` naming the bound.

The answer is `{now, items, next_cursor}`: `now` the time the answer was
computed at, and one item per trigger:

| Field | Meaning |
|---|---|
| `trigger_id`, `name` | the trigger's id and `metadata.name` |
| `created_at` | its first apply |
| `fires_on` | `schedule`, `event` (`on`) or `at` |
| `suspended` | `spec.suspend` |
| `per_week_usd_micro` | `spec.budget.perWeek`; absent without one |
| `wake` | a wake set in a session ([[086-proactive-agents]]) |
| `target` | for a trigger whose firings continue one session, a wake or one naming `session.target`: `{session_id, trigger_id}`, `trigger_id` the trigger that started that session, absent for a session a person started |
| `runs` | its runs from `since`, oldest first, at most `MaxRunsListed`, the newest kept: for a wake, the wake runs of its session, every wake set in it, each once its own trigger; for any other trigger, its own runs and the wake runs of the sessions it started, which count toward it ([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]) |
| `more_runs` | true when older runs from `since` were left out |
| `per_run_usd_micro` | the mean cost of the latest `trigger.PaceLooks` (5) runs that started or continued and ended, of its own firings (for a wake, of its session's wake runs), from the whole record, `since` aside; absent before any |
| `fires` | the times it fires after now and before `until`, oldest first, at most `MaxRunFires`: a schedule's next times in its zone, a one-shot's `at`, or now for a one-shot past its time still waiting for its allowance; none for an event trigger or a suspended one. The hold is not applied |
| `more_fires` | true when fire times before `until` were left out |

Each run in `runs`:

| Field | Meaning |
|---|---|
| `firing_id`, `at`, `outcome`, `reason`, `session_id`, `wake`, `cost_usd_micro`, `ended` | as the record holds them |

A trigger that fires on events reads how often it ran from its runs and
how old it is from `created_at`; the read estimates nothing.

Before it answers, the read records each firing of a trigger it answers
since `since` that has one of the four outcomes and no run: a firing from
before this spec, or one whose run's write failed. A wake's firings
before this spec are gone with their triggers and are not recorded.

### Fire times

The walk of a schedule's fire times is one function, used by
`expectedLooks` for the allowance's window and by the read up to `until`,
so both read the same times: the minute loop's `NextFire` from a time,
again from each, until the end or the bound.

### The constants

Constants of `manifest/trigger`, read by the server, the OpenAPI text and
the tests:

| Constant | Value | Why this value |
|---|---|---|
| `RunsKept` | 6 weeks | a month's forecast reads four weeks back at most, and the mean of five runs of a weekly schedule needs five weeks |
| `RunsSinceDefault` | 28 days | the four weeks a rate of runs is read over |
| `RunsAhead` | 62 days | the rest of a month and the next, so a client sees past the month's end |
| `MaxRunsListed` | 3000 | a run every 20 minutes for six weeks; past that a trigger fires too often for a person's month to be read run by run |
| `MaxRunFires` | 3000 | the same bound for the times ahead |
| `RunsName` | `runs` | the path segment of the read, which no trigger takes as its name |

Apply refuses `metadata.name: runs` as `invalid_manifest` with the
field's path, as it refuses `hold`.

### A trigger's createdAt

`status.createdAt` of a trigger, in `GET /triggers`, `GET
/triggers/{ref}`, an apply's answer and the read, is the time of its
first apply, which the record keeps. A later apply changes `version`,
`digest` and the spec, and leaves `createdAt`.

### The OpenAPI document

The document describes `GET /triggers/runs`, its parameters and its
answer, and says `status.createdAt` of a trigger is its first apply.

## What a hosted installation needs

| Need | Why |
|---|---|
| Its authorizer answers `trigger.list` asked with `owner` for the service that reads a person's month, as it does for spec 074's list | the read is asked as the list is, and answers the triggers the list would |
| A forecast that reads the runs once per page of triggers and keeps the arithmetic its own | Topos gives facts; what a run will cost against the person's credit is the installation's |
| The image of this release pinned | the record, the read and `createdAt` are the server's |

## Not in this spec

A forecast, a rate or a price; a run's cost split by model or by step; a
person's own turns in a session a trigger started, which are not runs;
the runs of a trigger once it is deleted, which no read answers; a
notice when a run is skipped; the words a client shows.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A run is stored, written again with a new outcome, given its cost and ended, listed by owner from a time oldest first, kept after its trigger is deleted, and pruned before a time, in every store | the stores' conformance case `trigger runs`, on Postgres in the postgres tier | built |
| A firing that starts, continues, is skipped at its allowance or is refused records its run, a wake's with the trigger that started its session, a skipped one ended; a late, busy or held firing records none | `internal/triggers.TestAFiringRecordsItsRun` | built |
| The count of a firing writes the cost of each look it read to its run, and `ended` once its turn closed | `internal/triggers.TestTheCountWritesEachRunsCost` | built |
| The minute loop deletes the runs older than `RunsKept` | `internal/triggers.TestTheMinuteLoopPrunesOldRuns` | built |
| A schedule's fire times are walked to a time and a bound, a one-shot's is its time or now when past, an event trigger and a suspended one have none, and `expectedLooks` reads the same walk | `internal/triggers.TestFireTimes`, `internal/triggers.TestTheSpendOfAWatch` | built |
| The read answers each trigger the list admits with its fields, its runs since `since`, a wake's its session's, a trigger's its own and the wakes of the sessions it started, the mean of its latest five ended runs, its fire times to `until` and the bounds' flags; `owner` and the filter narrow it as the list's | `internal/server.TestTheRunsOfAnOwnersTriggers` | built |
| The read completes a run that had not ended from its session's log, once per session, and records a firing that has no run | `internal/server.TestTheReadCompletesAndRecordsRuns` | built |
| `since` and `until` outside their bounds, or not times, are `invalid_request` | `internal/server.TestTheRunsReadsBounds` | built |
| Apply refuses the name `runs` | `manifest.TestTheRouteNamesAreNoTriggers` | built |
| A trigger's `status.createdAt` stays its first apply through a later apply | `internal/server.TestATriggersCreatedAtIsItsFirstApply` | built |
| The OpenAPI document describes the read | `internal/server.TestOpenAPIIsGenerated`, `internal/server.TestTheDocumentDescribesSpec088` | built |
