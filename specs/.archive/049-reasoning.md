---
title: "Reasoning: the model's level is named reasoning at the API and kept as effort in storage, and an authorizer's decision may set it"
status: complete
track: core
depends_on: [003-manifest.md, 004-session-log.md, 006-identity.md, 015-api.md, 038-routed-models.md]
affects: [authorizer/, manifest/, session/, harness/, internal/auth/, internal/server/, api/openapi.yaml]
effort: medium
created: 2026-10-05
updated: 2026-10-05
author: changkun
---

# Reasoning

## Overview

How much a model thinks before it answers is a level of four values,
`minimal`, `low`, `medium` and `high`. The API has named it `effort`:
on an agent's `spec.model`, on a session's `model`, in the body of
`PATCH /v1/sessions/{id}`, in both halves of `session.model_changed`,
and as a field of the `session.update` question. An installation that
offers its people a choice of how much work a request gets, and calls
that choice effort, then shows one object that says both: the level a
person chose, by its routed name ([[038-routed-models]]), beside an
`effort` that is the model's own thinking depth.

This spec renames the field `reasoning` at the API, keeps `effort` as
the spelling every store holds, reads `effort` on input through every
v0.x release, and lets an authorizer's decision set the level a session
runs at, where [[038-routed-models]] let it set the model.

## Current state

Built. The authorizer's limits carry `reasoning` (`authorizer/limits.go`).
`v1.AgentModel` and `session.ModelRef` carry the level under both names,
`Effort` the stored one and `Reasoning` the answered one
(`manifest/v1/agent.go`, `session/session.go`). The resolver stores the
level under `effort` whichever name a manifest gave
(`manifest/resolve.go`), and the API renames it at its edge
(`internal/server/answer.go`). The create, the change and the send read
the allow's level (`internal/server/sessions.go`, `update.go`), and a
deny's limits reach the client (`internal/auth/guard.go`,
`internal/server/errors.go`).

## Design

### 1. One name at the API, one in storage

Three stores hold the level under `effort`, and each must stay readable:

- **Agent versions.** `ReadBundle` decodes a stored version strictly,
  renders the decoded spec again and compares its hash with the digest
  the version was stored under ([[003-manifest]]). A spec rendered with
  `reasoning` hashes a stored `"effort": "high"` to other bytes, and
  every create, send, change and resume of that agent would fail.
- **Session headers.** A header is stored as an encoded document with
  `model.effort` in it, and a release before this one decodes it so.
- **Logs.** An event's payload is stored and returned as raw bytes
  ([[004-session-log]]), so a log answers what it was written with for
  as long as it lives.

So `effort` stays the stored spelling, and the API renames at its edge:

| | Stored | At the API |
|---|---|---|
| an agent's resolved spec | `spec.model.effort`, `spec.advisor.model.effort` and each inline subagent's, bytes and digest as before | a manifest names `reasoning`; an agent read, a version read and an apply answer `reasoning` |
| a session's header | `model.effort` | a session answers `model.reasoning`; a `PATCH` body names `model.reasoning` |
| `session.model_changed` | `old.effort`, `new.effort` | the events list, the send's answer and the stream answer `old.reasoning` and `new.reasoning`, for events stored before this release too |
| the runner protocol, `/internal/v1` | `effort` | not the API: a runner reads what the store holds |

- Every stored agent version keeps its digest, and an agent applied
  again unchanged, under either name, keeps its version.
- A release before this one reads everything this one stores, so a
  rollback loses no level.
- The API answers one name from this release on, old events included.
- A version read answers a spec that is not byte for byte the spec its
  digest covers. The API document says so on every route that answers
  an agent.
- An answer that an idempotency key replays was stored as it was
  answered, so a replay inside the window of an answer stored by an
  earlier release names `effort`.

### 2. The old name on input

A manifest's model (`spec.model`, `spec.advisor.model`, an inline
subagent's) and a `PATCH` body may name the level `effort`. A model or
a body that names both with one level is that level; both with two
levels is `invalid_manifest` at the manifest's `reasoning` path, or
`invalid_request` for a body. `effort` is read on input through every
v0.x release and dropped in v1.0.

### 3. A decision may name the level

An allow's limits may carry `reasoning`, read where `model` is: at
`session.create`, at `session.update` and at `session.send`. It has
three states, so it is a pointer on both sides:

| On the wire | Meaning |
|---|---|
| absent | the session keeps the level it has |
| a level | the session's next turn runs at it |
| `""` | the session returns to its agent's own, what an empty level means in a `PATCH` body |

```json
{"allow": true, "limits": {"model": "vendor/model-a", "reasoning": "medium"}}
```

A value that is neither empty nor one of the four refuses the request
as `authorizer_unavailable`, as any limit the core cannot read does.

| Decision | With `limits.reasoning` in the allow |
|---|---|
| a session's create | the session starts at that level; the header holds it beside the model and `via`, and no event is appended |
| `session.update` that names a model or a level | the change runs at that level in place of the one the body names |
| `session.send` | the session moves to that level before the turn starts, with `session.model_changed` by the service in one batch with the event sent |

`""` is resolved to the agent's own level before it is compared and
before it is written, as an empty level in a `PATCH` body is, so a
session already at its agent's own appends nothing however often an
allow answers `""`. A send compares the model and the level with what
the session stands on and appends one change when either differs; a
level alone keeps the model and its `via`. A fork is not one of the
three: it starts on the model and the level its parent stood on at the
fork point ([[038-routed-models]]). A change of the approval mode alone
reads no level.

