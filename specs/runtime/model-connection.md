---
title: Model Connection
status: complete
depends_on:
  - specs/runtime/agentic-loop.md
affects:
  - model.go
  - models/
  - models/anthropic/
  - models/fake/
effort: medium
created: 2026-06-28
updated: 2026-06-28
author: changkun
dispatched_task_id: null
---

# Model Connection

## Goal

Let a host choose how the runtime reaches a model without exposing internal model
types, and keep provider secrets out of the host application. The same agents
should run against a real provider, a gateway, or a deterministic test model with
only a config change.

## Design

`ModelOptions` is the public model connection. It names a `Kind`, an optional
`Provider`, `Model` id, and `BaseURL`, plus exactly one credential: a static
`APIKey` or a `BearerSource` callback that supplies a per-call token. The host
never touches the internal model interface; the runner builds the right adapter
from these options once, when the runner is created.

`ModelKind` selects the backing:

- `ModelFake`: a deterministic, network-free model. It needs no keys and no
  network, so a host can exercise a full autonomous run (the loop calls a tool,
  the sandbox runs it, the model stops) in tests and quick checks.
- `ModelLux`: reaches a provider through Lux, the model gateway. Provider secrets
  live in the gateway, not in the embedding application; the host presents only a
  gateway virtual key or a rotating token. Local development can point at a local
  stateless gateway with the developer's own keys, so there is no cloud dependency
  for dev.
- `ModelDirect`: talks to a provider endpoint directly, a convenience for local
  development with a self-supplied provider key.

The model itself is always the same internal seam (see the agentic loop spec), so
the loop is unaware of which backing it got. The supported provider wire today is
Anthropic-style: `ModelLux` and `ModelDirect` build the Anthropic-wire adapter,
and a non-Anthropic provider value is rejected with a clear error. Adapter code for
other providers exists in `models/` but is not yet selectable through this surface.

## Diagram

```mermaid
graph TD
  opts[ModelOptions: Kind, creds, BaseURL] --> build{buildModel}
  build -->|ModelFake| fake[deterministic fake model]
  build -->|ModelLux| lux[Anthropic-wire adapter via Lux gateway]
  build -->|ModelDirect| direct[Anthropic-wire adapter, direct endpoint]
  fake --> seam[internal Model seam]
  lux --> seam
  direct --> seam
  seam --> loop[agentic loop]
```

## Outcome

Shipped in `model.go`: `ModelOptions`, `ModelKind` (`ModelFake`, `ModelLux`,
`ModelDirect`), and `buildModel`, which builds the fake model from `models/fake`
or the Anthropic-wire adapter from `models/anthropic`, and rejects unsupported
providers. The provider-agnostic seam is `models.Model` in `models/`.
