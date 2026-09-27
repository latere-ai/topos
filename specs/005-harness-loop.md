---
title: "The harness loop: turns and steps, stops, output limits, retries, interrupt, validation, parallel calls"
status: drafted
track: core
depends_on: [001-architecture.md, 004-session-log.md, 007-models.md, 008-tools.md]
affects: [harness/]
effort: large
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# The harness loop

## Overview

The harness runs one agent's loop over a session. A **turn** is the
agent's work from one input until the session is idle again; a
**step** is one model request and the tool calls it asked for. The
harness turns the fold of the log into a request, streams the response,
validates, scores and executes the tool calls on the session's
machine, and hands every step's events to the runner to append at fixed
commit points. It keeps no state between steps: a fresh harness given
the log and the machine continues where another stopped (invariant 4 of
[[001-architecture]]). Every stop the model did not choose ends the
turn with a stop reason that names it (invariant 9).

## Current state

The loop of v0.7.0 spec 001 is replaced. A probe that drove it through
its public surface reproduced eight failures, each of which now has a
test that fails the old behavior (the table at the end of Design). The
three-phase tool path of v0.7.0 spec 001 (validate, permission,
execute) is kept, with each phase now defined. From v0.7.0 spec 028 the
loop keeps the rule that a budget that cannot be priced is refused
rather than treated as free, and that one meter is shared by every
agent of a session; from the retired hosted service, the budget meter
threaded into the loop at each request.

## Design

### The contract with the runner

```go
type Config struct {
	Model       models.Model
	Connection  models.Connection
	Machine     machine.Machine
	Tools       *tools.Registry
	Hooks       []Hook
	Permissions permission.Policy
	Retry       retry.Policy // latere.ai/x/pkg/retry
	Clock       func() time.Time
	Observer    Observer
}

type Appender interface {
	Append(ctx context.Context, batch []session.Event) error
}

func (h *Harness) RunTurn(ctx context.Context, s session.Session, log []session.Event, a Appender) (session.Status, error)
```

`RunTurn` reads nothing but its arguments and the machine, appends
through `a` only, and returns when the session is idle or ended. An
`Append` error (a lost lease, a store failure) stops the turn at once
with no further call and no further event: the runner, not the
harness, decides what happens next ([[016-runners]]).

### A turn

1. Fold the log ([[004-session-log]]). If the fold's `Open` is not
   empty, run [[016-runners]]'s recovery of a step without results
   first.
2. Apply pending inputs: each `user.tool_confirmation` with `allow`
   runs its call; `deny` appends a `tool.result` with outcome `denied`
   and the note; each `user.tool_result` is already in the log. If a
   call still waits, the session goes back to idle with the same stop
   reason.
3. Loop over steps:
   1. Boundary checks, in order: a `user.interrupt` since the turn
      began stops with `interrupted`; the turn deadline passed stops
      with `turn_limit`; the budget's pre-request check
      ([[007-models]]) failing stops with `budget`.
   2. Build the request: the fold, the context management of
      [[010-context]], the prompt parts of [[011-instructions-and-skills]],
      the tool definitions of the registry, `max_tokens` from the
      model catalog, and the agent's effort.
   3. Send it with retry, streaming deltas to the Observer.
   4. Commit point one: append `model.request`, `agent.message`, and
      one `agent.tool_use` per call that passed validation, each with
      its risk and verdict ([[012-permissions-and-approvals]]). Nothing
      runs before this batch is durable.
   5. Dispatch on the IR stop reason (the table below).
   6. Run the calls whose verdict is allow or flag; commit point two
      is each `tool.result`, appended when its call returns.
   7. If any call's verdict is ask, go idle with `tool_confirmation`;
      if any call is client-executed, go idle with `tool_result`.
      Otherwise start the next step.
4. At the turn's end, take the checkpoint ([[034-checkpoints-and-rewind]])
   and append `session.status` idle with the stop reason and the
   checkpoint, commit point three.

There is no limit on the number of steps.

### Stop reasons

| Condition | Stop reason ([[004-session-log]]) | Appended before the status |
|---|---|---|
| IR `end_turn` or `stop_sequence` | `end_turn` | nothing |
| IR `refusal` | `end_turn`, `detail` `refusal` | nothing |
| IR `tool_use`, every call answered | next step | the `tool.result` events |
| a call's verdict is ask | `tool_confirmation` | the step's other results |
| a client-executed call | `tool_result` | the step's other results |
| IR `max_tokens`, continued fewer than twice in a row | next step, a continuation | nothing |
| IR `max_tokens` a third time in a row | `output_limit` | `session.error` `output_truncated` |
| the turn deadline | `turn_limit` | nothing |
| the budget's pre-request check | `budget` | nothing |
| `user.interrupt` | `interrupted` | the canceled calls' results |
| a model error after retries, or a non-retryable one | `error` | `model.request` with outcome `error`, then `session.error` `model_error` |
| a failure inside the harness | `error` | `session.error` `internal` |

