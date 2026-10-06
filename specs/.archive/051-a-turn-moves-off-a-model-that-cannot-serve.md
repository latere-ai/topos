---
title: "A turn moves off a model that cannot serve: a gateway's answer that the model is down asks the authorizer for another model inside the turn, one quick retry where none can be had, and an error a client can name"
status: complete
track: core
depends_on: [005-harness-loop.md, 006-identity.md, 007-models.md, 015-api.md, 016-runners.md, 038-routed-models.md, 049-reasoning.md]
affects: [models/, harness/, session/, runner/, internal/server/, internal/runnerapi/, internal/runnerrole/, internal/hosted/, cmd/toposd/, authorizer/doc.go, test/stubs/luxstub/, test/stubs/keystub/, api/openapi.yaml]
effort: medium
created: 2026-10-05
updated: 2026-10-06
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
| `model_not_allowed`, `model_disabled` and every 4xx but `upstream_rejected` | a refusal of the caller or the request | not retried |
| `store_unavailable`, `authorizer_unavailable` (503) | the gateway's own failure; every model behind it fails alike | spec 005's retry |
| a transport failure on the way to the gateway, a stream cut | the gateway, not the model, is unreachable | spec 005's retry |
| a provider reached without a gateway, in its own words (529 `overloaded_error`) | no gateway has tried another target yet | spec 005's retry |

Marking a model down for a failure of the gateway itself would move
every turn through every model of the pool, so the classification reads
the type and never the status alone.

A provider's refusal of the request, `upstream_rejected`, is not down
either. A routed turn asks the same question about it with a reason, as
"A provider's rejection of the request" says.

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

The harness gains one callback, `Config.Failover(ctx, standing, failed, detail)`,
asked only by the session's own thread, on a session whose model has a
`via`, while the turn has moves left. A thread's turn runs its own
agent's model, which no router picked, and never asks. The runner sets
the callback on every drive:

| Runner | Where the question goes |
|---|---|
| a `toposd serve` runner, in process | `Server.Failover`, toposd's question to its authorizer |
| a `toposd runner` process | `POST /internal/v1/leases/{session}/failover` on the server's internal listener, with `{generation, failed, detail}` and `standing` when the turn stands on another model than `failed`, under the lease's generation as the token route checks it; a request with no `standing` stands on `failed`; `lease_lost` ends the lease, and a question the server could not answer is `no_failover` with why |

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
| `failed_model` | the model that failed: the one the session stands on, or one an earlier answer of the turn named that could not be connected (see "A model named that cannot be connected"); its presence asks for a pick that passes over it |
| `failed_detail` | the developer detail of the failure, at most 1024 bytes: the gateway's, or why the model named could not be connected; absent when there is none |
| `failed_reason` | why the model failed when it is not that it cannot serve now: `rejected` for a request the provider rejected; absent for a model that cannot serve now or could not be connected |

An authorizer that routes answers a switch to a routed name as it does
today, picking with `failed_model` passed over, and names the model in
`limits.model` and its level in `limits.reasoning`. toposd checks the
model by the rule a switch checks one by ([[007-models]]) and resolves
the level as a send resolves it, `""` being the agent's own
([[049-reasoning]]). The answer keeps the session's `via`. Nothing
moves, and the answer is the model the session stands on, when the
allow names no model, the same one or the failed one, when `standing`
is not the routed model the header names, and when `failed` is not a
model of the same routed name. A deny, an authorizer that cannot be
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

### Connecting the model named

The runner connects the model answered as any model: its figures are the
embedded catalog's, overlaid by those the door's model list gives for
the session's key, read with that key. A Lux door lists the models the
presented key may use, and the authorizer widens the session's key to
the model it names as it answers. A gateway of several replicas applies
that change on each replica within a window of its own, so the replica
the runner reaches moments later may still list the key's earlier
models alone. While a door answers a list of other models and not the
one connected, the runner reads it again every 250 ms, for at most
`hosted.DoorSettle` (3 seconds); Lux's replicas read a key's change
within a second. A door that answers no list, or a list of no model, is
read once, as before. A model the list still does not name after the
settle is connected as before, by the catalog's figures, or refused as
`model_unknown`. The same settle covers a model a send moved the session
to, which the authorizer widens the key to in the same way. The server's
own check of a model (`Runnable`) reads the door with the installation's
key, which no move widens, and reads it once.

