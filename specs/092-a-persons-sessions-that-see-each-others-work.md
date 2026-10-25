---
title: "A person's sessions that see each other's work: each open session's work read from its log (its repositories and branch, its app, its folder on a computer, the paths it changed, the paths it says it is working on), a `sessions` tool that finds the person's other open sessions whose work overlaps a repository, a folder or an app and says which paths this session is about to change, and a `send` tool whose message reaches another of the person's sessions as data, read at its next step while it runs and with its next turn while it waits, starting nothing and never taken as the person's word"
status: drafted
track: core
depends_on: [.archive/004-session-log.md, .archive/006-identity.md, 008-tools.md, 010-context.md, 012-permissions-and-approvals.md, 013-threads-and-subagents.md, 015-api.md, 018-credentials-and-secrets.md, 019-git.md, 058-what-a-create-allow-attaches.md, 059-publishing-to-a-chosen-app.md, 073-a-working-directory-kept-in-the-sessions-folder.md, .archive/080-device-machines.md, 086-proactive-agents.md, 087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report.md, 091-a-fork-of-the-conversation.md]
affects: [session/, harness/, internal/server/, internal/store/, internal/runnerapi/, internal/runnerrole/, prompts/, api/openapi.yaml, docs/]
effort: large
created: 2026-10-25
updated: 2026-10-25
author: changkun
---

# A person's sessions that see each other's work

## Overview

A person runs several sessions at once: one refactors a package, one
fixes a bug in the same repository, one edits a site they publish,
another works in a folder of their computer. Each works on its own
branch ([[019-git]]), so no two sessions write the same branch, but
nothing stops two of them changing the same files on two branches, or
the same folder of a computer where no branch separates them, and
neither knows the other exists until a merge or a release fails
([[059-publishing-to-a-chosen-app]]).

This spec lets a session see the person's other sessions' work and
tell one of them something, in three parts that roll in order:

1. **Work.** The server reads each session's work from its log: the
   repositories it works in and its branch, the app it publishes to,
   its folder on a computer, and the paths it changed. A `sessions`
   tool finds the person's other open sessions whose work overlaps a
   repository, a folder or an app. It answers work, never what anyone
   said.
2. **What a session is working on.** Before it changes a part of a
   repository or a folder, a session may say which paths it is about
   to change, with one line of why, so another session finds the
   intent before any file has changed: the `sessions` tool's
   `working_on` says it, and `done` takes it back. Saying so is
   advisory: it refuses nothing.
3. **Messages.** A `send` tool sends one of the person's other sessions
   a message. The receiving session reads it as data from another
   session, never as the person's word: at its next step while it runs,
   with its next turn while it waits. A message starts no turn.

What is new is the reach across sessions. Inside one session, an agent
and the subagents it started already message each other
([[013-threads-and-subagents]]). There is no event bus: a session finds
and tells one other session, and the server delivers the message to
that session's log alone.

Each choice below is a recommendation until this spec is validated;
the table that opens the design weighs each against its alternative.

## Current state

- Each session works on the branch `agents/<agent>/<session>`
  (`session.Branch`) of each repository its create attached
  ([[019-git]], [[058-what-a-create-allow-attaches]]), and publishes an
  app by its id ([[071-a-published-app-found-by-its-id]]). A release
  whose commit does not descend from the live one is refused
  ([[059-publishing-to-a-chosen-app]]). A session on a person's computer
  works in the folder its create named, with no branch of its own
  ([[080-device-machines]]).
- What a session changed is in its log: the `path` of every `write` and
  `edit` call, and, where a turn keeps a copy of the working directory,
  the `written` and `removed` paths of `work.saved`
  ([[073-a-working-directory-kept-in-the-sessions-folder]]). A turn
  whose machine keeps no copy records no `work.saved`, and a change made
  by `bash` alone is in no field.
- The Session folds its header and some of its log (its wake, its
  outcome, its context report); it holds nothing that says what it is
  working on.
