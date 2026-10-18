---
title: "A title that follows the conversation: after a turn a person started, the harness asks a small model whether the session's title still names what the conversation is about and writes a new one when it does not, an agent turns it on with spec.title, and the sessions it ran before are named once"
status: drafted
track: core
depends_on: [.archive/004-session-log.md, .archive/006-identity.md, .archive/007-models.md, 010-context.md, 016-runners.md, 017-external-runners-handoff-fork.md, 025-task-suite.md, .archive/038-routed-models.md, .archive/049-reasoning.md, 054-a-sessions-title-and-filing.md, 061-a-messages-shape-and-the-route-an-allow-names.md]
affects: [manifest/v1/, manifest/, session/, harness/, runner/, authorizer/, prompts/, internal/hosted/, cmd/toposd/, test/tasks/, api/openapi.yaml, docs/]
effort: medium
created: 2026-10-19
updated: 2026-10-19
author: changkun
---

# A title that follows the conversation

## Overview

A session that stays open until a person deletes it is one long
conversation, and its subject moves: a person who asked about a release
note goes on to a migration and then to a test that fails. A list of
sessions shows each by its title, so the title is how a person finds the
conversation again. Today the title is whatever the create gave, usually
a client's cut of the first message, and it never changes unless the
person renames it.

This spec has the harness keep the title on the conversation's current
subject. After a turn a person started, it asks a small model, with the
title and the latest exchanges, whether the title still holds; the model
answers that it does or gives a new one, and a new one is written as a
`session.title_changed`. A person's own rename always wins. An agent turns
this on in its manifest, and the sessions it ran before are named once.

## Current state

- The title is the create's `title`, or a fork's copy of its parent's
  marked as its continuation, and a person changes it through
  `PATCH /v1/sessions/{id}` ([[054-a-sessions-title-and-filing]]). Each
  change is a `session.title_changed` `{by, old, new}`, and it does not
  move the header's `updated_at`.
- A compaction already sends a request of its own beside the thread's
  steps (`harness/context.go`, `summarize`), recorded as a
  `model.request` that counts toward the budget.
- A turn ends with `session.status` idle in `turn.finish`
  (`harness/harness.go`); the runner then releases the machine and its
  lease, or drives the next turn when input is pending
  (`runner/runner.go`, `drive`). A claim queue polls every
  `runner.DefaultPoll`, two seconds, when nothing wakes it.
- A session keeps the bundle of the agent version it was created on and
  runs that version for its whole life (spec 003).

## Design

### Turning it on

The agent's manifest gains `spec.title`, an object:

```yaml
spec:
  title:
    model: tier/quick
```

Its presence turns titles on. `model`, optional, is the name the title
request asks for; absent, the request asks for the name the session's
model was asked by (its `via`), or its model where it has none. Validation
refuses a `model` that is empty once trimmed.

Naming a session changes nothing the agent does, so the setting is read
from the agent's latest version, not from the version the session runs:
an agent that turns titles on names the sessions it already runs, and one
that turns them off stops. A runner that cannot read the agent's latest
version, such as an external runner without the store, reads the
session's own bundle.

### When the harness asks

After a turn of the session's own thread ends, the runner asks the
harness for a title when all of these hold:

1. the agent's `spec.title` is set;
2. the turn ended with the session idle (an ended session's log takes no
   more events);
3. the turn's input held a `user.message` whose sender is a person (a
   trigger's run keeps the name its routine gave it);
