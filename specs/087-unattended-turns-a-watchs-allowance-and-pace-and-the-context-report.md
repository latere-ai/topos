---
title: "Unattended turns, a watch's allowance and pace, and the context report: a turn a trigger began decides by the session's mode without the person's remembered allows and says so to a decision service, a trigger's weekly allowance the server enforces with skipped_budget and a proposal carries as the agent's estimate, the spend and pace it writes and tells the turn, and the context every model request records"
status: drafted
track: core
depends_on: [.archive/004-session-log.md, .archive/007-models.md, 010-context.md, 012-permissions-and-approvals.md, 015-api.md, .archive/022-triggers.md, .archive/037-decision-services.md, 086-proactive-agents.md]
affects: [harness/, session/, internal/decisions/, internal/triggers/, internal/store/, internal/server/, manifest/v1/, manifest/trigger/, prompts/, api/openapi.yaml, docs/]
effort: medium
created: 2026-10-23
updated: 2026-10-23
author: changkun
---

# Unattended turns, a watch's allowance and pace, and the context report

## Overview

[[086-proactive-agents]] lets an agent run with nobody watching: a
routine fires, an event arrives, a wake comes due. Three things follow
from that, and a fourth a client needs beside them.

1. **Unattended turns follow the session's approval mode, and nothing
   more is added.** `plan`, `confirm` and `progressive`, with or
   without a decision service, decide as they do today. Two rules
   only: an allow the person gave with `remember` does not apply on a
   turn a trigger's message began, since the person approved it with
   that context in front of them; and every decision recorded, and
   every question a decision service is asked, carries `attended`, so
   a service can weigh a decision nobody watches.
2. **A watch's allowance.** A trigger may name a weekly allowance. The
   server counts what the turns its firings began cost over a rolling 7
   days, and a firing while that spend is at the allowance starts
   nothing and records `skipped_budget`. Nothing needs a manual resume.
   The `trigger` tool's proposal carries the agent's estimate of what
   the watch's looks cost in a week as that allowance, which the person
   reads in the offer and the server enforces.
3. **Pace.** The server writes the trigger's spend, its cost per look
   and what the window is on course to cost on `status.spend`, and
   tells each turn a trigger or wake began the same numbers in one
   line, with whether it is ahead of pace. The server enforces the
   ceiling; the model only paces.
4. **The context report.** Every `model.request` records the model's
   input window, the tokens the request used, and the parts the
   harness assembled, and the Session folds the latest.

Each choice below is a recommendation until this spec is validated;
the table that opens the design weighs each against its alternative.

## Current state

- A call is decided by the mode, the lists, the session's remembered
  patterns and, in `progressive`, a decision service
  ([[012-permissions-and-approvals]], [[037-decision-services]]),
  whoever began the turn. A pattern remembered in a conversation
  applies on every later turn of the session, a wake's included.
- `agent.tool_use` records the verdict, the reason, the mode, the score
  and the review probability; nothing says whether a person began the
  turn. The prediction a decision service is asked carries the action
  and its environment and nothing more.
- A trigger's spend is bounded by `maxActive` times each session's
  budget and past that by the installation ([[022-triggers]]). Nothing
  bounds what a trigger spends in a week, and nothing tells a woken
  turn what its looks have cost.
- `model.request` records the request's usage and cost. Nothing records
  the model's window or what the request was made of, and the Session
  says nothing of how full its context is. The harness already
  estimates a request's tokens for compaction ([[010-context]]).
- The Session's `context` is the text the allow of its create attached
  ([[058-what-a-create-allow-attaches]]).

## Design

### Recommendations

