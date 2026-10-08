---
title: "A tool call written as text: a response that writes a call of a tool its request offered into its text, instead of calling it, is not kept, its model is reminded once to call the tool, a routed turn whose model does it again moves to another model once, and the log says which"
status: testing
track: core
depends_on: [005-harness-loop.md, 007-models.md, 008-tools.md, 039-questions.md, 051-a-turn-moves-off-a-model-that-cannot-serve.md]
affects: [harness/, session/, prompts/, runner/, internal/runnerapi/, internal/server/, internal/hosted/, cmd/toposd/, authorizer/doc.go, api/openapi.yaml, docs/questions.md]
effort: medium
created: 2026-10-08
updated: 2026-10-08
author: changkun
---

# A tool call written as text

## Overview

A model calls a tool through the tool calls of its response: the request
offers the tools, and the response carries a `tool_use` block for each
call ([[005-harness-loop]], [[008-tools]]). Some models, small open
models among them, sometimes do not. The response calls nothing, ends
`end_turn`, and its text holds the call written out as markup, in the
tool's own words or in another model's call format.

On a hosted installation, a person asked a chat agent to help build a
to-do app. The routed name's first model was busy and the turn moved to
another model of the name ([[051-a-turn-moves-off-a-model-that-cannot-serve]]),
an open model served by a provider that the gateway lists as supporting
tools. The request offered the agent's tools, the question tool of
[[039-questions]] among them, and their definitions. The response called
no tool and ended `end_turn`; its text was a short answer, a plan, and
the question to the person written as markup, in this shape:

```text
<plan> 1. **Define the Tech Stack**: ... </plan>
<question> <questions> <question> <header>Tech Stack</header>
<question>What technologies would you like to use? ...</question> <options>
<option label="Web (React + Tailwind)" description="Recommended. ..." recommended="true"></option> ...
</options> </question> ...
```

Every field of the question tool's input is there, as elements and
attributes. The turn ended, and the person read raw tags where a
question they could answer belonged. The gateway's routing could not
have avoided it: the provider serves the model with tools, and the model
did not use them. Any model can do this.

This spec recognizes such a response narrowly, does not keep it, asks
the same model once more with a reminder to call the tool, and moves a
routed turn to another model when the model does it again.

## Current state

Built with this spec; see the Outcome. Before it, `commitStep` kept
every response that did not stop at the output cap as the step's
answer, and a response with no call ended the turn `end_turn`.

## Design

### Which responses write a call as text

A response is read only when it ended on its own, IR `end_turn` or
`stop_sequence`, and holds no `tool_use` block. A response that calls
any tool, one cut at `max_tokens`, a refusal, and a response that
continues one cut at the output limit ([[005-harness-loop]]) are kept as
they are: the part before a continuation was kept, and the reminder asks
for the whole answer again.

Its text blocks are joined, and every fenced code block and inline code
span is taken out first, as Markdown reads them:

| Code | Read as |
|---|---|
| a fenced block | a line that opens with three or more backticks or tildes after any indentation, up to a line of the same character at least as long, or the end of the text; a backtick line with another backtick on it opens no fence |
| an inline code span | a run of backticks up to the next run of the same length in its paragraph; a run nothing closes is text |

The text writes a call of an offered tool when it holds, outside code,
either shape:

| Shape | What matches | Example |
|---|---|---|
| an element named after the tool | `<name>` or `<name attributes>` closed later in the text by `</name>`, or `<name/>`, where `name` is exactly the name of a tool the request offered | `<question>...</question>`, `<todo/>` |
| a call wrapper naming the tool | one of `tool_call`, `tool_use`, `function_call` and `invoke`, opened as an element, whose open tag and content, up to its closing tag or the end of the text, name the tool as `name="tool"` or `"name": "tool"`; or `<function=tool>` | `<tool_call>{"name": "question", ...}</tool_call>`, `<invoke name="question">` |

The tools offered are those of the request sent, as its tool definitions
name them. When the text writes several calls, the first is named. The
rest of what a response says is never read: a tool named in prose
("I can ask you with the question tool"), an element opened and never
closed, a tag that only shares a prefix (`<questions>` where the tool is
`question`), markup named after no offered tool, and markup in
thinking all write no call.

