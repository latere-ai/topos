---
title: "A fork of the conversation: the `agent` tool's `subagent: fork` starts a subagent that continues its caller's conversation, sent on the caller's model and effort with the caller's tool definitions and system prompt byte for byte, so its first request reads the prefix its caller's last request cached; what a fork may run is narrowed when a call runs, not in what it is offered; one depth count for every subagent; a subagent's own spending ceiling over everything under it; a subagent that leaves its model out runs its caller's; and a trigger whose every session starts as a fork of the conversation that set it up"
status: in-progress
track: core
depends_on: [.archive/004-session-log.md, 005-harness-loop.md, .archive/007-models.md, 008-tools.md, 010-context.md, 012-permissions-and-approvals.md, 013-threads-and-subagents.md, 017-external-runners-handoff-fork.md, .archive/022-triggers.md, .archive/038-routed-models.md, 056-editing-a-message-and-the-branches-of-a-session.md, 086-proactive-agents.md, 087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report.md]
affects: [harness/, harness/tools/, session/, manifest/v1/, manifest/, manifest/trigger/, internal/triggers/, internal/server/, internal/proposal/, prompts/tools/, prompts/results/threads/, prompts/threads/, api/openapi.yaml, docs/]
effort: large
created: 2026-10-25
updated: 2026-10-29
author: changkun
---

# A fork of the conversation

## Overview

A fresh subagent of [[013-threads-and-subagents]] starts empty: its
first request holds its own instructions, its own tools and the task,
and nothing its caller knows. The caller writes what the subagent needs
into the task, and what it leaves out is lost. A conversation that has
read a repository for an hour cannot hand that reading to a fresh
subagent except by retelling it.

This spec adds the other value of the `agent` tool's `subagent` input,
`fork`, which starts a subagent that continues its caller's
conversation, as `fork` does among the subagent types of Claude Code's
`Agent` tool; the design's first table weighs it against a tool of its
own. A fork's first request is its caller's own next request with the
task appended: the same tool definitions, the same system prompt and
the same messages, byte for byte, on the same model at the same effort.
A provider that caches a prompt's prefix serves that request from the
cache its caller's last request wrote, so the fork starts with
everything the conversation holds and pays the cache-read price for it,
a fraction of the input price: a tenth, or less, on Anthropic's models.

Four rules follow from keeping that prefix, and two features come with
it:

1. **A fork is narrowed when a call runs, not in what it is offered.**
   Removing a tool from a fork's definitions would change the first
   bytes of its request and lose the cache. A fork holds its caller's
   definitions, and a call of a tool the fork may not run is refused
   with a code of its own.
2. **A fork runs its caller's model and effort.** A provider's cache
   belongs to one model, and a change of effort or reasoning parameters
   invalidates the cached conversation. A subagent on a cheaper model is a
   fresh one, not a fork.
3. **Fresh subagents and forks share one depth count**, with 013's
   default of 2 and its limit of 4.
4. **A subagent has a spending ceiling of its own** that counts every
   subagent under it. 013 specifies it; it is not built.
5. **A subagent may leave its model out** and run its caller's. Today a
   subagent names a model the installation's door must list, so an
   installation that routes models by a name only its authorizer
   resolves cannot give a session subagents at all.
6. **A trigger's sessions may start as a fork of the conversation that
   set it up**, so a routine's run knows what the conversation
   established without continuing one conversation that grows with
   every run.

## Current state

- `agent` (named `spawn` through v0.58.0, [[013-threads-and-subagents]])
  starts a thread of a subagent `spec.subagents` names, with the
  subagent's instructions, the intersection of the tools as
  `t.root.Subset(names)`, and an empty transcript but the task
  (`harness/threads.go`). `send_message` continues a subagent its
  caller started. Several calls in one step run at once, at most
  `spec.threads.maxConcurrent` (default 8); past it a call is answered
  `too_many_subagents` and is not queued.
- A thread's registry withholds `agent` and `send_message` at the depth
  limit (default 2, at most 4), and `agent` refuses past it with
  `depth_exceeded`. A subagent is never given `question`,
  `request_connection`, `publish`, `trigger`, `wake` or `notify`: they
  are left out of its definitions.
- A subagent's system prompt is rendered without the blocks that
  speak to the session's own agent: the initiator's instructions, the
  attached context, and the connections the person may be asked to
  connect (`headerBlocks` with `own` false, `harness/request.go`).
  `childConfig` drops the advisor, the question tool and `notify`.
