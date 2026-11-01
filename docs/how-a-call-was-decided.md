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
| `fallback` | the decision service did not answer, so the call asks | none |

A rule is named by what the rule says: a pattern is the organization's
or the person's own text, never the call's input.

## Rule or learned

Every value but `learned` is a rule: the same call under the same
settings is decided the same way. `learned` appears only when a decision
service's suggestion decided the call, in `progressive`, in the modes
`decide` and `learned` (`TOPOS_DECISIONS_MODE`,
[configuration.md](configuration.md)). In `learned`, the service decides
only where it has evidence about the person: its `allow`, its `block`,
and an `ask` on answers that disagree. Where it has too few answers, the
call records the rule or the score that decided it, with the suggestion
beside it. In `confirm`, in `shadow`, and on a person's own computer, the
service only suggests: the call records the rule that decided it, and its
`suggestion` beside it.

A call a decision service decided keeps the service's `suggestion`, with
the approval probability and the thresholds. Show the service's
`reason` to the person, never to the agent.

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
