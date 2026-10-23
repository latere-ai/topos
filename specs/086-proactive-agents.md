---
title: "Proactive agents: an agent proposes a trigger its owner approves through a held call, sets its own next wake as a one-shot trigger with a note, bounded in delay, horizon and count, cancels it, and says whether a turn a trigger began brought anything to the person; trigger sessions that wait for their owner's answers, and one hold over all of an owner's triggers"
status: in-progress
track: core
depends_on: [.archive/003-manifest.md, .archive/004-session-log.md, 008-tools.md, 012-permissions-and-approvals.md, 013-threads-and-subagents.md, 015-api.md, 016-runners.md, .archive/022-triggers.md, .archive/037-decision-services.md, 039-questions.md]
affects: [manifest/v1/, manifest/trigger/, manifest/, internal/triggers/, internal/server/, internal/store/, internal/hosted/, internal/proposal/, internal/runnerapi/, internal/runnerrole/, runner/, harness/, harness/tools/, session/, prompts/, api/openapi.yaml, docs/]
effort: large
created: 2026-10-22
updated: 2026-10-23
author: changkun
---

# Proactive agents

## Overview

A trigger starts or continues sessions of an agent when nobody is
present ([[022-triggers]]), and only a person or a program with a token
applies one. An agent in a session cannot offer standing work ("every
Monday at 9", "when a push lands on main"), cannot come back to a
conversation later to look again, and cannot say, when a trigger woke
it, that it found nothing worth anyone's attention. Every turn a
trigger begins reads to a client as a result the person should see.

This spec makes an agent proactive: woken by a schedule, by a delivered
event, or by itself, and on each wake deciding whether anything deserves
the person's attention. It adds three harness tools and the trigger
fields they need:

- `trigger` lists the owner's triggers of the agent, and proposes,
  changes, pauses, resumes or deletes one. Every proposal, change,
  resume and delete is held for the person who owns the trigger, through
  the hold of [[012-permissions-and-approvals]], and runs only on that
  person's allow.
- `wake` sets the session's next wake: a one-shot trigger at a time the
  agent chooses, which sends a note the agent wrote to its future self
  back into the same session. It is bounded in delay, horizon and count,
  and canceled by the same tool, by the person, or by the session's end.
- `notify` marks a turn a trigger began as one that brought something to
  the person. A turn that ends without it, and without waiting for a
  person, is quiet, and the log says so.

A trigger may now fire once (`at`), continue a named session
(`session.target`), start sessions that wait for their owner's answers
(`session.attended`), and say whether its turns always bring their
result or the agent judges (`session.notify`). One hold stops every
trigger of an owner at once.

Latere's platform is the first installation to use it, for the agent
of Latere Chat and Latere Studio. Each choice below is a recommendation
until this spec is validated; the table that opens the design weighs
each against its alternative.

## Current state

- The Trigger kind fires on `schedule` or `on`, never once, and a firing
  starts a session or continues the open session its key names
  (`manifest/v1/kinds.go`, `internal/triggers/engine.go`). No field names
  an existing session.
- A firing's session is created with `attended` false
  (`internal/server/triggers.go`), so a `question` it asks is answered at
  once as `unanswered` ([[039-questions]]). A call whose verdict is ask
  still waits, idle `tool_confirmation`, for a person who is not told.
- A continued firing is held while its session waits for a person, on a
  confirmation, a question or its budget (`waitsForPerson`,
  `internal/triggers/render.go`).
- `end_on_idle` ends a session `completed` at its first idle `end_turn`
  ([[004-session-log]]); a trigger sets it under `new`.
- No tool reads or writes a trigger. The harness tools are `spawn`,
  `message`, `advisor`, `question` and `history`; `tools.Confirmer` lets a
  tool hold a call for a person every time, which `publish` uses for a
  change of who can open an app ([[043-publishing-a-folder]]).
- `suspend` stops one trigger. Nothing stops every trigger of an owner.
- Nothing in the log or on the Session says whether a turn a trigger
  began found anything, so a client marks every run as new, and keeps
  the mark on its own side.
- A session's `limits.max_age` is `168h` unless the authorizer or the
  create sets less ([[004-session-log]]).

## Design

### Recommendations

| Item | Recommended | Weighed against, and why not |
|---|---|---|
| How a proposal reaches the person | the `trigger` call is held by `tools.Confirmer`: idle `tool_confirmation`, answered by `user.tool_confirmation` | a `question` first and then the call: the person would be asked twice, and a question in a session that is not attended is answered at once; a new event and stop reason: every client already shows a held call, and an ask never times out ([[012-permissions-and-approvals]]) |
| Who approves | the session's initiator, who becomes the trigger's owner; another sender's confirmation of a held `trigger` call is `forbidden` | any sender `session.send` allows: in an organization's session a member could make standing work that runs as someone else |
| A self-wake | a one-shot Trigger with `session.target`, applied by the runner as the initiator | a field of the Session read by the minute loop: a second firing pipeline with its own claims, leases and held messages, and a wake no trigger list would show |
| What a wake continues | the session that set it, with its whole context | a new session with the note as its message: the agent would lose what the conversation established, which is what the note cannot carry |
| A wake past the session's life | refused at the call: a wake lives inside the session's `expires_at` | extending the session: an agent could keep a session open forever by waking it |
| A long chain of wakes | the eleventh wake in a row with no word from the person is held for the person | refusing it: the agent's watch would end without the person deciding; no bound: an agent could wake itself for the session's whole life |
| Quiet or brought | `notify`, a harness tool, and `outcome` on the idle `session.status` | a marker in the final text: a client would parse prose; a field the model fills at every turn end: every turn of every session would carry it |
| A routine's runs | `session.notify: always` by default, so a schedule trigger applied today behaves as it does | `judged` by default: every routine a person already made would start staying quiet |
| Pausing everything | one hold per owner, `PUT /v1/triggers/hold`, read by the minute loop and the fire route | a client suspending every trigger in turn: a race with a wake set during the loop, and resuming would also resume what the person had paused one by one |
| Names | tools `trigger`, `wake`, `notify` | `routine`: a client's word for one kind of trigger; `remind`: names one use of a wake; `alert`: names one use of a notice |
| A proposal's trigger | the routine shape of `latere.ai/x/sdk-go/agents/routine`, written by its `Manifest`: every client lists, changes and deletes it as a routine | a `proposed-` name and a purpose annotation of the core's own: a second shape of a routine, which every client would learn to read as one (decided 2026-10-22) |
| A proposal's schedule | a routine's: a minute and an hour, every day or on the days listed, read by `routine.ParseSchedule` | any cron, bounded by an interval: a schedule no routine form shows |

### The Trigger kind's added fields

The rows below join [[022-triggers]]'s table; that spec still owns the
others.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `at` | RFC 3339 time with an offset | one of `schedule`, `on` and `at` is required | fire once at this time; the trigger is spent by its firing (below) |
| `session.target` | `ses_…` | none | under `continue` only: the open session every firing sends its message to, in place of the session its key names; the applier must be allowed `session.send` on it, asked at apply |
| `session.attended` | boolean | `false` | the `attended` of each session a firing starts ([[039-questions]]): its questions wait for the owner. Ignored with `session.target`, whose session keeps its own |
| `session.notify` | `always` or `judged` | `always` | whether every turn a firing begins brings its result to the person (`always`), or the agent decides with `notify` (`judged`). A wake's turn is `judged` whatever the field says |

The added fields' defaults are applied where the trigger fires and are
not written into the resolved spec, so a trigger that sets none of them
keeps its digest ([[003-manifest]]).

Apply refuses, as `invalid_manifest` with the field's path: `at` beside
`schedule` or `on`; `at` in the past; `session.target` without `policy:
continue`, beside `session.key`, or naming a session that is ended, gone,
or of another agent than `agent`; `session.notify: judged` for an agent
whose latest version does not name `notify`, whose every turn would be
quiet; and the name `hold`, which the hold's route below takes. A
firing whose agent version no longer names `notify` records `notified`,
as under `always`.

Two things are written by the server and ignored on input:

| Field | Where | Meaning |
|---|---|---|
| `origin` | the Trigger's `status`, kept across later applies | `{kind, sessionId, toolUseId, confirmedBy?}` when a session's call made the trigger: `kind` is `proposal` or `wake`, `confirmedBy` the subject who allowed a proposal. Absent for a trigger applied at the route. The questions carry it as `{kind, session_id, tool_use_id, confirmed_by?}`. A later call's origin, a change's or a pause's, is in its questions alone |
| `paused` | the Trigger's `status`, written when an apply suspends it, kept while it stays suspended, cleared when it resumes | `{why?, by, sessionId?, toolUseId?, at}`: who the suspend was applied as, when, and, for the `trigger` tool's `pause`, the agent's `why` and its call. A suspend at the route names neither `why` nor a call (added 2026-10-23, so a client shows who paused a trigger and why) |

A proposal's why is read in its held call, not stored on the trigger, so
the routine keeps the routine's shape; a wake's title is its
`metadata.displayName`.

### One-shot triggers, and spent

An `at` trigger runs through [[022-triggers]]'s pipeline once. The
minute loop takes it as it takes a schedule whose next fire time has
passed, with `at` as the scheduled time and the same dedupe string, so
one firing acts once across replicas.

A one-shot trigger is spent when its firing records `started`,
`continued`, `skipped_late`, `skipped_ended` or `refused`, and, as built,
`skipped_active` or `skipped_busy` too: a one-shot has one firing, so a
skipped one is spent as a skipped schedule's time is gone. A manual
firing at the fire route spends it the same way. The server deletes a
spent trigger with its firing in the same transaction
(`store.Triggers.SpendFiring`); what it did stays in the log of the
session it started or continued. A firing `held` keeps the trigger until
the held message is sent; one `failed` runs again at the next minute.

A firing under `session.target` whose session has ended or is gone is
`skipped_ended`, a new outcome of [[022-triggers]]'s table: it starts
nothing, since a new session would not hold the conversation the
message was written for.

### The hold

| Route | Action | Behavior |
|---|---|---|
| `GET /v1/triggers/hold` | `trigger.hold` with `held` absent | answers `{"held": bool, "since"?: time}` for the caller in the context its token names |
| `PUT /v1/triggers/hold` | `trigger.hold` with the `held` asked | sets or releases the hold, `{"held": true}` or `{"held": false}`, and answers as `GET` does |

The question names the owner as its resource, with `owner` and `held`.
The owner policy lets an owner hold and release their own; `trigger.hold`
is a new action of the vocabulary ([[006-identity]]). While an owner is
held:

- the minute loop fires none of their schedules and records nothing, as
  for a suspended trigger; on release a schedule starts from then, so it
  fires nothing it missed;
- the fire route answers `conflict` for any of their triggers, as for a
  suspended one, so a delivered event is dropped and not retried;
- a one-shot trigger whose time passes keeps it; on release it fires
  within the minute, `maxAge` aside, and a wake's message says when it
  was due (below);
- `wake` and `trigger` calls go on as without the hold: a wake set while
  held keeps its time, and the call's result tells the model that the
  person has paused every trigger.

A trigger that fires nothing while its owner is held records no
`skipped_*` row, so a released owner's counts are what ran.

### The `trigger` tool

`trigger` is a harness tool, as `question` is ([[039-questions]]): it is
added to the registry of the session's own thread when the agent's
`spec.tools` names it, and the hosted runner alone offers it, since only
a server keeps triggers. A thread is never offered it
([[013-threads-and-subagents]]).

| Name | Parallel | Effect | Input | Limits |
|---|---|---|---|---|
| `trigger` | no | `read` for every action (built: a tool's effect is one per tool, so a list and a pause run in every mode, plan included, and every other action is held by `tools.Confirmer` and blocked in plan) | `action`, and the fields of the table below | the bounds below |

| Field | Actions | Meaning |
|---|---|---|
| `action` | all | `list`, `propose`, `change`, `pause`, `resume` or `delete` |
| `id` | `change`, `pause`, `resume`, `delete` | a `trg_` id of a trigger the session's initiator owns, in the session's context, that runs the session's agent |
| `name` | `propose`, `change` | the name a person reads, at most `routine.MaxTitle` characters; it becomes `metadata.displayName` and each started session's title |
| `ask` | `propose`, `change` | what each firing asks, in words; the server escapes `{{` so nothing in it is read as a placeholder |
| `why` | `propose`, `change`, `pause` | one sentence the person reads: why the trigger exists, or why it is paused; at most `trigger.MaxPurposeLength` characters |
| `schedule`, `timeZone` | `propose`, `change` | a routine's schedule: five cron fields of a minute and an hour, every day or on the days of the week listed, and the IANA zone it is read in; one of `schedule` and `on` |
| `on` | `propose`, `change` | as the Trigger's `on` ([[022-triggers]]) |
| `notify` | `propose`, `change` | `always` or `judged`; `always` for a schedule and `judged` for `on` when absent |

What each action does:

| Action | Held for the person | What runs on allow, or at once |
|---|---|---|
| `list` | no | answers the owner's triggers of the agent, newest first, at most `trigger.MaxListed`: id, name, title, `schedule`, `timeZone`, the product of `on`, the ask, `suspend`, the next fire time and `origin`; from step 3 `at`, and a wake's target session id and title |
| `propose` | yes, every time | applies a new trigger, below |
| `change` | yes, every time | applies the trigger again with the given fields changed and every other field kept, as the SDK's routine change does |
| `pause` | no | sets `suspend`; the result tells the model to tell the person what it paused and why |
| `resume` | yes, every time | clears `suspend` |
| `delete` | yes, every time | deletes the trigger; sessions it started keep running |

A proposal is applied as the routine `routine.Manifest` of
`latere.ai/x/sdk-go/agents/routine` writes, the one shape every client
reads as a routine, named so that a retry of the same call applies the
same trigger:

| Field | Value |
|---|---|
| `metadata.name` | `routine.Name` of the session id, `/` and the call's `tool_use_id`: `routine-` and the first 20 hex digits of its SHA-256 |
| `metadata.labels["applied-by"]` | the name of the session's agent |
| `metadata.displayName` | `name` |
| `spec.agent` | the session's agent, by name, read in the owner's context; an apply whose name reads another agent is refused |
| `spec.schedule`, `spec.timeZone` | as given, in the routine's form |
| `spec.session.message` | `ask`, escaped |
| `spec.session.title` | `name`, escaped |
| `spec.skipIfActive` | `false`, so a run waiting for the person never skips the next |
| `status.origin` | `{kind: proposal, sessionId, toolUseId, confirmedBy}`, written by the server |

The routine's shape carries no annotation of its own, so `why` is read
in the held call alone, where a client words the offer from it. Until
roll step 4, `session.attended` and `session.notify` are left to their
defaults, as a routine a person makes through a client is.

#### A proposal on events (decided 2026-10-23)

The routine's shape names three ways to start: a schedule, every email
that arrives (`on: {product: mail, verbs: [received]}`) and every call of
the person's link (`on: {product: webhook, verbs: [received]}`). A
proposal's `on` is applied by the same `routine.Manifest`:

| The proposal's `on` | Applied as |
|---|---|
| exactly a routine's: `mail` or `webhook`, the one verb `received`, no `resources` and no `match` | the routine that starts from an email or a link, written by `routine.Manifest` with its `Starts`, under the routine's name, with the SDK's own words after the ask that hand the run what arrived; every client reads it as a routine |
| any other: a repository's pushes, more verbs, a filter narrowed by `resources` or `match` | the manifest `routine.Manifest` writes for the name, the ask and the title, with the filter as `spec.on` and the event after the ask (lines `internal/proposal` writes as the SDK writes its own, which render the event's product, verb, resource, actor, time and payload), under the name `event-` and the routine's 20 hexadecimal digits |

Both carry the label `applied-by`, the `displayName`, `skipIfActive:
false` and `status.origin`, and from roll step 4 `session.notify:
judged` unless the call names `always`. An `event-` trigger is listed,
changed, paused, resumed and deleted by its id and read by its
`status.origin`, as the SDK's list of the person's watches reads every
trigger a conversation offered. A change keeps a routine a routine: a
change of a routine to a filter no routine expresses is refused
`invalid_input`, and the agent proposes the new trigger instead.

Weighed against: every `on` under a `routine-` name, which
`routine.Read` would read as a routine on a schedule with no schedule, so
every client would show a routine it can neither show nor change; and
refusing every `on` but a routine's, which would leave an agent no way to
offer a watch on a repository's pushes, which 086 names.

A change, a pause and a resume apply the stored trigger again: a routine
through `routine.Manifest` over its stored document with its fields read
by `routine.Read` and the call's written over them, as the SDK's change
does, and any other trigger with the call's fields written into its
document, every other field kept. `status.origin` is the call that made
the trigger and is kept: a later call's origin is in its questions alone.

The bounds of a proposal are constants of `manifest/trigger`, read by
the tool's schema, its description and the check before the hold:

| Constant | Value | Bounds | Why this value |
|---|---|---|---|
| `MaxPurposeLength` | 200 | `why` | two sentences, shown under the name |
| `MaxListed` | 50 | the triggers `list` answers | enough to find a duplicate before proposing; a person with more manages them in a client |

The name's bound is the routine's own, `routine.MaxTitle` (60), and a
routine's schedule runs at most once a day, so no interval bound is
needed.

A proposal names no machine, resources, budget or limits: its sessions
run where the agent's own definition and the authorizer place them. The
trigger is owned by the session's initiator, in the session's context,
with `origin` `{kind: proposal, session_id, tool_use_id, confirmed_by}`.

Before a call is held, the tool checks what it can know from the input,
and answers a call that would be refused without asking anyone
([[012-permissions-and-approvals]]), as `invalid_input` with a text of
`prompts/results/trigger/`:

| Rule | Why |
|---|---|
| exactly one of `schedule` and `on`, each valid as the Trigger's | the same grammar the resolver checks at apply ([[022-triggers]]) |
| a schedule no routine runs on, a zone the server does not know, and a name, an ask or a why past the routine's checks | the checks `routine.New` holds a routine to, read by the tool and again by the server |
| `id` names a trigger the owner holds, of this agent, that is not a wake | a session never changes another agent's standing work, or another session's wake; asked of the server through `tools.Prober`, answered `not_found` as a trigger the caller cannot see, and checked again when a pause, which is not held, runs |
| `trigger.create` or `trigger.update` as the owner, asked before the hold without `confirmed_by`, denies | no person approves a call bound to fail; the reason is the result. The harness asks it through `tools.Prober`, the `Checker` that may reach the call's service |

The call is then held: `tools.Confirmer` names it, with its reason, so
it is `ask` in `confirm` and
`progressive`, whatever pattern, session pattern or decision service
([[037-decision-services]]) would allow it, and `block` in `plan`. Only a
`user.tool_confirmation` from the session's initiator closes it; another
sender's is refused `forbidden` at the send route. A deny may carry the
person's words as its `note`, which the model reads in the call's
result: that is how a person says "make it 8:00", and the agent proposes
again from the note. A client sends a reply typed in place of an answer
that way, and does not rely on a `user.message` denying a held call,
which [[039-questions]] records as not built.

On allow, the runner asks `trigger.create` (or `trigger.update`,
`trigger.delete`) again, now with `confirmed_by`, since the authorizer's
answer may have changed while the person decided, and then applies,
changes or deletes the trigger through the code of the trigger routes.
The call's result carries the trigger's id, name and next fire time; a
refusal at this point is its result with the reason, and nothing was
applied.

### The `wake` tool

`wake` is offered as `trigger` is: a harness tool of the session's own
thread, on the hosted runner, when the agent names it.

| Name | Parallel | Effect | Input | Limits |
|---|---|---|---|---|
| `wake` | no | `write` | `action`, `at` or `after`, `title`, `note` | the bounds below |

| Field | Required | Meaning |
|---|---|---|
| `action` | yes | `set` or `cancel` |
| `at` | one of `at` and `after`, for `set` | an RFC 3339 time with an offset |
| `after` | one of `at` and `after`, for `set` | a Go duration from now in hours and minutes, such as `90m` or `36h` |
| `title` | for `set` | one line the person reads: what the agent will look for, at most `trigger.MaxWakeTitle` characters |
| `note` | for `set` | what the agent tells itself: what to look at, what it knew, what to do with each answer; at most `trigger.MaxWakeNote` characters |

`set` applies, as the session's initiator, this one-shot trigger, and
appends `session.wake_set`:

| Field | Value |
|---|---|
| `metadata.name` | `wake-` and the session id's ULID in lowercase, so a session has one wake |
| `metadata.displayName` | `title` |
| `spec.agent` | the session's agent |
| `spec.at` | the time asked |
| `spec.session.message` | `note`, escaped |
| `spec.session.policy`, `spec.session.target` | `continue`, the session |
| `spec.maxAge` | `trigger.WakeMaxAge` |
| `metadata.labels["applied-by"]` | the session's agent, by name, as a proposal's: a client lists a wake's agent without reading its session (added 2026-10-23) |
| `status.origin` | `{kind: wake, sessionId, toolUseId, confirmedBy?}`, `confirmedBy` for a set the initiator allowed |

A session has at most one wake: a `set` while one is pending replaces it,
applying the same trigger again. `cancel` deletes it and appends
`session.wake_cleared`. Both are asked of the authorizer as
`trigger.create`, `trigger.update` or `trigger.delete` as the owner, with
`origin` `{kind: wake, session_id, tool_use_id}`.

The bounds are constants of `manifest/trigger`, read by the tool's
schema, its description, the server's check of a trigger whose `origin`
is a wake, and the OpenAPI text, so none is written twice. The check is
the server's, in the trigger route's apply, not the resolver's: the
resolver never reads `origin`, which is ignored on input, and the
server holds the session whose `expires_at` bounds the wake. The tool
checks the same bounds, by `trigger.CheckWake`, before anything is
decided:

| Constant | Value | Bounds | At the bound |
|---|---|---|---|
| `MinWakeDelay` | 10 minutes | how soon a wake may be | answered `invalid_input`, `too_soon`, naming the earliest time |
| `MaxWakeAhead` | 168 hours | how far ahead a wake may be | answered `invalid_input`, `too_late`, naming the latest time |
| the session's `expires_at` | its `limits.max_age` after its creation | how late a wake may be | the same: a wake lives inside the session's life |
| `MaxWakesWithoutPerson` | 10 | wakes in a row with no message, answer or confirmation from the person | the next `set` is held for the person, below |
| one pending wake | 1 | wakes a session holds | a `set` replaces the pending one |
| `WakeMaxAge` | 24 hours | how late a wake may fire, after a stop of the server | `skipped_late`, and `session.wake_cleared` with `skipped` |
| `MaxWakeTitle` | 80 | `title` | the schema's `maxLength` |
| `MaxWakeNote` | 4000 | `note` | the schema's `maxLength` |

Why these values. Ten minutes keeps a chain of wakes from being a loop:
at the floor, ten wakes take more than an hour and a half before the
person is asked. A look once a day for the session's default age of
seven days fits under ten with room for a few closer looks. A wake past
seven days, or past the session's life, is standing work, which the
agent proposes and the person approves.

The count is folded from the log: the wakes' `user.message` events since
the latest `user.message`, `user.answer` or `user.tool_confirmation` from
the session's initiator. The result of a `set` names the count, the
earliest and the latest time a wake may be set, so the model reads the
bounds where it acts. When the count has reached `MaxWakesWithoutPerson`,
`tools.Confirmer` holds the `set` with the reason
`prompts/results/wake/many.md`; the person's allow is their word, so the
count starts again, and their deny leaves the session without a wake.

A wake is cleared, and its trigger deleted, when the session ends for
any reason, is deleted, or is archived; the server appends
`session.wake_cleared` with `ended` where the log remains. A person, or
a client, deletes a wake as any trigger, at `DELETE /v1/triggers/{ref}`;
the server appends `session.wake_cleared` with `canceled` and the
sender. A session with a pending wake is not ended by `end_on_idle`: the
rule of [[004-session-log]] becomes "end `completed` at an idle
`end_turn` with no pending wake", so a trigger's run that sets a wake
stays open for it and ends at the idle after its last wake.

A wake set in a session on a person's own computer
([[080-device-machines]]) continues a session the device already let
in; it starts none, so the device's start decision is not asked again.
While the device is away, the wake's turn waits `machine_waiting` like
any turn.

A wake's turn spends from the session's own budget, as every turn does.
A session idle on `budget` holds its wake's message ([[022-triggers]]),
so a wake never spends past what the person allowed.

### The `notify` tool, and quiet

`notify` is a harness tool of the session's own thread, offered when the
agent names it, on every runner.

| Name | Parallel | Effect | Input | Limits |
|---|---|---|---|---|
| `notify` | no | `none` | `title`: one line a client shows the person, at most `MaxNoticeTitle` characters | one call a turn; a second is answered `invalid_input` |

`MaxNoticeTitle` is a constant of the `session` package, 80 characters:
a notification's line on a Mac and a list row's on a phone.

Effect `none` scores 0.0 and is `allow` in every mode. It changes
nothing outside the log: what a client does with a notice, a
notification on a Mac, a mark in a list, is the client's.

A turn whose first input is a trigger's `user.message` (one carrying
`firing_id`) ends with an outcome on its idle `session.status`:

