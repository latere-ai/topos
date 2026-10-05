---
title: "The machine starts at the first tool call: a session's sandbox starts when the model's response begins a call of a tool that acts on it, beside the call's arguments, in a new session and in one that had a sandbox alike, and a turn that only talks starts none"
status: complete
track: core
depends_on: [009-machines.md, 016-runners.md, 046-the-machine-starts-with-the-turn.md]
affects: [machine/, harness/, runner/, test/stubs/luxstub/, test/stubs/cellastub/]
effort: small
created: 2026-10-05
updated: 2026-10-05
author: changkun
---

# The machine starts at the first tool call

## Overview

[[046-the-machine-starts-with-the-turn]] starts a session's machine as
each turn of an agent whose tools act on it begins, so the sandbox
comes up beside the model's first answer. A turn that only talks
starts a sandbox too. On an installation whose sandbox environment
caps how many sandboxes one owner keeps, and where a stopped sandbox
counts, every conversation that only talked holds one of them until a
lifetime rule deletes it, and a person runs out of conversations that
can use a tool. A session that already had a sandbox is worse off: its
every drive opens the sandbox before the turn, so a turn that only
talks starts a stopped one, and once the sandbox environment counts
running sandboxes against the owner's ceiling, a start it refuses fails
the drive before the model is asked. This spec moves the start to the
first moment the turn is known to need the machine: the model's
streamed response beginning a call of a tool that acts on it, in a new
session and in one that had a sandbox alike. A turn that only talks
starts nothing; a turn that calls such a tool overlaps the machine's
start with the call's arguments streaming.

## Current state

| Piece | Where | Today |
|---|---|---|
| The start | `runner.Drive`, before each `RunTurn` | `Deferred.Start` when any tool of the agent's registry has an effect |
| A session that had a machine | `runner.Drive`, before the first turn | `Deferred.Open` at once, which finds the sandbox by name and starts it when it is stopped; a failed open fails the drive's setup |
| The request's machine context | `session.systemParts` | the latest `session.machine` in the log: its context block, instruction files and skills |
| The open | `turn.execute` in `harness/harness.go` | `machine.Open` before a call of a tool whose effect is not `none` |
| The stream | `turn.stream` in `harness/harness.go` | each event goes to the observer; a `block_start` of a `tool_use` block carries the tool's name before its arguments |
| The stub model | `test/stubs/luxstub` | writes a whole response, then flushes once |

## Design

### When the machine starts

The harness reads each event of a model's response as it arrives. At a
`block_start` whose block is a `tool_use`, it looks the tool's name up
in the thread's registry, and when the tool acts on the machine it
starts the machine. A tool acts on the machine when it has an effect
other than `none` and the runner, not a client, runs it: the same
predicate, `opensMachine`, decides the start and the open before the
call runs, so the two cannot disagree. A name the registry does not
hold starts nothing; the call is refused as unknown before anything
would run on the machine.

The start is `machine.Start`, which calls `Start` on a machine that
implements `machine.Starter` and does nothing to any other. A
`Deferred` is the one starter: its `Start` begins the open in the
background and returns at once, and a second start while one runs, or
once the machine is open, does nothing. Starting on the block's start
rather than on the call's validation puts the machine's open beside
the arguments' streaming, which for a call that writes a file is the
longest part of the response.

The runner no longer starts the machine at the turn's start. Everything
else of 046 stands: the start does not run the hook, the first
operation records the machine, a failed start answers the next
operation, and an idle release does not wait for a start and the
session's end does.

### A session that already had a machine

A drive of a session whose log records a machine no longer opens it
before the turn. It opens the way a new session's does, at the first
call of a tool that acts on it, and the open's hook runs as for a
session that had one: it finds the recorded sandbox and records
nothing, or records a sandbox that replaced it, with reason `handoff`,
beside the turn. The turn's requests carry the machine context the log
recorded, the latest `session.machine`'s context block, instruction
files and skills, which is where they came from before as well: the
reopen recorded nothing when it found the recorded sandbox. A turn that
only talks therefore starts no stopped sandbox and is never refused by
a ceiling on running sandboxes. A start the sandbox environment
refuses is the open of the call that needed it, so the call's result
carries the environment's sentence to the model, a `session.error`
names `machine_unavailable`, and the turn goes on; it no longer fails
the drive.

What a turn loses against the reopen before it:

| What | Before | Now |
|---|---|---|
| A sandbox that was replaced, deleted at its lifetime and created again | found before the turn; its `session.machine` reached the turn's first request | found at the first tool call; its `session.machine` lands beside the turn and reaches the requests after it, and the turn's first request describes the recorded one |
| Files attached to the turn's message | written to the machine before the turn | written at the first tool call, before that call runs, as in a new session |
| A checkpoint of a turn that only talks | taken, since the machine was open | none; rewinding to such a turn answers `checkpoint_missing`, and a fork at it restores the latest earlier checkpoint, which holds the same files |
| The session's end in a drive that opened nothing | `end_on_idle` deleted the sandbox | the sandbox is left to the sandbox environment's lifetime rules, as a session ended through the API already leaves it |
| The machine's description before the open | the open machine's | its kind alone; the harness reads only the kind, for a call's risk |

