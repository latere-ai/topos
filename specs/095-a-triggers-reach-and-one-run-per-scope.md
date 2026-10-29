---
title: "A trigger's reach and one run per scope: spec.reach names a kind the authorizer maps and, with split scope, makes each firing ask trigger.runs which scopes are due before it starts anything, start one session for each with the scope and the time since which it reads, and record skipped_nothing_new when none is, so a night with nothing new starts no session and asks no model"
status: testing
track: core
depends_on: [.archive/022-triggers.md, 086-proactive-agents.md, 087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report.md, 088-a-triggers-runs-and-fire-times-read-for-a-forecast.md, 090-a-trigger-that-fires-at-a-list-of-times.md, 093-memory-folders-the-file-tools-reach.md]
affects: [manifest/v1/, manifest/trigger/, manifest/, authorizer/, internal/triggers/, internal/store/, internal/store/postgres/, internal/store/dir/, internal/server/, api/openapi.yaml, docs/]
effort: medium
created: 2026-10-29
updated: 2026-10-29
author: changkun
---

# A trigger's reach and one run per scope

## Overview

A trigger with one job needs far less than its agent can do, and an
installation that runs such a job for a person, on a schedule, in the
background, wants the authorizer to hold every call of its runs to that
job. Some jobs also divide by scope: a nightly pass over what a person
keeps in mind about themselves and about each of their projects must
run once per project, each run in its own session, and must not run at
all on a night with nothing new.

This spec adds the parts the core owns. `spec.reach` names a kind, a
name the installation's authorizer maps to what the trigger's runs may
reach; the core carries it in its questions and decides nothing from
it. With `split: scope`, each firing first asks the authorizer which
scopes are due, `trigger.runs`, and starts one session for each, its
create naming the scope and the time since which the run reads. An
answer that names none records `skipped_nothing_new`, starts nothing
and asks no model, so it costs nothing.

What a kind reaches, how a scope is named, when one is due, and where a
run writes are the installation's: [[093-memory-folders-the-file-tools-reach]]'s
folders are how one installation's authorizer gives a run the files it
works in.

## Current state

- A trigger fires on a schedule, on events, once at `at`, or at a list
  of times ([[022-triggers]], [[086-proactive-agents]],
  [[090-a-trigger-that-fires-at-a-list-of-times]]). Each firing starts
  or continues one session, asked of the authorizer as `session.create`
  or `session.send` as the trigger's owner, with `trigger_id`,
  `firing_id` and `attended` on the create.
- A firing at its weekly allowance starts nothing and records
  `skipped_budget` ([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]).
- The runs read answers each trigger's runs, one per firing that ran or
  was stopped, keyed by the firing
  ([[088-a-triggers-runs-and-fire-times-read-for-a-forecast]]).
- Nothing tells the authorizer what a trigger's runs are for, and a
  firing can start one session at most.

## Design

### Recommendations

| Item | Recommended | Weighed against, and why not |
|---|---|---|
| How a reach is written | a named kind the authorizer maps, `reach.kind` | a list of paths and actions in the manifest: every client could write a wider one, and the rule would live in every client instead of one authorizer |
| Who decides which scopes are due | the authorizer, asked `trigger.runs` at the firing | the core: it would need to know what a scope is and what is new in it, which is the installation's data |
| One run per scope | the firing starts one session for each due scope | one trigger per scope: every client would group and edit many triggers as one item, and they would drift; one session for all scopes: one scope's details would share a context with another's |
| A firing with nothing due | `skipped_nothing_new`, a run with a row and a count, no session | a session that looks and ends: a model call every night for nothing; a refusal: `refused` reads as a failure on every surface |
| A scope's session | its key is the scope, so `skipIfActive` holds per scope | the trigger's one key: the second scope of a night would be skipped behind the first |
| `maxActive` | does not cut a split firing's runs short; the answer bounds them at `MaxRunsPerFiring` | `maxActive` across scopes: the default of one would run one scope a night |
| A scope whose last session is still active | not started this firing, and recorded nowhere; the next firing asks again | `skipped_active` per scope: a run row of an outcome that is no run ([[088-a-triggers-runs-and-fire-times-read-for-a-forecast]]), and a count beside the firing's own |
| A start the authorizer refuses for one scope | that scope's run is recorded `refused` with its reason; the other scopes start | the whole firing refused: one project's refusal would stop the person's other runs |
| A start that fails for one scope | not recorded; the firing's other scopes stand, and the next firing asks again since the scope's last run did not move | the whole firing failed: a redelivery would start the scopes that started again |
| `last` | the latest started run of each scope of the trigger, from the runs the core keeps | none: the authorizer would keep its own copy of the core's runs |
| The kinds and policies a split trigger fires with | a schedule, policy new, no `session.target`, no `session.key` | events, `at` or `times`: a run per scope of an event or of a one-shot has no use yet, and each would need its own rules for retakes and spending |

