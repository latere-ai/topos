---
title: "Routed models: a model name the authorizer resolves, at a session's create, at a model change and between turns"
status: complete
track: core
depends_on: [005-harness-loop.md, 010-context.md, 012-permissions-and-approvals.md, 015-api.md]
affects: [authorizer/, session/, internal/server/, harness/, manifest/]
effort: medium
created: 2026-10-04
updated: 2026-10-04
author: changkun
---

# Routed models

## Overview

An agent names one model, and a session runs that model until someone
changes it. An installation may want a name that stands for a choice:
"the quick one", which is whichever of several models the installation
offers that person today. Which model that is depends on who asks and on
what the installation allows, so it is the authorizer's to say, and it
must stay the same from one turn to the next for as long as the
provider's prompt cache holds, or every turn pays for its whole prefix
again.

This spec lets an authorizer's decision name the model a session runs,
keeps the name that was asked beside it, and asks again between turns
once the session has been quiet for a while.

## Current state

Built. An allow's `limits.model` is read at a session's create, at a
`session.update` that names a model and at `session.send`
(`authorizer/limits.go`, `internal/server/sessions.go`,
`internal/server/update.go`). The session's model reference and
`session.model_changed` carry `via` (`session/session.go`), and the
three questions carry the model the session stands on, with
`idle_seconds` at a send. The harness and the runner are unchanged: a
turn runs the model its session's header names when it starts
([[005-harness-loop]]), and a model the hosted runner connects is the
header's ([[015-api]]). An agent's `spec.model.name` was any name
already, so the manifest is unchanged. Cache breakpoints are placed on
every request as before ([[010-context]]), and nothing in the core
knows how long a provider keeps what they mark.

## Design

### A decision may name the model

An allow may carry `model` in its `limits` object: the name of the
model to run, a string. It is read at three decisions.

| Decision | With `limits.model` in the allow |
|---|---|
| a session's create | the session starts on that model instead of the agent's |
| `session.update` that names a model | the session changes to that model instead of the one asked |
| `session.send` | the session changes to that model before the turn starts |

```json
{"allow": true, "limits": {"model": "vendor/model-a"}}
```

Without `model` each behaves as it does without this spec. A decision
never names a model for a turn in progress: a change appended while a
turn runs leaves that turn on its model and takes effect at the next
one, as a person's change does ([[015-api]]). An allow of an effort
change alone, which names no model, moves nothing whatever it carries.
A `limits.model` that is not a string, or has space around it, refuses
the request as `authorizer_unavailable`, as any limit toposd cannot
read does ([[006-identity]]).

A fork is not one of the three. It starts on the model the session it
forks stood on at the fork point, with the name that model was asked
by ([[017-external-runners-handoff-fork]]): the model its create's
allow named when the copied log holds no change. Its first turn starts
with a send, which is asked as any send is.

### Both names are kept

A session's model gains `via`: the name that was asked, when the
authorizer answered another. `session.model_changed` carries it in `old`
and `new` as the session does.

```json
{"model": {"name": "vendor/model-a", "via": "tier/quick"}}
```

`via` is the agent's name for its model at a create, the name a
`PATCH` asked at a change, and at a send the name the session was
already asked by, or the model it ran when it had none. It is absent
when the session runs the name asked. A client that offers the choice
reads `via` and never maps a model's name back to the choice.
`model.request` keeps recording the model that ran. A change the
authorizer made at a send is recorded as made by the service, not by
the person who sent the message:

```json
{"type": "session.model_changed", "payload": {"by": {"subject": "service:authorizer", "kind": "service"},
  "old": {"name": "vendor/model-a", "via": "tier/quick"}, "new": {"name": "vendor/model-b", "via": "tier/quick"}}}
```

It is appended in one batch with the event that was sent, straight
before it, so a refused send changes nothing. A change made at a
`PATCH` is the person's, as before. A session's create appends nothing
for its model: the header holds it from the start.

A name the authorizer does not resolve is sent as written, and fails as
an unknown model does when no door lists it. The model a session is put
on is checked by the one rule a create and a switch check a model by
([[007-models]]), after the question, since the name asked may be one
only the authorizer resolves: the model the allow names, or the name
asked when it names none. One the installation does not run is
`model_unknown`, or `model_unavailable` when its figures could not be
read, at the create, the `PATCH` or the send, and nothing is created or
appended.

### What the authorizer is told

The resource of each decision carries the session's model as it stands,
its name and the name it was asked by, as flat fields
([[006-identity]]).

| Decision | Fields |
|---|---|
| `session.create` | `model`, the agent's name for its model |
| `session.update` | `model`, the name the change asks, as before, when it names one; `current_model`, the model the session stands on; `current_model_via`, its `via`, when it has one |
| `session.send` | `model`, the model the session stands on; `model_via`, its `via`, when it has one; `idle_seconds` |
| `session.fork` | `model`, the model the fork starts on; `model_via`, its `via`, when it has one |

