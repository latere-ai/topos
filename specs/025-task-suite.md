---
title: "The task suite and the release bar: tasks and checkers, the pinned model, the threshold, spend, replays, IR against SDK"
status: drafted
track: core
depends_on: [005-harness-loop.md, 007-models.md, 008-tools.md, 024-client-cli-skill.md, 026-stubs-and-tiers.md]
affects: [test/tasks/, .github/workflows/]
effort: large
created: 2026-09-27
updated: 2026-10-02
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
| `task.yaml` | `name` and `category`, which match the directory; `prompt`; `agent`, overrides of the suite's agent: `tools` (the built-ins it holds; absent holds every one), `instructions`, and `subagents` (each with its `instructions` and `tools`); `timeout` (default `30m`); `maxCost` (USD, default `2.00`); `runs` (default 3); `serve`, a directory of the task served over loopback for each run |
| `fixture/` | the starting working directory, copied fresh for each run, or `fixture.bundle`, a git bundle cloned for each run; a task with neither starts in an empty directory. A fixture holding Go code has its own `go.mod`, so the repository's module never compiles it |
| `check.yaml` or `check.sh` | the checker: given the final working directory and the session log, it passes or fails, deterministically. `check.yaml` is a list of assertions; `check.sh` runs with `/bin/sh`, the working directory and the log's path as its arguments, and passes on exit 0 |
| `solution.yaml` | a script of the scripted model ([[026-stubs-and-tiers]]) that solves the task |
| `wrong.yaml` | a script that fails the task the way a model plausibly would; every instruction test has one |
| `testdata/` | what the checker compares against or overlays on a copy of the final directory, such as a test the model never saw; the Go tool ignores it |

A prompt, a `check.yaml` and a script may hold two placeholders,
replaced for each run: `${WORKDIR}`, the working directory's absolute
path with symbolic links evaluated as the host machine sees it, and
`${SERVE_URL}`, the base URL of the task's `serve` directory.

A checker reads outcomes (tests pass, a file has the required content,
a branch was merged, no file outside the fixture changed), never the
model's prose, and no checker in the gate asks a model. The tool calls
of the log are outcomes too: an instruction test's checker reads them.
A run that ends at a limit, a budget or an error fails.

| Category | Holds, at phase 1's close |
|---|---|
| `coding` | at least 30 tasks: fix a failing test, implement from a written requirement, refactor across files, debug from a stack trace, a task needing more than 50 steps, one whose context passes the compaction threshold, one whose answer needs more than 4096 output tokens |
| `files` | at least 10 tasks on files that are not code: configuration, data conversion, documentation |
| `instructions` | one directory per built-in tool ([[008-tools]]), one for project instruction files and one for skills ([[011-instructions-and-skills]]) |
| `threads` | added with 013's stages: a subagent that reads its parent's file, a thread messaged twice, three parallel threads, two isolated threads merged |

The first tasks are the ones the failures of v0.7.0 ([[005-harness-loop]])
would have failed, and ordinary coding tasks:

| Task | Exercises |
|---|---|
| `coding/manysteps` | a chain of 24 files, each naming the next, with only `read` and `write`: 26 steps, past the 16 calls v0.7.0 stopped after (failure 1) |
| `files/longoutput` | a 300-row CSV converted to JSON in one `write` of more than 16,000 bytes, an answer past 4096 output tokens (failure 2) |
| `threads/readparent` | a subagent that reads the file its parent wrote and writes what it counted (failure 5) |
| `files/abspaths` | an edit and a new file named by absolute paths, with no file created anywhere else (failure 6) |
| `coding/personpath` | a `go run` through `bash`, which needs the person's `PATH` and `HOME` (failure 6) |
| `coding/fixtest` | a failing Go test fixed without changing it, then a test the model never saw |
| `coding/addfunc` | a function and its table test added from a written requirement, then a test the model never saw |
| `coding/rename` | a function renamed across files, packages and tests, with a function sharing its prefix left alone |