| The turn | `outcome` |
|---|---|
| the trigger's `session.notify` is `always`, and the turn is not a wake's | `notified` |
| it called `notify` | `notified` |
| it waited for the person on the way (a question, a held call, a site, a connection) | `notified` |
| otherwise, ending `end_turn` | `quiet` |

A turn that ends on an error, a limit or a budget records no outcome; a
client shows it as it shows such a stop today. A turn a person began
records none.

The context tells the model, in `prompts/context/woken-v1.md`, on a turn
a trigger's message began under `judged` and on every wake's turn: that
nobody is waiting, that it calls `notify` only when what it found
deserves the person's attention now, that a `question` waits for the
person when the session is attended, and that ending the turn without
either leaves the person undisturbed.

### How a wake shows in the log

| Event | Appended by | Payload |
|---|---|---|
| `session.wake_set` | the runner, after the trigger is applied | `trigger_id`, `at`, `title`, `note`, `tool_use_id`, `count`: the wake's place in the chain |
| `session.wake_cleared` | the runner or the server | `trigger_id`, `reason` (`canceled`, `ended` or `skipped`), `tool_use_id` for the agent's cancel, `sender` for a person's or a client's |

A wake's firing is a `user.message` with the sender `trigger:<trg_id>`,
`firing_id` ([[022-triggers]]) and one added field, `wake`:
`{trigger_id, set_at, due_at, count}`; its content is the note. `set_at`
and `count` are read from the latest `session.wake_set` of the trigger,
`count` from 1. The context renders it after a line of
`prompts/wake/message-v1.md` that says when the wake was set and was
due, how many wakes in a row it is, and, for a wake that fired late,
after a hold or a stop of the server, that it is late: as built, later
than `session.WakeLateAfter` (two minutes, twice the trigger loop's
pass) past `due_at`. The fold clears the Session's wake at that message.

