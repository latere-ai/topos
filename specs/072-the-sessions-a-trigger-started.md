---
title: "The sessions a trigger started: the sessions list and its summary take trigger=<id>, which keeps the sessions that trigger started beside every other filter, the cursor and group=tree"
status: testing
track: core
depends_on: [014-store.md, 015-api.md, 022-triggers.md, 056-editing-a-message-and-the-branches-of-a-session.md, 057-a-sessions-metadata-in-its-questions-and-lists.md]
affects: [session/, internal/server/, internal/store/postgres/, internal/store/postgres/migrations/, api/]
effort: small
created: 2026-10-12
updated: 2026-10-25
author: changkun
---

# The sessions a trigger started

## Overview

A trigger starts sessions on a schedule or on a delivered event
([[022-triggers]]), and each session it starts records the trigger's id
as `trigger_id`. A client that shows a routine, a trigger as the person
made it, groups the routine's runs into one row and gives the routine a
page of its own with all of its runs. It cannot ask for them: the list
of sessions filters by agent, status, runner, archived, tree and one
metadata entry, not by trigger. So a client counts only the runs in the
first page of the whole list, and a routine's page pages through every
session of the person to find its own.

This spec adds one filter. `GET /v1/sessions?trigger=<id>` lists only
the sessions that trigger started, and `GET /v1/sessions/summary`
counts them.

| Capability | What a client reads |
|---|---|
| a routine's runs, a page at a time | `GET /v1/sessions?trigger=<id>`, newest first, with `next_cursor` |
| how many runs a routine has, by status | `GET /v1/sessions/summary?trigger=<id>` |

## Current state

- A session started by a trigger's firing has `trigger_id`, set at its
  create and changed by nothing; a session a person or a fork created
  has none (`session.Session`, `internal/server/sessions.go`).
- `GET /v1/sessions` reads `agent`, `status`, `runner` and `archived`
  in `sessionScope`, `root`, `parent` and `group` in `treeFilters`, and
  `metadata.<key>` in `metadataFilter` (`internal/server/sessions.go`).
  `GET /v1/sessions/summary` takes the scope and the metadata filter.
  `GET /v1/sessions/search` takes the scope alone.
- `session.ListOptions` has no member for a trigger. The memory and
  directory stores filter by `ListOptions.keeps`; the Postgres store by
  its columns, where `trigger_id` lives only in the session's body.

## Design

### The filter

```http
GET /v1/sessions?trigger=trg_01M34HGBA0JR98WKKXWZGAX6VA&archived=any&limit=50
GET /v1/sessions/summary?trigger=trg_01M34HGBA0JR98WKKXWZGAX6VA
```

- The value is a trigger's id, `trg_` and its ULID. Any other value is
  `invalid_request` naming `trigger`, as a malformed `archived` or
  `group` is. An empty value is no filter, as an empty `root` is.
- It keeps the sessions whose `trigger_id` is the value, beside every
  other filter, with the cursor and the limit as the list pages today.
- It narrows the list the caller's scope already gives: the agents of
  the caller's context and the owners the authorizer's `session.list`
  decision names. The question is unchanged; the filter narrows what an
  allow admits and widens nothing.
- The trigger is not looked up. The id of no trigger, of a deleted one,
  and of another context's trigger each keep none of the caller's
  sessions, and a caller cannot tell them apart.
- With `group=tree` the filter keeps the sessions it matches and the
  list groups those by tree ([[056-editing-a-message-and-the-branches-of-a-session]]):
  a tree stands for itself when one of its sessions the trigger
  started. A fork of a run records no trigger, so it is not kept, and
  the tree stands as the run.

`session.ListOptions` gains `Trigger string`, which every store applies
in List and in Summarize.

### The column

Migration `0010_session_trigger`:

```sql
ALTER TABLE sessions ADD COLUMN trigger_id text;
UPDATE sessions SET trigger_id = body::jsonb ->> 'trigger_id'
    WHERE COALESCE(body::jsonb ->> 'trigger_id', '') <> '';
CREATE INDEX sessions_trigger ON sessions (trigger_id, id DESC) WHERE trigger_id IS NOT NULL;
```

A create writes the column beside the body, and a save of the header
sets it where it is empty and the header names a trigger, as the tree
columns are kept. A list by trigger then reads the index and orders by
the session's id as today, so a page is one indexed read. While an
earlier release still runs beside this one, a session it creates has
the trigger in its body only, and enters the filtered list once its
first batch saves its header, which a run's first turn does.

### The routes that take it

| Route | Takes `trigger` | Why |
|---|---|---|
| `GET /v1/sessions` | yes | the routine's page |
| `GET /v1/sessions/summary` | yes | the routine's row counts its runs by status in one read, which is what the list could not give; it takes `metadata.<key>` for the same reason |
| `GET /v1/sessions/search` | no | it takes none of the list's narrowing filters, tree or metadata, and no client searches within one routine's runs |

### Decisions

| Decision | Chosen | Alternatives and why not |
|---|---|---|
| The parameter | `trigger=<id>` | `trigger_id`: the list's other filters are named by the thing, `agent`, `root`, `parent`; a trigger's name: a name is read in a context, and a run keeps the id |
| A malformed value | `invalid_request` | no match: a client that sends a name or a mistyped id would read an empty routine |
| An unknown or another context's id | no session, with no lookup | `not_found`: the answer would tell an id that exists in another context from one that exists nowhere |
| Where the filter runs in Postgres | a column with a partial index | the body's JSON per row: the body is text, and a cast per row reads every header the scope keeps |
| The summary | takes it | left out: the count is the one number a routine's row shows, and the list gives it only by paging |

## Compatibility

A client that sends no `trigger` sees no difference. A client facing a
core before this release has the parameter ignored and lists every
session; it tells the two apart by the core's version, or by a filtered
answer that holds a session whose `trigger_id` is another.

## Not in this spec

A filter by several triggers, by a session's absence of a trigger, or
by a firing. The trigger's own counts ([[022-triggers]]), which count
firings, not sessions.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `GET /v1/sessions?trigger=<id>` lists only the sessions that trigger started, newest first, a page at a time through the cursor, beside `agent`, `root` and `archived`, and under `group=tree`, where a fork of a run leaves its tree to the run | `internal/server.TestListFiltersByTheTriggerThatStartedASession` | built |
| Another context's trigger and an id of no trigger list none of the caller's sessions, and another caller lists none of the trigger's | `internal/server.TestListFiltersByTheTriggerThatStartedASession` | built |
| The summary counts the sessions the filter keeps | `internal/server.TestListFiltersByTheTriggerThatStartedASession` | built |
| A value that is not a trigger's id is `invalid_request` naming `trigger`, in the list and the summary | `internal/server.TestListFiltersByTheTriggerThatStartedASession` | built |
| Every store keeps the filter beside the owners, the agents, a tree, a grouping by tree and the archived filter, and counts the same set: memory, directory and Postgres | `session/storetest`'s `ListByTrigger` case, run by each store's conformance test | built |
| The migration fills the column from each body, a session written without the column gains it when a batch saves its header, and a create writes it | `internal/store/postgres.TestTriggerColumnFollowsTheBody` (postgres tier) | built |
| The API document names the parameter on the list and the summary | `internal/server.TestOpenAPIIsGenerated` | built |

## Outcome

Built on 2026-10-12, in no release yet. Every criterion has its test,
and each new test failed against the code before the change. The
search route takes no `trigger`, as the table above decides.
