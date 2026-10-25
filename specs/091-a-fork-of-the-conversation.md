---
title: "A fork of the conversation: a `fork` tool starts a thread that continues its caller's conversation, sent on the caller's model and effort with the caller's tool definitions and system prompt byte for byte, so its first request reads the prefix its caller's last request cached; what a fork may run is narrowed when a call runs, not in what it is offered; one depth count for spawns and forks; a thread's own spending ceiling over everything under it; a subagent that leaves its model out runs its parent's; and a trigger whose every session starts as a fork of the conversation that set it up"
status: drafted
track: core
depends_on: [.archive/004-session-log.md, 005-harness-loop.md, .archive/007-models.md, 008-tools.md, 010-context.md, 012-permissions-and-approvals.md, 013-threads-and-subagents.md, 017-external-runners-handoff-fork.md, .archive/022-triggers.md, .archive/038-routed-models.md, 056-editing-a-message-and-the-branches-of-a-session.md, 086-proactive-agents.md, 087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report.md]
affects: [harness/, harness/tools/, session/, manifest/v1/, manifest/, manifest/trigger/, internal/triggers/, internal/server/, internal/proposal/, prompts/tools/, prompts/results/threads/, prompts/threads/, api/openapi.yaml, docs/]
effort: large
created: 2026-10-25
updated: 2026-10-25
author: changkun
---

# A fork of the conversation

## Overview

A subagent of [[013-threads-and-subagents]] starts empty: its first
request holds its own instructions, its own tools and the task, and
nothing its parent knows. The parent writes what the child needs into
the task, and what it leaves out is lost. A conversation that has read a
repository for an hour cannot hand that reading to a helper except by
retelling it.

This spec adds a second way to start a thread: a fork, which continues
its caller's conversation. A fork's first request is its caller's own
next request with the task appended: the same tool definitions, the
same system prompt and the same messages, byte for byte, on the same
model at the same effort. A provider that caches a prompt's prefix
serves that request from the cache its caller's last request wrote, so
the fork starts with everything the conversation holds and pays the
cache-read price for it, a fraction of the input price: a tenth, or
less, on Anthropic's models.

Four rules follow from keeping that prefix, and two features come with
it:

1. **A fork is narrowed when a call runs, not in what it is offered.**
   Removing a tool from a fork's definitions would change the first
   bytes of its request and lose the cache. A fork holds its caller's
   definitions, and a call of a tool the fork may not run is refused
   with a code of its own.
2. **A fork runs its caller's model and effort.** A provider's cache
   belongs to one model, and a change of effort or reasoning parameters
   invalidates the cached conversation. A helper on a cheaper model is a
   spawn, not a fork.
3. **Spawns and forks share one depth count**, with 013's default of 2
   and its limit of 4.
4. **A thread has a spending ceiling of its own** that counts every
   thread under it. 013 specifies it; it is not built.
5. **A subagent may leave its model out** and run its parent's. Today a
   subagent names a model the installation's door must list, so an
   installation that routes models by a name only its authorizer
   resolves cannot give a session subagents at all.
6. **A trigger's sessions may start as a fork of the conversation that
   set it up**, so a routine's run knows what the conversation
   established without continuing one conversation that grows with
   every run.

## Current state

- `spawn` starts a thread of a subagent `spec.subagents` names, with the
  subagent's instructions, the intersection of the tools as
  `t.root.Subset(names)`, and an empty transcript but the task
  (`harness/threads.go`). `message` continues a thread its caller
  spawned. Several calls in one step run at once, at most
  `spec.threads.maxConcurrent` (default 8); past it a call is answered
  `too_many_threads` and is not queued.
- A thread's registry withholds `spawn` and `message` at the depth
  limit (default 2, at most 4), and `spawn` refuses past it with
  `depth_exceeded`. A spawned thread is never given `question`,
  `request_connection`, `publish`, `trigger`, `wake` or `notify`: they
  are left out of its definitions.
- A spawned thread's system prompt is rendered without the blocks that
  speak to the session's own agent: the initiator's instructions, the
  attached context, and the connections the person may be asked to
  connect (`headerBlocks` with `own` false, `harness/request.go`).
  `childConfig` drops the advisor, the question tool and `notify`.