- `agent`'s schema takes a `budget`, its description
  (`prompts/tools/agent-v1`) does not name it, and its `Run` does not
  read it: nothing reads `budget` until the ceiling below is built. No
  thread has a ceiling of its own: the harness checks the session's
  spend against the session's ceiling before each request
  (`checkBudget`), whichever thread sends it. `subagent.started` has a
  `budget` member that nothing writes.
- Through v0.58.0 every subagent's spec had to name `model.name`, and
  the subagent's model was connected by that name when the drive
  started, through no route of the authorizer: 038 routes the session's
  own model at its create, a change and a send, and 083 a title's. A
  name only the authorizer resolves reached the door unresolved, so a
  session whose agent named such a subagent could not start. After
  v0.58.0 an inline subagent may leave `model` out and runs its
  caller's (below); a subagent that names a model is still connected by
  that name (`manifest/config.go`, `internal/hosted/hosted.go`).
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
| The tool | a value of the `agent` tool's `subagent` input, `fork`, beside the names of the agent's subagents ([[013-threads-and-subagents]]): one input makes the one choice, fresh or fork, and every other input (`task`, `isolation`, `tools`, `budget`) means the same for both; it is the shape of Claude Code's `Agent` tool, whose `subagent_type` names a defined subagent or `fork`, so a model that knows that tool reads this one; and `send_message` continues both kinds | a tool of its own, `fork`, this spec's first draft: two tools that start a subagent split one decision across two definitions, and a model that knows the `Agent` tool looks for the fork among the subagents; the draft's reason, that a fork takes no agent, model or effort, no longer separates them, since `agent` takes no model or effort for any subagent and the input that differs is the choice itself; both have effect `none`, so one tool still has one effect, decided, narrowed and scored on its own. Leaving `subagent` out to fork: shorter, but a model that forgets the input gets a fork it did not choose, and the description reads better with both choices named side by side. A `context: fork` input beside `subagent`: two inputs for one choice, and a value of `subagent` the call must then leave out |
| How an agent offers forks | its `spec.tools` names `agent`, which adds `fork` to `subagent` and offers the tool to an agent with no subagents; `fork` is refused as a subagent's name at resolve | forks for every agent that has subagents: a default changes what each agent that never chose may do on upgrade, as the depth row says; a switch under `spec.threads`: a second place that turns a tool's use on, beside `spec.tools`, which already does |
| The fork's opening message | a result for every call of the caller's step, each the same fixed text, then the task in a text block after them | the task as the fork's own call's result: sibling forks of one step would differ from the first fork's result on, and share less of the prefix |
| Narrowing | when a call runs: a refusal at validation with `withheld_from_fork` | a narrowed list of definitions: the list is the first part of the cached prefix, so every narrowed fork would pay for the whole conversation again |
| The tools of the conversation with the person (`question`, `publish`, `trigger`, ...) | held in a fork's definitions and refused when called | run by the fork: the session's own thread holds the conversation with the person, and two threads asking at once is what 039 rules out |
| The model and the effort | the caller's, with no input to change them | a model per call: caches are per model, and a fork on another model is a fresh inline subagent, which already exists |
| The default depth | 013's 2, kept; an agent names `spec.threads.maxDepth: 3` to nest three levels, as Latere Chat's agent does | 3 as the core's default: a default applies to every agent of every installation that never chose, so raising it changes what each of them may do on upgrade; depth is not where spending is bounded (concurrency and ceilings are), and each level is one more hop at which a subagent's work is cut to its final text |
| Depth for fresh subagents and forks | one count: a subagent is one deeper than its caller, fresh or fork | two counts: a fork of a fresh subagent of a fork would escape both |
| A subagent's ceiling | counts the subagent and every subagent under it | its own requests alone: a fork that forks again would spend past it |
| A subagent with no model | runs its caller's model, connection and effort, at its first turn and at each later one (below) | routing every subagent's model through the authorizer: a new question at every start, and a subagent most often needs only the model the person chose |
| A routine's run from its conversation | a session fork of that conversation at the turn that set the routine up, every run | continuing the conversation (086's `session.target`): it grows by every run; a new session (today): it holds the ask and nothing the conversation established |
| A proposal's default | `startFrom: ask`, today's new session; the agent asks for `conversation` when the run needs it | `conversation` by default: a run's first request pays for the whole conversation at the full input price, since no cache lives from one run to the next |
| A conversation gone before a firing | the firing starts a new session and its message says so | skipping the run: the person deleted a conversation, not their routine |
| Names | `subagent: fork`, `withheld_from_fork`, `subagent_budget`, `startFrom` | `clone`: names a copy, not a continuation; `delegate`: v0.7.0's tool, retired; `thread_budget`: thread is the log's word, not one a model reads |

