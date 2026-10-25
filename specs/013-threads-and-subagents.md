---
title: "Subagents and the advisor: the `agent` tool that starts a subagent, `send_message` that continues one, worktree isolation, narrowing, depth"
status: in-progress
track: core
depends_on: [001-architecture.md, 004-session-log.md, 005-harness-loop.md, 008-tools.md, 009-machines.md, 012-permissions-and-approvals.md]
affects: [harness/, harness/tools/, manifest/, prompts/advisor/, prompts/harness/, prompts/results/threads/, prompts/tools/, test/tasks/threads/]
effort: large
created: 2026-09-27
updated: 2026-10-25
author: changkun
---

# Subagents and the advisor

## Overview

An agent can hand part of its work to a subagent: one of the agents it
defines, run inside the same session from a first message the caller
writes. The caller stands where a person would. It writes the task,
reads the subagent's final text as the result of its call, and may send
the subagent more work afterward, which the subagent answers from its
own conversation. This spec builds that with two tools: `agent`, which
starts a subagent, and `send_message`, which continues one. Several
subagents may run at once. Subagents share the session's machine, so a
subagent reads the file its caller wrote, or each works in a git
worktree of its own with `isolation: worktree` and returns a branch the
caller merges. Authority only narrows from a caller to its subagent,
and a message carries data, never authority. The advisor is a subagent
the agent consults with its transcript, on a stronger model.

A subagent of this spec starts fresh, from its task alone. A fork,
which continues its caller's conversation instead, is the other value
of the same tool's `subagent` input; it is
[[091-a-fork-of-the-conversation]], with a subagent's own spending
ceiling and a subagent that runs its caller's model, kept apart so that
this spec's status goes on recording the criteria it has proven.
Workflows, graphs of subagents declared before they run, are
[[033-peers-and-authored-graphs]], on the same tools.

The tools are built under older names, which Current state gives. The
names `agent` and `send_message`, and one release in which the older
names still work, are an open criterion below.

## Current state

