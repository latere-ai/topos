# Quiet runs and notices: whether a run brought the person anything

A turn a trigger begins, a routine's run, an event's or the agent's own
wake, has nobody waiting for it. Topos records whether that turn brought
the person something, so a client marks only the runs worth a look and
leaves the rest quiet. This page is the contract a client follows to
read it. Everything here is on the wire of `/v1`.

## Who decides

A trigger says, in `spec.session.notify`, whether every turn its firings
begin brings its result to the person (`always`, the default) or the
agent decides (`judged`). A wake's turn is judged whatever its trigger
says. An agent decides with the `notify` tool, which its manifest names
and every runner offers to the session's own agent, not to a subagent:

```yaml
spec:
  tools: [read, web_fetch, wake, notify]
```

`judged` is refused for an agent whose latest version does not name
`notify`, since every turn would be quiet. A judged trigger's message is
a `user.message` with `firing_id` and `notify: "judged"`; a wake's
carries `wake` instead. The agent reads, with that message, that nobody
is waiting, that it calls `notify` only for what deserves the person's
attention now, and that ending the turn without it leaves the person
undisturbed.

## The outcome

The `session.status` that closes a turn a trigger's message began carries
`outcome`, on the idle status or, for a session created with
`end_on_idle`, on the end:

| The turn | `outcome` |
|---|---|
| its trigger always brings its result, and the turn is no wake's | `notified` |
| it called `notify` | `notified` |
| it waited for the person, on a held call, a question or a client's call, or asked them on the way about a site or a connection | `notified` |
| the agent holds no `notify` tool | `notified` |
| otherwise, ending `end_turn` | `quiet` |

A turn that ends on an error, a limit, a budget, an interrupt or a
computer that is away records none, and so does a turn a person began.

## Reading it without the log

The Session folds the latest of each, so a list shows them:

```json
"outcome": "quiet",
"notice": {"title": "The fare to Lisbon dropped to 89 EUR", "seq": 41, "at": "2026-10-24T09:01:12Z"}
```

`outcome` is the latest turn's that recorded one; `notice` is the latest
`notify`, its title, the sequence of the call's `tool.result`, and when.
The `tool.result` of a `notify` call carries the same title in its
`meta.notice`. A notice changes nothing outside the log: what a client
does with one, a notification on a computer, a mark in a list, is the
client's.

## Attended sessions

`spec.session.attended: true` makes each session a firing starts
attended: a `question` its agent asks waits for the trigger's owner, as
in a session a person started from a client that shows questions, and the
turn records `notified`. Without it a question is answered at once as
`unanswered`. A trigger under `session.target` continues a session that
keeps its own `attended`.
