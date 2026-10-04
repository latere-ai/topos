---
title: "Web search: a tool that searches the web through a search service the installation configures, with the session's own key, and the cost a search reports counted in the session's spend"
status: in-progress
track: core
depends_on: [003-manifest.md, 004-session-log.md, 005-harness-loop.md, 007-models.md, 008-tools.md, 012-permissions-and-approvals.md, 016-runners.md, 018-credentials-and-secrets.md, 024-client-cli-skill.md, 025-task-suite.md]
affects: [search/, harness/tools/, harness/, session/, manifest/, prompts/, internal/hosted/, internal/config/, internal/toposcli/, cmd/toposd/, test/tasks/, docs/]
effort: medium
created: 2026-10-05
updated: 2026-10-05
author: changkun
---

# Web search

## Overview

An agent can read a page whose address it knows, with `web_fetch`, and
has no way to find one. A question about a current fact, a library's
latest release or a project's documentation either goes unanswered or
is answered from the model's memory, and an agent that guesses an
address fetches a page that may not exist.

This spec adds one tool, `web_search`. It sends a query to a search
service the installation configures, `TOPOS_SEARCH_URL`, and returns
results the model can cite: a title, a URL and a snippet each. The core
defines the contract the service speaks and names no provider: any
service that answers it works, and an installation fronts the provider
of its choice behind it. The call carries the session's own key, the
one the session asks models with, so the service knows which session
searched and whose credit pays for it. A service that charges reports
the cost in its answer, and the session's spend counts it as it counts
a model request's.

Each choice is a recommendation until the spec is validated. The table
that opens the design lists them with what each was weighed against.

## Current state

- The built-in set is eight tools (`harness/tools/builtin.go`), and it
  is the default set of an agent whose manifest names none
  (`manifest.Builtins`). `web_fetch` is among them: it GETs one http or
  https URL through the machine's `Fetcher`, inside the machine's
  network boundary, and answers "not available" on a machine without
  one.
- The harness adds `question` when the agent's tools name it, and it
  is in no default set ([[039-questions]]). The manifest validator
  accepts a built-in's name with `outputLimit` alone, and `question`
  with its name alone.
- A hosted session's model connection to the installation's model URL
  carries the session's own key when the installation mints one
  (`TOPOS_SESSION_KEYS_URL`), asked of the drive's token source for
  each request, and `TOPOS_MODELS_KEY` otherwise
  (`internal/hosted/credentials.go`). The code states that the
  session's key "never leaves for another base URL": an agent that
  names its own base URL is sent the installation's key or its own,
  never the session's.
- The session's spend is the sum of every `model.request`'s
  `cost_usd_micro` (`session.Spent`, `session.ApplyBatch`), and the
  harness checks it with the next request's estimated input against
  the session's budget before each request ([[007-models]]). Nothing
  else a session does costs anything in the log.
- A tool's effect sets its risk and its verdict ([[012-permissions-and-approvals]]):
  `none` and `read` are allowed in every mode, `external` scores 0.6
  and asks in `confirm` and `progressive` unless a list allows it.

## Design

### Recommendations

| Item | Recommended | Weighed against, and why not |
|---|---|---|
| the name | `web_search` | `search`: `grep` and `glob` already answer "search" in the file tools' texts (`results/search/`), and the pair `web_fetch`, `web_search` reads as one family |
| where the search runs | from the runner, over HTTP to `TOPOS_SEARCH_URL` | through the machine's `Fetcher`, as `web_fetch` does: `web_fetch` reaches any host, so it runs inside the machine's network boundary. A search reaches one address the operator set, with the session's credential, which is a model call's shape, and it then works for a session with no machine |
| the service | a contract this spec defines, which any service may answer | a client of one provider's API: a public core would carry one company's request shape and price, and an installation could not change providers without a release |
| the credential | the session's own key when the installation mints one, else `TOPOS_SEARCH_KEY` | a hosted-agent token for a new audience: it needs the identity provider to mint for that audience, and an installation without an identity provider would have none. The session's key is what the installation's authorizer already registered and can recognize by its hash |
| depth | none: one kind of search | a `depth` input for a slower multi-step search: the agent's own loop already searches, reads and searches again, a deeper search typically costs several times as much and takes tens of seconds, and one kind of search lets a service charge one price a person can learn. A later member can add it without breaking a caller |
| the result count | `max_results`, 1 to `MaxResults`, `DefaultResults` when omitted | a fixed count: a quick check needs two results and a survey ten |
| the effect | `none`: allowed in every mode, plan included, risk 0, and the call never opens a machine | `external`, as `web_fetch`: a search reaches only the service the operator chose, so a query cannot be sent to a host an injected instruction names, and it changes nothing anywhere but the wallet it is charged to, as a model request does. `read`: the harness opens a machine opened on demand before any call whose effect is not `none`, so a session that only searches would create a sandbox. An organization that wants each search confirmed names `web_search` in `alwaysConfirm` |
| the cost | the service reports it, the `tool.result` records it, and the session's spend counts it | the core pricing searches itself: the core does not know what a service charges, and a price in two places would disagree |
| a refusal | the service's own sentence is the result, outcome `error` | a sentence of the core's per refusal: the core cannot know why an installation refuses (a level that does not pay for searches, an empty wallet, a rate), and the service's `message` is written for the person, which the model passes on |
| where it lives | a built-in of `harness/tools` that an agent holds only when it names it, with the service's client in a package of its own, `search` | a ninth member of the default set: every agent that names no tools would start searching, and charging, at its next apply. A harness tool beside `question`: a search needs no Session and holds no turn |