v0.7.0 spec 006 modeled delegation as agents-as-tools: a `delegate`
tool whose child ran with the intersection of its parent's grants,
which is kept here as narrowing ("a peer of a peer is a subset of a
subset"). It ran each child in a fresh, empty sandbox, so a child could
not read its parent's files (probe failure 5 of [[005-harness-loop]]),
and a child's work ended with its one answer. v0.7.0 spec 007 bounded
depth with two independent gates, the tool withheld at the bound and
the tool refusing past it; both gates are kept. The region, autonomy
and topology concepts of v0.7.0 specs 004, 005, 009 and 012 are
retired. The advisor is one of the patterns that remain in the core
when the adversarial engine left for `latere.ai/x/adversarial`.

Today the tool that starts a subagent is named `spawn`, with the inputs
`agent`, `task`, `isolation`, `tools` and `budget`, and the tool that
continues one is `message`, with `thread`, `content` and `end`
(`harness/threads.go`). Their descriptions are `prompts/tools/spawn-v1`
and `prompts/tools/message-v1`, the harness prompt's section on them is
`prompts/harness/threads-v1`, and the texts of their results, in
`prompts/results/threads/`, say thread, spawn and message. The scripted
task `threads/readparent` calls `spawn`.

## Design

### Subagents in the log

In the log a subagent's conversation is a **thread**, and this design
says thread where it means that record. A thread is identified by the
id of its `thread.started` event, whose own `thread` field names it; the
session's own conversation is the thread with none
([[004-session-log]]). An event's `thread` is the thread whose
transcript it renders into: a thread's own events carry its id, and a
`thread.message` carries the receiving thread's. The events of threads
running at once interleave in sequence order, and each thread's
transcript is folded from its own events.

The event types `thread.started`, `thread.message` and `thread.ended`,
the `thread` member of every event, and the manifest's `spec.threads`
keep their names. Every stored session's log holds them, a fold and a
replay read them byte for byte, the SDK and every client read them, and
no model reads an event's type or a manifest's field names: the fold
renders a subagent's events into its transcript as messages and calls.
Renaming them would change no word a model reads and would need a
migration of every log.

A thread's status is derived from the log:

| Status | When |
|---|---|
| `running` | between a `thread.started` or `thread.message` addressed to it and the `tool.result` of the caller's call that drove it |
| `idle` | after that result, waiting for a message |
| `ended` | after its `thread.ended` |

### Which subagents an agent may start

A thread may start only the subagents its agent's `spec.subagents`
names ([[003-manifest]]), each a reference to another agent or an
inline definition. The harness offers `agent` to a thread below the
depth limit whose agent names at least one subagent. When the agent's
`spec.tools` names `agent`, the tool's `subagent` input holds `fork`
beside the subagents' names, and the tool is offered to an agent with
no subagents too ([[091-a-fork-of-the-conversation]]); an agent that
does not name it starts fresh subagents alone. `fork` is that value and
no subagent's name: a manifest whose `spec.subagents` names `fork` is
refused at resolve.

A subagent is a thread of the session, never a second principal in
it: it acts with the session's credentials and spends the session's
budget, whoever owns the agent it names. A referenced agent contributes
its instructions, tools, model, approvals, hooks, skills, subagents,
advisor, threads and context settings, each narrowed as the table below
says. Its `identity`, `permissions`, `model.credential`, `connections`,
`memoryStores`, `repositories` and `machine` are not used, and
`thread.started` lists the ones the referenced agent declares in
`ignored`, so a reader of the log sees that the subagent did not act as
that agent. Reaching another agent's own authority is starting a
session of that agent, which the installation's authorizer decides and
that agent's owner pays for. No spec of this core builds it: a
workflow's nodes are subagents of the session
([[033-peers-and-authored-graphs]]), and the `agent` tool never reaches
it.

### The `agent` tool

| Input | Meaning |
|---|---|
| `subagent` | a name from `spec.subagents`, which starts a fresh subagent from the task alone; or `fork`, which continues this conversation ([[091-a-fork-of-the-conversation]]) |
| `task` | the subagent's first message; a fresh subagent knows nothing else of this conversation |
| `isolation` | `shared` (default) or `worktree` |
| `tools` | optional: names narrowing the subagent's tools further |
| `budget` | optional: a USD amount narrowing the subagent's budget, the ceiling [[091-a-fork-of-the-conversation]] defines and builds |

The names follow the tool Claude Code gives a model for the same work,
`Agent`, whose `subagent_type` names a defined subagent or `fork`
([[008-tools]]).

An `agent` call appends `thread.started` with the subagent's agent, its
caller's thread as `parent`, the call's `tool_use_id`, the task, the
isolation, its working directory and branch, its depth, its model and
the tools and budget it holds after narrowing, then runs the
subagent's first turn with the same loop ([[005-harness-loop]]) on the
same runner and machine. When that turn goes idle with `end_turn`, the
call's `tool.result` is the subagent's final text followed by the line
`Subagent <id> is idle; send it more work with send_message.` A turn
that stops for any other reason makes the call's result an error naming
the stop reason, except `tool_confirmation` and `tool_result`, which
pause the whole session: it goes idle with that stop reason and the
caller's call waits with it.

### The `send_message` tool

| Input | Meaning |
|---|---|
| `to` | the id of a subagent this one started, as the `agent` call's result gives it |
| `message` | the text to send |
| `end` | optional: `true` ends the subagent after this turn |

`to` and `message` are the inputs of Claude Code's `SendMessage`. A
`send_message` call appends `thread.message` from the caller to the
subagent and runs the subagent's next turn; the call's result is the
subagent's final text. A subagent receives messages only from the one
that started it (messages between siblings are not built, and messages
between a person's sessions are
[[092-a-persons-sessions-that-see-each-others-work]]). A message is
data: it changes nothing the receiving subagent may do. With `end`, or
when the session ends, the runner appends `thread.ended` with the
subagent's final text, usage and cost.

### The names before `agent` and `send_message`

A model is offered `agent` and `send_message` alone. Through the
release that renames them, Topos still reads the old names wherever a
stored agent version, a recorded script or a model names a tool, and
the release after it refuses them:

| Where | The old name | Read as |
|---|---|---|
| a call the model makes | `spawn` with `agent`, `task`, `isolation`, `tools`, `budget` | `agent` with `subagent`, `task`, `isolation`, `tools`, `budget` |
| a call the model makes | `message` with `thread`, `content`, `end` | `send_message` with `to`, `message`, `end` |
| an approval list's pattern, a hook's matcher, a pattern a person's answer remembered, a call's `tools` | `spawn`, `message` | `agent`, `send_message` |

A call by an old name is recorded in the log as the model wrote it and
runs as the new tool. A session whose log holds `spawn` calls keeps
them in its transcript; its next request carries the new definitions.

### Worktree isolation

With `isolation: worktree` the subagent works in a git worktree of its
own on the session's machine, branched from the HEAD commit of the
caller's working directory, on the branch
`agents/<agent>/<session>.<thread>`, created as [[009-machines]]
creates a session's. The caller's uncommitted changes are not in it,
and the call's result says so when there are any. At the end of each
of the subagent's turns the harness commits the worktree's changes to
its branch, with the trailers of [[019-git]], and the result names the
branch and the commit; the caller merges it with `git`. Two isolated
subagents that write the same file leave two branches, and the caller
resolves the merge. Removal follows the worktree rule of
[[009-machines]]. A working directory that is not a git checkout
refuses `worktree` with `isolation_unavailable`.

### Narrowing from caller to subagent

| What | The subagent holds |
|---|---|
| tools | the intersection of the caller's tools, the subagent's declared tools, and the call's `tools`; an empty intersection is no tools. A fork narrows what it may run instead ([[091-a-fork-of-the-conversation]]) |
| permission mode | the stricter of the caller's and the subagent's, strictest first `plan`, `confirm`, `progressive` |
| lists and thresholds | an `always_allow` within the caller's, an `always_confirm` that contains the caller's, thresholds no higher than the caller's ([[012-permissions-and-approvals]]) |
| budget | at most the caller's remaining budget, and the call's `budget` or the subagent's `budget.maxCost` when lower; its spend counts toward every caller above it and is attributed to the thread by its id in the ledger ([[023-events-and-observability]]) |
| machine | the caller's; a subagent cannot get another machine |
| credentials | the session's; a subagent acts with nothing the session does not hold, and a referenced agent's identity, permissions, credential and connections are ignored ([[018-credentials-and-secrets]]) |
| model | the subagent's own, whose family the thread pins when it starts ([[007-models]]) |

A subagent cannot call a tool it was not granted, whatever the model
asks: the call is answered `unknown_tool` and nothing runs.

### Depth and concurrency

The session's own conversation has depth 0 and a subagent one more than
its caller. The limit defaults to 2 and an agent may set it up to 4
with `spec.threads.maxDepth`; a manifest asking more is refused at
resolve. Two gates hold it: a subagent at the limit is offered neither
`agent` nor `send_message`, and `agent` refuses past it with
`depth_exceeded`. Several `agent` and `send_message` calls in one step
run their subagents at once ([[005-harness-loop]]), at most
`spec.threads.maxConcurrent` (default 8) running in a session. An agent
that divides work among several subagents in one step and messages them
afterward needs no mechanism of its own: it is a pattern of its
instructions on these tools.

### The advisor

An agent whose `spec.advisor` names a model is offered the `advisor`
tool, with one optional input, `question`. The first call appends a
`thread.started` for the advisor: the agent `advisor`, isolation
`shared`, no tools, the named model and the advisor's instructions.
Each call sends the advisor one `thread.message` holding the caller's
transcript rendered as text and the question, runs its turn, and
returns its answer. The advisor sees what the caller has done and acts
on nothing; its cost counts toward the session's budget.

### Error codes

| Code | Meaning |
|---|---|
| `unknown_subagent` | `agent` named a subagent not in `spec.subagents` |
| `depth_exceeded` | `agent` past the depth limit |
| `too_many_subagents` | a subagent would start past `maxConcurrent`; built as `too_many_threads` |
| `subagent_not_found` | `send_message` named no subagent this one started; built as `thread_not_found` |
| `isolation_unavailable` | `worktree` asked for outside a git checkout |

A code begins the text of the call's error result, so a model reads
it; the two built with thread in them take their new names with the
tools. None ends the turn.

### Texts

The descriptions of the tools are files of `prompts/tools/`:
`agent-v1` and `send_message-v1` replace `spawn-v1` and `message-v1`
in every request from the rename on, and the old files stay embedded,
so a replay rebuilds a request that carried them
([[011-instructions-and-skills]]). `advisor-v1` is unchanged. The
harness prompt's section on subagents is `prompts/harness/threads-v2`,
after `threads-v1`, which names spawn and message. The advisor's
instructions, used when its configuration names none, and the request
that carries the caller's transcript and question are files of
`prompts/advisor/`. The texts of the results above, with the idle line,
the branch line and the note on uncommitted changes, are files of
`prompts/results/threads/`; each that says thread, spawn or message
(`idle`, `stopped`, `ended`, `depth-exceeded`, `not-spawned`,
`no-worktrees`, `too-many`, `subagent-gone`, `commit-failed`,
`uncommitted`) gets a next version that says subagent, `agent` and
`send_message`. The directory keeps its name, which no model reads.

## Not in this spec

Workflows ([[033-peers-and-authored-graphs]]); forks, a subagent's
ceiling and a subagent without a model of its own
([[091-a-fork-of-the-conversation]]); messages between sessions
([[092-a-persons-sessions-that-see-each-others-work]]); critic and grader subagents
([[031-review-and-graded-iteration]]); the multi-agent tasks that gate
each stage ([[025-task-suite]]); what the model is told about starting
subagents ([[011-instructions-and-skills]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A subagent reads the file its caller wrote in the step before it started | the task `threads/readparent` under `test/tasks.TestScriptedSolutions` ([[025-task-suite]]) | built |
| A subagent cannot call a tool its caller lacks, with the tool neither offered nor run when the model asks for it | `TestSubagentCannotCallToolParentLacks` | not built |
| A subagent receives a second message after its first task, continues from its own transcript, and answers it | `harness.TestSpawnRunsASubagentAndMessageContinuesIt` | built |
| A subagent's mode, lists, thresholds and budget are never looser than its caller's | `TestNarrowingFromCallerToSubagent` as a table test | not built |
| A subagent that names an agent with its own identity and permissions acts with the session's credentials, and its `thread.started` lists them in `ignored` | `harness.TestASubagentActsWithTheSessionsCredentials`, `manifest.TestIgnoredFieldsNameWhatAThreadDoesNotUse` | built |
| A subagent at the depth limit is offered neither the tool that starts subagents nor the one that messages them, and a call's `tools` narrows the subagent's set | `harness.TestTheDepthLimitWithholdsSpawn` | built |
| A forced call that starts a subagent past the limit is refused with `depth_exceeded`, and a manifest asking depth 5 is refused at resolve | `harness.TestSpawnRefusesAnUnknownSubagentAndTheDepthLimit`, `manifest.TestStrictDecodingCollectsEveryProblem` | built |
| Several calls that start subagents in one step run concurrently, at most `maxConcurrent` at once, and each subagent's transcript holds only its own conversation | `harness.TestParallelSpawnsAndTheConcurrencyCap`, `harness.TestSpawnRunsASubagentAndMessageContinuesIt` | built |
| A subagent started with `isolation: worktree` works in its own worktree on `agents/<agent>/<session>.<thread>`, each of its turns is a commit on that branch, the caller's checkout is untouched and told of its uncommitted changes, and a machine without worktrees refuses with `isolation_unavailable` | `harness.TestAnIsolatedThreadWorksOnItsOwnBranch`, `harness.TestWorktreeIsolationNeedsAMachineThatKeepsThem`, `machine/host.TestWorktrees` | built |
| Two subagents started with `isolation: worktree` write the same file on two branches, and the caller merges both | `TestIsolatedSubagentsReturnBranches` | not built |
| A subagent that reaches an ask pauses the session `tool_confirmation`, and after the confirmation the subagent and then its caller continue, through the call that started it and through a message, however many times it pauses | `harness.TestAPausedThreadPausesTheSessionAndResumes`, `harness.TestAPausedMessageResumes`, `harness.TestAThreadCanPauseAgainAfterResuming` | built |
| The advisor gets the caller's transcript, has no tools, runs on the configured model, keeps one conversation across calls, and its requests count in the meter every subagent shares | `harness.TestTheAdvisorSeesTheConversationAndActsOnNothing`, `harness.TestAnAdvisorCallResumes`, `harness.TestSubagentsDoNotInheritTheAdvisor` | built |
| A call whose subagent finished before the runner stopped yields its final text without running it again, and a call that started no subagent is closed `unknown_effect` | `harness.TestAThreadThatFinishedBeforeACrashIsNotRunAgain` | built |
| A session whose runner is lost while two subagents run resumes both from the log on another runner | `TestSubagentsResumeAfterRunnerLoss` | not built |
| The tools are offered as `agent`, with `subagent`, `task`, `isolation`, `tools` and `budget`, and `send_message`, with `to`, `message` and `end`, described by `tools/agent-v1` and `tools/send_message-v1`, answering `too_many_subagents` and `subagent_not_found` and the next versions of the result texts; a subagent named `fork` is refused at resolve; through the release that renames them, a call named `spawn` or `message` with the old inputs runs as the new tool, and a pattern or matcher naming an old name matches the new tool's calls | `harness.TestTheAgentAndSendMessageTools`, `harness.TestTheOldToolNamesRunForOneRelease`, `manifest.TestAPatternNamingSpawnMatchesAgent`, `manifest.TestForkIsNoSubagentsName` | not built |

## Outcome

Shipped as designed in v0.9.0 (2026-09-29): `spawn` and `message`,
subagents with their own model, instructions, effort and narrowed tools,
concurrent spawns under `maxConcurrent`, the depth limit, `isolation:
worktree` on a branch of its own, a subagent's ask that pauses the
session, and the advisor.

Open: the table test of narrowing from caller to subagent, the refusal
of a tool the caller lacks, two isolated subagents merged by their
caller, subagents resumed after a runner is lost, and the names
`agent` and `send_message` with a release of the old names beside them.

The status is `in-progress`, from these criteria alone: the rows
marked built are proven by their tests, and the rest are open.
