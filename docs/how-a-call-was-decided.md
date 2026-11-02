# How a call was decided: the layer and the rule

Each tool call is decided by the first layer of the session's approval
rules that settles it, or, in `progressive`, by a decision service for
what the rules leave open. The log records which, so a client can show a
person whether a rule or what was learned from their answers decided a
call, without reading the free-text `reason`. This page is for a client
that shows decisions.

## What the log records

`agent.tool_use` carries two fields from v0.65.0:

- `decided_by`: the layer that decided the call, one of the values
  below.
- `decided_by_rule`: the rule within that layer, where one applies.

An event written before v0.65.0 carries neither. Show its `reason`
alone, as for a value your client does not know.

| `decided_by` | What decided | `decided_by_rule` |
|---|---|---|
| `network` | the session's network, for a `web_fetch` ([network.md](network.md)) | none |
| `always_confirm` | a pattern of the organization's `always_confirm` list, so the call asks | the pattern, or `memory` |
| `tool` | the call's tool holds it for a person, whatever the lists, the score or a decision service say | the tool's name |
| `plan` | `plan` blocked a call that changes something | none |
| `read_only` | the call reads and changes nothing, so it runs | none |
| `always_allow` | a pattern of the `always_allow` list | the pattern |
| `remembered` | a pattern the person allowed earlier in the session with `remember` | the pattern |
| `sandbox` | in `confirm`, a write that stays inside a sandbox runs | none |
| `score` | in `progressive`, the risk score and its thresholds | `below_flag_at`, `flag_at`, `ask_at` or `block_at` |
| `confirm` | in `confirm`, a call no other rule settles asks | none |
| `memory` | a call that reaches memory folders alone runs ([memory-folders.md](memory-folders.md)) | none |
| `question` | a question tool's call is itself put to a person ([questions.md](questions.md)) | none |
| `learned` | a decision service's suggestion decided | the service's model, such as `learned/3` |
| `fallback` | in the mode `decide`, the decision service did not answer, so the call asks. In `learned` and `shadow`, the rules decide such a call, and the call records their layer | none |
| `denials` | after repeated refusals, the session asks the person for each call that changes something until the person's next message ([Repeated refusals](#repeated-refusals)) | none |

A rule is named by what the rule says: a pattern is the organization's
or the person's own text, never the call's input.

## Rule or learned

Every value but `learned` is a rule: the same call under the same
settings is decided the same way. `learned` appears only when a decision
service's suggestion decided the call, in `progressive`, in the modes
`decide` and `learned` (`TOPOS_DECISIONS_MODE`,
[configuration.md](configuration.md)). In `confirm`, in `shadow`, and on
a person's own computer, the service only suggests. The call records the
rule that decided it, and its `suggestion` beside it.

In `learned`, the service decides only where it has evidence about the
person: its `allow`, its `block`, and an `ask` with `ask_kind: split` or
`ask_kind: wider`. For the other kinds of ask (`unsure`, `short`, `few`,
or a kind that Topos does not know), the call records the rule or the
score that decided it, with the suggestion beside it. If the service
does not answer, the call records the rule or the score that decided
it, with no suggestion.

An `allow` of the service can run a call that the risk score asks
about. `TOPOS_DECISIONS_LOOSEN` sets when it can, in `learned` and in
`decide` ([configuration.md](configuration.md)):

- `covered`, the default: the `allow` decides only if the suggestion
  says that the approvals of the person cover the call
  (`cover.covered: true`). If not, the rules and the risk score decide
  the call, and the call records the suggestion beside their layer. An
  older service sends no `cover`, so its `allow` never runs a call that
  the risk score asks about.
- `never`: the stricter of the `allow` and the verdict of the rules
  decides. The service can make a call wait or stop it. It cannot run a
  call that the risk score asks about.

With both values, the rules decide first where they set a ceiling:
`always_confirm`, a tool that holds its call, the session's network and
`block_at`.

A call a decision service decided keeps the service's `suggestion`, with
the approval probability and the thresholds. Show the service's
`reason` to the person, never to the agent.

