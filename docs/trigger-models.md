# The model a trigger's runs start on

A trigger can name the model, and the reasoning level, that every
session its firings start begins on, in place of its agent's. A client
that lets a person choose how much effort each routine takes writes that
choice here, so a task that needs more than the agent's default gets it
from its first turn. This page is the contract a client follows. Everything
here is on the wire of `/v1`.

## Naming it

`spec.session.model` takes the shape a change of a session's model
takes, `{name, reasoning}`, with either member or both:

```yaml
apiVersion: topos.latere.ai/v1
kind: Trigger
metadata: {name: morning-headlines}
spec:
  agent: assistant
  schedule: "0 8 * * 1-5"
  timeZone: Europe/Berlin
  session:
    message: "Summarize this morning's headlines."
    model: {name: tier/thorough}
```

`name` is any name a session's model may be changed to: a model the
installation runs, or a name its authorizer resolves to one. `reasoning`
is one of `minimal`, `low`, `medium` and `high`. A member left out is the
agent's, so `{reasoning: high}` runs the agent's model at `high`. A
trigger without `model` runs its agent's, as before.

An apply is refused `invalid_manifest`, naming the field, when `model`
names neither member, when `name` has space around it, or when
`reasoning` is another level.

## What each firing does

| The firing | Its session's model |
|---|---|
| starts a session | the trigger's `model`, as the authorizer answers it (below) |
| continues the open session its key names, under `policy: continue` | the one that session runs, unchanged, a model the person moved the conversation to included |
| continues the session `session.target` names, a wake's among them | that session's own; `model` is ignored beside `target` |

A firing under `policy: continue` whose key has no open session starts
one, on the trigger's model.

## What the authorizer is asked

A firing that starts a session asks `session.create` as any firing
does, with `model` the trigger's name, or the agent's when the trigger
names a level alone, and the level under both `reasoning` and `effort`
when it names one. The answer is read as for the agent's own model: an
allow that names another model starts the session on that one, and the
Session's `model` says so:

```json
"model": {"name": "anthropic/claude-sonnet-4-5", "via": "tier/thorough", "reasoning": "medium"}
```

`via` is the name the trigger asked for. An allow that names a level
runs at it, whatever level the trigger named. A model the installation
cannot run refuses the firing, recorded `refused` with the reason
`model_unknown`.
