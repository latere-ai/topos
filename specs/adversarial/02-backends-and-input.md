---
title: Port the Backends and Input into Topos
status: proposed
depends_on:
  - specs/adversarial/01-engine-core.md
affects:
  - adversarial/claude/
  - adversarial/critic/
  - adversarial/input/
effort: medium
created: 2026-07-07
updated: 2026-07-07
author: changkun
dispatched_task_id: null
---

# Port the Backends and Input into Topos

## Goal

Complete the code move by bringing the three remaining pieces into Topos: the
Claude-CLI backend, the Topos-native critic, and the input package. After this
step the entire `agon` engine surface exists under `topos/adversarial/`, and the
only thing keeping `agon` alive is that consumers still import it (fixed in
[04](04-migrate-wallfacer.md) and [05](05-migrate-latere-cli.md)).

## Scope

Move three packages into `topos/adversarial/`:

- **`claude/`** from `pkg/adversarial/claude`. The Claude-CLI proposer and critic
  that fork a real Claude Code session (`NewProposer`, `NewCritic`, and the
  `ProposerOption`/`CriticOption` builders). Both wallfacer and latere-cli use
  this, so it moves into Topos rather than into either consumer. It stays under
  `adversarial/`, deliberately outside the core runtime packages, because it is
  Claude-Code-specific subprocess glue and must not pollute the provider-agnostic
  core.
- **`critic/`** from `pkg/adversarial/topos` (the native critic, `NewCriticFactory`
  over topos and Lux). Renamed on move: the source path is
  `pkg/adversarial/topos`, which inside the Topos module would read
  `topos/adversarial/topos`. Rename to `adversarial/critic`. Because it now lives
  inside Topos, it imports the sibling `topos` packages directly instead of
  reaching across a module edge.
- **`input/`** from `pkg/adversarial/input`. `Compute`/`Trivial` (working-tree
  diff) and `LocateTranscript`/`FindSession`/`ReadTranscript`/`ExtractFirstUser`/
  `EncodeCwd`/`DecodeCwd` (Claude Code transcript locator). Only latere-cli uses
  this today. The diff half is generic; the transcript half is Claude-Code-specific
  but small, so both stay together under `adversarial/input` for now.

Rewrite all import paths from `latere.ai/x/agon/...` to
`latere.ai/x/topos/adversarial/...`. Port each package's tests alongside it (the
`input` package has substantial transcript and diff tests; they move as-is).

## Non-goals

- No refactor of `input` into generic-diff and Claude-transcript halves. That is a
  tempting cleanup but out of scope for a migration; keep the package intact and
  revisit later if a non-Claude consumer ever needs the diff alone.
- No new backend abstractions or capability entrypoint. The thin `Review` surface
  is [03](03-capability-surface.md).
- No `agon` renames in symbols yet beyond the forced `topos` -> `critic` package
  rename. The word `agon` does not appear in these packages' identifiers today
  (they are named `claude`, `topos`, `input`); the on-disk `.agon/` path is owned
  by consumers and renamed there.

## Steps

1. Move `claude/`, `input/`, and `pkg/adversarial/topos` -> `adversarial/critic/`.
2. Rewrite import paths; update the native critic to import sibling `topos`
   packages directly.
3. Port all three packages' tests.
4. `go build ./...` and `go test ./adversarial/...` green.
5. `grep -rn "latere.ai/x/agon" adversarial/` returns nothing.

## Acceptance

- All of `adversarial/{claude,critic,input}` builds and tests green in Topos.
- The native critic wires to the in-module `topos` runtime with no `x/agon` and no
  cross-module hop.
- `grep -rn "latere.ai/x/agon" adversarial/` is empty across the whole
  `adversarial/` tree (core plus backends).
- `go mod tidy` leaves the Topos module graph consistent; any dependency the
  backends pull in (the Claude CLI has none beyond stdlib; the native critic uses
  already-present Topos deps) is accounted for.

## Risks and decisions

- **Native critic and module deps.** `pkg/adversarial/topos` currently imports
  `latere.ai/x/topos` across the module edge. In-module this becomes an internal
  import; confirm no import cycle arises (the critic uses the runtime, not the
  reverse, so none should).
- **Lux routing.** The native critic routes models through Lux. Moving it does not
  change routing; verify the `Config` surface (`NewCriticFactory(cfg Config)`)
  still resolves model options the same way from inside Topos.

## Outcome

To be written when this spec is implemented.
