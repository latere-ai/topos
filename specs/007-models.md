---
title: "Models: the connection, the IR in the log, llmdialect's codecs, raw capture, cost and the budget"
status: in-progress
track: core
depends_on: [001-architecture.md, 002-scaffold-and-configuration.md, 004-session-log.md]
affects: [models/, models/dialect/, tools/catalog/, cmd/toposd/]
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

llmdialect carries thinking signatures, redacted thinking and cache
hints through its IR, and reports in `ir.Request.Loss` every field a
target dialect cannot represent. The provider-opaque block below
landed in `latere.ai/x/pkg` v0.88.0, so its Responses backend replays
OpenAI's encrypted reasoning items instead of dropping them, and the
harness asks a Responses model for them.

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
	Message       lux.Message // the Lux wire assistant message, every block verbatim
	StopReason    ir.StopReason
	Usage         lux.Usage
	RawResponse   []byte // the response body as received
	RequestBytes  []byte // kept only when Capture is set
	RequestSHA256 string
	RequestSize   int64
	Codec         string // "<dialect>@<llmdialect module version>"
	Loss          []string
	FirstToken    time.Duration // from sending to the first content event
}
```

`models/dialect` is the one implementation: it encodes with the
dialect's llmdialect backend codec, posts the bytes, reads the stream,
decodes it with the same codec into IR events and a final IR response,
and encodes that response as the Lux wire message the log stores. The
decoders write a well-formed tail when an upstream closes a stream
early, so `models/dialect` also checks the raw stream for its dialect's
own terminal frame (`message_stop` for Messages, `response.completed`
or `response.incomplete` for Responses, `[DONE]` or a `finish_reason`
for Chat); a stream without it is an incomplete response, which is
retried ([[005-harness-loop]]). A decoder that reports the terminal
frame itself would replace this check in pkg.

### The connection

| Field | Meaning | Default |
|---|---|---|
| `base_url` | a Lux door or a provider's API base, for example `https://lux.example.com/v1/models/anthropic` | `TOPOS_MODELS_URL` ([[002-scaffold-and-configuration]]), which may name a Lux root instead: its discovery document, `GET <url>/.well-known/lux`, names each family's door, and the connection takes its family's (`anthropic` for Messages, `openai` for Responses and Chat), never the translating `/lux` door; a URL that already names a door, and one that does not answer as Lux, are used as they are; an agent's own `spec.model.baseURL` is used as it is |
| `model` | the upstream model name | none; required |
| `family` | `anthropic`, `openai`, or `other` | from the catalog entry |
| `dialect` | `anthropic-messages`, `openai-responses` or `openai-chat` | `anthropic` → `anthropic-messages`, `openai` → `openai-responses`, `other` → `openai-chat` |
| `credential` | resolved per call, never stored in the connection | the agent's `spec.model.credential` (a provider key the owner brings); otherwise, on an installation with an authorizer, the session's Lux key ([[018-credentials-and-secrets]]), never the installation's key; on one without, `TOPOS_MODELS_KEY`; none of them is `model_credential_missing` |

The request path is the dialect's own under the base URL
(`/v1/messages`, `/v1/responses`, `/v1/chat/completions`), and the
credential goes in the header the dialect's provider reads (`x-api-key`
with `anthropic-version` for Messages, `Authorization: Bearer`
otherwise; Lux accepts both). The zero `Connection` is invalid:
`Connection.Validate` refuses an empty base URL or model, and there is
no implicit model.

### The catalog

Each model has an entry: `name`, `aliases`, `family`, `dialect`,
`input_window` and `max_output_tokens` in tokens, `pricing` (USD per
million input, output, cache-read and cache-write tokens, held exactly
as micro-USD), and `supports` (`thinking`, `effort`, `images`,
`parallel_tools`). A zero figure is unknown, never zero. Entries come
from, in order of precedence: the agent's `spec.model` fields
([[003-manifest]]), the figures a Lux connection serves for the model,
and the catalog file embedded in the build, `models/catalog.json`,
which `Catalog.Resolve` overlays in that order.