`session.wake_set` is the runner's record of what it applied and is not
redactable, as a status is not; the note it carries is also in the
call's `agent.tool_use`, which is.

The idle `session.status` gains `outcome` (above). The Session gains
three folded fields, which every client reads without reading the log:

| Field | Type | Meaning |
|---|---|---|
| `wake` | object | the pending wake: `trigger_id`, `at`, `title`, `count`; absent when none |
| `outcome` | string | `quiet` or `notified`, of the latest turn that recorded one |
| `notice` | object | the latest `notify`: `title`, `seq`, `at` |

The sessions list answers them as it answers every Session field
([[015-api]]); filtering by them is left out until a client needs it.

### The authorizer

| Question | Added fields |
|---|---|
| `trigger.create`, `trigger.update`, `trigger.delete` asked for a session's call | `origin`: `{kind, session_id, tool_use_id, confirmed_by?}`, asked as the owner stored for the session, its initiator, with `org_id` of the session's context as the claims, the way a firing asks ([[022-triggers]]); a create or an update also carries `at` for a one-shot, in RFC 3339, and `notify`. A `change`, `pause`, `resume` and `delete` carry `kind: proposal`, a wake's set and cancel `kind: wake` |
| `trigger.list` asked for a session's `list` | `owner` and `agent` |
| `session.create` asked for a firing | `attended` |
| `trigger.hold` | new: `owner`, `held` |

