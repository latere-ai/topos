---
title: Migrate wallfacer to the Topos Adversarial Capability
status: proposed
depends_on:
  - specs/.archive/016-capability-surface.md
affects:
  - wallfacer/go.mod
  - wallfacer/internal/adversarial/
  - wallfacer/internal/handler/
effort: medium
created: 2026-07-07
updated: 2026-07-07
author: changkun
dispatched_task_id: null
---

# Migrate wallfacer to the Topos Adversarial Capability

## Goal

Repoint wallfacer from `x/agon` to `topos/adversarial`, drop the `agon` module
dependency, and scrub every `agon` identifier from wallfacer per the total-scrub
decision. wallfacer's behavior (post-run adversarial verification, gated at
runtime) is unchanged; only the source of the engine and the names change.

This spec touches wallfacer, which lives outside the Topos module. It is planned
here because Topos is the program's coordination point; implementation happens in
the wallfacer repo against the Topos tag from [03](016-capability-surface.md).

## Scope

**Import repoint.** Change these to the Topos paths:

- `latere.ai/x/agon/pkg/adversarial` -> `latere.ai/x/topos/adversarial` in
  `internal/handler/handler.go`, `internal/handler/tasks_autoimplement.go`,
  `internal/adversarial/{agon.go,session_proposer.go,noop.go,harness_critic.go}`.
- `latere.ai/x/agon/pkg/adversarial/claude` -> `latere.ai/x/topos/adversarial/claude`
  in `internal/adversarial/session_proposer.go`.

**Module.** In `wallfacer/go.mod`, remove `latere.ai/x/agon` and bump
`latere.ai/x/topos` to the tag from [03](016-capability-surface.md). Run
`go mod tidy`.

**Identifier scrub.** Rename, following wallfacer's existing style:

| Before                     | After                        |
| -------------------------- | ---------------------------- |
| `AgonEnabled` / `SetAgon`  | `ReviewEnabled` / `SetReview` |
| `NewAgonVerifier`          | `NewReviewVerifier`          |
| `agonEnabled` field        | `reviewEnabled`              |
| `agonInFlight` / `agonMu`  | `reviewInFlight` / `reviewMu` |
| `maxConcurrentAgon`        | `maxConcurrentReview`        |
| `agonCriticHarnessIDs`     | `reviewCriticHarnessIDs`     |
| `agonStateDir` / `newestAgonSession` | `reviewStateDir` / `newestReviewSession` |
| `agonTuning`               | `reviewTuning`               |
| config key `"agon"`        | `"review"`                   |
| breaker `"auto-agon"`      | `"auto-review"`              |
| file `internal/adversarial/agon.go` | `internal/adversarial/review.go` |
| file `internal/handler/agon_transcript.go` | `internal/handler/review_transcript.go` |

The package `internal/adversarial` keeps its name (it contains no `agon` string).
Sweep comments and log prefixes (`[agon]`) too.

**On-disk path.** wallfacer today derives the engine's `StateDir` from the task
worktree (`agonStateDir(primaryWorktree(task.WorktreePaths))`, and the per-critic
`.agon-critic-<id>` dir in `worktree.go`). Because [03](016-capability-surface.md)
makes the engine brand-neutral with no default of its own, wallfacer now passes an
explicit `StateDir` rooted at a **stable server-side data directory it owns**, not
in the ephemeral worktree and not in a human `$HOME` (`~/.latere` is a
developer-machine notion; the daemon runs as a service user). Rooting review output
outside the worktree also means artifacts survive worktree teardown, which the
daemon needs since it serves them to the frontend. Rename the per-critic dir off
the `agon` name (`.agon-critic-<id>` -> `.review-critic-<id>`) and place it under
the daemon's data dir rather than beside the source worktree. See the compatibility
note below.

## Compatibility decisions

- **Config key `"agon"` -> `"review"`.** This is a wire contract between the
  wallfacer daemon and its frontend/clients. The frontend lives in the same repo
  and updates in lockstep, so the default is a clean break with a coordinated
  frontend change in the same PR. If any persisted config or external client sends
  `"agon"`, add a temporary read-side alias (accept both keys, write only
  `"review"`) and note a removal date; otherwise break outright. Decide from the
  actual persistence: if config is ephemeral per-process, break; if persisted,
  alias.
- **On-disk `.agon/` and `.agon-critic-*` directories.** A running deployment may
  hold `.agon/sessions/` and `.agon-critic-<id>` dirs beside worktrees from prior
  runs. New runs write under the daemon's stable data dir with the `review` naming.
  Do not migrate old artifacts; they are transient review outputs. Confirm nothing
  in wallfacer reads a fixed `.agon`-prefixed path at startup, and that
  `newestAgonSession`/`agonStateDir` (renamed to `review`) resolve against the new
  data dir, not the worktree.

## Steps

1. Repoint imports; bump Topos; remove `x/agon`; `go mod tidy`.
2. Apply the identifier scrub across `internal/adversarial` and `internal/handler`.
3. Update the frontend to send `"review"`/`"auto-review"`.
4. `go build ./...`, `go test ./...`, and the wallfacer e2e/verification tests
   green.
5. `grep -rniw agon wallfacer` returns nothing (source, config, frontend, docs).

## Acceptance

- `grep -rl "latere.ai/x/agon" wallfacer/go.mod` is empty.
- `grep -rniw agon wallfacer` is empty.
- wallfacer builds and its adversarial-verification tests pass against
  `topos/adversarial`.
- The runtime gate (formerly `AgonEnabled`) still toggles verification, now under
  the `review` name and config key.

## Outcome

To be written when this spec is implemented.