### A fork through the `agent` tool

A fork is a call of the `agent` tool ([[013-threads-and-subagents]])
whose `subagent` is `fork`. The value is offered when the agent's
`spec.tools` names `agent`, beside the names of its subagents, to every
thread below the depth limit, and the tool is then offered to an agent
with no subagents too. A fork itself carries its caller's definitions
whatever its depth (below).

| Input | For a fork |
|---|---|
| `subagent` | `fork` |
| `task` | what the fork does, at most `MaxForkTask` (8000) characters; the tool's check refuses a longer one `invalid_input`, and a fresh subagent's task, which carries everything that subagent knows, has no such bound |
| `isolation` | `shared` (default) or `worktree`, as for a fresh subagent |
| `tools` | optional: names narrowing what the fork may run, never what it is offered |
| `budget` | optional: a USD amount, the fork's ceiling (below) |

The tool takes no model and no effort for any subagent, so a fork has
none to choose: it runs its caller's. The tool's description says, in
one sentence each, that a fork continues this conversation on its
model and that a fork's `tools` narrow what it runs.

An `agent` call with `subagent: fork` appends `subagent.started` with the
members 013 gives it and three more:

| Member | Meaning |
|---|---|
| `context` | `fork` for a fork; absent for a fresh subagent |
| `from` | `{thread, seq}`: the caller's thread id (`""` for the session's own) and the sequence of the `agent.message` that holds the call |
| `grant` | the tools the fork may run after narrowing, beside `tools`, which for a fork lists the definitions it holds |

and runs the fork's first turn on the same runner and machine. Its
result is any `agent` call's ([[013-threads-and-subagents]]): the
fork's final text and the line that says it takes more work, the branch
line for an isolated fork, and an error naming any other stop.
`send_message` continues a fork as it continues a fresh subagent, and
`end` ends it.

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
2. the opening message, rendered from `subagent.started`;
3. the events whose `subagent` is the fork's, by the rules of any thread.

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
| the tools of the conversation with the person | removed: `question`, `request_connection`, `publish`, `trigger`, `wake`, `notify`, and every tool a later spec reserves for the session's own thread ([[033-peers-and-authored-graphs]]'s `run_workflow`, [[092-a-persons-sessions-that-see-each-others-work]]'s `list_sessions` and `set_working_on`); and `send_message` to another session, which 092 refuses a fork as `withheld_from_fork` while a fork's message to its own subagent runs |
| the depth limit | `agent` removed for a fork at the limit; a call of it there is answered `depth_exceeded` |

A call of a tool the fork holds and may not run is answered at
validation, as an unknown tool is ([[005-harness-loop]]): outcome
`error`, the text `withheld_from_fork:` and `prompts/results/threads/withheld.md`,
which names the tool and says what to do instead (end the turn saying
what needs the person, for a tool of the conversation with the person).
It gets no `agent.tool_use` and nothing runs. It counts in the
repeated-call rule of [[005-harness-loop]] as every refusal does, so a
fork that keeps calling it ends its turn.

Every call the grant allows is decided as a fresh subagent's are: the
caller's mode, lists and thresholds, narrowed as 013's table says, with
the session's credentials and machine.

### Depth and concurrency

A subagent's depth is its caller's plus one, fresh or fork. The limit
stays 013's: 2 unless `spec.threads.maxDepth` says otherwise, at most
4. Of 013's two gates, the first, `agent` not offered at the limit,
applies to every subagent but a fork, which carries
its caller's definitions; the second, a call past the limit refused
with `depth_exceeded`, applies to all. Forks count toward
`spec.threads.maxConcurrent` as fresh subagents do.

### A subagent's ceiling

A subagent may have a ceiling of its own, in USD micros, the least of:

| Source | Ceiling |
|---|---|
| the call's `budget` | that amount |
| for a fresh subagent, its `spec.budget.maxCost` | that amount |
| the caller's ceiling, when it has one | what is left of it when the subagent starts |

A thread's spend is the cost of every `model.request`, and every tool
result a service charged for ([[007-models]]), whose `subagent` is the
subagent or one under it. Before each request, the harness checks
the thread and each thread above it that has a ceiling: its spend, plus
the next request's input estimate of 007's check, against its ceiling.
At a ceiling, the requesting thread's turn ends with stop reason
`budget` and detail `subagent_budget`, and the call that drove it is
answered with the error that names it. The session's own ceiling still
holds over every thread. `subagent.started` records the ceiling as
`budget: {max_cost_usd_micro}`, absent when the thread has none.