- `spawn`'s schema takes a `budget`, and its `Run` does not read it. No
  thread has a ceiling of its own: the harness checks the session's
  spend against the session's ceiling before each request
  (`checkBudget`), whichever thread sends it. `thread.started` has a
  `budget` member that nothing writes.
- Every subagent's spec must name `model.name` (`manifest/validate.go`).
  The subagent's model is connected by that name when the drive starts
  (`manifest/config.go`, `internal/hosted/hosted.go`), through no
  route of the authorizer: 038 routes the session's own model at its
  create, a change and a send, and 083 a title's. A name only the
  authorizer resolves reaches the door unresolved, so a session whose
  agent names such a subagent cannot start.
- The advisor is built in the harness and wired by no runner: the
  hosted runner sets no `Advisor` in the harness's configuration, and
  `topos run` refuses an agent that names one.
- Each request is built as [[010-context]] orders it and records the
  SHA-256 of its tool definitions (`tools_sha256`) on its
  `model.request`. Cache hints are written as `cache_control` only for
  the Messages dialect and, on the OpenAI chat dialect, for models of
  the `anthropic` and `qwen` vendors
  (`latere.ai/x/pkg/llmdialect/openaichat`); every other provider caches
  a repeated prefix on its own, and a mark on Google's Gemini costs a
  cache write per turn that is never read.
- A session fork (017) copies its parent's log up to a turn boundary
  into a new session. A trigger's firing starts a new session or
  continues one (022, 086's `session.target`); none starts a fork.

## Design

### Recommendations

| Item | Recommended | Weighed against, and why not |
|---|---|---|
| The tool | a harness tool of its own, `fork`, beside `spawn` and `message` | `spawn` with a reserved agent name or a `context` input: a fork takes no agent, model or effort, and a tool with one schema is read correctly more often than one whose fields mean different things by a flag; one tool for every kind, as Claude Code's Agent tool is: Topos decides, narrows and scores each tool on its own, with one effect per tool, so spawn, fork and message stay three tools, and `message` is the agent-to-agent channel for both kinds |
| The fork's opening message | a result for every call of the caller's step, each the same fixed text, then the task in a text block after them | the task as the fork's own call's result: sibling forks of one step would differ from the first fork's result on, and share less of the prefix |
| Narrowing | when a call runs: a refusal at validation with `withheld_from_fork` | a narrowed list of definitions: the list is the first part of the cached prefix, so every narrowed fork would pay for the whole conversation again |
| The tools of the conversation with the person (`question`, `publish`, `trigger`, ...) | held in a fork's definitions and refused when called | run by the fork: the session's own thread holds the conversation with the person, and two threads asking at once is what 039 rules out |
| The model and the effort | the caller's, with no input to change them | a model per call: caches are per model, and a fork on another model is a spawn of an inline subagent, which already exists |
| The default depth | 013's 2, kept; an agent names `spec.threads.maxDepth: 3` to nest three levels, as Latere Chat's agent does | 3 as the core's default: a default applies to every agent of every installation that never chose, so raising it changes what each of them may do on upgrade; depth is not where spending is bounded (concurrency and ceilings are), and each level is one more hop at which a thread's work is cut to its final text |
| Depth for spawns and forks | one count: a thread is one deeper than its caller, whichever tool started it | two counts: a fork of a spawn of a fork would escape both |
| A thread's ceiling | counts the thread and every thread under it | its own requests alone: a fork that forks again would spend past it |
| A subagent with no model | runs its parent thread's model, connection and effort, pinned when it starts | routing every subagent's model through the authorizer: a new question at every spawn, and a helper most often needs only the model the person chose |
| A routine's run from its conversation | a session fork of that conversation at the turn that set the routine up, every run | continuing the conversation (086's `session.target`): it grows by every run; a new session (today): it holds the ask and nothing the conversation established |
| A proposal's default | `startFrom: ask`, today's new session; the agent asks for `conversation` when the run needs it | `conversation` by default: a run's first request pays for the whole conversation at the full input price, since no cache lives from one run to the next |
| A conversation gone before a firing | the firing starts a new session and its message says so | skipping the run: the person deleted a conversation, not their routine |
| Names | `fork`, `withheld_from_fork`, `thread_budget`, `startFrom` | `clone`: names a copy, not a continuation; `delegate`: v0.7.0's tool, retired |

### The fork tool

`fork` is a harness tool, as `spawn` is: the harness adds it to a
thread's registry when the agent's `spec.tools` names it and the thread
is below the depth limit. A fork itself carries its caller's
definitions whatever its depth (below).

