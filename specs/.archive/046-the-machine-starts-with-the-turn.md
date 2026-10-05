---
title: "The machine starts with the turn: a session's sandbox opens beside the turn's first model call, not at its first tool call"
status: complete
track: core
depends_on: [009-machines.md, 016-runners.md]
affects: [machine/, runner/]
effort: small
created: 2026-10-05
updated: 2026-10-05
author: changkun
---

# The machine starts with the turn

## Overview

A hosted session's Cella sandbox is created the first time a tool acts
on it ([[009-machines]]). The model answers first, decides to call a
tool, and only then does the sandbox begin to come up; the call waits
the whole time it takes, about 29 seconds for a new sandbox, while the
person sees the session running and nothing else. The model's first
answer and the sandbox's start are independent, so they can overlap.
This spec starts the machine as a turn of an agent whose tools act on
it begins, beside the turn's first model call.

Revised on 2026-10-05 by [[048-the-machine-starts-at-the-first-tool-call]]:
the machine now starts when the model's response begins a call of a
tool that acts on it, so a turn that only talks starts none. The rest
of this spec stands. The rows below record what this spec built; the
two whose rule 048 changed name the tests that replaced theirs.

## Current state

The runner gives a hosted Cella session a `machine.Deferred`, which
opens the sandbox at the first operation and runs the runner's hook
then: the hook delivers the session's repositories and appends
`session.machine` beside the turn. A drive of a session whose log
already records a machine opens it at once, before the turn, since its
sandbox exists. A session whose agent never calls a tool that acts on
a machine never creates one.

## Design

### When the machine starts

At the start of each turn of a drive, after the turn's log is read, the
runner calls `Deferred.Start` when one of the agent's tools acts on the
machine: a tool whose effect is not `none`, the tools before whose call
the harness opens the machine ([[008-tools]]). An agent whose tools act
on no machine starts none, as before. A drive of a session that already
had a machine opens it before the turn as before, so `Start` finds it
open and does nothing.

### What a start does

`Start` opens the machine in the background under the drive's context,
the context every open of a deferred machine runs under, and returns at
once. It does not run the hook: the first operation that needs the
machine runs it on the machine the start opened, so the session records
its machine, delivers its repositories and writes its attachments where
it did without a start, between the call's `agent.tool_use` and its
`tool.result`. An operation that comes while the open runs waits for
it. A failed open is answered to that first operation, as the open it
would have made itself would have failed, and the next one tries again.
A second `Start` while one runs, or once the machine is open, does
nothing; one after a failed start tries again.

### When the turn needs no machine

A turn whose model calls no tool that acts on the machine ends without
waiting for the start. The drive lets the session go at once, so a
person's delete or next message is not held by a sandbox coming up.
The open goes on until the drive's context ends; the sandbox it made,
or began, is the session's to find by name at its next open, and Cella
stops it after its idle time, keeping its workspace. A drive that ends
the session waits for the start and removes what it made. The session
records no machine for a start no tool used.

### Cost

An agent whose tools act on a machine now has its sandbox started at
the first turn of each session, a turn that only talks included, and
started again at each later turn after Cella stopped it for idleness.
That is the price of the overlap: up to `cella.AutoStop`, 15 minutes,
of an idle sandbox for a session that never needed one.

## Not in this spec

Opening, before the turn's first model call, the machine of a session
that already had one; it is opened as before, and a sandbox stopped for
idleness starts before that call. Starting the machine before the
session's first message is sent.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `Start` opens the machine without the hook; an operation during the open waits for it, then runs the hook on that machine; a second `Start` opens nothing more | `machine.TestAStartedMachineOpensInTheBackground` | built |
| A failed start answers the first operation after it, the next one tries again, and so does a later `Start`; a hook that fails on a started machine answers the operation that ran it and leaves the machine open | `machine.TestAStartThatFailsAnswersTheNextCall` | built |
| An idle release does not wait for a start; the session's end waits for it and removes the machine it made | `machine.TestAReleaseAndAStartedOpen` | built |
| The first turn of an agent whose tools act on the machine starts it beside the first model call: with a machine and a first answer of one second each, the turn takes about one second, not two, and the first tool runs on the started machine, recorded once | `runner.TestTheMachineStartsAsTheFirstToolCallBegins` | revised by 048: the machine starts at the call's start |
| A turn that only talks starts the machine, records none, and does not wait for it; an agent with no tool that acts on a machine starts none | `runner.TestATurnThatOnlyTalksStartsNoMachine`, `runner.TestAMachineOnDemandIsRecordedWhenAToolFirstActsOnIt`, `runner.TestAnEndOnIdleSessionThatOnlyTalksHasNoMachine` | revised by 048: a turn that only talks starts none |
| A hosted session's talking turns share one sandbox and record no machine | `internal/hosted.TestASessionThatOnlyTalksCreatesNoSandbox` | revised by 048: talking turns create no sandbox |

## Outcome

Built as designed on 2026-10-05. Measured with the runner's stub
machine, which takes one second to open, and the stub model, which
takes one second for its first answer, a tool call: the turn took
2.13 seconds when the machine opened at the tool call, and 1.10 to
1.14 seconds with the start, the machine's start hidden behind the
model's answer. In a hosted session the wait at the first tool call
falls by the time the model takes to decide on it; the sandbox's own
start time is unchanged.

One point the design left open was settled. The start opens the
machine but leaves its record to the first tool, so a drive that ends
need not wait for the sandbox: waiting held the session's lease for
the rest of the sandbox's start, and a person's delete right after the
answer was refused as a conflict.
