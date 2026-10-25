---
title: "Workflows: a graph of phases that an agent writes or a manifest declares, run as threads of the session that starts it, estimated before it starts as a range from the recorded cost of comparable threads, held for the person's yes with that range and a ceiling, kept to the ceiling while it runs, and ended by an interrupt; message edges between its nodes are left for later"
status: drafted
track: core
depends_on: [.archive/003-manifest.md, .archive/004-session-log.md, .archive/006-identity.md, 005-harness-loop.md, .archive/007-models.md, 008-tools.md, 012-permissions-and-approvals.md, 013-threads-and-subagents.md, 025-task-suite.md, 039-questions.md, 086-proactive-agents.md, 087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report.md, 091-a-fork-of-the-conversation.md]
affects: [harness/, harness/tools/, session/, manifest/v1/, manifest/, internal/server/, internal/store/, internal/runnerapi/, prompts/, api/openapi.yaml, docs/]
effort: large
created: 2026-09-27
updated: 2026-10-25
author: changkun
---

# Workflows

## Overview

Some work is too large to hand out one task at a time: review eighty
contracts, check every page of a site, try the same change in every
package of a repository. A session can do it today only as an
orchestrator does ([[013-threads-and-subagents]]): its model calls
`spawn` several times in one step, at most eight threads at once, reads
the results, and calls again, step after step. Nothing says before the
first spawn what the whole will cost, nothing asks the person, and
nothing stops it short of the session's budget.

A workflow is that work written down before it runs: a graph of
phases, each a list of tasks, one task per item of a list, or one task
that combines what earlier phases returned. The agent writes it in a
`workflow` call, or names one an author declared as a manifest. Before
anything runs, Topos estimates its cost as a range from the recorded
cost of comparable threads, and the call is held for the person with
the range and a ceiling. On their yes the run starts: every node is a
thread of the session, a fork of the calling conversation
([[091-a-fork-of-the-conversation]]) or a spawn of a subagent, so the
log, the fold, narrowing, depth and the advisor apply unchanged. The
run is held to its ceiling, an interrupt ends it, and its result is the
call's.

Where the run starts in a turn nobody attends, a routine's or a wake's
([[086-proactive-agents]]), the held call waits for the person like
any other held call, so the workflow is offered rather than started.

Peers, message edges declared between sibling threads, were this
spec's first stage. They are left out of this revision: no suite task
and no caller needs them yet, and a workflow's phases pass results
forward without them.

Each choice below is a recommendation until this spec is validated;
the table that opens the design weighs each against its alternative.

## Current state

- Threads, `spawn`, `message`, worktree isolation, the depth limit and
  the advisor are built ([[013-threads-and-subagents]]). Several spawns
  in one step run at once up to `maxConcurrent` (8); a spawn past it is
  answered `too_many_threads` and is not queued, so a fan-out of fifty
  is batched by the model by hand.
- A thread's cost is in the log: every `model.request` carries its
  `thread` and `cost_usd_micro`, and `thread.ended` sums them. Nothing
  indexes it across sessions. A trigger's runs are recorded with their
  cost ([[088-a-triggers-runs-and-fire-times-read-for-a-forecast]]);
  no other unit of work is.
- No thread has a ceiling of its own; [[091-a-fork-of-the-conversation]]
  adds one.
- A call that a person approves every time is a `tools.Confirmer` call,
  held in every mode past every list, pattern and decision service, and
  blocked in `plan` ([[012-permissions-and-approvals]]); a
  `tools.Prober` asks the service a call would reach before it is held
  ([[086-proactive-agents]]). A question in a session that is not
  attended is answered at once ([[039-questions]]).
- Rollouts ([[077-rollouts]]) run N unattended sessions of one agent on
  one task for an evaluation, each its own machine; a session does not
  start one.
- The manifest has no `Workflow` kind, and an Agent names no workflow.

## Design

### Recommendations

