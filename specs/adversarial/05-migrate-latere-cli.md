---
title: Migrate latere-cli to the Topos Adversarial Capability
status: proposed
depends_on:
  - specs/adversarial/03-capability-surface.md
affects:
  - latere-cli/go.mod
  - latere-cli/internal/commands/
  - latere-cli/docs/
  - latere-cli/specs/
effort: medium
created: 2026-07-07
updated: 2026-07-07
author: changkun
dispatched_task_id: null
---

# Migrate latere-cli to the Topos Adversarial Capability

## Goal

Repoint latere-cli from `x/agon` to `topos/adversarial`, rename the `latere agon`
command to `latere review`, drop the `agon` module dependency, and scrub every
`agon` identifier and doc reference. The command's behavior (run an adversarial
debate over the working-tree diff of the most recent Claude Code session, critics
routed through Lux) is unchanged.

Planned here for coordination; implemented in the latere-cli repo against the Topos
tag from [03](03-capability-surface.md).

## Scope

**Import repoint** in `internal/commands/agon.go`:

- `latere.ai/x/agon/pkg/adversarial` -> `latere.ai/x/topos/adversarial`
- `latere.ai/x/agon/pkg/adversarial/claude` -> `latere.ai/x/topos/adversarial/claude`
- `latere.ai/x/agon/pkg/adversarial/input` -> `latere.ai/x/topos/adversarial/input`
- `latere.ai/x/agon/pkg/adversarial/topos` (alias `atopos`) ->
  `latere.ai/x/topos/adversarial/critic` (the renamed native critic)

**Command rename.** `latere agon` -> `latere review`. Update `Use`, `Short`,
`Long`, and `Example` strings; drop the word `agon` from all help text.

**Identifier and file scrub.**

| Before                              | After                                  |
| ----------------------------------- | -------------------------------------- |
| `internal/commands/agon.go`         | `internal/commands/review.go`          |
| `internal/commands/agon_test.go`    | `internal/commands/review_test.go`     |
| `agonOpts`                          | `reviewOpts`                           |
| `runAgon`                           | `runReview`                            |
| command constructor (`newAgonCmd`)  | `newReviewCmd`                         |
| `[agon]` stderr prefixes            | `[review]`                             |
| `--state-dir` default `.agon/...`   | `.topos/review/sessions/`              |

**Module.** Remove `latere.ai/x/agon` from `latere-cli/go.mod`; bump
`latere.ai/x/topos` to the [03](03-capability-surface.md) tag; `go mod tidy`.

**Docs and local spec.** Rename `docs/agon.md` -> `docs/review.md` and
`specs/agon-local-subcommand.md` -> `specs/review-local-subcommand.md`; rewrite
their bodies to `latere review` and remove `agon` framing. Update any README link.

## Compatibility decisions

- **Command name `agon` -> `review`.** This breaks muscle memory and any scripts
  invoking `latere agon`. Per the total-scrub decision there is no hidden `agon`
  alias, because an alias would keep the word in the binary. Announce the rename in
  the latere-cli release notes and update all docs in the same release. This is an
  accepted break.
- **On-disk `.agon/sessions/`.** The default `StateDir` moves to
  `.topos/review/sessions/`. A user may still have old `.agon/` output in a repo;
  it is transient and not migrated. Ensure the command does not read a fixed
  `.agon/` path.

## Steps

1. Repoint imports; bump Topos; remove `x/agon`; `go mod tidy`.
2. Rename the command, files, and identifiers; update help text and the state-dir
   default.
3. Rewrite `docs/review.md` and `specs/review-local-subcommand.md`.
4. `go build ./...`, `go test ./...` green, including the command test.
5. `grep -rniw agon latere-cli` returns nothing.

## Acceptance

- `latere review` runs an end-to-end adversarial debate against
  `topos/adversarial` and writes `.topos/review/sessions/<id>/`.
- `grep -rl "latere.ai/x/agon" latere-cli/go.mod` is empty.
- `grep -rniw agon latere-cli` is empty (source, docs, specs, help output).
- The command test passes under the new name.

## Outcome

To be written when this spec is implemented.
