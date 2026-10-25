---
title: "Reading back what the context let go: a history tool that reads the calling conversation's own log as it was written, by range, by tool call or by words, so a result cleared or a stretch summarized is never lost to the agent, with the cleared, summary and compaction texts saying so"
status: testing
track: core
depends_on: [.archive/004-session-log.md, 008-tools.md, 010-context.md, 013-threads-and-subagents.md, 018-credentials-and-secrets.md, 025-task-suite.md, .archive/048-the-machine-starts-at-the-first-tool-call.md, 083-a-title-that-follows-the-conversation.md]
affects: [harness/, harness/tools/, session/, prompts/, manifest/, test/tasks/, api/openapi.yaml, docs/]
effort: medium
created: 2026-10-19
updated: 2026-10-25
author: changkun
---

# Reading back what the context let go

## Overview

A long session runs past its model's context. The harness first clears
old tool results and then replaces older stretches with a summary
([[010-context]]); a session that goes on long enough is summarized
again and again. The log keeps every event as it was written, but the
agent reads only its request, so a number a person gave early on, the
exact error a command printed, or the text of a page fetched an hour ago
is gone for it, or kept only as well as a summary put it.

This spec gives an agent a tool that reads its own conversation's log
back as it was written: a range of events, one tool call and its result,
or the events that hold given words. The texts the agent reads where
something was cleared or summarized say what was let go and how to read
it back.

## Current state

- The log is append-only. Clearing and compaction each append a
  `context.compacted` and remove nothing; the fold renders a summary in
  place of the range it covers and a cleared result as
  `prompts/transcript/cleared-v1.md`: "cleared to save context; run the
  tool again if the result is needed". Running it again is wrong for a
  call that changed something, a page that changed since, or a search
  that costs.
- The compaction prompt, `prompts/compact/compact-v2.md`, tells the model
  that its summary "is the only record the rest of this session will
  have".
- An output past a tool's cap is written whole to a spill file on the
  machine and the result names its path (`prompts/results/spill-v1.md`);
  the log keeps the head and the tail.
- Built-ins an agent must name to hold, such as `web_search`, are
  `tools.OptIn()` (spec 047).

## Design

### The tool

`history` is a built-in an agent names in `spec.tools`, added to
`tools.OptIn()`. The harness runs it, since it reads the log: it never
opens a machine ([[048-the-machine-starts-at-the-first-tool-call]]), may
run beside other calls of a step, changes nothing and may run again.

Its input names one of three reads:

| Input | Reads |
|---|---|
| `{"from": 120, "to": 180}` | the events with those sequence numbers, either bound optional |
| `{"tool_use_id": "toolu_..."}` | that call and its result |
| `{"query": "ECONNREFUSED", "from": 1, "to": 400}` | the events whose text holds the words, case folded, the range optional |

Any other shape, a `from` past `to`, and an empty query are
`invalid_input`.

### What it reads

The calling thread's view of the log, as the fold reads it
(`session.inView`), through the event before the call: a subagent reads
its own thread, the session's thread its own. It reads the events as they
were written: a summary and a clearing change nothing it returns. A
fork's copied events are its log, so a continuation reads the
conversation it continues.

Each event is one entry, headed by its sequence number, its time and who
wrote it:

- a person's message: its text and the names of its files;
- the agent's message: its text, without its reasoning;
- a tool call: the tool's name and its input;
- a tool result: its outcome and text, a picture named by its path;
- a question and its answer, an interrupt, and a change of the title.

Every other event, and a summary itself, is left out. A redacted event is
one line that says it was removed, with nothing of what it held
([[018-credentials-and-secrets]]). A spilled result reads as the log
keeps it, its head, its tail and the path of the whole.

A query answers the matching entries in order, each with its sequence
number and the text around the first match, up to
`harness.HistoryMatchChars` (400) characters, at most
`harness.HistoryMatches` (20) to an answer.

### Bounded answers

An answer holds whole entries up to the tool's output limit
(`tools.DefaultOutputLimit`); an entry longer than the limit is cut with
a line that says where. An answer that stops before its range or its
matches end closes with the input that reads on, such as
`{"from": 181, "to": 400}`. Nothing spills, since spilling opens a
machine.

### What the agent is told

For a thread that holds `history`, three texts change, each a new
version kept beside the old, which a thread without the tool still reads:

- `prompts/transcript/cleared-v2.md`: the result was cleared to save
  context, and `history` reads it back by the call's id, which the text
  names;
- `prompts/transcript/summary-v2.md`: the summary covers events `from` to
  `to`, which `history` reads back or finds by their words;
- `prompts/compact/compact-v3.md`: the summary is what the agent works
  from, so it must still let it continue, but the messages it replaces
  stay readable through `history`, so a long exact text may be named
  rather than copied.

The compaction request keeps the transcript as the thread's steps send
it, with no sequence numbers added, so it is still read from the
provider's prompt cache; the summary names its range instead. The fold
learns which texts to render from a new `session.FoldOptions.History`,
set from the thread's tools, so a replay renders them again.

## Not in this spec

