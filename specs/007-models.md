---
title: "Models: the connection, the IR in the log, llmdialect's codecs, raw capture, cost and the budget"
status: drafted
track: core
depends_on: [001-architecture.md, 002-scaffold-and-configuration.md, 004-session-log.md]
affects: [models/, models/dialect/, cmd/toposd/]
effort: large
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Models

## Overview

A model connection is a base URL, a credential and a dialect. The log
stores model output as `latere.ai/x/pkg/llmdialect`'s intermediate
representation, encoded as the Lux wire JSON, and the harness encodes
each request with llmdialect's backend codec for the model's family
(Anthropic Messages, OpenAI Responses, OpenAI Chat Completions), then
sends the native bytes through a Lux door or to a provider. Topos
writes no adapter of its own. Beside the IR the log keeps every raw
response body and a hash of every request's bytes, so a replay proves
it re-encodes the same request. Usage and cost come from the
provider's own figures and feed the session's budget, which is checked
before every request.

## Current state

v0.7.0 spec 002 selected a model through `ModelOptions` and reached Lux
through an adapter over the Lux Go SDK. Two of the probe's failures
were here: a zero `ModelOptions.Kind` silently selected the fake model
and reported success, and the adapter dropped thinking blocks, cached
only the system block, and discarded the gateway's cost, so a budget
failed on the first turn ([[005-harness-loop]]). v0.7.0 spec 028
established what this spec keeps: cost is carried as a nullable
`CostUSDMicro` where nil means unknown and never zero, a budget over an
unpriced model is refused before any spend, and one meter bounds every
agent of a session.

llmdialect today carries thinking signatures, redacted thinking and
cache hints through its IR, and reports in `ir.Request.Loss` every
field a target dialect cannot represent. Its Responses backend drops
OpenAI's encrypted reasoning items instead of replaying them; the
provider-opaque block below closes that and is a change to pkg that
lands before this spec is built.

## Design

### The interface

```go
type Model interface {
	Stream(ctx context.Context, req Request) (Stream, error)
}

type Request struct {
	IR         ir.Request  // latere.ai/x/pkg/llmdialect/ir
	Connection Connection
	Capture    bool        // keep the request bytes as a blob
}

type Stream interface {
	Next() (ir.Event, error) // io.EOF after the terminal event
	Result() Result          // valid after io.EOF
	Close() error
}

type Result struct {
	Message       json.RawMessage // the Lux wire assistant message
	StopReason    ir.StopReason
	Usage         ir.Usage
	RawResponse   []byte // the response body as received
	RequestBytes  []byte // kept only when Capture is set
	RequestSHA256 string
	Codec         string
	Loss          []string
}
```

`models/dialect` is the one implementation: it encodes with the
dialect's llmdialect backend codec, posts the bytes, reads the stream,
decodes it with the same codec into IR events and a final IR response,
and encodes that response as the Lux wire message the log stores.

### The connection

| Field | Meaning | Default |
|---|---|---|
| `base_url` | a Lux door or a provider's API base, for example `https://lux.example.com/anthropic` | `TOPOS_MODELS_URL` ([[002-scaffold-and-configuration]]) |
| `model` | the upstream model name | none; required |
| `family` | `anthropic`, `openai`, or `other` | from the catalog entry |
| `dialect` | `anthropic-messages`, `openai-responses` or `openai-chat` | `anthropic` → `anthropic-messages`, `openai` → `openai-responses`, `other` → `openai-chat` |
| `credential` | resolved per call, never stored in the connection | the agent's `spec.model.credential`, then the session's agent key ([[018-credentials-and-secrets]]), then `TOPOS_MODELS_KEY`; none of them is `model_credential_missing` |

The request path is the dialect's own under the base URL
(`/v1/messages`, `/v1/responses`, `/v1/chat/completions`), and the
credential goes in the header the dialect's provider reads (`x-api-key`
with `anthropic-version` for Messages, `Authorization: Bearer`
otherwise; Lux accepts both). The zero `Connection` is invalid:
`Connection.Validate` refuses an empty base URL or model, and there is
no implicit model.