| Item | Recommended | Weighed against, and why not |
|---|---|---|
| Where a run's nodes run | as threads of the session that started it, on its machine, under its log and lease | as sessions of their own, as rollouts are: a machine and a lease per node, which scales past one machine, but a run of two hundred nodes would ask for two hundred sandboxes; kept for a later placement when a caller needs it |
| What a workflow is | a declared graph of phases: a list of tasks, one task per item of a list, or one task that combines | a program the agent writes, as Anthropic's Managed Agents' dynamic workflows are: a program's cost is not known until it runs, so nothing could be estimated or bounded per phase before the person's yes, and the runner has no interpreter to run agent-written code outside the machine |
| Who writes it | the agent, in the call; or an author, as a `Workflow` manifest the agent names | authored only: most fan-outs are shaped by the request in front of the agent |
| How the person says yes | the `workflow` call, held by `tools.Confirmer` for the session's initiator, with the estimate recorded beside it | a `question` and then the call: a question in a session nobody attends is answered at once, and a yes to a question binds to an answer the model then acts on, not to the graph and the ceiling the person read; the held call runs exactly what was approved, the choice [[086-proactive-agents]] made for a proposal |
| A turn nobody attends | the held call waits, idle `tool_confirmation`, and the turn's outcome is `notified`, so a client lists it among what waits for the person | refusing it: the work the agent found would be lost; starting it under an amount the person set beforehand: left open (Not in this spec) |
| Who estimates | Topos, from the graph's counts and the recorded cost of comparable threads, which it holds; the installation's authorizer refuses or lowers the ceiling | the authorizer alone: it holds neither the graph nor the threads' costs; the agent's own estimate: a model's guess where a record exists |
| The estimate's shape | a range: low and high, with how many runs it reads; no high end before enough runs exist, and then the call names its ceiling | a point: a fan-out's cost varies by a factor of several between items |
| The ceiling | the high end of the range unless the call names one; the authorizer may lower it; enforced over the run's threads by 091's ceiling | the estimate as advice: a run past its estimate would spend what nobody approved |
| Concurrency | the run's own bound, `maxAtOnce`, default 8, at most 32; nodes do not count toward the session's `maxConcurrent` | the session's 8: a workflow is the way past it, approved for it |
| The call and the turn | the call holds the caller's turn until the run ends, as `spawn` does | a run that goes on while the session waits for its next message: a state of the session no client reads today, and a person's message during it would interleave a second conversation with the run; left for later |
| Cancel | an interrupt: running nodes are canceled as a turn's running calls are, waiting nodes never start, and the call's result is the partial report | a cancel route: the run is the turn, and an interrupt already ends a turn |
| A node that fails | recorded failed with its stop reason; the run goes on, and a combining node is told | failing the run: one item of eighty would lose the other seventy-nine |
| The calling thread | the session's own thread alone | any thread: a node starting a run of its own multiplies the person's yes; a workflow is the conversation's |
| Peers | left out of this revision | declared edges between sibling threads: no task and no caller needs them |

### The `workflow` tool

`workflow` is a harness tool, as `question` is: added to the session's
own thread when the agent's `spec.tools` names it. A fork carries its
definition and is refused it ([[091-a-fork-of-the-conversation]]), and
a spawned thread is not offered it.

| Name | Parallel | Effect | Input |
|---|---|---|---|
| `workflow` | no | `none` | the table below |

| Input | Meaning |
|---|---|
| `graph` | a graph (below); one of `graph` and `name` |
| `name` | the name of a `Workflow` the agent's `spec.workflows` lists |
| `inputs` | for `name`: a value for each of the workflow's inputs |
| `maxCostUsd` | the ceiling asked, a decimal USD amount above zero with at most six fraction digits; absent, the estimate's high end, and required when the estimate has none |

A call passes four steps before anything runs:

1. **Check** (`tools.Checker`): the graph against its schema and its
   bounds, below. A failure is answered `invalid_input` with the field.