An authorizer that does not know `origin` reads a session's call as its
owner's own apply. An installation's authorizer is rolled first, so it
decides the new questions from their first asking; until it learns
`trigger.hold`, it denies it, and the hold route answers `forbidden`.

### Roll order

1. This spec's manifest fields, one-shot triggers, the `trigger` tool's
   `list` and `propose` of a schedule, the held call, and `origin`. An
   agent that does not name `trigger` sees nothing new.
2. `change`, `pause`, `resume` and `delete`.
3. `wake`, `session.wake_set` and `session.wake_cleared`, the `wake`
   field of the message and the Session, `end_on_idle` with a pending
   wake.
4. `notify`, `outcome`, `notice`, `session.attended` and
   `session.notify`.
5. The hold.

Each step is a release; an installation's authorizer rolls before the
release whose questions it answers.

## What a hosted installation needs

Latere's platform is the first: it is the installation's authorizer, it
delivers events at the fire route, and it runs the managed agent of
Latere Chat. In order:

| Need | Why |
|---|---|
| The authorizer answers `trigger.create`, `trigger.update` and `trigger.delete` asked with `origin`, holds `confirmed_by` to the owner, and caps the standing triggers and the pending wakes one person holds | the core asks as the owner with no token of theirs; a person's allow is what makes standing work theirs |
| The managed agent's next version names `trigger`, and its instructions say when to offer standing work (an ask that repeats, "tell me when"), to say the schedule back in `name` and `why`, and not to offer again in a conversation after a no | the agent makes a routine when the person accepts its offer |
| Then `wake` and `notify`, with instructions for a follow-up inside a conversation, a note that carries what the agent knew, quiet when nothing changed, and a cancel when the watch is done | the self-wake and the judgment |
| The authorizer learns `trigger.hold` | pausing everything |
| Events from a person's connections (a calendar event, a repository change, a page change) delivered at the fire route, from the change notifications its connections service receives | an event wake beyond the mail, links and repository pushes the platform delivers today |

