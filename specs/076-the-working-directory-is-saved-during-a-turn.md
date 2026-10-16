---
title: "The working directory is saved during a turn: a long turn saves the working directory to the session's folder every few minutes, after a tool call that may have changed files, with the same work.saved record a turn end writes, so a client reading the folder mid-turn sees the work so far"
status: drafted
track: core
depends_on: [073-a-working-directory-kept-in-the-sessions-folder.md, 008-tools.md, 009-machines.md, 004-session-log.md]
affects: [session/, runner/, machine/, internal/filestore/, docs/configuration.md]
effort: medium
created: 2026-10-16
updated: 2026-10-16
author: changkun
---

# The working directory is saved during a turn

## Overview

[[073-a-working-directory-kept-in-the-sessions-folder]] keeps a session's working directory as plain files under `work/` in the session's folder, written at the end of each turn that changed files. A client that wants the work so far reads that folder. For a turn that lasts an hour, the folder holds nothing of that hour until the turn ends, and if the turn never ends normally (the machine is lost, the session is deleted, the client gives up), nothing of it is saved at all.

The first caller is ReplicHAI, which runs a research reproduction as one long turn and keeps a snapshot of the session's files every few minutes, so a run that fails late still leaves what it had done. On Topos those snapshots advance only at turn ends.

## Current state

The working directory is written to the session's folder at turn ends only, in the batch of the `session.status` that ends the turn ([[073-a-working-directory-kept-in-the-sessions-folder]]). The live files route reads one known path from the running machine ([[064-files-an-answer-links]]) and cannot list the directory, so a client cannot find a file the session created mid-turn. Not built.

## Design

**When.** During a turn, the runner saves the working directory after a tool call that may have changed files (`write`, `edit`, `bash`, and any tool the harness marks as writing) completes, at most once every `MidTurnSaveEvery` (three minutes). A turn shorter than that saves at its end exactly as today. The save runs between steps, never during a tool call, so it reads a directory no tool of this session is writing.

**What.** A mid-turn save is the turn-end save of [[073-a-working-directory-kept-in-the-sessions-folder]]: the same walk, the same per-file and per-save bounds, the same writes of the files whose digests differ from the copy's, and the same `work.saved` record, appended to the log after the tool result that triggered it. The fold of every `work.saved` in order still gives the copy as it stands, so readers need no change: a mid-turn save is one more save. The record carries `"during": true`, so a reader of the log can tell a mid-turn save from a turn end's.

**Bounded.** A mid-turn save counts against the same bounds as a turn end's. If one would take longer than its writing bound, it stops where a turn end's would and the rest is left for the next save, recorded as `deferred`. A save that fails (`machine_unavailable`, `files_unavailable`) is recorded as today and does not stop the turn.

**Cost.** At most one walk and its writes every three minutes of a turn, and none in a turn that changed nothing since the last save, since the walk's digests find nothing to write.

## Acceptance criteria

1. A turn that runs longer than `MidTurnSaveEvery` and changes files appends a `work.saved` with `during: true` after the first file-changing tool call past each interval, and the session's folder then holds the changed files.
2. A mid-turn save writes only files whose digests differ from the copy's and stays within the turn-end bounds, recording `deferred` when it stops short.
3. The fold of the log's `work.saved` records gives the same copy whether a turn's changes were saved mid-turn, at its end, or both.
4. A failed mid-turn save is recorded and the turn continues.
5. A turn that ends before `MidTurnSaveEvery` saves exactly as today, at its end.

## Not in this spec

A live directory listing on the running machine; saving more often than the interval on request; keeping each mid-turn save as a checkpoint ([[034-checkpoints-and-rewind]] stays at turn boundaries).
