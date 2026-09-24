# Contributing to Topos

Topos welcomes focused bug fixes, documentation improvements, and proposals that
strengthen its Go package surface.

## Before you start

Search the existing issues before opening a new one. For a substantial API or
behavior change, open a feature request first so the design and compatibility
cost can be discussed before implementation. Report vulnerabilities through the
private process in [SECURITY.md](SECURITY.md), not a public issue.

## Local development

Topos requires Go 1.27 or later. Clone the repository, download the module
dependencies, install the hooks, and run the quality gate:

```sh
go mod download
make hooks   # formatting and modernization before a commit; lint and the changelog rule before a push
make all
make check
```

`make all` is the quick local bar: formatting, golangci-lint, `go vet` and the
suite, the per-package coverage floor of 90%, the spec lint, and the
vulnerability scan. `make check` runs `go tool lateregate`, the whole bar CI
runs on every push and pull request. Beyond `make all` it runs the suite under
the race detector, the suite with only the toolchain and the directories
`.lateregate.yaml` names on `PATH`, a check that the suite leaves nothing in
the temporary directory, the license notice, the modernization check, and the
repository checks the shared bar defines. `go tool lateregate list` prints the
plan with any waived gate and its expiry, and `go tool lateregate <gate>` runs
one.

The gates come from `latere.ai/x/ci-gate`, pinned as a tool in `go.mod`, so a
gate fails the same way on a laptop as on a runner. What each asserts for this
repository is in `.lateregate.yaml`; a waiver there carries a reason and an end
date.

The suite runs offline and needs no configuration: tests drive the fake model
and the local sandbox, so nothing reaches a network or a hosted service. A few
tests skip only where the machine cannot express what they check: running as
root, a non-POSIX environment, or a file system without symbolic links.

## Where the code is

| Path | What it holds |
|---|---|
| `topos.go`, `model.go`, `directory.go` | the root package: `Runner`, regions, graphs, turns, the trace, the observer bridge, and the model construction |
| `graph/` | the authored JSON form of a graph and its lowering to `topos.Graph` |
| `billing/` | the cost sources, the rate card, the meter, and the budget enforcer |
| `runtime/loop/` | the agentic loop: prompt, stream, run tool calls, repeat, and the budget check at each turn boundary |
| `harness/` | the spawner that derives a child's authority by intersection with its parent's |
| `harness/tools/` | the tool registry, the builtins, and the grant families |
| `harness/hooks/` | the hook bus every event passes through, and the tool call path of validation, permission, and execution |
| `models/`, `models/lux/`, `models/fake/` | the model interface, the adapter over the Lux dialect, and the deterministic model |
| `sandbox/` | the `Provider` interface, `Confine`, and `Consent` |
| `sandbox/local/`, `sandbox/cella/`, `sandbox/rpc/` | the three providers |
| `adversarial/` | adversarial review: the engine, the debate rounds, the ledger, and the backends |
| `examples/` | runnable programs; exempt from the coverage floor |
| `specs/` | the design record of each capability; `specs/README.md` gives the reading order |

The root package imports `sandbox/local` as its default and no other sandbox
backend: a hosted backend reaches the runtime only as a `sandbox.Provider` the
host passes in. Two import boundaries are held by tests: the `sandbox` package
does not import `sandbox/cella`, and within `adversarial/` only
`adversarial/critic` imports the runtime.

## Pull requests

- Keep each change focused and explain the user-visible result.
- Add a regression test for every bug fix. The test must fail without the fix.
- Add or update runnable examples for new public behavior.
- Update package comments, the README, and the guides in `docs/` when the
  public API changes.
- Add a line under `Unreleased` in [CHANGELOG.md](CHANGELOG.md) for a change a
  user of the module would notice, and mark a breaking one.
- Preserve backward compatibility when practical. Call out any breaking change.
- Use `gofmt` and keep commits small enough to review independently.

A substantial capability starts as a spec in `specs/` with its acceptance
criteria, and the spec records the outcome once the change ships.

## Releases

Every tag has a section in `CHANGELOG.md`, and that section becomes the body
of the GitHub release. `go tool lateregate release vX.Y.Z` turns the
`Unreleased` section into the tag's section, commits, tags, and pushes. The
pre-push hook and the release workflow both refuse a tag without a section.

## Writing

Every sentence Topos emits or carries is written for one reader, and the
register follows the reader:

- User, a person or a coding harness: the documentation, the text an
  application shows from a typed error. Short and plain: what happened and
  what to do next, naming a command or a page, never a package, a function,
  a table, or a Kubernetes object.
- Contributor, someone changing Topos: specs, this file, package
  documentation, commit messages, source comments. Precise, in the project's
  own terms, with the reason a design is what it is.
- Developer, someone debugging a running system: the typed errors' `Error()`
  text, observer events, logs. Exact and complete: object, operation,
  observed value, expected value, and the underlying error.

An error has one code, one fixed user sentence in `message`, and one
developer detail in a separate field shown only on request. The canonical
statement, worked examples, and the review checklist are in the registers
document in pkg:
https://github.com/latere-ai/pkg/blob/main/docs/writing/registers.md
The rule applies to new text and to reviews; existing text is fixed as it is
touched.

By contributing, you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
