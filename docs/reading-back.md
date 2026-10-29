# Reading back what the context let go

A long session outgrows its model's context. Topos first clears old tool
results, then replaces the older part of the conversation with a summary.
The session's log still keeps every message, call and result as it was
written, but the agent only sees its context: a number a person gave an
hour ago, the exact error a command printed, or a page it fetched early
on is gone for it, or kept only as well as the summary put it.

An agent that holds the `history` tool reads its own conversation back
from the log. This page is for whoever writes an agent and decides
whether it should.

## Turning it on

Name the tool in the agent's manifest:

```yaml
apiVersion: topos.latere.ai/v1
kind: Agent
metadata: {name: release-helper}
spec:
  model: {name: anthropic/claude-sonnet-4.5}
  tools: [read, write, edit, bash, history]
```

An agent holds `history` only when its manifest names it; an agent that
names no tools holds the eight built-ins and not this one. A subagent
holds it when its parent does and its own tools do not leave it out.

A call reads the session's log and nothing else: it changes nothing,
never starts the session's machine, and is allowed in every approval
mode, `plan` included.

## What the agent is told

Where something was let go, the agent of a session that holds `history`
reads where to find it:

- a cleared result reads `[this result was cleared to save context;
  history reads it back with {"tool_use_id": "toolu_..."}]`, naming the
  call, so the agent reads the result again rather than running a
  command again that may change something, cost money, or answer
  differently now;
- a summary opens with the numbers of the events it replaces, and says
  that `history` reads them back or finds them by their words;
- the request that asks for a summary tells the model that the messages
  it replaces stay readable, so a long exact text, such as a file's
  content or a command's whole output, may be named in the summary
  rather than copied.

A session whose agent does not hold the tool reads these as before.

## What it reads

Each answer is a list of entries, one per event, each headed by the
event's number, its time and who wrote it:

```text
[#12 2026-10-19T09:12:04Z] Ada wrote:
Deploy to staging. The staging port is 8443.
Files: /work/attachments/evt_01.../staging.env

[#15 2026-10-19T09:12:31Z] You called bash, id toolu_01:
{"command":"curl -s localhost:8443/health"}

[#16 2026-10-19T09:12:32Z] bash returned ok, id toolu_01:
curl: (7) Failed to connect to localhost port 8443: Connection refused
```

The agent asks for one of three reads:

| Input | Reads |
|---|---|
| `{"from": 120, "to": 180}` | the events with those numbers; either bound may be left out |
| `{"tool_use_id": "toolu_01"}` | one call and its result |
| `{"query": "connection refused", "from": 1, "to": 400}` | the events whose text holds the words, case ignored, each with up to 400 characters around its first match, at most 20 to an answer; the range is optional |

- It reads the people's messages and the names of their files, the
  agent's own messages without its reasoning, each call with its input,
  each result with its outcome and text, a person's answers to the
  agent's questions, interrupts, and changes of the title. A picture a
  result returned is named by the file it was read from, not shown.
- It reads events as they were written: a clearing or a summary changes
  nothing it returns, and a summary itself is left out.
- It reads only the conversation of the agent that calls it, up to the
  call. A subagent reads its own task and work, never the session's
  conversation.
- An event removed from the session, such as a message holding a secret,
  reads as one line that says it was removed, with nothing of what it
  held.
- An answer holds at most 32 KiB. One that stops early ends with the
  input that reads on, such as `{"from":181,"to":400}`.

An output that was too long for a tool's result was never kept whole in
the log: the log, and so `history`, holds its start and its end, and the
result names the file on the machine that held all of it.

## What it costs

Each read is one tool call of the agent's turn, with its result in the
context like any other. The tool's description tells the model to read
back only what its context no longer holds.