`models/catalog.json` is generated by `tools/catalog` and committed. It
takes names and prices from a Lux model catalog (one Model manifest per
file) and input windows and output limits from OpenRouter's public
model list, matched by name, and adds OpenRouter's free models that take
tools at price zero, on the Chat dialect, for development runs. A
missing cache-read price is the input price; a missing cache-write
price is 1.25 times the input price, the rate of the one provider that
reports cache writes.

A model with no `input_window` or no `max_output_tokens` from any
source is refused with `model_unknown` when the harness is built,
before the session's first step, because the loop's output limit and
the compaction threshold both come from here ([[005-harness-loop]],
[[010-context]]).

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

A change to pkg, landed in v0.88.0. The IR has a block type `opaque`
carrying `Opaque{Dialect, Kind string; Raw
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

The meter is a ceiling and a pre-check, not where spend is enforced.
The installation's authorizer is the budget authority: it holds the
payer's wallet, the caps and the per-session ledger across every priced
core, and each core enforces its own resource at its point of use
against the allowance the authorizer gives it: Lux for model calls,
Cella for sandbox time, a memory backend for storage. A sandbox's own
model calls reach Lux through Cella's egress, never through the
harness, so the meter does not see them; Lux attributes them to the
session with workload `sandbox`, and the ledger keeps them apart from
the session's own requests. A core's refusal for spend, the codes
`budget_exhausted` and `spend_exceeded` of `models.SpendRefused`, from
any core the session reaches (a model request, a sandbox create or
command, a memory push) is never retried and stops the turn `budget`
with the refusal in `detail`. With a bring-your-own provider key, model
tokens are billed by the provider, Lux still meters them, and the
ceiling still applies.

A session idle with `budget` resumes with a `user.message`, or with
`POST /v1/sessions/{id}/resume` ([[015-api]]), which a client or the
platform calls once the cap is raised or the wallet refilled. The
server asks the authorizer `session.resume`, applies the `limits` of
its decision and an optional new `max_cost_usd_micro` (each capped by
the agent's), and appends `session.resumed`, which is pending input: a
runner claims the session and continues the turn from where it
stopped.

### Refusals

The scripted model of [[026-stubs-and-tiers]] is reached only through
a connection whose base URL has the scheme `scripted:`, which `topos`
accepts for tests. `toposd serve` and `toposd runner` refuse to start
without `TOPOS_MODELS_URL`, and refuse one with the `scripted:` scheme,
each with one configuration line and exit 1. A role that runs sessions
asks a URL naming no door for Lux's discovery document at start, and
one that does not answer stops the start the same way.

### Error codes

| Code | Where | Meaning |
|---|---|---|
| `model_credential_missing` | `session.error` | no credential source of the connection table has one |
| `model_unknown` | `session.error` | no catalog source gives the model's input window and output limit |
| `model_unpriced` | `session.error`, and the session create answer | a budget applies and the model cannot be priced |
| `budget_exhausted`, `spend_exceeded` | `session.status` `detail` | a core refused a request for spend; never retried ([[004-session-log]]) |
| `codec_mismatch` | the replay report | a step was encoded by another codec version and is not compared |

## Not in this spec

Where cache breakpoints go and how tokens are counted between
requests ([[010-context]]); the loop's use of the result
([[005-harness-loop]]); credentials, the agent's identity and the session's Lux keys
([[018-credentials-and-secrets]]); the scripted model itself
([[026-stubs-and-tiers]]); the task suite's comparison of the IR path
with the provider's own SDK ([[025-task-suite]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Each of the three dialects sends a request with its own headers through the stub Lux and decodes a streamed response of text and a tool call into the Lux wire message, with usage | `models/dialect.TestEveryDialectRoundTripsThroughTheStub`, one subtest per dialect | built |
| A thinking block and its signature come back in the next request | `harness.TestThinkingIsReplayedWithItsSignature` | built |
| For each of the three dialects, recorded responses carrying text, thinking with signatures, redacted thinking, parallel tool calls and, for Responses, encrypted reasoning, round-trip through the log into the next request with zero loss | `TestSameFamilyReplayIsLossless`, one subtest per dialect over `models/dialect/testdata/` | not built |
| A change of family mid-session drops the other family's thinking and opaque blocks and names each in `loss` | `TestFamilyChangeReportsLoss` | not built |
| Every `model.request` carries `request_sha256` of the exact bytes sent, their size, `codec` and a `response_blob` holding the raw response; `request_blob` appears only with capture on and holds the bytes sent | `models/dialect.TestEveryDialectRoundTripsThroughTheStub`, `models/dialect.TestCodecVersionNamesTheDialect`, `harness.TestATurnRunsToolsAndEnds`, `harness.TestCaptureKeepsTheRequestBytes` | built |
| Replaying a recorded session reproduces every request hash with the same codec version | `TestReplayReproducesRequestHash` | not built |
| `max_tokens` on each request equals the catalog's output limit, and a model with no input window or output limit is refused with `model_unknown` | `harness.TestATurnRunsToolsAndEnds`, `harness.TestNewRefusesAnIncompleteConfig`, `models.TestResolveOverlaysSources` | built |
| The embedded catalog resolves a model by name or alias with its windows and prices, carries free development models at price zero, and overlays the Lux and agent figures it is handed in precedence order; `tools/catalog` merges Lux prices with OpenRouter windows | `models.TestEmbeddedCatalog`, `models.TestResolveOverlaysSources`, `tools/catalog.TestRunMergesPricesAndWindows`, `tools/catalog.TestRunRefusesBadInput` | built |
| The figures a Lux connection serves for a model are read and overlaid between the agent's and the embedded catalog's | `TestLuxServedFiguresOverlayTheCatalog` | not built: nothing reads them yet |
| Cost comes from the gateway's figure when reported and from the catalog otherwise, a missing cache-read price is the input price and a missing cache-write price 1.25 times it, and the cost reaches the session's meter | `models.TestCost`, `models.TestPrices`, `harness.TestATurnRunsToolsAndEnds`, `harness.TestTheBudgetStopsTheTurn` | built |
| A turn that would pass the budget stops with `budget` before the request is sent; a budget over an unpriced model is refused with `model_unpriced` before any request | `harness.TestTheBudgetStopsTheTurn`, `harness.TestAnUnpricedModelUnderABudgetIsRefused` | built |
| A model gateway's refusal for spend is not retried and stops the turn `budget` with the refusal in `detail` | `models.TestSpendRefusalsAreNotRetried`, `harness.TestASpentBudgetAtTheGatewayStopsTheTurnWithBudget` | built |
| A spend refusal from Cella or a memory backend stops the turn `budget` the same way | `TestACoresSpendRefusalStopsTheTurn` | not built |
| `POST /v1/sessions/{id}/resume` on a session idle with `budget` appends `session.resumed` with the decision's raised cap and a runner continues the turn; on any other status, and for a budget already spent, it answers `conflict` | `internal/server.TestResumeAfterTheCapIsRaised`, `harness.TestAResumedSessionContinuesTheTurn` | built |
| The zero connection is an error, and so is one with no model or an unknown scheme, family or dialect | `models.TestConnectionValidate` | built |
| `TOPOS_MODELS_URL` naming a Lux root sends each model to its family's door, as the discovery document names it, never the `/lux` door; a door URL and a provider's base are used as they are; a URL that does not answer stops a server role's start and fails a `topos` run naming the variable | `models.TestDoorsPickTheFamilysDoor`, `models.TestNamesADoor`, `models/dialect.TestDiscoverFindsLuxsDoors`, `cmd/toposd.TestServeFindsTheFamilysDoorFromLuxsRoot`, `internal/toposcli.TestRunReachesTheFamilysDoorFromLuxsRoot` | built |
| `toposd serve` and `toposd runner` exit 1 with one configuration line with no `TOPOS_MODELS_URL` or with a `scripted:` one | `cmd/toposd.TestServeRefusesScriptedModel` | built |
| A stream that ends before its dialect's terminal frame is an incomplete response and is retried, in every dialect; an error event inside a stream, an unreachable server and a canceled request are classified | `models/dialect.TestErrorsAreClassifiedForRetry` | built |
| A retry of a stream reuses no partial output: the stored message equals the final attempt's response | `harness.TestRetriedStreamStoresFinalAttempt` | built |
| The credential reaches the gateway in its header and never appears in any event or blob of a session, every request's bytes captured | `harness.TestModelCredentialNeverLogged` | built |
