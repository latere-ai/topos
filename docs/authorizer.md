# The authorizer: how toposd asks who may do what

`toposd` decides nothing about a person. Before it serves a request that
reads or changes an agent, a session or a trigger, it asks one endpoint,
the installation's authorizer, whether the caller may take that action on
that object, and it does what the answer says. This page is the contract
for whoever writes that endpoint: when `toposd` asks, what it sends, what
it accepts back, what it does when there is no usable answer, and how to
check an endpoint before pointing `toposd` at it.

Without an authorizer, a built-in owner policy decides
([Without an authorizer](#without-an-authorizer)). With one, the owner
policy is not consulted at all, so the endpoint decides every question.

The envelope, the client with its cache and retry, the reserved probe id,
the scaffold an endpoint can be written on and the conformance suite are
the shared contract in the Go module `latere.ai/x/pkg/authz`. The numbers
below are those of the version `go.mod` pins. The action
names, resource kinds, resource fields and `limits` members are Topos's
own, in the Go package `latere.ai/x/topos/authorizer`. Where this page and
the code differ, the code is right.

## The shape of one call

`toposd` sends an HTTP `POST` to `TOPOS_AUTHORIZER_URL`, exactly as
written, with these headers:

```text
Authorization: Bearer <TOPOS_AUTHORIZER_TOKEN>
Content-Type: application/json
Accept: application/json
```

The body is the question ([The question](#the-question)). The endpoint
answers `200` with a JSON object ([The answer](#the-answer)). A deny is a
`200` with `"allow": false`. Any other status, including `401`, `403` and
`404`, is not a deny: it is no answer, and `toposd` fails closed on it
([Failing closed](#failing-closed)).

## When toposd asks

`toposd serve` asks once per request, before it writes anything. A runner
holds no authorizer setting and never asks: for the two questions that
arise in the middle of a turn it asks the server, which asks the
authorizer ([Questions from inside a turn](#questions-from-inside-a-turn)).

| Moment | Questions, in order |
|---|---|
| apply an agent | `agent.read` for each agent the manifest refers to; then `agent.update` when the name exists, else `agent.create` |
| read, list, archive or delete an agent | `agent.read`, `agent.list`, `agent.archive`, `agent.delete` |
| create a session | `session.create` |
| fork a session | `session.read` of the parent, `session.fork`, and, when the call carries a first message, `session.send` of the new session |
| send a message, a tool confirmation, a client tool's result or an answer | `session.send`, with `event_type` naming which |
| interrupt, or withdraw a waiting message | `session.interrupt` |
| end or delete a session | `session.read`, and only for a session the route would act on, then `session.end` or `session.delete` |
| read a session, its events, its stream, a blob or a file | `session.read`, once when the request arrives, not once per event |
| list, count or search sessions | `session.list`; a search then asks `session.read` of each session it found |
| change a session's model, effort, approval mode, title or metadata | `session.read`, then `session.update` |
| archive or unarchive a session | `session.read`, then `session.update` |
| resume a session | `session.resume` |
| redact an event | `session.redact` |
| read a day's activity | `session.activity` |
| apply, read, list or delete a trigger | `trigger.create`, `trigger.update`, `trigger.read`, `trigger.list`, `trigger.delete`; an apply also asks `agent.read` for the trigger's agent and, for a trigger that continues a session, `session.send` |
| read or set the trigger hold | `trigger.hold` |
| fire a trigger | `trigger.fire`; the firing then asks as the trigger's owner |
| a trigger fires, by schedule, by event or by that call | `trigger.runs` first, for a trigger whose reach splits by scope; then `session.create`, or `session.send` when the firing continues an open session |
| an agent's `trigger` or `wake` tool | `trigger.create`, `trigger.update`, `trigger.delete`, `trigger.list`, as the session's initiator |
| a turn's model cannot serve | `session.update`, as the session's initiator |
| a session is titled | `session.title`, as the session's initiator |

Three things follow from that table.

- **A deny can add a question.** A denied mutation of an existing object
  makes `toposd` ask the same caller `<kind>.read` about the same
  resource. The answer to that second question decides whether the
  caller hears `forbidden` or `not_found` ([What a deny becomes](#what-a-deny-becomes)).
  A `forbidden` `session.create` that carries a reason, for an agent the
  caller does not own, makes one more question, `agent.read` of that
  agent, to decide whether the reason may be shown. An endpoint has to answer the `.read`
  actions consistently with the mutations.
- **An allow can be acted on.** `session.end` and `session.delete` are
  asked only after the session is known to be endable or deletable, so an
  endpoint may revoke credentials or close records when it allows them.
  A fork whose first message is refused after `session.fork` was allowed
  writes nothing. `toposd` logs the refusal as a `session.fork` mutation
  of the new session with the refusal's code as its outcome, and sends the
  endpoint nothing about it, so an endpoint that recorded the fork at the
  allow keeps a record of a session that does not exist.
- **Tool calls are not asked.** Whether an agent's `bash` or `edit` call
  runs is decided by the session's approval mode, its network, the
  organization's lists and, when configured, a decision service, never by
  this endpoint. See [How a call was decided](how-a-call-was-decided.md).
  The authorizer's part in a session is what it grants when the session
  starts and at a send: the limits, the network, the connections.

The actions `session.rewind`, `session.append`, `session.handoff`,
`session.scope`, `credential.create`, `credential.read`, `credential.list`,
`credential.delete`, `memory_store.create`, `memory_store.read`,
`memory_store.list`, `memory_store.update`, `memory_store.delete` and
`memory_store.write` are in the vocabulary but no route asks them. An
endpoint still has to know them, because the conformance suite sends them
and an endpoint built on the scaffold answers `400` to a name outside its
vocabulary.

### Questions from inside a turn

Some questions have no request of the person's in hand: a trigger's
firing, asked as the trigger's owner, the session's `trigger` and `wake`
tools, and the two below, asked as the session's initiator. Each carries
the claims `{"org_id": "<organization, or empty for the person's own>"}`
alone, and a `request` object of empty strings.

- **A model that cannot serve.** When the model of a session that was
  routed (its `model_via` is set) fails with the gateway answering
  `upstream_error`, `provider_unavailable` or `upstream_timeout`, or a
  provider rejects a request (`failed_reason` `rejected`), or a model
  writes a tool call as text twice (`failed_reason` `tool_as_text`),
  `toposd` asks `session.update` with `model` the name the session was
  routed from, `current_model`, `current_model_via` and
  `current_model_route`, and `failed_model`, `failed_reason` and
  `failed_detail` (at most 1,024 bytes). An allow that names another model
  in `limits.model` moves the turn to it. Any other answer, an allow that
  names no model, the failed one or the current one, a deny or no
  answer, leaves the turn where it is: it ends with the gateway's error,
  or, for `tool_as_text`, keeps the response as the model wrote it. An
  endpoint refuses a `failed_reason` it does
  not know, because one that reads `failed_model` alone would move a turn
  on any rejection. A model the allow names that the runner cannot connect
  counts as a failed move, and the next question names it as
  `failed_model` with the reason it could not be connected in
  `failed_detail`.
- **A title.** After a turn a person started, for an agent whose latest
  version sets `spec.title`, `toposd` asks `session.title`. A deny, an
  error or an action the endpoint does not know leaves the title as it is.
  See [Titles](titles.md#the-model-it-runs-on).

## Configuration

| Setting | What it does | Where it is defined |
|---|---|---|
| `TOPOS_AUTHORIZER_URL` | the endpoint, an absolute `http` or `https` URL with no query or fragment; the question is posted to it as written. `http` is accepted on loopback, and elsewhere only with `TOPOS_AUTHORIZER_INSECURE=true` | [configuration.md](configuration.md) |
| `TOPOS_AUTHORIZER_INSECURE` | the consent to send the bearer and every question over plain `http` to a host other than loopback | [configuration.md](configuration.md) |
| `TOPOS_AUTHORIZER_TOKEN` | the bearer the endpoint requires; required whenever the URL is set | [configuration.md](configuration.md) |
| `TOPOS_AUTHORIZER_MESSAGE_TEXT` | whether a message's words may reach the endpoint | [configuration.md](configuration.md) |
| `TOPOS_ADMIN_SUBJECTS` | the admins of the owner policy; read only when no URL is set | [configuration.md](configuration.md) |
| `TOPOS_SESSION_KEYS_URL`, `TOPOS_SESSION_KEYS_TOKEN` | the endpoint's session key routes, a second URL | [Session keys](#session-keys) |
| `TOPOS_IDENTITY_URL` | refused without `TOPOS_AUTHORIZER_URL`: only the installation's authorizer checks the sessions its agents' tokens name | [configuration.md](configuration.md) |

A start with a URL and no token, a URL that is not absolute `http` or
`https`, or an `http` URL on a host other than loopback without
`TOPOS_AUTHORIZER_INSECURE=true`, exits 1. With that consent the bearer
and every question travel to the plain `http` address in the clear.

## The question

The body is one JSON object. Its keys are sorted on the wire.

| Field | Meaning |
|---|---|
| `subject` | the caller as `<issuer>|<sub>`: the issuer with its trailing slash removed, a vertical bar, and the token's `sub`. Two issuers that agree on a `sub` are two subjects. `toposd` never asks for an anonymous caller: a request with no valid token is `unauthenticated` before any question. The empty string is the subject of the probe |
| `issuer`, `sub` | the two halves of `subject` |
| `claims` | the token's verified claims, verbatim. `toposd` reads none of them to decide. A narrowed key's `token_use` and `authorization_details` are among them; see [Grants a key carries](#grants-a-key-carries) |
| `action` | one of [the vocabulary](#the-vocabulary) |
| `resource` | the object the action touches, flat: `kind`, `id` when the object exists, and the fields of [What each question carries](#what-each-question-carries) beside them |
| `request` | `id`, the request's id, for correlation only; `ip`, the peer address of the TCP connection, which is a proxy's address behind one, since `toposd` reads no forwarding header; `user_agent` |

```json
{
  "subject": "https://login.example.com|alice",
  "issuer": "https://login.example.com",
  "sub": "alice",
  "claims": {"aud": "topos", "iss": "https://login.example.com", "sub": "alice"},
  "action": "session.create",
  "resource": {
    "kind": "session",
    "agent": "agt_01",
    "agent_version": 3,
    "agent_owner": "https://login.example.com|alice",
    "runner": "hosted",
    "machine": "cella",
    "initiator": "https://login.example.com|alice",
    "permissions": [{"action": "lux:model.use", "resource": "claude-haiku-4-5"}],
    "session_id": "ses_01",
    "repositories": [{"type": "repository", "url": "https://git.example.com/acme/web.git"}],
    "model": "claude-haiku-4-5"
  },
  "request": {"id": "req_1", "ip": "203.0.113.4", "user_agent": "topos/0.70"}
}
```

The envelope and the way `resource` is flattened are the shared client's;
the resource fields are what `internal/server/sessions.go` builds for a
`session.create` of a user's own request. A `session.create` asked for a
trigger's firing adds `trigger_id`, `firing_id` and `attended`.

An endpoint ignores a field it does not know. A resource shape only
gains fields, and a `limits` member keeps its wire name and its meaning.

### What each question carries

An existing object is asked with its `id`; a create and a list carry none,
with the exceptions noted. Every question about an existing session
carries `agent` (the agent's id), `owner` (the session's initiator, a
rendered subject) and `runner` (`hosted` or `external`), beside the fields
of the action.

| Action | Resource fields beside `kind` |
|---|---|
| `agent.create` | no `id`; `name`; `owner` as `{"type": "organization", "id": ...}` only when the caller acts in an organization's context |
| `agent.read`, `agent.update`, `agent.archive`, `agent.delete` | `id`; `name`; `owner`, a rendered subject, or `{"type": "organization", "id": ...}` for an organization's agent |
| `agent.list` | no `id`; `owner` only when the request names whose agents it lists |
| `session.create` | no `id`; `session_id` (the id the session will have), `agent`, `agent_version`, `agent_owner` (a subject, or `{type, id}` for an organization's agent), `runner`, `machine` (`cella`, `host` or `device`), `initiator`, `permissions` (the pinned version's `{action, resource}` list, then `lux:model.use` for each model the agent names, its advisor and its subagents), `repositories` (`{type, url, ref?}` each), `model` (the name the session would start on), `agent_identity` when `TOPOS_IDENTITY_URL` is set. Added when they apply: `environment` and `workdir` beside `machine` `device`; `model_via` and `model_route` when the session would start on a routed name; `reasoning` and `effort`, one value under both names, and `model` as named, when the create names its model; `rollout`; `metadata`; `message_chars`, `attachments`, `links` and `tools_last_turn` for the first message of a trigger's firing; for a firing, `trigger_id`, `firing_id`, `attended`, `fired_by` and `reach` |
| `session.fork` | `id` of the session forked; every field of `session.create` for the new session, `initiator` the person forking; and `owner`, `parent`, `seq` (the end of the copy, 0 for a fork before the opening message) and `root` (the root of the fork tree the new session joins, equal to `session_id` when the fork starts a conversation of its own) |
| `session.read` | `id`; the three common fields. A list or a search, `session.list`, carries no `id`: `status`, `runner` and `agent_owner` of the context. A conversation folder's list adds `origin` |
| `session.send` | `id`; the common fields; `sender`; `event_type` (`user.message`, `user.tool_confirmation`, `user.tool_result` or `user.answer`); `model`, `model_via` and `model_route` while the session has them; `idle_seconds`, the whole seconds since its last model request ended, absent before the first. For a `user.message`: `message_chars` (characters of its text), `attachments`, `links` (web addresses in its text) and `tools_last_turn`; `askable`, only when true; `answers`; `sources`; and `message_text` ([Message text](#message-text)) |
| `session.interrupt` | `id`; the common fields; `sender`; `event_type` (`user.interrupt` or `user.withdraw`) |
| `session.end`, `session.delete` | `id`; the common fields |
| `session.redact` | `id`; the common fields; `event_id` |
| `session.resume` | `id`; the common fields; `stop_reason`; `max_cost_usd_micro` when the body names one |
| `session.update` | `id`; the common fields; `session_id`; and what the change names: `model`, `effort` and `reasoning` (one level under both names), with `current_model`, `current_model_via` and `current_model_route`; `approval_mode`, `current_approval_mode` and `agent_approval_mode`; `title`; `archived`; `metadata` (a string sets a key, `null` deletes it) with `current_metadata`. A turn's failover adds `failed_model`, `failed_reason` and `failed_detail`, with `model` the routed name |
| `session.title` | `id`; the common fields; `session_id`; `model` (the name the title request asks for); `current_model`, `current_model_via` and `current_model_route`; `title`, empty for none |
| `session.activity` | no `id`; `day` (`YYYY-MM-DD`, UTC); `sender` only when the request narrows to one |
| `trigger.create`, `trigger.update` | `name`, `agent`, `owner`; `on` for an event trigger; `at` or `times` (RFC 3339) for a one-shot or a list; `notify`; `machine`, `environment` and `workdir` for a trigger whose session names a person's computer; `reach` as `{kind, split?}`; `budget` as `{per_week_usd_micro}`; `origin` for a call of the session's `trigger` or `wake` tool. A `trigger.create` carries the `id` that the new trigger will have |
| `trigger.read`, `trigger.delete`, `trigger.fire` | `id`; `name`, `agent`, `owner`; a deletion by a session's tool adds `origin` |
| `trigger.list` | no `id`; `owner` and `agent` as the request or the session's tool names them |
| `trigger.hold` | no `id`; `owner`, the caller; `held` when it sets the hold |
| `trigger.runs` | `id`; `name`, `agent`, `owner`; `reach`; `firing` as `{id, at, origin}`; `last`, `{"<scope>": "<RFC 3339>"}` for each scope's latest run |

`origin` of a trigger question is `{"kind", "session_id", "tool_use_id",
"action", "confirmed_by"}`; `action` is `propose`, `change`, `pause`,
`resume` or `delete`, or `set` or `cancel` for a wake, and `confirmed_by`
is present only on the question asked after the person allowed a held
call. [Offers](offers.md#what-the-authorizer-is-asked) and
[Wakes](wakes.md#what-the-authorizer-is-asked) describe those calls.
`fired_by` is `{"token_use", "authorization_details"?}` of the token that
fired a trigger when a key minted it; `toposd` reads nothing in it. A
`reach` on a firing's `session.create` is `{"kind", "scope"?, "since"?}`.

`sources` lists the files of a person's own files that a message names, as
`{"owner", "path", "version"?}`, the sender's own subject standing for the
alias `me`; an endpoint may hold the sender to them. `answers` is the ask
of an earlier refusal that this message answers, at most 512 bytes.

### Message text

A question about a person's message carries its shape and never its words,
unless two things both hold: the operator set
`TOPOS_AUTHORIZER_MESSAGE_TEXT` to `true`, and the session's last
allow named a model and `"message_text": true`. Then `message_text` is the
text blocks of the message joined by a blank line: whole up to 2,000 code
points, and otherwise its first 1,500, a line holding an ellipsis, and its
last 500. It is absent when the message has no text, for a create's first
message (the session has no allow yet), and for a message of any other
event type. The words are built for the question and kept nowhere.

## The vocabulary

The core is named `topos`. Each action acts on one kind and the kind never
changes. An action string never changes and never disappears.

| Kind | Actions |
|---|---|
| `agent` | `agent.create`, `agent.read`, `agent.list`, `agent.update`, `agent.archive`, `agent.delete` |
| `session` | `session.create`, `session.read`, `session.list`, `session.send`, `session.interrupt`, `session.end`, `session.delete`, `session.fork`, `session.rewind`, `session.resume`, `session.redact`, `session.append`, `session.handoff`, `session.scope`, `session.update`, `session.title`, `session.activity` |
| `trigger` | `trigger.create`, `trigger.read`, `trigger.list`, `trigger.update`, `trigger.delete`, `trigger.fire`, `trigger.hold`, `trigger.runs` |
| `credential` | `credential.create`, `credential.read`, `credential.list`, `credential.delete` |
| `memory_store` | `memory_store.create`, `memory_store.read`, `memory_store.list`, `memory_store.update`, `memory_store.delete`, `memory_store.write` |

A name ending in `.list` is a list action: its answer may carry a filter
([Filtering a list](#filtering-a-list)). `session.activity` is a read of a
collection and asks with no `id`. The client refuses an action outside
this table, or a resource whose `kind` is not the action's, before sending
anything, so a request that reaches the endpoint is always inside it.

The subject is `<issuer>|<sub>`. The initiator cap and the sender rule are
the endpoint's policy and not `toposd`'s: `session.create` carries the
initiator and the permissions the agent's pinned version declares, and
`session.send` carries the sender, and the endpoint refuses a session
whose agent could do more than its initiator, or a send from someone who
could not have started the session.

## The answer

The body of a `200` is one JSON object. Names are matched without regard
to case, a repeated name is accepted, and a member `toposd` does not know
is ignored.

| Field | Type | Meaning |
|---|---|---|
| `allow` | boolean | required. A body without it is no answer |
| `reason` | string | for a deny. A snake_case token of at most 64 characters, starting with a letter, is returned to the caller where `toposd` may disclose it; any other text is not ([What a deny becomes](#what-a-deny-becomes)). The conformance suite requires one on every deny |
| `ttl` | integer | seconds an allow may be cached. Absent, zero or negative: the default. Above the maximum: the maximum. The numbers are under [Caching](#caching) |
| `limits` | object | what an allow grants or routes, and, on a deny, a figure to hand back (`resets_at`). [Limits](#limits) |
| `filter` | object | on a list, `{"owners": [...], "labels": {...}}`. [Filtering a list](#filtering-a-list) |

```json
{"allow": true, "ttl": 30, "limits": {"budget_usd_micro": 5000000, "model": "vendor/model-a"}}
```

```json
{"allow": false, "reason": "budget_spent", "limits": {"resets_at": "2026-11-09T00:00:00Z"}}
```

```json
{"allow": true, "filter": {"owners": ["https://login.example.com|alice"]}}
```

### Limits

`limits` is read on an allow of the actions in the second column and
nowhere else; on any other allow it is ignored without being checked. A
member `toposd` knows that does not decode, or breaks a rule below, makes
the request fail as `authorizer_unavailable` and writes nothing, because a
ceiling it cannot read is one it cannot apply. The error's detail names
the member. A member left out means no change: the session keeps its
agent's value, or at a send the value it has.

Where a figure meets the agent's and the request's own, the session takes
the lowest. Lists and thresholds merge with the agent's approvals so that
neither side loosens the other.

| Member | Read on | Rule and effect |
|---|---|---|
| `always_confirm`, `always_allow` | `session.create`, `session.fork` | lists of permission patterns, merged with the agent's. See [How a call was decided](how-a-call-was-decided.md#what-the-log-records) |
| `thresholds` | same | `{flag_at, ask_at, block_at}`, numbers with 0 <= flag_at <= ask_at <= block_at <= 1; each lowers the agent's cut-off to the lower of the two |
| `budget_usd_micro` | same, and `session.resume` | integer, 0 or more, a ceiling on the session's spend in micro-USD; 0 allows no model request. At a resume it caps the new budget |
| `turn_timeout`, `max_age` | `session.create`, `session.fork` | Go durations, such as `30m`, above zero; ceilings |
| `retention` | same | a Go duration above zero: how long the session is kept after it ends. Absent, until it is deleted |
| `scope` | same | the session's starting scope, a list of grant objects in the grammar of an agent's `permissions` |
| `owner` | `agent.create`, `agent.update` | `{"type": "user" or "organization", "id": ...}`, naming the owner of the identity an agent gets at its first apply when `TOPOS_IDENTITY_URL` is set. It confirms the owner `toposd` derives, the applier as a user or the organization of the context; a different one is refused `authorizer_unavailable`. Absent, no check |
| `model` | `session.create`, `session.update`, `session.send`, `session.title` | a model's name with no space around it. The session runs it in place of the name asked, and keeps the name asked as `via`. At a create that names its model, an allow that names another is refused `model_not_allowed`. A fork is not routed and starts on its parent's model. The model is checked after the question: one the installation cannot run refuses the request, `model_unknown` or `model_unavailable`. [The model a trigger's runs start on](trigger-models.md#what-the-authorizer-is-asked) shows the trigger case |
| `route` | with `model` | the routed name the endpoint resolved the name asked to; kept beside the model. An allow that names a model without it clears the route |
| `message_text` | with `model` | `true` asks for the opening of each later message ([Message text](#message-text)); kept beside the route, and cleared by an allow that names a model without it |
| `reasoning` | where `model` is read | absent keeps the session's level; `minimal`, `low`, `medium` or `high` sets the level of the next turn; `""` returns the session to its agent's own |
| `network` | `session.create`, `session.fork`, `session.send` | `{"mode": "open" or "allowlist" or "none", "hosts": [...], "ask": bool}`; only an allowlist takes `hosts` or `ask`; at most 512 hosts, each an exact name or one leading `*.`, never an address, a port or a single label. At a send, one that differs from the session's replaces it. See [A session's network](network.md#where-the-network-comes-from) |
| `boundary` | `session.create`, `session.fork`, `session.send` | `{"hosts": [...]}`: the hosts the organization owns, at most 256, under the same host rule. A send's answer that names one replaces the session's; one that names none keeps it. [What a call reaches](how-a-call-was-decided.md#what-a-call-reaches) |
| `denials` | `session.create`, `session.fork` | `{"consecutive": n, "total": n}`, each 1 or more; applies where lower than the installation's, so an answer makes a limit stricter and never turns one off. [Repeated refusals](how-a-call-was-decided.md#repeated-refusals) |
| `instructions` | `session.create`, `session.fork` | the initiator's standing instructions, text of valid UTF-8. [The initiator's instructions](network.md#the-initiators-instructions) |
| `repositories` | `session.create`, `session.fork` | repositories to attach, `{url, ref?, app?}`; at most 8 together with the request's, each an `https` URL with no credential, query or fragment; no URL or app named twice. `app` is `{slug, name, url, id?}`. See [Keeping an app going across sessions](publishing.md#keeping-an-app-going-across-sessions) |
| `context` | same | titled text `{title, text}`: a one-line title of at most 100 characters, non-empty text, at most 32 KiB of titles and texts together |
| `connections` | `session.create`, `session.fork`, `session.send` | the session's connections to other services; a send's replace them. [Naming a session's connections](connections.md#naming-a-sessions-connections) |
| `folder` | `session.create`, `session.fork` | `{url, owner, path}`: the folder of the installation's files store that keeps the session's attachments and files. `url` is an absolute `http` or `https` URL with no user, query or fragment; `path` is a clean relative path of two segments or more. Refused when `TOPOS_FILES_AUDIENCE` is unset |
| `memory` | `session.create`, `session.fork` | memory folders, at most 4. [Naming a session's memory folders](memory-folders.md#naming-a-sessions-memory-folders) |
| `runs` | `trigger.runs` | the scopes due, at most 16, each `{scope, since?}`. [Which scopes are due](trigger-reach.md#which-scopes-are-due) |

A fork reads the same members as a create, with two exceptions: it is not
routed to another model, and the repositories, connections, folder and
memory folders its parent's allow named are not carried, so the fork's own
allow names them again. A run of a rollout takes no `instructions`,
`context`, `repositories`, `connections` or `memory`, whatever the allow
names.

A send's allow that routes the session to another model changes it before
the message's turn and appends `session.model_changed` by the
authorizer. `toposd` decides nothing about which model a name stands for
or how long a provider keeps a prompt cache; `idle_seconds` is there for
an endpoint that does.

### Filtering a list

On a list action the allow may carry `filter`. It narrows what `toposd`
would list and never widens it.

- `owners` admits only the objects owned by one of the subjects named. For
  `session.list` the owner is the session's initiator. An empty or absent
  `owners`, as in `{}`, does not narrow by owner, on every list.
- `labels` admits only objects that carry every named label. The objects
  `toposd` lists carry no labels to the authorizer, so a filter that names
  labels admits nothing.

An allow of a list with no filter admits everything in the caller's
context. A list the owner policy answers for a non-admin carries
`{"owners": [<the caller>]}`.

## What a deny becomes

A deny is a decision, not a failure. `toposd` turns it into an HTTP answer
by the verb of the action and what it knows of the caller's reach:

| Denied | The API answers |
|---|---|
| a read, such as `session.read` or `agent.read` | `not_found`, the same answer as an object that does not exist, with no reason |
| a create, a list, or any action with no object `id` | `forbidden` |
| any other action on an existing object, such as an update, a send or a fork | `forbidden` when the same caller is allowed `<kind>.read` of that object, else `not_found` with no reason |

A `forbidden` carries the deny's `reason` in `details.reason` when it
matches `^[a-z][a-z0-9_]{0,63}$`, and the deny's `limits`, when they are an
object, in `details.limits`, so a deny for a bound that resets can say
when. A reason of any other shape is dropped. A session create's reason is
shown when the agent is the caller's own or the caller may read it; a
reason that is withheld withholds its limits too. Give a reason a person's
client can act on, and one that names nothing of another subject.

For a trigger's firing the same deny is not an API answer but the
firing's outcome: `refused` with the reason, or with the error code when
the deny has none. See [A trigger's runs](trigger-runs.md).

## Failing closed

`toposd` never reads an allow into the absence of an answer. These all
produce no decision:

- the call takes longer than the timeout, retries included;
- the connection is refused or reset, or closes before a response line,
  after the one retry;
- the status is anything but `200`;
- the body is not JSON, has no `allow`, has an `allow` that is not a
  boolean or a `ttl` that is not an integer;
- the body is larger than the decision bound.

For each of them the API answers `503` with the code
`authorizer_unavailable`, and nothing is written: no event, no object. A
test asks every route with an unreachable authorizer and checks that each
answers it and that the log and the objects are as they were. If the
second question of a deny (`<kind>.read`) has no answer, that is
`authorizer_unavailable` too. A `limits` object that does not decode is
the same refusal, as above.

How the other callers fail:

- A trigger's firing records `failed` when the authorizer was
  unavailable, and a redelivery runs it again; for a scope of a split
  reach, the next firing asks again. A deny records `refused`.
- A failover that cannot get an answer ends the turn with the gateway's
  error. A title that cannot get one leaves the title.
- An answer of `trigger.runs` that does not read, names more than 16 runs,
  names a scope outside the scope pattern or names one twice records the
  firing `refused` with the reason `runs_invalid`. The pattern is under
  [Which scopes are due](trigger-reach.md#which-scopes-are-due).

The retry happens once, only when the connection failed before any
response line arrived. A timeout after the request was sent, a non-`200`
and a body that does not parse are never retried.

An endpoint that wants to refuse something answers `200` with
`"allow": false`. One that answers `403` or `400` for an ordinary refusal
turns every refusal into an outage.

## Caching

The client keeps a decision cache, shared by every question the process
asks.

| Rule | Value |
|---|---|
| timeout for one call, retry included | 5 seconds |
| cached allow | the answer's `ttl` seconds; 60 when it names none; at most 600 |
| cached deny | 5 seconds |
| unavailable | never cached |
| entries | 65,536, the least recently used evicted first |
| decision body | at most 64 KiB |

The key is the whole question except `request.id`: the same `subject`,
`claims`, `action`, resource fields, peer address and user agent. A new
token has new claims, so it asks again. A question whose resource has no
`id` is never cached: a create, a list and `session.activity`. A
`session.create` is therefore asked every time, while a `session.read` of
a session, asked again with the same token inside the `ttl`, is answered
from the cache.

Because an allow is remembered, a revocation reaches `toposd` after the
`ttl` of the allow it replaces. An endpoint that must revoke at once
answers a short `ttl`, and one that lets an allow stand for a while does
not need to resend it.

`toposd` sets none of these. They are the shared client's, and
`NewAuthorizer` passes the client no option that changes them.

## Without an authorizer

With `TOPOS_AUTHORIZER_URL` unset, `toposd` decides itself with the owner
policy, a function of the question and of the owner the question names.
It makes no network call. The rows, in order:

1. The probe id is denied, with the reason `probe`.
2. An anonymous subject is denied, with `anonymous`.
3. An action outside the vocabulary, or a resource of another kind, is
   denied with `unknown_action`.
4. `session.activity` is allowed for an admin and for a caller naming
   themselves as `sender`; otherwise it is denied `not_owner`.
5. A list is allowed. For an admin it carries no filter; for anyone else it
   carries `{"owners": [<the caller>]}`.
6. A resource owned by an organization, or a create that names one as its
   owner, is allowed for an admin alone: the policy knows no
   organization's members. The denial is `not_owner`.
7. `session.create` and `session.fork` are allowed for an admin, or when
   the agent's owner (`agent_owner`) is the caller.
8. An admin is allowed everything.
9. The owner of an object that exists, the subject in its `owner` field,
   is allowed every action on it.
10. The create action of a kind is allowed on an object that does not
    exist yet.
11. Everything else is denied `not_owner`; a denied read answers
    `not_found`.

The admins are the subjects in `TOPOS_ADMIN_SUBJECTS`, each written
`<issuer>|<sub>`. The policy carries no limits, so a session on it has the
agent's own figures and the server's defaults. It also applies a narrowed
key's grants ([Grants a key carries](#grants-a-key-carries)), so a key
that grants one action is held to it without an authorizer. A claim the
policy cannot read as grants is denied with `grant`.

Features whose content comes from `limits` are therefore absent here:
there are no connections, memory folders, attached repositories, routed
models or organization boundary, and every firing of a trigger whose reach
splits by scope records `skipped_nothing_new`, since the owner policy
knows no kind and allows `trigger.runs` with no limits
([A trigger's reach](trigger-reach.md#which-scopes-are-due)).

`TOPOS_IDENTITY_URL` and `TOPOS_SESSION_KEYS_URL` need an authorizer and
stop the start without one ([configuration.md](configuration.md)).

## Grants a key carries

A personal access token can carry grants its holder chose, in the claims
`token_use` and `authorization_details`. A decision point intersects its
answer with them: an allow becomes a deny with the reason `grant` when the
token carries grants and none covers the action, qualified as
`topos:<action>`, and the resource's `id` when the grant names one. It
never turns a deny into an allow.

`toposd` does this for its own owner policy and does not repeat it for an
endpoint's answer. An endpoint applies the grants itself. One built on
`latere.ai/x/pkg/authz/server` gets it by construction, and the
conformance suite sends a token with grants and checks that a request no
grant covers is denied.

## Session keys

`TOPOS_SESSION_KEYS_URL` is a second URL on the authorizer, not the
question endpoint. With it set, `toposd` generates a model key and a
sandbox key for each hosted session and registers only their SHA-256, then
renews them while the session runs:

```text
PUT <TOPOS_SESSION_KEYS_URL>/<session id>/keys/<workload>
Authorization: Bearer <TOPOS_SESSION_KEYS_TOKEN>
{"value_sha256": "<hex digest>"}
```

A success names `{"expires_at": "<RFC 3339>"}`. A status of 500 or more,
or a connection error, is unavailable; a status of 300 to 499 is a
refusal carrying the error's `code`, such as `session_unknown` for a
session the endpoint has no record of. The endpoint therefore has to know
the sessions it allowed at `session.create`. See
[configuration.md](configuration.md).

## Checking an implementation

`authorizer/conformance_test.go` runs the shared conformance suite against
two endpoints: the shared stub, and a twenty-line decider on the shared
scaffold. The same call checks yours:

```go
conformance.Run(t, url, bearer,
	conformance.WithVocabulary(authorizer.Vocabulary()),
	conformance.WithSubjects(subjectA, subjectB),
	conformance.WithPageActions())
```

The imports are `latere.ai/x/pkg/authz/conformance` and
`latere.ai/x/topos/authorizer`. Put the call in a Go test of the module
that holds your endpoint, with the endpoint running at `url` and
`bearer` its bearer. There is no command-line form: `toposd check` is not
built yet ([configuration.md](configuration.md)).

The suite sends, for every action in Topos's table:

- the reserved probe id (`00000000-0000-0000-0000-000000000001`) as each
  subject and as the anonymous subject, and expects a `200` deny. An
  endpoint that allows it does not read the request;
- a wrong bearer and no bearer, and expects anything but `200`;
- an action outside the table, and expects `400`, not a deny;
- a token with a grant for one action on one resource, and expects a deny
  for another action on that resource and for that action on another
  resource;
- an ordinary request from each subject, and expects a `200` whose body is
  a decision: `allow` a boolean, `ttl` a positive integer when present,
  `limits` an object when present, `filter` with `owners` and `labels`
  alone, and a `reason` on every deny.

Without `WithPageActions()`, the suite also accepts a page of the
endpoint's own shape from a list action. `toposd` reads a decision only, so
an endpoint for it answers every list with one, and the empty
`WithPageActions()` above makes the suite hold it to that. The repository's
own run leaves the option out.

The suite proves the shape of the answers, not the policy behind them and
not the limits' contents. To hold your own answers to what `toposd`
reads, decode them with the same package:

```go
limits, err := authorizer.DecodeLimits(decision) // a Limits value, or the error toposd would refuse with
runs, err := authorizer.DecodeRuns(decision)     // for trigger.runs
```

and render an answer's `limits` through `authorizer.WireLimits`, as
`TestScaffoldSpeaksTheVocabulary` does. `authorizer.Vocabulary()`,
`authorizer.Known`, `authorizer.Kind` and `authorizer.Create` give the
table.

### Rolling out a change

A new action or a new field is a change to both sides. The order that
keeps a running installation working:

1. Roll the endpoint out first. An endpoint that does not know an action
   answers `400` on the scaffold, which `toposd` reads as unavailable for
   the routes that ask it; an endpoint that denies an unknown action makes
   that route `forbidden`. Either way only the new route is affected.
2. Then roll out `toposd` and the runners.
3. Then the clients that set a field the endpoint now reads, such as
   `attended` on a session create.

An endpoint that lists the `event_type` values it allows on `session.send`
adds a new one before the server that sends it is released.
[Questions](questions.md#for-the-installations-authorizer) describes the
newest, `user.answer`.
