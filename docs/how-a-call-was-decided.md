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
person: its `allow`, its `block`, and an `ask` on answers that disagree.
Where it has too few answers, the call records the rule or the score that
decided it, with the suggestion beside it. If the service does not
answer, the call records the rule or the score that decided it, with no
suggestion.

A call a decision service decided keeps the service's `suggestion`, with
the approval probability and the thresholds. Show the service's
`reason` to the person, never to the agent.

A `suggestion` also records how much the person's answers settle it, if
the service sends this:

- `ask_kind`: for an `ask`, `unsure` if the person gave too few answers,
  or `split` if the answers disagree.
- `evidence_ratio`: how much of the suggestion the person's answers
  settle.

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
approval continues as it would without a service.

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
