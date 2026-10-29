---
title: "What a session has cost so far and how far its to-do list is, read from its stream: the budget counts the log through last_seq and each model.request and tool.result carries what it added, so no event of its own reports the spending; a todo call's result records the list, described as TodoResultMeta; and no progress count beyond the to-do list until workflow runs count their nodes"
status: testing
track: core
depends_on: [008-tools.md, 023-events-and-observability.md, 047-web-search.md, 056-editing-a-message-and-the-branches-of-a-session.md, 082-a-pdf-read-as-text-through-a-parsing-service.md, 033-peers-and-authored-graphs.md]
affects: [internal/server/, api/openapi.yaml, docs/]
effort: small
created: 2026-10-30
updated: 2026-10-30
author: changkun
---

# What a session has cost so far and how far its to-do list is

## Overview

A client that follows a session while it works shows two things beside
the conversation: what the session has cost so far, and how far the
agent is through its work. Both must stay exact while the session runs,
from the stream alone, without the client reading the session again
after every step.

The log already carries both. This spec states the contracts a client
builds on, describes the one record the document left undescribed, and
decides what is not built yet:

1. **The cost so far.** A session's `budget.spent_cost_usd_micro` is the
   cost of its log through `last_seq`, and every event that adds to it
   carries what it added. A client reads the session once and adds each
   later event's cost. No event of its own reports the spending.
2. **The to-do list.** A `todo` call's `tool.result` records the whole
   list in its meta. The document describes it as `TodoResultMeta`.
3. **Progress without a to-do list.** Not built now: the reasons, and
   what would change the decision, are below.

## Current state

- `session.Spent` is the budget meter of spec 007: the cost of every
  `model.request`, the session's own and each subagent's, and of every
  `tool.result` a service charged for, a web search's (spec 047) and a
  PDF's parse (spec 082), a redacted result's from its tombstone. Every
  store folds the same sum into the session's budget in the batch that
  moves `last_seq`, so a read's spending is the cost of its log through
  its `last_seq`.
- A fork copies its parent's events, and its budget holds what they cost
  apart as `carried_cost_usd_micro` (spec 056); its own spending, which
  its cap holds, is spent less carried.
- The `todo` tool (spec 008) replaces the conversation's list on each
  call and records the list in its result's meta as `todos`, items
  `{id, content, status}`; only a call that succeeded records it, and an
  emptied list is `[]`.
- The document described the meta of `read`, `question` and `web_search`
  in schemas of their own, and not the `todo` call's, so a client read
  the list from the call's input.

## Design

### The cost so far

`CostRule`, carried by the descriptions of `getSession`, `listEvents`
and `streamEvents`, states the contract:

- `budget.spent_cost_usd_micro` is the cost of the log through
  `last_seq`: each `model.request`'s `cost_usd_micro`, a compaction's
  and a title's included, and each `tool.result`'s, a redacted one's
  from its tombstone. No other event adds to it.
- A client that read the session adds the `cost_usd_micro` of each
  `model.request` and `tool.result` after `last_seq` as the stream
  carries it; the sum is the next read's `spent_cost_usd_micro`.
- A fork's own spending is `spent_cost_usd_micro` less
  `carried_cost_usd_micro`.

| Decision | Chosen | Alternatives and why not |
|---|---|---|
| How a client learns the spending moved | the events that move it, each with its own cost, and the session's `last_seq` as the point a read counted to | a `session.spending` event appended after each priced event with the new total: it says again what the event before it said, doubles the log's rows for every model request, and is a new event type every installation's authorizer and every reader learns before the core that writes it rolls, for no new information; a running total on the envelope of each event: a field on every event of every type for the one figure two types change |

### The to-do list

`TodoResultMeta` describes the meta of a `todo` call's `tool.result`:
`todos`, required, at most `TodoLimit` items, each `{id, content,
status}` with `status` one of `pending`, `in_progress`, `completed`. The
latest list of a conversation, the session's own or the subagent the
event's `subagent` names, is that conversation's list; a result of any
outcome but `ok` records none and leaves the list as it was. Nothing on
the wire changes: the schema describes what the tool already records,
and a test holds the two together.

### Progress without a to-do list: not now

A count such as "18 of 42 listings" for an agent that keeps no to-do
list would be a third record: what is counted, how many are done and how
many there are. It is not built now:

- **Only the model knows what it counts.** The harness cannot tell that
  a session is reading 42 listings; the count would be the model's own
  claim, through a new tool on every agent's requests, which the core
  cannot check. A count that runs ahead of the work is worse for the
  person than no count.
- **Behavior needs the task suite.** A new tool changes what every agent
  does, and an agent's behavior is proven by the task suite (spec 025):
  that models call it, keep it true and do not spend steps on it. That
  evidence does not exist yet.
- **The to-do list already counts planned work.** An agent that plans
  steps keeps a list, and the list's completed items of all of them is
  the count.
- **Counted work of many like items is a workflow run.** Reading 42
  listings is the shape of a phase that fans out into 42 nodes (spec
  033). A run's nodes are the harness's own, so its progress, nodes done
  of all of them, comes from its events with no claim of the model's.
  That is where a count for work without a list belongs.

What would change the decision: spec 033 built, whose run events then
carry a run's count; or a task-suite result that a model keeps a
reported count true, with an installation that wants the count sooner.

## Not in this spec

An estimate of what a session will cost in all: the core holds a cap,
`max_cost_usd_micro`, not an estimate, and an estimate is a product's
decision. The words a client shows.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `getSession`, `listEvents` and `streamEvents` state the cost rule | `internal/server.TestTheSessionAndEventRoutesStateTheCostRule` | built |
| A session read, plus the `cost_usd_micro` of each later `model.request` and `tool.result` the events route answers, is the next read's spending; a redacted result's tombstone keeps its cost; a fork's own spending is spent less carried | `internal/server.TestASessionsSpendingKeptCurrentFromItsStream` | built |
| `TodoResultMeta` names every member a `todo` call's meta holds and no other, its statuses are the tool's, a refused call records no meta, and an emptied list is `todos: []` | `internal/server.TestTodoResultMetaIsWhatTheTodoToolRecords` | built |

## Outcome

Built on 2026-10-30: `CostRule` in the three routes' descriptions,
`TodoResultMeta` in the document, and `docs/cost-and-progress.md`. No
event, field or behavior changed, so no authorizer question changes and
the release rolls alone. The status is `testing` until a client keeps a
session's cost so far and its to-do count from the stream against an
installation running this release.