| Item | Recommended | Weighed against, and why not |
|---|---|---|
| Which turn is unattended | a turn whose first input is a trigger's `user.message` (one with `firing_id`, a wake's included), as `session.TriggerTurn` reads it; a turn a person's message, answer or confirmation began is attended | the Session's `attended` ([[039-questions]]): it says whether a client answers the session's questions, and an attended trigger session still runs its turns with nobody watching; a session-wide flag: a person who answers a held call in a woken turn is watching the turn that call resumes |
| What an unattended turn drops | the session's remembered patterns alone; `always_allow`, `always_confirm`, the mode, the thresholds, the network, a decision service and a tool's hold decide as on any turn | a woken-only permission table or a stricter mode: a second policy a person would have to keep in step with the first; dropping `always_allow` too: a list the agent's owner or the organization wrote stands whoever watches |
| What a call the mode asks about does | waits for the person as a held call does today ([[012-permissions-and-approvals]]) | refusing it on a woken turn: an agent that is refused routes around the refusal |
| How a decision service learns of it | `attended` in the action's `attributes`, which Limen's contract reads as one more observable fact | a top-level field: a service that keeps to its contract never reads it |
| The allowance's unit | `spec.budget.perWeek`, an integer of USD micros, the unit the session's spend, the status and the question count in | a decimal USD string as `spec.session.budget.maxCost`: the status and the question would carry one unit and the manifest another |
| What an allowance counts | every turn a firing of the trigger began, in the sessions it started and the sessions it continued, and the turns wakes began in the sessions it started | the whole of each session it started: a person's own turns in it are attended and theirs; a wake's own allowance: a wake is made by a tool that names none, and a new wake is a new trigger |
| A proposal's allowance | the `trigger` tool's `perWeekUsd`, a decimal USD amount: the agent's estimate of a week of looks, from its model's price the tool's description gives and a look's likely size, applied as `spec.budget.perWeek`; Topos passes it through, and whether a proposal without one is refused is the installation's authorizer's | an allowance the server derives from the schedule: it would know neither a look's size nor what the person will pay; refusing a proposal without one in the core: an installation without caps would lose every offer it makes today |
| A wake past its session's allowance | skipped, as the trigger's own firings are | counted and never refused: an agent could wake past the ceiling the person set |
| The window | the rolling 7 days before the firing | a calendar week: a burst at its turn, and a week that starts on a different day in every time zone |
| At the allowance | `skipped_budget`, a new outcome; a one-shot keeps its trigger and tries again each minute until it fires or `maxAge` passes | suspending the trigger: it would need a person to resume it once the window moved |
| Where a look's cost comes from | the session logs, read at each firing: the log already holds every cost, and a wake's firing row is deleted with its spent trigger | a cost column on each firing row: a second record of what the log holds, and none for a wake's look |
| When `status.spend` is written | at each firing that counts toward the trigger, before its look | on every read: a list of fifty triggers would read every log of every watch |
| How the pace reaches the turn | the firing's `user.message` carries `spend`, and the fold renders one line before it, beside the woken context of [[086-proactive-agents]] | the runner asking the server at each turn: only the hosted runner could, and the log would not say what the turn was told |
| The projection | the spend of one 7-day window: the window that ends 7 days after the oldest look it counts | the next 7 days after the spend so far: two weeks against a weekly allowance, ahead of pace at half of it |
| The report's `used` | the provider's prompt tokens when it answers them, else the harness's estimate, with `source` | the estimate alone: a provider's count is exact where it exists |
| The report's parts | the harness's estimate of each part, scaled so the parts sum to `used`; `free` is the window less `used` | raw estimates: parts that do not add up to what was used |
| The Session's field | `context_report` | `context`: the Session's `context` is the attached text of [[058-what-a-create-allow-attaches]], and a client reads it today |

### Unattended turns

A turn is unattended when its first input, after the status that
closed the session's own thread's previous turn, is a trigger's
`user.message`: one carrying `firing_id`, a wake's among them. Every
other turn is attended. A subagent's thread is in the turn of the
session's own thread that drove it. A turn that resumes after a
person's `user.tool_confirmation` or `user.answer` is attended: the
person is answering it.

On an unattended turn the harness decides each call as on any turn,
with one difference: the patterns the session's
`user.tool_confirmation` events added with `remember` are not applied.
`always_allow` and `always_confirm`, merged from the authorizer and the
agent, the mode, the thresholds, the session's network, a decision
service and a tool's hold (`tools.Confirmer`) decide as they do. A call
the result asks about waits for the person, idle `tool_confirmation`,
as today; a `user.tool_confirmation` with `remember` given on an
unattended turn adds its pattern for the session's attended turns.