| Name | Parallel | Effect | Input |
|---|---|---|---|
| `fork` | yes | `none` | the table below |

| Input | Meaning |
|---|---|
| `task` | what the fork does, at most `MaxForkTask` (8000) characters |
| `isolation` | `shared` (default) or `worktree`, as `spawn`'s ([[013-threads-and-subagents]]) |
| `tools` | optional: names narrowing what the fork may run, never what it is offered |
| `budget` | optional: a USD amount, the fork's ceiling (below) |

A `fork` call appends `thread.started` with the members 013 gives it
and three more:

| Member | Meaning |
|---|---|
| `context` | `fork` for a thread this tool started; absent for a spawn |
| `from` | `{thread, seq}`: the caller's thread id (`""` for the session's own) and the sequence of the `agent.message` that holds the call |
| `grant` | the tools the fork may run after narrowing, beside `tools`, which for a fork lists the definitions it holds |

and runs the fork's first turn on the same runner and machine. Its
result is `spawn`'s ([[013-threads-and-subagents]]): the fork's final
text and the line that says it takes more work, the branch line for an
isolated fork, and an error naming any other stop. `message` continues
a fork as it continues a spawned thread, and `end` ends it.

### The fork's first request

The fork's request is built by the same function, from the same parts,
as its caller's next request would be:

| Part ([[010-context]]) | A fork's |
|---|---|
| tool definitions | the caller's, in the caller's order, the same bytes: the definitions of the request the caller's step was sent with |
| the harness prompt, the agent's instructions, the context block, project files, skills, memory notes, the route | the caller's, rendered as the caller renders them: `headerBlocks` with the caller's `own`, the harness prompt with the caller's options |
| the date | the caller's turn's |
| the messages | the caller's fold through the `agent.message` at `from.seq`, then the opening message, then the fork's own events |
| the model, the connection, the effort | those the caller's step was sent with |
| the cache key | the session's, which every thread shares |

The opening message is one user message: a `tool_result` for every
call of the `agent.message` at `from.seq`, in the calls' order, each
the text of `prompts/results/threads/forked.md`, the same for every call
and every fork; then one text block of `prompts/threads/fork-v1.md`,
which says that the conversation above is the fork's context, gives the
task and the call's id, says to do the task itself and to end with what
the task asks for, since its final message is the result its caller
reads, and says which tools a fork does not run.

A call's results must follow the message that made it, so the opening
message answers every call of that message, the caller's other calls
included. The fork reads none of their results: they run in the
caller's thread, beside it.

What the first request reads from the cache, where the provider caches:

| Bytes | Price |
|---|---|
| tool definitions, system prompt, and the messages through the caller's last user message, which the caller's last request marked as breakpoint 4 and wrote | cache read |
| the caller's `agent.message` holding the call, the opening message | input, and a cache write at breakpoint 4 of the fork's request |

That is what the caller's own next step pays, apart from the results.
Several forks of one step share every byte through the last result and
differ only in the task block. Every later request of the fork rolls
breakpoints 3 and 4 forward as any thread's does.

The rule holds for every family. Where the dialect takes marks
(Anthropic's, and Anthropic's and Qwen's models on the OpenAI chat
dialect) the request carries 010's breakpoints; elsewhere the provider
caches a repeated prefix itself, and Google's Gemini is given no mark.
The cache lives minutes: Anthropic keeps an entry five minutes from its
last read, or an hour when it is written at twice the input price
rather than 1.25 times, which Topos does not ask for. A fork starts within its caller's step, seconds after the
request it reads, so it reads it. Nothing started later reads it: a
trigger's fork of a conversation a day after (below) inherits the
conversation's context, not its cache, and pays for it at the input
price.

What would break the prefix, and is therefore not a fork's to choose: a
model other than the caller's, an effort other than the caller's, a
definition removed or reordered, other instructions, a header block
rendered for another thread.

### The fork's transcript in the log

The fork's events are its own; the conversation before them is its
caller's, read in place from the same log. The fold of a fork's thread
is:

1. its caller's fold through the event at `from.seq`, by the caller's
   rules, with the caller's clearings and summaries whose range ends at
   or before `from.seq`;
2. the opening message, rendered from `thread.started`;
3. the events whose `thread` is the fork's, by the rules of any thread.