A `session.update` carried `model` as the name asked before this spec,
and a field keeps its meaning, so there the session's own model has
names of its own.

`idle_seconds` is how long ago the session's last model request ended:
the whole seconds since its last `model.request` was appended, on
whichever thread, and absent on a session that has made none. An
authorizer that keeps a session on one model while a cache is warm can
tell from it when it is not. The runner computes nothing about caches
itself: how long a cache lives is a property of a provider, and the
installation that pools providers is what knows it.

```json
{"kind": "session", "id": "ses_01", "agent": "agent_01", "owner": "https://login.example|alice", "runner": "hosted",
  "sender": "https://login.example|alice", "event_type": "user.message",
  "model": "vendor/model-a", "model_via": "tier/quick", "idle_seconds": 450}
```

### The switch itself

A change between turns is the change a person's `PATCH` makes: the event,
then a new connection for the next turn. The context is the log's, so
the next model reads what the last one wrote. Reasoning effort is kept
when the new model takes one and dropped when it does not, as a person's
switch does: the session's model keeps the effort across the change,
and a model that takes none ignores it ([[015-api]]).

## Not in this spec

- How an installation chooses: pools, order, prices, what a person may
  narrow. That is the authorizer's.
- A cache's lifetime per provider, and placing breakpoints by it.
- Changing the model inside a turn.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| With an authorizer that answers `model` at create, the session's header holds that model and `via` holds the agent's, and the first `model.request` names the model that ran | `internal/server.TestARoutedSessionStartsOnTheModelItsAllowNames`, `cmd/toposd.TestARoutedSessionRunsTheModelsItsAuthorizerNames` | built |
| A `PATCH` to a name the authorizer resolves answers the session with the resolved model and `via`, and appends one `session.model_changed` | `internal/server.TestAPatchToANameTheAuthorizerResolves` | built |
| A send whose allow names a model different from the session's appends `session.model_changed` made by the service before the turn's first `model.request`, and one whose allow names the same model appends nothing | `internal/server.TestASendMovesTheSessionToTheModelItsAllowNames`, `cmd/toposd.TestARoutedSessionRunsTheModelsItsAuthorizerNames`, `harness.TestARoutedTurnRunsTheModelNotTheNameAsked` | built |
| The send's resource carries the time since the last model request, and none on a session that has made no request | `internal/server.TestASendTellsHowLongTheSessionHasBeenQuiet` | built |
| An allow without `model` leaves every route as it is today, for a conformance run against an authorizer that never sets it | `internal/server.TestAnAllowWithoutAModelRoutesNothing`, `authorizer.TestScaffoldSpeaksTheVocabulary`, `authorizer.TestARoutingScaffoldSpeaksTheVocabulary`, the route tests of [[015-api]] under the owner policy | built |
| A name no door lists and the authorizer does not resolve fails as `model_unknown` | `internal/server.TestANameNothingResolvesIsUnknown`, `cmd/toposd.TestARoutedSessionRunsTheModelsItsAuthorizerNames` | built |

## Outcome

Built on 2026-10-04 and in no release yet. Every criterion has its
test. What shipped differs from the draft in these points:

- **The model is checked after the question.** A create and a `PATCH`
  checked the model before they asked the authorizer, so the authorizer
  decided on a model that exists. A name only the authorizer resolves
  cannot be checked first, so both now ask and then check the model
  that will run. An authorizer is therefore asked about a name the
  installation may not run, and a deny of one is `forbidden` where it
  was `model_unknown`. An allow answers as before.
- **The field names.** The decision's model is `limits.model`. The
  session's model as it stands is flat fields, `model` and `model_via`,
  not an object of `name` and `via`; on `session.update`, where `model`
  already was the name asked, it is `current_model` and
  `current_model_via`.
- **A create appends nothing.** The header takes the model the create's
  allow names, with no `session.model_changed`: the session never ran
  another.
- **A fork carries the model over.** Because a create appends nothing,
  a fork's copied log may not name the model the session ran, so the
  fork takes the model the session stood on at the fork point and is
  checked by it, where it was checked by the agent's. Without this a
  fork of an agent whose model no door lists would be `model_unknown`.
  The allow of `session.fork` is not read for a model.
- **Every `session.send` is read**, a confirmation and a client tool's
  result as well as a message. A turn that waited on one resumes on the
  model the allow names, as it would after a `PATCH` made while it
  waited.
- **A send reads the session's agent version and the end of its log**
  to say what the session stands on and how long it has been quiet,
  which it did not read before. The question's resource changes with
  `idle_seconds`, so a cached allow of `session.send` is seldom reused.
- **Limits are decoded at a send and at a model change**, where they
  were ignored: an allow whose limits do not decode refuses either as
  `authorizer_unavailable`.