`<plan>` is markup named after no tool: `plan` is an approval mode
([[012-permissions-and-approvals]]), not a tool, and an agent is offered
no tool of that name unless it declares one. A plan written as markup is
an answer that formats its plan oddly, and is kept.

### The reminder

The first response of a step that writes a call as text is not kept. Its
`model.request` is appended with `outcome` `tool_as_text`, its response
blob holding the response as received, its cost spent, and no
`agent.message`, so the fold never sees it; the Observer receives a reset
for the step, so an attached client discards the output it streamed.
This is how a response sent again at the output limit is recorded
(`escalated`, [[005-harness-loop]]).

The same request is then sent again with one text block after its fold:
added to the last message when that message is the person's or the
tools' results, as a compaction's summary request adds its ask
([[010-context]]), else as a user message of its own. The messages
before it are unchanged, so the prefix a provider cached for the first
request serves the second. The text is the prompt
`reminders/tool-as-text-v1`, rendered with the tool:

> Your last answer was not shown: it wrote a call of the question tool
> into its text instead of calling the tool. Text reaches the person
> exactly as written, so a call written as markup, XML or JSON never runs
> and they would see the raw tags. Answer again in full, and make each
> call, of question or any other tool, as a tool call.

The response written as text is not in the request sent again. It would
have to be an assistant message no event of the log holds, and the model
answers in full from the conversation as it stands, with the reminder
saying why. The reminder is the harness's word for one request: no event
carries it, so the `model.request` of a request that carried it names it
in `reminder`, and a replay ([[007-models]]) renders it again:

| Member of `model.request` | Value |
|---|---|
| `outcome` | `tool_as_text` for a response that wrote a call as text and was not kept; the other outcomes as before |
| `reminder` | `{prompt, tool}`: the prompt the reminder was rendered from, `reminders/tool-as-text-v1`, and the tool it names; set on every request that carried it, the one sent again at the output limit among them, and absent on every other |

A model is reminded once a step. A response to the reminded request that
calls the tool goes on as any step does, and one that answers without
markup is the answer.

The markup is not turned into a call. Its shape is the model's
invention, each model's different; mapping it onto a tool's input would
be guessing, and the call would be one the model did not make, put to a
person who would answer a question in a form the model never chose.

### A model that does it again

A reminded response that writes a call as text again tells more than a
slip: the model cannot use its tools through the request as it is
served, and the next step and the next turn will most likely go the same
way. On the session's own thread of a session whose model has a `via`,
with a router to ask and moves left ([[051-a-turn-moves-off-a-model-that-cannot-serve]]),
the harness asks spec 051's failover question, standing on the model the
turn runs:

| Field of `session.update` | Value |
|---|---|
| `failed_model` | the model that wrote the call, which is also `current_model` |
| `failed_reason` | `tool_as_text` (`harness.FailedToolAsText`) |
| `failed_detail` | the tool, as "a call of question was written as text, again after a reminder" |

An allow that names another model moves the turn as spec 051's move
does: the reminded request's `model.request`, `outcome` `tool_as_text`,
and `session.model_changed` made by the service, in one batch, then the
step's request built again for the model named, with no reminder:

```json
{"type": "session.model_changed", "turn": 1, "step": 1, "payload": {
  "by": {"subject": "service:authorizer", "kind": "service"},
  "old": {"name": "vendor/model-a", "via": "tier/quick"},
  "new": {"name": "vendor/model-b", "via": "tier/quick"},
  "reason": "tool_as_text",
  "detail": "a call of question was written as text, again after a reminder"}}
```

| Reason of `session.model_changed` | Sentence for a person (`session.MessageToolAsTextChange`) |
|---|---|
| `tool_as_text` | "The model could not use its tools, so another one answered." |

`model_busy` is not reused: the model was not busy, and a client that
says so would tell the person something false. A client that does not
know the reason shows the change without one.