The instruction tests hold one directory per built-in tool, each
measuring the use its description asks for:

| Task | Passes when the model |
|---|---|
| `instructions/read` | reads a 3000-line file with `offset` rather than all at once |
| `instructions/write` | replaces an existing file with no write refused for want of a read |
| `instructions/edit` | changes one of two identical lines with an `old_string` unique from the first try, not `write` |
| `instructions/bash` | starts a server with `background`, never letting a foreground call time out |
| `instructions/grep` | finds call sites with `output_mode` `content` |
| `instructions/glob` | lists files by a `*.md` pattern with `glob` |
| `instructions/todo` | keeps a list of at least four items, one in progress at a time, all completed at the end |
| `instructions/web_fetch` | reads the page the prompt names with `web_fetch` |

### The checker

`check.yaml` is a list; each entry is one assertion, evaluated in
order, and the first that fails fails the run with its number, its kind
and the reason.

| Assertion | Passes when |
|---|---|
| `go: {args, overlay}` | `go <args>` exits 0 in a copy of the final directory, with the task's `overlay` directory copied over it first, `GOWORK=off`, `GOTOOLCHAIN=local` and a `TMPDIR` of the checker's own |
| `file: {path, equals, equals_file, json_equals_file, contains, not_contains, matches}` | the file exists and every condition holds; `equals` and `equals_file` compare with trailing white space removed, `json_equals_file` compares decoded values |
| `unchanged: [paths]` | each file equals the fixture's copy |
| `absent: [paths]` | no path exists |
| `only_files: [paths]` | the working directory holds exactly these files, `.git` aside |
| `no_match: {glob, pattern}` | no file matching the glob matches the RE2 pattern |
| `call: {tool, thread, outcome, input, min, max}` | at least `min` (default 1) and at most `max` calls match: an `agent.tool_use` of the tool, in the `root` thread, a `sub` thread or any, whose `tool.result` has the outcome, and whose top-level input fields meet their matchers (`present`, `equals`, `not_equals`, `matches`, `contains`, `min_length`) |
| `no_call: {...}` | no call matches |
| `todo: {min_items, max_in_progress, final}` | the root thread's `todo` lists held at least `min_items` items, never more than `max_in_progress` in progress, and the last list's items all have the status `final` |

The verdict of a checker is a function of the final directory and the
log alone: `go test` durations are removed from a reason, and a second
evaluation of the same run gives the same verdict.

### A run

A run copies the task's starting files into a fresh directory, creates
a session over it in a directory store, appends the prompt, and drives
it in process through `runner` and `harness`, the pieces `topos run`
assembles: the host machine with the person's own environment, every
built-in tool the task holds, every call allowed (`confirm` mode with
every built-in on the always-allow list, since no person answers), the
task's instructions and subagents, the turn deadline of `timeout`, and
a checkpoint directory. The session's budget is the task's `maxCost`,
and it ends when its turn ends. The machine's commands run with a
`TMPDIR` inside the run's directory, so that what they leave behind,
such as the build directory of a `go run` the session's end stopped,
stays in the run's artifact. When the turn ends the machine is released
for good, which stops the run's background jobs.

A run passes when the session ended its turn, spent no more than its
`maxCost`, and the checker passed. Its result carries the steps (model
requests of every thread), the tool calls, the cost, the stop reason,
the session and the paths of its log and working directory.

In the unit tier every task runs with its `solution.yaml`, which must
pass, and with its `wrong.yaml`, which the checker must refuse; an
instruction test's wrong solution must be refused by an assertion on
the log, not on the files. That proves every task and checker end to
end with no model, and proves nothing about agent behavior, which only
a real model's runs do.

### The pass rate and the bar

