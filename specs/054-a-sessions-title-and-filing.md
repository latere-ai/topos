---
title: "A session's title and its filing: the title member of PATCH, the session.title_changed event, and an archive of an idle session"
status: in-progress
track: core
depends_on: [004-session-log.md, 006-identity.md, 015-api.md, 017-external-runners-handoff-fork.md, 041-approval-mode-change.md]
affects: [session/, internal/server/, api/openapi.yaml, docs/]
effort: small
created: 2026-10-06
updated: 2026-10-06
author: changkun
---

# A session's title and its filing

## Overview

A client that keeps a list of a person's sessions offers the actions a
list of conversations has: rename one, and file one away out of the
list. The title is set once, at create, and nothing changes it after.
An archive files a session away, but only once it has ended, and a
client whose sessions stay open for the next message, idle between
turns until the person deletes them, never has one to file.

This spec lets a person change a session's title through the update
they already change its model and approval mode by, recorded in the log
as every other change of the session is, and lets an idle session be
archived as an ended one is.

## Current state

- `PATCH /v1/sessions/{id}` takes `model` and `policy` and refuses any
  other member as `invalid_request` ([[015-api]], [[041-approval-mode-change]]).
- The title is the create's `title`, or a fork's copy of its parent's
  marked as its continuation ([[017-external-runners-handoff-fork]]),
  and is never bounded.
- `POST /v1/sessions/{id}/archive` refuses an idle or a running session
  as `conflict`, so that archiving never stops work ([[015-api]]).

## Design

### The title member

`PATCH /v1/sessions/{id}` takes `title`, a string, alone or beside
`model` and `policy`:

```json
{"title": "Release notes for v1.4.0"}
```

The title is trimmed of surrounding white space. One that is empty
once trimmed, holds a control character (any of Unicode's `Cc` class, a
tab and a line feed among them) or a line or paragraph separator, or is
longer than `session.MaxTitleLength`, 200 characters, is
`invalid_request` before the session is read. The bound is the one
constant every check and the API document read.

One question carries the whole change: `session.update` with
`session_id` and `title`, the trimmed title, beside the fields a change
of the model or the mode adds when the body names those ([[006-identity]]).
A deny is `forbidden` and changes nothing. An ended session is
`conflict`, as for any update: its log takes no more events.

### The event

An allowed change to a title other than the session's appends:

| Type | Written by | Read by the model | Payload |
|---|---|---|---|
| `session.title_changed` | the server | no | `by` (a Sender), `old` and `new`, the title the session had and the one it has, `old` empty for a session created without one |

It is appended in the same batch as `session.model_changed` and
`session.policy_changed` when the body names those too, and after them.
The header takes `title` from it. A change to the title the session has
appends nothing and answers the Session.

The event is a record of a change, as a model or a mode change is, and
is not redactable ([[018-credentials-and-secrets]]); deleting the
session removes it.

A title change does not move the header's `updated_at`: a client that
orders sessions by when they last moved keeps a renamed session in its
place. Every other event moves it as before, and retention, which reads
it, is unchanged.

A fork copies its parent's `session.title_changed` events as history and
keeps the title its own create gave it, as it keeps its own mode.

### Archiving an idle session

An archive refuses a running session alone. An idle session is filed
away as an ended one is, and stays what it was: it takes a message, its
turn runs, and it stays archived until it is unarchived. An unarchive
restores it to the lists as it is. Archiving never stops work, which is
why a running session stays `conflict`: its turn is interrupted or
waited out first.

## Not in this spec

Bounding the title a create or a trigger's template gives: a create's
title stays as long as the client sends it. Ordering the list by
anything but the id. A per-person name for a session another person
started: the title is the session's.

## Roll order

toposd alone. An authorizer that decides `session.update` by the fields
it knows may deny a question that carries `title` alone as an
invalid resource until it reads the field; the client then hears
`forbidden` and the title stays. Roll the authorizer's reading of
`title` before or with toposd.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A title alone asks `session.update` with `session_id` and the trimmed `title`, appends `session.title_changed` `{by, old, new}`, answers the Session with the new title, keeps `updated_at`, and a change to the same title appends nothing | `internal/server.TestASessionChangesItsTitle` | built |
| A title beside a model and a mode is one question and one batch, the title's event last | `internal/server.TestATitleChangesWithTheModelAndTheMode` | built |
| An empty, blank, control-character or 201-character title, and a title that is no string, are `invalid_request`; a deny is `forbidden`; another person hears `not_found`; an ended session is `conflict`; each leaves the title and the log as they were | `internal/server.TestATitleChangeIsRefused` | built |
| The header folds the title from the log, and a fork keeps the title its create gave it over its parent's copied changes | `session.TestApplyBatchFoldsATitleChange`, `internal/server.TestAForkKeepsItsOwnTitle` | built |
| An idle session is archived, takes a message while archived and stays archived; a running one is `conflict` | `internal/server.TestArchiveASession`, `internal/server.TestAnArchiveIsRefused` | built |