## What the Go SDK needs

`latere.ai/x/sdk-go`'s `agents` package owns the shape Latere Chat and
Latere Studio read and write:

| Need | Why |
|---|---|
| A routine read with its `origin`, so a client marks one a conversation offered; the routine itself is already the SDK's shape, listed, changed and deleted as any | a routine the agent made on the person's yes shows in both clients' routines, marked as offered |
| The person's watches in one list: routines, event triggers the agent proposed, and wakes, each with its purpose, its next look, its origin and, for a wake, its session | both clients show what is watching for the person and why from one reading |
| The hold, read and set | pausing everything |
| The Session's `wake`, `outcome` and `notice`, and a held `trigger` call read as an offer: its action, name, why, schedule or event, notify, and the trigger a change changes | both clients word an offer and a quiet run alike |

## Not in this spec

The words a person reads (a client's); notifications outside a client,
by mail or a phone; the producers of connection events and the
signatures they verify; a proposal that names a person's own computer as
its machine, which stays a person's own apply; a filter of the sessions
list by `outcome`; a wake in a local session of `topos run`, which has no
server to keep the trigger.

## Progress

Roll step 1 is built, for the offer the decision of 2026-10-22 orders
first: the `trigger` tool's `list` and `propose` of a schedule, held for
the session's initiator, refused before the hold through `tools.Prober`,
blocked in plan mode, applied as the initiator in the routine's shape
with `status.origin`, and reached by a runner process over the internal
listener (`POST /internal/v1/leases/{session}/triggers/{list,check,apply}`).

Roll step 2 is built: `change`, `pause`, `resume` and `delete`, and a
`propose` on events, decided above. The check and the apply routes of the
internal listener carry every action in the proposal's `action`. `list`
answers each trigger's id and ask, which a change names.

Roll step 3 is built: `at` and its spending, `session.target` and
`skipped_ended`, the `wake` tool with its bounds and its eleventh set
held for the initiator alone, `session.wake_set` and
`session.wake_cleared`, the message's `wake`, the Session's `wake`,
`end_on_idle` with a pending wake, and the wake cleared on the session's
end, expiry, deletion and archive and recorded on a person's delete.
The internal listener carries a wake's set and cancel
(`POST /internal/v1/leases/{session}/triggers/{wake,unwake}`). The
`trigger` tool's list answers a wake's `at` and its session. The
Trigger's `status.paused` is built, and a proposal on events reads the
SDK's own shape of it (`latere.ai/x/sdk-go/agents/routine` v0.10.0:
`EventName`, `EventMessage`, `StartsOn`), so `internal/proposal` holds no
copy.

