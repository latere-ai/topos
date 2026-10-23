# Offers: an agent proposes standing work, and the person answers

An agent that holds the `trigger` tool can offer the person it works for
standing work: an ask it then carries out again, with nobody waiting, on
a schedule or when an event arrives. It can also change, pause, resume or
delete the triggers that person holds of it. An offer, a change, a resume
and a delete wait for the person every time, in every approval mode, and
are done only on that person's yes. A pause, which only stops work, runs
at once. This page is the contract a client follows to show an offer and
send the answer. Everything here is on the wire of `/v1`.

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

An offer is an `agent.tool_use` whose `payload.name` is `trigger` and
whose `verdict` is `ask`, with no `user.tool_confirmation` of its
`tool_use_id` after it. The session goes idle with `stop_reason`
`tool_confirmation`, as for any call that waits for a person. The input's
`action` says what the person is asked:

| `action` | The person is asked to |
|---|---|
| `propose` | make a new trigger |
| `change` | change the trigger `id` names, only in the fields the input names |
| `resume` | run again the trigger `id` names, which was paused |
| `delete` | delete the trigger `id` names; the sessions it started keep going |

A `list` and a `pause` are never held. A pause's result tells the agent to
say what it paused and why, the trigger's `spec.suspend` reads `true`, and
its `status.paused` keeps who paused it, when, and the agent's why:

```json
"paused": {"why": "You are away this month.", "by": "https://login.example|alice", "sessionId": "ses_01...", "toolUseId": "toolu_01...", "at": "2026-10-23T09:00:00Z"}
```

`sessionId` and `toolUseId` name the conversation's call that paused it;
a trigger suspended at the API names neither and has no `why`. The record
stays while the trigger is suspended, through any change, and clears
when it resumes.

| Input | Meaning |
|---|---|
| `id` | for `change`, `resume`, `delete` and `pause`: the trigger's `trg_` id |
| `name` | the trigger's name, as the person would call it |
| `ask` | what each run asks, as a message from the person to the agent |
| `why` | one sentence that says the schedule or the event back and what the person gets |
| `schedule` | five cron fields of a minute and an hour, every day or on the days of the week listed, such as `0 9 * * 1` |
| `timeZone` | the IANA time zone the schedule is read in |
| `on` | in place of `schedule`: the events each run starts on, as a Trigger's `on` |

Show the offer as a choice between two answers: do it, or leave it as it
is. A Go client reads a schedule into days and a time with
`routine.ParseSchedule` of `latere.ai/x/sdk-go/agents/routine`, and reads
a change's trigger by its `id` at `GET /v1/triggers/{id}` to show what
changes.

An offer the installation would refuse, such as one past what the person
may hold, or a change of a trigger that is not the person's to change from
this session, never reaches the person: the call's `tool.result` answers
it at once, and there is no `agent.tool_use` to show.

## Answering

Only the person who started the session answers an offer. Another
sender's `user.tool_confirmation` of a `trigger` call is refused
`forbidden`, whoever else may send to the session.

```http
POST /v1/sessions/ses_01.../events
{"type": "user.tool_confirmation", "payload": {"tool_use_id": "toolu_01...", "decision": "allow"}}
```

`deny` leaves everything as it was. A reply the person types in place of
an answer is a `deny` whose `note` carries their words: the agent reads
the note in the call's result and may offer again from it.

```http
POST /v1/sessions/ses_01.../events
{"type": "user.tool_confirmation", "payload": {"tool_use_id": "toolu_01...", "decision": "deny", "note": "Make it 8:00"}}
```

## What a yes makes

On the person's allow, the server does the call as that person, in the
session's context, and the call's result names the trigger. A proposal
is written in the one shape every client reads as a routine, by
`latere.ai/x/sdk-go/agents/routine`:

- its name is `routine-` and 20 hexadecimal digits made from the session
  and the call, so a retry of the call applies the same trigger;
- its label `applied-by` names the session's agent;
- a proposal on events a routine starts from, every email that arrives
  (`{"product": "mail", "verbs": ["received"]}`) or every call of the
  person's link (`{"product": "webhook", "verbs": ["received"]}`), is the
  routine that starts from an email or a link;
- its `status.origin` says a conversation offered it:

```json
"origin": {"kind": "proposal", "sessionId": "ses_01...", "toolUseId": "toolu_01...", "confirmedBy": "https://login.example|alice"}
```

A proposal on any other events, such as a repository's pushes or a
filter narrowed to some resources, is no routine a client could show or
change as one. It is written the same way, with the filter as its `on`
and the event that started each run after the ask, under the name
`event-` and the same 20 digits, so a client that lists routines leaves
it out and shows it as a trigger the conversation offered, by its
`status.origin`.

A change keeps every field the call does not name, and a routine stays a
routine: a change of a routine to events no routine starts from is
refused, and the agent proposes a new trigger instead. The person
changes, pauses and deletes any of these like any trigger, and each keeps
its origin through every change, the agent's included.

## What the authorizer is asked

Every question of the tool is asked as the session's initiator, with
`org_id` of the session's context as the claims, since no request of
theirs is in hand. A held call asks its question twice: once before it is
held, with `origin` naming the session and the call, and once after the
allow, with `confirmed_by` added. A proposal asks `trigger.create`, a
change, a pause and a resume `trigger.update`, and a delete
`trigger.delete`, the last three about the trigger as it is stored. A
pause asks once, with no `confirmed_by`. A list asks `trigger.list`
naming the `owner` and the `agent`. An authorizer that knows no `origin`
reads the question as its owner's own apply, so an installation's
authorizer that holds the confirmation to the owner rolls out before the
server that asks it.
