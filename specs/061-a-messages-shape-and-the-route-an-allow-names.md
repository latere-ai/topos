---
title: "A message's shape and the route an allow names: a send tells the authorizer how long the message is, what it attaches and links, and whether the last turn ran a tool, and an allow may name the routed name it chose on the way to the model, which the session keeps beside it"
status: testing
track: core
depends_on: [004-session-log.md, 006-identity.md, 015-api.md, 038-routed-models.md, 051-a-turn-moves-off-a-model-that-cannot-serve.md, 056-editing-a-message-and-the-branches-of-a-session.md]
affects: [authorizer/, session/, internal/server/, api/openapi.yaml, docs/]
effort: small
created: 2026-10-08
updated: 2026-10-08
author: changkun
---

# A message's shape and the route an allow names

## Overview

An installation may offer a routed name that stands for a choice of
choices: "pick the kind of work for each message", which resolves first
to another routed name, say `tier/thorough`, and then to a model of it
([[038-routed-models]]). Two things are missing for that.

1. **The authorizer cannot see what a message asks.** A send's question
   names the sender, the event type, the model the session stands on
   and how long it has been quiet. Nothing says whether the message is a
   one-line question or a long task with a file, so the authorizer
   cannot choose by it.
2. **Nothing keeps the name chosen in between.** The session keeps the
   name asked (`via`) and the model that runs (`name`). A client that
   says "this answer ran as Thorough" has nothing to read it from, and
   the authorizer, at the next send, cannot tell which choice the
   session stands on.

This spec gives the send question the message's shape, never its words,
and lets an allow name the routed name it chose, which the session keeps
as `route` beside `via` and asks with every later question.

## Current state

- `session.send` carries `sender`, `event_type`, `model`, `model_via`
  and `idle_seconds` (`internal/server/events.go`, `sessions.go`
  `askSend`). A fork that is sent a message asks the same
  (`fork.go`), and a trigger's firing too (`triggers.go`).
- A create that carries its first message, a trigger's, appends it
  without a send question (`sessions.go` `create`).
- An allow's `limits.model` moves the session, and `via` keeps the name
  asked (`session.ModelRef`). A failover keeps `via`
  ([[051-a-turn-moves-off-a-model-that-cannot-serve]]).

## Design

### 1. The message's shape

A question about a person's message carries four fields beside the
ones it carries today. They describe the message and the session; the
message's words are never sent.

| Field | Value |
|---|---|
| `message_chars` | the characters of the message's text blocks together, counted as Unicode code points |
| `attachments` | the files the message attaches, and the image blocks of its content |
| `links` | the web addresses in its text: each `http://` or `https://`, in any case, followed by at least one character that is not white space |
| `tools_last_turn` | whether the session's last turn made a tool call: an `agent.tool_use` of the header's last turn, on any thread. `false` for a session with no turn yet |

They are sent where a message is asked about:

| Question | When |
|---|---|
| `session.send` | its `event_type` is `user.message`: the send route, a trigger's firing into an open session, and a fork that is sent its message ([[056-editing-a-message-and-the-branches-of-a-session]]) |
| `session.create` | the create carries its first message, a trigger's firing that starts a session. `tools_last_turn` is `false` |

No other event carries them: a tool confirmation, a tool result and an
answer continue a turn.

`tools_last_turn` is read from the log's tail, as `idle_seconds` is:
backward from the last event, over the events of the last turn, until
an `agent.tool_use` of that turn or an event of an earlier one. A turn
that called no tool is a few events long. A fork that is sent its
message reads the events it copied, whose last turn is the fork
point's, as it reads `idle_seconds` from them.

### 2. The route an allow names

An allow that names `model` may also name `route`: the routed name the
authorizer resolved the name asked to before it picked the model.

```json
{"allow": true, "limits": {"model": "vendor/model-b", "route": "tier/thorough"}}
```

The core never reads what a route means. It keeps it:

- **On the session's model**, beside `via`, in the header and in both
  sides of `session.model_changed`:

  ```json
  {"model": {"name": "vendor/model-b", "via": "tier/auto", "route": "tier/thorough"}}
  ```

- **Wherever the model is set by an allow**: a create, a `PATCH` that
  names a model, and a send. An allow that names a model and no route
  clears the route; an allow that names no model changes nothing, a
  route beside it included. `route` is read only beside `model`.
- **Alone, at a send.** An allow whose model is the one the session
  stands on and whose route differs appends one `session.model_changed`
  made by the service, so the log says the route moved though the model
  did not.
- **Through a failover.** A turn moved off a model that cannot serve
  stays on its route: the model changes inside the turn, the route the
  turn began on does not.
- **Through a fork.** A fork starts on its parent's model at the fork
  point with its `via` and its `route`.

A `route` that is not a string, or has space around it, refuses the
request as `authorizer_unavailable`, as `model` does.

### 3. The route in every question

Each question that carries the session's `model_via` carries its
`route` beside it, so the authorizer decides from the core's word what
the session stands on, as it does for the model:

| Question | Field |
|---|---|
| `session.send` | `model_route` |
| `session.fork` | `model_route`, the parent's at the fork point |
| `session.update` | `current_model_route`, wherever `current_model_via` is sent: a `PATCH` that names a model or a reasoning level, and a failover |

Each is absent when the session has no route.

## Compatibility

Both sides tolerate the other's absence. An authorizer that reads none
of the new fields decides as before, and one that names no `route`
leaves every session without one. A core before this spec ignores
`route` in an allow, as it ignores any member it does not know, and
sends none of the fields; an authorizer that routes by them reads their
absence as a short message with nothing attached and no tool before it.

## Not in this spec

- What a route means, which choices exist, and how an installation
  chooses among them.
- A message's words, or anything read from a file's content.

## Acceptance criteria

| Criterion | Test that proves it |
|---|---|
| A send of a message carries its characters, attachments and links, and whether the last turn called a tool; a send of any other event carries none of them | `internal/server.TestASendTellsTheShapeOfItsMessage` |
| A fork sent a message, a trigger's firing into an open session, and a create with a first message carry the shape | `internal/server.TestEveryQuestionAboutAMessageCarriesItsShape` |
| `tools_last_turn` is true when the last turn called a tool on any thread, and false after a turn that only talked and before the first turn | `internal/server.TestToolsLastTurnReadsTheLastTurn` |
| An allow's route is kept beside the model at a create, a `PATCH` and a send, in the header and in `session.model_changed`; one that names a model and no route clears it | `internal/server.TestARouteIsKeptBesideTheModel` |
| A route that moves while the model stays appends one change made by the service | `internal/server.TestARouteAloneMovesAtASend` |
| A failover keeps the route; a fork carries it | `internal/server.TestAFailoverKeepsTheRoute`, `TestAForkCarriesTheRoute` |
| The send and fork questions carry `model_route`, and the update question `current_model_route`, while the session has one | `internal/server.TestQuestionsCarryTheRoute` |
| A route that does not decode refuses the request | `authorizer.TestDecodeLimitsReadsARoute` |
| The API document names the fields and the member | `internal/server.TestOpenAPIDocumentsTheRoute` |

## Outcome

Built on 2026-10-08 on the branch `auto-level`, in no release yet. Every
criterion has its test in `internal/server/shape_test.go` and
`authorizer/limits_test.go`. What shipped:

- `session.ModelRef` gains `route`; `authorizer.WireLimits` and `Limits`
  gain `Route`, read only beside `Model`.
- One backward read of the log's end (`tailOf`) finds both the last model
  request and whether the last turn called a tool; a fork reads the
  events it copied the same way (`tailOfCopy`).
- The shape is counted where the message is read: the send route, a
  trigger's firing into an open session, a fork's message, and a create's
  first message. An image block counts as an attachment.
- `TestASendTellsHowLongTheSessionHasBeenQuiet` holds the whole send
  question, so it now names the four fields as well.