The rule is the same for fresh subagents and forks, and is the one
[[033-peers-and-authored-graphs]] holds a workflow's subagents to.

### A subagent that runs its caller's model

An inline subagent's spec may leave `model` out. It runs the model its
caller's thread stands on, with that thread's connection, figures and
effort. The session's permissions already name that model, so its
create's question is unchanged. A referenced agent keeps its own model,
and a subagent that names a model is connected by that name, as today.
`subagent.started` records the model the subagent started on in `model`
and records no effort; each `model.request` records the model it was
sent with, and the request it keeps holds the effort.

What such a subagent runs at a turn after its first, a `send_message`
to it:

| | Built after v0.58.0 | Recommended |
|---|---|---|
| model | the model it started on, connected again when its caller has moved to another since; when that model cannot be connected, the call's result is `Subagent <id> stopped: error <code>` | the model its caller stands on at that turn, with its connection |
| effort | its caller's at that turn, since nothing records the effort it started at | the same, unrecorded |

| Item | Recommended | Weighed against, and why not |
|---|---|---|
| The model at a later turn | the caller's model at that turn: it is always one the session may use, so following it reaches nothing the session does not hold, needs no second connection, and goes where the person or the authorizer moved the session | the model the subagent started on (built): after a failover within a turn ([[051-a-turn-moves-off-a-model-that-cannot-serve]]), a subagent started before it goes back to the model that failed; on the hosted runner, after the authorizer moves the session to another model, the old one cannot be connected once the session's key no longer lists it, and the call stops with an error. Falling back to the caller's model only when the old one fails to connect: two paths, and a model that failed over may still connect and fail again at its first request |
| The effort | follows the caller's current effort, unrecorded, as built | recorded in `subagent.started` and kept for the subagent's life: a member and a pin that keep a subagent at a level the person has since changed, while its model already follows the caller |

A change of model between a subagent's turns is encoded as
[[007-models]] encodes a change of the session's model to another
family, a translation; the family a thread pins when it starts (013)
stays pinned for a subagent that names its model. A fork follows the
same rule: its first request is sent within its caller's step, on the
model and effort of that step, so the cached prefix holds; a later turn
of a fork follows its caller as any subagent does, and reads from the
cache only when the caller has not moved.

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
| `subagent: fork` | a value of the `agent` tool's input ([[013-threads-and-subagents]]) | its inputs and result |
| `subagent.started.context`, `.from`, `.grant` | event members | a fork's |
| `subagent.started.budget` | event member, written from now | a subagent's ceiling |
| `subagent_budget` | `detail` of stop reason `budget` | a subagent's ceiling reached |
| `withheld_from_fork` | error code | a fork's call of a tool outside its grant |
| `MaxForkTask` | constant, 8000 | the most characters a fork's `task` holds, which the `agent` tool's check holds it to |
| `session.policy: fork`, `session.from`, `session.fromSeq`, `status.forkPoint` | Trigger fields | a routine's run from its conversation |
| `fork_missing` | `user.message` member | a firing whose conversation is gone |
| `startFrom` | `trigger` tool input | a proposal's choice |
| the next versions of `prompts/tools/agent` after 013's `agent-v1`: the one shipped with step 1 names `budget`, the one shipped with step 2 names `fork`; `prompts/threads/fork-v1.md`, `prompts/results/threads/forked.md`, `prompts/results/threads/withheld.md` | texts | the description, the opening, the placeholder result, the refusal |

### Roll order

1. A subagent that leaves its model out, and a subagent's ceiling for
   fresh subagents, with an `agent` description that names `budget`.
   They close 013's open budget row and let an installation that
   routes its models give a session subagents. The model-less subagent
   is built after v0.58.0, with its later turns on the model it started
   on; its later turns on the caller's model, and the ceiling, are
   open.
2. After 013's rename to `agent` and `send_message` has shipped,
   `subagent: fork`: the value and `spec.tools` naming `agent`, the
   opening message, the fold, the grant and `withheld_from_fork`, and
   the instruction task.
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
| The managed agent's definition carries subagents and `threads`, and a version names a subagent that leaves its model out | step 1: subagents for an agent that runs on a routed name |
| A later version names `agent` among its tools, so its subagents include `fork`, with instructions on when a fork beats a fresh subagent (the task needs what the conversation read) and when neither is worth it | step 2 |
| The authorizer answers `session.fork` asked for a firing as it answers a firing's create | step 3 |

