# Messages sent while the agent works

A person does not have to wait for the agent to finish before writing
again. A message sent while a turn runs waits, and the agent reads it at
its next step, after the results of the commands it is running, with a
line that says it was sent while it worked. A person who cannot wait
sends it to be read now: the agent stops what it is doing, moving a
long command to the background rather than killing it, and reads the
message at once. Until the agent reads a message, its sender can take
it back. This page is the contract a client follows. Everything here is
on the wire of `/v1`, and `api/openapi.yaml` states the same events
under `sendEvent`.

## Sending while the agent works

A message sent while the session is running is sent as any other:

```http
POST /v1/sessions/ses_A/events
{"type": "user.message",
 "payload": {"content": [{"type": "text", "text": "Use port 9000, not 8000."}]}}
```

The answer is `200` with the appended event. What happens to it depends
on who sent it:

| Sender | Read by |
|---|---|
| a person | the agent's next request in the same turn, after the results of the calls running when it arrived |
| a trigger's run or a wake | the first request of the next turn, which it starts once the current turn ends |
| anyone else, such as another session's message | the first request of the next turn, whatever starts it |

A trigger's or a wake's message never steers a turn already running. It
starts the turn after, unless the session is then waiting for the person
(an approval, a question, a client's tool): the person's answer starts
the turn, and that turn reads the waiting message after the answer.

The model reads a person's message after one line:
"Ada sent this while you were working:". So a client needs no wording
of its own for that.

## Showing a message's state

Every state is read from the log. A message is **waiting** when it was
appended while the session's turn ran, after a `session.status` with
`status: running` and before the status that closes that turn. A message
sent while the session was idle is not waiting: it started its turn, as
before.

| State | When | Show |
|---|---|---|
| waiting | no `messages.read` names it and no `user.withdraw` withdrew it | the message, marked as not read yet, with a way to withdraw it |
| read | a `messages.read` names its id | the message as any other |
| withdrawn | a `user.withdraw` names it before any `messages.read` does | nothing, or the message struck out |

`messages.read` is written by the runner just before the request that
reads the messages it names:

```json
{"type": "messages.read", "seq": 42,
 "payload": {"event_ids": ["evt_01J9Z3Q4W8KX6T0M2V5N7R0031"]}}
```

A client that shows the conversation in model order places a read
message at its `messages.read`, not where it was appended: that is where
the agent read it. A client that shows it where it was sent is right
too, as long as it marks when it was read.

Sessions recorded before `messages.read` existed have none. Their
messages were read by the next request: a `model.request` without
`names_reads` read every waiting message up to its `fold_seq`.

## Sending a message to be read now

Set `send_now` on a message the agent must read before it does anything
else:

```http
POST /v1/sessions/ses_A/events
{"type": "user.message",
 "payload": {"content": [{"type": "text", "text": "Stop, the server is already up."}],
             "send_now": true}}
```

On an idle session `send_now` changes nothing. While a turn runs, the
agent acts on it according to what it is doing:

| The agent is | What happens |
|---|---|
| waiting for the model | the request is canceled, and the turn ends |
| running commands | each running `bash` command moves to the background and keeps running, other calls get a second to return, and any still running after that are canceled |

When every call returns in that second, the turn goes on, and its next
request reads the message as above. Otherwise the turn ends with
`session.status` `idle`, `stop_reason` `interrupted` and `detail`
`send_now`, and the next turn starts at once and reads the message after
the line "Ada stopped your work to send this:". Show such a stop as the
person's own choice, not as an error.

A moved command's call is answered `ok`. Its result has what the command
wrote so far, the process id it keeps running as and the log it now
writes to, so the agent can read the log later or stop the command. A
call of a subagent is not moved: it is canceled, and the subagent can be
continued later.

A client's "send now" control belongs next to its send control while a
turn runs, for a message already typed. To stop the agent without a
message, send a `user.interrupt`.

## Withdrawing and editing a waiting message

The person who sent a waiting message can take it back before the agent
reads it:

```http
POST /v1/sessions/ses_A/events
{"type": "user.withdraw",
 "payload": {"event_id": "evt_01J9Z3Q4W8KX6T0M2V5N7R0031"}}
```

A withdrawn message is never read, its files are never written to the
agent's machine, and it starts no turn. A withdrawal is checked against
the log it is appended after, so it and the agent's read of the same
message never both stand. The refusals:

| Code | When |
|---|---|
| `invalid_request` | the payload names no `event_id` |
| `conflict` | the event is no message of this session; or it was sent while the session was idle and started its turn; or the agent already read it; or it was already withdrawn |
| `forbidden` | someone else sent the message |

The case of a `conflict` is in its `detail` for a developer. A client
words its own sentence from the message's state in the log, which it
already shows: "The agent has already read this message." for a read
one.

To edit a waiting message, withdraw it and send the new text as a new
message. A withdrawal asks the authorizer `session.interrupt`, as an
interrupt does, and spends nothing.

## Rolling out

The events and members here are new to `/v1`: `send_now` on
`user.message`, `user.withdraw`, `messages.read`, and `names_reads` on
`model.request`. A server that predates them refuses `send_now` and
`user.withdraw` as `invalid_request`, so a client shows its send-now and
withdraw controls only once the server it talks to takes them. An
authorizer that allows by event type needs no change: `user.withdraw`
is asked as `session.interrupt`.
