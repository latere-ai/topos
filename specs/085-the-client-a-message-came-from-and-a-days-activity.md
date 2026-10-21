---
title: "The client a message came from, and a day's activity: the server writes the OAuth client of the verified token on every sender it records, and one route answers, for one day, how many messages each person sent from each client, how many sessions they started from each, and how many answers to their messages finished, counts with no content"
status: testing
track: core
depends_on: [.archive/004-session-log.md, .archive/006-identity.md, .archive/014-store.md, 015-api.md, .archive/022-triggers.md, 056-editing-a-message-and-the-branches-of-a-session.md, 077-rollouts.md]
affects: [session/, authorizer/, internal/auth/, internal/server/, internal/store/postgres/, api/openapi.yaml, docs/configuration.md, docs/activity.md]
effort: medium
created: 2026-10-21
updated: 2026-10-21
author: changkun
---

# The client a message came from, and a day's activity

## Overview

An installation that serves several applications over one core wants to
know two things the log already holds the facts for: on which days a
person used the agents, and from which application. A person who talks
to the same agent from a web page and from a desktop application leaves
one log per conversation, and nothing in it says which application sent
which message. The token that sent it does: every token an OAuth issuer
mints for a client names the client.

This spec has the server write that client on every sender it records
from a verified token, and adds one read: for one day, per sender, how
many messages they sent from each client, how many sessions they started
from each, and how many answers to their messages finished. It answers
counts only, never a word of a message or an answer, and only to a
caller the authorizer allows.

## Current state

- The server builds every sender of a person's event from the verified
  caller (`Sender{Subject, Kind: person}`): the session's initiator, a
  `user.message`, an interrupt, a confirmation, a tool result, an
  answer, a fork's message, a rename. A request body never names the
  sender. `internal/auth.Caller.Claims` holds the token's claims
  verbatim and nothing reads a client from them.
- A trigger's message has a sender of kind `trigger` and a `firing_id`
  (spec 022). A message of a session's own subagent threads has a
  `thread`.
- A turn ends with a `session.status` on the session's thread: `idle`
  with a stop reason, or `ended` with `completed` for an agent that ends
  on idle.
- The Postgres store keeps events with `type`, `time`, `thread` and the
  payload as text, keyed by session and sequence, with no index on time.

## Design

### The client on a sender

`Sender` gains `client`, omitted when empty: the OAuth client of the
token the server verified for the request that wrote the event. It is
the token's `client_id` claim (RFC 9068 section 2.2), or its `azp` claim
when it has no `client_id`, and empty for a token that names neither.
`auth.ClientOf(Caller)` reads it, and every sender the server builds for
a caller carries it. Nothing else writes it:

- a request body cannot name it, as it cannot name the sender;
- a trigger's sender, the authorizer's and the harness's carry none;
- a fork copies its parent's events as they were, so a copied message
  keeps the client that sent it, and the fork's own message carries the
  client of the fork's request.

A store keeps it as part of the payload, so an older event reads with an
empty client. The core reads no meaning from it: it is a fact about the
request, like the sender's subject.

### A day's activity

`GET /sessions/activity?day=YYYY-MM-DD` answers one day, from 00:00 to
24:00 UTC, as a page of senders like every list:

```json
{
  "items": [
    {
      "sender": "https://issuer.example|5f0c...",
      "messages": [{"client": "", "count": 1}, {"client": "web", "count": 3}],
      "sessions": [{"client": "web", "count": 1}],
      "answers": 4
    }
  ],
  "next_cursor": ""
}
```

- **messages** counts the `user.message` events of the day on a
  session's own thread (`thread` empty) whose sender is of kind
  `person`, that carry no `firing_id` and are not redacted, by the
  sender's client. A message of a trigger, of a subagent's thread, or
  one later redacted is not a message a person sent that day.
- **sessions** counts the sessions created that day whose initiator is
  of kind `person`, that no trigger started and that are no run of a
  rollout (spec 077), by the initiator's client. A fork is a session the
  person created (spec 056).
- **answers** counts the turns of the day that ended normally on a
  session's own thread: a `session.status` of `idle` with `end_turn`, or
  of `ended` with `completed`. Each is counted for the sender of the
  last `user.message` on that thread before it, when that message was a
  person's and no trigger's. A turn that stopped on an error, a limit,
  an interrupt, a question or an approval is not a finished answer.
