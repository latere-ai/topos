---
title: Expose Adversarial Review as a Topos Capability
status: proposed
depends_on:
  - specs/adversarial/02-backends-and-input.md
affects:
  - adversarial/review.go
  - adversarial/doc.go
  - specs/README.md
  - README.md
effort: small
created: 2026-07-07
updated: 2026-07-07
author: changkun
dispatched_task_id: null
---

# Expose Adversarial Review as a Topos Capability

## Goal

Give Topos a small, named entrypoint for adversarial review so it reads as a
first-class capability rather than a loose subpackage, document it, and cut the
Topos release tag that carries the whole capability. This tag is the gate that
unblocks the three consumer migrations ([04](04-migrate-wallfacer.md),
[05](05-migrate-latere-cli.md), [06](06-agents-capability-page.md)).

## Design

The `Engine` struct is the full-control surface and stays. On top of it, add one
thin convenience that expresses the common case: review a working-tree diff with a
proposer and a critic factory over N forks, and get a `Summary`. Keep it a wrapper,
not a new engine.

```go
// Review runs an adversarial debate over opts.DiffPatch and returns the Summary.
// It is a thin convenience over Engine for the common single-call case; callers
// needing per-fork control use Engine directly.
func Review(ctx context.Context, opts ReviewOptions) (*Summary, error)

type ReviewOptions struct {
    StateDir    string // required; caller-chosen root, engine writes sessions/<id>/ under it
    Cwd         string
    Forks       int
    Proposer    Proposer
    NewCritic   CriticFactory
    MaxRounds   int
    CostCap     int
    TaskContext string
    DiffPatch   string
}
```

`Review` constructs an `Engine` from `ReviewOptions` and calls `Run`. That is the
entire body; it exists so a host, and the agents capability page behind it, can
name the capability in one call. Add a package `doc.go` that describes adversarial
review in Topos terms (proposer plus critics debating a diff, per-fork lineage) and
points at both `Review` and `Engine`.

The engine stays brand-neutral about where it writes. `Review` and `Engine`
write `sessions/<id>/` under the caller-provided `StateDir` and invent no default
of their own. topos is embeddable by any host (not only Latere), so it must not
bake in a Latere path like `~/.latere/` or a `.topos/` working-tree directory. The
`StateDir` field is required; if a caller leaves it empty, the engine errors rather
than guessing a location. Choosing a good default is a consumer decision:
latere-cli defaults to an XDG state dir under the user's home
([05](05-migrate-latere-cli.md)), and wallfacer uses a stable server-side data dir
outside the ephemeral worktree ([04](04-migrate-wallfacer.md)). This also retires
the old `.agon/sessions/` working-tree location entirely; nothing writes into the
reviewed repo anymore.

## Documentation

- Add an "Adversarial Review" section to the Topos `README.md` and the root
  package doc, framed as a capability of the runtime.
- Flip the [overview](00-overview.md) and this track's entry in
  `specs/README.md` context as needed once the capability is real.
- The capability keeps no `agon` string in any doc, symbol, or example.

## Release

After 01, 02, and this surface land and tests are green, cut a Topos tag (a minor
version bump, e.g. `v0.1.0` or the next minor over the current line). This is the
first Topos release that ships adversarial review. Record the tag in the Outcome so
the consumer specs pin an exact version.

## Acceptance

- `adversarial.Review` exists, is covered by a test that runs a fake proposer and
  critic end to end, and returns a `Summary` equivalent to the `Engine` path.
- The engine writes `sessions/<id>/` under the caller's `StateDir` and invents no
  path of its own; a test asserts the layout under a temp `StateDir` and that an
  empty `StateDir` is a caller error.
- Topos README and package doc describe the capability with no `agon` reference.
- A Topos tag is cut and named in the Outcome.

## Non-goals

- No region-preset rewrite. `Review` wraps `Engine`; it does not re-express the
  debate as a Topos `Region`/`Graph`. That remains explicitly out of scope for
  this program (see [overview](00-overview.md)).
- No consumer changes; those follow the tag.

## Outcome

To be written when this spec is implemented, including the exact Topos tag the
consumer specs pin.