A run of the suite runs every task `runs` times; the pass rate is
passed runs over all runs. `test/tasks/bar.yaml` names the one gating
model under `model` (its `name`, `family` and `connection`), the
`baseline`, the `noise`, the `threshold`, and under `runs` the `ref`
and `pass_rate` of the five release runs they were measured from. A bar
names its model before the baseline is measured and carries no figures
until then; a pinned model's run against an unmeasured bar fails. A
bar's figures must be the ones its runs give.

| Figure | How it is set |
|---|---|
| baseline | the mean pass rate of five full runs of the suite on the pinned model at the commit that closes phase 1 |
| noise | twice the population standard deviation of the pass rate across those five runs |
| threshold | baseline less noise, rounded down to a whole percent |

A release job runs the suite on the pinned model and fails when the
pass rate is below the threshold or the run is incomplete; the rate,
the threshold and the report are in the release notes, and the tag
that closes phase 1 says so. Changing the pinned model measures a new
baseline in the same way, in its own commit. Other models run in a
report job that never fails a release: a report of any model but the
pinned one is not gating. A developer may run the suite against a free
or local model while working; such results are reports and are never
the baseline.

### Spend

A release run uses a CI key whose Lux budget caps the run's total
spend; each task's `maxCost` caps one run, as the session's budget. A
run that hits its `maxCost`, at the budget's check before a request or
by the cost of its last response, fails. When Lux refuses the key for
spend (HTTP 429 with the error type `budget_exhausted` or
`spend_exceeded`) the suite stops before its next run, and the report
is incomplete, which never passes. The suite also takes a budget of its
own, `TOPOS_TASKS_BUDGET` in USD, past which it stops the same way.

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

`test/tasks` is importable: `tasks.Run(ctx, tasks.Options{Dir, Work,
Model, Connection, Machine, Filter, Runs})` returns a report, so an
external evaluation harness runs the same harness as one cell of its
matrix. `Dir` is the suite's root: by default the directory the package
was built from, or the working directory when a build with `-trimpath`
leaves no such directory. The Go fixtures are modules of their own,
which a module download leaves out, so an external harness runs the
suite from a checkout. `Work` keeps every run's working
directory and session data and must not be inside a git checkout,
`Machine` opens a run's machine (the host by default; the checker reads
the final directory from local disk, so the machine's working directory
is one), and `Filter` is a regular expression over task ids. `Script` plays each task's
`solution.yaml` or `wrong.yaml` through the scripted model in place of
the connection. `tasks.RunTask` runs one task once, and `tasks.Judge`
evaluates a finished run's checker again.

The tasks tier is `go test -tags tasks -timeout 0 ./test/tasks/`, and
the instruction tier the same with `-tags instructions`, which runs only
`instructions/`. Both read the model from the environment and skip,
naming the variables, when the connection, the credential or the model
is missing:

| Variable | Holds |
|---|---|
| `TOPOS_MODELS_URL` | the connection's base URL, as `topos run` reads it |
| `TOPOS_MODELS_KEY` | the credential |
| `TOPOS_TASKS_MODEL` | the catalog name of the model to run |
| `TOPOS_TASKS_FILTER` | a regular expression over task ids; empty is every task of the tier |
| `TOPOS_TASKS_RUNS` | the runs per task, in place of each task's own |
| `TOPOS_TASKS_BUDGET` | the suite's own spend cap in USD |
| `TOPOS_TASKS_COMMIT` | the commit the report names |
| `TOPOS_TASKS_OUT` | where the report and the runs are kept; by default a new directory under the system's temporary directory, named in the test's log |

A single task runs with `go test ./test/tasks -run
'TestScriptedSolutions/<category>/<name>'` against its scripts, or with
`TOPOS_TASKS_FILTER` and the tag of the tier against a model
([[026-stubs-and-tiers]]).

### The report

`report.json` and `report.md`: per task the runs, passes, median steps,
median cost and the stop reasons seen; per run of the suite the pass
rate, the model, the commit, the spend, and whether it is incomplete
and why; and every run's result with the reason it failed.

## Not in this spec

