# Wakes: an agent comes back to a conversation later

An agent that holds the `wake` tool can set the conversation's next wake:
at a time it chooses, a note it wrote to itself comes back into the same
session, with everything the conversation holds, so it can look again,
whether a price dropped, a reply came or a build finished. Nobody waits
for it. This page is the contract a client follows to show a pending
wake, the turn it starts, and a way to cancel it. Everything here is on
the wire of `/v1`.

## Who holds the tool

An agent holds the tool when its manifest names it, and only on a
server's hosted runner, which keeps the trigger a wake is. A local
`topos run` refuses an agent that names it, and a subagent's thread never
holds it.

```yaml
spec:
  tools: [read, web_fetch, wake]
```

## What a wake is

A wake is a one-shot trigger of the session's agent, applied as the
person who started the session, that continues this session once:

| Field | Value |
|---|---|
| `metadata.name` | `wake-` and the session id's ULID in lowercase, so a session has one wake |
| `metadata.displayName` | the wake's title, the line a person reads |
| `metadata.labels["applied-by"]` | the session's agent, by name |
| `spec.at` | when it is due |
| `spec.session` | `policy: continue`, `target` the session, `message` the note, read literally |
| `spec.maxAge` | `24h0m0s`: a wake that fires later than that after a stop of the server sends nothing |
| `status.origin` | `{"kind": "wake", "sessionId": "ses_...", "toolUseId": "toolu_..."}` |

It lists among the person's triggers at `GET /v1/triggers` like any
other. A set while one is pending replaces it, applying the same trigger
with the new time.

The bounds hold whatever sets it: a wake is at least 10 minutes and at
most 168 hours ahead, and inside the session's life, its `expires_at`.
After 10 wakes in a row with no message, answer or confirmation from the
person who started the session, the agent's next set waits for that
person, as any held call does, and only that person answers it: their
allow counts as their word, and the chain starts again.

## Reading the pending wake

The Session folds the pending wake, so a list shows it without reading
the log:

```json
"wake": {"trigger_id": "trg_01...", "at": "2026-10-24T09:00:00Z", "title": "Flight prices to Lisbon", "count": 2}
```

`count` is the wake's place in the chain of wakes with no word from the
person, from 1. `wake` is absent when none is pending.

The log records each change:

| Event | Payload |
|---|---|
| `session.wake_set` | `trigger_id`, `at`, `title`, `note`, `tool_use_id` of the agent's call, `count` |
| `session.wake_cleared` | `trigger_id`, `reason`: `canceled`, `ended`, `skipped`, or `refused` when the installation refused the wake's message, with `refusal` its reason; and `tool_use_id` for the agent's cancel or `sender` for a person's or a client's delete |

## The turn a wake starts

At its time the wake sends the note as a `user.message` from the sender
`trigger:<trg_id>`, with `firing_id` and a `wake` field:

```json
"wake": {"trigger_id": "trg_01...", "set_at": "2026-10-23T09:00:00Z", "due_at": "2026-10-24T09:00:00Z", "count": 2}
```

Show it as the agent looking again, not as the person's message. The
Session's `wake` clears at that message. A session that waits for a
person, on a confirmation, a question or its budget, holds the message
until it is answered, so a wake never spends past what the person
allowed.

In a session a trigger started, a wake's look counts toward that
trigger's weekly allowance: the message also carries `spend`, the week
of that trigger with its `trigger_id`, and a wake due while the week is
spent waits at its time until it is not, or until its `maxAge` passes.
See `docs/allowances.md`.

## Cancelling and ending

A client cancels a wake as any trigger:

```http
DELETE /v1/triggers/trg_01...
```

The session records `session.wake_cleared` with `canceled` and the
sender. The agent cancels its own with the tool, recorded with its call.

A wake ends with its session: the end route, the reaper's end of an
expired session, the archive and the delete each delete it, and the end,
the expiry and the archive record `session.wake_cleared` with `ended`. A
session created with `end_on_idle`, as a trigger's run is, is not ended
at an idle while a wake is pending: it stays open for the wake and ends
at the idle after its last.

## What the authorizer is asked

A set asks `trigger.create`, or `trigger.update` when it replaces a
pending wake, and a cancel `trigger.delete`, each as the person who
started the session, with `org_id` of the session's context as the
claims, and with `origin` `{"kind": "wake", "session_id", "tool_use_id",
"action"}`, `action` `set` or `cancel`, and, once the person allowed a
held set, `confirmed_by`. A create or an
update carries `at`. The applier is also asked `session.send` on the
session the wake continues.