Every decision recorded carries whether its turn was attended:

| Event | Added field |
|---|---|
| `agent.tool_use` | `attended`: boolean, written on every call from this spec; absent on events written before it |
| `approval.requested` | `attended`, the same |

A decision service is asked with it ([[037-decision-services]]): the
action of `POST /v1/predictions`, and of `POST /v1/decisions` when it
carries one, holds `attributes: {"attended": true|false}`. Limen reads
`attributes` as observable facts of the action, learns a person's
approvals with it, and needs no change to weigh a call nobody watches.

### A watch's allowance

The Trigger kind gains one field ([[022-triggers]] owns the others):

| Field | Type | Default | Meaning |
|---|---|---|---|
| `budget.perWeek` | integer, USD micros, above zero | none | the most the looks counted toward the trigger may cost in a rolling 7 days; a trigger with none has no allowance |

Apply refuses `budget.perWeek` at or below zero as `invalid_manifest`
with the field's path. A trigger that names no `budget` keeps its
digest ([[003-manifest]]). A change through the `trigger` tool keeps
the allowance, since a change applies the stored document with the
call's fields written over it ([[086-proactive-agents]]).

#### A proposal's allowance

The `trigger` tool of [[086-proactive-agents]] takes one more input:

| Field | Actions | Meaning |
|---|---|---|
| `perWeekUsd` | `propose`, `change` | a decimal USD amount above zero with at most six fraction digits: the agent's estimate of what the watch's looks cost in a week, said back to the person in `why`. Applied as `spec.budget.perWeek`, in micros; a `change` that names it replaces the allowance, and one that does not keeps it |

The tool checks it before the call is held, as it checks every input,
and answers `invalid_input` for an amount at or below zero or with more
than six fraction digits. The questions `trigger.create` and
`trigger.update`, asked before the hold and again on the allow, carry
it as `budget` (below), so an authorizer that refuses a proposal
without an allowance, or above the person's share, refuses it before
anyone is asked. Topos refuses none for lacking one: a proposal without
`perWeekUsd` applies a trigger with no allowance, as before this spec.
The held call's input holds `perWeekUsd`, so a client words the offer's
weekly cost from it, as it words the rest of the offer
([[086-proactive-agents]]).

The tool's description tells the model to estimate from its model's
price, which it states per million input and output tokens from the
catalog ([[007-models]]), and from a look's likely size: what one look
reads and writes, its calls and searches, times the looks of a week.
It says the estimate is the ceiling the server enforces: once a week's
looks cost it, no look starts until the oldest leave the 7 days. A
model the catalog prices at nothing is told to estimate from a look's
size alone. `list` answers each trigger's allowance and its week's
spend, which a change reads.

#### Looks, and what counts

A look is a turn a trigger's message began. Its cost is what the log's
events from that `user.message` through the status that closes the
turn of the session's own thread add to the session's spend
([[007-models]]): model requests and the tool results a service
charged for, in every thread. It ends earlier at the next `user.message`
of the session's own thread, so two messages that open one turn are
not counted twice. A turn still running counts what it has spent so
far. Events a fork copied from its parent are not counted.

A trigger's spend is the cost of the looks counted toward it whose
message is in the 7 days before now:

| A look | Counts toward |
|---|---|
| begun by a firing of the trigger, in a session it started or a session it continued | the trigger |
| begun by a wake, in a session the trigger started (its `trigger_id`) | the trigger |
| begun by a wake, in a session no trigger started | none: the session's own budget bounds it |

The trigger a firing counts toward is the trigger itself, or, for a
wake, the trigger that started the session the wake continues. The
server reads the sessions a trigger's firings started or continued in
the window, its sessions still open and its `session.target`.

#### At the allowance

When a firing is decided, and again when a held firing is sent, the
server computes the spend of the trigger it counts toward. When that
trigger names `budget.perWeek` and the spend is at or past it, the
firing starts nothing, sends nothing and records `skipped_budget`, a new
outcome of [[022-triggers]]'s table, counted in `status.counts` as
`skippedBudget`.

