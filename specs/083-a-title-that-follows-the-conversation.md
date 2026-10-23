---
title: "A title that follows the conversation: after a turn a person started, the harness asks a small model whether the session's title still names what the conversation is about and writes a new one when it does not, an agent turns it on with spec.title, and the sessions it ran before are named once"
status: testing
track: core
depends_on: [.archive/004-session-log.md, .archive/006-identity.md, .archive/007-models.md, 010-context.md, 016-runners.md, 017-external-runners-handoff-fork.md, 025-task-suite.md, .archive/038-routed-models.md, .archive/049-reasoning.md, 054-a-sessions-title-and-filing.md, 061-a-messages-shape-and-the-route-an-allow-names.md]
affects: [manifest/v1/, manifest/, session/, harness/, runner/, authorizer/, prompts/, internal/server/, internal/runnerapi/, internal/runnerrole/, cmd/toposd/, test/tasks/, api/openapi.yaml, docs/]
effort: medium
created: 2026-10-19
updated: 2026-10-23
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
| `spec.title` decodes with and without `model`, an empty `model` is refused, and the setting is read from the agent's latest version | `manifest/v1.TestATitleDecodesWithAndWithoutAModel`, `manifest.TestAnAgentTurnsTitlesOn`, `runner.TestTheTitleSettingIsTheAgentsLatest`, `runner.TestATurnOfASessionWithoutTitlesCostsLittle`, `internal/server.TestTheTitleSettingIsReadFromTheLatestVersion` | built |
| After an idle turn a person started, the runner asks `session.title` with the resource above and sends one request with the title prompt, the window, no tools and `max_tokens` 64; the answer is appended as `session.title_changed` by `service:harness` with its `model.request` (`purpose` `title`) in one batch, `updated_at` unchanged | `runner.TestATitleFollowsATurnAPersonStarted`, `internal/server.TestATitleAsksTheInitiator`, `harness.TestATitleRequestNamesTheConversation`, `harness.TestTheTitleWindow`, `session.TestATitlesRequestKeepsTheUpdateTime`, `session/storetest` `ATitleKeepsTheUpdateTime` (memory, directory and Postgres stores), `cmd/toposd.TestServeNamesItsSessions`, `cmd/toposd.TestTheRunnerRoleNamesASession` | built |
| No title is asked after a trigger's turn, a turn that ended the session or left input pending, a turn whose budget is spent, or in a session a person renamed (copied rename included) | `runner.TestNoTitleIsAskedWhenItIsNotDue`, `runner.TestBlockedAndStartedByPerson`, `runner.TestAPersonsRenameAndAnEndWinOverATitle`, `runner.TestATitlesAppendIsDecidedOverWhatItRaced`, `runner.TestATitleThatReadAnEndAppendsNothing`, `session.TestAHarnessTitleOverAnotherTitleChangesNothing` | built |
| `KEEP`, an empty, an invalid, a too long and an unchanged answer write no title; a failed, denied or timed-out request writes no `session.error` and leaves the status idle | `harness.TestATitleAnswerThatWritesNoTitle`, `harness.TestAFailedTitleRequestIsRecorded`, `harness.TestATitleThatCannotBeRecorded`, `runner.TestATitleThatFailsChangesNothing`, `runner.TestInputDuringTheTitleStartsTheNextTurn` | built |
| The request runs on the allow's model, else the name asked; an agent's named model that resolves to nothing the installation runs sends nothing, and the session's model is the fallback only when the agent names none | `runner.TestATitleRunsOnTheModelTheAllowNames`, `runner.TestATitleThatFailsChangesNothing`, `runner.TestTitleNames`, `harness.TestATitleRunsOnTheFirstModelThatConnects` | built |
| No delta of a title request reaches the observer, and input that arrived during the request starts the next turn on the same lease | `runner.TestATitleFollowsATurnAPersonStarted`, `harness.TestATitleRequestNamesTheConversation`, `runner.TestInputDuringTheTitleStartsTheNextTurn`, `runner.TestInputAtTheTurnsEndStartsTheNextTurn`, `internal/server.TestADeleteWaitsForATitlesLease` | built |
| `Rebuild` builds a title request again from its `fold_seq` and prompt and never as a step | `harness.TestRebuildATitleRequest`, `harness.TestATitleThatSpentItsCapReasoningIsAskedAgain` | built |
| The pass names each due session once, skips a held lease, a renamed and a named session, and logs its counts; a second start names nothing new | `runner.TestThePassNamesEachDueSessionOnce`, `runner.TestThePassWaitsBetweenSessions`, `runner.TestThePassCountsWhatFailed`, `runner.TestThePassNamesASessionWhoseTitleFailed`, `runner.TestAPanicOfThePassFailsOneSession`, `internal/server.TestATitleSettingThatCannotBeRead`, `cmd/toposd.TestServeNamesItsSessions` | built |
| `session.title` is in the vocabulary with kind session, and the API document shows `purpose` and the harness's sender | `authorizer.TestVocabularyMatchesTheSpec`, `authorizer.TestARoutingScaffoldSpeaksTheVocabulary`, `internal/auth.TestOwnerPolicyRows`, `internal/server.TestTheAPIDocumentShowsATitle`, `internal/server.TestOpenAPIIsGenerated` | built |
| On the pinned model, a conversation that moved to another subject for two messages is retitled to it, and one that came back from a side question keeps its title | task suite: `titles/moved`, `titles/aside`; the scripted solution and wrong solution of each in `tasks.TestScriptedSolutions` | built; passed 6 of 6 live runs on `claude-haiku-5-5` (3 each, $0.0044 in all, 2026-10-19), the suite having no pinned model yet |