### A model named that cannot be connected

A model the answer names that cannot be connected, with no figures once
the settle has passed, a key the authorizer refuses, or any other setup
failure, is a move that failed. It counts against `MaxModelSwitches`,
and while moves are left the next question stands on the model the turn
runs, as `current_model`, and names the one that could not be connected
as `failed_model`, with why as `failed_detail`, so the authorizer passes
over it as over any failed model. The change the turn then records
carries, after the gateway's answer, each model passed over and why. A
turn that runs out of moves this way ends `model_busy` with every reason
in the detail, and nothing recorded but the failed request. A turn
interrupted while it connects the model named asks nothing more, since
the connection failed for the turn's own end, and stops interrupted.

### The retry of a model that cannot move

A turn that may not move (a session on a model named itself, a runner
with no question, a turn out of moves) retries a down failure under
`harness.DownRetry`: one retry, one second after the failure, with no
jitter. A `Retry-After` longer than that second ends the attempts at
once. A turn whose question named no other model or could not be asked
ends at once: the router has just said nothing else serves. A model
named that cannot be connected is asked past, as the section above
says.

| | Before | After |
|---|---|---|
| a routed turn whose model is down | spec 005's six attempts on the same model, waits of 2, 4, 8, 16, 32 s less up to a fifth, about a minute, then `model_error` | no retry; the authorizer is asked at once and the step is sent on the model it names, up to 3 times a turn |
| a routed turn the authorizer cannot move | the same minute | ends at once with `model_busy` |
| a turn on a model named itself, or out of moves | the same minute | one retry a second later, then `model_busy` |
| every other retryable failure | spec 005's policy | unchanged |

A compaction's summary request never moves the turn and takes the one
quick retry.

### A provider's rejection of the request

A model gateway answers `upstream_rejected` (400) when it reached the
provider and the provider refused the request with a 4xx of its own.
Lux does so for any upstream 4xx but 401 and 403, which are its own
credential and answer `upstream_error`, and 408 and 429, which it
retries on the model's next target; it tries no other target after a
rejection. The refusal is either the request's, such as a body the model
cannot take, or the provider's, which no longer serves the model under
that name, as providers withdraw and change free variants without
notice. A provider that has withdrawn a model answers in milliseconds,
every request, and before this section every turn that reached such a
model ended with `model_error`, while the routed name stood for other
models that would have answered.

The core cannot tell the two apart, and which models are worth passing
over is the authorizer's to say. `models.Rejected` reports the answer:
the type `upstream_rejected` at a 4xx, never the status alone. A turn
that may move asks the question of "The question" for it at once, the
callback given `harness.FailedRejected` as its reason:

| Field | Value |
|---|---|
| `failed_model` | the model the request was sent on, which is also `current_model` |
| `failed_reason` | `rejected` |
| `failed_detail` | the gateway's code and its developer detail, such as `upstream_rejected: upstream status 404: {"error":...}`, at most 1024 bytes |

An allow that names another model moves the turn as "The move" says: the
failed request and the service's change in one batch, reason
`model_busy`, the gateway's answer in the change's detail, and one of
the turn's `MaxModelSwitches` moves. `model_busy` is kept rather than a
reason of its own: to the person, the model could not answer and another
one did, and the detail says which failure it was. A model named that
cannot be connected is asked past as "A model named that cannot be
connected" says, with no reason, since it failed to connect, not to
serve.

An allow that names no other model, a deny, a question that cannot be
asked, and a turn that may not move end the turn at once with
`model_error` and the gateway's sentence, as a rejection ended before.
The request may be what was refused, so the quick retry is not taken,
and the turn does not end `model_busy`, whose sentence asks the person
to send the message again. The `session.error` detail carries the
gateway's status and type, its developer detail in parentheses, and why
the turn did not move, such as `HTTP 400 upstream_rejected (upstream
status 404: {...}); no other model was named`.