Nothing is copied. A compaction of the fork's own thread
([[010-context]]) may cover the events it inherited, by sequence, as it
covers its own; a later compaction of the caller does not change the
fork. A redaction of an inherited event forces a compaction before the
next request of every thread whose fold holds it, the fork's included,
by 010's rule. A runner that takes the session over rebuilds a running
fork's request from the log alone.

### What a fork may run

A fork holds its caller's definitions. What it may run, its grant, is:

| Rule | The grant |
|---|---|
| the caller's grant | every tool the caller may run; for a fork of a fork, that fork's grant |
| the call's `tools` | intersected with the grant when given |
| the tools of the conversation with the person | removed: `question`, `request_connection`, `publish`, `trigger`, `wake`, `notify`, and every tool a later spec reserves for the session's own thread ([[033-peers-and-authored-graphs]]'s `workflow`, [[092-a-persons-sessions-that-see-each-others-work]]'s `sessions` and `send`) |
| the depth limit | `spawn` and `fork` removed for a fork at the limit |

A call of a tool the fork holds and may not run is answered at
validation, as an unknown tool is ([[005-harness-loop]]): outcome
`error`, the text `withheld_from_fork:` and `prompts/results/threads/withheld.md`,
which names the tool and says what to do instead (end the turn saying
what needs the person, for a tool of the conversation with the person).
It gets no `agent.tool_use` and nothing runs. It counts in the
repeated-call rule of [[005-harness-loop]] as every refusal does, so a
fork that keeps calling it ends its turn.

Every call the grant allows is decided as a spawned thread's are: the
caller's mode, lists and thresholds, narrowed as 013's table says, with
the session's credentials and machine.

### Depth and concurrency

A thread's depth is its caller's plus one, whether `spawn` or `fork`
started it. The limit stays 013's: 2 unless `spec.threads.maxDepth`
says otherwise, at most 4. Of 013's two gates, the first, `spawn` not
offered at the limit, applies to every thread but a fork, which carries
its caller's definitions; the second, a call past the limit refused
with `depth_exceeded`, applies to all. Forks count toward
`spec.threads.maxConcurrent` as spawned threads do.

### A thread's ceiling

A thread may have a ceiling of its own, in USD micros, the least of:

| Source | Ceiling |
|---|---|
| the call's `budget` | that amount |
| for a spawn, the subagent's `spec.budget.maxCost` | that amount |
| the caller's ceiling, when it has one | what is left of it when the thread starts |

A thread's spend is the cost of every `model.request`, and every tool
result a service charged for ([[007-models]]), whose `thread` is the
thread or a thread under it. Before each request, the harness checks
the thread and each thread above it that has a ceiling: its spend, plus
the next request's input estimate of 007's check, against its ceiling.
At a ceiling, the requesting thread's turn ends with stop reason
`budget` and detail `thread_budget`, and the call that drove it is
answered with the error that names it. The session's own ceiling still
holds over every thread. `thread.started` records the ceiling as
`budget: {max_cost_usd_micro}`, absent when the thread has none.

The rule is the same for spawns and forks, and is the one
[[033-peers-and-authored-graphs]] holds a workflow's threads to.

### A subagent that runs its parent's model

An inline subagent's spec may leave `model` out. Its thread runs the
model its parent thread stands on when it is spawned, with that
thread's connection and effort, pinned for the thread's life as 013
pins a subagent's family. The session's permissions already name that
model, so its create's question is unchanged. A referenced agent keeps
its own model. A subagent that names a model is connected by that name,
as today.

### A trigger whose sessions start as a fork

The Trigger kind gains a session policy and two fields; [[022-triggers]]
owns the others:

| Field | Type | Meaning |
|---|---|---|
| `session.policy: fork` | the value beside `new` and `continue` | each firing forks `session.from` at its fork point and sends the firing's message to the fork |
| `session.from` | `ses_…` | the session forked; required with `fork` and refused without it; the applier must be allowed `session.fork` on it ([[017-external-runners-handoff-fork]]), asked at apply |
| `session.fromSeq` | integer | the fork point, a turn boundary of `from` (017); absent: for a trigger a session's call made, the first turn boundary after that call's result; for one applied at the route, the session's last turn boundary at the apply |