## Outcome

Built on 2026-10-19, in no release yet, and not yet run against an
installation's authorizer. Every criterion has its test, and the two
tasks passed live on `claude-haiku-5-5` called directly. What shipped
differs from the draft in these points:

- **The update time.** A title's `model.request` moves the header's
  `updated_at` no more than its `session.title_changed` does
  (`session.ApplyBatch` reads the request's purpose), so a request that
  keeps the title, and the pass over older sessions, reorder no list.
- **`idle_seconds`.** A send's `idle_seconds` counts from the last request
  of the session's turns, not from a title's, which runs on another model
  and warms no cache of theirs.
- **Reasoning.** The request carries no reasoning setting: the dialects
  of this build cannot turn reasoning off, and a model that reasons by
  default, as `claude-haiku-5-5` does, spent all 64 tokens reasoning in
  the first live runs. Where the agent names its title `model`, a
  response that stops at its `max_tokens` with no text is therefore
  asked once more at `harness.TitleReasoningTokens` (1024), the first
  request recorded with outcome `escalated` in the title's batch, as
  spec 005 escalates a step; on the session's own model, which may cost
  far more, it is not, and the title stays (`harness.TitleAsk.Retry`).
  The task suite's title tasks name the run's own model as the agent's
  title model, `${MODEL}`, to be asked as such an agent is.
- **One deadline.** `harness.TitleTimeout` is 15 seconds, over the whole
  title the runner asks for: the authorizer's question, the connection of
  its model, a door's settling included, and its requests. Each write of
  a request's record, its blobs and its append, has
  `harness.TitleRecordTimeout` (2 seconds) of its own from when the write
  starts, so a request the deadline cut short, or one slower than 2
  seconds, is still recorded (`harness.TestATitleSlowerThanItsRecordTimeoutIsRecorded`),
  and a runner holds the lease for a title a few seconds past the 15 at
  most.
- **Input ends a title.** The drive's watch of the log, which always runs
  now, ends the title at once when a message, a confirmation, a client
  tool's result, an answer, an interrupt or a resume arrives after what
  the title read; its request is recorded with outcome `canceled`, and
  the drive starts the next turn. The pass watches the session it names
  the same way, and the claim queue drives the input once it lets go.
- **A person's rename and an end win.** The title's batch is appended
  through the runner's log over the events others appended since what
  the title read, read in the same step as the store takes the batch,
  again when the store refused it for events between: a person's
  `session.title_changed` among them leaves the harness's change out,
  the request still recorded, and a `session.status` ended leaves
  everything out, logged. An end the title's own read holds leaves it
  out too: the status the title checked was read before its log, so an
  end can land between the two. `session.ApplyBatch` also moves the header by a
  harness change only from the title it names as `old`.
- **A delete waits for the title.** `DELETE /v1/sessions/{id}` of an idle
  session whose lease a runner holds tries again every 250 ms for
  `server.DefaultLeaseWait`, the title's deadline and its record's, and
  deletes it once the runner lets go; one still held, or running, is
  `conflict` as before. No other route refuses an idle session for a held
  lease: an archive, a fork, a resume and an end write beside the lease.
- **The pass names by an answered request.** A session is named by a
  title request with outcome `ok` alone, so one whose request failed, was
  refused or cut short is due again at the next start. An agent whose
  latest version cannot be read is logged and left out of the pass, and
  a panic in one session's title counts that session failed, logged with
  its stack, and the pass goes on.
- **The cost of a turn.** After a turn, the drive reads the log's end
  for input first, then asks nothing more of a runner that has no title
  question, then reads the agent's setting, which a runner keeps for
  `runner.TitleSettingTTL` (30 seconds) by agent, so a change of the
  agent's latest version applies within it; only a session whose agent
  sets a title reads its whole log.
- **The prompt.** `title/title-v1` is one message of the user and no
  system prompt, with no cache breakpoint, since nothing of it is sent
  again; it asks for the answer with no label, after the first live runs
  answered `NEW: <title>`. `KEEP` is read without case and a closing
  period, and one surrounding pair of straight, curly, angle or corner
  quotes is removed.
- **The person's text is bounded.** Each of the person's messages is cut
  to `harness.TitleMessageChars` (2000) characters, so a pasted log adds
  no cost and no subject.
- **The turn's input.** A turn's input is the person's messages after the
  `fold_seq` of the last request of the session's own thread before the
  turn opened: a message that arrived while the turn before ran, after
  its last request was built, is the input of the turn that read it.
- **Input at a turn's end.** The drive reads the log again after every
  turn that left nothing pending, for every session, and a message that
  arrived between the turn's close and that read starts the next turn in
  the same drive, as one sent during the title request does.
- **The route.** An allow's `route` is read and kept nowhere: a title
  moves no model of the session.
- **A fork's title.** The title the request carries as current is the
  harness's only when the session's own last change of the title is the
  harness's; a fork's copied changes were its parent's, so a fork's first
  naming offers no `KEEP`. A copied title request still keeps the pass
  from naming the fork, as the draft says.
- **Runner processes.** A runner process asks the question over the
  internal listener, `POST /internal/v1/leases/{session}/title`
  (`runnerapi.TitleRequest`, `runner.Titler`), and reads the setting from
  the session's bundle. The local `topos` command asks no question and
  names no session.
- **The pass's counts.** A session the pass read and did not ask about is
  counted as skipped, a session that is not due among them. It lists
  hosted sessions whose writer is hosted, as the claim queue does. A
  session whose lease the claim queue held for its own look at the
  session is skipped as any held lease and named at the next start; the
  minute's delay keeps the two apart at a real start.
- **The session's key.** A title request is sent with the session's own
  model key where the installation mints one, as every request of the
  session is. An authorizer that answers `session.title` with a model
  widens the session's key to it, as it does for a routed switch, and an
  agent's title `model` must be one the key reaches; otherwise the
  gateway refuses the request, the runner logs it, and the title stays.
  The live runs called the provider with its own key, so this path is
  proven by the stubs alone.
- **The Cella stub.** The full gate failed twice on the hosted device
  case of a message sent while the computer was away: its long command
  ran to its end, since the stub's `Disconnect` found no session to drop
  in the window between the exec route's check that the sandbox runs and
  the session's registration. A session that registers on a computer
  that went away is now dropped at once, as an open one is. The failure
  never showed outside the full gate, and the base commit's gate was not
  run, so whether the drive's read of the log after each turn made the
  window easier to hit is not known.
- **The task suite.** A task whose agent sets `title` runs in a session
  that stays idle at the end of its turn, with the question allowed and
  naming no model, and its title request is no step of the run's count.
