---
title: "Context: the order of the prompt's parts, cache breakpoints, token accounting, clearing, compaction"
status: drafted
track: core
depends_on: [004-session-log.md, 005-harness-loop.md, 007-models.md, 011-instructions-and-skills.md]
affects: [harness/, harness/prompt/]
effort: medium
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Context

## Overview

A long session outgrows the model's input window, and a request whose
prefix changes pays for it again. This spec fixes the order of a
request's parts so the prefix stays stable, places the cache
breakpoints, counts tokens between requests, and keeps a thread under
its model's window in two steps: first clearing tool results older than
the last ten steps, then, if that is not enough, a compaction step that
replaces the older conversation with a summary. Both are recorded as
`context.compacted` events, so the fold that rebuilds a request after a
restart applies them the same way.

## Current state

v0.7.0 had no context management: a long session failed on the
provider's window error, and only the system block carried a cache
breakpoint, so every step paid for its history again (probe failure 8
of [[005-harness-loop]]). The retired laptop client had no compaction.
Nothing is borrowed.

## Design

### The order of a request's parts

| # | Part | Source | Changes |
|---|---|---|---|
| 1 | tool definitions: the built-ins in the registry's order, then MCP tools by name, then client tools by name | [[008-tools]], [[021-mcp-servers]] | when the tool set changes |
| 2 | the harness prompt | [[011-instructions-and-skills]], by `prompt_version` | never within a session |
| 3 | the agent's instructions | the agent version | never within a session |
| 4 | the context block | `session.machine` | when a machine attaches |
| 5 | project instruction files | `session.machine` | when a machine attaches |
| 6 | the skills index | `session.machine` | when a machine attaches |
| 7 | memory notes | `memory.attached` | when a store attaches |
| 8 | the messages | the fold ([[004-session-log]]) | every step, by appending |

Parts 2 to 7 are the system prompt. The order puts what changes least
first. Parts 4 to 7 are the fold's structured system parts
([[004-session-log]]); the harness renders each into text blocks,
reading an instruction file's content from its blob, in the fold's
order, and [[011-instructions-and-skills]] fixes the text each renders
to.

### Cache breakpoints

| Breakpoint | On |
|---|---|
| 1 | the last tool definition |
| 2 | the last block of the system prompt |
| 3 | the last block of the previous request's final user message |
| 4 | the last block of this request's final user message |

Breakpoints 3 and 4 roll forward with the conversation, so each step's
request reads the previous step's prefix from the cache and writes its
own. [[007-models]] expresses them per dialect. The history is only
appended to, so a prefix, once cached, stays valid until a compaction.

### Token accounting

Each `model.request` gives the request's prompt tokens: for
`anthropic-messages`, `input_tokens` plus the cache-read and
cache-write tokens; for the OpenAI dialects, `input_tokens`, which
already counts cached tokens. The estimate for the next request is the
last request's prompt tokens, plus its output tokens, plus the tokens
of every event appended since, counted by
`latere.ai/x/pkg/llmdialect/tokencount`. The estimate feeds the
threshold here and the budget check of [[007-models]].

### The threshold, clearing and compaction

The threshold is 80% of the model's input window from the catalog
([[007-models]]), and an agent may set it between 50% and 95% with
`spec.context.compactAt` ([[003-manifest]]). Before each request, per
thread:

1. If the estimate is under the threshold, send.
2. Otherwise clear: every `tool.result` older than the thread's last
   ten steps, except `todo` results, is cleared, by appending one
   `context.compacted` with `kind` `clear_tool_results`, the cleared
   `tool_use_ids`, the range, and the estimates before and after.
3. If the estimate is still over the threshold, compact:
   1. Send one request with the thread's transcript and the compaction
      prompt `harness/prompt/compact-v1.md`, on the thread's own model,
      recorded as a `model.request` that counts toward the budget.
   2. The summary covers every event up to the last complete step
      before the thread's three most recent steps; a step is never
      split from its results.
   3. Append `context.compacted` with `kind` `summary`, `cause`
      `threshold`, the range, the summary, the compaction's request,
      and the estimates.
4. If the estimate is still over the window's limit after compaction,
   the turn fails with `session.error` `context_exhausted`.

The compaction prompt asks for, in order: the person's requests quoted
exactly, the decisions made and why, the files changed and their state,
the commands that matter and their results, the open problems, and the
next step. A compaction that fails after retries ends the turn with
`error` and `session.error` `compaction_failed`; nothing is cleared or
replaced by a failed attempt.

A redaction ([[004-session-log]]) forces a compaction with `cause`
`redaction` whose range covers the redacted event, before the thread's
next request.

### Cache hit rate

A turn's cache hit rate is the cache-read tokens over the prompt tokens
of its requests from the second on. Outside a turn with a compaction it
is at least 0.8 for the Messages dialect; after a compaction the next
request is a miss by construction and the rate over that turn's
remaining requests is at least 0.8 again from the third request after
it. The stub Lux of [[026-stubs-and-tiers]] reports cache reads by
matching breakpoint prefixes against its earlier requests, so the rate
is testable without a provider.

### Error codes

| Code | Meaning |
|---|---|
| `compaction_failed` | the compaction request failed after retries |
| `context_exhausted` | a request is over the model's window after compaction |

## Not in this spec

The content of the harness prompt, the context block and the
instruction files ([[011-instructions-and-skills]]); the fold's
rendering of `context.compacted` ([[004-session-log]]); cache hint
encoding and pricing ([[007-models]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A request's parts are in the table's order and carry the four breakpoints | `TestRequestPartOrderAndBreakpoints` | not built |
| A session past the threshold clears first and compacts only when clearing is not enough, and continues without an error | `TestClearThenCompact` with a scripted model and a small catalog window | not built |
| Clearing keeps the last ten steps' results and every `todo` result | `TestClearingKeepsRecentSteps` | not built |
| The summary never splits a step from its results and keeps the three most recent steps verbatim | `TestCompactionRangeRespectsSteps` | not built |
| A runner restarted after a compaction rebuilds the same request as the one that would have followed it | `TestCompactionSurvivesRestart` | not built |
| A turn of twenty steps against the stub Lux has a cache hit rate of at least 0.8 from its second request | `TestCacheHitRate` | not built |
| A redaction forces a compaction covering the redacted event before the next request | `TestRedactionForcesCompaction` | not built |
| A compaction that fails leaves the log as it was apart from its `model.request` and the error | `TestFailedCompactionChangesNothing` | not built |
