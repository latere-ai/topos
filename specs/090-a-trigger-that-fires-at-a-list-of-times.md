---
title: "A trigger that fires at a list of times: spec.times lists 1 to MaxTimes instants, each fired once as a one-shot's at is, late past maxAge or once the next listed time comes, a refusal that resets waiting no later than the next time, the latest time's firing spending the trigger, times already past kept and never fired, the runs read answering fires_on times, and the trigger tool keeping the list"
status: testing
track: core
depends_on: [.archive/022-triggers.md, 086-proactive-agents.md, 087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report.md, 088-a-triggers-runs-and-fire-times-read-for-a-forecast.md]
affects: [manifest/v1/, manifest/trigger/, manifest/, internal/triggers/, internal/server/, internal/proposal/, api/openapi.yaml, docs/]
effort: medium
created: 2026-10-25
updated: 2026-10-25
author: changkun
---

# A trigger that fires at a list of times

## Overview

A person who asks for standing work "on Sunday at 17:40, Wednesday at
9 and the Monday after at noon" names times no schedule expresses. A
trigger fires on a schedule, on delivered events, or once at `at`
([[022-triggers]], [[086-proactive-agents]]), so a client today applies
one one-shot per time, and shows the person several items for what they
asked as one.

This spec adds `spec.times`: a list of times a trigger fires at, each
once, as a one-shot fires at its `at`. Every rule a one-shot keeps holds
for each time, and one rule more bounds each time by the next: a time
not fired by the time the next one comes gives way to it. The latest
time's firing spends the trigger as a one-shot's does. Wakes keep `at`.

## Current state

- `TriggerSpec` sets exactly one of `schedule`, `on` and `at`
  (`manifest/validate.go`). A one-shot fires at `at` once, the firing
  keyed by its time; a firing that spends it deletes the trigger
  (`internal/triggers/engine.go`, `schedule`, `recordFiring`).
- A one-shot skipped at its allowance is tried each pass, and one a deny
  refused for a limit that resets is tried once more at the reset, both
  bounded by `maxAge` ([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]).
- The runs read answers `fires_on` `schedule`, `event` or `at`
  ([[088-a-triggers-runs-and-fire-times-read-for-a-forecast]]).
- The trigger tool's change, pause and resume write a trigger in a
  routine's name and labels through the SDK's routine shape, whose
  fields hold a schedule or events and nothing else
  (`internal/proposal/shape.go`). A one-shot in a routine's name and
  labels is refused there, since the shape's check wants a schedule.

## Design

### Recommendations

| Item | Recommended | Weighed against, and why not |
|---|---|---|
| The field | `spec.times`, a list of RFC 3339 times with an offset, beside `schedule`, `on` and `at`, one of the four set | `at` taking a list: every client and authorizer reads `at` as one time, and a wake's `at` would change type |
| Its bound | `trigger.MaxTimes`, 100 | none: the minute loop and the read walk the list at each pass; a person names a handful of times, and a hundred is three months of twice-weekly runs |
| The list as stored | as written, in its order and with its offsets; the server reads a sorted UTC copy | sorted at apply: a list a client reads back and sends again in its own order would move the digest |
| Times already past | kept and never fired; refused only when no time is after now | refused: a client that resends the list it read, to pause or change it, would be refused for the times that fired |
| Which time a firing is | the latest listed time at or before the next fire time, so a time that waits keeps its own time while it is tried again | the next fire time itself: a time tried again at a reset would be a firing of no listed time, and its dedupe, retake and lateness would break |
| When a time is late | past `maxAge`, as a one-shot is, or once the next listed time has come | `maxAge` alone: two times close together would both run once a long wait ends, the earlier after the later was due |
| A refusal that resets past the next time | waits until the next time, which fires in its place; the earlier time's firing stays refused | waiting past it: the later time would wait behind a time the person no longer needs |
| What spends the trigger | the firing of the latest listed time, on the outcomes that spend a one-shot | each time's: the trigger would be deleted at its first firing |
| A manual fire | runs once now and spends no listed time | spending the next time: a manual fire is the person's own run, not one of the times they listed |
| The trigger tool | a trigger with `at` or `times` is changed as a document, whatever its name and labels; a change to a schedule or to events replaces `at` and `times` | the SDK's routine shape: it holds no time, so the change of a one-shot or a list fails its check |
| The tool's list | unchanged: a list's trigger reads with its next fire time | its times: a new version of the list's text, for a field the next fire time already answers |

### The field

The row below joins [[022-triggers]]'s table of the Trigger kind's
fields.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `times` | list of RFC 3339 times with an offset | one of `schedule`, `on`, `at` and `times` is required | fire at each time once; the latest time's firing spends the trigger (below). `timeZone` beside it is the zone a client shows the times in |

| Constant | Value | Why |
|---|---|---|
| `trigger.MaxTimes` | 100 | the bound above |

Apply refuses, as `invalid_manifest` with the field's path: `times`
beside `schedule`, `on` or `at` (`spec`); an empty list or one of more
than `MaxTimes` (`spec.times`); an instant listed twice, whatever its
offsets (`spec.times[i]`, naming the earlier index); and a list with no
time after the resolver's clock (`spec.times`). A trigger that lists no
times writes none, so its digest stays ([[003-manifest]]).

`trigger.create` and `trigger.update` carry `times`, each in RFC 3339
in UTC, in the list's order, beside `at` for a one-shot
([[086-proactive-agents]]).

### How each time fires