### The field

The rows below join [[022-triggers]]'s table of the Trigger kind's
fields.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `reach.kind` | a name, `^[a-z][a-z0-9_]{0,31}$` | none: a trigger without `reach` runs as before | what the trigger's runs reach, as a whole; the core carries it in its questions, and the authorizer maps it |
| `reach.split` | `scope` | none: one run per firing | each firing asks `trigger.runs` and starts one run per due scope (below) |

Apply refuses, as `invalid_manifest` with the field's path, a kind
outside the pattern (`spec.reach.kind`), a split other than `scope`
(`spec.reach.split`), and a split trigger that fires on `on`, `at` or
`times`, names `policy: continue`, `session.target` or `session.key`
(`spec.reach.split`).

`trigger.create` and `trigger.update` carry `reach: {kind, split}`,
split left out when absent, so the authorizer refuses a kind it does
not know or one its owner may not use when the trigger is applied.
Every session a firing of a trigger with a reach starts carries `reach`
on its `session.create`: `{kind}` for a trigger with no split, and
`{kind, scope, since}` for a run of one scope (below), so the
authorizer holds each session to its kind.

### `trigger.runs`

A new action of [[006-identity]]'s table, of kind `trigger`, asked as
the trigger's owner with the claims its apply kept, as every question
of a firing is. The fire route's row names it beside `session.create`
and `session.send`, since a manual fire asks it.

| Field | Value |
|---|---|
| resource id | the trigger's id |
| `owner` | the trigger's owner |
| `name` | the trigger's name |
| `agent` | the agent id it runs |
| `reach` | `{kind, split}` |
| `firing` | `{id, at, origin}`: the firing's id, its time in RFC 3339, and `schedule` or `manual` |
| `last` | `{<scope>: <RFC 3339>}`: the time of each scope's latest run of this trigger that started a session, among the runs the core keeps ([[088-a-triggers-runs-and-fire-times-read-for-a-forecast]], `RunsKept`); `{}` when none |

The allow's limits are `{"runs": [{"scope": "you", "since": "2026-10-28T03:00:00Z"}, {"scope": "projects/prj_01JA4R"}]}`:
the scopes due, in the order they start, each with the time since which
its run reads, which may be absent. `authorizer.WireRuns` is the object
and `authorizer.DecodeRuns` the core's reading of it.

| Constant | Value | Why |
|---|---|---|
| `trigger.MaxRunsPerFiring` | 16 | a firing starts at most this many sessions; a person's scopes past it wait for the next firing, which the authorizer orders |

An answer of more than `MaxRunsPerFiring` runs, a scope outside
`^[A-Za-z0-9][A-Za-z0-9_./:-]{0,127}$`, a scope named twice, or limits
that do not read refuse the firing with reason `runs_invalid`. A deny
refuses the firing with its reason, a deny whose limits name
`resets_at` with that time, as a refused start is. An authorizer that
does not answer fails the firing, which a redelivery runs again.

An authorizer that knows no kind, the owner policy of a standalone
installation among them, allows with no limits: every firing of a
split trigger records `skipped_nothing_new`.

### A firing of a split trigger

After the firing is judged late or not, its message held to its size,
and its allowance read ([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]),
and before any session starts:

1. The engine asks `trigger.runs`.
2. No run: the firing records `skipped_nothing_new`, which is a run
   with a row, ended at a cost of nothing, counted in `status.counts`
   as `skippedNothingNew`.
3. For each run in the answer's order: a scope whose key's session is
   still active is passed over when `skipIfActive` holds; any other
   starts a session as a firing does, its key the scope, its
   `session.create` carrying `reach: {kind, scope, since}` beside
   `trigger_id`, `firing_id` and `attended`, since left out when absent.
4. Every started scope is a run with its scope and its session, and
   every scope the authorizer refused a run `refused` with its reason.
   The firing records, in this order: `started` with its first started
   session when any scope started; `refused` with the first refusal's
   reason, and its `until`, when one was refused; `failed` when a start
   was tried and failed; and `skipped_active` when every scope was
   passed over. A failed scope is no run, and a passed-over one neither.

Every session of one firing carries the firing's spend, and the turns
they begin count toward the trigger's weekly allowance together, as
looks do; the allowance's read counts the sessions of the trigger's
runs, so a firing's second session is counted when it ends.

