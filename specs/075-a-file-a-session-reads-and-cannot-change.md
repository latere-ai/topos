---
title: "A file a session reads and cannot change: a message's attachment marked read-only is delivered outside the working directory, where no tool of the session can write, edit, move or delete it, and its digest is on the record"
status: drafted
track: core
depends_on: [004-session-log.md, 008-tools.md, 009-machines.md, 015-api.md, 064-files-an-answer-links.md, 073-a-working-directory-kept-in-the-sessions-folder.md]
affects: [session/, runner/, machine/, machine/cella/, internal/server/, api/openapi.yaml]
effort: medium
created: 2026-10-16
updated: 2026-10-16
author: changkun
---

# A file a session reads and cannot change

## Overview

A client sometimes hands a session a file that the session must read and must not change: the one copy of a paper that every run of a study reads, a dataset whose digest a result cites, the snapshot of a repository a paper's authors released. Today a message's attachment is written into the working directory at `attachments/<event id>/<name>`, where the agent's write, edit and bash tools can change it, delete it or commit it into a branch. A client that needs the file unchanged has no way to ask for that, and no way to show afterwards that it was unchanged.

The first caller is ReplicHAI, which audits whether research papers reproduce. A study runs one paper many times and compares the runs, so every run must read exactly the same inputs. ReplicHAI's local runtime and its memory runtime already deliver such files read-only and outside the workspace; on the Latere platform a study cannot run until Topos can do the same.

## Current state

A message's attachment is written into the working directory at `attachments/<event id>/<name>` ([[064-files-an-answer-links]]), where every tool of the session can change it and the working directory's copy under `work/` ([[073-a-working-directory-kept-in-the-sessions-folder]]) carries it. Nothing marks an attachment as read-only, no machine enforces one, and the record does not carry the attachment's digest. Not built.

## Design

**Marked on the attachment.** A `user.message` attachment gains `read_only: true`. Absent or false is today's attachment, unchanged.

**Delivered outside the working directory.** The runner writes a read-only attachment under a directory of the session's machine outside the working directory (for example `<machine dir>/inputs/<event id>/<name>`), when the machine opens or before the next step when it is open, as it delivers attachments today. The message's record names that path, and the model reads the path in the message as it does for an attachment.

**Unchangeable by the session.** No tool of the session can change a read-only input: the write and edit tools refuse its path, and the directory is not writable by the user the session's commands run as, so a bash command that tries to write, move or delete it fails. On a Cella machine that is a mount or an ownership the sandbox's process cannot change; on the host it is the host sandbox's deny rule. How it is enforced is the machine's; that it is enforced is the contract, and a machine that cannot enforce it refuses the message with `read_only_unavailable` rather than delivering a writable file.

**On the record.** The attachment's record carries its SHA-256 digest, as a blob's does, so a reader of the log can show which bytes the session was handed.

**Kept with the session.** A read-only input is not part of the working directory, so it is not written under `work/` (073) and never reaches a branch. A machine that is restored or replaced gets it again from its blob. A fork carries the inputs of the messages it copies.

## Acceptance criteria

| Criterion | Test | State |
|---|---|---|
| A message with a read-only attachment delivers it outside the working directory, at the path its record names, with its digest | `TestAReadOnlyAttachmentIsDeliveredOutsideTheWorkdir` | not built |
| The write and edit tools refuse a read-only input's path, and a bash command that writes, moves or deletes it fails, on a Cella machine and on the host | `TestNoToolChangesAReadOnlyInput` | not built |
| A read-only input is never in `work/` and never in a commit of the session's branch | `TestAReadOnlyInputStaysOutOfWork` | not built |
| A restored or replaced machine gets the input again; a fork carries the inputs of the messages it copies | `TestAReadOnlyInputSurvivesARestore` | not built |
| A machine that cannot enforce read-only refuses the message with `read_only_unavailable` | `TestReadOnlyUnavailableIsRefused` | not built |

## Not in this spec

Read-only repositories (a repository input is already cloned onto the session's own branch); inputs larger than an attachment's limit, which are a Storage reference rather than a message's bytes.