### The tool

| Name | Parallel | Effect | Input | Limits |
|---|---|---|---|---|
| `web_search` | yes | none | `query`, `max_results` | the bounds below; the service's timeout and body limit |

```json
{"query": "latest stable release of the Go programming language", "max_results": 5}
```

| Field | Required | Meaning |
|---|---|---|
| `query` | yes | what to search for, as a person would type it, 1 to `MaxQueryLength` characters |
| `max_results` | no, default `DefaultResults` | how many results to return, 1 to `MaxResults` |

Bounds are constants of the `search` package. The schema, the client's
check and the description file are rendered from them or held to them
by a test, so no bound is written twice. Lengths are characters
(Unicode code points).

| Constant | Value | Bounds | Why this value |
|---|---|---|---|
| `MaxQueryLength` | 400 | `query` | a long natural-language question fits; a pasted document does not, and a service's index matches on a few terms anyway |
| `DefaultResults` | 5 | `max_results` when omitted | enough to compare sources without filling the context |
| `MaxResults` | 10 | `max_results` | a page of results; more is a sign the query should be narrower |
| `MaxTitleLength` | 300 | one result's `title`, made one line and cut by the client | a title is a line; a longer one is a page's text in the wrong member |
| `MaxSnippetLength` | 1000 | one result's `snippet`, cut by the client | ten results at the bound are about 3,000 tokens, under the tool's output cap, so a search never spills, which would open a machine |
| `Timeout` | 30 seconds | one search, from the request to the end of the body | a service that searches in several steps answers in seconds; the runner's other limits are in the same range ([[008-tools]]) |
| `MaxResponseBody` | 1 MiB | the service's answer | ten results at the bound are about 15 KiB |

The description is `prompts/tools/web_search-v1.md`. It says when to
search and when to fetch: search for something whose address the
agent does not have, fetch an address it has, and fetch a result to
read more than its snippet. It asks the model to cite the URL of each
result it relies on, and says that a search may be charged and that a
refused search's reason is for the person.

The result lists the results in the service's order:

```text
1. Go 1.25 is released
   https://go.dev/blog/go1.25
   Go 1.25 is now available. This release brings ...

2. Release History
   https://go.dev/doc/devel/release
   go1.25.0 (released 2025-08-12) ...
```

| The service | The result | Outcome |
|---|---|---|
| answers results | the list above (`results/web_search/results-v1`); the `tool.result`'s `cost_usd_micro` is the answer's | `ok` |
| answers no results | "No results for <query>." (`results/web_search/none-v1`) | `ok` |
| refuses, with the error envelope | "The search service refused the search: <message>" and, after a 429 with `Retry-After`, when it may be tried again (`results/web_search/refused-v1`) | `error` |
| answers 5xx, or anything the contract does not describe | "The search failed: <detail>." (`results/web_search/failed-v1`) | `error` |
| passes `Timeout` | `results/web_search/timeout-v1` | `timeout` |
| the turn is interrupted | `results/web_search/canceled-v1` | `canceled` |
| no service is configured | "Web search is not available on this server." (`results/web_search/unavailable-v1`) | `error` |

A result's URL that is not http or https is dropped, a title or a
snippet is made valid UTF-8, a snippet past `MaxSnippetLength` is cut
and ends with "...", and results past `max_results` are dropped. The
detail of a failure names the status or the transport error and never
a credential.

### The contract

```http
POST <TOPOS_SEARCH_URL>
Authorization: Bearer <credential>
Content-Type: application/json
Accept: application/json

{"query": "latest stable release of the Go programming language", "max_results": 5}
```

```json
{"results": [
  {"title": "Go 1.25 is released", "url": "https://go.dev/blog/go1.25",
   "snippet": "Go 1.25 is now available. This release brings ..."}],
 "cost_usd_micro": 10000}
```

| Member | Required | Meaning |
|---|---|---|
| `results` | yes | in the service's order; empty for no results |
| `results[].title` | yes | the page's title |
| `results[].url` | yes | the page's address, http or https |
| `results[].snippet` | no | the part of the page that matched, as plain text |
| `cost_usd_micro` | no | what the search was charged, in millionths of a USD; absent or zero for a service that charges nothing |

