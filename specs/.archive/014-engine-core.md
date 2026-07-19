---
title: Port the Adversarial Engine Core into Topos
status: proposed
depends_on:
  - specs/.archive/013-overview.md
affects:
  - adversarial/adversarial.go
  - adversarial/engine.go
  - adversarial/assemble.go
  - adversarial/internal/
effort: medium
created: 2026-07-07
updated: 2026-07-07
author: changkun
dispatched_task_id: null
---

# Port the Adversarial Engine Core into Topos

## Goal

Move the backend-agnostic core of the adversarial debate engine from
`latere.ai/x/agon/pkg/adversarial` into `latere.ai/x/topos/adversarial`, with no
behavior change. After this step Topos compiles and tests the engine, protocol,
interfaces, and result types on its own, and the ported package imports nothing
from `x/agon`. Backends and input come in the next step; this one is the core only.

## Scope

Move, verbatim except for import paths, these files into `topos/adversarial/`:

- `adversarial.go` (the protocol and interfaces: `Proposer`, `Critic`,
  `CriticFactory`, `Verifier`, and the result types `ProposerResult`,
  `CriticInput`, `CriticResult`, `RoundFileRef`, `TokenUsage`, `Summary`,
  `ForkOutcome`, `VerifyInput`, `VerifyResult`, plus `AssemblePrompt`).
- `engine.go` (the `Engine` struct and `Run`).
- `assemble.go`.

Move the engine's internal dependencies into `topos/adversarial/internal/`:

- `agent/`, `critic/`, `ledger/`, `round/`, `state/`, `summary/`, `ansi/`.

The first six are the internals `engine.go` imports directly
(`latere.ai/x/agon/internal/{agent,critic,ledger,round,state,summary}`); `ansi` is
a transitive engine dependency, imported by `internal/round/loop.go` and
`internal/summary/print.go` for the escape codes the round loop and summary render.
It is part of the engine, not CLI-side, so it moves with the core; without it
`topos/adversarial` will not compile. Under `adversarial/internal/` Go visibility
restricts all of these to importers within `adversarial/`, preserving today's
privacy.

Rewrite every moved import from `latere.ai/x/agon/internal/...` to
`latere.ai/x/topos/adversarial/internal/...`, and the package self-reference from
`latere.ai/x/agon/pkg/adversarial` to `latere.ai/x/topos/adversarial`.

Port the engine tests alongside (`adversarial_test.go`: `TestAssemblePrompt`,
`TestEngineSteadyState`, `TestVerifierInterface`, `TestEngineRun_WritesEndJSON`).

## Non-goals

- No backends. `claude/`, `critic/` (native), and `input/` move in
  [02](015-backends-and-input.md).
- No `internal/web`. That is the `agon-web` site and is retired in
  [07](020-retire-agon.md), not moved. (Note: `internal/ansi` is not excluded; it is
  a transitive engine dependency and moves in Scope above.)
- No API changes, renames, or signature changes. The package name stays
  `adversarial`; exported identifiers are unchanged. The on-disk `.agon/sessions/`
  path stays as-is at this step (the engine still writes it); the directory rename
  happens in the consumer specs that own the write path.
- No consumer changes. wallfacer and latere-cli still import `x/agon` after this
  step; they repoint in [04](017-migrate-wallfacer.md) and
  [05](018-migrate-latere-cli.md) after the tag in [03](016-capability-surface.md).

## Steps

1. Create `topos/adversarial/` and `topos/adversarial/internal/`.
2. Copy the core files and the six internal packages in; rewrite import paths.
3. Port `adversarial_test.go`.
4. `go build ./...` and `go test ./adversarial/...` in the Topos module.
5. Confirm no residual `x/agon` reference: `grep -rn "x/agon" adversarial/`
   returns nothing.

## Acceptance

- `topos/adversarial` builds as part of `go build ./...`.
- The ported engine tests pass under `go test ./adversarial/...`.
- `grep -rn "latere.ai/x/agon" adversarial/` is empty.
- Coverage on the ported package is at or above what it was in `agon` (the engine
  tests move with it, so no regression).

## Risks and decisions

- **Test fixtures.** `TestEngineRun_WritesEndJSON` and the steady-state test may
  rely on fake proposer/critic implementations that live in the test file or a
  testdata dir; move those with the tests. If any test reaches into a backend, it
  belongs in [02](015-backends-and-input.md); note and defer it rather than pulling
  a backend in early.
- **Module graph.** Topos gains no new external dependency from the core (it is
  backend-agnostic). Verify `go mod tidy` adds nothing surprising; if it does, a
  backend leaked in and should move to 02.

## Outcome

To be written when this spec is implemented.