`session.attended`, `session.notify`, `notify` and the hold are not
built.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A proposal of a schedule is held in `confirm` and `progressive` whatever pattern or decision service would allow it, and blocked in `plan` | `harness.TestACallItsToolHoldsWaitsForAPerson`, `harness.TestPlanModeBlocksAHeldCall`, `internal/proposal.TestEveryProposalWaitsForItsPerson` | built |
| A proposal that the input or the authorizer would refuse is answered at once and never held | `harness.TestAProbedCallAsksNobody`, `internal/proposal.TestAProposalIsCheckedBeforeAnyoneIsAsked`, `internal/server.TestAProposalTheAuthorizerRefusesIsARefusal` | built |
| The initiator's allow applies the trigger as the initiator, in the session's context, in the routine's shape with `origin`, and a retry of the same call applies the same trigger; another sender's confirmation is `forbidden` | `internal/server.TestAnAllowedProposalIsTheOwners`, `internal/server.TestOnlyTheInitiatorConfirmsATrigger`, `internal/runnerrole.TestARemoteLeaseAsksTheTriggerQuestions` | built |
| A message in place of an allow denies the proposal with the message as its note | the harness's deny of a held call by a message (spec 012), which a `trigger` call takes as any held call | built |
| `pause` sets `suspend` without asking; `change`, `resume` and `delete` are held; none touches a trigger of another agent or a wake | `internal/proposal.TestOnlyAListAndAPauseRunWithoutThePerson`, `internal/server.TestTriggerActionsOnlyTouchTheAgentsOwn` | built |
| A proposal on events is the routine that starts from an email or a link when a routine expresses it, and an `event-` trigger with the filter as its `on` otherwise; a change keeps a routine a routine | `internal/proposal.TestAProposalIsWrittenInTheRoutinesShape`, `internal/server.TestAnEventProposalIsARoutineOrAnEventTrigger` | built |
| A schedule no routine runs on is refused before the hold | `internal/proposal.TestAProposalIsCheckedBeforeAnyoneIsAsked` | built |
| An `at` trigger fires once across two replicas and is deleted when spent; a held one stays until sent | `internal/store/postgres.TestAOneShotFiresOnce` (postgres tier), `internal/triggers.TestASpentTriggerIsDeleted`, the stores' conformance case `trigger spend` | built |
| A trigger under `session.target` continues that session and never another; an ended or gone one is `skipped_ended` | `internal/triggers.TestATargetIsContinuedOrSkippedEnded`, `manifest.TestTheTriggerFieldsOfSpec086` | built |
| A wake continues the session that set it with its note and `wake` on the message; a wake of an ended session is `skipped_ended` | `internal/server.TestAWakeContinuesItsSession` (the message's `wake` is the server's), `session.TestTheSessionFoldsItsWake`, `harness.TestAWakesMessageSaysWhenItWasDue` | built |
| A wake sooner than `MinWakeDelay`, later than `MaxWakeAhead` or past the session's `expires_at` is refused with the bound named; a second `set` replaces the first | `internal/proposal.TestWakeBounds` (the tool lives beside the trigger tool), `internal/server.TestAWakeIsHeldToItsBoundsAtTheServer`, `internal/server.TestAWakeContinuesItsSession`, `manifest/trigger.TestAWakeLivesInsideItsBounds` | built |
| The eleventh wake in a row with no word from the initiator is held, and an allow starts the count again | `harness.TestTheEleventhWakeAsks`, `internal/proposal.TestTheEleventhWakeWaitsForThePerson`, `session.TestWakesInARowCountFromTheInitiatorsWord`, `internal/server.TestOnlyTheInitiatorConfirmsAWake` | built |
| A session's end, deletion or archive deletes its wake and records `session.wake_cleared`; a person's delete records it with the sender | `internal/server.TestAWakeEndsWithItsSession` | built |
| `end_on_idle` leaves a session with a pending wake open and ends it at the idle after its last wake | `harness.TestEndOnIdleWaitsForAWake`, `harness.TestACanceledWakeLetsTheSessionEnd` (the rule is the harness's) | built |
| A pause records why and by whom in `status.paused`, kept while suspended and cleared on resume | `internal/server.TestAPauseRecordsWhyAndByWhom` | built |
| A turn a trigger began records `notified` under `always`, after `notify`, or after a wait for the person, and `quiet` otherwise; a person's turn records none; the Session folds `wake`, `outcome` and `notice` | `session.TestTheOutcomeOfATriggersTurn` | not built |
| A firing of a trigger with `session.attended` starts an attended session, and its question waits | `internal/server.TestAnAttendedTriggerWaitsForItsOwner` | not built |
| While an owner is held, the minute loop fires nothing of theirs and records nothing, the fire route answers `conflict`, and a one-shot due during the hold fires at release with its lateness in the message | `internal/server.TestAHoldStopsEveryTrigger` | not built |
| Every `trigger.*` question a session's call asks carries `origin`, asked as the owner with `org_id` alone; `trigger.hold` names `owner` and `held` | `internal/server.TestTheQuestionsOfASessionsTriggerCalls` | not built |
| The three tools' descriptions hold to their constants, and an instruction task each: a repeating ask is proposed with its schedule said back, a follow-up is a wake with a note, and a look that finds nothing ends quiet | `harness.TestToolTextsReadTheirBounds`, `test/tasks/instructions/propose/`, `test/tasks/instructions/wake/`, `test/tasks/instructions/quiet/` | not built |