The gateway's own refusals carry their own types and ask nothing:
`invalid_request`, `model_not_allowed`, `model_disabled`,
`model_unpriced`, `rate_limited`, `budget_exhausted` and
`spend_exceeded` keep the policies of the table above.

| | Before | After |
|---|---|---|
| a routed turn whose request the provider rejects | ends at once with `model_error` | the authorizer is asked at once; the step is sent on the model it names, within the same 3 moves |
| a routed turn the authorizer keeps on the model, or refuses | ends at once with `model_error` | the same, the detail saying why it did not move |
| a turn on a model named itself, or out of moves | ends at once with `model_error` | unchanged |

### The turn that fails

A turn that ends on a down failure ends `error` with `detail`
`model_busy`, and its `session.error` is:

| Member | Value |
|---|---|
| `code` | `model_busy` |
| `message` | "The model is busy right now. Send your message again in a moment." |
| `retryable` | true |
| `detail` | the error and the gateway's detail, then why the turn did not move, when it could have: each model named that could not be connected and why, then that no other model was named or the question's error |

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

A question with `failed_reason` rolls the same way, and here the order
is required. An authorizer that reads `failed_model` and not
`failed_reason` reads the question as one about a model that cannot
serve, and would pass over a model on any rejected request, the
request's own fault among them. An authorizer that refuses a
`failed_reason` it does not know, or answers no other model, leaves the
turn as before, ending `model_error`. The deployment rolls the
authorizer that reads it, then toposd and every runner. A runner with
this change asks a server before it with `reason` in the runner
protocol's failover request, a member that server does not decode, so it
answers `invalid_request` and the turn ends `model_error` as before.

