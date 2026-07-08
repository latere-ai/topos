---
title: Engine API
status: current
updated: 2026-07-08
author: changkun
---

# Engine API

The public embedder contract is the `adversarial` package (`adversarial.go`,
`engine.go`, `assemble.go`). Its types carry no `internal/` dependency, so any
module that imports Topos can implement the interfaces and drive a debate. An
embedder supplies a `Proposer` and a `CriticFactory`, sets them on an `Engine`,
calls `Run`, and reads the `Summary`.

## Canonical embedding pattern

```go
import "latere.ai/x/topos/adversarial"

sum, err := (&adversarial.Engine{
    StateDir:    stateDir,   // parent of sessions/<id>/
    Cwd:         cwd,        // working directory for agent calls
    ForkCount:   forks,      // independent critics
    Proposer:    proposer,   // e.g. claude.NewProposer(sessionID, cwd)
    NewCritic:   critics,    // e.g. critic.NewCriticFactory(cfg)
    MaxRounds:   maxRounds,  // per-fork round cap (2 x turns)
    CostCap:     costCap,    // soft token budget across forks
    TaskContext: taskCtx,    // the task description
    DiffPatch:   diff,       // unified diff under review
}).Run(ctx)
```

Ready-made `Proposer`/`Critic` implementations are in [Backends](backends.md);
transcript and diff helpers are in [Inputs](inputs.md).

## The interfaces you implement

- **`Proposer`** - drives the implementation agent across forks.
  `FirstRound(ctx, pointer)` creates the fork and returns its `ForkID`;
  `NextRound(ctx, forkID, pointer)` continues it. `pointer` is the mediator
  message directing the agent to review the critic's comments.
- **`Critic`** - `Round(ctx, CriticInput) (*CriticResult, error)`. Stateless
  across calls; the engine calls it once per critic turn.
- **`CriticFactory`** = `func(forkIdx int) Critic` - creates one `Critic` per
  fork (1-based index), so backends can key sandboxes or lineage on the fork.

## The result and input types

- **`CriticInput`** - `AspectName`, `SystemPrompt` (the already-assembled aspect +
  round contract), `CriticIndex`, `Round`, `TaskContext`, `DiffPatch`,
  `PriorRoundFiles []RoundFileRef` (non-empty from round 3, pointing the critic at
  its prior attacks and the proposer's defense), `Cwd`, `Deadline`, `Model`.
- **`CriticResult`** - `Markdown` (the raw attack document the engine parses),
  `Tokens`, `Usage TokenUsage`, `USD`, `Duration`.
- **`ProposerResult`** - `ForkID`, `Response`, `Usage`, `USD`, `Duration`.
- **`Summary`** - `Termination` (one of `steady-state`, `cost-cap`, `max-turn`,
  `interrupted`, `malformed-output`), `Forks []ForkOutcome`, `Unresolved`,
  `Headline`, `SessionDir`, `USD`, `WallSeconds`.
- **`TokenUsage`** - `Input`, `Output`, `CacheCreate`, `CacheRead`, with `Total()`.

`AssemblePrompt(in CriticInput) string` combines `SystemPrompt`, `TaskContext`,
`DiffPatch`, and `PriorRoundFiles` into the single string a backend feeds its
model. Backends call it so every critic sees the same assembled prompt regardless
of runtime.

`Engine.Run` creates the session directory, runs the forks, and persists the
terminal artifacts (`summary.md`, `end.json`) under `StateDir/sessions/<id>/`
before returning; see [Session format](session-format.md). The library path
is silent by default. An embedder that wants progress lines wraps its
`Proposer`/`Critic`.

## The Verifier seam

`Verifier` is a higher-level, one-call integration point for tools that want
adversarial verification as a plugin step without assembling an `Engine`:

```go
Verify(ctx, VerifyInput) (*VerifyResult, error)
```

`VerifyInput` carries the task, criteria, session ID, diff, cwd, state dir, fork
count, round cap, and cost cap; `VerifyResult` returns `Unresolved`, `Headline`,
`SessionDir`, and `USD`. A `(nil, nil)` return signals a skip (verification
disabled, or the diff too trivial to debate). The interface is the seam a hosted
verifier or a plugin host builds on; the repo ships the interface, not a concrete
implementation.

## Stability

The `adversarial` package is pre-1.0 and semver-exempt: the surface can change
while the protocol and embedders stabilize. Stabilizing it is a roadmap item. The
protocol in [Debate protocol](protocol.md) is more stable than the Go surface,
since it is what crosses the model boundary.