### The runs

`store.Run` gains `Scope`, `""` for a run of a trigger with no split; a
run is keyed by its firing and its scope. Postgres migration `0023`
adds the column and makes the key `(firing_id, scope)`; the directory
store names a scope's run file after its firing and a digest of its
scope. A run's cost is read from the log of its session, so a cost
names its session as well as its firing and is written to the firing's
run of that session; a cost of no session is written to every run of
its firing.

The runs read answers each run's `scope`, left out when empty, and
`skipped_nothing_new` among the outcomes.

## What a hosted installation needs

| Need | Why |
|---|---|
| An authorizer that decides `trigger.runs`, refuses `reach` kinds it does not know at `trigger.create` and `trigger.update`, and reads `reach` on `session.create` | the core decides nothing from the kind |
| The authorizer rolled before the image of this release | the release asks a new action; an authorizer that refuses an unknown action refuses every firing of a split trigger |
| A client that shows a run per scope reads `scope` in the runs read, and `skipped_nothing_new` as a run that cost nothing | the field and the outcome are new |

## Not in this spec

What a kind reaches and how a run is held to it; what a scope is and
when it is due; where a run writes and what happens to what it wrote;
the item an installation ships with a reach; a split by anything but
scope; a split trigger on events, `at` or `times`.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `spec.reach` resolves as written; a kind outside the pattern, a split other than `scope`, and a split trigger on events, at `at` or `times`, under policy continue, with a target or with a key are `invalid_manifest` at their paths; a trigger without it resolves to the digest it had | `manifest.TestTheReachOfSpec095` | built |
| `trigger.runs` is in the action table, of kind trigger, in the table's order | `authorizer.TestVocabularyMatchesTheSpec` | built |
| `DecodeRuns` reads the runs in order, none for an allow with no limits, and refuses limits that do not read or runs `trigger.CheckRuns` refuses | `authorizer.TestDecodeRuns` | built |
| A split firing asks `trigger.runs` with the trigger, the reach, the firing and each scope's last started run; nothing due records `skipped_nothing_new` with no start; two due scopes start two sessions, each with its reach and its scope's key; a deny is refused with its reason and its until; an invalid answer is `runs_invalid`; no answer fails the firing; a trigger without a split asks nothing, one with a kind starting with `{kind}` | `internal/triggers.TestASplitFiring` | built |
| The minute loop's firing asks as `schedule`, at its scheduled time | `internal/triggers.TestASplitFiringAtItsSchedule` | built |
| A scope whose session is active is passed over, one refused is recorded refused, a failed one is no run, a firing's outcome follows the order above, and with `skipIfActive` false and `maxActive` 1 every scope starts | `internal/triggers.TestASplitFiringsScopesEachStandAlone` | built |
| The runs of a split firing count toward the allowance together, a session that ended counted through the runs that name it, and each run's cost is its own session's | `internal/triggers.TestASplitFiringsRunsCountTogether` | built |
| A run is kept by its firing and its scope in every store, its cost written by its session | `internal/store/storetest` "trigger runs by scope" (memory, directory, Postgres) | built |
| A scope's run file is named after its firing and a digest of its scope, read back after a restart, and a file under another scope's name is refused | `internal/store/dir.TestScopedRunsReopen` | built |
| A run a database at migration 0022 holds reads back after 0023 with no scope, beside the runs of the same firing's scopes | `internal/store/postgres.TestARunBeforeTheScopeColumnReadsWithNone` | built |
| Applied at the route, `trigger.create` and `trigger.update` carry the reach; a tick asks `trigger.runs` with the resource above, starts a session per due scope whose create carries the reach, and the runs read answers each run's scope; a tick with nothing due starts nothing and is read as `skipped_nothing_new`; a manual fire with an invalid answer is `runs_invalid`; a trigger with a kind and no split asks nothing and starts with `{kind}` | `internal/server.TestASplitTriggerEndToEnd` | built |
| The OpenAPI document describes the field, the outcome and the run's scope | `internal/server.TestOpenAPIIsGenerated`, `internal/server.TestTheDocumentDescribesSpec095` | built |

## Outcome

Built on 2026-10-29, in no release yet. The field, the action, the
outcome, the run's scope and migration `0023` are as designed. One
addition: a trigger with a kind and no split carries `reach: {kind}` on
each firing's `session.create`, so a kind applies to every session a
trigger starts and not only to a split one's. A failed start of one
scope logs a warning and leaves the firing's other scopes as they are.
