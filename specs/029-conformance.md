---
title: "Conformance suite: the API contract as an importable test package against any toposd"
status: drafted
track: core
depends_on: [004-session-log.md, 006-identity.md, 015-api.md, 026-stubs-and-tiers.md, 030-shared-origin.md]
affects: [test/conformance/, .github/workflows/]
effort: medium
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Conformance suite

## Overview

`test/conformance` is the contract as executable cases: an importable
Go test package that drives any `toposd` through its public API and
reports which cases pass. It runs against the server at
`TOPOS_TEST_URL`, or against one it starts in process when the variable
is unset, and the release pipeline runs it against the released image.
It covers identity, agents, sessions, events and streaming, fork,
errors, the OpenAPI document, and the session schema round trip: a
recorded log appended through the API, read back byte-identical, and
folded identically. An installation that builds on Topos runs the same
package against its own deployment.

## Current state

v0.7.0 had no server and no suite. The shape follows Lux's and Cella's
conformance suites: a package, not a binary; a target the caller hands
it; a report a pipeline can publish; declared gaps rather than skipped
cases.

## Design

### The package

```go
type Target struct {
	BaseURL string            // the API root, with any base path
	Token   func() string     // a bearer the target accepts
	Admin   func() string     // a bearer for an admin subject, for cases that need a second subject
	Gaps    map[string]string // case id to the reason the target does not serve it
}

func Run(t *testing.T, tg Target)
func FromEnv(t *testing.T) Target // TOPOS_TEST_URL, or an in-process toposd on the stubs
```

`FromEnv` reads `TOPOS_TEST_URL` ([[002-scaffold-and-configuration]]);
unset, it starts `toposd serve` in process on a temporary directory
store with the local issuer, the owner policy and the stub Lux of
[[026-stubs-and-tiers]], and mints its tokens with the local issuer's
key. A case the target declares in `Gaps` is reported as a gap with
its reason, never as a pass; a gap for a case the target does serve
fails, so a declared gap cannot outlive the fix.

### Groups

| Group | Cases |
|---|---|
| `identity` | no bearer, a bad signature and a wrong audience answer `unauthenticated`; a second subject's session answers `not_found` ([[006-identity]]) |
| `agents` | apply, get by name and by id, versions, an unchanged apply creates no version, archive |
| `sessions` | create with each Session field, get, list with filters, end, delete, the limits of an allow applied |
| `events` | send each `user.*` type, the sender set from the token, a non-user type refused, list from a sequence with paging |
| `stream` | replay then live from a sequence, `Last-Event-ID` resume, `deltas=1` frames with no `id`, the close after the session ends |
| `fork` | fork at a turn boundary, `invalid_fork_point` elsewhere ([[017-external-runners-handoff-fork]]) |
| `schema` | the round trip below |
| `errors` | every code of [[015-api]]'s table reachable from the outside answers its status and the envelope |
| `idempotency` | a repeated `POST` with the same key answers once; a different body conflicts |
| `openapi` | the served document parses and names every route the other groups called |
| `base_path` | the groups above rerun under the target's base path, and every URL the server wrote starts with it ([[030-shared-origin]]) |

### The schema round trip

The suite carries recorded logs, one per event type of
[[004-session-log]] and one of a whole multi-thread session. For each,
it creates an `external` session, appends the log through the append
route of [[017-external-runners-handoff-fork]] in batches, reads it back
with `GET /v1/sessions/{id}/events` and through the stream, and
asserts the events equal the recording byte for byte and that
`session.Fold` of what it read equals the fold of the recording for
every thread. A target that holds no external sessions declares the
group a gap.

### The report

`Run` writes a JSON report to the path of the test flag
`-conformance.report`, which the pipeline passes: one entry per case with
`id` (`<group>/<case>`), `result` (`pass`, `fail`, `gap`), the gap's
reason, and the target's `/version`. The release job of
[[028-release-and-installation]] runs the suite against the image it is
about to publish and attaches the report to the release.

## Not in this spec

The routes and errors themselves ([[015-api]]); the stubs
([[026-stubs-and-tiers]]); the task suite, which measures agent
behavior rather than the API ([[025-task-suite]]); the release pipeline
([[028-release-and-installation]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The suite passes against an in-process toposd with `TOPOS_TEST_URL` unset | `TestConformanceInProcess` | not built |
| The suite passes against the released image in the release job, and the report is attached | the `conformance` job of the release pipeline | not built |
| A recorded log of every event type, appended and read back, is byte-identical and folds identically on every thread | the `schema` group | not built |
| A declared gap is reported as a gap, and a declared gap for a case the target serves fails | `TestGapsAreHonest` | not built |
| The suite passes with a base path set | the `base_path` group against an in-process toposd with `TOPOS_BASE_PATH` | not built |