### The catalog

Each model has an entry: `name`, `family`, `dialect`, `input_window`
and `max_output_tokens` in tokens, `pricing` (USD per million input,
output, cache-read and cache-write tokens), and `supports` (`thinking`,
`effort`, `images`, `parallel_tools`). Entries come from, in order of
precedence: the agent's `spec.model` fields ([[003-manifest]]), the
figures a Lux connection serves for the model, and the catalog file
embedded in the build, `models/catalog.json`. A model with no
`input_window` or no `max_output_tokens` from any source is refused at
the session's first step with `model_unknown`, because the loop's
output limit and the compaction threshold both come from here
([[005-harness-loop]], [[010-context]]).

### The IR in the log, and replay

An `agent.message` stores the response's content as the Lux wire
message of llmdialect, every block verbatim: text, `thinking` with its
signature, `redacted_thinking`, `tool_use`, and the opaque block. A
request is rebuilt from the fold on every step, never cached, and is
encoded by the backend codec of the thread's family. Because the
history is append-only, a provider that binds thinking to the
conversation sees the same prefix it saw before.

Each thread pins its family when it starts ([[013-threads-and-subagents]]).
A change of the session's model to another family is a translation:
the next request is encoded by the other family's codec, every field
it cannot carry (another dialect's opaque blocks, another provider's
thinking signatures) is dropped, and the dropped fields are the
`model.request`'s `loss`. Within one family, replay is lossless, and
that is a gate: `models/dialect`'s round-trip suite takes recorded
native responses for each dialect, decodes them to IR and Lux JSON,
folds them into the next request, and asserts the native encoding of
the assistant turn equals what the provider sent, byte for byte after
JSON normalization.

### The provider-opaque block

A change to pkg, landed before this spec is built. The IR gains a
block type `opaque` carrying `Opaque{Dialect, Kind string; Raw
json.RawMessage}`, encoded in the Lux wire JSON as:

```json
{"type":"opaque","opaque":{"dialect":"openai-responses","kind":"reasoning","raw":{"type":"reasoning","id":"rs_01","encrypted_content":"gAAAA...","summary":[]}}}
```

A backend encodes an opaque block only when its dialect equals
`Dialect`, placing `Raw` where it came from; every other backend drops
it and adds `content.opaque.<kind>` to the loss report. The first case
is the Responses API's reasoning items, requested with
`include: ["reasoning.encrypted_content"]` and `store: false`, so a
stateless request carries the model's reasoning forward.

### Raw capture and request hashes

| Kept | When | Where |
|---|---|---|
| the raw response body, the stream bytes as received | always | `response_blob` of `model.request` |
| `request_sha256` and `request_bytes` of the exact bytes sent | always | `model.request` |
| `codec`, `<dialect>@<llmdialect module version>` read from the build info | always | `model.request` |
| the full request bytes | only while the session's `capture.requests` is true | `request_blob` |

Full requests are off by default because every request resends the
whole history, so their size grows with the square of the session's
length. `models.Replay` folds a log step by step, re-encodes each
request with the current build, and compares the hash with the
recorded one; a step whose recorded codec differs from the build's is
skipped and reported as `codec_mismatch` rather than failed.

### Cache hints and effort

The harness marks cache breakpoints on the IR as [[010-context]] places
them; the codec expresses them: `cache_control` blocks for Messages
(at most four per request), the session id as `prompt_cache_key` for
Responses, and nothing for Chat, which reports the hint as loss. The
agent's `spec.model.effort` is one of `minimal`, `low`, `medium` or
`high`, set as the IR's `Reasoning.Effort`, which each codec maps to
its dialect.

### Usage and cost

Usage is the provider's own figures as the codec decodes them: input,
output, cache-read, cache-write and reasoning tokens, where an absent
cache figure is nil and never zero. Cost, in micro-USD:

