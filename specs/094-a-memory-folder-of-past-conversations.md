---
title: "A memory folder of past conversations: an allow names a read-only memory folder whose files are the initiator's other conversations in the session's context, kept by a metadata entry, served from the server's log as one Markdown file each at /memory/<name>/, with every list and read asked of the authorizer as the initiator with the session as the question's origin"
status: testing
track: core
depends_on: [015-api.md, 016-runners.md, .archive/050-searching-sessions.md, 056-editing-a-message-and-the-branches-of-a-session.md, 057-a-sessions-metadata-in-its-questions-and-lists.md, 058-what-a-create-allow-attaches.md, 077-rollouts.md, 083-a-title-that-follows-the-conversation.md, 086-proactive-agents.md, 093-memory-folders-the-file-tools-reach.md]
affects: [session/, memory/, memory/arca/, runner/, internal/server/, internal/runnerapi/, internal/runnerrole/, internal/hosted/, internal/store/, cmd/toposd/, api/openapi.yaml, docs/]
effort: medium
created: 2026-10-29
updated: 2026-10-30
author: changkun
---

# A memory folder of past conversations

## Overview

A person comes back to an agent and says "as we discussed last week".
The session they are in does not hold last week's conversation; the
server does, in that session's log. Spec 050 finds a session by its
words for a client. This spec lets the agent itself find and read the
person's earlier conversations, with the file tools it already has.

An allow's `memory` member ([[093-memory-folders-the-file-tools-reach]])
may name a folder whose files are the session initiator's other
conversations, in place of a folder of a files store. The server serves
it from its own log: one Markdown file per conversation, with what the
person and the agent said. `glob` lists them, `grep` finds what was said,
`read` opens one. The folder is read only. Which conversations it holds
is the allow's, by one metadata entry, narrowed to the initiator's own
sessions in the session's context; each list and each read is asked of
the authorizer as the initiator, with the reading session named as the
question's origin, so an installation decides every one.

## Current state

- A memory folder names a folder of an Arca-compatible files store:
  `url`, `owner`, `path`, `access` ([[093-memory-folders-the-file-tools-reach]]).
  The memory machine (`memory/arca`) reaches it with the session's token
  for `TOPOS_FILES_AUDIENCE`, which a runner must have for any session
  with memory folders.
- `GET /v1/sessions/search` finds the caller's sessions by their words
  ([[050-searching-sessions]]): `session.list` for where to look, then
  `session.read` of each session found. It reads `user.message`,
  `agent.message` and `user.answer` of the session's own thread, never a
  tool's output, a thinking block, a subagent's thread or a redacted
  event (`session.Searched`).
- A list keeps sessions by one metadata entry with exactly its value
  (`ListOptions.Metadata`, [[057-a-sessions-metadata-in-its-questions-and-lists]]);
  nothing keeps the sessions that lack a key.
- The trigger tool reaches the server from a runner through
  `runner.Triggers`: the runners toposd drives call the server directly,
  and a runner process's lease asks over the internal listener
  ([[086-proactive-agents]]).

## Design

### The member

A memory folder names `sessions` in place of `url`, `owner` and `path`:

```json
{"memory": [
  {"name": "user", "url": "https://storage.example.com/v1", "owner": "https://issuer.example|9ab3",
   "path": "files/memory/you", "access": "read_only"},
  {"name": "chats", "access": "read_only", "sessions": {"metadata": {"project": "p1"}}}
]}
```

| Field | Rule |
|---|---|
| `sessions` | the folder holds conversations, not files of a store; with it, `url`, `owner` and `path` are absent |
| `sessions.metadata` | at most one entry: a string keeps the sessions whose metadata holds the key with that value, `null` keeps the sessions whose metadata does not hold the key; absent or empty keeps every session of the folder's scope. The key follows the metadata key rule, the value the metadata value rule |
| `access` | `read_only`, and no other |