A `suggestion` also records how much the person's answers settle it, if
the service sends this:

- `evidence_ratio`: how much of the suggestion the person's answers
  settle, from 0 to 1. It is near 0 for a call that is not like a call
  the person answered. It is near 1 for a kind of call that the person
  answered frequently.
- `ask_kind`: for an `ask`, one of these values.
  - `unsure`: the evidence ratio is less than 0.5. The person's answers
    say little about this kind of call, and more answers can move the
    suggestion.
  - `split`: the evidence ratio is 0.5 or more, and the person's answers
    about this kind of call disagree. In `learned`, the call waits for
    the person.
  - `short`: the evidence ratio is 0.5 or more, and the answers agree,
    but the approval probability is still between the thresholds. More
    answers that agree can move it past a threshold. In `learned`, the
    rules and the risk score decide.
  - `wider`: the call reaches wider than each call of its kind that the
    person allowed. In `learned`, the call waits for the person.
  - `few`: the person allowed calls of this reach, but not with
    sufficient considered answers, or not in sufficient sessions. In
    `learned`, the rules and the risk score decide.

An example of a `short` ask: the person allowed `publish` two times and
did not deny it. The approval probability is 0.80. The profile lets a
call run without a person only above 0.90. The service has evidence,
but 0.80 is between the thresholds, so the service asks. More allows
of `publish` can move the probability above 0.90. A service that does
not send `short` sends `split` for this ask, and in `learned` the call
then waits for the person.

An older service sends neither field, and an event that an older Topos
wrote has neither. Show such a suggestion without them.

Topos sends each decision to the service with a `source` that names the
mode and the layer: `topos/<mode>/<decided_by>`. The mode is `confirm`
or `progressive`, and the layer is the `decided_by` of the call. Some
examples are `topos/progressive/learned`, `topos/progressive/score` and
`topos/confirm/always_allow`. With the layer, the service can tell a call
that its suggestion decided from a call that a rule or the risk score
decided.

A call on a person's own computer is sent as `confirm`. A decision
without a layer is sent as `topos/<mode>`. Topos before this release
sent `topos/confirm` or `topos/progressive` for each decision.

The service learns from the person's answers. Topos sends it these
answers:

- A person's confirmation of a call that asked, such as a fetch of a
  host outside the session's network.
- A person's confirmation of a network approval. The approval names the
  call whose connection was refused, and Topos sends the answer as an
  answer to that call. A call can cause more than one approval. Topos
  sends one answer for each confirmation. If the person allows the host
  for the rest of the session, the answer applies to this call only.

