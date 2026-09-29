---
title: "Triggers: schedules and delivered events that start or continue sessions, filters, the message template, the session policy, and the limits"
status: drafted
track: core
depends_on: [003-manifest.md, 004-session-log.md, 006-identity.md, 014-store.md, 015-api.md]
affects: [internal/triggers/, internal/serve/, internal/server/, manifest/v1/, authorizer/, internal/store/]
effort: medium
created: 2026-09-27
updated: 2026-09-30
author: changkun
---

# Triggers

## Overview

A trigger starts or continues sessions of one agent when no person is
present. It fires on a schedule, or when an event is delivered to it.
Each firing is matched against the trigger's filter, renders the
trigger's message template against what fired it, and follows the
trigger's session policy: start a new session, or send the message to
the open session that the firing's key names.

The core keeps no listener of its own. An event reaches a trigger
through the fire route, called by whatever produces events for the
installation: the signals plane, a hosted platform's integrations, a
CI job, a relay in front of a webhook. The core verifies no provider's
signature, knows no provider's event names, and polls nothing. The
Trigger kind's original fields are [[003-manifest]]'s; this spec adds
the event fields, and it owns what all of them do.

## Current state

Nothing of it is built. `manifest/v1` has the Trigger kind with the
schedule fields (`agent`, `schedule`, `timeZone`, `session`,
`skipIfActive`, `maxAge`, `suspend`), and the authorizer vocabulary has
`trigger.create`, `read`, `list`, `update` and `delete`. The store's
lookup answers every trigger as not found; there is no
`internal/triggers`, no trigger route, and no table.

v0.7.0 had no triggers. The retired hosted service accepted schedules
with no credential behind them, so none could act. From its schedules
the fields `skip_if_active` and `max_age` are borrowed, as
`skipIfActive` and `maxAge`.

## Design

### Fields

The table is the whole Trigger kind. The rows marked "added" are this
spec's; the others are [[003-manifest]]'s, and what they do is defined
below.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `agent` | reference | required | the agent the trigger's sessions run, at its latest version at each firing |
| `schedule` | a five-field cron expression, or `@hourly`, `@daily`, `@weekly` | one of `schedule` and `on` is required, never both | fire on this schedule |
| `timeZone` | an IANA zone name | `UTC` | the zone `schedule` is read in |
| `on` (added) | object, below | one of `schedule` and `on` | fire on each delivered event that matches |
| `session.message` | template | required | the message each firing sends |
| `session.title` | template | none | the title of a session a firing starts |
| `session.policy` (added) | `new` or `continue` | `new` | below |
| `session.key` (added) | template | `""` on a schedule, `{{event.resource}}` on an event | the name a firing's session goes by |
| `session.machine`, `session.budget`, `session.limits` | as the session's fields | the agent's | the session a firing starts |
| `session.resources` | as the session's field; a repository's `url` and `ref` may be templates (added) | the agent's | the session a firing starts |
| `session.endOnIdle` | boolean | `true` under `new`, `false` under `continue` | [[004-session-log]]'s `end_on_idle` |
| `skipIfActive` | boolean | `true` | under `new`, skip a firing while a session of its key is active |
| `maxActive` (added) | integer, 1 to `triggers.MaxActiveCeiling` | `triggers.DefaultMaxActive` | the most sessions of this trigger active at once |
| `maxAge` | Go duration | `1h` | skip a firing more than this late |
| `suspend` | boolean | `false` | fire nothing |

`on` selects events by the envelope's fields (below). Every field given
must match; a list matches when any of its entries does.

| Field | Type | Matches |
|---|---|---|
| `on.product` | string, required | the envelope's `product`, exactly |
| `on.verbs` | list of strings, at least one | the envelope's `verb`: an entry is exact, or ends in `*` and matches the prefix before it |
| `on.resources` | list of strings | the envelope's `resource`, the same way; absent matches every resource |
| `on.match` | list of `{path, in}` | `path` is a path into `payload` (`payload.action`, `payload.pull_request.base.ref`); the value there, as the template renders it, matches one of `in` the same way; a path that names nothing, or names an object or a list, matches no entry |