A question whose `failed_model` is a model named that could not be
connected, not the one `current_model` names, is a shape the authorizer
must accept: an authorizer that routes may hold it to the model it
recorded when it named it. One that holds `failed_model` to
`current_model` refuses it as `invalid_resource`, and the turn ends
`model_busy` at once, as it did before; nothing breaks, and such an
authorizer rolls first to accept it. The settle of "Connecting the model
named" needs no authorizer change. The runner protocol sends `standing`
only when it differs from `failed`, so a runner with this change asks a
server before it the first question as before; the second, which a
server before it refuses as `invalid_request`, ends the turn as before
too.

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
| A question that names no other model or fails ends the turn at once with `model_busy` and why, with no retry and no change; so does one that names a model that cannot be connected when the next question names no other | `harness.TestATurnTheRouterCannotMoveEndsAtOnce` | built |
| A model named that cannot be connected is a failed move: the next question stands on the model the turn runs and names it as `failed_model` with why as the detail, the turn moves to the model that answer names, and the change says which model was passed over; such models count against `MaxModelSwitches`, and a turn that runs out of moves on them ends `model_busy` with each reason | `harness.TestAModelNamedThatCannotBeConnectedIsPassedOver` | built |
| A turn interrupted while it connects the model named asks the router nothing more and stops interrupted, with no change recorded | `harness.TestATurnInterruptedWhileConnectingAsksNothingMore` | built |
| A door that lists other models and not the one connected is read again until it names it, for at most `hosted.DoorSettle`; one that never does is read for the settle alone; no list, or a list of no model, is read once; a drive that ends stops the reads | `internal/hosted.TestAConnectionWaitsForTheDoorToListItsModel` | built |
| A turn that cannot move retries a down model once, a second later, and not at all past a longer `Retry-After`; a gateway's own failure and a provider's overload keep spec 005's six attempts and waits | `harness.TestAModelThatCannotServeTakesOneQuickRetry` | built |
| A spent wallet on the model moved to stops the turn with `budget`, and spend never asks the question | `harness.TestASpentWalletOnTheModelMovedToStopsWithBudget` | built |
| toposd asks `session.update` as the initiator with `org_id` of the session's context, with `model`, `current_model`, `current_model_via`, `failed_model` and `failed_detail` cut at 1024 bytes on a character's boundary, checks the model named, appends nothing, and moves nothing for an allow of no, the same or the failed model, for a turn standing on a model the header does not name, for a failed model of another routed name, or for a session on a model named itself; a deny and an unknown model are errors; a failed model named before that could not be connected is asked about beside the model the session stands on | `internal/server.TestAFailoverAsksTheInitiatorForAnotherModel`, `TestAFailoverThatNamesNoOtherModelMovesNothing`, `TestAFailoverPassesOnAModelNamedThatCouldNotBeConnected`, `TestAnOrganizationsSessionFailsOverInItsContext` | built |
| A runner process's lease asks the question over the internal listener under its generation, with the model it stands on and the failed one, and a request with no standing model stands on the failed one; a stale one is `lease_lost`, a question the server cannot answer is `no_failover` with why, and a server with no question answers the model the turn stands on | `internal/runnerrole.TestARemoteLeaseAsksTheServerToFailOver` | built |
| A drive asks its lease's question, else the runner's, else none | `runner.TestADriveAsksTheFailoverOfItsLease` | built |
| Through toposd against an authorizer that routes, on an installation whose sessions act with their own keys and a door that answers each key for the models it selects, a routed session whose model the gateway answers `upstream_error` over a 429 is answered in its first turn by the model the `session.update` allow names, the question carrying `failed_model` and `failed_detail`, while the door applies the key's widening only some time after the allow, and every request carries the session's key | `cmd/toposd.TestARoutedTurnMovesOffAModelThatCannotServe` | built |
| Through toposd, a model the allow names that the door never lists for the key is passed over: the second question names it as `failed_model` beside the model the session stands on, and the turn is answered by the model the second allow names | `cmd/toposd.TestARoutedTurnPassesOverAModelItCannotConnect` | built |
| `models.Rejected` is the gateway's `upstream_rejected` at a 4xx and nothing else: not a model that cannot serve, and none of the gateway's own refusals | `models.TestRejected` | built |
| A routed turn whose request the provider rejects asks the question at once with `failed_reason` `rejected` and the gateway's code and detail, moves to the model named, records the failed request and the change with `model_busy`, and answers within the turn; a model named that cannot be connected is asked past with no reason | `harness.TestATurnMovesOffAModelItsProviderRejected` | built |
| A rejected request the router keeps on its model, or whose question is refused, ends the turn at once with `model_error` and the gateway's sentence, not retryable, with no retry and no change, the detail saying why it did not move | `harness.TestARejectedRequestTheRouterKeepsEndsWithTheModelsError` | built |
| Moves off rejected requests count against `MaxModelSwitches`; a rejection past them asks nothing and ends `model_error` | `harness.TestRejectionsCountAgainstTheMoves` | built |
| The gateway's own refusals of a routed turn ask nothing and keep their policies; a rejection on a session on a model named itself asks nothing and ends `model_error` at once | `harness.TestTheGatewaysOwnRefusalAsksNothing` | built |
| toposd asks the rejection's question with `failed_reason` beside `failed_model` and `failed_detail`, and an allow that names no model moves nothing | `internal/server.TestAFailoverOfARejectedRequestSaysWhy` | built |
| The runner protocol's failover request carries `reason` only when there is one, a drive passes it to the server, and a reason the server does not know is `invalid_request` | `internal/runnerrole.TestARemoteLeaseAsksTheServerToFailOver`, `runner.TestADriveAsksTheFailoverOfItsLease` | built |
| Every model failure's `session.error`, `model_error`, a spend refusal and `compaction_failed` alike, carries the gateway's developer detail after the status and type, at most `models.MaxDetail` bytes cut on a character's boundary, with the message unchanged | `harness.TestAModelErrorsDetailCarriesTheGatewaysDetail`, `harness.TestAFailedCompactionCarriesTheGatewaysDetail` | built |
| Through toposd, a routed session whose first model the gateway answers 400 `upstream_rejected` asks `session.update` with `failed_reason` `rejected` and `failed_detail` `upstream_rejected: <the gateway's detail>`, and its first turn is answered by the model the allow names | `cmd/toposd.TestARoutedTurnMovesOffAModelItsProviderRejected` | built |

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

