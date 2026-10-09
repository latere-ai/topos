---
title: "A message's opening for an authorizer that routes by it: an allow may ask for each later message's opening, which the session keeps beside its route under the operator's ceiling; and a message may say its client shows an authorizer's ask, and name the ask it answers"
status: testing
track: core
depends_on: [006-identity.md, 015-api.md, 038-routed-models.md, 056-editing-a-message-and-the-branches-of-a-session.md, 061-a-messages-shape-and-the-route-an-allow-names.md]
affects: [authorizer/, session/, internal/config/, internal/server/, api/openapi.yaml, docs/configuration.md, docs/]
effort: medium
created: 2026-10-08
updated: 2026-10-09
author: changkun
---

# A message's opening for an authorizer that routes by it

## Overview

[[061-a-messages-shape-and-the-route-an-allow-names]] gave a message's
question its shape and never its words, so an authorizer could route a
session that stands for a choice of choices by how long a message is and
what it attaches. Shape does not tell a short message that asks for a
day's work from a greeting, nor a long paste with a one-line question
from a long specification. An authorizer that routes by what a message
asks needs to read it, and, before it runs a message on the costliest
choice, may want to ask the person.

This spec adds three things, each off unless asked for:

1. An allow may ask for the opening of each later message on the
   session; the session keeps the request beside its route, and the core
   sends the opening only while the operator allows it.
2. A message may say its client puts an authorizer's ask to the person
   (`askable`).
3. A message sent again after such an ask may name it (`answers`).

The core reads nothing into any of them.

## Current state

- A question about a person's message carries `message_chars`,
  `attachments`, `links` and `tools_last_turn` (spec 061), from
  `shapeOf` in `internal/server/sessions.go`, at the send route
  (`events.go`), a trigger's firing into an open session (`triggers.go`),
  a fork sent its message (`fork.go`), and a create with a first message
  (`sessions.go`).
- An allow that names `model` may name `route`, which the session keeps
  on its model beside `via` (`session.ModelRef`), cleared by an allow
  that names a model and no route, kept through a failover and copied to
  a fork.
- A refused question reaches the client as `forbidden` with the
  authorizer's reason and limits in `details` (`errors.go`, `classify`).
- toposd's authorizer client keeps a deny for 5 seconds under a hash of
  the whole question (`latere.ai/x/pkg/authz`), so the same question
  asked again in that time is refused without reaching the authorizer.

## Design

### 1. The operator's ceiling

`TOPOS_AUTHORIZER_MESSAGE_TEXT`, a boolean, false by default. False, no
question carries a message's words, whatever an allow asks; true, a
session whose model asks for them (section 2) carries them. An
installation that never sets it sends no words.

### 2. An allow asks for the opening

An allow that names `model` may also name `message_text: true`:

```json
{"allow": true, "limits": {"model": "vendor/model-b", "route": "tier/thorough", "message_text": true}}
```

The session keeps it on its model, beside `route`, by the same rules
(spec 061, section 2): set or cleared wherever an allow names a model (a
create, a `PATCH` that names a model, a send), cleared by an allow that
names a model without it, kept through a failover, copied to a fork at
the fork point. A send whose allow changes only `message_text` appends
one `session.model_changed` made by the service, as a route alone does,
so the log says when a session's words began and stopped reaching the
authorizer.

```json
{"model": {"name": "vendor/model-b", "via": "tier/auto", "route": "tier/thorough", "message_text": true}}
```

A `message_text` that is not a boolean refuses the request as
`authorizer_unavailable`, as a `route` that does not decode does.

### 3. The opening

On a session whose model holds `message_text: true`, with the ceiling
on, a question about a person's message carries `message_text`: the text
of the message's text blocks, joined by a blank line, counted in Unicode
code points:

- the whole text when it has at most `MaxMessageText`, 2,000;
- otherwise its first `MessageTextHead`, 1,500, then a line holding `…`
  (`"\n…\n"`), then its last `MessageTextTail`, 500.

Its end is kept because a request often follows what it is about: a
pasted log, then "find why this fails". It is absent when the message
has no text. Images and files are not read; their count is
`attachments`, as before.

It is sent beside the shape, on `session.send` whose `event_type` is
`user.message`: the send route, a trigger's firing into an open session,
and a fork sent its message (the fork's model is its parent's at the
fork point, `message_text` included). A create's first message carries
none: the session has no allow yet.

### 4. A message's word to the authorizer

A `user.message` as a client sends it, at the send route and in a fork's
`message`, may carry two members, which the core checks for shape,
forwards on the question about the message under the same names, and
never stores:

| Member | Value | Forwarded when |
|---|---|---|
| `askable` | `true`: the client puts an authorizer's ask on this message to the person, so the authorizer may refuse the message to ask | true |
| `answers` | a string of at most 512 bytes with no space around it: the ask, as a refusal of an earlier copy of this message named it in its limits, that this message is sent in answer to | present |

The event the log keeps holds neither. A trigger's firing and a create's
first message carry neither. A value of another type, or an `answers`
that is empty, longer, or has space around it, is `invalid_request`.

How an authorizer asks is its own: Latere's refuses with a reason and
limits that name the ask (platform spec 183). The member makes the
question about the answer differ from the one refused, so the deny
toposd's authorizer client keeps for 5 seconds does not answer it.

### 5. What the core keeps

The opening is built for the question and dropped with it; the core logs
neither it nor the question's fields. toposd's authorizer client hashes
the whole question, the opening included, to key the deny it keeps for 5
seconds; it keeps no text. The message itself is in the session's log
as before. `askable` and `answers` are kept nowhere.

## Compatibility

An authorizer that names no `message_text` gets no words, and one that
reads no `askable` or `answers` decides as before. A core before this
spec ignores `message_text` in an allow, as it ignores any member it does
not know, and refuses a message that carries `askable` or `answers`
(`invalid_request`, a message's members are read strictly), so a client
sends them only to a core that documents them. An authorizer reads the
absence of `message_text` as the core before this spec, and falls back
to what it read before.

## Not in this spec

- What an authorizer decides from the words or the members, which
  choices exist, and what it keeps.
- Earlier messages, the agent's answers, or a file's content.

## Acceptance criteria

| Criterion | Test that proves it |
|---|---|
| An allow's `message_text` is kept beside the route at a create, a `PATCH` and a send, in the header and in `session.model_changed`; one that names a model without it clears it; a change of it alone appends one change made by the service; a failover keeps it and a fork carries it | `internal/server.TestAnAllowAsksForTheOpening` |
| With the ceiling on and the session asking, a message's question carries its opening; with either off, and for any other event, none | `internal/server.TestAMessageCarriesItsOpeningOnlyWhenAsked` |
| The opening is the whole text up to 2,000 code points, and past that the first 1,500 and the last 500 around a line holding `…` | `internal/server.TestTheOpeningIsBounded` |
| A trigger's firing into an open session and a fork sent its message carry it under the same rule; a create's first message carries none | `internal/server.TestEveryQuestionAboutAMessageCarriesItsOpening` |
| `askable` and `answers` are forwarded on the send and the fork's message, kept in no event, and refused when malformed | `internal/server.TestAMessageForwardsItsWordToTheAuthorizer` |
| A `message_text` that does not decode refuses the request | `authorizer.TestDecodeLimitsReadsMessageText` |
| The variable is read as a boolean, false by default | `internal/config.TestAuthorizerMessageTextIsTheCeiling` |
| The configuration reference and the API document name them | `internal/server.TestOpenAPIDocumentsTheOpening`, the docs check |

## Outcome

Built on 2026-10-09 on the branch `semantic-auto`, in no release yet.
Every criterion has its test: `internal/server/opening_test.go`,
`authorizer/limits_test.go` and `internal/config/config_test.go`, and the
configuration tables of `docs/configuration.md` and spec 002 name the
variable. What shipped:

- `TOPOS_AUTHORIZER_MESSAGE_TEXT` is read by `serve` and `check` as Go's
  `strconv.ParseBool` reads a boolean (`true`, `false`, `1`, `0` and
  their short and capitalized forms), not as the `on` and `off` of
  `TOPOS_HOST_SESSIONS`; any other value stops the start.
- `authorizer.WireLimits` and `Limits` gain `MessageText`, read only
  beside `Model`, and `session.ModelRef` gains `message_text`. It is set
  where `route` is, so `standing`, the failover's answer and a fork's
  copied model carry it with no code of their own.
- `askSend` takes the message's shape and adds `message_text` itself,
  where the model the session stands on is known; the send route, a
  trigger's firing and a fork's message all reach it, and a create never
  does. The opening is asked of the model the session stands on before
  the send's allow, so a send whose allow first sets `message_text`
  carries none, and the next one does.
- An empty text block holds no text, so it adds no blank line to the
  opening, and a message whose text blocks are all empty has none.
- `askable` and `answers` are members of the message body the send route
  and a fork's `message` read; JSON `null` for either is read as absent,
  as for the body's other optional members. `MaxAnswers` is 512 bytes.
