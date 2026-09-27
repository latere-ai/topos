---
title: "Threads, subagents and the advisor: the session's graph, spawn and message, worktree isolation, narrowing, depth"
status: drafted
track: core
depends_on: [001-architecture.md, 004-session-log.md, 005-harness-loop.md, 008-tools.md, 009-machines.md, 012-permissions-and-approvals.md]
affects: [harness/, harness/tools/, prompts/advisor/, prompts/results/threads/, prompts/tools/]
effort: large
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Threads, subagents and the advisor

## Overview

A session's agents form a graph. Its nodes are **threads**, each the
conversation of one agent inside the session; the first is the
session's own. Its edges are **spawn edges**, along which authority
narrows, and **message edges**, which carry data and never authority.
This spec builds two stages of that graph: persistent subagents, which
a thread starts with the `spawn` tool and keeps messaging afterward
with `message`; and several threads running at once under one
orchestrating thread. Threads share the session's machine, so a
subagent reads the file its parent wrote, or work in a worktree of
their own with `isolation: worktree` and return a branch the parent
merges. The advisor is a thread the agent consults with its transcript,
on a stronger model. Peers and graphs declared up front are
[[033-peers-and-authored-graphs]], on the same threads and messages.

## Current state

v0.7.0 spec 006 modeled delegation as agents-as-tools: a `delegate`
tool whose child ran with the intersection of its parent's grants,
which is kept here as narrowing ("a peer of a peer is a subset of a
subset"). It ran each child in a fresh, empty sandbox, so a child could
not read its parent's files (probe failure 5 of [[005-harness-loop]]),
and a child's work ended with its one answer. v0.7.0 spec 007 bounded
depth with two independent gates, the tool withheld at the bound and
the spawner refusing past it; both gates are kept. The region, autonomy
and topology concepts of v0.7.0 specs 004, 005, 009 and 012 are
retired. The advisor is one of the patterns that remain in the core
when the adversarial engine left for `latere.ai/x/adversarial`.

## Design

### Threads in the log

A thread is identified by the id of its `thread.started` event, whose
own `thread` field names it; the session's thread has none
([[004-session-log]]). An event's `thread` is the thread whose
transcript it renders into: a thread's own events carry its id, and a
`thread.message` carries the receiving thread's. The events of threads
running at once interleave in sequence order; each thread's transcript
is its own fold. A thread's status is derived
from the log:

| Status | When |
|---|---|
| `running` | between a `thread.started` or `thread.message` addressed to it and the `tool.result` of the parent's call that drove it |
| `idle` | after that result, waiting for a message |
| `ended` | after its `thread.ended` |

### What a thread may spawn

A thread may spawn only the agents its agent's `spec.subagents` names
([[003-manifest]]), each a reference to another agent or an inline
definition. The harness offers `spawn` only to a thread whose agent
names at least one subagent and whose depth is below the limit.

A subagent is a thread of the session, never a second principal in
it: it acts with the session's credentials and spends the session's
budget, whoever owns the agent it names. A referenced agent contributes
its instructions, tools, model, approvals, hooks, skills, subagents,
advisor, threads and context settings, each narrowed as the table below
says. Its `identity`, `permissions`, `model.credential`, `connections`,
`memoryStores` and `machine` are not used, and `thread.started` lists
the ones the referenced agent declares in `ignored`, so a reader of the
log sees that the thread did not act as that agent. Reaching another
agent's own authority is starting a session of that agent, which the
installation's authorizer decides and that agent's owner pays for
([[033-peers-and-authored-graphs]]); a spawn never reaches it.

### The spawn tool

| Input | Meaning |
|---|---|
| `agent` | a name from `spec.subagents` |
| `task` | the first message of the new thread |
| `isolation` | `shared` (default) or `worktree` |
| `tools` | optional: names narrowing the subagent's tools further |
| `budget` | optional: a USD amount narrowing the thread's budget |

A `spawn` call appends `thread.started` with the subagent's agent, its
parent, the call's `tool_use_id`, the task, the isolation, its working
directory and branch, its depth, its model and the tools and budget it
holds after narrowing, then runs the thread's first turn with the same
loop ([[005-harness-loop]]) on the same runner and machine. When that
turn goes idle with `end_turn`, the `spawn` call's `tool.result` is
the thread's final text followed by the line `Thread <id> is idle;
send it more work with message.` A thread's turn that stops for any
other reason makes the call's result an error naming the stop reason,
except `tool_confirmation` and `tool_result`, which pause the whole
session: it goes idle with that stop reason and the parent's call waits
with it.

### The message tool

| Input | Meaning |
|---|---|
| `thread` | the id of a thread this thread spawned |
| `content` | the text to send |
| `end` | optional: `true` ends the thread after this turn |

A `message` call appends `thread.message` from the caller to the
thread and runs the thread's next turn; the call's result is the
thread's final text. A thread may message only the threads it spawned
(declared edges between siblings are [[033-peers-and-authored-graphs]]).
A message is data: it changes nothing the receiving thread may do. With
`end`, or when the session ends, the runner appends `thread.ended`
with the thread's final text, usage and cost.

### Worktree isolation

With `isolation: worktree` the thread works in a git worktree of its
own on the session's machine, branched from the HEAD commit of the
parent's working directory, on the branch
`agents/<agent>/<session>.<thread>`, created as [[009-machines]]
creates a session's. The parent's uncommitted changes are not in it,
and the call's result says so when there are any. At the end of each
of the thread's turns the harness commits the worktree's changes to the
thread's branch, with the trailers of [[019-git]], and the result names
the branch and the commit; the parent merges it with `git`. Two
isolated threads that write the same file leave two branches, and the
parent resolves the merge. Removal follows the worktree rule of
[[009-machines]]. A working directory that is not a git checkout
refuses `worktree` with `isolation_unavailable`.

### Narrowing along spawn edges

| What | The child holds |
|---|---|
| tools | the intersection of the parent's tools, the subagent's declared tools, and the call's `tools`; an empty intersection is no tools |
| permission mode | the stricter of the parent's and the subagent's, strictest first `plan`, `confirm`, `progressive` |
| lists and thresholds | an `always_allow` within the parent's, an `always_confirm` that contains the parent's, thresholds no higher than the parent's ([[012-permissions-and-approvals]]) |
| budget | at most the parent's remaining budget, and the call's `budget` or the subagent's `budget.maxCost` when lower; its spend counts toward every ancestor and is attributed to the thread by its id in the ledger ([[023-events-and-observability]]) |
| machine | the parent's; a thread cannot get another machine |
| credentials | the session's; a thread acts with nothing the session does not hold, and a referenced agent's identity, permissions, credential and connections are ignored ([[018-credentials-and-secrets]]) |
| model | the subagent's own, whose family the thread pins when it starts ([[007-models]]) |

A thread cannot call a tool it was not granted, whatever the model
asks: the call is answered `unknown_tool` and nothing runs.

### Depth and concurrency

The session's thread has depth 0 and a child one more than its parent.
The limit defaults to 2 and an agent may set it up to 4 with
`spec.threads.maxDepth`; a manifest asking more is refused at resolve.
Two gates hold it: a thread at the limit is not offered `spawn`, and
`spawn` refuses past it with `depth_exceeded`. Several `spawn` and
`message` calls in one step run their threads at once
([[005-harness-loop]]), at most `spec.threads.maxConcurrent` (default
8) threads running in a session. An orchestrator is an agent whose
instructions and subagents make it spawn several threads in one step
and message them afterward; it is a pattern on these tools and needs no
mechanism of its own.

### The advisor

An agent whose `spec.advisor` names a model is offered the `advisor`
tool, with one optional input, `question`. The first call appends a
`thread.started` for an advisor thread: the agent `advisor`, isolation
`shared`, no tools, the named model and the advisor's instructions.
Each call sends the advisor thread one `thread.message` holding the
caller's transcript rendered as text and the question, runs its turn,
and returns its answer. The advisor sees what the caller has done and
acts on nothing; its cost counts toward the session's budget.

### Error codes

| Code | Meaning |
|---|---|
| `unknown_subagent` | `spawn` named an agent not in `spec.subagents` |
| `depth_exceeded` | `spawn` past the depth limit |
| `too_many_threads` | a thread would start past `maxConcurrent` |
| `thread_not_found` | `message` named no thread this thread spawned |
| `isolation_unavailable` | `worktree` asked for outside a git checkout |

These are the text of the call's error result; none ends the turn.

The descriptions of `spawn`, `message` and `advisor` are files of
`prompts/tools/`; the advisor's instructions, used when its
configuration names none, and the request that carries the caller's
transcript and question are files of `prompts/advisor/`; and the texts
of the results above, with the idle line, the branch line and the note
on uncommitted changes, are files of `prompts/results/threads/`
([[011-instructions-and-skills]]).

## Not in this spec

Declared message edges between siblings, and graphs declared up front
([[033-peers-and-authored-graphs]]); critic and grader threads
([[031-review-and-graded-iteration]]); the multi-agent tasks that gate
each stage ([[025-task-suite]]); what the model is told about spawning
([[011-instructions-and-skills]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A subagent reads the file its parent wrote in the step before it was spawned | the task `threads/readparent` under `test/tasks.TestScriptedSolutions` ([[025-task-suite]]) | built |
| A subagent cannot call a tool its parent lacks, with the tool neither offered nor run when the model asks for it | `TestSubagentCannotCallToolParentLacks` | not built |
| A thread receives a second message after its first task, continues from its own transcript, and answers it | `harness.TestSpawnRunsASubagentAndMessageContinuesIt` | built |
| A child's mode, lists, thresholds and budget are never looser than its parent's | `TestNarrowingAlongSpawnEdges` as a table test | not built |
| A subagent that names an agent with its own identity and permissions acts with the session's credentials, and its `thread.started` lists them in `ignored` | `harness.TestASubagentActsWithTheSessionsCredentials`, `manifest.TestIgnoredFieldsNameWhatAThreadDoesNotUse` | built |
| A thread at the depth limit is not offered `spawn` or `message`, and a spawn's `tools` narrows the child's set | `harness.TestTheDepthLimitWithholdsSpawn` | built |
| A forced `spawn` past the limit is refused with `depth_exceeded`, and a manifest asking depth 5 is refused at resolve | `TestDepthGates` | not built |
| Several `spawn` calls in one step run concurrently, at most `maxConcurrent` at once, and each thread's fold contains only its own conversation | `harness.TestParallelSpawnsAndTheConcurrencyCap`, `harness.TestSpawnRunsASubagentAndMessageContinuesIt` | built |
| A thread spawned with `isolation: worktree` works in its own worktree on `agents/<agent>/<session>.<thread>`, each of its turns is a commit on that branch, the parent's checkout is untouched and told of its uncommitted changes, and a machine without worktrees refuses with `isolation_unavailable` | `harness.TestAnIsolatedThreadWorksOnItsOwnBranch`, `harness.TestWorktreeIsolationNeedsAMachineThatKeepsThem`, `machine/host.TestWorktrees` | built |
| Two threads spawned with `isolation: worktree` write the same file on two branches, and the parent merges both | `TestIsolatedThreadsReturnBranches` | not built |
| A thread that reaches an ask pauses the session `tool_confirmation`, and after the confirmation the child and then the parent continue, through `spawn` and through `message`, however many times it pauses | `harness.TestAPausedThreadPausesTheSessionAndResumes`, `harness.TestAPausedMessageResumes`, `harness.TestAThreadCanPauseAgainAfterResuming` | built |
| The advisor thread gets the caller's transcript, has no tools, runs on the configured model, keeps one thread across calls, and its requests count in the meter every thread shares | `harness.TestTheAdvisorSeesTheConversationAndActsOnNothing`, `harness.TestAnAdvisorCallResumes`, `harness.TestSubagentsDoNotInheritTheAdvisor` | built |
| A spawn whose thread finished before the runner stopped yields its final text without running it again, and a spawn that started no thread is closed `unknown_effect` | `harness.TestAThreadThatFinishedBeforeACrashIsNotRunAgain` | built |
| A session killed while two threads run resumes both from the log on another runner | `TestThreadsResumeAfterRunnerLoss` | not built |