2. **Estimate**: the range and the ceiling, below.
3. **Probe** (`tools.Prober`): `session.workflow` asked of the
   authorizer, below. A deny is the call's result, with no
   `agent.tool_use`, and nobody is asked. An allow may lower the
   ceiling. The estimate, with the ceiling the allow left, is then
   appended as `workflow.estimate` in the batch of the call's
   `agent.tool_use`.
4. **Hold** (`tools.Confirmer`): the call waits for the session's
   initiator in every mode, whatever list, pattern or decision service
   would allow it, and is blocked in `plan`. Only the initiator's
   `user.tool_confirmation` closes it; another sender's is refused
   `forbidden`. A deny may carry the person's words as its note, which
   the model reads in the result ("make it twenty contracts").

On the allow the runner asks `session.workflow` again with
`confirmed_by`, since the answer may have changed while the person
decided, and starts the run.

### The graph

| Field | Meaning | Bounds |
|---|---|---|
| `name` | one line the person reads | at most `MaxWorkflowName` (80) characters |
| `phases` | the phases, run in order | 1 to `MaxPhases` (8) |
| `maxAtOnce` | threads of the run running at once | default `DefaultWorkflowAtOnce` (8), at most `MaxWorkflowAtOnce` (32) |
| `timeout` | a Go duration | default and at most what is left of the turn's timeout ([[005-harness-loop]]) |

Each phase:

| Field | Meaning |
|---|---|
| `id` | `[a-z][a-z0-9-]{0,31}`, unique in the graph |
| `name` | one line, at most 80 characters |
| `run` | `fork`: each node is a fork of the calling thread, made at the call; or a name of `spec.subagents`: each node is a spawn of it |
| `tasks` | a list of task texts, one node each |
| `each` | one node per item: `{items: [..], task}` with a literal list of strings, or `{from: <id>, maxItems: n, task}` with the items an earlier phase returned, at most `maxItems`; `task` names the item as `{{item}}` |
| `combine` | one node: `{from: [ids], task}`; its task is followed by every output of the listed phases, each under the line that names its phase and item |
| `returns` | `text` (default): a node's output is its final text; `items`: its final text is a JSON array of strings, which a later `each` reads |
| `isolation`, `tools` | as a spawn's or a fork's |
| `maxCostPerNodeUsd` | optional: each node's ceiling, 091's thread ceiling |

Exactly one of `tasks`, `each` and `combine`. A phase reads only
phases before it. The graph's nodes, counting an `each` over an earlier
phase at its `maxItems`, are at most `MaxNodes` (200). A task text is at
most `MaxForkTask` characters ([[091-a-fork-of-the-conversation]]).

A node of a phase that `returns: items` whose final text is not a JSON
array of strings of at most the reading phase's `maxItems` is sent one
`thread.message` of `prompts/results/workflow/items.md`, which says what
was wrong and asks for the array alone; a second failure records the
node failed. An output longer than `MaxNodeOutput` (16 KiB) is cut where
a combining node reads it, and the cut is said.

Fork nodes all fork the calling thread at the `agent.message` that
holds the `workflow` call. Every one of them, in every phase, has the
same prefix and differs in its task alone, so each first request reads
the calling conversation from the provider's cache while starts keep
it alive ([[091-a-fork-of-the-conversation]]).

### The estimate

Topos keeps a record of each thread's first piece of work:

| Field | Meaning |
|---|---|
| `session_id`, `thread_id` | the thread |
| `owner` | the session's owner |
| `kind` | `spawn`, `fork` or `node` (a workflow's) |
| `agent` | the agent version the thread runs, by digest: the subagent's for a spawn, the session's agent's for a fork |
| `model` | the model the thread ran |
| `inherited_tokens` | for a fork, the tokens its first request inherited; 0 otherwise |
| `cost_usd_micro`, `steps`, `outcome` | what the thread's work cost through the result of the call that started it, how many steps it took, and how it ended (`ok`, or the stop reason) |
| `at` | when it started |

The server writes a row where it folds the batch holding that result,
and deletes rows older than `ThreadRunsKept` (6 weeks) at the minute
loop's pass, as [[088-a-triggers-runs-and-fire-times-read-for-a-forecast]]
prunes runs.

A phase's comparable runs are the latest `EstimateRuns` (20) rows of
the owner with the phase's agent, model and kind (`fork` and `node`
compare with each other for a fork phase, `spawn` and `node` for a
spawn phase), and, for a fork phase, an `inherited_tokens` within a
factor of two of the calling thread's context now
([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]'s
`context_report`). With at least `EstimateMinRuns` (5), a node costs
from the 25th to the 90th percentile of their `cost_usd_micro`.

| A phase's nodes | Counted |
|---|---|
| `tasks` | the list's length, both ends |
| `each` over items | the list's length, both ends |
| `each` from a phase | 0 to `maxItems` |
| `combine` | 1 |

The estimate is the sum over phases: low, the fewest nodes at the 25th
percentile; high, the most at the 90th. A phase with fewer than five
comparable runs adds to the low end only its nodes' first requests (a
fork's inherited tokens at the cache-read price and its task at the
input price; a spawn's instructions and task at the input price), and
leaves the estimate with no high end: the call then names its ceiling,
or is answered `invalid_input` before the hold, saying that nothing yet
records what such a run costs.

`workflow.estimate`:

| Member | Meaning |
|---|---|
| `tool_use_id` | the call |
| `low_usd_micro`, `high_usd_micro` | the range; `high_usd_micro` absent when it has no high end |
| `ceiling_usd_micro` | the call's `maxCostUsd`, or the high end |
| `nodes_min`, `nodes_max` | the graph's node counts |
| `phases` | per phase: `id`, `run`, `model`, `nodes_min`, `nodes_max`, `runs` (the comparable runs read) |

The tool's description tells the model that the person reads the range
and the ceiling before anything runs, that a run stops at the ceiling,
and to keep a workflow to work with many independent pieces: one
question at a time is answered directly, and a few pieces are spawned
or forked.

### The authorizer

`session.workflow` is a new action of the vocabulary
([[006-identity]]):