### Output limits

`max_tokens` on every request is the model's output limit from the
catalog ([[007-models]]); the harness has no default of its own, and a
model with no known limit is refused at the session's first step. A
response that stops at `max_tokens` is appended as an `agent.message`
with `truncated` true and no `agent.tool_use` for any of its calls, so
none runs, the truncated one included; the fold drops their blocks and
asks the model to continue ([[004-session-log]]). The continuation's
`agent.message` names the truncated one in `continuation_of`. Two
continuations in a row are allowed; a third `max_tokens` fails the
turn with `output_limit`.

### Retries

The request is sent through `retry.Do` with `retry.Policy{MaxAttempts:
6, Base: 2 * time.Second, Max: 60 * time.Second, Jitter: 0.2}`, and a
provider's `Retry-After` raises a delay and never lowers it.

| Retried | Not retried (`retry.Stop`) |
|---|---|
| HTTP 408, 429, 500, 502, 503, 504, 529; an overloaded or rate-limit error event inside a stream; a connection error; a stream that ends before its terminal event | HTTP 400, 401, 403, 404, 413, 422; a codec error encoding the request |

A retried stream's partial output is discarded and the Observer
receives a reset for the step. Waiting counts against the turn
deadline, and a wait that would pass it stops with `turn_limit`. After
the last attempt the step appends one `model.request` with `outcome`
`error` and `attempts`, then `session.error` with code `model_error`
and `retryable` true; every earlier event of the turn stays in the log.

### Tool-call validation

Before a call is scored, each is checked and a failing one is answered
without running or asking:

| Check | Outcome ([[008-tools]]) | Result text |
|---|---|---|
| the name is in the registry | `unknown_tool` | `No tool named <name>. Available tools: <names>.` |
| the input is a JSON object matching the tool's JSON Schema | `invalid_input` | `The input does not match the schema of <name>:` and up to five lines `<JSON pointer>: <problem>` |

The validator is Go-native and covers `type`, `properties`,
`required`, `additionalProperties`, `items`, `enum`, `const`,
`minimum`, `maximum`, `minLength`, `maxLength`, `pattern`, `oneOf` and
`anyOf`; a tool schema using another keyword is refused when the
registry is built.

### Parallel calls

Each tool declares whether it may run beside others ([[008-tools]]).
The calls of a step are taken in order and grouped into maximal runs
of consecutive parallel tools; each run executes concurrently, at most
eight at a time, and each other call runs alone, in order. Results are
appended as calls return; the fold orders them by the model's order.
Several `spawn` calls in one step run their threads at once
([[013-threads-and-subagents]]).

### Interrupt

A `user.interrupt` takes effect at the next step boundary, and the
harness brings the boundary forward: an in-flight model request is
canceled (its `model.request` has `outcome` `canceled`, no
`agent.message` is appended, and its partial output is discarded), and
running calls are canceled through their context. Every canceled call
still gets a `tool.result`, with outcome `canceled`; `bash` sends its
process group `SIGTERM` and `SIGKILL` 5 seconds later. Then the turn
stops with `interrupted`.

### Streaming deltas versus events

Deltas are the live stream of text, thinking and tool input fragments
while a response arrives. They go to the `Observer` (`OnDelta`,
`OnReset`), carry the thread, turn, step and block index, and are never
appended: they have no sequence and a client that misses one loses
nothing, because the `agent.message` that follows is the record. The
runner forwards deltas to attached clients ([[015-api]]).

### Limits

| Limit | Default | Set by |
|---|---|---|
| steps per turn | none | nothing |
| turn wall clock | `2h` | the agent's `limits.turnTimeout` ([[003-manifest]]), the session's `limits.turn_timeout`, the authorizer's `limits`; the lowest wins |
| session budget | none | the agent's `budget`, the session's `budget`, the authorizer's `limits`; the lowest wins ([[007-models]]) |
| session age | `168h` | the same three, into `expires_at` ([[004-session-log]]) |

### Error codes

| Code | Retryable | Meaning |
|---|---|---|
| `model_error` | yes | the model answered with an error after retries, or with one that is not retried; `detail` carries the provider's status and error type |
| `output_truncated` | yes | three `max_tokens` stops in a row |
| `internal` | no | a failure of the harness itself; `detail` carries the error |

