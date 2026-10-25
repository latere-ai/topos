---
title: "The model a trigger's sessions start on: spec.session.model names the model and the reasoning level each session a firing starts begins on, in the shape a session's change of its model takes, asked at session.create as a create that names its model is and answered by the allow as the agent's own model is, while a firing that continues a session leaves its model"
status: testing
track: core
depends_on: [.archive/022-triggers.md, .archive/038-routed-models.md, .archive/049-reasoning.md, 077-rollouts.md, 086-proactive-agents.md]
affects: [manifest/v1/, manifest/, internal/server/, internal/proposal/, api/openapi.yaml, docs/]
effort: small
created: 2026-10-25
updated: 2026-10-25
author: changkun
---

# The model a trigger's sessions start on

## Overview

A trigger's every session starts on its agent's model at its agent's
reasoning level ([[022-triggers]]). A client that lets a person choose,
per routine, how much effort its runs take cannot say so: an
installation whose agent names a routed name ([[038-routed-models]])
for its quickest level runs every routine at that level, and a task that
needs more fails at it. A conversation a person starts can move to
another level with a change of its model after its create, but a
firing's session has nobody to make that change before its first turn.

This spec adds one field. A trigger's `spec.session.model` names the
model and the level each session a firing starts begins on. The firing's
`session.create` is asked with it, and the allow answers it as it
answers the agent's own model. A firing that continues an open session
sends its message and changes nothing about the model that session
runs.

## Current state

- `TriggerSession` (`manifest/v1/kinds.go`) names no model. The engine's
  `start` hands the actor the trigger's spec, the firing and the
  rendered message (`internal/triggers/engine.go`), and the actor creates
  the session through the create code of the API with no model
  (`internal/server/triggers.go`).
- A create asks `session.create` with `model`, the agent's name for its
  model. An allow that names another model starts the session on it,
  with the agent's name as `via`, and an allow that names a level starts
  it at that level ([[038-routed-models]], [[049-reasoning]]).
- `POST /sessions` takes `model {name, reasoning}`, asked with that name
  and level, and run as named: an allow that names another model or
  another level is `model_not_allowed` ([[077-rollouts]], R3), so a run
  that a comparison reads ran the model it says.
- `PATCH /sessions/{id}` takes `model {name, reasoning}` and runs the
  model the allow names in place of the name asked, keeping the name as
  `via` (`internal/server/update.go`).

## Design

### Recommendations

| Item | Recommended | Weighed against, and why not |
|---|---|---|
| The field's shape | `{name, reasoning}`, either member or both, the shape `PATCH /sessions/{id}` takes | a level alone: a client that names a routed name per level, or a model outright, could not; the agent's whole `spec.model`: family, dialect, base URL and credential are the agent's, never a firing's |
| How the allow answers it | as it answers the agent's own model: the model it names runs in place of the name asked, kept as `via`, at the level it names when it names one | run as named, as a create's `model` is ([[077-rollouts]], R3): a routed name always resolves to another model, so every firing of a trigger that names one would be refused |
| A firing that continues a session | sends its message and leaves the session's model | the trigger's model applied at each send: a person who moved the conversation to another model would be moved back at the next firing |
| Beside `session.target` | ignored, as `session.attended` is ([[086-proactive-agents]]) | refused at apply: a wake's trigger and any other target trigger continue a session whose model is its own, and a field that is never read changes nothing |
| `effort`, the level's name before [[049-reasoning]] | not taken | read as a session's change reads it: the field is new, so nothing writes the old name |
| The trigger's questions | `trigger.create` and `trigger.update` unchanged | carrying the model: the model is decided where the session starts, at `session.create`, whose allow may route it, and an apply that names a model an installation does not offer is refused at its first firing |

### The field

The row below joins [[022-triggers]]'s table of the Trigger kind's
fields.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `session.model` | `{name, reasoning}` | none, the agent's model at its level | the model each session a firing starts begins on: `name`, a model's name, any the session's change of its model takes; `reasoning`, one of `minimal`, `low`, `medium`, `high`. A member left out is the agent's. Ignored with `session.target` |

```yaml
spec:
  agent: assistant
  schedule: "0 8 * * 1-5"
  timeZone: Europe/Berlin
  session:
    message: "Summarize this morning's headlines."
    model: {name: tier/thorough}
```

A trigger that names no model writes none into its resolved spec, so
its digest stays what it was ([[003-manifest]]).

Apply refuses, as `invalid_manifest` with the field's path, the rules a
session's change of its model is held to: a model that names neither
member (`spec.session.model`), a name with space around it
(`spec.session.model.name`), and a level outside the four
(`spec.session.model.reasoning`). A member the shape does not have,
`effort` among them, is refused by the strict field check at its path.

### What a firing asks and runs