`allow` is an approval and `deny` is a denial. Topos sends each answer
after the call's decision. A message in place of an answer is not an
answer. If the service does not receive an answer, the call or the
approval continues as it would without a service. Each answer also
tells how the person answered ([How the person answered](#how-the-person-answered)).

```json
{
  "type": "agent.tool_use",
  "payload": {
    "tool_use_id": "toolu_01...",
    "name": "bash",
    "input": {"command": "git push origin main"},
    "verdict": "ask",
    "reason": "on the organization's always-confirm list",
    "mode": "progressive",
    "decided_by": "always_confirm",
    "decided_by_rule": "bash(git push*)"
  }
}
```

## What a call reaches

`agent.tool_use` records `scope` from this release: the reach that the
call was decided on. Topos records it with the decision, before the call
waits for the person. Show it beside the command, for example "cannot
be undone" or "makes the app public". The command or the input names
the target. `scope` holds no name: no path, no host, no branch and no
name of an app.

| Member | Values | Meaning |
|---|---|---|
| `undo` | `none`, `reversible`, `recoverable`, `irreversible` | the call changes nothing; the working directory can undo it with no loss; a copy or another call can undo it; nothing can undo it, or other people saw it |
| `crosses` | `machine`, `remote`, `network`, `organization`, `public`, each `true` or `false` | the boundaries that the call crosses: the machine outside the working directory, another system, a host outside the session's network, a party that the organization does not own, anyone at all |
| `target_class` | `scratch`, `own`, `shared`, `primary` | a disposable thing; a thing this session made; a thing other people or the person's own files use; the default or a protected branch, production, a live release |
| `target_kind` | a word such as `path`, `ref`, `app`, `host` | the kind of the target |
| `contact` | `first`, `repeat` | the session's first contact with the target, or a later one |
| `source` | `service`, `topos` | where the scope comes from |

A member that is not there is not known. A boundary that is not in
`crosses` is not known. Do not show it as `false`.

`source` is `service` when the decision service read the call. The
scope then joins the reading of the service and the claims of Topos,
and each member is the wider of the two. `source` is `topos` when the
scope has the claims of Topos alone: in `plan`, without a decision
service, when the service did not answer, or when its suggestion has no
scope. Only the service reads a `bash` command, so a `bash` call with
`source: topos` has no other member. Show that the reach of the command
is not known, and not that the command is safe.

```json
{
  "type": "agent.tool_use",
  "payload": {
    "tool_use_id": "toolu_02...",
    "name": "publish",
    "input": {"access": "link"},
    "verdict": "ask",
    "decided_by": "tool",
    "decided_by_rule": "publish",
    "scope": {"undo": "irreversible", "crosses": {"remote": true, "public": true}, "target_class": "primary", "target_kind": "app", "contact": "repeat", "source": "topos"}
  }
}
```

Topos sends the decision service its claims about each call, and the
limits that the machine enforces for each call:

| Call | What Topos claims |
|---|---|
| `read`, `glob`, `grep`, `history`, `todo`, a question, `agent`, `send_message` | `undo: none` |
| `write`, `edit` | whether the path is outside the working directory; `undo: reversible` inside it on a machine that keeps checkpoints; a target of kind `path`, `own` in a sandbox and `shared` on a host or a person's own computer |
| `web_fetch` | `undo: none`; another system; outside the session's network or not; outside the organization or not, when the authorizer names the organization's hosts; first or repeat contact with the host |
| `publish` | another system; public for `access: link`, or a release of an app that anyone with its address opens; `irreversible` for `access: link`, `recoverable` otherwise; first contact when the call makes the app; a target of kind `app`, `primary` for a release and `own` for a preview |
| `bash`, an MCP tool, a client's tool, every other call | nothing |

The limits (`environment.holds`) are `egress` in a sandbox whose network
is `allowlist` or `none`, `protected_refs` when the repositories of the
session are on the installation's git host, and `checkpoints` when the
machine keeps a checkpoint of each turn. `environment.repositories` is
`private`, `public` or `mixed`: what the git host told a read with no
credential when the first machine of the session opened.

## How the person answered

A client can add four fields to a `user.tool_confirmation`. Set each
field that your client can measure. Do not set a field that it cannot
measure.

| Field | Type | Set to |
|---|---|---|
| `answer_ms` | integer, 0 to 86400000 | the milliseconds from when the question became visible to the person (on the screen, in a window that has focus) to the answer. If the person left and came back, add the times that the question was visible |
| `details_opened` | boolean | `true` if the person opened the details of the call before the answer: the full command or input, the change, or the target. `false` if the client showed a way to open them and the person did not. Do not set it if the client has no details |
| `suggestion_shown` | boolean | `true` if the client showed a part of the suggestion of the decision service before the answer: its verdict, its approval probability, its reason, or an explanation. `false` if it showed none |
| `surface` | `prompt`, `notification`, `queue`, `bulk` | where the person answered: the conversation, with the call in it; a system notification; a list of questions or reviews; one gesture that answered more than one call |

```json
{
  "type": "user.tool_confirmation",
  "payload": {
    "tool_use_id": "toolu_02...",
    "decision": "allow",
    "answer_ms": 5200,
    "details_opened": true,
    "suggestion_shown": false,
    "surface": "prompt"
  }
}
```

A field of another type, an `answer_ms` outside its range, or a
`surface` that is not in the list is `invalid_request`. A server before
this release refuses the four fields. Send them only to a server whose
`GET /v1/openapi.yaml` lists `answer_ms` in
`Event.user.tool_confirmation`. If a confirmation with them gets
`invalid_request`, send it one more time without them.

Topos sends the decision service these fields with each answer, also
for a network approval. It adds `waited_ms`, the milliseconds from the
event that asked to the confirmation, from the times of the log, and
`remembered`, `true` if the confirmation had `remember`. Topos never
sends the `note` or the pattern of `remember`. The application that the
person used is `sender.client`, which the server writes from the token,
and Topos does not send it to the service.

## Repeated refusals

A refusal is a call that did not run because of one of these causes:

- The risk score blocked it (`decided_by: score`, `block_at`).
- The session's network blocked it (`decided_by: network`).
- The decision service blocked it (`decided_by: learned`).
- The person denied its confirmation.
- The person denied a network approval that the call caused.

A block in `plan` is not a refusal. A call that changes something and
runs ends a run of refusals: an `allow`, a `flag`, or an `ask` that the
person allowed. A call that reads only, or that reaches memory folders
alone, does not count, and does not end a run. The refusals of the
subagents count with the session's own.

After 3 refusals in a row or 20 in the session, Topos records
`session.denials_reached` after the refusal that reached the limit.
`TOPOS_DENIALS_CONSECUTIVE` and `TOPOS_DENIALS_TOTAL` change the limits,
and the authorizer can set lower limits for a session
([configuration.md](configuration.md)).

```json
{"type": "session.denials_reached", "payload": {"consecutive": 3, "total": 7, "limit": "consecutive", "effect": "ask"}}
```

- `effect: ask`, on a turn that a person began: from the next call, each
  call that changes something asks the person, with `decided_by:
  denials`. A call that the rules block stays blocked. A call that reads
  only, and a call that reaches memory folders alone, run as before.
  The decision service records its suggestion, as in `confirm`. The
  person's next `user.message` ends this and sets the counts to zero.
- `effect: stop`, on a turn that a trigger began, and in a run of a
  rollout: no person is there to ask. The turn stops with the stop reason `denials`, and each subagent
  stops at its next step. The next turn that a person begins asks for
  each change, as for `effect: ask`, until the next message of that
  person.

When a subagent's run had a refusal, its result starts with this text,
and the `tool.result` of the `agent` or `send_message` call records
`refused`, the number of refusals, in its `meta`:

> 2 of this subagent's calls did not run: a rule or the person refused
> them. Check its work before you rely on it.

Each call tells the decision service how deep it was delegated
(`attributes.depth`: 0 for the session's own conversation, 1 for a
subagent that it started). An `agent` or `send_message` call tells what
it gives the subagent: `handoff.writes`, `handoff.external`, and, for
`agent`, `handoff.isolation`. Topos never sends the task of a subagent,
the messages or reasoning of the agent, the output of a tool, a `note`,
a remembered pattern, or a path, a host, a URL or a branch name as a
scope or a target.

## The organization of a call

A decision service keeps its records by an organization, a subject, and
an action. The subject is always the person who started the session.
The organization is one of these two:

- The subject of the person, which is the context of the person.
  toposd uses it for each call when `TOPOS_DECISIONS_ORG` is `initiator`,
  which is the default.
- The ID of the organization that owns the agent of the session. toposd
  uses it when `TOPOS_DECISIONS_ORG` is `agent` and an organization owns
  the agent ([configuration.md](configuration.md)).

A session of an agent that an organization owns shows the ID of the
organization as `org`. A session of the agent of a person has no `org`.

`agent.tool_use` records `decision_org`: the organization of the key
that toposd sent the decision of the call under. If your client reads
the records of a call at the decision service, use `decision_org` as
the organization. An example is a client that shows why a call asked.
An event without `decision_org` was keyed by the subject of the person
who started the session. An event in `plan` mode, or of an
installation without a decision service, has no `decision_org`. The
decision service did not receive such a call.

The answers of a person in the context of an organization and in the
context of the person are separate. An answer in one context does not
change a suggestion in the other context.