- `max_results` is always sent, the default included, so a service
  never applies a default of its own.
- A refusal is a 4xx with the error envelope
  `{"error": {"code": "...", "message": "...", "details": {...}}}`.
  `message` is one sentence written for the person whose session
  searched; the model reads it as the result and passes it on.
  `code` is recorded in the result's meta as `refusal`, a
  `WebSearchResultMeta` the API document names, for a client, and is not
  shown to the model. A 429 may carry `Retry-After` in seconds.
- A 4xx without the envelope, a 5xx, a body that does not decode, or a
  body past `MaxResponseBody` is a failure, never charged by the
  contract's reading: a service charges only a search it answered 200.
- Unknown members are ignored, so a service may answer more.
- The service decides everything the core does not: who may search,
  what it costs, whether a session's level pays for it, and how often
  one person may search.

The contract, with an example service, is written for a service's
author in `docs/web-search.md`.

### The credential

The client asks for its credential at each search, so a renewed key
reaches the next search, as the model connection asks for its key at
each request.

| The installation | The credential |
|---|---|
| mints session keys (`TOPOS_SESSION_KEYS_URL`) | the session's own model key for the runner's workload, from the drive's token source |
| mints none, `TOPOS_SEARCH_KEY` set | `TOPOS_SEARCH_KEY` |
| mints none, no key set | none: the request carries no `Authorization` header, for a service that admits the installation by its network alone |

[[018-credentials-and-secrets]] gains one rule, and the code comment
that says the key never leaves for another base URL is rewritten to
it: the session's key goes to two addresses, the installation's model
URL and its search URL, both the operator's settings, and to no
address an agent names. The sandbox's key is never sent: a search is
the runner's call.

`TOPOS_SEARCH_KEY` is refused beside `TOPOS_SESSION_KEYS_URL`, as
`TOPOS_MODELS_KEY` is: such an installation's sessions search with
their own keys.

### Cost

- `tools.Result` gains `CostUSDMicro`, and the harness copies it onto
  the `tool.result` as `cost_usd_micro` ([[004-session-log]], an
  optional member, so an old log reads as before).
- `session.Spent` and `session.ApplyBatch` add each `tool.result`'s
  `cost_usd_micro` to the spend, beside each `model.request`'s, so the
  session's header, its budget check before the next request
  ([[007-models]]) and every client that shows the spend count it. A
  redacted result keeps counting: redaction removes content, and the
  money was spent.
- A search is not refused by the core for the session's budget before
  it runs, since the core does not know its price. The search that
  passes the ceiling is counted, and the next request's check stops
  the turn `budget`, so a turn passes its budget by at most the
  searches of one step.
- The task suite's run cost adds tool results' costs as it adds model
  requests' ([[025-task-suite]]).

### Where it is offered

- `harness/tools` gains `NameWebSearch` and `WebSearch(Searcher)`, and
  `OptIn()`, the names of the built-ins an agent holds only by naming
  them, which is `web_search` alone. `tools.Builtins()` and the default
  set stay the eight, so the digest of an agent that names no tools is
  unchanged.
- The manifest validator accepts a name of `OptIn()` as it accepts a
  built-in's, with `outputLimit` alone, and refuses a client tool that
  takes it ([[003-manifest]]).
- The hosted runner and `topos run` add `web_search` to the registry
  when the agent's tools name it, with the client of
  `TOPOS_SEARCH_URL`; with the URL unset the tool is still offered and
  answers that search is not available, as `web_fetch` does on a
  machine without network. A thread holds it when its agent names it,
  as with every built-in ([[013-threads-and-subagents]]).
- The task suite offers it to a task that names it, with a fixed
  search service of canned results, and its instruction test is
  `test/tasks/instructions/web_search`.

### Configuration

| Variable | Read by | Default | Meaning |
|---|---|---|---|
| `TOPOS_SEARCH_URL` | `serve`, `runner`, `topos run` | unset | the search service `web_search` sends each query to: an absolute http or https URL; malformed stops the start |
| `TOPOS_SEARCH_KEY` | `serve`, `runner`, `topos run` | unset | the bearer sent to it when the installation mints no session keys; needs `TOPOS_SEARCH_URL`; refused beside `TOPOS_SESSION_KEYS_URL` |

Both join the table of [[002-scaffold-and-configuration]] and
`docs/configuration.md`.

## Open for the owner

1. **Does `web_search` join the default set later?** Recommended: no
   while a search may be charged. An agent's author decides that the
   agent searches.

## Not in this spec

- A deeper, slower search, domain and date filters, images, and an
  answer the service writes with its sources.