A model named that cannot be connected is asked past as spec 051 says,
with no reason. The model moved to is reminded once in the step too. A
turn moves for a call written as text once, and the move counts against
`harness.MaxModelSwitches`; for calls written as text a step therefore
sends at most four requests, two on each of two models, and a turn never
loops on it.

In every other case the reminded response is the step's answer, kept as
an answer was before this spec: an `agent.message` whose `model.request`
has `outcome` `ok` and the `reminder`, and the turn ends `end_turn`. The
cases are a thread's turn, which runs its own agent's model and never
asks; a session on a model named itself; a runner with no router; a turn
out of moves or that already moved for this; and an allow that names no
other model, a deny or a question that cannot be asked. The person reads
the markup, as before this spec, never an error.

| | Before | After |
|---|---|---|
| a response that writes a call as text | the answer; the turn ends | not kept; the same model is asked once more with the reminder |
| the reminded response calls the tool | | the step goes on with the call |
| the reminded response writes it again, routed turn, allow names another model | | the turn moves, once; the step is sent on that model |
| the reminded response writes it again, any other case | | the answer; the turn ends |

### What the log shows

| Case | Events, in order |
|---|---|
| a response written as text | `model.request` `tool_as_text`, no `agent.message` |
| the reminded request answered | `model.request` with `reminder`, `outcome` `ok`, then `agent.message` and the step's calls |
| the reminded request written as text again, moved | `model.request` `tool_as_text` with `reminder` and `session.model_changed` `tool_as_text` in one batch, then the moved request's `model.request` with no `reminder` |
| the reminded request written as text again, kept | `model.request` with `reminder`, `outcome` `ok`, then `agent.message` holding the markup |

Counting `model.request` events by `outcome` `tool_as_text` and `model`
gives how often each model writes calls as text, and a `tool_as_text`
outcome followed by an `ok` with a `reminder` whose message calls a tool
is a reminder that worked.

The harness also writes one line to `harness.Config.Log` for each
response written as text, the hosted runner's log in `toposd`. It never
holds a word of the response:

| Attribute | Value |
|---|---|
| `msg` | `a response wrote a tool call as text` |
| `session`, `thread`, `turn`, `step` | where it happened; `thread` empty on the session's own |
| `model` | the model that wrote it |
| `tool` | the offered tool it wrote a call of |
| `action` | `reminded`, at info; `moved`, at info, with `to`, the model moved to; `kept`, at warn, with `why`, what kept the answer |

### The roll

An authorizer that does not know `failed_reason` `tool_as_text` and
refuses it keeps every such turn on its model, and the reminded response
is the answer; one that ignores `failed_reason` would read the question
as one about a model that cannot serve. An installation whose authorizer
should move such turns rolls an authorizer that reads the reason first,
and decides there whether the model is passed over for this session
alone or for others too: the model answers, and may serve a turn that
calls no tool.

A `toposd runner` process with this change asks a server before it with
`reason` `tool_as_text` in the runner protocol's failover request, which
that server refuses as `invalid_request`; the turn keeps the answer.
The reminder needs no other part: it is the harness's alone.

## Not in this spec

- A call written inside a fenced code block, as some models print a call
  in a fence named after their call format. Code is how an answer shows
  code, and a fence is never read.
- A call written in a shape the two of "Which responses write a call as
  text" do not name, such as a bare JSON object, a model's own special
  tokens printed as text, or a call written beside a real call in the
  same response.
- Which models an authorizer passes over, and for how long.
- A reminder for a response that a client's own markup convention would
  render, such as a chat client that renders a custom element.