4. no input was pending when the turn ended (the next turn's end asks);
5. no `session.title_changed` in the log is by a person, copied events of
   a fork included: a person who named the conversation keeps the name;
6. the session's budget, where it has one, is not spent.

The request runs after the turn's closing `session.status` is committed,
so a client sees the turn end at once. The runner holds the session's
lease while it runs, and reads the log again afterwards: input that
arrived meanwhile starts the next turn on the same lease, as input
pending at the turn's end does, so a message sent during the title
request never waits for the queue's poll.

### What the model reads and answers

One request, built from `prompts/title/title-v1.md`, with no tools, no
reasoning where the model allows none (spec 049), `max_tokens`
`harness.TitleMaxTokens` (64), and a deadline of `harness.TitleTimeout`
(20 seconds) of its own, apart from the turn's. It carries:

- the current title, when the harness wrote it before; on the first
  naming it carries none and the model must name the conversation;
- the person's latest `harness.TitleMessages` (6) messages in order,
  their text, and the names of the files they carry;
- after each, the agent's final text of that turn, cut to
  `harness.TitleAnswerChars` (600) characters.

Reasoning, tool calls, tool results, summaries and redacted events are
left out: the subject is what the person asked and what the agent
answered. The prompt asks for one line, either `KEEP` or a title: a few
words that name the subject, in the language the person writes in, as a
heading, with no quotes, no closing period and no emoji, at most
`harness.TitleMaxLength` (60) characters. It keeps the title unless the
person's latest two messages are about something the title does not
cover, so a question on the side that the conversation comes back from
does not rename it.

The answer is trimmed and one surrounding pair of quotes removed. `KEEP`,
an empty answer, an answer `session.CheckTitle` refuses, one longer than
`harness.TitleMaxLength`, and a title equal to the current one write no
title.

### The model it runs on

Before the request, toposd asks a new question, `session.title`, of kind
session, as the session's initiator with its claims' `org_id` alone, as
the question of a turn that moves off a failed model does
([[051-a-turn-moves-off-a-model-that-cannot-serve]]). Its resource is
`session_id`, `model`, the name asked, `current_model`,
`current_model_via` and `current_model_route`, the model the session
stands on and its names, and `title`, the current title. An allow may
name `model` and `route` as an allow of a create does
([[038-routed-models]], [[061-a-messages-shape-and-the-route-an-allow-names]]).
A deny writes nothing and asks nothing more for that turn.

The request runs on the model the allow names, else on the name asked,
when the installation runs it. When the agent names a title `model` and
neither runs, no request is sent: a title never falls back to the
session's own model behind the agent's back, since that model may cost
far more. When the agent names none, the name asked is the session's own,
and the session's current model is the last fallback.

### What it writes

The request is recorded as a `model.request` with a new member,
`purpose`, `title`, and the prompt's version; absent `purpose` is a step
of a thread, as before. It counts toward the budget as any request does.
`harness.Rebuild` builds a title request again from the log through its
`fold_seq` and the prompt it names, and never folds it as a step. Its
response is never streamed to `Config.Observer`: a client sees the title,
not the model writing it.

A new title is appended with the `model.request` in one batch as
`session.title_changed` `{by, old, new}`, `by` the Sender
`{subject: "service:harness", kind: "service"}`, `session.HarnessSubject`.
It does not move `updated_at` ([[054-a-sessions-title-and-filing]]), so a
list ordered by the last change does not reorder when a title moves.

### What a title never does

A title request that fails, is denied, times out or answers nothing
writes no `session.error`, ends no turn and changes no status; the
runner logs it with the session id and the cause. A failed request is
recorded as a `model.request` with its outcome, like any request, so its
cost is on the record.

### The sessions an agent ran before

Where toposd runs hosted sessions, it names, once per start, the sessions
of every agent whose latest version sets `spec.title` that have never
been named:

- a session is due when it is idle, its log holds a person's
  `user.message`, no `model.request` with `purpose` `title` and no
  `session.title_changed` by a person; copied events of a fork count, so
  a continuation of a named conversation is not named again;
- the pass starts `runner.TitlePassDelay` (one minute) after start, so a
  rolling deploy has settled, and names one session at a time,
  `runner.TitlePassInterval` (two seconds) apart;
- it takes the session's lease as a runner's claim does and skips a
  session whose lease is held, so two replicas and a running turn never
  meet; it asks as after a turn, with the same question and the same
  rules, on the session's latest exchanges;
- at the end it logs how many it named, kept, skipped and failed. A
  session that failed is due again at the next start.

### The task suite starts a task from a log

A task may name `log`, a file of the task holding JSON lines of events,
which the suite writes into the session before it sends the prompt, so a
task starts in the middle of a conversation. A check may name `title`,
which holds the session's title after the run to a `matches` or
`not_matching` pattern.

## Not in this spec

A title for each subject of a conversation kept as chapters, and a list
that searches the titles a session had: both read the log's
`session.title_changed` events, which this spec writes. Handing the name
back to the harness after a person renamed a session. Bounding how often
an installation's endpoint is asked: an endpoint that wants fewer titles
denies.

## Roll order

An endpoint that does not know `session.title` denies it, and the session
keeps its title: toposd may roll before the authorizer. Titles appear
once the endpoint answers the question and an agent sets `spec.title`.
An endpoint that routes must answer it with a model the installation
runs.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `spec.title` decodes with and without `model`, an empty `model` is refused, and the setting is read from the agent's latest version | `manifest/v1`, `manifest`, `runner` tests | not built |
| After an idle turn a person started, the runner asks `session.title` with the resource above and sends one request with the title prompt, the window, no tools and `max_tokens` 64; the answer is appended as `session.title_changed` by `service:harness` with its `model.request` (`purpose` `title`) in one batch, `updated_at` unchanged | `runner`, `harness` tests | not built |
| No title is asked after a trigger's turn, a turn that ended the session or left input pending, a turn whose budget is spent, or in a session a person renamed (copied rename included) | `runner` tests | not built |
| `KEEP`, an empty, an invalid, a too long and an unchanged answer write no title; a failed, denied or timed-out request writes no `session.error` and leaves the status idle | `harness`, `runner` tests | not built |
| The request runs on the allow's model, else the name asked; an agent's named model that resolves to nothing the installation runs sends nothing, and the session's model is the fallback only when the agent names none | `runner` tests | not built |
| No delta of a title request reaches the observer, and input that arrived during the request starts the next turn on the same lease | `runner` tests | not built |
| `Rebuild` builds a title request again from its `fold_seq` and prompt and never as a step | `harness.TestRebuild*` | not built |
| The pass names each due session once, skips a held lease, a renamed and a named session, and logs its counts; a second start names nothing new | `runner` tests | not built |
| `session.title` is in the vocabulary with kind session, and the API document shows `purpose` and the harness's sender | `authorizer`, `internal/server` API document tests | not built |
| On the pinned model, a conversation that moved to another subject for two messages is retitled to it, and one that came back from a side question keeps its title | task suite: `titles/moved`, `titles/aside` | not built |