| The firing | At the allowance |
|---|---|
| a schedule's | `skipped_budget`; the schedule fires again at its next time |
| an event's | `skipped_budget`; a redelivery of the same event is decided again |
| a one-shot's, a wake's included | `skipped_budget`; the trigger is not spent, and the minute loop tries it again at each pass until the spend falls below the allowance and it fires, or `maxAge` passes and it is `skipped_late` |
| a held one, when its session stops waiting | `skipped_budget`; a held one-shot goes back to its time and is tried again as above |
| a manual one, at the fire route | `skipped_budget`, answered `200` with the firing |

Nothing is suspended and nothing needs a manual resume: the window rolls
past the oldest looks and the next firing runs. `skipped_budget` is the
one recorded outcome a later claim of the same firing takes again, as
`failed` is, so a one-shot's single firing can run once the spend
allows it; the count it added is taken back when it is.

#### The question

`trigger.create` and `trigger.update` carry `budget:
{"per_week_usd_micro": n}` when the trigger names an allowance, so an
installation's authorizer can refuse a trigger without one, or with one
above what it allows. A trigger with no allowance asks as before. A
session's proposal asks with the allowance its `perWeekUsd` names, with
the `origin` of [[086-proactive-agents]] beside it.

### Pace

The server writes `status.spend` on the trigger at each firing that
counts toward it, before the firing's look, from the logs it read for
the ceiling:

| Field | Type | Meaning |
|---|---|---|
| `windowDays` | integer | 7 |
| `spentUsdMicro` | integer | the trigger's spend: the cost of the looks counted toward it in the last 7 days |
| `perWeekUsdMicro` | integer | the allowance, absent when the trigger names none |
| `perLookUsdMicro` | integer | the mean cost of the latest `trigger.PaceLooks` (5) looks of the window that ended; absent when none has |
| `expectedLooks` | integer | the looks expected from the firing to the end of the window, the firing's own included (below) |
| `projectedUsdMicro` | integer | `spentUsdMicro` plus `perLookUsdMicro` times `expectedLooks`; absent with `perLookUsdMicro` |

The window ends 7 days after the oldest look the spend counts, when
that look leaves the rolling window, or 7 days after the firing when
the spend counts none. `projectedUsdMicro` is so the spend of one whole
window, the one whose end the ceiling reaches first. `expectedLooks` is
the firing's own look and then:

| The trigger counted toward | More looks expected |
|---|---|
| on a `schedule` | its fire times after the firing and before the window's end |
| at `at` | none: its one look is the firing's own |
| on `on` | its looks of the last 7 days spread over the time left in the window: their count times the time left, over 7 days or the trigger's age when it is younger, rounded |
| suspended | none |

`status.spend` is absent on a trigger that has not fired since this
spec. It holds the looks that ended before the firing that wrote it, so
it reads the week as of the trigger's last firing.

#### The line a woken turn reads

A firing's `user.message` gains one field, `spend`, written by the
server beside `firing_id`, `wake` and `notify`, with the numbers of
`status.spend` in the log's case and the trigger they count toward:

| Field | Meaning |
|---|---|
| `trigger_id` | the trigger the look counts toward: the sender's trigger, or for a wake, the trigger that started the session |
| `window_days`, `spent_usd_micro`, `per_week_usd_micro`, `per_look_usd_micro`, `expected_looks`, `projected_usd_micro` | as `status.spend`, absent where it is |

A wake of a session no trigger started counts toward none, and its
message carries no `spend`. The fold renders one line of
`prompts/context/pace-v1.md` before a message that carries `spend`,
after the woken context and before a wake's line, so the request's
cached prefix stays the same. The line gives the spend, the allowance,
the cost of a look and the projection. When the projection is past the
allowance it says the watch is ahead of pace, that the server starts no
look while the week's spend is at the allowance, and what to do: for a
wake, set the next one later and keep this look short; for a trigger's
look, keep the check short, or propose a change with the `trigger` tool
that looks less often, which the person confirms.

### The context report

Each `model.request` a thread's step or a compaction sends records
`context`:

