---
title: "Release, installation and running on your own: images, archives, attestations, the compose file, toposd check"
status: drafted
track: core
depends_on: [002-scaffold-and-configuration.md, 006-identity.md, 025-task-suite.md, 026-stubs-and-tiers.md, 029-conformance.md]
affects: [Dockerfile, deploy/, examples/, docs/install.md, internal/check/, .github/workflows/, CHANGELOG.md]
effort: medium
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Release, installation and running on your own

## Overview

What a tag publishes and how someone runs it with nothing but the
release: two images, archives of the three binaries, their
attestations, a compose file that starts `toposd` on the directory
store with the local issuer and the owner policy, `toposd check`, an
installation document CI walks, and example agents. A release is cut
only when the task suite's bar and the conformance suite pass on the
exact commit. Nothing in a released artifact names a Latere host or a
particular installation.

## Current state

v0.7.0 was a library with no binary or image. Its tags v0.0.2 to v0.7.0
are held by the checksum database and are never tagged again. Nothing
of its release process is borrowed; the pipeline is the family's
shared one, as Lux and Cella release.

## Design

### Versions

Every new tag sorts above v0.7.0. The rebuild ships as pre-releases
`v0.8.0-rc.N`, which do not move `@latest`; v0.8.0 is cut when phase 1
closes, and its release notes carry the task suite's pass rate
([[025-task-suite]]). A tag needs a `CHANGELOG.md` section, and
`go tool lateregate release vX.Y.Z` cuts it.

### Artifacts

| Artifact | Holds |
|---|---|
| image `topos` | `toposd` on a distroless base, non-root, both ports exposed, a volume at `/var/lib/topos`; for installations whose sessions run on Cella machines, since it carries no shell |
| image `topos-host` | `toposd`, `git`, a POSIX shell and the host sandbox (`srt` with Node, and Bubblewrap, `socat` and `ripgrep`) on a slim base, for a server whose sessions run on its host machine with `TOPOS_HOST_SESSIONS=on` ([[009-machines]]); its container needs unprivileged user namespaces, which the role's start-up probe checks |
| archives | `toposd`, `topos` and `topos-machine` for linux and darwin on amd64 and arm64, and windows on amd64 for `topos` |
| attestations | SLSA provenance and an SBOM for every image and archive, and checksums signed by the pipeline |

Images publish under the namespace of the repository that pushed the
tag, so a fork's release lands in the fork's registry namespace and
never in the upstream's.

### The release job

The pipeline builds the artifacts once, then runs, against the exact
commit and the built image: the gate, the e2e, postgres and cella tiers
([[026-stubs-and-tiers]]), the conformance suite against the released
image ([[029-conformance]]), the release bar ([[025-task-suite]]), and
a walk of `docs/install.md`. Any failure stops the release before
anything is published.

### Running on your own

`deploy/compose.yaml` starts `toposd serve` from the `topos-host`
image on the directory store in a volume, with a generated local issuer
key, the owner policy with one admin subject, `TOPOS_HOST_SESSIONS=on`,
and `TOPOS_MODELS_URL` pointing at a model gateway or provider the
operator names; a profile
adds Postgres. `docs/install.md` walks it: start the compose file, mint
a token with `toposd token` ([[006-identity]]), apply an example agent
with `topos apply`, run a session with `topos run`, attach to it.
CI executes the document's commands in order.

### toposd check

`toposd check` reads the whole configuration and prints one line per
requirement, `ok` or `fail` with the reason, and exits 1 on any
failure: the configuration loads; the store opens and its migrations
are current; each issuer's discovery answers, or the local issuer key
reads; the authorizer answers a probe, or the owner policy is in force
with at least one admin; the model connection answers; Cella, the git
host and the memory backend answer when configured; credentials that
still wait for a rewrap after a key rotation are counted.

### Examples

`examples/` holds agent manifests (a code reviewer, a scheduled report
with its Trigger, an agent with a memory store) and embedding programs
restricted to the supported import set ([[024-client-cli-skill]]). The
manifests name no host outside the example domains.

## Not in this spec

The tiers ([[026-stubs-and-tiers]]); the suites the job runs
([[025-task-suite]], [[029-conformance]]); a hosted installation's
deployment, which lives in the operator's overlay.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The installation document is walked in CI and a session runs to `end_turn` against the stub Lux | the `install` job, `TestInstallDocumentWalks` | not built |
| `topos` builds for the `windows/amd64` archive, and the module passes `go vet` for it | `machine/host.TestTheModuleBuildsForWindows`, which cross-compiles from the Unix runner; no Windows runner is available, so no Windows binary runs in CI | built |
| A release built from a fork publishes under the fork's namespace | `TestReleasePublishesUnderTheOwnersNamespace` | not built |
| `toposd check` prints one line per requirement and exits 1 when any fails | `TestCheckReportsEveryRequirement` | not built |
| The release job refuses to publish when the bar or the conformance suite fails | `TestReleaseStopsOnFailedSuite` over the workflow | not built |
| No released artifact names a Latere host or an installation | `TestNoLatereCoordinatesInReleasedArchives` over the built archives and image layers | not built |
| Every image and archive has provenance and an SBOM | `TestArtifactsAreAttested` | not built |