| Firing | `session.create` asks | The session runs |
|---|---|---|
| one that starts a session, of a trigger that names a model | `model`: its `name`, or the agent's when it names a level alone; `reasoning` and `effort`: its level, when it names one | the model the allow names, with the name asked as `via` when they differ; the name asked when the allow names none; at the level the allow names when it names one, `""` being the agent's own, and at the trigger's level, or the agent's, otherwise |
| one that starts a session, of a trigger that names none | `model`: the agent's name, as before | as before |
| one that continues the open session its key names | nothing: it asks `session.send`, which names the model the session stands on, as any send does | the model the session runs, unchanged |
| one of a trigger under `session.target`, a wake's among them | nothing: it asks `session.send` | the target's model, unchanged |

The question names the model as a create that names its model does
([[077-rollouts]], R3), so an authorizer reads the same fields whichever
started the session; `trigger_id`, `firing_id` and `attended` are beside
them as before. The answer is the agent's own model's: the allow's
`model` runs, its `route` and `message_text` kept as a routed create
keeps them ([[038-routed-models]]), and its `reasoning` holds
([[049-reasoning]]). The session's header holds the model it starts on,
and none when that is the agent's own at its own level.

The model that runs is checked after the question, as at any create: a
name no source gives an input window and an output limit is
`model_unknown`, and the firing that asked is `refused` with that code;
a gateway that does not answer is `model_unavailable`, a `failed` firing
that a redelivery runs again ([[022-triggers]]).

A firing under `policy: continue` whose key names no open session, or
one that ended, starts a session, and so starts it on the trigger's
model.

### The trigger tool

The agent's `trigger` tool takes no model. A change, a pause or a resume
it applies keeps the stored trigger's other fields
([[086-proactive-agents]]), the model among them, so a level the person
chose holds when the agent moves the routine's time.

### The OpenAPI document

`ManifestSessionModel {name, reasoning}` is `TriggerSession.model`, and
the apply route's text says what a firing asks with it and runs, and
what a continued firing does.

## What a hosted installation needs

| Need | Why |
|---|---|
| Its authorizer answers a firing's `session.create` that names a routed name as it answers a change of a session's model to it, with the person's own pools and the rules its proactive work is held to | the session starts on what that allow names |
| A client that offers a level per routine writes `spec.session.model` in the manifest it applies | the field is the trigger's; the SDK's routine shape keeps a field it does not write |
| The image of this release pinned | the field and its question are the server's |

An authorizer that sets a routed name's level itself answers its own in
`reasoning`, and the session runs at it whatever level the trigger names.

## Not in this spec

A model for a session a wake or another target trigger continues; a
model per firing, or chosen by the event; the trigger tool's own model
field; `trigger.create` and `trigger.update` naming the model; refusing,
at apply, a level beside a name an installation routes, which Topos does
not know; the words a client shows for each level.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `spec.session.model` resolves as written, a name, a level or both, and a trigger without it writes none and keeps its digest; neither member, a name with space around it, a level outside the four and `effort` are `invalid_manifest` at their paths | `manifest.TestTheModelATriggersSessionsStartOn` | built |
| The apply route refuses the same models as `invalid_manifest` and stores nothing | `internal/server.TestATriggersModelIsCheckedAtApply` | built |
| A firing that starts a session asks `session.create` with the trigger's model and level; the session starts on the allow's model with the trigger's name as `via`, at the trigger's level or the allow's, and the model checked is the one that runs; an unrouted name starts as named, a level alone starts the agent's model at it, and a trigger without a model starts on the agent's, asked as before | `internal/server.TestAFiringStartsItsSessionOnTheTriggersModel` | built |
| A firing that continues its key's session, or its target, leaves the session's model, one a person changed included, asks `session.send` with the model the session stands on and no level, and appends no `session.model_changed`; the key's next session starts on the trigger's model | `internal/server.TestAContinuedFiringLeavesItsSessionsModel` | built |
| The trigger tool's change, pause and resume of a routine, and its change of any other trigger, keep `spec.session.model` | `internal/proposal.TestAChangeKeepsTheModelItsTriggerNames` | built |
| The OpenAPI document describes the field and what a firing does with it | `internal/server.TestOpenAPIIsGenerated`, `internal/server.TestTheDocumentDescribesSpec089` | built |

## Outcome

Built on 2026-10-25, in no release yet. Every criterion has its test,
and `TestAFiringStartsItsSessionOnTheTriggersModel` fails at every case
but the trigger without a model when the actor does not pass the
trigger's model to the create. Nothing diverged from the design.
Applying a trigger under `session.target` through `PUT
/triggers/{name}` answered `internal`, since the route's row did not name
the `session.send` its apply asks; the row names it now, proven by
`internal/server.TestATriggerThatNamesATargetIsAppliedThroughItsRoute`.