There is no expression language: a filter is these fields and nothing
else, so a trigger's reader sees at once which events it takes. An
event outside every filter a person wants is one they filter out in
the producer, not in the core.

### The envelope

A delivered event is one JSON object, the envelope the signals plane
carries, so the plane can deliver to a trigger without translating.

| Field | Type | Meaning |
|---|---|---|
| `id` | string, 1 to 200 characters, required | the producer's id for this delivery; unique per `product`; the core deduplicates on it |
| `product` | string, required | where the event happened, lowercase: `origo`, `github`, `jira` |
| `verb` | string, required | what happened, dotted: `push`, `pull_request.opened`, `issue.created` |
| `resource` | string, required | what it happened to, as the producer names it: `changkun/topos-e2e#42`, `PROJ-7` |
| `actor` | string | who caused it at the source, for display |
| `subject` | string | the person or organization the event belongs to, for display |
| `time` | RFC 3339 time, required | when it happened at the source |
| `payload` | object | the event itself, as the producer passes it |

The body is at most the API's JSON limit ([[015-api]]). The core keeps
the envelope with the firing and never writes any of it into the
trigger.

### The template

`session.message`, `session.title`, `session.key` and a repository
resource's `url` and `ref` are templates. A placeholder is `{{`, a
path, and `}}`, with optional spaces inside the braces; `\{{` is a
literal `{{`. A path is a root and dotted segments of letters, digits,
`_` and `-`; a segment of digits indexes a list.

| Root | Paths |
|---|---|
| `event` | `event.id`, `event.product`, `event.verb`, `event.resource`, `event.actor`, `event.subject`, `event.time`, `event.payload.…` |
| `trigger` | `trigger.id`, `trigger.name` |
| `firing` | `firing.id`, `firing.time`: the scheduled time of a scheduled firing, the arrival time of an event or of a manual firing |

A string renders as itself, a number or a boolean as its JSON, an
object or a list as compact JSON, and a path that names nothing or
`null` as nothing. Each placeholder's value is cut to
`triggers.MaxValueBytes` at a character boundary, with `[cut]` after
it, so one long issue body cannot crowd out the rest. A rendered
message longer than `triggers.MaxMessageBytes` refuses the firing
(`refused`, reason `message_too_large`).

Apply refuses a template with an unknown root, a `{{` that opens no
placeholder, or an `event.` path in a schedule trigger, each as
`invalid_manifest` with the field's path. A template has no logic and
calls nothing: it is path lookup, so applying a trigger cannot run
code in the server. The rendered message goes through [[018-credentials-and-secrets]]'s
input check like any message.

### Firing

One pipeline serves both kinds of firing:

1. **Match.** An event outside `on` is `filtered`: it is counted, no
   firing row is written, and a redelivery is filtered again.
2. **Claim.** The firing is claimed as a row in the store keyed by
   the trigger and a dedupe string: the scheduled time for a schedule,
   `product` and `id` for an event ([[014-store]]). A firing already
   claimed answers its recorded outcome and starts nothing, so with
   several replicas, and with a producer that retries, one firing acts
   once. A firing whose outcome is `failed` is the exception: a
   redelivery of it runs again.
3. **Skip.** A suspended trigger fires nothing and records nothing. A
   firing more than `maxAge` late, after the event's `time` or after
   its scheduled time, is `skipped_late`, never run late.
4. **Render** the templates. The key is the rendered `session.key`.
5. **Act** by the session policy, below.
6. **Record** the outcome, the key and the session on the firing row,
   and the counts on the trigger's status.

The minute loop in `toposd serve` finds the scheduled firings due in
each trigger's `timeZone` and runs them through the pipeline:

| Rule | Behavior |
|---|---|
| `schedule` | a five-field cron expression (minute, hour, day of month, month, day of week) or `@hourly`, `@daily`, `@weekly` |
| daylight saving | a local time that does not exist fires at the next valid minute; a local time that occurs twice fires once |

The fire route (below) runs a delivered event through the same
pipeline in the request, and answers with the firing.

### The session policy

