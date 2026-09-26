# Contributing to Topos

Topos welcomes focused bug fixes, documentation improvements, and proposals
that move a spec forward.

## Before you start

Search the existing issues before opening a new one. A change in behavior
starts as a change to a spec in [specs](specs): the deck is the design, and
code follows the spec it builds. Report vulnerabilities through the private
process in [SECURITY.md](SECURITY.md), not a public issue.

## Local development

Topos requires Go 1.27 or later.

```sh
go mod download
make hooks   # formatting before a commit; lint and the changelog rule before a push
make check   # the whole bar, as CI runs it
```

`make check` runs `go tool lateregate`: the suite, the suite under the race
detector, the per-package coverage floor, the spec lint, the dependency
check, the license notice, and the identity rules for an open core. The
gates come from `latere.ai/x/ci-gate`, pinned as a tool in `go.mod`, so a
gate fails the same way on a laptop as on a runner. What each asserts for
this repository is in `.lateregate.yaml`. `go tool lateregate list` prints
the plan, and `go tool lateregate <gate>` runs one.

## Layout

| Path | What it is |
|---|---|
| `cmd/toposd` | the server and its roles: `serve`, `runner`, `check`, `token` |
| `cmd/topos` | the scripting and test client |
| `internal/config` | typed configuration from `TOPOS_*` variables |
| `internal/arch` | the structural invariants of spec 001, as tests |
| `deploy/` | the base manifests, the bootstrap, and examples |
| `specs/` | the design |
| `docs/history/` | the v0.7.0 runtime's specs, as a record |

The exported trees of spec 001 (`session`, `harness`, `models`, `machine`,
`runner`, `memory`, `manifest`, `client`, `authorizer`) are created by the
specs that build them.

## Changes

One logical change per commit, with a message of the form `scope: what
changed`. A change a release reader would notice gets a line under
`Unreleased` in [CHANGELOG.md](CHANGELOG.md).
