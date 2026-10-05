---
title: "The machine starts at the first tool call: a session's sandbox starts when the model's response begins a call of a tool that acts on it, beside the call's arguments, and a turn that only talks starts none"
status: in-progress
track: core
depends_on: [009-machines.md, 016-runners.md, 046-the-machine-starts-with-the-turn.md]
affects: [machine/, harness/, runner/, test/stubs/luxstub/]
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
can use a tool. This spec moves the start to the first moment the turn
is known to need the machine: the model's streamed response beginning
a call of a tool that acts on it. A turn that only talks starts nothing;
a turn that calls such a tool overlaps the machine's start with the
call's arguments streaming.

## Current state

| Piece | Where | Today |
|---|---|---|
| The start | `runner.Drive`, before each `RunTurn` | `Deferred.Start` when any tool of the agent's registry has an effect |
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
operation, an idle release does not wait for a start and the session's
end does, and a drive of a session that already had a machine opens it
before the turn.

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
| Opening, before the turn, the machine of a session that already had one | kept from 046: the turn's first request carries the machine's context, read from the open machine. A turn that only talks in such a session still starts its stopped sandbox; rendering that context from the session's record instead is its own change |

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `machine.Start` begins a deferred machine's open without waiting, and does nothing to a machine that exists or to none | `machine.TestStartBeginsADeferredOpenAndLeavesAnyOtherMachineAlone` | not built |
| A response that only talks, calls a tool of no effect, or names a tool the thread does not have starts no machine; one that calls a tool that acts on the machine starts it at the call's block start, before the arguments stream, and the call runs on that machine | `harness.TestTheMachineStartsWhenAToolCallBegins` | not built |
| A client's tool and a tool of no effect open no machine; a tool of any other effect does | `harness.TestOpensMachine` | not built |
| With a second before the call, a second of arguments and a machine of a second, the machine opens after the call begins and the turn takes about two seconds, not three | `runner.TestTheMachineStartsAsTheFirstToolCallBegins` | not built |
| A turn that only talks opens no machine and records none | `runner.TestATurnThatOnlyTalksStartsNoMachine`, `runner.TestAnEndOnIdleSessionThatOnlyTalksHasNoMachine` | not built |
| A hosted session whose turns only talk creates no sandbox and starts none | `internal/hosted.TestASessionThatOnlyTalksCreatesNoSandbox` | not built |
| A reply's hold runs before each event, after the events before it reached the client | `luxstub.TestAHoldPacesTheStream` | not built |