| Field | Type | Meaning |
|---|---|---|
| `window` | integer | the model's input window from the catalog ([[007-models]]); 0 when the catalog names none |
| `used` | integer | the input tokens the request used |
| `source` | `provider` or `estimate` | `provider` when the response counted its prompt tokens (input with the cache reads and writes), `estimate` when it did not, a request that failed or was cut short, and `used` is the harness's estimate of [[010-context]] |
| `parts` | list of `{part, tokens}` | the request's parts in this order: `instructions` (the system prompt, the date included), `tools` (the tool definitions), `messages` (the conversation), `files` (the files persons brought that the request names and carries, [[079-files-a-person-brings-into-a-message]]), `summary` (the summary of compacted history), and `free` (`window` less `used`, never below 0) |

The harness estimates each part with the same counter it estimates the
request with, and scales the estimates so the parts but `free` sum to
`used`; the rounding's remainder goes to the largest part. A request
beside the conversation, a title's ([[083-a-title-that-follows-the-conversation]]),
records none.

The Session gains `context_report`: the `context` of the latest
`model.request` of the session's own thread that records one, absent
before any. A subagent's requests and a title's do not change it. The
sessions list answers it as it answers every Session field
([[015-api]]).

### The OpenAPI document

The document describes `budget` on a Trigger, which a session's
proposal writes from the `trigger` tool's `perWeekUsd`, `status.spend`,
`skippedBudget` among the counts, `spend` on a `user.message`,
`attended` on `agent.tool_use` and `approval.requested`, `context` on
`model.request`, and `context_report` on the Session.

### Roll order

1. The context report: `context` on `model.request` and the Session's
   `context_report`. The harness and the fold alone.
2. Unattended turns: `attended` on `agent.tool_use` and
   `approval.requested`, the remembered patterns left out of an
   unattended turn, and `attributes.attended` sent to a decision
   service.
3. The allowance: `budget.perWeek`, the question's `budget`,
   `skipped_budget` and its claim, `status.spend`, the message's
   `spend` and the pace line, and the `trigger` tool's `perWeekUsd`.

The three ship in one release. An installation's authorizer learns
`budget` before that release reaches it, or a trigger with an allowance
is decided as one without.

## What a hosted installation needs

Latere's platform is the first: its authorizer answers the trigger
questions, Limen is its decision service for auto mode (`progressive`
with a decision service), and Latere Chat and Latere Studio show the
person's watches.

| Need | Why |
|---|---|
| The authorizer reads `budget` on `trigger.create` and `trigger.update`, and refuses a session's proposal without one, or with one above the person's share, as it chooses | the ceiling the person approves is the one the server enforces; the authorizer decides which watches may run without one, and Topos refuses none |
| The managed agent's next version says, in its instructions, to say the weekly estimate back in the offer's `why` | the person approves a cost they read |
| Limen reads `attributes.attended` on each prediction, as it reads every attribute | auto mode can ask on a woken turn where it would allow in a conversation, from the person's own answers |
| A client that makes a routine or accepts an offer sets `spec.budget.perWeek`, and shows `status.spend` and `skippedBudget` | the person sets the allowance and sees the week |
| The image of this release pinned | the server writes and enforces all of it |

## What the Go SDK needs

| Need | Why |
|---|---|
| `agents/routine` reads and writes `spec.budget.perWeek`, keeps it through a change, and reads `status.spend` and `counts.skippedBudget` | both clients set an allowance on a watch and show its week |
| The Offer read from a held `trigger` call reads `perWeekUsd` | both clients show the weekly cost the person approves |
| The person's watches list carries each one's spend, and for a wake the spend of the trigger it counts toward | a wake's looks count toward the routine that started its conversation |
| The Session's `context_report` and `model.request`'s `context` | both clients show how full a conversation's context is and what fills it |
| `user.message`'s `spend` read on a woken turn | a client shows what a look cost and whether the watch is ahead of pace |

## Not in this spec