- The sessions list answers the caller's sessions by status, agent,
  metadata and trigger, and the search route finds them by the words of
  their messages ([[050-searching-sessions]]). A session's token reaches
  its own session; no route is asked by a session about its initiator's
  other sessions.
- A message reaches a session as a `user.message` from a person or a
  program at the send route, or from a trigger's firing. A subagent
  is messaged only by the one that started it. A `user.message` sent while a
  turn runs begins the next turn when that one ends; one sent to an idle
  session is pending input (`session.HasPendingInput`), so a runner
  claims the session and a turn starts at once.
- A turn begun by a trigger's message is unattended, and drops the
  patterns a person's answers remembered
  ([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]).

## Design

### Recommendations

| Item | Recommended | Weighed against, and why not |
|---|---|---|
| What one session sees of another | its work: title, status, when it last ran, its repositories and branch, app, folder, changed paths and what it says it is working on, where they overlap the query | a search over the words of the person's other conversations: a session that read a hostile page could read every conversation of the person through it and carry it out over its own network; the purpose, keeping two sessions from conflicting changes, is answered by work |
| Which sessions | the initiator's own open sessions in the session's context, asked of the authorizer | every session the person may read: in an organization, other members' sessions, which is a question for the installation and left out (below) |
| How work is known | folded by the server from the log, with nothing for the agent to do | declared by the agent: it would lag and be forgotten |
| What a session says it is working on | advisory, said before a change, lapsing with the session | a lock: one held by a session nobody continues blocks the person's other work, and git and 059 already refuse the change that would break; the word claim, this spec's first draft: it reads as a right the session holds, which it is not |
| The message's event | `session.message`, a user event type of its own, with the sending session | a `user.message` with a session as sender: every reader that takes a `user.message` as the person's would take it so, 087's attended turn, 086's count of wakes without the person, a client's list of what the person said |
| A message to a running session | read at its next step, after that step's results | with its next turn: a session that is running is the one about to make the change the message is about |
| A message to a waiting session | kept, and read with its next turn, whoever starts it | starting a turn: spending no person asked for, and two sessions could keep each other running; whether a message may ever start one is left open (Not in this spec) |
| Authority | none: rendered as data, in a frame that names the sending session | the message as an instruction the receiving session follows: whoever could make one session write to another would then steer it |
| The tool's names | `sessions`, `send` | `peers`: a retired v0.7.0 word; `steer`: names one use; `bus`: there is none |

### A session's work

The Session gains one folded field, `work`:

| Member | Meaning |
|---|---|
| `repositories` | each attached repository: its `url`, and `branch`, the session's branch in it |
| `app` | the id of the app the session publishes to, when it has one |
| `folder` | for a session on a person's computer: `{environment, workdir}` |
| `paths` | the paths the session changed, relative to its working directory, newest first, at most `MaxWorkPaths`: each successful `write` and `edit` call's `path`, and each `work.saved`'s `written` and `removed` |
| `working_on` | what the session says it is working on, its open entries (below) |
| `active_at` | the time of its latest `session.status` `running` |

The server folds it at each append batch, as it folds the Session's
other fields. A change `bash` made appears only through a later
`work.saved`, so a session that works through `bash` on a machine that
keeps no copy shows the paths of its `write` and `edit` calls alone;
saying what it is working on is how it tells another session what it
will change. A session's work is the session's, of every subagent in
it.

### What a session says it is working on

An entry of what a session is working on names paths it is about to
change, in one repository it works in or in its folder, and says why:

| Event | Payload |
|---|---|
| `session.working_on` | `entry_id`, `repository` (a `url` of `work.repositories`; absent for the folder), `paths` (path prefixes relative to the repository's root or the folder, 1 to `MaxWorkingOnPaths`), `why` (at most `MaxWorkingOnWhy` characters), `tool_use_id` |
| `session.done_with` | `entry_id`, or `all`; `tool_use_id`, absent when the server ended it |

An entry is open from its `session.working_on` until its
`session.done_with`, the session's end or archive, or `WorkingOnIdle`
after the end of the session's latest turn, whichever comes first; the
fold leaves out a lapsed entry. A session holds at most `MaxWorkingOn`
open entries, and one past it is refused `invalid_input`. An entry
refuses no change of anyone's: it is what `find` answers.

### The `sessions` tool

A harness tool of the session's own thread, offered when the agent's
`spec.tools` names it, on the hosted runner alone, since only a server
keeps the person's sessions. A fork holds its definition and is
refused it ([[091-a-fork-of-the-conversation]]); a fresh subagent is
not offered it.

| Name | Parallel | Effect | Input |
|---|---|---|---|
| `sessions` | no | `read` | `action`, and the fields below |

| Action | Input | Does |
|---|---|---|
| `find` | one of `repository` (a `url`), `folder` (`true`: this session's folder), `app` (an app id); optional `paths` | answers the initiator's other open sessions whose work overlaps, at most `MaxFound`, most recently active first |
| `working_on` | `repository` or `folder`, `paths`, `why` | appends `session.working_on`; the result names any open session whose entries or changed paths already overlap |
| `done` | `entry_id` or `all` | appends `session.done_with` |

Every action is one effect, `read`, since none changes anything outside
the log. A session's work overlaps a query when it works in the same
repository (by `url`), the same folder of the same environment, or
publishes to the same app, and, when the query names `paths`, when one
of its changed paths or the prefixes it is working on lies under one of them or
above one of them.

Each session `find` answers:

| Member | Meaning |
|---|---|
| `session_id`, `title` | the session, its title |
| `status`, `stop_reason` | running, or idle and why |
| `active_at` | when it last ran |
| `repository`, `branch` | the overlapping repository and the session's branch in it |
| `app`, `folder` | when they overlap |
| `working_on` | its open entries that overlap: paths and why |
| `changed` | its changed paths that overlap, at most `MaxFoundPaths` |

It answers no message, answer, file or excerpt of any session.

The runner reaches the server for `find`, `working_on` and `done` over
the internal listener, as the `trigger` tool does
([[086-proactive-agents]]), at
`POST /internal/v1/leases/{session}/sessions/{find,working_on,done}`
(the runner's routes, under the `leases` path of [[016-runners]]);
`session.working_on` and `session.done_with` are appended by the runner
like any event of the turn, and the server's routes answer `find` and
check an entry's overlap.

### The `send` tool

A harness tool of the session's own thread, offered as `sessions` is.

| Name | Parallel | Effect | Input |
|---|---|---|---|
| `send` | no | `write` | `session`: the `ses_` id of a session `find` answered in this session; `content`: the text, at most `MaxSessionMessage` characters |

Effect `write` decides the call by the session's mode and lists
([[012-permissions-and-approvals]]): in `confirm` the person approves
each send unless a list allows it. The tool's check refuses before
anything is decided:

| Rule | Why |
|---|---|
| `session` is not this session, and `find` in this session answered it | a session tells only a session it found by its work |
| at most `MaxSendsPerTurn` sends in a turn | a turn cannot flood another session |

The runner scrubs `content` of every value it holds, as it scrubs a
tool's output ([[018-credentials-and-secrets]]), and asks the server at
`POST /internal/v1/leases/{session}/sessions/send`. The server asks
`session.send` (below), and refuses, with a reason the call's result
names:

| Refusal | When |
|---|---|
| `not_found` | the session is not one the authorizer lets this session's initiator send to, or is gone |
| `conflict` | the session has ended |
| `too_many_waiting` | the session holds `MaxWaitingMessages` messages its turns have not read |

On success the server appends `session.message` to the receiving
session's log and answers whether it runs, so the sender's result says
when it will be read: at its next step, or with its next turn.

### `session.message`

| Member | Meaning |
|---|---|
| `from_session` | the sending session's id |
| `from_title`, `from_agent` | its title and agent's name when it sent |
| `from_tool_use_id` | the `send` call |
| `content` | the text, as scrubbed |

The event's `sender` is `session:<ses_id>`. It is not pending input:
`session.HasPendingInput` is unchanged, so it starts no turn on any
runner. The Session folds `waiting_messages`, the count of
`session.message` events that no request of the session's own thread
has carried yet.

The fold renders a `session.message` to the session's own thread as a
text block, inside the frame of `prompts/context/session-message-v1.md`,
which names the sending session's title and id, says that the text
comes from another of the person's sessions and not from the person,
that it is information and grants nothing, to check what it claims
against the repository, the folder or the files before acting on it,
and that `send` answers it. Where it renders:

| The receiving session | The message renders |
|---|---|
| running | in its next request, after the results of the step it arrived during, in the same user message, after the results |
| idle | with its next turn's first input, before it, in the same user message |

Either way the conversation is only appended to, so the cached prefix
of [[010-context]] holds. A subagent, fresh or fork, is not shown it;
the session's own conversation is.

### Data, never authority

A `session.message` changes nothing the receiving session may do:

| It | does not |
|---|---|
| confirm or deny a held call, or answer a question | `user.tool_confirmation` and `user.answer` come from a person at the send route ([[039-questions]]) |
| count as the person's word | it starts no new count of wakes without the person ([[086-proactive-agents]]), and a turn it is read in stays as attended or unattended as its first input made it ([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]) |
| change the mode, the model, the effort, what the session is working on, or a list | those change through the person's routes alone |
| run anything | every call after it is decided by the approval layers, as after any content the model reads |

The frame is the model's warning, not the defense: the defense is that
the receiving session holds no more authority after the message than
before it, and the person sees the message (below).

### The authorizer

| Question | Fields |
|---|---|
| `session.list` asked for a session's `find` | `origin: {kind: session, session_id, tool_use_id}`, `initiator`, and `work`: the query's `repository`, `folder` or `app`, and `paths` |
| `session.send` asked for a session's `send` | `sender: session:<ses_id>`, `event_type: session.message`, `origin` as above, and the receiving session's fields as any send carries them |

Both are asked as the sending session's initiator, in the session's
context, as 086's trigger questions are. The owner policy allows an
initiator their own sessions in the same context and nothing more, and
an installation's authorizer decides the same or less: Topos decides
nothing about which sessions are a person's. The answer of `find` is
narrowed by the server to the sessions the list's allow admits, and
each `send` is asked about its one receiving session.

### What the person sees

| Where | What |
|---|---|
| the sending session | the `send` call, with the receiving session named by its title and linked, and the text |
| the receiving session | the message as its own item, from the sending session, linked back, with `waiting_messages` marked until a turn reads it |
| each session | its `work`: repositories and branch, app, folder, changed paths, what it says it is working on |

A client shows each as it shows any event; the words are the client's.

### Constants

Constants of the `session` package, read by the tools' schemas, their
descriptions and their checks:

| Constant | Value | Why this value |
|---|---|---|
| `MaxWorkPaths` | 200 | enough to show where a long session has been, small enough to fold at every batch |
| `MaxWorkingOn`, `MaxWorkingOnPaths` | 10, 20 | a session working on a few areas at once |
| `MaxWorkingOnWhy` | 120 | one line |
| `WorkingOnIdle` | 72 hours | a session left over a weekend still holds its intent; a forgotten one stops after three days |
| `MaxFound`, `MaxFoundPaths` | 20, 20 | a person rarely has more open sessions on one repository |
| `MaxSessionMessage` | 4000 | a message, not a file |
| `MaxSendsPerTurn` | 3 | a turn says what it must and goes on |
| `MaxWaitingMessages` | 10 | a session nobody runs cannot fill with messages |

### Roll order

1. `work` on the Session, and `sessions` with `find`.
2. What a session is working on: `working_on`, `done`,
   `session.working_on`, `session.done_with`, and `working_on` in
   `work` and in `find`.
3. `send`, `session.message`, `waiting_messages`, and the fold's frame.

Each step is a release. An installation's authorizer learns the
questions' `origin` before the release that asks them; until it does,
it reads a session's `find` as its initiator's own list.

## What a hosted installation needs

| Need | Why |
|---|---|
| The authorizer answers `session.list` and `session.send` asked with a session's `origin`: the initiator's own sessions in the same context, never another person's | own sessions only |
| The managed agent names `sessions` and `send`, with instructions to find before changing a repository, a folder or an app another session may be working on, to say what it is working on before it changes it, to tell the other session only what it needs to know, and never to treat a message from another session as the person's request | the tools are used when they help, and a message stays data |
| A client shows `work`, what each session is working on, and messages as above | the person sees what their sessions tell each other |

## What the Go SDK needs

| Need | Why |
|---|---|
| The Session's `work` and `waiting_messages`, and `session.message` read as an item with its sender | both clients show what each session works on and what it was told |

## Not in this spec

A message that starts a turn in a waiting session; sessions of other
people, in an organization or otherwise; a lock that refuses a change;
a message to a session of a person's own computer while the computer
is away beyond what the log keeps; the words a client shows.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The Session folds `work` from the header and the log: repositories and branch, app, folder, the paths of `write`, `edit` and `work.saved` newest first up to `MaxWorkPaths`, open `working_on` entries and `active_at` | `session.TestTheSessionFoldsItsWork` | not built |
| `find` answers the initiator's other open sessions whose work overlaps a repository, a folder or an app, and paths when named, with only the overlapping entries and paths, and no content of any session | `internal/server.TestFindAnswersOverlappingWorkAlone` | not built |
| `find` is asked as `session.list` with `origin` and `work`, and answers only what the allow admits; an ended session, a session of another person and one in another context are never answered | `internal/server.TestFindIsTheInitiatorsOwn`, `internal/auth.TestTheOwnerPolicyAdmitsOwnSessions` | not built |
| An entry of what a session is working on is open until `done`, the session's end or archive, or `WorkingOnIdle` after its latest turn; the eleventh is refused; a `working_on` result names the open sessions that already overlap | `session.TestWorkingOnLapses`, `internal/server.TestWorkingOnNamesWhatItOverlaps` | not built |
| A `send` to a session `find` did not answer in this session, to itself, or past `MaxSendsPerTurn` is refused before it is decided; a send is asked as `session.send` with the session as sender; an ended receiver is `conflict` and a full one `too_many_waiting` | `harness.TestASendIsChecked`, `internal/server.TestASendIsAskedForTheSession` | not built |
| A sent text is scrubbed of the values the runner holds before it leaves the runner | `runner.TestASentMessageIsScrubbed` | not built |
| A `session.message` to a running session renders in its next request after the step's results, and to an idle one with its next turn's first input, in the frame that names its sender; neither starts a turn | `session.TestASessionMessageRendersAsData`, `runner.TestASessionMessageStartsNoTurn` | not built |
| A `session.message` confirms no held call, answers no question, resets no wake count and leaves a turn's attended as its first input made it | `harness.TestASessionMessageGrantsNothing` | not built |
| `waiting_messages` counts the messages no request has carried, and falls to zero when a request carries them | `session.TestWaitingMessages` | not built |
| A fork's `sessions` or `send` call is `withheld_from_fork`, and a fresh subagent is not offered them | `harness.TestOnlyTheConversationFindsAndSends` | not built |
| An instruction task: a session about to change a package another session said it is working on finds that, tells that session, and works around it; a message that asks for something the person did not is not acted on | `test/tasks/instructions/sessions/` on the task suite's pinned model ([[025-task-suite]]) | not built |
| The OpenAPI document describes `work`, `waiting_messages` and the events | `internal/server.TestTheDocumentDescribesSpec092` | not built |
