---
title: "Events and observability: the content-free sink, spans derived from the log, metrics"
status: drafted
track: core
depends_on: [002-scaffold-and-configuration.md, 004-session-log.md, 006-identity.md, 014-store.md, 015-api.md]
affects: [internal/events/, internal/serve/, runner/, harness/]
effort: medium
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Events and observability

## Overview

An installation learns what happened in `toposd` three ways. The
**sink**: every mutation emits exactly one event, signed and
content-free, to the endpoint `TOPOS_EVENTS_URL` names, which is how
an installation attributes usage and reacts to a session ending
(invariant 11 of [[001-architecture]]). **Traces**: OpenTelemetry
spans derived from the session log, a session's turns, steps, model
requests and tool calls, with the session id carried as baggage into
every core the runner calls. **Metrics**: one table, served on the
internal listener. None of the three carries a message, a tool input,
a result, or any other content of a session.

## Current state

v0.7.0's deterministic trace (v0.7.0 spec 008) was a rendered run
graph kept beside the run; it is retired, and spans now come from the
log. The retired hosted service sent content-free events to an
installation's sink; the envelope and the signature follow Lux's and
Cella's event specs so one sink reads all three.

## Design

### The sink envelope

```json
{"id":"01J9Z4A7C3E5G7J9K1M3P5R7T9","type":"session.create","occurred_at":"2026-09-27T10:11:12.123Z",
 "subject":"https://issuer.example.com|u123","object":{"kind":"session","id":"ses_01J9Z3P9D2F6H8K0M2Q4S6U8W0"},
 "session_id":"ses_01J9Z3P9D2F6H8K0M2Q4S6U8W0","agent":{"id":"agent_01J9Z2...","version":3},
 "outcome":"ok","attributes":{"runner":"hosted"}}
```

| Field | Meaning |
|---|---|
| `id` | a ULID of the delivery, unrelated to any `evt_` id; a sink deduplicates on it |
| `type` | the type below |
| `occurred_at` | when the mutation committed |
| `subject` | who did it; a runner-originated event carries the session's initiator |
| `object` | `kind` and `id` of what changed |
| `session_id`, `agent` | present when a session or an agent is involved |
| `outcome` | `ok`, or the error code of a refused mutation the authorizer allowed |
| `attributes` | named figures only: counts, statuses, stop reasons, model names, tokens, cost; never a message, an input, a result, a path or a value |

The body is signed with HMAC-SHA256 under `TOPOS_EVENTS_SECRET`
([[002-scaffold-and-configuration]]) in the header
`Topos-Signature: t=<unix seconds>,v1=<hex>`, over `<t>.<body>`. A sink
answering 2xx has the event; anything else is retried with backoff
(1 s doubling to 5 minutes) for 24 hours, then dropped and counted.
Delivery is at least once and in order per object. Events are written
to the store's outbox in the same transaction as the mutation
([[014-store]]), so a crash between commit and delivery loses nothing;
the outbox holds at most 100000 events, past which the oldest are
dropped and counted.

### Sink types

| Type | When | Attributes |
|---|---|---|
| the action's name, for example `agent.update`, `session.create`, `session.redact` | every API mutation, named by its action in [[006-identity]] | per action: the version created, the runner, the event type sent |
| `session.status_changed` | the runner appended a `session.status` | `status`, `stop_reason` |
| `session.usage` | a turn ended | `model`, `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_write_input_tokens`, `cost_usd_micro`, `requests` |

A read, a list and a stream emit nothing. A `user.*` event sent
through the API is one `session.send` (or `session.interrupt`); the
runner's own appends are summarized by the two runner types, not
emitted one by one.

### Spans from the log

Traces go through `latere.ai/x/pkg/otel` and the standard `OTEL_*`
variables; without an endpoint they are off. The runner opens:

| Span | From | To | Attributes |
|---|---|---|---|
| `topos.session` | the claim | the release | `topos.session_id`, `topos.agent`, `topos.agent_version` |
| `topos.turn` | `session.status` `running` | the turn's end | `topos.turn`, `topos.stop_reason` |
| `topos.step` | the step's first boundary check | its last result | `topos.step`, `topos.thread` |
| `topos.model_request` | the request sent | the `model.request` | `gen_ai.request.model`, token counts, `topos.cost_usd_micro`, `topos.attempts` |
| `topos.tool_call` | the call started | its `tool.result` | `topos.tool`, `topos.outcome`, `topos.verdict` |

The session id is carried as the baggage member `topos.session_id` on
every call the runner makes to a model gateway, Cella, a git host or a
memory backend, so a trace joins across cores. Because the spans are
derived from the log, a runner that resumes a session opens a new
`topos.session` span linked to the previous one by the session id; a
tool call the recovery closed has no span.

### Metrics

Served as Prometheus text at `GET /metrics` on the internal listener
([[002-scaffold-and-configuration]]).

| Metric | Type | Labels |
|---|---|---|
| `topos_sessions_running` | gauge | none |
| `topos_turns_total` | counter | `stop_reason` |
| `topos_model_requests_total` | counter | `dialect`, `outcome` |
| `topos_model_request_seconds` | histogram | `dialect` |
| `topos_model_retries_total` | counter | `dialect` |
| `topos_tool_calls_total` | counter | `tool`, `outcome`, `verdict` |
| `topos_leases_held` | gauge | none |
| `topos_leases_lost_total` | counter | none |
| `topos_cost_usd_micro_total` | counter | none |
| `topos_sink_deliveries_total` | counter | `outcome` (`ok`, `retried`, `dropped`) |
| `topos_api_requests_total` | counter | `route`, `code` |

`tool` takes the built-in names and `mcp` for every MCP tool, so the
label set is bounded; no label carries a session id or a subject.

## Not in this spec

The session log itself ([[004-session-log]]); who may read it
([[006-identity]]); what an installation does with the sink's events.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every mutation route of [[015-api]] emits exactly one sink event of its action's type, and reads, lists and streams emit none | `TestEveryMutationEmitsOneSinkEvent`, driven from the route table | not built |
| No sink event, span attribute or metric label carries a canary string placed in a message, a tool input and a tool result of an e2e session | `TestNoContentInTelemetry` | not built |
| A sink that fails twice then answers 200 receives the event once more, with a valid signature, and per-object order holds | `TestSinkRetryAndOrder` | not built |
| A crash after a mutation commits and before delivery delivers the event after restart | `TestOutboxSurvivesCrash` | not built |
| A turn's spans nest session, turn, step, model request and tool call, and the stub Lux receives the `topos.session_id` baggage | `TestSpansFromTheLog` | not built |
| The metric table here and the registry are the same set | `TestMetricTableMatchesTheSpec` | not built |