### 4. The question names the level twice

A `session.update` whose body names a level carries the resolved level
under both `effort` and `reasoning`, through every v0.x release. A
question is no API answer, and an authorizer written before this spec
reads `effort` and refuses a change that names neither a model nor
`effort`. One that reads `reasoning`, and `effort` where it is absent,
decides the same.

### 5. A deny's limits reach the client

A deny may carry limits too, such as the moment a bound that refused a
send resets. The core passed on a deny's reason and dropped its limits,
so no client could say when a refused message can be sent again. A
forbidden refusal now carries the deny's limits object beside the
reason, as `details.limits`:

```json
{"error": {"code": "forbidden", "message": "...", "details": {"reason": "rate_limited",
  "limits": {"resets_at": "2026-10-06T00:00:00Z"}, "detail": "..."}}}
```

The limits go with the reason and only with it: a deny whose reason is
not a reason token, and a refusal whose reason is withheld because the
caller may not read the object it is about, carry none. Limits that are
no JSON object are not passed. The authorizer owns the members.

## Not in this spec

- Levels above `high`. The manifest admits four; a model's own scale may
  run higher, and widening it is a change to the manifest of its own.
- Which level an installation sets for a session, and when. That is the
  authorizer's.
- Dropping `effort` on input, which v1.0 does.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| An agent version stored by v0.16.0 with its levels under `effort` starts, sends, changes its level and resumes with its digest unchanged, its version read answers `reasoning`, and the same manifest applied again, under either name, keeps the version and writes nothing | `internal/server.TestAVersionStoredBeforeTheRenameRunsWithItsDigest`, over `internal/server/testdata/agent-stored-by-v0.16.0.json`; `manifest.TestTheReasoningLevelIsReadUnderEitherName` | built |
| A header stored with `effort` keeps its level, and a header this release stores names it under `effort` alone, which a reader of the earlier shape reads with its level | `session.TestAHeaderKeepsItsLevelAcrossTheRename` | built |
| Every answer names the level `reasoning` and none `effort`: an agent's apply, read, list and version read, a session's create, read, list, change, fork, end, archive, unarchive and resume, and `session.model_changed` in the events list and the stream, a change stored before the release included; what is stored keeps `effort` | `internal/server.TestEveryAnswerNamesTheLevelReasoning`, `session.TestAModelRefAnswersItsLevelAsReasoning`, `manifest.TestEveryModelOfASpecHoldsItsLevelUnderEffort` | built |
| A manifest and a `PATCH` body are read under either name; both names with different levels are refused | `internal/server.TestALevelIsReadUnderEitherName`, `manifest.TestTwoLevelsForOneModelAreRefused`, `manifest.TestALevelReadsUnderEitherName` | built |
| The `session.update` question carries the level under both names | `internal/server.TestTheUpdateQuestionNamesTheLevelUnderBothNames` | built |
| With an allow that names a level, the session's next turn runs at it; an authorizer's change of the level appends one `session.model_changed` by the service at the next message; a fork starts at its parent's level; a level outside the four is `authorizer_unavailable` | `internal/server.TestAnAuthorizersLevelMovesTheSession`, `cmd/toposd.TestASessionRunsAtTheLevelItsAuthorizerNames`, `authorizer.TestTheReasoningLevelHasThreeStatesOnTheWire` | built |
| A session whose agent names a level, answered `""` or its own level, appends no `session.model_changed` | `internal/server.TestAnAllowOfTheAgentsOwnLevelAppendsNothing` | built |
| A send refused at a bound that resets answers the moment it does as `details.limits.resets_at`; a withheld reason withholds the limits | `internal/server.TestARefusedSendSaysWhenItOpensAgain`, `internal/auth.TestADenysLimitsGoWithItsReason` | built |

## Outcome

Built on 2026-10-05 and in no release yet. Every criterion has its test.
What shipped differs from the draft in these points:

- **Two fields, one level.** `v1.AgentModel` and `session.ModelRef` each
  carry `Effort`, the stored name, and `Reasoning`, the answered one,
  with `Level`, `Stored` and `Answered` to read and move the level. A
  struct that rendered `reasoning` would change every stored digest, and
  a separate answer type per object would have to follow the recursion
  of inline subagents. Nothing stored sets `Reasoning`: the resolver
  moves a manifest's level to `effort` before it renders and hashes, and
  the harness reads a header's level under either name.
- **The edge is the reply.** The API renames at the one place every
  route answers JSON, and at the stream's frame. The runner protocol is
  left as stored, so a runner of an earlier release reads the level a
  newer server stores.
- **A level alone writes the header.** A create whose allow names a
  level other than the agent's own, on the agent's own model, writes the
  header's model with no `via`, so the session's model is present in the
  answer where before it was absent until a first change.
- **An old event that does not decode refuses the read.** A
  `session.model_changed` whose payload is not a change cannot be
  renamed, so the events list refuses it and the stream ends, logged,
  rather than answer one event under the old name.
- **A replayed answer is as it was stored.** An idempotency key's replay
  inside its window answers the bytes first answered, `effort` included
  for an answer an earlier release stored.
- **`supports.effort` of the catalog is unchanged.** It is a catalog
  source's flag for whether a model takes a level, read by the core and
  answered by no route of the API.
