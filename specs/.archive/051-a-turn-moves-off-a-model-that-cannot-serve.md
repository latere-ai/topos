---
title: "A turn moves off a model that cannot serve: a gateway's answer that the model is down asks the authorizer for another model inside the turn, one quick retry where none can be had, and an error a client can name"
status: complete
track: core
depends_on: [005-harness-loop.md, 006-identity.md, 007-models.md, 015-api.md, 016-runners.md, 038-routed-models.md, 049-reasoning.md]
affects: [models/, harness/, session/, runner/, internal/server/, internal/runnerapi/, internal/runnerrole/, cmd/toposd/, authorizer/doc.go, test/stubs/luxstub/, api/openapi.yaml]
effort: medium
created: 2026-10-05
updated: 2026-10-05
author: changkun
---

# A turn moves off a model that cannot serve

## Overview

A session on a routed name ([[038-routed-models]]) runs the model an
authorizer picked for it. When that model's upstream fails, a model
gateway such as Lux tries every target of the model, opens a circuit per
target after a run of failures, and answers `upstream_error` (502),
`provider_unavailable` (503) or `upstream_timeout` (504). Until this
spec the harness retried that answer as any 5xx under spec 005's policy:
six attempts, waiting 2, 4, 8, 16 and 32 seconds, each less up to a
fifth for jitter, about a minute in all, on the same model, and then
ended the turn with `model_error` and the gateway's words as the
message. A person watched the turn think for half a minute and more
and then read an error, while the routed name stood for several other
models that could have answered.

This spec moves the turn. On such an answer the harness asks the
authorizer at once which model the turn continues on, records the
change, and sends the step again on the model named, inside the same
turn, a bounded number of times. Where no move can be had, the same
model gets one quick retry. A turn that still fails ends with a code a
client names in one sentence.

## Current state

Built with this spec; see the Outcome.

## Design

### Which failures move a turn

`models.Down` reports a model gateway's answer that the model asked
cannot serve now: an HTTP error of status 5xx whose type is
`upstream_error`, `provider_unavailable` or `upstream_timeout`, Lux's
names. An upstream's own 429 reaches the core as `upstream_error`, since
Lux folds every retryable upstream status into it. These are not down,
and keep the policy they had:

| Failure | Why it is not down | Policy |
|---|---|---|
| `budget_exhausted`, `spend_exceeded` | a refusal about the caller's spend; another model is refused the same | stops the turn with `budget` ([[007-models]]) |
| the gateway's `rate_limited` | a bound on the caller's key | spec 005's retry |
| `model_not_allowed`, `model_disabled` and every 4xx | a refusal of the caller or the request | not retried |
| `store_unavailable`, `authorizer_unavailable` (503) | the gateway's own failure; every model behind it fails alike | spec 005's retry |
| a transport failure on the way to the gateway, a stream cut | the gateway, not the model, is unreachable | spec 005's retry |
| a provider reached without a gateway, in its own words (529 `overloaded_error`) | no gateway has tried another target yet | spec 005's retry |

Marking a model down for a failure of the gateway itself would move
every turn through every model of the pool, so the classification reads
the type and never the status alone.

### The gateway's detail

Lux sends the developer detail of a failure in the `Lux-Error-Detail`
header: for `upstream_error`, the upstream's own status and the start of
its body, such as `upstream status 429: {"error":{...}}`. The model
client keeps it on the error, at most `models.MaxDetail` (1024) bytes,
Lux's own bound, and the harness records it beside the error's message
in the failed `model.request`'s `error`, in the turn's `session.error`
detail and in a change's detail. The failover question carries it, so
an installation can tell a provider's rate limit, shared by every model
behind one account, from one model's outage.

### The question

The harness gains one callback, `Config.Failover(ctx, failed, detail)`,
asked only by the session's own thread, on a session whose model has a
`via`, while the turn has moves left. A thread's turn runs its own
agent's model, which no router picked, and never asks. The runner sets
the callback on every drive:

| Runner | Where the question goes |
|---|---|
| a `toposd serve` runner, in process | `Server.Failover`, toposd's question to its authorizer |
| a `toposd runner` process | `POST /internal/v1/leases/{session}/failover` on the server's internal listener, with `{generation, failed, detail}`, under the lease's generation as the token route checks it; `lease_lost` ends the lease, and a question the server could not answer is `no_failover` with why |

