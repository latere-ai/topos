# Offers: an agent proposes a routine, and the person answers

An agent that holds the `trigger` tool can offer the person it works for
a routine: an ask it then carries out again on a schedule, with nobody
waiting. The offer waits for the person every time, in every approval
mode, and the routine is made only on that person's yes. This page is
the contract a client follows to show an offer and send the answer.
Everything here is on the wire of `/v1`.

## Who holds the tool

An agent holds the tool when its manifest names it, and only on a
server's hosted runner, which keeps triggers. A local `topos run` refuses
an agent that names it. A subagent's thread never holds it: the thread
that talks with the person offers.

```yaml
spec:
  tools: [read, write, bash, web_fetch, question, trigger]
```

## Finding the offer

An offer is an `agent.tool_use` whose `payload.name` is `trigger`, whose
input's `action` is `propose`, and whose `verdict` is `ask`, with no
`user.tool_confirmation` of its `tool_use_id` after it. The session goes
idle with `stop_reason` `tool_confirmation`, as for any call that waits
for a person.

| Input | Meaning |
|---|---|
| `name` | the routine's name, as the person would call it |
| `ask` | what each run asks, as a message from the person to the agent |
| `why` | one sentence that says the schedule back and what the person gets |
| `schedule` | five cron fields of a minute and an hour, every day or on the days of the week listed, such as `0 9 * * 1` |
| `timeZone` | the IANA time zone the schedule is read in |

Show the offer as a choice between two answers: make the routine, or
leave it at this once. A Go client reads the schedule into days and a
time with `routine.ParseSchedule` of `latere.ai/x/sdk-go/agents/routine`.

An offer the installation would refuse, such as one past what the person
may hold, never reaches the person: the call's `tool.result` answers it
at once, and there is no `agent.tool_use` to show.

## Answering

Only the person who started the session answers an offer. Another
sender's `user.tool_confirmation` of a `trigger` call is refused
`forbidden`, whoever else may send to the session.

```http
POST /v1/sessions/ses_01.../events
{"type": "user.tool_confirmation", "payload": {"tool_use_id": "toolu_01...", "decision": "allow"}}
```

`deny` leaves nothing behind. A reply the person types in place of an
answer is a `deny` whose `note` carries their words: the agent reads the
note in the call's result and may offer again from it.

```http
POST /v1/sessions/ses_01.../events
{"type": "user.tool_confirmation", "payload": {"tool_use_id": "toolu_01...", "decision": "deny", "note": "Make it 8:00"}}
```

## What a yes makes

On the person's allow, the server applies the routine as that person, in
the session's context, and the call's result names it. The trigger is
written in the one shape every client reads as a routine, by
`latere.ai/x/sdk-go/agents/routine`:

- its name is `routine-` and 20 hexadecimal digits made from the session
  and the call, so a retry of the call applies the same trigger;
- its label `applied-by` names the session's agent;
- its `status.origin` says a conversation offered it:

```json
"origin": {"kind": "proposal", "sessionId": "ses_01...", "toolUseId": "toolu_01...", "confirmedBy": "https://login.example|alice"}
```

The person changes, pauses and deletes it like any trigger, and it keeps
its origin through those changes.

## What the authorizer is asked

Every question of the tool is asked as the session's initiator, with
`org_id` of the session's context as the claims, since no request of
theirs is in hand. A proposal asks `trigger.create` twice: once before it
is held, with `origin` naming the session and the call, and once after
the allow, with `confirmed_by` added. A list asks `trigger.list` naming
the `owner` and the `agent`. An authorizer that knows no `origin` reads
the question as its owner's own apply, so an installation's authorizer
that holds the confirmation to the owner rolls out before the server that
asks it.