## What the Go SDK needs

| Need | Why |
|---|---|
| A routine's `startFrom`, read and written, and kept through a change | a client shows whether a routine's runs start from the conversation, and offers the choice |
| A subagent's kind (fresh, or `fork` by `subagent.started`'s `context`), its caller and its ceiling, read from `subagent.started` | a client shows the subagents a conversation started and what each may spend |

## Not in this spec

A fork that runs while its caller goes on (every subagent holds the
call that drove it); a fork on another model; a fork moved to a
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
| A fork at the depth limit holds `agent` in its definitions and a call of it is `depth_exceeded`; a fresh subagent at the limit is not offered it; fresh subagents and forks count one depth | `harness.TestOneDepthForEverySubagent` | not built |
| Forks count toward `maxConcurrent` | `harness.TestForksCountTowardTheConcurrencyCap` | not built |
| A fork's fold is its caller's through `from.seq`, the opening message and its own events; a caller's later compaction leaves it; a redaction of an inherited event compacts it before its next request; a runner restarted during a fork rebuilds the same request | `session.TestAForksFold`, `harness.TestARedactionCompactsEveryForkThatHoldsIt`, `harness.TestAForkResumesOnAnotherRunner` | not built |
| A fork's result is its final text and the idle line, `send_message` continues it, and an isolated fork returns its branch | `harness.TestAForkReturnsAndContinues` | not built |
| A subagent's ceiling, from a call's `budget`, a fresh subagent's `maxCost` or what is left of its caller's, counts the subagents under it; at it the subagent's turn ends `budget` with `subagent_budget` and its caller's call names it; the session's ceiling still holds; the `agent` description names `budget` | `harness.TestASubagentsCeilingCountsWhatIsUnderIt` | not built |
| An inline subagent with no `model` resolves, and its first turn runs its caller's model, connection and effort; a referenced agent still names its own | `manifest.TestASubagentMayLeaveItsModelOut`, `harness.TestASubagentWithoutAModelRunsItsCallers` | built |
| A subagent with no `model` runs each later turn on the model, connection and effort its caller stands on then: after a failover within a turn, or after the authorizer moved the session to a model the session's key now lists alone, it runs the caller's new model, connects nothing else, and its `model.request` records that model | `harness.TestASubagentWithoutAModelFollowsItsCaller` | not built |
| A `fork` trigger asks `session.fork` on `from` at apply, writes `status.forkPoint` once, and each firing forks `from` there with the trigger's title, `trigger_id` and `end_on_idle`; the run's fold at the fork point equals the conversation's | `internal/triggers.TestAForkTriggerForksItsConversation`, `internal/server.TestAForkTriggerAsksToForkAtApply` | not built |
| `fork` without `from`, `from` beside another policy, and a `fromSeq` that is no turn boundary are `invalid_manifest` with the field's path | `manifest.TestTheForkPolicysFields` | not built |
| A firing whose conversation is gone starts a new session whose message carries `fork_missing`, rendered as the line that says so | `internal/triggers.TestAForkWhoseConversationIsGoneStartsNew`, `session.TestTheFoldSaysTheConversationIsGone` | not built |
| The `trigger` tool's `startFrom: conversation` applies a `fork` trigger from the session, and its check names the tokens a run inherits and what reading them costs | `internal/proposal.TestAProposalCanStartFromTheConversation` | not built |
| `subagent` holds `fork` only for an agent whose `spec.tools` names `agent`, which is offered the tool with no subagents too | `harness.TestForkIsOfferedWhenTheAgentNamesTheTool` | not built |
| The `agent` tool's description names `fork` beside the subagents, says in one sentence each that a fork continues the conversation on its model and that its `tools` narrow what it runs, holds to `MaxForkTask`, and names the tools a fork does not run; a fork's longer task is `invalid_input`, and a fresh subagent's task has no such bound | `harness.TestForkTextsReadTheirBounds`, `harness.TestAForksTaskIsBounded` | not built |
| An instruction task: a task that splits into parts that each need what the conversation read is forked per part and combined; a part that needs nothing of it goes to a fresh subagent or is done directly | `test/tasks/instructions/fork/` on the task suite's pinned model ([[025-task-suite]]) | not built |
| The OpenAPI document describes the Trigger's fields and `subagent.started`'s members | `internal/server.TestTheDocumentDescribesSpec091` | not built |