### The eight failures of v0.7.0

| # | v0.7.0 behavior | Now | Test |
|---|---|---|---|
| 1 | a turn stopped after 16 model calls and reported success with the last preamble as the answer | no step cap; a turn ends only as the stop table says | `TestProbe/no_step_cap`: 200 scripted tool steps, the turn ends `end_turn` after all 200 |
| 2 | output capped at 4096 tokens; a `max_tokens` stop treated as a normal stop, leaving a `tool_use` with no result | `max_tokens` from the catalog; a `max_tokens` stop continues or fails with `output_limit` and never runs a call | `TestProbe/max_tokens_from_catalog`, `TestProbe/truncated_call_never_runs` |
| 3 | one transient model error returned an empty turn and discarded every tool call already run | retry with backoff; a failure after retries keeps every earlier event and ends `error` | `TestProbe/transient_error_retried`, `TestProbe/failure_keeps_earlier_events` |
| 4 | no system prompt: the model was never told its working directory, platform, date or path rules | the harness prompt and the context block on every request | `TestProbe/system_prompt_present` ([[011-instructions-and-skills]]) |
| 5 | a delegated agent ran in a fresh, empty sandbox and could not read the file its parent wrote | threads share the session's machine | `TestProbe/subagent_reads_parent_file` ([[013-threads-and-subagents]]) |
| 6 | file tools re-rooted absolute paths; `bash` ran with a fixed `PATH` and `HOME=/tmp` | absolute paths are the machine's own; `bash` has the person's environment on the host | `TestProbe/absolute_paths_not_rerooted`, `TestProbe/bash_has_persons_path` ([[008-tools]], [[009-machines]]) |
| 7 | a zero model kind silently selected the fake model and reported success | a connection with no base URL is an error; a server role refuses the scripted model | `TestProbe/no_silent_fake_model` ([[007-models]]) |
| 8 | thinking blocks were dropped, only the system block was cached, and the gateway's cost was discarded | thinking replayed with its signature, rolling cache breakpoints, cost into the meter | `TestProbe/thinking_replayed`, `TestProbe/cost_reaches_budget` ([[007-models]], [[010-context]]) |

Each probe test is written against the old loop's behavior first and
fails it; the suite lives in `harness/probe_test.go`.

## Not in this spec

The fold ([[004-session-log]]); request encoding, cost and the budget
check ([[007-models]]); each tool ([[008-tools]]); verdicts, modes and
hooks ([[012-permissions-and-approvals]]); context management
([[010-context]]); recovery of a step without results and leases
([[016-runners]]); threads ([[013-threads-and-subagents]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The eight failures of v0.7.0 each have a test that fails the old behavior and passes the new | `TestProbe` and its eight subtests | not built |
| A grep for a numeric step cap or a default output cap in `harness/` returns nothing | `TestNoFixedLimitsInHarness` | not built |
| Each row of the stop table ends the turn with its stop reason and appends what the row names, with a scripted model driving each condition | `TestStopReasons`, one subtest per row | not built |
| Nothing runs before commit point one is durable: an Appender that fails the first batch leaves no call executed on the machine | `TestNoCallRunsBeforeToolUseIsDurable` | not built |
| A 429 then a 200 is one step with `attempts` 2 and a delay of at least `Retry-After`; a 400 is not retried | `TestRetryHonorsRetryAfter`, `TestClientErrorNotRetried` | not built |
| An unknown tool and an input failing its schema are answered with `unknown_tool` and `invalid_input` and nothing runs | `TestValidationAnswersWithoutRunning` | not built |
| A step with three parallel calls and one serial call runs the three concurrently and the serial one alone, and the fold orders the results as the model did | `TestParallelCallsGroupedAndOrdered` | not built |
| An interrupt during a streaming response cancels it, appends no `agent.message`, and ends `interrupted`; an interrupt during a 60 second `bash` ends within 6 seconds with a `canceled` result | `TestInterruptCancelsStream`, `TestInterruptCancelsRunningCall` | not built |
| A fresh harness given the log of a turn stopped after any commit point continues it with the same next request bytes as the original harness | `TestResumeFromLogOnFreshHarness` | not built |
| Deltas reach the Observer and never appear in the log | `TestDeltasAreNotAppended` | not built |
| The turn wall clock takes the lowest of agent, session and authorizer limits and ends `turn_limit` | `TestTurnLimitTakesTheLowest` | not built |
