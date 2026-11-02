# Unattended turns: decisions nobody watches

A turn a trigger's message began, a wake's included, runs with nobody
watching. It is decided by the session's approval mode, as any turn is,
with one difference, and the log says which turns these are. This page
is for a client that shows decisions and an installation that runs a
decision service.

## What changes

On a turn a trigger's message began, the patterns the person remembered
in the conversation, by answering a held call with `remember`, are not
applied: they allowed the call with that conversation in front of them.
Everything else decides as on any turn: the agent's and the
organization's `always_allow` and `always_confirm` lists, the mode and
its thresholds, the session's network, a decision service, and a tool's
own hold. A call the mode would ask about waits for the person, as a
held call always does.

A turn the person's answer or confirmation resumes is theirs again.

No person is there to ask when the session's refusals reach a limit: a
turn a trigger's message began stops with the stop reason `denials`,
and the next turn a person begins asks them for every call that
changes something until their next message
([How a call was decided](how-a-call-was-decided.md#repeated-refusals)).

## What the log records

`agent.tool_use` and `approval.requested` carry `attended`: `false` on a
turn a trigger's message began and in a subagent that turn drove, `true` on a turn a person began. Events written before this field
carry none.

A decision service is told the same in the action it predicts and
records, as one of its attributes:

```json
"attributes": {"attended": false, "depth": 0}
```

so a decision service that learns a person's approvals can weigh a call
nobody sees, from that person's own answers. `depth` is how deep the call
was delegated: 0 for the session's own conversation, 1 for a subagent it
started.
