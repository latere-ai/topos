---
title: "Routed models: a model name the authorizer resolves, at a session's create, at a model change and between turns"
status: drafted
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

Nothing of this is built. A model name is sent to the gateway as it is
written ([[015-api]]): any name runs if the door lists it. A decision
carries ceilings and a scope and no model. A model change appends
`session.model_changed` and the next turn connects again
([[005-harness-loop]]); nothing reads how long the session has been
quiet. Cache breakpoints are placed on every request ([[010-context]]),
and nothing knows how long a provider keeps what they mark.

## Design

### A decision may name the model

An allow may carry `model`: the name of the model to run. It is read at
three decisions.

| Decision | With `model` in the allow |
|---|---|
| a session's create | the session starts on that model instead of the agent's |
| `session.update` that names a model | the session changes to that model instead of the one asked |
| `session.send` | the session changes to that model before the turn starts |

Without `model` each behaves as it does today. A decision never names a
model for a turn in progress: the three are asked between turns.

### Both names are kept

A session's model gains `via`: the name that was asked, when the
authorizer answered another. `session.model_changed` carries it in `old`
and `new` as the session does.

```json
{"model": {"name": "vendor/model-a", "via": "tier/quick"}}
```

A client that offers the choice reads `via` and never maps a model's
name back to the choice. `model.request` keeps recording the model that
ran. A change the authorizer made at a send is recorded as made by the
service, not by the person who sent the message.

A name the authorizer does not resolve is sent as written, and fails as
an unknown model does today when no door lists it.

### What the authorizer is told

The resource of the three decisions carries the session's model as it
stands, `name` and `via`. At a send it also carries how long ago the
session's last model request ended, so an authorizer that keeps a
session on one model while a cache is warm can tell when it is not. The
runner computes nothing about caches itself: how long a cache lives is a
property of a provider, and the installation that pools providers is
what knows it.

### The switch itself

A change between turns is the change a person's `PATCH` makes: the event,
then a new connection for the next turn. The context is the log's, so
the next model reads what the last one wrote. Reasoning effort is kept
when the new model takes one and dropped when it does not, as a person's
switch does.

## Not in this spec

- How an installation chooses: pools, order, prices, what a person may
  narrow. That is the authorizer's.
- A cache's lifetime per provider, and placing breakpoints by it.
- Changing the model inside a turn.

## Acceptance criteria

1. With an authorizer that answers `model` at create, the session's
   header holds that model and `via` holds the agent's, and the first
   `model.request` names the model that ran.
2. A `PATCH` to a name the authorizer resolves answers the session with
   the resolved model and `via`, and appends one `session.model_changed`.
3. A send whose allow names a model different from the session's
   appends `session.model_changed` made by the service before the turn's
   first `model.request`, and one whose allow names the same model
   appends nothing.
4. The send's resource carries the time since the last model request,
   and none on a session that has made no request.
5. An allow without `model` leaves every route as it is today, for a
   conformance run against an authorizer that never sets it.
6. A name no door lists and the authorizer does not resolve fails as
   `model_unknown`.