The server writes the fork point it resolved into the Trigger's
`status.forkPoint`, at the apply when the boundary exists then and at
the first firing otherwise, and keeps it: every run starts from the
same point, whatever the conversation does after it.

Each firing is a fork as [[017-external-runners-handoff-fork]] makes
one, asked as `session.fork` as the trigger's owner, with the fields a
firing's `session.create` carries ([[022-triggers]]). The fork takes the
trigger's session title in place of the continuation's title, its
`trigger_id`, and the trigger's `end_on_idle` and `session.attended`;
`skipIfActive` and `maxActive` count it as a `new` trigger's sessions.
The firing's message is its first input. A run so starts with the
conversation as its context and grows by its own turns alone. Its turns
count toward the trigger's allowance as a started session's
([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]),
and the events the fork copied are not counted, as 087 says.

A firing whose `from` is gone, or whose fork point is no longer a
boundary of it, starts a new session as `new` would, and its message
carries `fork_missing: true`, which the fold renders as one line saying
the conversation the routine was set up in is gone.

A run forked from a conversation holds a copy of that conversation, as
every fork does: a client says so where it offers the choice, and
deleting the conversation deletes none of its runs.

#### The `trigger` tool's proposal

The `trigger` tool of [[086-proactive-agents]] takes one more input for
`propose` and `change`:

| Field | Meaning |
|---|---|
| `startFrom` | `ask` (default): each run starts new with the ask, as today; `conversation`: each run is a fork of this conversation, applied as `session.policy: fork` with `session.from` the session |

The tool's check answers, for `conversation`, the tokens the fork point
holds, from the `context_report` of the session's latest request
([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]),
and what reading them once costs at the model's input price, so the
agent counts it in `perWeekUsd` and says it in `why`. The tool's
description says to choose `conversation` only when a run needs what
the conversation established and the ask cannot carry it.

### Events, fields and texts

| Name | Kind | Defined here |
|---|---|---|
| `fork` | harness tool | its input and result |
| `thread.started.context`, `.from`, `.grant` | event members | a fork's |
| `thread.started.budget` | event member, written from now | a thread's ceiling |
| `thread_budget` | `detail` of stop reason `budget` | a thread's ceiling reached |
| `withheld_from_fork` | error code | a fork's call of a tool outside its grant |
| `MaxForkTask` | constant, 8000 | `task`'s `maxLength` |
| `session.policy: fork`, `session.from`, `session.fromSeq`, `status.forkPoint` | Trigger fields | a routine's run from its conversation |
| `fork_missing` | `user.message` member | a firing whose conversation is gone |
| `startFrom` | `trigger` tool input | a proposal's choice |
| `prompts/tools/fork.md`, `prompts/threads/fork-v1.md`, `prompts/results/threads/forked.md`, `prompts/results/threads/withheld.md` | texts | the description, the opening, the placeholder result, the refusal |

### Roll order

1. A subagent that leaves its model out, and a thread's ceiling for
   `spawn`. They close 013's open budget row and let an installation
   that routes its models give a session subagents.
2. `fork`: the tool, the opening message, the fold, the grant and
   `withheld_from_fork`, and the instruction task.
3. `session.policy: fork` and the `trigger` tool's `startFrom`.

Each step is a release. Step 3 asks `session.fork` at the apply of a
`fork` trigger and at each of its firings, as the trigger's owner with
a firing's fields; an installation's authorizer learns to answer the
firing's question before the release reaches it, and until then a
`fork` trigger's firing is `refused`.

## What a hosted installation needs

Latere's platform is the first installation, and the managed agent of
Latere Chat the first caller:

| Need | Why |
|---|---|
| The managed agent's definition carries subagents and `threads`, and a version names a helper that leaves its model out | step 1: subagents for an agent that runs on a routed name |
| A later version names `fork`, with instructions on when a fork beats a spawn (the task needs what the conversation read) and when neither is worth it | step 2 |
| The authorizer answers `session.fork` asked for a firing as it answers a firing's create | step 3 |

## What the Go SDK needs

| Need | Why |
|---|---|
| A routine's `startFrom`, read and written, and kept through a change | a client shows whether a routine's runs start from the conversation, and offers the choice |
| A thread's kind (`spawn` or `fork`), its parent and its ceiling, read from `thread.started` | a client shows the helpers a conversation started and what each may spend |

## Not in this spec