Every firing has a key. Under `new` it scopes `skipIfActive`; under
`continue` it names the one open session its firings share. The store
maps a trigger's key to its open session ([[014-store]]); the mapping
is claimed in the same transaction as the firing, so two firings of
one key on two replicas never start two sessions.

| Policy | The key names no open session | The key names an open session |
|---|---|---|
| `new` | start a session: `started` | with `skipIfActive`, while that session is active: `skipped_active`; otherwise start one: `started` |
| `continue` | start a session and map the key to it: `started` | send the message to it: `continued`, or hold it (below) |

A session is open until it ends, and active while it is neither ended
nor idle with nothing pending ([[004-session-log]]). A session a
firing starts is created as [[015-api]]'s create route creates one,
with the rendered message as its first `user.message`, `end_on_idle`
from `session.endOnIdle`, and `trigger_id` set. A continued message is
a `user.message` sent as the send route sends one: while a turn runs,
it waits for the turn's end.

A firing that would start a session while `maxActive` of the
trigger's sessions are active is `skipped_busy`. A continued message
is not bounded by `maxActive`, since it starts nothing.

A `continue` firing whose session is idle waiting for a person, on
`tool_confirmation` or on `budget`, is `held`: a message then would
answer the person's question for them, since a `user.message` denies
every pending call. The minute loop sends held firings in the order
they arrived once the session leaves that state, and records each as
`continued`. When the session ends first, its held firings run through
the policy again as firings of a key with no open session, so the
first starts a session and the others continue it.

A session ends as any session does: by its limits' `max_age`, by an
end the owner sends, or at idle under `end_on_idle`. The next firing
of its key starts a new session. The new session does not read the
old one's log; what should carry over belongs in a memory store the
agent attaches ([[020-memory-stores]]).

A schedule under `continue` keeps one session for the trigger, since
its key is `""` unless the trigger names one: each firing is the next
message of that session.

### Whose authority

A firing's message is sent with the sender `trigger:<trg_id>`, and its
`user.message` names the firing as `firing_id`. The initiator of a
session a firing starts is the trigger's owner, the person who last
applied it ([[004-session-log]]). The initiator is who the cap is
checked against: `trigger:<trg_id>` holds no rights of its own, so a
trigger can never start a session its owner could not. The create is
asked of the authorizer as `session.create` like any other
([[006-identity]]), and the session's memory partition is the owner's
([[020-memory-stores]]). A continued message is asked as
`session.send` with the owner as the subject.

A trigger's sessions act with the agent's own credentials
([[018-credentials-and-secrets]]): an organization's agent with its
agent identity, a personal agent as the person who applied the
trigger, narrowed to the agent. A firing the authorizer refuses starts
or sends nothing and is `refused`, with the refusal's reason on the
firing row.

### The routes

| Route | Action | Behavior |
|---|---|---|
| `POST /v1/triggers/{ref}/fire` | `trigger.fire` | an event trigger takes one envelope; a schedule trigger takes an empty body and fires now, outside its schedule, deduplicated only by an `Idempotency-Key` ([[015-api]]). Answers `200` with the firing, for a new firing and for a redelivery alike; `503` with the firing when its outcome is `failed`, so the producer retries |
| `GET /v1/triggers/{ref}/firings` | `trigger.read` | the trigger's firings, newest first, paged as every list is |

A firing answers `id` (`frg_…`), `trigger_id`, `origin` (`schedule`,
`manual` or `event`), `event` (`product`, `verb`, `resource`, `id` of
the envelope, for an event), `key`, `outcome`, `reason` for `refused`
and `failed`, `session_id` when it started or continued one, and
`received_at`.

`trigger.fire` is a new action of the vocabulary: the owner policy
lets a trigger's owner fire it, and an authorizer grants it to the
producers of an installation. The core trusts a caller that holds it
to deliver only events the trigger's owner may see: deciding that,
and verifying a provider's signature, is the producer's, since the
core knows no provider.

### Outcomes and status

