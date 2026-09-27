---
title: "Stubs and test tiers: the scripted model, the stub Lux, authorizer, issuer, sink and Cella, the tiers and their tags"
status: drafted
track: core
depends_on: [002-scaffold-and-configuration.md, 004-session-log.md, 007-models.md, 009-machines.md]
affects: [models/scripted/, test/stubs/, test/e2e/, Makefile, .github/workflows/]
effort: medium
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Stubs and test tiers

## Overview

Every tier of the suite runs in CI without a paid credential, except
the release bar of [[025-task-suite]] and the instruction tier, which
need a real model. This spec owns the scripted model, the stubs the
tiers run against (a Lux that speaks the three dialects, an authorizer,
an issuer, a sink, and a Cella API), the tiers, and the build tags
that select them. A scripted model proves what the harness does with a
response; it never proves agent behavior, which only the task suite
does.

## Current state

v0.7.0's tests drove a deterministic fake model, which was also
selected silently in production when no model was named (probe failure
7 of [[005-harness-loop]]). The scripted model here is reachable only
through the `scripted:` scheme, which every server role refuses
([[007-models]]). The stub shapes follow Lux's and Cella's stub specs;
the authorizer and issuer stubs are `latere.ai/x/pkg`'s own.

## Design

### The scripted model

`models/scripted` plays a script: a YAML file named by the connection
`scripted:<path>`, a list of steps, each one response.

| Step field | Meaning |
|---|---|
| `text`, `thinking` | text and thinking blocks; a thinking block gets the signature `scripted` and is replayed as given |
| `tool_calls` | a list of `{name, input}`; ids are `call_<seq>_<n>` |
| `stop` | `end_turn` (default), `tool_use` (default when there are calls), `max_tokens`, `refusal` |
| `usage` | token counts, and `cost_usd_micro` when set |
| `fail` | inject a failure before the response: `{status, retry_after, times}` for an HTTP error, or `cut` for a stream that ends early |
| `expect` | assertions on the request: a tool result containing a string, a system part present, a number of messages |

A step's `expect` failing fails the test with the request's diff. The
script's responses go through the Lux wire encoding like a real
model's, so the log a scripted session writes is a v1 log.

### The stubs

| Stub | Where | Does |
|---|---|---|
| Lux | `test/stubs/lux` | serves `/anthropic/v1/messages`, `/openai/v1/responses` and `/openai/v1/chat/completions` with scripted responses per model name, streaming; reports usage, with cache reads computed by matching each request's breakpoint prefixes against its earlier requests, and `cost_usd_micro`; injects the failures a script or a header names |
| authorizer | `latere.ai/x/pkg/authz/stub` told the `authorizer` vocabulary ([[006-identity]]) | allows, denies and answers limits as a test sets |
| issuer | `latere.ai/x/pkg/authkit/issuertest` | an OIDC issuer on loopback that signs tokens for any subject |
| sink | `test/stubs/sink` | receives sink events, verifies the signature, and records them for assertions ([[023-events-and-observability]]) |
| Cella | `test/stubs/cella` | the Cella routes `machine/cella` calls (sandboxes, exec with streaming, files, tar), backed by a temporary directory per sandbox; the `cella` tier uses a real Cella control plane instead |

`test/stubs` builds into one binary, `topos-stubs`, which `make run`
starts beside `toposd` ([[002-scaffold-and-configuration]]) and the
e2e tier starts for each test. It is a test artifact and never part of
an installation.

### The tiers

| Tier | Build tag | Needs | Runs |
|---|---|---|---|
| unit | none | the Go toolchain, `/bin/sh`, `git` | every package, the scripted model, the directory store, the host machine in temporary directories; the hermetic gate runs it |
| e2e | `integration` | the stubs, built by the test | `toposd` and `topos` as processes against the stubs |
| postgres | `postgres` | a container engine | the Postgres store, the queue and the store conformance suite, with a Postgres started by Testcontainers |
| cella | `cella` | a container engine | `machine/cella` against a real Cella control plane with its local driver |
| instructions | `instructions` | a model credential | the instruction tests of [[008-tools]] and [[011-instructions-and-skills]] |
| tasks | `tasks` | the CI key | the task suite and the release bar ([[025-task-suite]]) |
| conformance | `conformance` | a running `toposd` or none | [[029-conformance]] |

`go test ./...` with no tag runs the unit tier and needs nothing else.
The CI workflow runs unit, race and hermetic on every push, e2e,
postgres and cella on every push to `main` and every pull request, and
instructions and tasks only in the release pipeline and on demand.

## Not in this spec

The task suite and the bar ([[025-task-suite]]); the conformance suite
([[029-conformance]]); what each stub's consumer asserts.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every tier but instructions and tasks runs in CI with no paid credential and no secret in the workflow | `TestWorkflowTiersNeedNoPaidCredential` over the workflow files | not built |
| `go test ./...` with no tag passes in the hermetic gate with only `/bin` and `/usr/bin` on `PATH` | the `hermetic` gate | not built |
| A script's `fail` injects a 429 with `Retry-After`, a 500, and a cut stream, each observed by the harness as that failure | `TestScriptedFailures` | not built |
| A script's `expect` that does not match fails the test and prints the request's diff | `TestScriptedExpectations` | not built |
| The stub Lux answers each dialect in its native streaming form, and reports cache reads for a repeated prefix and none for a changed one | `TestStubLuxDialects`, `TestStubLuxCacheSimulation` | not built |
| The stub Cella passes the parity cases `machine/cella` runs against the real Cella tier | `TestStubCellaMatchesCella` | not built |
| `make run` starts `toposd` and the stubs and a session completes against them with no credential | `TestMakeRunCompletesASession` | not built |
