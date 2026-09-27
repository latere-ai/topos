---
title: "Stubs and test tiers: the scripted model, the stub Lux, authorizer, issuer, sink and Cella, the tiers and their tags"
status: drafted
track: core
depends_on: [002-scaffold-and-configuration.md, 004-session-log.md, 007-models.md, 009-machines.md]
affects: [models/scripted/, test/stubs/luxstub/, test/stubs/, test/e2e/, Makefile, .github/workflows/]
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
| `tool_calls` | a list of `{name, input}`; ids are `call_<request>_<n>` |
| `stop` | the IR stop reason; default `tool_use` when there are calls, `end_turn` otherwise |
| `usage` | `input_tokens`, `output_tokens`, and `cost_usd_micro` when set |
| `fail` | a failure before the response: `{status, retry_after, times, type}` for an HTTP error `times` over, or `cut` for a stream that ends before its terminal event |
| `expect` | assertions on the request: `tool_result_contains`, `system_contains`, `messages` (a count) |

A script is `{steps: [...]}`; one `scripted.Model` keeps each script's
position, so the requests of a session advance through its steps, and a
request past the last step is `ErrExhausted`. A step's `expect` failing
answers an `ExpectationError` naming the step, the problem and the
request. The script's responses go through the Lux wire encoding like a
real model's, and a scripted `model.request` records the codec
`scripted@1`, so the log a scripted session writes is a v1 log.

### The stubs

| Stub | Where | Does |
|---|---|---|
| Lux | `test/stubs/luxstub` | an in-process HTTP server on loopback (`luxstub.New(t)`) that serves `/anthropic/v1/messages`, `/openai/v1/responses` and `/openai/v1/chat/completions`, decodes each request with that dialect's frontend codec, and streams the reply scripted for the request's model (`Script(model, replies...)`) back through the same codec; a reply may carry an `Expect` check on the decoded request and a `Failure` (an HTTP status `Times` over with `RetryAfter`, a `Cut` stream, or a dialect error `Event` after the first events); `Requests()` returns every request with its dialect, headers and body; usage, cache figures and `cost_usd_micro` are the scripted response's, and cache reads computed by matching each request's breakpoint prefixes against its earlier requests join them for [[010-context]] |
| authorizer | `latere.ai/x/pkg/authz/stub` told the `authorizer` vocabulary ([[006-identity]]) | allows, denies and answers limits as a test sets |
| issuer | `latere.ai/x/pkg/authkit/issuertest` | an OIDC issuer on loopback that signs tokens for any subject |
| sink | `test/stubs/sink` | receives sink events, verifies the signature, and records them for assertions ([[023-events-and-observability]]) |
| Cella | `test/stubs/cellastub` | an in-process HTTP server on loopback (`cellastub.New(t)`) that serves the Cella routes `machine/cella` calls: the sandboxes' create (held or not), read by id or name, start, stop and delete; the synchronous exec route and the exec socket, whose two outputs interleave and whose input has no half close; the file routes and the tar routes, confined to the workspace; and the secret read. Each sandbox is a temporary directory whose workspace path is a real path, and a command runs with `/bin/sh` on the machine the test runs on, in that directory, so a command and a file route see the same files. It answers Cella's error envelope and phases, refuses an unknown Environment or secret, a taken name, a wrong bearer, and a command past Cella's 64 KiB body limit, with a reserved variable or with a working directory outside the workspace, injects a refusal per operation (`Fail`), holds a sandbox in `Starting` (`StartAfter`), stops one as the idle stop does (`Stop`), fails one (`SetFailed`), loses one (`Remove`), and records every request; the `cella` tier uses a real Cella control plane instead |

Unit tests use the stubs in process: `luxstub.New(t)` starts a server
that the test's cleanup stops. For `make run` and the e2e tier the
stubs also build into one binary, `topos-stubs`, which `make run`
starts beside `toposd` ([[002-scaffold-and-configuration]]) and the
e2e tier starts for each test. Both are test artifacts and never part
of an installation.

### The tiers

| Tier | Build tag | Needs | Runs |
|---|---|---|---|
| unit | none | the Go toolchain, `/bin/sh`, `git` | every package, the scripted model, the directory store, the host machine in temporary directories; the hermetic gate runs it |
| e2e | `integration` | the stubs, built by the test | `toposd` and `topos` as processes against the stubs |
| postgres | `postgres` | `DATABASE_URL`, or a container engine | the Postgres store, the queue and the store conformance suite, against `DATABASE_URL` when set, otherwise a `postgres:17-alpine` the test binary starts with `podman` or `docker` and removes at exit, each test on a database of its own |
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
| A script plays step by step: text, thinking with its signature, tool calls with their ids, usage and cost; its `fail` injects an HTTP error the given number of times and a cut stream; its `expect` refuses a request that does not match with an `ExpectationError`; a request past the last step is `ErrExhausted` | `models/scripted.TestAScriptPlaysStepByStep` | built |
| A malformed script is refused | `models/scripted.TestScriptsAreRefusedWhenMalformed` | built |
| A scripted connection drives the `topos` command to the end of a turn | `internal/toposcli.TestAScriptedRun` | built |
| The stub Lux answers each dialect in its native streaming form, refuses an unknown door, an undecodable body, a missing reply and a failed expectation, injects failures, and records every request | `test/stubs/luxstub.TestTheStubAnswersAsScripted`, `test/stubs/luxstub.TestEventsFollowTheGrammar`, `test/stubs/luxstub.TestAnInjectedErrorEventFollowsThePartialStream`, `models/dialect.TestEveryDialectRoundTripsThroughTheStub` | built |
| The stub Lux reports cache reads for a repeated prefix and none for a changed one | `TestStubLuxCacheSimulation` | not built |
| The stub Cella serves the sandbox, exec, exec socket, file, tar and secret routes `machine/cella` calls, with Cella's envelope, codes and phases, its refusals and its injected failures, and holds file routes to the workspace | `test/stubs/cellastub.TestSandboxLifecycle`, `test/stubs/cellastub.TestTokenFailuresAndRecords`, `test/stubs/cellastub.TestSecret`, `test/stubs/cellastub.TestExecWait`, `test/stubs/cellastub.TestExecSession`, `test/stubs/cellastub.TestExecRules`, `test/stubs/cellastub.TestFirstFrameRules`, `test/stubs/cellastub.TestFileRoutes`, `test/stubs/cellastub.TestTarRoutes`, `test/stubs/cellastub.TestWebSocketFrames` | built |
| The stub Cella passes the parity cases `machine/cella` runs against the real Cella tier | `TestStubCellaMatchesCella` | not built |
| `make run` starts `toposd` and the stubs and a session completes against them with no credential | `TestMakeRunCompletesASession` | not built |
| The task suite's recorded tool calls give the same results on the host machine and on a Cella machine, in the e2e tier | `TestMachineParity` | not built |