| | Rule |
|---|---|
| next fire time | the soonest listed time after the last fired, or after now at apply; while a time waits, when it is tried again |
| the firing | of the latest listed time at or before the next fire time, keyed `schedule:<time>` as a one-shot's, retaking a refused firing as a one-shot does |
| late | `skipped_late` past `maxAge`, or once the next listed time has come; the next time then fires in the same pass |
| at its allowance | `skipped_budget`, the next fire time kept at its time, tried each pass until it fires or is late |
| refused for a limit that resets | `refused` with its reason, tried once more at the reset or at the next listed time, whichever comes first; at the next listed time that time fires, and the earlier firing stays `refused` |
| held | as any held firing ([[022-triggers]]); the trigger moves on to its next time |
| spent | an earlier time's firing that spends a one-shot keeps the trigger, which moves on to its next time; the latest time's deletes it with its firings and tells the actor, as a one-shot's does |
| manual | a firing of now, never late, spending no listed time |

An apply that keeps the list, by its instants, keeps the next fire time,
so a time due or waiting before the apply still fires. A hold's release
keeps it as it keeps a one-shot's time ([[086-proactive-agents]]): each
time that passed while held fires in turn, late by the rules above.

The allowance's expected looks count the listed times still to come in
the window, as a schedule's fire times are counted
([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]).

### The runs read

The value below joins [[088-a-triggers-runs-and-fire-times-read-for-a-forecast]]'s
`fires_on`.

| `fires_on` | `fires` |
|---|---|
| `times` | the listed times after now and before `until`; while a time waits, first when it is tried again, or now when that has come |

Its runs are its own, as any trigger's, and its fire times walk the
same function the allowance's window walks.

### The trigger tool

A stored trigger with `at` or `times` is changed, paused and resumed as
a document of its fields, never through the SDK's routine shape, so its
times are kept. A change to a schedule or to events deletes `at` and
`times`, so the trigger fires on what the change names.

## What a hosted installation needs

| Need | Why |
|---|---|
| A client that offers a list of times writes `spec.times` in the manifest it applies, and reads `fires_on: times` in the runs read | the field and the value are new |
| Its forecast treats each listed time as a one-shot's, a wait bounded by the next listed time | the read answers the times; what they cost is the installation's |
| The image of this release pinned | the field and its firing are the server's |

An authorizer that reads nothing of `times` decides a list's triggers as
before; `trigger.create` and `trigger.update` only add the field.

## Not in this spec

A recurrence rule or an exception list; times added to or removed from
a list by its firings; a time per firing's own message; the trigger
tool proposing a list, and its list's text showing one; a wake at more
than one time.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `spec.times` resolves as written, order and offsets kept, a past time among them; an empty list, one past `MaxTimes`, one instant twice, no time after now, and `times` beside `schedule`, `on` or `at` are `invalid_manifest` at their paths | `manifest.TestTheTriggerFieldsOfSpec090` | built |
| A list's times read soonest first in UTC; the next time, the due time and the last time are read as above | `manifest/trigger.TestTheTimesOfAList` | built |
| Each listed time fires once, soonest first, a past one never; an earlier time keeps the trigger with the next as its next fire time, and the latest deletes it | `internal/triggers.TestEachListedTimeFiresOnce` | built |
| A time not fired before the next comes, and one past `maxAge`, is `skipped_late` | `internal/triggers.TestAListedTimeIsLateOnceTheNextHasCome` | built |
| A time refused for a limit that resets is tried once more at the reset under its first firing, waits no later than the next time, which fires in its place, and a refusal for good keeps the trigger for the next | `internal/triggers.TestAListedTimeRefusedUntilALimitResetsWaits` | built |
| A time at its allowance is tried each pass until the next time comes | `internal/triggers.TestAListedTimeAtItsAllowanceGivesWayToTheNext` | built |
| A held earlier time keeps the trigger and its sweep keeps it; the latest held keeps it until the sweep sends it, which spends it | `internal/triggers.TestAHeldListedTimeKeepsItsTrigger` | built |
| A manual fire spends no listed time | `internal/triggers.TestAManualFireSpendsNoListedTime` | built |
| The fire times and the expected looks of a list | `internal/triggers.TestTheFireTimesOfAList` | built |
| The apply route takes a list with the soonest time to come as its next fire time, `trigger.create` carries `times`, the same list sent again and a hold's release keep a due next fire time, a changed list starts from now, and bad lists are `invalid_manifest` | `internal/server.TestATriggerOfListedTimesIsAppliedAndAsked` | built |
| The runs read answers `fires_on: times`, its runs, and its times to come, a waiting time's next try first | `internal/server.TestTheRunsOfAListOfTimes` | built |
| The trigger tool's change, pause and resume keep `times` and `at`, of a trigger in a routine's name and labels and of any other, and a change to a schedule replaces them | `internal/proposal.TestAChangeKeepsTheTimesItsTriggerFiresAt` | built |
| The OpenAPI document describes the field, its firing and `fires_on: times` | `internal/server.TestOpenAPIIsGenerated`, `internal/server.TestTheDocumentDescribesSpec090` | built |

## Outcome

Built on 2026-10-25, in no release yet. Every criterion has its test;
the lateness rule, the wait's bound by the next time, the kept next fire
time at an apply and at a release, and the trigger tool's path each fail
their tests when undone. Building the tool's path found that a one-shot
in a routine's name and labels could not be changed, paused or resumed
through the tool, which the same change fixes. Nothing diverged from
the design.