Keeping a spilled output's whole text beyond the machine that wrote it.
Pictures returned as pictures. Reading another thread's or another
session's log. Moving a session to a newer version of its agent, which a
session created before its agent named `history` needs to hold the tool.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `history` is opt-in, never opens a machine, and an agent that names it holds it in every thread | `harness/tools.TestHistoryIsOptIn`, `manifest.TestHistoryIsAnOptInName`, `harness.TestHistoryNeverOpensAMachine`, `harness.TestEveryThreadOfTheAgentHoldsHistory`, `harness.TestAHistoryCallRunsAgainOnResume`, `internal/hosted.TestTheHarnessOfAHostedSession`, `internal/toposcli.TestHistoryIsOfferedWhenNamed` | built |
| A range, a call and a query each answer the thread's own view through the event before the call, as written, compactions ignored, a redacted event as one line, a subagent never reading the session's thread | `harness.TestHistoryReadsTheThreadsLogAsWritten`, `harness.TestHistoryReadsBeforeItsOwnCall`, `harness.TestEveryThreadOfTheAgentHoldsHistory`, `session.TestHistoryReadsTheLogAsWritten`, `session.TestHistoryReadsOneThread`, `session.TestHistoryShowsACallNoToolUseRecords` | built |
| An answer stops at the output limit with the input that reads on; a long entry is cut with a line that says where; a query answers at most 20 matches of 400 characters | `harness.TestHistoryAnswersStopAtTheLimit`, `harness.TestHistoryQueriesAreBounded`, `harness/tools.TestHistoryHandsTheReadToItsReader`, `prompts.TestHistoryDescriptionHoldsTheBounds` | built |
| Invalid input, a reversed range and an empty query are `invalid_input` | `harness/tools.TestHistoryRefusesInvalidInput` | built |
| With the tool, a cleared result renders `cleared-v2` naming the call's id, a summary renders `summary-v2` naming its range, and compaction asks with `compact-v3`; without it each renders as before | `session.TestAFoldForHistoryNamesWhatItReadsBack`, `harness.TestHistoryTextsNameWhatItReadsBack`, `prompts.TestEveryTextRendersItsCurrentBytes` | built |
| `Rebuild` renders the same texts for a thread that held the tool | `harness.TestRebuildRendersTheHistoryTexts`, `harness.TestHistoryTextsNameWhatItReadsBack` | built |
| On the pinned model, an agent whose earlier conversation was summarized without a value a person gave reads it back with `history` and answers it exactly | task suite: `context/readback`, its premise held by `test/tasks.TestTheReadbackLogHidesWhatHistoryFinds` | built; passed 3 of 3 live runs on `claude-haiku-5-5` ($0.0141 in all, 2026-10-19), the suite having no pinned model yet |

## Outcome

Built on 2026-10-19 on the branch `history`, in no release yet, and run
against the stub Lux and scripted models, not a real one. Where the
build differs from the design as written, or adds to it:

- **Where the parts live.** `session.History` reads a thread's view into
  entries, pure, beside the fold; the harness bounds an answer, finds a
  query's matches and holds `HistoryMatches` and `HistoryMatchChars`;
  `harness/tools.History` holds the name, the schema and the input check,
  and runs a reader the harness binds to each thread. A runner sets
  `harness.Config.History` when the agent names the tool, as it sets
  `Question`; the hosted runner, `topos run` and the task suite do.
- **A thread holds it as it holds a built-in.** The session's own thread
  holds it, and a fresh subagent when the names it was started with do,
  which a subagent's tools and an `agent` call's `tools` narrow, and
  which its `thread.started` records. The advisor holds none. A client
  tool of the name, which an agent stored before the name was reserved
  may hold, stays the client's, and its thread reads every text as
  without the tool. A history call a runner stopped under is run again
  (`Repeatable`): it answers the same from the same log.
- **The event before the call** is the one before the call's
  `agent.tool_use`, so the calls of the same step decided before it are
  read and none of their results, however the step's calls run.
- **A subagent's task and its caller's messages are entries.** The
  spec's list names a person's messages; a subagent's conversation opens
  with its `thread.started` task and goes on with `thread.message`, so
  both read back, headed "The agent that started you as its subagent
  gave you this task:" (`results/history/head-v2`, after v0.58.0;
  `head-v1` said "The thread that started you") and "<agent> wrote:".
- **A call refused before it was decided**, which no `agent.tool_use`
  records, reads inside the agent message that made it, and a
  `tool_use_id` read finds that message. An agent message of recorded
  calls alone has no entry of its own, since each call has one; the calls
  of a message cut off at the output limit that never ran are left out,
  as the fold leaves them out.
- **A question's answer** reads as the person's `user.answer`, their
  choices and words, beside the question call's result, which holds the
  answer as the model read it.
- **The input that reads on always names `to`**: the end of the range
  the first read covered, so a read that goes on does not run on to a
  later call and take in the reads between. An answer to a `tool_use_id`
  read that stops early reads on as a range to the call's last entry.
- **An entry cut is the one an answer opens with.** An entry that does
  not fit after the entries before it opens the next answer, and only an
  entry longer than a whole answer is cut, with a line that names its
  bytes and the bytes shown. Its rest does not read back.
- **A query** matches its words as one string, case folded rune by rune
  with Unicode simple folding, in an entry's text below its head. The
  excerpt is centered on the first match, and `...` marks text beyond it,
  outside the 400 characters.
- **The compaction prompt also names a long text a request carried.**
  `compact-v3` keeps every heading of `compact-v2` and still asks for
  requests quoted exactly, but lets a long text a request carried, such
  as a pasted log, be named rather than copied.
- **The API document is unchanged.** It lists no tools or prompts by
  name, the tool records no meta, and `context.compacted.prompt` already
  names the prompt a summary was asked with.
- **An instruction test, as every built-in has one.**
  `test/tasks/instructions/history` asks for the time the person's
  message was sent, which only the log holds; its scripted right and
  wrong solutions run, not yet against a real model.
- **`context/readback` reads its log through the suite's loader.** Its
  `task.yaml` names `log: log.jsonl`, lines of events without an id or a
  sequence, which the suite numbers from 1 as the session's first
  events; its summary, asked with `compact-v3`, keeps that a build number
  was given and not the number. `context` is in `tasks.Categories` and
  spec 025's category table. The task passed 3 of 3 live runs on
  `claude-haiku-5-5`, each reading the person's message back with
  `history` in two steps.