- A sender with no message, no session and no answer that day is not in
  the page. Senders are ordered by subject; `limit` is 1 to `MaxLimit`,
  `DefaultListLimit` when absent, and `cursor` is the page's
  `next_cursor`.
- `sender=<subject>` narrows the answer to one sender.

A day is accepted from `FirstActivityDay`, 2000-01-01, up to the current UTC day; a day that
has not ended answers what the log holds so far. Anything else is
`invalid_request`.

### The question

The route asks a new action, `session.activity`, of kind `session`,
about the collection: the resource has no id and carries `day`, and
`sender` when the request names one. It acts on no single session, so
it is asked once per request.

- With an authorizer, the authorizer decides. An installation allows it
  to its own reporting service and to nobody else.
- The owner policy allows it to the admins of `TOPOS_ADMIN_SUBJECTS`,
  and to any caller who names themselves as `sender`, since their own
  counts are theirs; anything else is `not_owner`.

### The stores

`session.Activity` is an optional interface of a store that counts a day
without reading every log. The Postgres store answers it with three
grouped queries over the day's rows, a partial index on `events (time)`
for the `user.message` and `session.status` rows of a session's own
thread (migration 0014) and an index on `sessions (created_at)`
(migration 0015), so the read touches the day's rows alone. Both are
built concurrently, each the one statement of its file, so a running
installation's appends are not held while they build.
The other stores are counted by `session.CountActivity`, which reads
every session's log, and is the reference the store suite holds the
Postgres answer to.

## Not in this spec

- What an installation does with the counts. Topos keeps no record of a
  day apart from the log, and a log deleted with its session takes its
  counts with it.
- A client named by anything but the verified token, such as a header or
  the session's metadata.

## Acceptance criteria

| Criterion | Test that proves it |
|---|---|
| `auth.ClientOf` answers `client_id`, then `azp`, then empty | `auth.TestClientOf` |
| Every sender the API builds for a caller carries the caller's client: a session's initiator, a message, an interrupt, a rename, a fork's initiator and its message; what a body says of its sender is not what is recorded | `server.TestSendersCarryTheCallersClient` |
| A message sent with a token that names no client has no `client` in its payload; an event written before this spec reads with an empty client | `server.TestSendersCarryTheCallersClient`, `session.TestSenderClientIsOptional` |
| The rules: a person's message on the own thread with no firing counts and a trigger's, a firing's, a subagent's and a redacted one do not; a turn's normal end finishes an answer and an error, a limit, a question, an end through the API and a subagent's end do not; a session a person started counts and a trigger's and a rollout's do not | `session.TestTheActivityRules` |
| A day's messages, sessions and answers are counted per sender and client by those rules on every store, an answer for the person whose message the turn answered, nothing outside the day, and a sender, an after and a limit narrow the answer | `storetest` `Activity` on the memory, directory and Postgres stores; `server.TestActivityCountsADay` |
| The Postgres store answers what `session.CountActivity` reads from every log, and each of its three counts reads by an index of migrations 0014 and 0015 | `storetest` `Activity` on Postgres, `postgres.TestActivityUsesTheTimeIndex` |
| A page holds `limit` senders and names the next one's cursor; the last names none | `server.TestActivityPages` |
| The route asks `session.activity` once, about the collection, with `day` and `sender`; the owner policy allows an admin and a caller naming themselves, and refuses anyone else; a deny answers `forbidden` | `server.TestActivityAsksOnce`, `auth.TestOwnerPolicyActivity` |
| A day that is absent, not a date, before the first day or after today, a limit out of range and a cursor not issued here answer `invalid_request` and ask nothing | `server.TestActivityRefusesABadDay` |
| The OpenAPI document describes the route, its parameters and its answer, and the example is what the route answers | `server.TestOpenAPIIsGenerated`, `server.TestOpenAPIMatchesHandlers`, `server.TestTheExamplesAreWhatTheRoutesAnswer` |

## Outcome

Built on 2026-10-21 as designed, every criterion with its test, and not
released. Two changes against the first draft:

- The answer is the page every list answers, `{"items", "next_cursor"}`,
  rather than an object that repeats the day.
- The messages and answers of a rollout's runs are left out beside its
  sessions, since a rollout's turns run unattended.

The client is what the verified token names. A token minted for a
person by an issuer that leaves the client off records an empty client;
the core counts such a message all the same.