toposd asks `session.update` as the session's initiator, in the context
the session runs in, the context of its agent's owner ([[036-organization-owners]]):
the subject is the initiator's, and the claims carry `org_id` alone, as
a trigger's firing asks for its owner. No request of the initiator's is
in hand mid-turn, and the initiator is the one person every decider
admits for the session. The resource is the session's, with:

| Field | Value |
|---|---|
| `session_id` | the session's id |
| `model` | the routed name the session runs, its `via` |
| `current_model`, `current_model_via` | the model the session stands on, which failed, and that name |
| `failed_model` | the model that failed; its presence asks for a pick that passes over it |
| `failed_detail` | the gateway's developer detail of the failure, at most 1024 bytes; absent when the gateway sent none |

An authorizer that routes answers a switch to a routed name as it does
today, picking with `failed_model` passed over, and names the model in
`limits.model` and its level in `limits.reasoning`. toposd checks the
model by the rule a switch checks one by ([[007-models]]) and resolves
the level as a send resolves it, `""` being the agent's own
([[049-reasoning]]). The answer keeps the session's `via`. Nothing
moves, and the answer is the model the session stands on, when the
allow names no model or the same one, and when `failed` is not the
routed model the header names. A deny, an authorizer that cannot be
asked and a model the installation does not run are errors the harness
records as why the turn did not move.

### The move

On a down failure of a turn that may move, the harness sends no retry
of the same model. It asks the question; when the answer names another
model it connects it as a switch between turns connects one, and
appends in one batch the failed request's `model.request`, outcome
`error`, and `session.model_changed` made by the service:

```json
{"type": "session.model_changed", "turn": 1, "step": 1, "payload": {
  "by": {"subject": "service:authorizer", "kind": "service"},
  "old": {"name": "vendor/model-a", "via": "tier/quick"},
  "new": {"name": "vendor/model-b", "via": "tier/quick", "reasoning": "low"},
  "reason": "model_busy",
  "detail": "models: HTTP 502: upstream_error: The provider returned an error. (upstream status 429: ...)"}}
```

`reason` is new and is set on this change alone: `model_busy`, which a
client renders as "The model was busy, so another one answered." The
step then builds its request again for the new model's connection,
window and output limit, holds the new price to the session's budget,
and sends it. The header takes the change, so the next turn stays on
the new model, and the next send's question carries it as the model the
session stands on.

A turn moves at most `harness.MaxModelSwitches` (3) times: enough to
pass three entries of a routed name that fail together, as free models
behind one provider account do, and reach a fourth.

### The retry of a model that cannot move

A turn that may not move (a session on a model named itself, a runner
with no question, a turn out of moves) retries a down failure under
`harness.DownRetry`: one retry, one second after the failure, with no
jitter. A `Retry-After` longer than that second ends the attempts at
once. A turn whose question named no other model, could not be asked,
or named a model that cannot be connected ends at once: the router has
just said nothing else serves.

| | Before | After |
|---|---|---|
| a routed turn whose model is down | spec 005's six attempts on the same model, waits of 2, 4, 8, 16, 32 s less up to a fifth, about a minute, then `model_error` | no retry; the authorizer is asked at once and the step is sent on the model it names, up to 3 times a turn |
| a routed turn the authorizer cannot move | the same minute | ends at once with `model_busy` |
| a turn on a model named itself, or out of moves | the same minute | one retry a second later, then `model_busy` |
| every other retryable failure | spec 005's policy | unchanged |

A compaction's summary request never moves the turn and takes the one
quick retry.

### The turn that fails

A turn that ends on a down failure ends `error` with `detail`
`model_busy`, and its `session.error` is:

| Member | Value |
|---|---|
| `code` | `model_busy` |
| `message` | "The model is busy right now. Send your message again in a moment." |
| `retryable` | true |
| `detail` | the error and the gateway's detail, then why the turn did not move, when it could have: no other model was named, the question's error, or the model named and why it could not be connected |

Every other model failure keeps `model_error`.

### The roll