- The task suite's measure of how often a reminder works against a real
  model that writes calls as text ([[025-task-suite]]): the scripted
  model proves the loop.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A response is read only when it ended `end_turn` or `stop_sequence` with no call; outside code, an element named after an offered tool and closed, a self-closing one, a call wrapper naming an offered tool to its close or the end, and `<function=tool>` write a call, the first named; prose, an unclosed element, a prefix, a tool not offered (`<plan>` among the chat agent's tools), thinking, fenced blocks of either fence character, nested, unclosed or closed by a longer line, and code spans write none | `harness.TestWrittenCallReadsMarkupNamedAfterAnOfferedTool` | built |
| A question written as nested markup is recorded `tool_as_text` with its response in the blob and no `agent.message`, the Observer resets once, the same request is sent again with the reminder after its fold and nothing else changed, the call it makes is asked of the person, both requests are spent, and one log line says `reminded` with the session, thread, turn, step, model and tool and none of the answer's words | `harness.TestAQuestionWrittenAsTextIsAskedAgainAsACall` | built |
| On the OpenAI Chat Completions wire the reminder is a user message after the person's message or after the step's tool messages, and the call it brings is asked | `harness.TestAReminderOnTheChatWire` | built |
| `<plan>` among an agent's tools that name no plan is the answer; where a tool is named plan, the model is reminded to call it and the call runs | `harness.TestAPlanWrittenAsMarkupIsTheAnswerUnlessPlanIsATool` | built |
| Prose that names a tool, markup in a code fence and markup in a code span are the answer, with one request and no log line | `harness.TestProseAndCodeThatNameAToolAreTheAnswer` | built |
| A model that writes the call as text again on a session that cannot move is not asked a third time: two requests, the second's response the answer with its `reminder`, no change, and a `kept` line at warn naming why | `harness.TestAModelThatKeepsWritingCallsAsTextIsRemindedOnce` | built |
| A routed turn whose reminded model writes the call again asks the router with `failed_reason` `tool_as_text` and the tool in the detail, records the reminded request and the change with reason `tool_as_text` in one batch, sends the step on the model named with no reminder, and the next turn stays there | `harness.TestARoutedTurnMovesOffAModelThatWritesCallsAsText` | built |
| A turn moves for this once: the model moved to is reminded once, and when it writes the call again its response is the answer, with four requests, one question to the router and one change | `harness.TestATurnMovesOffWrittenCallsOnce` | built |
| A router that refuses the reason keeps the turn on its model with the reminded response as the answer, no change, and the refusal in the log line | `harness.TestARouterThatRefusesTheReasonKeepsTheAnswer` | built |
| A replay builds every reminded request again, the one sent again at the output limit among them, to its recorded hash; without the `reminder` member they do not match; an unknown reminder is not rebuilt | `harness.TestAReminderIsReplayed` | built |
| A thread is reminded as the session's own thread is and never asks the router, on a routed session too | `harness.TestAThreadIsRemindedAndNeverMoves` | built |
| A response that continues one cut at the output limit is kept as it is | `harness.TestAContinuationIsNotReminded` | built |
| The runner protocol carries `reason` `tool_as_text` to the server, and an unknown reason is still `invalid_request` | `internal/runnerrole.TestARemoteLeaseAsksTheServerToFailOver` | built |
| toposd asks `session.update` with `failed_reason` `tool_as_text` and the detail, and moves the turn to the model the allow names | `internal/server.TestAFailoverOfACallWrittenAsTextSaysWhy` | built |
| A hosted session's harness logs to the installation's log | `internal/hosted.TestTheHarnessOfAHostedSession` | built |
| The reminder's text is pinned, released and rendered from its tool | `prompts.TestEveryTextRendersItsCurrentBytes`, `prompts.TestReleasedPromptsAreImmutable` | built |
| The API document states the rule, the outcome, the member, the reason and its sentence | `internal/server.TestOpenAPIIsGenerated` | built |

## Outcome

Built on 2026-10-08, in no release yet. Every criterion has its test.
What shipped:

- `harness/written.go` holds the detection (`writtenCall`) and the
  decision (`writtenAsText`); the step loop calls it where a response
  would be committed. Spec 051's failover became `move`, which takes the
  reason, the detail and the request to record, so a model that cannot
  serve and one that writes calls as text share one question, one
  connection and one batch.
- `session.ModelRequest` gains `reminder`, `session.OutcomeToolAsText`
  and `session.ReasonToolAsText` are new, and the prompt
  `reminders/tool-as-text-v1` opens a directory of texts the harness adds
  to one request.
- `harness.Config.Log` is new; the hosted harness takes toposd's log.
- The runner protocol's failover request accepts `reason`
  `tool_as_text`.