The tiers and their build tags ([[026-stubs-and-tiers]]); what an
instruction test measures for each tool ([[008-tools]]); the release
pipeline around the job ([[028-release-and-installation]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A release job fails when the pinned model's pass rate is below the threshold in `bar.yaml`, and passes at or above it | `test/tasks.TestReleaseBarFailsBelowThreshold` over recorded reports | built |
| `bar.yaml` names exactly one gating model, and the report job's other models cannot fail a release | `test/tasks.TestBarNamesOneModel` | built |
| The threshold is computed from five recorded runs as baseline less twice the standard deviation | `test/tasks.TestThresholdFromBaselineRuns`, `test/tasks.TestParseBarChecksItsFigures` | built |
| A run that hits its `maxCost` fails, and a suite run that hits the CI budget ends the job incomplete | `test/tasks.TestSpendCapsFailClosed` | built |
| Every checker is deterministic: two evaluations of the same final directory and log agree | `test/tasks.TestScriptedSolutions` | built |
| Every task's scripted solution passes its checker and every wrong solution is refused, with no model | `test/tasks.TestScriptedSolutions`, `test/tasks.TestEveryTaskHasItsScripts` | built |
| Each assertion of `check.yaml` passes and fails as its row says, and a malformed check is refused | `test/tasks.TestFileAssertions`, `test/tasks.TestTreeAssertions`, `test/tasks.TestGoAssertions`, `test/tasks.TestCallAssertions`, `test/tasks.TestTodoAssertions`, `test/tasks.TestCheckScripts`, `test/tasks.TestParseCheckRefuses` | built |
| A task directory that breaks the format is refused when the suite loads | `test/tasks.TestLoadTaskRefuses`, `test/tasks.TestLoadRefusesASuite` | built |
| A run starts from a fresh copy of its fixture or a clone of its bundle, and reports its verdict, steps, cost, stop reason and log | `test/tasks.TestRunReportsEveryRun`, `test/tasks.TestABundleIsCloned`, `test/tasks.TestRunCountsFailedRuns` | built |
| A run that stops other than at the end of its turn fails | `test/tasks.TestAStopOtherThanTheEndOfTheTurnFails` | built |
| A run goes through `models/dialect` over HTTP, as against a real model, with no credential | `test/tasks.TestARunThroughTheHTTPModel` against the stub Lux | built |
| No Go file of a task is part of the repository's module | `test/tasks.TestFixturesAreModules` | built |
| The report holds, per task, the runs, passes, median steps and cost and the stop reasons, and is written as JSON and Markdown | `test/tasks.TestSummarizeGroupsRunsByTask`, `test/tasks.TestTheReportIsWrittenAndRead` | built |
| The IR path and the SDK path of one recorded suite run are compared and a gap beyond the noise fails | `test/tasks.TestIRAgainstSDKComparison` | built |
| `test/tasks/sdkmodel` runs the suite through the pinned provider's own Go SDK | `TestSDKModelRunsTheSuite` | not built |
| The tasks of the v0.7.0 failures pass against the pinned model | `test/tasks.TestTheSuiteAgainstAModel` with the `tasks` tag | not built |
| The replay tier re-encodes every recorded request to its recorded hash | `TestRecordedRunsReplay` | not built |
| The suite at phase 1's close has the task counts of the category table | `TestSuiteComposition` | not built |

## Outcome

Shipped in v0.9.0 (2026-09-29): `test/tasks`, the task format and its
checkers, a scripted right and wrong solution for every task, runs
through `models/dialect` as against a real model, the report as JSON and
Markdown, and the release bar's arithmetic.

Open: no `bar.yaml` names the pinned model and no baseline has been
measured, so no release has carried a pass rate; the run through the
provider's own SDK, the v0.7.0 failure tasks against the pinned model,
the replay tier, and the category table's task counts.

The status stays `drafted` while [[005-harness-loop]], [[008-tools]],
[[024-client-cli-skill]] and [[026-stubs-and-tiers]] are open, since the
gate starts no spec before its dependencies close.