- Pricing, refusals and rates: the service's.
- A per-session count of searches in the summary.
- The instruction test against a real model in the instruction tier
  ([[025-task-suite]]); the scripted run is in this spec.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `web_search` is offered when an agent names it, by the hosted runner and `topos run`, and not otherwise; `tools.Builtins` and the default set stay the eight, with the digest an agent had; the validator accepts the name with `outputLimit` alone and refuses a client tool that takes it | `internal/hosted.TestSearchCredential`, `internal/toposcli.TestWebSearchIsOfferedWhenNamed`, `manifest.TestWebSearchIsAnOptInName`, `harness/tools.TestWebSearchSchemaFollowsTheConstants` | built |
| The schema states every bound from the constants, and the description holds none that differs | `harness/tools.TestWebSearchSchemaFollowsTheConstants`, `prompts.TestWebSearchDescriptionHoldsTheBounds` | built |
| The client sends `query` and `max_results` with the credential its function answers at that search, and reads results, cost, refusals with `Retry-After`, failures, a body past the bound and a timeout as the contract says | `search.TestTheClient`, `search.TestTheClientsFailures` | built |
| Each row of the result table renders its text and outcome; results past `max_results` and non-http URLs are dropped; a long title or snippet is cut | `harness/tools.TestWebSearchResults`, `search.TestTheClient`, `prompts.TestEveryTextRendersItsCurrentBytes` | built |
| A result's cost is on the `tool.result`, and `session.Spent` and `ApplyBatch` count it, redacted or not; an old log reads as before; a search past the budget stops the turn `budget` before the next request | `session.TestSpentCountsToolCosts`, `harness.TestASearchCostIsCounted` | built |
| A hosted session searches with its own key, asked at each search; an installation without session keys sends `TOPOS_SEARCH_KEY`; a key that cannot be had closes the turn; the sandbox's key is never sent | `internal/hosted.TestSearchCredential` | built |
| `TOPOS_SEARCH_URL` is checked at start, and `TOPOS_SEARCH_KEY` is refused without it and beside `TOPOS_SESSION_KEYS_URL`; both are in spec 002's table and the configuration page | `internal/config.TestSearchVariables`, `internal/config.TestConfigurationTableMatchesTheSpec`, `internal/config.TestConfigurationPageNamesEveryRead` | built |
| `web_search` is allowed in every mode with risk 0, an `alwaysConfirm` pattern that names it asks, and a call never opens a machine opened on demand | `harness.TestWebSearchChangesNothing` | built |
| The instruction test exists, its task names the canned results, and its checker passes its scripted solution and refuses its scripted wrong one; a run's cost counts what its tools were charged | `test/tasks.TestEveryToolDescriptionHasAnInstructionTest`, `test/tasks.TestScriptedSolutions`, `test/tasks.TestTheCannedSearch`, `test/tasks.TestLoadTaskRefuses` | built |
| The instruction test passes against a real model in the instruction tier | `test/tasks.TestTheSuiteAgainstAModel` with the `instructions` tag | not built |
| The API document names `WebSearchResultMeta` and where a search's cost is | `internal/server.TestOpenAPIIsGenerated` | built |
| End to end on a server over the stub Lux and the stub session key routes, with a search service: an agent that names `web_search` searches, the service sees the hash the routes registered for the session's runner key, the next request holds the results, the `tool.result` carries the cost, the session's spend includes it, and no sandbox is opened; a refusal's sentence is the result the model reads, with its code in the meta | `cmd/toposd.TestASessionSearchesWithItsOwnKey` | built |

## Outcome

Built on 2026-10-05 as designed, but for the instruction test against a
real model, which needs a model and its spend and waits for the next
instruction-tier run. Where the build departs from the draft:

- **The effect is `none`, not `read`.** The draft chose `read`. The
  harness opens a machine opened on demand before any call whose effect
  is not `none` (`harness.execute`), so a session that only searched
  created a Cella sandbox, which the end-to-end test showed. `none` is
  also allowed in every mode with risk 0, so nothing else changes.
- **A title is bounded too.** `MaxTitleLength`, 300 characters, joins
  the constants: a title is made one line and cut, and with the snippet
  bound a search's text stays under the output cap, so it never spills,
  which would open a machine.
- **A key that cannot be had closes the turn.** On an installation with
  session keys, the search's key is the model's key, so a failure to
  have it at the turn's setup closes the turn with
  `model_credential_missing`, as the model connection does. A URL the
  client refuses closes it with `search_unavailable`; configuration
  checks the URL first, so only a programmatic caller meets it.
- **A cost of zero is no cost.** The result carries `cost_usd_micro`
  only when the service reported more than zero.
- **The task suite names its results in `task.yaml`.** `search` names a
  file of results the suite's service answers; a task whose agent holds
  `web_search` must name one, and one that does not hold it must not.
