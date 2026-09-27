---
title: "The task suite and the release bar: tasks and checkers, the pinned model, the threshold, spend, replays, IR against SDK"
status: drafted
track: core
depends_on: [005-harness-loop.md, 007-models.md, 008-tools.md, 024-client-cli-skill.md, 026-stubs-and-tiers.md]
affects: [test/tasks/, .github/workflows/]
effort: large
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# The task suite and the release bar

## Overview

Agent behavior is proven by tasks finished, not by lines covered. The
suite is a fixed set of coding and file tasks, each with a checker,
run against a real model through Lux. One pinned model gates every
release: a release job fails when that model's pass rate is below the
threshold, which is the phase-1 baseline less the suite's measured
noise. Other models are reported and never gate. A release run's spend
is capped by a Lux budget on the CI key. The suite runs once through
the IR path and once through the provider's own SDK, and the two must
agree; its recorded sessions replay with no model. Multi-agent tasks
join with each stage of [[013-threads-and-subagents]] and
[[033-peers-and-authored-graphs]].

## Current state

v0.7.0 was tested only with a scripted model; 93 to 100% statement
coverage and four months of green builds shipped a loop that stopped
after 16 calls and reported success ([[005-harness-loop]]). No release
could have caught it. Nothing is borrowed.

## Design

### A task

A task is a directory `test/tasks/<category>/<name>/`:

| File | Holds |
|---|---|
| `task.yaml` | `name`, `category`, `prompt`, `agent` (a manifest path or overrides of the suite's agent), `timeout` (default `30m`), `maxCost` (USD, default `2.00`), `runs` (default 3) |
| `fixture/` | the starting working directory, copied fresh for each run, or `fixture.bundle`, a git bundle cloned for each run |
| `check.go` or `check.sh` | the checker: given the final working directory and the session log, it passes or fails, deterministically; `check.sh` passes on exit 0 |

A checker reads outcomes (tests pass, a file has the required content,
a branch was merged, no file outside the fixture changed), never the
model's prose, and no checker in the gate asks a model. A run that ends
at a limit, a budget or an error fails.

| Category | Holds, at phase 1's close |
|---|---|
| `coding` | at least 30 tasks: fix a failing test, implement from a written requirement, refactor across files, debug from a stack trace, a task needing more than 50 steps, one whose context passes the compaction threshold, one whose answer needs more than 4096 output tokens |
| `files` | at least 10 tasks on files that are not code: configuration, data conversion, documentation |
| `instructions` | one directory per built-in tool ([[008-tools]]), one for project instruction files and one for skills ([[011-instructions-and-skills]]) |
| `threads` | added with 013's stages: a subagent that reads its parent's file, a thread messaged twice, three parallel threads, two isolated threads merged |

### The pass rate and the bar

A run of the suite runs every task `runs` times; the pass rate is
passed runs over all runs. `test/tasks/bar.yaml` names the one gating
model (its name, family and connection), the baseline, the noise, the
threshold, and the release runs they were measured from.

| Figure | How it is set |
|---|---|
| baseline | the mean pass rate of five full runs of the suite on the pinned model at the commit that closes phase 1 |
| noise | twice the standard deviation of the pass rate across those five runs |
| threshold | baseline less noise, rounded down to a whole percent |

A release job runs the suite on the pinned model and fails when the
pass rate is below the threshold; the rate, the threshold and the
report are in the release notes, and the tag that closes phase 1 says
so. Changing the pinned model measures a new baseline in the same way,
in its own commit. Other models run in a report job that never fails a
release. A developer may run the suite against a free or local model
while working; such results are reports and are never the baseline.

### Spend

A release run uses a CI key whose Lux budget caps the run's total
spend; each task's `maxCost` caps one run. A run that hits its
`maxCost` fails. A suite run that hits the CI budget stops, and the job
fails as incomplete, never as a pass.

### IR path against the provider's SDK

The suite runs once through `models/dialect` and once through a
`Model` built on the pinned provider's own Go SDK
(`test/tasks/sdkmodel`, behind a build tag, never shipped). The two
pass rates must agree within the noise; a larger gap fails the job,
because it is evidence the IR loses something the provider's own
client keeps ([[007-models]]).

### Recorded replays

Every release run keeps its sessions, logs and blobs, as an artifact.
The replay tier folds each one and re-encodes every request with no
model ([[007-models]]'s `Replay`); any hash that differs at the same
codec version fails it. The recorded sessions are also the golden logs
of [[004-session-log]]'s fold tests.

### Running the suite from elsewhere

`test/tasks` is importable: `tasks.Run(ctx, tasks.Options{Model,
Machine, Filter, Runs})` returns a report, so an external evaluation
harness runs the same harness as one cell of its matrix. `topos` runs a
single task with `go test ./test/tasks -run <name>` and the tag of the
tier ([[026-stubs-and-tiers]]).

### The report

JSON and a Markdown table: per task the runs, passes, median steps,
median cost and the stop reasons seen; per run of the suite the pass
rate, the model, the commit and the spend.

## Not in this spec

The tiers and their build tags ([[026-stubs-and-tiers]]); what an
instruction test measures for each tool ([[008-tools]]); the release
pipeline around the job ([[028-release-and-installation]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A release job fails when the pinned model's pass rate is below the threshold in `bar.yaml`, and passes at or above it | `TestReleaseBarFailsBelowThreshold` over recorded reports | not built |
| `bar.yaml` names exactly one gating model, and the report job's other models cannot fail a release | `TestBarNamesOneModel` | not built |
| The threshold is computed from five recorded runs as baseline less twice the standard deviation | `TestThresholdFromBaselineRuns` | not built |
| A run that hits its `maxCost` fails, and a suite run that hits the CI budget ends the job incomplete | `TestSpendCapsFailClosed` | not built |
| Every checker is deterministic: two evaluations of the same final directory and log agree | `TestCheckersAreDeterministic` | not built |
| The IR path and the SDK path of one recorded suite run are compared and a gap beyond the noise fails | `TestIRAgainstSDKComparison` | not built |
| The replay tier re-encodes every recorded request to its recorded hash | `TestRecordedRunsReplay` | not built |
| The suite at phase 1's close has the task counts of the category table | `TestSuiteComposition` | not built |