A folder of conversations counts toward `session.MaxMemoryFolders`, and
breaking a rule refuses the create `authorizer_unavailable` naming the
member, as every rule of 093. It needs no `TOPOS_FILES_AUDIENCE`: the
runner reaches it through the server. The session's object in the API
still carries `memory: [{name, access}]`.

### What the folder holds

The **scope** of a folder is the sessions that are:

- of the agents of the session's context, the list's own scope
  ([[015-api]], 036's context), and of the session's initiator alone,
  narrowed further by the owners of the authorizer's list decision;
- kept by the folder's metadata entry;
- of no rollout ([[077-rollouts]]);
- one per fork tree, the tree's newest ([[056-editing-a-message-and-the-branches-of-a-session]]);
- never of the reading session's own tree.

Newest first, at most `memory.MaxListed`; past it, a glob or a grep says
the rest were not read, as for any folder. A session of the scope that
the authorizer does not let the initiator read is left out.

### A conversation's file

`/memory/<name>/<yyyy-mm-dd>-<session id>.md`, the day its session was
created, in UTC. The name never changes while the title rolls. The file:

```markdown
# Tide tables

Session ses_01JA…, started 2026-10-28 09:00 UTC, last changed 2026-10-28 10:12 UTC.

## User, 2026-10-28 09:00 UTC

What time is high tide at Kiel tomorrow?

## Agent, 2026-10-28 09:01 UTC

High tide at Kiel tomorrow is at 14:12.
```

- Each event `session.Searched` reads is one section: `user.message` as
  User, `agent.message` as Agent, `user.answer` as User with each
  answer's chosen labels and words. The text blocks are kept as written,
  markdown and all.
- A session with no title is headed by its id.
- `memory.MaxConversationBytes`, 256 KiB, bounds a file. A conversation
  past it keeps its sections in order up to the bound, and the last line
  says how many later messages were left out.
- The file's version is the session's id, its last sequence, its title
  and the time it last changed, so the memory machine's cache reads a
  conversation again only once it has changed.

### Who answers

`memory.Conversations` is a session's reach to its folders of
conversations:

| Method | Answers |
|---|---|
| `ListConversations(folder)` | the folder's files, newest first, each with its name, version and the time its session last changed, and whether the scope held more than `memory.MaxListed` |
| `ReadConversation(folder, name)` | the file named and its text |

The server implements it for a session: the runners toposd drives call
it directly, and a runner process's lease asks it over the internal
listener at `POST /internal/v1/leases/{session}/conversations/{list,read}`
with the lease's generation, as the trigger tool asks. A runner with no
reach refuses a session with such a folder `files_unavailable` at setup,
as 093 refuses a session with no files audience.

The memory machine carries every call on a folder of conversations to
it: `List`, `Stat` and `ReadFile` read; a glob and a grep list the folder
and read each file once per version, as for any folder; `WriteFile`,
`Remove` and `Rename` are refused before anything is asked, since the
folder is read only.

### The authorizer

| Question | Asked | Fields |
|---|---|---|
| `session.list` | once per list | the list's fields (`status` and `runner` empty, `agent_owner` the session's context), and `origin: {kind: memory, session_id, folder}` |
| `session.read` | of each session of the scope a list answers, and of the session a read names | the fields every `session.read` carries, `metadata` (the session's metadata, absent when it has none), and `origin` as above |

Both are asked as the session's initiator in the session's context, as
086's trigger questions are. A deny of a read leaves the session out of
a list and answers a read as a file that does not exist; a question the
authorizer gives no decision on fails the call as "Memory could not be
reached". An allow is cached as every allow is, and the origin is part
of what it is cached by, so an allow of the person's own read never
answers a folder's.

The owner policy answers both as it answers the person's own list and
read: their own sessions. An installation that reads `origin` decides
less: which conversations a session may read, and whether the person
allows it at all.

### The store

`MetadataEntry` gains `Absent`: it keeps the sessions whose metadata
does not hold the key. Every store filters by it before it pages, the
Postgres store with `NOT EXISTS` over its metadata rows.

## Decisions

| Decision | Chosen | Alternatives and why not |
|---|---|---|
| How an agent reaches past conversations | a memory folder the file tools reach | a search tool over 050: one more tool where `glob`, `grep` and `read` already reach files; a folder of a files store holding a copy of each conversation: a second copy of every log, kept in step at every turn and moved and deleted with its session |
| Which sessions | the allow's metadata entry inside the initiator's own sessions of the context | the allow listing session ids: unbounded, and fixed at the create, so a conversation started later would be missing |
| A file's name | the day and the session's id | the title: a title rolls ([[083-a-title-that-follows-the-conversation]]), and a name read by a glob would be gone at the read |
| What a file holds | what the person and the agent said, as 050 searches it | every event: tool output is most of a log's bytes and the least of what a person means by a conversation |
| Who decides each read | the authorizer, at every list and read, with the session as origin | the allow alone: an installation could not stop a running session's next read |

## Roll order

1. An installation's authorizer decides `session.list` and `session.read`
   with `origin.kind` `memory`, and names a folder of conversations only
   to a core whose release notes say it reads one; a core before this
   release refuses the member.
2. This release: the member, the scope, the files, the reach, the
   questions, the store's filter.
3. The installation names the folder.

## Not in this spec

Searching by meaning. A narrowing of a grep through 050's text index,
which waits for a measured need. Conversations of other people, or of
another context. A folder of a person's conversations on a runner that
no server drives.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A folder of conversations is decoded with its rules and kept on the session; one with a store's address, a writable access, two metadata entries or a bad key or value refuses the create | `session.TestAFolderOfConversations`, `authorizer.TestTheMemoryMember` | built |
| A list holds the initiator's own sessions of the context that the entry keeps, a null entry those without the key, one per tree, never the reading session's tree, no rollout's run, and none the authorizer denies; each question carries the origin, and a read the session's metadata; a refused list is the refusal the file tools say | `internal/server.TestAFolderOfConversations` | built |
| A conversation's file is named by its day and id and holds the title, the dates and the searched events in order, cut at `memory.MaxConversationBytes` with a last line saying so | `memory.TestAConversationsFile`, `memory.TestAConversationsVersion`, `memory.TestALongConversationIsCut` | built |
| The memory machine lists, reads, globs and greps a folder of conversations, caches a file by its version, and refuses a write before asking | `memory/arca.TestAFolderOfConversations`, `memory/arca.TestAFolderOfConversationsFails`, `memory/arca.TestAFolderOfConversationsBesideAStore` | built |
| A runner process reaches the folder through its lease, under its generation | `internal/runnerrole.TestTheLeaseReachesConversations` | built |
| A session with only a folder of conversations needs no files audience | `internal/hosted.TestAFolderOfConversationsNeedsNoFilesAudience` | built |
| Every store keeps the sessions without a key by an absent entry | `session/storetest` `ListByMetadata` ("without the key"), on the memory, directory and Postgres stores | built |

## Outcome

Built as designed, with these differences:

- The file's version also holds the time the session last changed, so a
  change of the header alone, an archive for one, reads the file again.
- The day in a file's name is checked against the session's own: a name
  with another day reads as a file that is not there, so a name a glob
  read is the only one that opens a conversation.
- A read opens any session of a tree the folder lists, not only the
  tree's newest, which is the one the listing names: an older version is
  a session of the same scope, and the newest's log holds what it said.
- A read asks `session.read` of the session it names and not
  `session.list`; a list asks `session.list` once and `session.read` of
  each session of the scope, 8 at a time.
- A release before this one decodes the member without `sessions` and
  refuses it for its missing `url`, `owner` and `path`, which refuses the
  create: an installation names the folder only to this release or later.
- The runner process's route and its client are proven by one test in
  `internal/runnerrole`, which drives the route of `internal/runnerapi`.
- `memory.Refusal` carries a refused list as it carries a store's
  refusal, so the tool's result and its meta read as for any folder.