| Question | Fields |
|---|---|
| `session.workflow` | `agent`, `owner`, `session_id`, `origin: {session_id, tool_use_id, confirmed_by?}`, `attended` (whether the turn is attended, [[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]), `phases` as `workflow.estimate` has them, `estimate: {low_usd_micro, high_usd_micro?}`, `ceiling_usd_micro` |

It is asked as the session's initiator, in the session's context, the
way 086's trigger questions are. Its allow may carry `budget_usd_micro`
in its limits, a lower ceiling for the run, which `workflow.estimate`
then records as the ceiling the person reads. The owner policy allows an
initiator their own session's runs. An authorizer that does not know
the action denies it, and the call is answered with the refusal.

### The run

On the allow the runner appends `workflow.started` and runs the phases
in order. A phase starts its nodes, at most `maxAtOnce` of the run's at
once, and ends when every node has an output or has failed.

| Event | Appended by | Payload |
|---|---|---|
| `workflow.estimate` | the harness, with the call's `agent.tool_use` | above |
| `workflow.started` | the runner, on the allow | `tool_use_id`, `name`, `phases` (id, name, run, nodes planned), `ceiling_usd_micro`, `max_at_once`, `deadline`, `confirmed_by` |
| `workflow.phase_ended` | the runner | `run`, `phase`, `ok`, `failed`, `items` (the items returned, for `returns: items`) |
| `workflow.ended` | the runner | `run`, `reason`: `completed`, `budget`, `interrupted`, `timeout` or `failed`; `spent_usd_micro`; the counts |

`run` is the id of `workflow.started`. Each node's `thread.started`
gains `workflow: {run, phase, index, item?}`, and its depth is the
calling thread's plus one, so a node may spawn or fork under the depth
limit, and what that spends is the run's.

The run's state is folded from these events and the nodes' threads. A
runner that takes the session over resumes it: a node whose turn had
ended is not run again (013's rule for a thread that finished), a node
mid-turn continues, a node not started starts.

| What happens | The run |
|---|---|
| a node's turn ends `end_turn` | its output is its final text, or its items |
| a node's turn ends for any other reason | the node is failed, with its stop reason; the run goes on |
| a node waits for a person (a held call of its own) | the session pauses, as 013 pauses it for any thread, and the run waits with it |
| the run's spend reaches its ceiling | the node whose request reached it ends `budget` with detail `workflow_budget`; no node starts; running nodes stop at their next check; `workflow.ended` `budget` |
| a `user.interrupt` | running nodes are canceled as a turn's calls are ([[005-harness-loop]]); `workflow.ended` `interrupted`; the call's result is `canceled` with the partial report |
| the run's `timeout` passes | running nodes are canceled; `workflow.ended` `timeout` |
| every node of a phase failed | `workflow.ended` `failed` |

The run's spend is the cost of every `model.request` and charged tool
result of its nodes and the threads under them; the run's ceiling is
checked before each of their requests as a thread's ceiling is
([[091-a-fork-of-the-conversation]]). The session's own ceiling still
holds.

The call's result is the run's report: the outputs of its last phase,
every node's in order or the combining node's, a line per failed node
with its stop reason, and what the run spent of its ceiling. A long
report is spilled to a file as any long result is ([[008-tools]]).

### Workflows an author declares

`manifest/v1` gains a `Workflow` kind ([[003-manifest]]):

| Field | Meaning |
|---|---|
| `metadata.name` | the name an agent names it by |
| `spec.description` | one line the model reads in the tool's description |
| `spec.inputs` | named inputs, each `{name, description}`; a task or an item names one as `{{input.<name>}}` |
| `spec.graph` | a graph, as above |

An Agent's `spec.workflows` lists the workflows its sessions may run,
by name, resolved and pinned with the agent version as a subagent
reference is. The tool's description lists them with their
descriptions and inputs. A named workflow is estimated, held and run as
an inline graph is, and `workflow.started` names it.

### Constants

Constants of the `harness` package, read by the tool's schema, its
description and its check, so none is written twice:

| Constant | Value | Why this value |
|---|---|---|
| `MaxPhases` | 8 | a plan, a pass, a check and a combination fit with room |
| `MaxNodes` | 200 | a run a person can read the report of; past it, the work is an evaluation's rollouts |
| `DefaultWorkflowAtOnce` | 8 | the session's `maxConcurrent` default |
| `MaxWorkflowAtOnce` | 32 | the threads one machine and one runner drive at once without one turn's calls starving another's |
| `MaxWorkflowName` | 80 | one line in a list |
| `MaxNodeOutput` | 16 KiB | eighty outputs fit a combining node's request |
| `EstimateRuns`, `EstimateMinRuns` | 20, 5 | enough rows for a 90th percentile to mean something, few enough to follow a changed agent within days |
| `ThreadRunsKept` | 6 weeks | as a trigger's runs are kept |

### Roll order

1. The thread record and the estimate, alone: every spawn and fork is
   recorded, so the estimate has runs to read when the tool ships.
2. The `workflow` tool with inline graphs: the check, the estimate,
   `session.workflow`, the hold and the run.
3. The `Workflow` kind and `spec.workflows`.

An installation's authorizer learns `session.workflow` before the
release of step 2 reaches it.

## What a hosted installation needs

| Need | Why |
|---|---|
| The authorizer answers `session.workflow`: the initiator's own session, a ceiling within what the person can spend, and for a turn nobody attends the share of credit proactive work draws on | the person approves a cost they can pay |
| The managed agent names `workflow`, with instructions on when work calls for one and when it does not | the agent starts runs only for work that needs them |
| A client shows the held call as an offer: the run's name, its phases and node counts, the range and the ceiling, from `workflow.estimate`; and a run's progress from its events | the person decides on what they read and follows what runs |

## What the Go SDK needs

| Need | Why |
|---|---|
| A held `workflow` call read as an offer, with its `workflow.estimate` | both clients word the offer alike |
| A run's events and its nodes' threads, grouped by `run` | both clients show a run's progress |

## Not in this spec

A run that goes on while the session waits for its next message; nodes
placed as sessions of their own; message edges between a run's nodes;
a workflow started without the person's yes, under an amount they set
beforehand; a node on another machine; the words a client shows.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A graph past a bound (phases, nodes, `maxAtOnce`, a name, a task, an id, a phase reading a later one, an `each` from a phase without `maxItems`) is answered `invalid_input` naming the field, and nothing is held | `harness.TestAWorkflowsGraphIsChecked` | not built |
| Every spawn and fork records its thread run with its cost, steps and outcome, in every store, pruned after `ThreadRunsKept` | the stores' conformance case `thread runs`, `internal/server.TestEveryThreadRecordsItsRun` | not built |
| The estimate sums each phase's node counts at the 25th and 90th percentiles of its comparable runs; a phase with fewer than five has no high end, and a call without a ceiling is then refused before the hold | `harness.TestAWorkflowsEstimate` | not built |
| A `workflow` call is held in `confirm` and `progressive` whatever a list, a pattern or a decision service says, blocked in `plan`, and closed only by the initiator's confirmation | `harness.TestAWorkflowIsHeldForItsPerson`, `internal/server.TestOnlyTheInitiatorStartsAWorkflow` | not built |
| `session.workflow` is asked before the hold with the estimate and the ceiling, and again on the allow with `confirmed_by`; a deny before the hold asks nobody; an allow's `budget_usd_micro` lowers the ceiling | `internal/server.TestTheWorkflowQuestion`, `authorizer.TestVocabularyMatchesTheSpec` | not built |
| On the allow, `workflow.started` is appended and the phases run in order; `tasks`, `each` over items, `each` from a phase and `combine` start the nodes the table says, at most `maxAtOnce` at once | `harness.TestAWorkflowRunsItsPhases` | not built |
| A node that returns items not in the shape asked is asked once more, and failed after a second miss | `harness.TestANodesItemsAreAskedForOnce` | not built |
| A failed node leaves the run going, and the combining node reads the failure | `harness.TestAFailedNodeLeavesTheRun` | not built |
| At the ceiling the run starts no node, ends `budget`, and its report names what it spent; an interrupt ends it `interrupted` with a `canceled` result carrying the partial report; past its timeout it ends `timeout` | `harness.TestAWorkflowStopsAtItsCeiling`, `harness.TestAnInterruptEndsAWorkflow`, `harness.TestAWorkflowsTimeout` | not built |
| A runner that takes the session over during a run runs no finished node again and finishes the run | `harness.TestAWorkflowResumesOnAnotherRunner` | not built |
| The fork nodes of a run share the calling thread's prefix and differ in their task alone | `harness.TestAWorkflowsForksShareTheCallersPrefix` | not built |
| A `workflow` call in a turn a trigger began waits for the person, and the turn's outcome is `notified` | `harness.TestAWorkflowInAnUnattendedTurnWaits` | not built |
| A fork's `workflow` call is `withheld_from_fork`, and a spawned thread is not offered the tool | `harness.TestOnlyTheConversationStartsAWorkflow` | not built |
| A `Workflow` manifest resolves, an agent's `spec.workflows` pins it, and a call by `name` fills its inputs and runs as an inline graph | `manifest.TestAWorkflowKind`, `harness.TestANamedWorkflowRuns` | not built |
| An instruction task: a review of many independent items is proposed as a workflow with a ceiling, and a request about one item is answered without one | `test/tasks/instructions/workflow/` on the task suite's pinned model ([[025-task-suite]]) | not built |
| The OpenAPI document describes the events, the members and the Workflow kind | `internal/server.TestTheDocumentDescribesSpec033` | not built |
