---
title: "Triggers: schedules that start sessions, the session template, skipIfActive and maxAge"
status: drafted
track: core
depends_on: [003-manifest.md, 004-session-log.md, 006-identity.md, 014-store.md, 015-api.md]
affects: [internal/triggers/, internal/serve/]
effort: small
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Triggers

## Overview

A trigger is a schedule that starts sessions without a person. Each
firing creates one session from the trigger's template, with the
trigger's agent, and sends its first message. Triggers are schedules
only: an event that should start a session (a push, a webhook) calls
the session-create API from outside, and the core keeps no event
listener of its own. The Trigger kind's fields are [[003-manifest]]'s;
this spec owns what they do.

## Current state

v0.7.0 had no triggers. The retired hosted service accepted schedules
with no credential behind them, so none could act. From its schedules
the fields `skip_if_active` and `max_age` are borrowed, as
`skipIfActive` and `maxAge`.

## Design

### Firing

`internal/triggers` runs in `toposd serve`. Every minute it finds the
firings due in each trigger's `timeZone` and claims each firing as a
row in the store keyed by trigger and scheduled time ([[014-store]]),
so with several replicas one firing starts one session.

| Rule | Behavior |
|---|---|
| `schedule` | a five-field cron expression (minute, hour, day of month, month, day of week) or `@hourly`, `@daily`, `@weekly` |
| daylight saving | a local time that does not exist fires at the next valid minute; a local time that occurs twice fires once |
| `suspend` | a suspended trigger fires nothing and records no missed firing |
| `maxAge` | a firing more than `maxAge` late, for example after the server was down, is skipped and counted, never run late |
| `skipIfActive` | while a session this trigger started is neither ended nor idle with nothing pending, a firing is skipped and counted |

A firing creates a session as [[015-api]]'s create route does, with
the template's `message` as its first `user.message`, `end_on_idle`
from `session.endOnIdle` (default true), `trigger_id` set, and the
sender and initiator `trigger:<trg_id>` ([[004-session-log]]). The
create is asked of the authorizer as `session.create` like any other
([[006-identity]]).

### Whose authority

A trigger's sessions act with the agent's own credentials
([[018-credentials-and-secrets]]): an organization's agent with its
agent identity, a personal agent as the person who applied the
trigger, narrowed to the agent. A trigger whose applier could no longer
start the session is refused at the firing, and the trigger records the
refusal.

### Status

A trigger's `status` carries `lastFiredAt`, `lastSessionId`,
`nextFireAt`, and counts of `skippedActive`, `skippedLate` and
`refused`.

## Not in this spec

The fields ([[003-manifest]]); the routes that apply and list triggers
([[015-api]]); starting a session from an external event, which a
caller does through the create route.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A scheduled trigger starts one session per firing, with two `serve` replicas running | `TestOneSessionPerFiring` on Postgres with a fake clock | not built |
| With `skipIfActive`, no session starts while one of the trigger's sessions is active, and the skip is counted | `TestSkipIfActive` | not built |
| A firing later than `maxAge` is skipped and counted | `TestMaxAgeSkipsLateFirings` | not built |
| A nonexistent local time fires at the next valid minute and a repeated one fires once | `TestDaylightSavingRules` | not built |
| A firing whose create the authorizer refuses starts nothing and is counted as refused | `TestRefusedFiring` | not built |