The context block's date and its git line are the record's, from the
first open, as they were.

### What it costs

A turn that calls a tool that acts on the machine waits, at that call,
for the machine's open less the time its arguments took to stream; 046
hid the model's time before the call as well. The start is a guess at
intent made before the call is validated or decided: a call later
refused by the session's permissions, or by a person in a mode that
asks, has started a machine it does not use, which the runner leaves
to its idle stop as 046 did for a turn that only talked. A dialect
whose `block_start` does not name the tool would start nothing and
open the machine at the call, as before 046; the three dialects the
harness reads name it.

### The stub model's pacing

`luxstub.Reply.Hold` runs before each event of a response is written,
after the events before it reached the client, so a test streams a
response the way a model does: a wait before a call's block, and
arguments that take a while. A reply without one is written as before.

## Not in this spec

| Item | Why |
|---|---|
| Rewinding to a turn that took no checkpoint by restoring the latest earlier one | a rewind, a failed checkpoint or a person's change between the two can make the earlier checkpoint the wrong files; a fork already walks back and a rewind does not |
| Deleting, at an `end_on_idle` session's end, a sandbox the last drive never opened | it needs a delete by name that does not start the sandbox; the sandbox environment's lifetime rules end it, as they end the sandbox of a session ended through the API |

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `machine.Start` begins a deferred machine's open without waiting, and does nothing to a machine that exists or to none | `machine.TestStartBeginsADeferredOpenAndLeavesAnyOtherMachineAlone` | built |
| A response that only talks, calls a tool of no effect, or names a tool the thread does not have starts no machine; one that calls a tool that acts on the machine starts it at the call's block start, before the arguments stream, and the call runs on that machine | `harness.TestTheMachineStartsWhenAToolCallBegins` | built |
| A client's tool and a tool of no effect open no machine; a tool of any other effect does | `harness.TestOpensMachine` | built |
| With a second before the call, a second of arguments and a machine of a second, the machine opens after the call begins and the turn takes about two seconds, not three | `runner.TestTheMachineStartsAsTheFirstToolCallBegins` | built |
| A turn that only talks opens no machine and records none | `runner.TestATurnThatOnlyTalksStartsNoMachine`, `runner.TestAnEndOnIdleSessionThatOnlyTalksHasNoMachine` | built |
| A hosted session whose turns only talk creates no sandbox and starts none | `internal/hosted.TestASessionThatOnlyTalksCreatesNoSandbox` | built |
| A reply's hold runs before each event, after the events before it reached the client | `luxstub.TestAHoldPacesTheStream` | built |
| A drive of a session that had a machine opens none for a turn that only talks, whose request carries the recorded context, and opens it once for a tool turn, recording it no second time | `runner.TestAMachineOnDemandIsRecordedWhenAToolFirstActsOnIt` | built |
| A turn that only talks, in a hosted session whose sandbox is stopped, starts nothing and completes; the next tool turn starts the sandbox once | `internal/hosted.TestATalkOnlyTurnLeavesAStoppedSandboxStopped` | built |
| A start Cella refuses for the running ceiling is the call's result, carrying Cella's sentence to the model, with a `session.error`; the drive completes and the session waits | `internal/hosted.TestAStartRefusedForTheRunningCeilingIsTheCallsResult` | built |
| A restarted runner finds a stopped sandbox by name and starts it when the resumed turn calls a tool on it | `cmd/toposd.TestCellaMachineSurvivesRunnerRestart` | built |

## Outcome

Built as designed on 2026-10-05. Measured with the runner's stub
machine, which takes one second to open, and the stub model paced by
`Hold`, one second before the call's block begins and a variable time
for its arguments; one drive per row, the start of each rule:

| Response | At the call (before 046) | At the turn (046) | At the call's start (048) |
|---|---|---|---|
| 1 s before the call, 1 s of arguments | 3.16 s | 2.14 s | 2.12 s |
| 1 s before the call, arguments at once | 2.14 s | 1.15 s | 2.10 s |
| 1 s before the call, 2 s of arguments | 4.16 s | 3.15 s | 3.14 s |
| talks only | no machine | a machine opened, none recorded | no machine |

A tool turn waits for the machine's open less the time its arguments
took, where 046 also hid the model's time before the call; a call
whose arguments stream for as long as the machine takes to open, a
file written whole, loses nothing against 046, and a short command
waits for the open. In a hosted session a new sandbox takes about 29
seconds, so the wait at the first tool call is that less the call's
arguments, and a turn that only talks creates no sandbox. Each new
test fails against 046: the machine opened before the call began, a
turn that only talked opened one, and a hosted session that only
talked created one.

One point was settled while it was built: the start and the open
before a call share one predicate, `opensMachine`, which leaves out a
client's tool, whose call never runs in the runner; 046 counted the
registry's effects alone.

The reopen of a session that had a machine was taken into this spec on
the same day, once the sandbox environment's count moved to running
sandboxes. The three tests of a session that had a machine fail
against the reopen: a turn that only talked opened the machine, a
hosted one started its stopped sandbox, and a start refused for the
ceiling failed the drive with `machine_unavailable` instead of
answering the call.