| Source | `cost_source` | When |
|---|---|---|
| the gateway's reported `cost_usd_micro` | `provider` | a Lux connection that reports one |
| `in*p_in + out*p_out + cache_read*p_cache_read + cache_write*p_cache_write`, reasoning tokens counted as output, rounded up | `catalog` | the connection reports none and the catalog prices the model |
| none | absent | neither; allowed only when no budget applies |

### The budget

A session's budget is its `budget.max_cost_usd_micro`
([[004-session-log]]), the lowest of the agent's, the session's and the
authorizer's `limits`. The meter is the sum of `cost_usd_micro` over
every `model.request` of every thread, and a thread's own budget is
carved from its parent's ([[013-threads-and-subagents]]). Before each
request the harness checks `spent + estimate >= max`, where `estimate`
is the next request's input tokens priced at the input rate (the last
request's input tokens plus those added since, [[010-context]]); a hit
ends the turn with `budget`. Output is not estimated, so a session can
exceed its budget by at most one response's output. A budget over a
model that cannot be priced is refused before the first request with
`model_unpriced`.

### Refusals

The scripted model of [[026-stubs-and-tiers]] is reached only through
a connection whose base URL has the scheme `scripted:`, which `topos`
accepts for tests. `toposd serve` and `toposd runner` refuse to start
without `TOPOS_MODELS_URL`, and refuse one with the `scripted:` scheme,
each with one configuration line and exit 1.

### Error codes

| Code | Where | Meaning |
|---|---|---|
| `model_credential_missing` | `session.error` | no credential source of the connection table has one |
| `model_unknown` | `session.error` | no catalog source gives the model's input window and output limit |
| `model_unpriced` | `session.error`, and the session create answer | a budget applies and the model cannot be priced |
| `codec_mismatch` | the replay report | a step was encoded by another codec version and is not compared |

## Not in this spec

Where cache breakpoints go and how tokens are counted between
requests ([[010-context]]); the loop's use of the result
([[005-harness-loop]]); credential custody and the agent's key
([[018-credentials-and-secrets]]); the scripted model itself
([[026-stubs-and-tiers]]); the task suite's comparison of the IR path
with the provider's own SDK ([[025-task-suite]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| For each of the three dialects, recorded responses carrying text, thinking with signatures, redacted thinking, parallel tool calls and, for Responses, encrypted reasoning, round-trip through the log into the next request with zero loss | `TestSameFamilyReplayIsLossless`, one subtest per dialect over `models/dialect/testdata/` | not built |
| A change of family mid-session drops the other family's thinking and opaque blocks and names each in `loss` | `TestFamilyChangeReportsLoss` | not built |
| Every `model.request` carries `request_sha256`, `codec` and a `response_blob` whose bytes equal the stub's response; `request_blob` appears only with capture on | `TestRequestHashAndBlobs` | not built |
| Replaying a recorded session reproduces every request hash with the same codec version | `TestReplayReproducesRequestHash` | not built |
| `max_tokens` on each request equals the catalog's output limit, and an unknown model is refused with `model_unknown` | `TestMaxTokensFromCatalog`, `TestUnknownModelRefused` | not built |
| Cost comes from the gateway's figure when reported and from the catalog otherwise, and reaches the session's meter | `TestCostSourceOrder`, `TestProbe/cost_reaches_budget` | not built |
| A turn that would pass the budget stops with `budget` before the request is sent; a budget over an unpriced model is refused before any request | `TestBudgetPreRequestCheck`, `TestBudgetOverUnpricedModelRefused` | not built |
| The zero connection is an error, and `toposd serve` exits 1 with no `TOPOS_MODELS_URL` or with a `scripted:` one | `TestZeroConnectionIsAnError`, `TestServeRefusesScriptedModel` | not built |
| A retry of a stream reuses no partial output: the stored message equals the final attempt's response | `TestRetriedStreamStoresFinalAttempt` | not built |
| The credential never appears in any event, blob or log line of a session | `TestModelCredentialNeverLogged` | not built |