An allowance on a session a person started; a monthly or daily window;
a notice to the person when a watch reaches its allowance (a client
reads `skipped_budget`); an allowance on a wake; the authorizer's caps
and shares; the words a client shows.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A remembered pattern allows a call on a person's turn and not on a trigger's or a wake's turn, where the mode decides; `always_allow` allows on both; a turn resumed by a person's confirmation is attended | `harness.TestAnUnattendedTurnForgetsWhatThePersonRemembered` | not built |
| `agent.tool_use` and `approval.requested` record `attended`: true on a person's turn, false on a trigger's turn and in a subagent's thread it drove | `harness.TestEveryDecisionRecordsWhetherItWasAttended` | not built |
| A decision service's prediction carries `attributes.attended`, and a decision that carries its action does too | `internal/decisions.TestTheServiceIsToldWhetherAnyoneWatches` | not built |
| `budget.perWeek` above zero applies, at or below zero is `invalid_manifest`, and a trigger without it keeps its digest | `manifest.TestATriggersAllowance` | not built |
| A look's cost runs from its message through the turn's closing status or the next message, counts a running turn so far, marks a wake's, and skips copied events | `session.TestLooksAreTheTurnsTriggersBegan` | not built |
| A firing at the allowance records `skipped_budget` and starts nothing, and fires again once the window rolls past the oldest look, with no resume | `internal/triggers.TestAFiringAtItsAllowanceStartsNothing` | not built |
| A one-shot at the allowance keeps its trigger, is tried again at the next pass, fires when the spend falls, and is `skipped_late` past `maxAge` | `internal/triggers.TestAOneShotAtItsAllowanceWaits` | not built |
| A wake's look counts toward the trigger that started its session and is skipped at that trigger's allowance; a wake of a session no trigger started counts toward none | `internal/triggers.TestAWakeCountsTowardTheTriggerThatStartedItsSession` | not built |
| A held firing sent past the allowance is `skipped_budget`, and a held one-shot goes back to its time | `internal/triggers.TestAHeldFiringIsHeldToTheAllowance` | not built |
| A claim takes a `skipped_budget` firing again and takes its count back, and a trigger's spend is kept, in every store | the stores' conformance case `trigger spend`, on Postgres through Testcontainers | not built |
| `status.spend` holds the spend, the allowance, the mean of the latest five ended looks, the expected looks of a schedule, a one-shot, an event trigger and a suspended one, and the projection over the window that ends 7 days after the oldest look | `internal/triggers.TestTheSpendOfAWatch` | not built |
| `trigger.create` and `trigger.update` carry `budget` when the trigger names one | `internal/server.TestTheTriggerQuestionsCarryTheAllowance` | not built |
| The `trigger` tool's `perWeekUsd` is applied as `spec.budget.perWeek` on a propose and a change, a change without it keeps the allowance, an amount at or below zero or past six fraction digits is refused before the hold, and the questions asked before the hold and on the allow carry it | `internal/proposal.TestAProposalCarriesItsAllowance`, `internal/server.TestAProposalsAllowanceIsTheTriggers` | not built |
| The tool's description states the model's price and that the estimate is the ceiling the server enforces, and `list` answers each trigger's allowance and spend | `internal/proposal.TestToolTextsReadTheirBounds`, `internal/proposal.TestTheListSaysEachAllowance` | not built |
| A firing's message carries `spend`, a wake's the spend of the trigger that started its session, and the trigger answers `status.spend` and `skippedBudget` | `internal/server.TestAFiringsMessageCarriesItsSpend` | not built |
| The fold renders the pace line before a message with `spend`, says ahead of pace when the projection passes the allowance with a wake's or a trigger's advice, and renders none without `spend` | `session.TestThePaceLineSaysWhenAWatchIsAheadOfPace` | not built |
| Every step's and compaction's `model.request` records `context`: the window, `used` from the provider or the estimate with its source, the parts in order summing to `used`, `files` and `summary` counted, and `free` | `harness.TestEveryRequestReportsItsContext` | not built |
| The Session folds `context_report` from the latest request of its own thread, and a subagent's and a title's leave it | `session.TestTheSessionFoldsItsContextReport` | not built |
| The OpenAPI document describes every field above | `internal/server.TestOpenAPIIsGenerated`, `internal/server.TestTheDocumentDescribesSpec087` | not built |