A fork that runs while its caller goes on (every thread holds the call
that drove it); a fork on another model; a fork's thread moved to a
session of its own; an hour-long cache entry; the advisor's wiring into
a runner; the words a client shows.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A fork's first request carries the caller's tool definitions, system blocks, model, effort and cache key, and the caller's messages through the call's `agent.message`, byte for byte, with the same `tools_sha256` as the caller's last request | `harness.TestAForksFirstRequestKeepsItsCallersPrefix` | not built |
| Against the stub Lux, a fork's first request reads from the cache every token through the caller's last breakpoint | `harness.TestAForkReadsItsCallersCache` | not built |
| Two forks of one step share every byte through the opening message's last result and differ in the task block alone | `harness.TestSiblingForksShareTheirPrefix` | not built |
| The opening message answers every call of the caller's step with the same text, so the request is valid for a step with other calls beside the fork | `harness.TestTheOpeningMessageAnswersEveryCallOfTheStep` | not built |
| A fork's call of `question`, `publish`, `trigger`, `wake`, `notify` or `request_connection` is answered `withheld_from_fork` with no `agent.tool_use`, nothing runs, and three in a row end the turn by the repeated-call rule | `harness.TestAForkIsRefusedTheConversationsTools` | not built |
| A fork's `tools` narrows its grant and leaves its definitions as its caller's | `harness.TestAForksToolsNarrowWhatItRuns` | not built |
| A fork at the depth limit holds `spawn` and `fork` in its definitions and a call of either is `depth_exceeded`; a spawned thread at the limit is offered neither; spawns and forks count one depth | `harness.TestOneDepthForSpawnsAndForks` | not built |
| Forks count toward `maxConcurrent` | `harness.TestForksCountTowardTheConcurrencyCap` | not built |
| A fork's fold is its caller's through `from.seq`, the opening message and its own events; a caller's later compaction leaves it; a redaction of an inherited event compacts it before its next request; a runner restarted during a fork rebuilds the same request | `session.TestAForksFold`, `harness.TestARedactionCompactsEveryForkThatHoldsIt`, `harness.TestAForkResumesOnAnotherRunner` | not built |
| A fork's result is its final text and the idle line, `message` continues it, and an isolated fork returns its branch | `harness.TestAForkReturnsAndContinues` | not built |
| A thread's ceiling, from a call's `budget`, a subagent's `maxCost` or what is left of its caller's, counts the threads under it; at it the thread's turn ends `budget` with `thread_budget` and its caller's call names it; the session's ceiling still holds | `harness.TestAThreadsCeilingCountsWhatIsUnderIt` | not built |
| An inline subagent with no `model` resolves, and its thread runs its parent thread's model, connection and effort; a referenced agent still names its own | `manifest.TestASubagentMayLeaveItsModelOut`, `harness.TestASubagentWithoutAModelRunsItsParents` | not built |
| A `fork` trigger asks `session.fork` on `from` at apply, writes `status.forkPoint` once, and each firing forks `from` there with the trigger's title, `trigger_id` and `end_on_idle`; the run's fold at the fork point equals the conversation's | `internal/triggers.TestAForkTriggerForksItsConversation`, `internal/server.TestAForkTriggerAsksToForkAtApply` | not built |
| `fork` without `from`, `from` beside another policy, and a `fromSeq` that is no turn boundary are `invalid_manifest` with the field's path | `manifest.TestTheForkPolicysFields` | not built |
| A firing whose conversation is gone starts a new session whose message carries `fork_missing`, rendered as the line that says so | `internal/triggers.TestAForkWhoseConversationIsGoneStartsNew`, `session.TestTheFoldSaysTheConversationIsGone` | not built |
| The `trigger` tool's `startFrom: conversation` applies a `fork` trigger from the session, and its check names the tokens a run inherits and what reading them costs | `internal/proposal.TestAProposalCanStartFromTheConversation` | not built |
| The tool's description holds to `MaxForkTask` and names the tools a fork does not run | `harness.TestForkTextsReadTheirBounds` | not built |
| An instruction task: a task that splits into parts that each need what the conversation read is forked per part and combined; a part that needs nothing of it is spawned or done directly | `test/tasks/instructions/fork/` on the task suite's pinned model ([[025-task-suite]]) | not built |
| The OpenAPI document describes the Trigger's fields and `thread.started`'s members | `internal/server.TestTheDocumentDescribesSpec091` | not built |