| Outcome | Meaning | Row |
|---|---|---|
| `started` | a new session | yes |
| `continued` | a message to the key's open session | yes |
| `held` | waiting for the key's session to stop waiting for a person | yes, until sent |
| `filtered` | the event is outside `on` | no, counted only |
| `skipped_active` | under `new` with `skipIfActive`, the key's session is active | yes |
| `skipped_busy` | `maxActive` sessions are active | yes |
| `skipped_late` | later than `maxAge` | yes |
| `refused` | the authorizer refused, or the message is too large | yes |
| `failed` | a transient error, such as an unavailable authorizer; a redelivery runs again | yes |

A trigger's `status` carries `lastFiredAt`, `lastSessionId`,
`nextFireAt` for a schedule, and `counts`, one count per outcome, each
named in camelCase (`skippedActive`).

### Security

An event's text is written by whoever caused the event, such as the
author of an issue on a public repository. It reaches the model as the
trigger's message and grants nothing: the agent's permissions, its
approval mode and the session's budget bound what the session does,
whatever the text says ([[012-permissions-and-approvals]]). A trigger
under `confirm` has its sessions wait for the owner at the first call
that asks, which is the choice for an agent that acts on text from
outside. A trigger's spend is bounded by `maxActive` times the
session budget, and past that by the wallet of the installation's
authorizer. A redelivered or replayed event acts once, and a stale
one not at all.

### Limits

| Constant | Value |
|---|---|
| `triggers.DefaultMaxActive` | 5 |
| `triggers.MaxActiveCeiling` | 100 |
| `triggers.MaxValueBytes` | 16 KiB |
| `triggers.MaxMessageBytes` | 64 KiB |

Every check, schema and message that names one of these reads the
constant.

## Not in this spec

The producers: the integrations that receive a provider's webhooks
(Latere Code, GitHub, Jira), verify their signatures, decide which
triggers may see which events, and call the fire route, and the
signals plane that will carry them; a webhook route in the core that
verifies a provider's signature, since each provider signs
differently and a relay in front of the fire route does it for a
self-hosted installation; the console's trigger screens; the size of
a session's machine, which is the sandbox's to set.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A scheduled trigger starts one session per firing, with two `serve` replicas running | `TestOneSessionPerFiring` on Postgres with a fake clock | not built |
| Under `new` with `skipIfActive`, no session starts while one of the same key is active, and the skip is counted | `TestSkipIfActive` | not built |
| A scheduled firing or an event later than `maxAge` is skipped and counted | `TestMaxAgeSkipsLateFirings` | not built |
| A nonexistent local time fires at the next valid minute and a repeated one fires once | `TestDaylightSavingRules` | not built |
| A firing whose create the authorizer refuses starts nothing and is counted as refused, with the reason | `TestRefusedFiring` | not built |
| An event matches on product, verb, resource and payload paths, and one outside the filter is counted and writes no row | `TestFilterMatchesTheEnvelope` | not built |
| A template renders each root, cuts a long value, refuses a message over the cap, and apply refuses an unknown root, an unopened `{{` and an `event.` path in a schedule | `TestTemplate`, `TestApplyRefusesABadTemplate` | not built |
| Under `continue`, two events of one key make one session with two messages; after it ends, the next event starts a new session | `TestContinueSendsToTheKeysSession` | not built |
| Two events of one key delivered at once to two replicas start one session | `TestOneSessionPerKey` on Postgres | not built |
| A `continue` firing to a session waiting on a confirmation is held, denies nothing, and is sent once the confirmation is answered; one held when the session ends starts the next session | `TestContinueHoldsWhileAPersonIsAsked` | not built |
| A redelivered event answers its first firing and starts nothing; a `failed` one runs again | `TestRedeliveryActsOnce` | not built |
| A firing that would start a session past `maxActive` is `skipped_busy`, and a continued message is not | `TestMaxActive` | not built |
| The fire route is asked as `trigger.fire`; a caller without it is refused; a firing's session has the owner as initiator and `trigger:<trg_id>` as sender | `TestFireIsAskedOfTheAuthorizer` | not built |
| An empty `fire` on a schedule trigger fires it now, once per `Idempotency-Key` | `TestFireASchedule` | not built |
