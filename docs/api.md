# Building on the Topos API

This page is for someone writing a client: a chat application, an editor, a
command line or a service that starts agents. It says what the API's
objects are, the calls a client makes in order, how to authenticate, which
errors to branch on and where each limit is stated. The route-by-route
reference is the OpenAPI document, [The reference](#the-reference) says
where. To run a server to build against, see
[Running Topos locally](local.md); to run one for others, see
[Installing Topos](install.md).

Paths on this page start at `/v1`. A server mounted under a prefix answers
under that prefix instead, as `TOPOS_BASE_PATH` in
[Configuration](configuration.md) says, and a client takes the API root
from the `servers` entry of the document the server serves, never from a
constant.

## Concepts

**An agent** is a definition: a model, instructions, the tools it may use,
how it is approved, and the machine it works on. A client applies an `Agent`
manifest of `topos.latere.ai/v1` with `PUT /v1/agents/{name}`. The name is
unique within its owner, the caller's organization or else the caller. A
changed spec creates the next **version**, which sessions pin; a change to
the metadata alone creates none. Archiving an agent is permanent and must
be confirmed in the request.

**A session** is one conversation of a person with an agent. It belongs to
the person who started it, its initiator, and runs on a **runner**: on the
server (`hosted`) or in a client's own process. It has a status, `idle`,
`running` or `ended`, and a `stop_reason` that says why it last stopped: the
turn ended, a call or a question waits for a person, the budget ran out, the
person interrupted. A session stays open and idle between turns until it
expires, is ended, or is deleted.

**The event log** is the session's record, an append-only list of typed
events with a sequence number from 1. Everything that happens in a session is
an event: the messages of the person and the agent, each tool call and its
result, each model request, status changes, approvals, title and model
changes. The log is the source of truth. A client builds its view of a
session from the events and keeps its place by sequence number. The types
are listed under `EventType` in the OpenAPI document. Events a person sends
are the `user.*` types: `user.message`, `user.interrupt`, `user.withdraw`,
`user.tool_confirmation`, `user.tool_result` and `user.answer`.

**A turn** is the work between a person's message and the session going
idle again. `session.status` events mark it, and each carries the turn's
number.

**A fork** is a new session that starts from a point in another session's
log, at the end of a turn or just before a message of the person, so a
question can be asked again differently. The original stays as it was.

**A trigger** starts or continues sessions without a person present: on a
schedule, at a time or list of times, or on an event delivered to the API.
Its owner is the initiator of every session it starts. A **wake** is the
agent's own return to a conversation at a time it chose, and an **offer** is
an agent's proposal of standing work for the person to accept.

**The authorizer** decides, for each request, whether the caller may do it,
and can attach limits and context to the answer. A client meets it as
`forbidden` with a `reason`. See [Authorizer](authorizer.md).

Ids are strings with a prefix: `agent_`, `ses_`, `evt_`, `trg_`. Treat each
as opaque.

## Authentication

Every route but `GET /v1/openapi.yaml` takes a bearer token:

```http
Authorization: Bearer <token>
```

The token is a JWT that an issuer the server lists in `TOPOS_OIDC_ISSUERS`
signed, or that the server's own local issuer signed (`toposd token`). The
server accepts it when:

- the issuer is one it lists, and the signature verifies against that
  issuer's keys;
- its audience is one of `TOPOS_OIDC_AUDIENCE`, `topos` unless the operator
  set others, so ask your issuer for a token whose `aud` is that name;
- it is not expired, and was issued within the last day, whatever its
  `exp` says.

There is no anonymous access. The caller's identity is the issuer and the
`sub` claim, written `<issuer>|<sub>`. The `org_id` claim, when present,
names the organization the caller acts in, and agents and triggers are
named within it. A client mints tokens by signing in with the issuer, not
through Topos; the server only verifies them.

A token with no valid signature or audience, or none, is `unauthenticated`
(401). A valid token the authorizer refuses is `forbidden` (403).

## Flows

Each flow lists the calls in order. The request and response shapes are in
[the reference](#the-reference). The flows that have a page of their own
link to it, and that page is the contract: this page does not repeat it.

### Create an agent

`PUT /v1/agents/{name}` with the manifest as JSON or YAML. It answers 201
for a new agent and 200 for an existing one, with the manifest as stored,
defaults written out. `GET /v1/agents` lists agents, `GET
/v1/agents/{ref}/versions` lists versions, and `ref` is an id or a name.
A manifest that holds what looks like a secret is refused as
`manifest_holds_secret`; name a credential instead.

### Create a session and send the first message

`POST /v1/sessions` with `{"agent": "<id or name>", "message": "..."}`
creates the session and appends the first message. Optional members set the
budget, limits, machine, resources, title and metadata, and an
`Idempotency-Key` header makes the create safe to repeat. Two members decide
how the session behaves:

- `attended`: `true` when a person answers its questions through your
  client. See [Questions](questions.md#who-answers).
- `end_on_idle`: end the session when its turn ends, instead of leaving it
  idle and open for the next message.

The answer is the session, with `status` `idle` until a runner claims it.
Without a `message`, the session waits for one.

### Send a message

`POST /v1/sessions/{id}/events` with `{"type": "user.message", "payload":
{"content": [{"type": "text", "text": "..."}]}}`. On an idle session the
message starts a turn. On a running one it waits and the agent reads it at
its next step: see [Messages sent while the agent works](messages-while-working.md).
A message can carry images and files; see [Reading a session's
files](reading-files.md) and, for files from a person's own storage,
the `source` member in the reference. A waiting message can be withdrawn
(`user.withdraw`) or the whole turn stopped (`user.interrupt`). To edit a
message, fork before it: see [Editing a message](editing-a-message.md).

### Stream events

Open `GET /v1/sessions/{id}/stream`. It is Server-Sent Events: every event
from `from_seq` (1 if absent) and then each new one, each frame an `id` that
is the sequence number, an `event` that is the type and `data` that is the
event as JSON. After a disconnect, reconnect with `Last-Event-ID` set to the
last sequence you received; browsers' `EventSource` does it itself. With
`deltas=1` the stream also carries the live output of a response as
`delta` frames that have no id, are never replayed and are best effort, so
build the final text from the events. The stream closes after the event
that ends the session.

To read without streaming, `GET /v1/sessions/{id}/events?from_seq=N`
returns a page and a `next_cursor`, and a `Link` header names the next page.

A client that shows a session reads the session once, then follows the
stream and applies each event. For the cost and the to-do list from the
stream alone, see [Cost so far and progress](cost-and-progress.md); for how
full the conversation is, [The context report](context-report.md); for a
day's use by application, [Activity](activity.md).

### Answer a question

An agent that holds the `question` tool may ask the person. On an attended
session the question waits as an `agent.tool_use` with the session idle on
`stop_reason` `question`; send a `user.answer` with one entry per question.
See [Questions](questions.md).

### Answer a confirmation

A tool call that needs the person's approval waits the same way, idle on
`tool_confirmation`. Send a `user.tool_confirmation` with the call's
`tool_use_id`, or the `approval_id` of an `approval.requested`, and a
`decision` of `allow` or `deny`. A client can add how the person answered;
see [How a call was decided](how-a-call-was-decided.md). A client that
runs the tool itself answers with `user.tool_result`.

### Change a running session

`PATCH /v1/sessions/{id}` changes the model and reasoning level of the next
turn, the approval mode (`plan`, `confirm` or `progressive`), the title and
the metadata, and appends the event that records it. A session idle on its
budget or on `machine_waiting` continues with `POST
/v1/sessions/{id}/resume`.

### Fork a session

`POST /v1/sessions/{id}/fork` starts a new session from a turn boundary
(`at_seq`) or before a message of the person (`before_seq`, with the
edited `message`). A point that is neither is `invalid_fork_point`. A fork
is a session of its own, with the parent named in its `parent` member.
[Editing a message](editing-a-message.md) covers drawing the versions and
listing conversations.

### Find, list and keep sessions

`GET /v1/sessions` lists with filters and cursors, `GET /v1/sessions/summary`
counts them by what needs a person, and `GET /v1/sessions/search` finds
them by what was said: see [Searching sessions](searching-sessions.md).
`POST /v1/sessions/{id}/archive` removes a session from lists without
deleting it, `.../end` ends an idle one, `DELETE /v1/sessions/{id}` deletes
it, and `.../events/{event_id}/redact` removes an event's content
([Questions](questions.md#redaction) shows one use).

### Read a session's files

Files a message carried, files an answer showed, and files of the working
directory are read through `GET /v1/sessions/{id}/blobs/{digest}` and
`GET /v1/sessions/{id}/files`. See [Reading a session's files](reading-files.md).

### Triggers, wakes and offers

`PUT /v1/triggers/{name}` applies a trigger; `GET /v1/triggers` lists them;
`POST /v1/triggers/{ref}/fire` delivers an event or fires a schedule now,
and `GET /v1/triggers/{ref}/firings` shows what each firing did. The pages:

| Page | Answers |
|---|---|
| [Trigger times](trigger-times.md) | a trigger that fires at a list of times |
| [Trigger models](trigger-models.md) | the model a trigger's sessions start on |
| [Trigger runs](trigger-runs.md) | what each run did, cost, and when it fires next |
| [Trigger reach](trigger-reach.md) | what a trigger's runs may reach, and a run per scope |
| [Holding triggers](holding-triggers.md) | pausing every trigger at once |
| [Allowances](allowances.md) | what a watch may spend in a week |
| [Wakes](wakes.md) | an agent comes back to a conversation later |
| [Offers](offers.md) | an agent proposes standing work and the person answers |
| [Quiet runs](quiet-runs.md) | whether a run brought the person anything |
| [Unattended turns](unattended-turns.md) | how a turn nobody watches is decided |

### What an installation can add

Which of these a session has depends on the installation, and a client
reads the result in the session and its events.

| Page | Answers |
|---|---|
| [Connections](connections.md) | a person's other services, reached without their credentials |
| [A session's network](network.md) | where the machine may reach, and asking the person |
| [Publishing](publishing.md) | putting a session's folder online |
| [Web search](web-search.md) | agents that search the web |
| [Memory folders](memory-folders.md) | what a session keeps across sessions |
| [Reading back](reading-back.md) | reading what the context let go |
| [Titles](titles.md) | a title that follows the conversation |

## Errors

Every error is one JSON envelope, with the status of its code:

```json
{"error": {"code": "invalid_request", "message": "The request is not valid.", "details": {"detail": "..."}}}
```

- `code` is stable. Branch on it, never on `message`, which is one sentence
  for a person and may change.
- `details.detail` says what was refused, for a log. A `not_found` carries
  no details.
- A refused manifest lists `details.problems`, each with the `document`
  (from 1), the field `path` and a `detail`.
- A `forbidden` can carry `details.reason`, a snake_case token the
  authorizer chose, such as `not_owner`, and `details.limits`; a limit that
  resets says when, in `resets_at`. The authorizer owns that vocabulary.
- A `rate_limited` answers with `Retry-After`, in seconds.
- Each response carries `X-Request-Id`. Quote it when you report a problem.

The codes, by status:

| Status | Codes | Do |
|---|---|---|
| 400 | `invalid_request`, `invalid_manifest`, `manifest_holds_secret`, `unknown_reference`, `unsupported_version`, `confirmation_required` | fix the request; `confirmation_required` is an archive sent without `{"permanent": true}` |
| 401 | `unauthenticated` | get a new token |
| 403 | `forbidden`, `identity_refused`, `model_not_allowed` | do not retry; show the `reason` |
| 404 | `not_found`, `attachment_source_not_found` | the object does not exist or the caller may not see it, and the two look alike |
| 409 | `conflict`, `sequence_conflict`, `idempotency_conflict`, `file_unavailable`, `agent_identity_missing` | read the object again; the state moved |
| 413 | `payload_too_large`, `attachment_too_large`, `file_too_large` | send less |
| 422 | `invalid_fork_point`, `machine_unavailable`, `model_unknown` | change what the request names |
| 429 | `rate_limited` | wait for `Retry-After` |
| 500 | `internal` | retry with backoff; quote the request id |
| 503 | `authorizer_unavailable`, `files_unavailable`, `identity_unavailable`, `model_unavailable` | retry with backoff; a request with an `Idempotency-Key` is safe to repeat |

The reference lists the same codes under `Error`. Some responses name a
code in the body of the session rather than the response: a turn that fails
ends with a `session.error` event whose `code` and `retryable` say whether
sending the message again can help.

A write that may be repeated, such as a create or a message, takes an
`Idempotency-Key`; a repeat with the same key and body answers the first
answer with `Idempotent-Replayed: true`, and the same key with another
request is `idempotency_conflict`. The window and the rules are in the
reference under `sendEvent`.

## Limits

Each limit has one home. This page names the home, not the number.

| Limit | Home |
|---|---|
| Requests per minute for a caller, answered by `rate_limited` | the server's `RequestsPerMin` constant in `internal/server/server.go`; the reference names the code only |
| Open streams per caller | `streamEvents` in the reference |
| Size of a message, of its images and files, and of files read from a person's storage | `sendEvent` in the reference |
| Metadata, budget and limits of a session; repositories and context a create may attach | `createSession` in the reference |
| Page size of a list | each list route's `limit` parameter in the reference |
| Hosted sessions driven at once | `TOPOS_RUNNER_CAPACITY` in [Configuration](configuration.md) |
| Refusals before a session is handed back to its person | `TOPOS_DENIALS_CONSECUTIVE` and `TOPOS_DENIALS_TOTAL` in [Configuration](configuration.md) |
| Spend of a trigger's runs in a week | [Allowances](allowances.md) |
| A question's options and answers | [Questions](questions.md) |
| What an authorizer may lower or attach to a session | [Authorizer](authorizer.md) |

## The reference

The full reference is the OpenAPI 3.1 document `api/openapi.yaml` in this
repository. A running server also serves it, with no token, at
`GET /v1/openapi.yaml` (under `TOPOS_BASE_PATH` when that is set), with its
`servers` entry set to the API root of that installation. A test holds the
file equal to what a server generates for `/v1`, so the two do not drift.

The document lists every route with its `operationId`, the authorizer
actions it asks (`x-topos-actions`), its request and response schemas, and
examples. A client that is generated from it should treat an unknown event
`type` or an unknown member as something to keep and ignore, not a failure:
new event types arrive in releases, and the
[CHANGELOG](../CHANGELOG.md) says when. A client that sends a member an older
server does not know is refused with `invalid_request`, so check that the
server's document lists a member before you send it.

What a session does to its machine and the permissions a person gives are
on [Security](security.md).