### A fix after the release

v0.19.0 shipped the design above, and its first failover on a hosted
installation failed. A Quick session started on a free model, whose
first request the gateway answered `upstream_error` over the upstream's
429 for its shared free pool. The authorizer named the next entry, a
priced model the embedded catalog does not hold, and the turn ended
`model_busy` with `... was named and could not be connected: models:
model_unknown: no source gives the input window and output limit of
...`.

**Cause.** The authorizer did widen the session's key to the model it
named before it answered, and the gateway applied the widening: its
record of the key held the model. The gateway runs two replicas, each
of which serves a key from a cache it drops when it reads the key's
change from its journal, once a second, or at the cache's window of ten
seconds. The widening was applied at one replica, and the runner read
the door's model list 11 to 21 ms later at the other, which answered
from its cache with the key's earlier selection: the free model alone.
`dialect.Served` found no entry, the catalog had none, and the model was
`model_unknown`. Every failover of that hour showed the same timing, and
none reached a request. A switch between turns had worked by latency
alone: the turn after it started seconds later, after every replica had
read the change. A request in that window on a model the catalog does
hold would have been refused `model_not_allowed` the same way.

**Fix.** The runner tolerates the gateway's convergence, as
"Connecting the model named" says: while a door lists other models and
not the one connected, it reads the list again for at most
`hosted.DoorSettle`. The authorizer still decides what the key
reaches; the runner only waits for the gateway to apply it. As a
defense, a model named that still cannot be connected counts as a
failed move, as "A model named that cannot be connected" says, where
v0.19.0 ended the turn at the first.

**The defense alone would have done harm.** Without the settle, each
model named in that hour would have been passed as failed and marked
down by the authorizer for every session for its cool-off, a healthy
priced model among them, and the next one named would have met the same
lag. It is a defense for a model that truly cannot be connected, behind
the settle.

**The embedded catalog is not where a priced model's figures belong.**
It is the fallback for a model no door lists. A door lists a priced
model, with its figures, as soon as the key reaches it; copying such
figures into the catalog would have hidden this fault behind a
`model_not_allowed` refusal and kept two sources of the same figures.

The stand-in gateway used by `cmd/toposd`'s tests listed every model for
any key, so the test of the whole path passed. `luxstub.Server.Select`
now answers each key for the models it selects, a request on another is
refused `model_not_allowed`, and the test widens the session's key only
some time after the authorizer's allow, as the lagging replica did.

### A provider's rejection

On a hosted installation, a routed turn moved off a free model that was
down to the next free model of its routed name, and the gateway answered
the request there `upstream_rejected` in about 35 ms. The turn ended
`model_error`, since only the three types of `models.Down` asked the
question, and a session that started on that model ended the same way at
its first request. The gateway's log showed every request on that model
rejected, in 19 to 41 ms, over four hours of the same day, after it had
answered earlier that afternoon, while the next entry of the routed
name, a priced model, would have answered. The log keeps no upstream
status, so whether the provider had withdrawn the variant is not known;
either way, an authorizer asked could have passed over it.

"A provider's rejection of the request" was built on 2026-10-06 and is
in no release yet. The decision stays the authorizer's: the core names
the failure and does not judge which models a rejection should pass
over. An installation that passes over a free model on a rejection, and
keeps a priced one, needs its authorizer to read `failed_reason` before
this core rolls, as "The roll" says.

The same change made every model failure's `session.error` carry the
gateway's developer detail. Before it, only `model_busy` did, through
`models.Described`: a `model_error`, a spend refusal and a failed
compaction read `HTTP 400 upstream_rejected` alone, and the provider's
own status and words were on the failed `model.request` and nowhere a
client reads an error. Each now reads the status and type, then the
gateway's detail in parentheses, at most `models.MaxDetail` bytes; the
`message` is unchanged (`harness.TestAModelErrorsDetailCarriesTheGatewaysDetail`,
`harness.TestAFailedCompactionCarriesTheGatewaysDetail`).