A core that adds fields to a question rolls after the authorizer that
reads them. An authorizer from before this spec reads `session.update`
with a routed name the session already stands on as a switch to its own
level and keeps the model, so a new core against it ends a down turn at
once with `model_busy`, in a second rather than a minute; it never
breaks. The deployment rolls the authorizer, then toposd and every
runner. A client that names `model_busy` and the change's `reason` may
roll at any time: before it does, it shows the code and the change as it
shows any.

## Not in this spec

- How an installation picks: which models stand for a name, which one a
  failure passes over and for how long. That is the authorizer's.
- Publishing a gateway's circuit or an upstream's status as structured
  fields; the detail is the gateway's developer text.
- Moving a thread's turn, or a compaction's request.
- A move on a stream that fails after its first byte, which the gateway
  writes as an error event and spec 005 reads by its text.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `models.Down` is the three gateway types at a 5xx and nothing else: not spend, the key's rate, a refusal, the gateway's own failure, a transport failure or a provider's own overload | `models.TestDown` | built |
| A routed turn whose model is down asks the question at once with the failed model and the gateway's detail, records the failed request and the service's change with `model_busy` in one batch between it and the request that answers, and answers within the turn at the level named, with no wait; the next turn stays on the new model | `harness.TestATurnMovesOffAModelThatCannotServe` | built |
| A turn moves at most `MaxModelSwitches` times, then takes one quick retry and ends `model_busy` with its sentence, retryable, and the gateway's answer in the detail | `harness.TestATurnThatRunsOutOfMovesEndsBusy` | built |
| A question that names no other model, fails, or names a model that cannot be connected ends the turn at once with `model_busy` and why, with no retry and no change | `harness.TestATurnTheRouterCannotMoveEndsAtOnce` | built |
| A turn that cannot move retries a down model once, a second later, and not at all past a longer `Retry-After`; a gateway's own failure and a provider's overload keep spec 005's six attempts and waits | `harness.TestAModelThatCannotServeTakesOneQuickRetry` | built |
| A spent wallet on the model moved to stops the turn with `budget`, and spend never asks the question | `harness.TestASpentWalletOnTheModelMovedToStopsWithBudget` | built |
| toposd asks `session.update` as the initiator with `org_id` of the session's context, with `model`, `current_model`, `current_model_via`, `failed_model` and `failed_detail` cut at 1024 bytes on a character's boundary, checks the model named, appends nothing, and moves nothing for an allow of no or the same model, for a failed model the header does not name, or for a session on a model named itself; a deny and an unknown model are errors | `internal/server.TestAFailoverAsksTheInitiatorForAnotherModel`, `TestAFailoverThatNamesNoOtherModelMovesNothing`, `TestAnOrganizationsSessionFailsOverInItsContext` | built |
| A runner process's lease asks the question over the internal listener under its generation, a stale one is `lease_lost`, a question the server cannot answer is `no_failover` with why, and a server with no question answers the failed model | `internal/runnerrole.TestARemoteLeaseAsksTheServerToFailOver` | built |
| A drive asks its lease's question, else the runner's, else none | `runner.TestADriveAsksTheFailoverOfItsLease` | built |
| Through toposd against an authorizer that routes, a routed session whose model the gateway answers `upstream_error` over a 429 is answered in its first turn by the model the `session.update` allow names, the question carrying `failed_model` and `failed_detail` | `cmd/toposd.TestARoutedTurnMovesOffAModelThatCannotServe` | built |

## Outcome

Built on 2026-10-05, in no release yet. Every criterion has its test.
What shipped differs from the first draft in these points:

- **The gateway's detail.** The draft asked with the failed model alone.
  Live failures of two free models behind one provider account, each in
  about 140 ms, suggested the account's shared rate limit, which no
  record the core kept could show: the model client read the gateway's
  type and dropped `Lux-Error-Detail`. The client now keeps the detail,
  the records show it, and the question carries it as `failed_detail`.
- **Three moves, not two.** A pool whose first three entries fail
  together needs three moves to reach its fourth.
- **A question that names no other model ends the turn at once** rather
  than taking the quick retry: the authorizer has just answered that
  nothing else serves, and the gateway has already tried every target of
  the model.
