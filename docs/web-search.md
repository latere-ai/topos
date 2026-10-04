# Web search: giving agents a search service

An agent that holds the `web_search` tool can look up what it has no
address for: a current fact, a library's latest release, the
documentation of a project. Each call sends a query to the search service
your installation configures and returns results the agent can cite, a
title, a URL and a snippet each. The agent then reads a result in full
with `web_fetch`.

Topos names no search provider. It sends every query to one URL and reads
one answer shape, so any service that answers the contract below works:
a thin adapter in front of the provider you choose, or a service of your
own. This page is for the operator who turns search on and for whoever
writes that service.

## Turning it on

1. Run a service that answers the contract.
2. Set `TOPOS_SEARCH_URL` to its URL. An installation without session
   keys also sets `TOPOS_SEARCH_KEY`, the bearer the service expects, if
   it expects one.
3. Name the tool in each agent that should search:

```yaml
apiVersion: topos.latere.ai/v1
kind: Agent
metadata: {name: researcher}
spec:
  model: {name: anthropic/claude-sonnet-4.5}
  tools: [read, write, web_fetch, web_search]
```

An agent holds `web_search` only when its manifest names it. An agent
that names no tools holds the eight built-ins and not this one, so an
agent never starts searching, and spending, because the installation
turned search on. With `TOPOS_SEARCH_URL` unset, an agent that names the
tool is still offered it, and every call answers that web search is not
available on this server.

A search changes nothing on the session's machine and never opens one,
so it is allowed in every approval mode, `plan` included. An
organization that wants each search confirmed names `web_search` in
`approvals.alwaysConfirm`.

## The request

```http
POST <TOPOS_SEARCH_URL>
Authorization: Bearer <credential>
Content-Type: application/json
Accept: application/json

{"query": "latest stable release of the Go programming language", "max_results": 5}
```

- `query` is 1 to 400 characters, the words the agent would type into a
  search engine.
- `max_results` is 1 to 10, and always present: Topos sends 5 when the
  agent names no count.
- The credential is the session's own model key when the installation
  mints session keys (`TOPOS_SESSION_KEYS_URL`). Your service can tell
  which session searched, and whose credit pays, the way your model
  gateway does: by the key your authorizer registered for the session.
  Without session keys it is `TOPOS_SEARCH_KEY`, and with neither the
  request carries no `Authorization` header.

The session's key goes to two addresses only, the model URL and the
search URL, both set by the operator. An agent cannot send it anywhere
else, and the key of the session's sandbox is never sent.

## The answer

```json
{"results": [
  {"title": "Go 1.25 is released", "url": "https://go.dev/blog/go1.25",
   "snippet": "Go 1.25 is now available. This release brings ..."}],
 "cost_usd_micro": 10000}
```

| Member | Required | Meaning |
|---|---|---|
| `results` | yes | in your order, the best first; empty when nothing matched |
| `results[].title` | yes | the page's title |
| `results[].url` | yes | the page's `http` or `https` address |
| `results[].snippet` | no | the part of the page that matched, as plain text |
| `cost_usd_micro` | no | what you charged for this search, in millionths of a USD |

- Topos keeps at most `max_results` results, drops a result whose URL is
  not `http` or `https`, and cuts a title past 300 characters and a
  snippet past 1,000.
- A search that charged records `cost_usd_micro` on its `tool.result`,
  and the session's spend counts it as it counts a model request's. A
  session with a budget stops its turn with `budget` once searches and
  model requests together pass it.
- Members Topos does not know are ignored, so the answer may hold more.

## Refusing a search

Answer a 4xx with the error envelope:

```json
{"error": {"code": "search_needs_credit",
  "message": "Searching the web needs a level that uses credit.",
  "details": {"detail": "..."}}}
```

`message` is one sentence for the person whose session searched. The
agent reads it as the result of its call and passes it on, so write it
for that person, not for a developer. `code` is recorded in the result's
meta as `refusal` (`WebSearchResultMeta` in `api/openapi.yaml`), for a
client that shows a refused search. A 429 may carry `Retry-After` in
seconds, and the agent is told when it may search again.

Anything else, a 5xx, a 4xx without the envelope, a body that does not
decode or is larger than 1 MiB, or no answer within 30 seconds, is a
failed search. The agent is told it failed, and nothing is charged by
Topos's reading of it: charge only a search you answered with 200.

Who may search, what a search costs, whether a session's model pays for
searches, and how often one person may search are your service's to
decide. Topos passes on what you answer.
