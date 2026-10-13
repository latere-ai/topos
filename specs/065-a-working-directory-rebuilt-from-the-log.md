---
title: "A working directory rebuilt from the log: a machine made new for a session that lost its own, or for a fork with no checkpoint, gets back the files the session's log holds, and the model is told what is back and what is not"
status: testing
track: core
depends_on: [008-tools.md, 009-machines.md, 017-external-runners-handoff-fork.md, 034-checkpoints-and-rewind.md, 064-files-an-answer-links.md]
affects: [harness/tools/, runner/, session/, machine/, prompts/transcript/, internal/server/, api/openapi.yaml]
effort: medium
created: 2026-10-11
updated: 2026-10-13
author: changkun
---

# A working directory rebuilt from the log

## Overview

A session with no repository has no checkpoint
([[034-checkpoints-and-rewind]]): its working directory exists in its
sandbox and nowhere else. Three ordinary things give such a session a
new, empty machine. A sandbox fails and Cella does not start it again
([[009-machines]]). A sandbox reaches the end of its lifetime while the
session lives on. And a person forks the session, to send a message
again, to edit one, or to continue from an earlier point, which makes a
new session with a machine of its own
([[017-external-runners-handoff-fork]]). In each case the person keeps
the conversation and loses every file.

Most of those files are in the conversation. The log holds the content
of every `write`, the text of every `edit`, the bytes of every file an
answer showed or linked, and the text of every file a `read` returned
in full, each with the hash the file had then. Replaying them in order
gives the working directory back, checked against the hashes the log
recorded.

This spec does that replay when a session's machine starts empty: the
runner rebuilds the working directory from the session's log before
the turn's first tool runs, records what it put back, and tells the
model what is back and what is not.

| Capability | What a client reads |
|---|---|
| rebuilt directory | `session.machine` with reason `restored` and `restore {source: "log", ...}`: the files put back, the ones that may be out of date, and the ones that are missing |

## Current state

A machine made new in the place of one the session lost is recorded as
`replaced` with `session.error` `machine_lost`, and the model reads
that the files are gone ([[009-machines]]). A fork's first machine gets
the fork point's checkpoint when its turn kept one and the runner can
reach it, and otherwise starts on its repositories alone, with
`session.error` `checkpoint_missing` when a checkpoint existed and
could not be had ([[017-external-runners-handoff-fork]],
[[034-checkpoints-and-rewind]]). A session with no repository keeps no
checkpoint, so neither path gives it a file.

## Design

### Where it runs

The runner rebuilds at the open of a machine, in the step that records
the machine ([[016-runners]]): after the machine opens, and before the
call that opened it runs.

| The machine | Rebuilt |
|---|---|
| says its open made it new, and the session's log records an earlier machine of its own | yes: the one the session had is gone |
| says its open made it new, and is the first machine of a fork that copied events | yes, when no checkpoint was restored: the fork's files are the ones its copied log holds |
| says its open made it new, and is the first machine of a session that is no fork | no: nothing came before it |
| does not say its open made it new, such as a directory on a person's host | no: it may hold the session's own files, newer than the log's |
| was given the files of a checkpoint | no: the checkpoint holds every file |

A machine says its open made it new through `machine.Recovering`, which
a Cella sandbox answers ([[009-machines]]). A rebuild that finds no
file to write leaves the open as it was: `replaced` with
`machine_lost`, or a fork's `attached`.

### What is replayed

The replay reads the session's log in order, through every thread,
since the threads of a session share its working directory. A path is
a file of the rebuild when it lies under the working directory the
latest `session.machine` before it names, and it is written back at
the same path under the new machine's working directory.

| Event | What it gives |
|---|---|
| a successful `write` | the file's content, the call's `content` |
| a successful `edit` | the file as rebuilt so far with the call's `old_string` replaced by its `new_string`, once, or every time with `replace_all`, by the rule the tool itself applies ([[008-tools]]) |
| a successful `read` of a whole file | the file's bytes, taken from the result: every line behind its number from line 1, with a last newline or without, whichever hashes to the `sha256` the result recorded; an image as the result carries it; and an empty file. This is how a file a command made comes back, and how a file a command changed after its last write comes back as changed |
| `files.kept` | for each file it stored, the bytes of its blob at the path it was read from ([[055-images-an-answer-shows]], [[064-files-an-answer-links]]) |
| `session.machine` with reason `replaced` | nothing: it empties the rebuild, since what was there before was lost then |
| a failed call, a `read` of part of a file, a `bash` call | no bytes |
| a redacted event | nothing. A file whose latest `write` or `edit` was redacted, the call or its result, is not rebuilt from an older one |

No tool of the registry deletes or moves a file, so the log records no
delete and no move: a file a command removed or renamed is rebuilt as
it was last written, at the path it was written to. A file's mode is
not in the log either: a rebuilt file is written 0644, and a script a
command made executable is not.

### What is checked, and what is named

Every `write`, `edit` and `read` result records the `sha256` the file
had after it ([[008-tools]]), and a kept file's blob is addressed by
its hash. The rebuild holds each file to the last hash the log
recorded for it.

| The log shows | The file is |
|---|---|
| a hash the rebuilt bytes have | put back |
| a later hash the rebuilt bytes do not have, from a read of part of the file | put back as rebuilt and counted `unverified`: a command changed it after the log last held it, so it may be out of date |
| an `edit` whose text is not in the file as rebuilt | left out, reason `edit_not_applied`: a command changed it between two events of the log |
| an `edit` or a read of part of a file whose content no event holds | left out, reason `unknown_content`: a command made it |
| a file past the bounds below | left out, reason `limit` |
| a kept file whose blob the store no longer has | left out, reason `blob_unavailable` |

A file left out comes back when a later `write`, read of the whole of
it, or `files.kept` gives its bytes. What a restore left out is named
in its record and to the model, and is not written.

Files that only a command made are not in the log and the runner
cannot list them: generated data, downloads an answer did not link,
installed packages, build output. The record counts the commands that
ran after the first rebuilt file was made, and the model is told that
nothing a command made is back.

### Bounds

| Bound | Value | Constant |
|---|---|---|
| files a rebuild holds | 2,000 | `MaxRebuiltFiles` |
| bytes they hold together | 67,108,864 (64 MiB) | `MaxRebuiltBytes` |
| paths each list of the record names | 100 | `MaxRestoreNames` |
| the writing into the machine | 64 seconds: the time `MaxRebuiltBytes` take at 1 MiB a second | computed from `MaxRebuiltBytes` |

The files are written through the machine's own file API in one call
where it takes one, `ImportTar`, as a stream that is never held whole,
with each kept file's blob read from the store as it is written; a
machine that takes no archive is written file by file. A rebuild that
cannot be written leaves the machine recorded as `replaced` with
`machine_lost`, whose detail says why, or a fork with
`checkpoint_missing`.

### The record

The machine is recorded as `session.machine` with the reason `restored`
and a `restore` member in the place a checkpoint restore's `checkpoint`
has ([[004-session-log]]). No `machine_lost` follows it.

```json
{"type": "session.machine", "payload": {
  "machine": {"kind": "cella", "id": "sbx_01J9Z3Q4W8KX6T0M2V5N7R1B3C", "workdir": "/workspace"},
  "reason": "restored",
  "restore": {"source": "log", "files": 28, "bytes": 151204, "unverified": 1, "omitted": 1, "commands": 14,
              "paths": ["package.json", "src/app.js"],
              "unverified_paths": ["package.json"],
              "omitted_files": [{"path": "data/rows.csv", "reason": "unknown_content"}]}}}
```

| Member | Meaning |
|---|---|
| `source` | `log` |
| `files`, `bytes` | the files written back and their sizes together |
| `unverified` | how many of them a command changed after the log last held them |
| `omitted` | how many files the log shows were there and could not be rebuilt |
| `commands` | how many commands ran after the first of the files was made |
| `paths`, `unverified_paths`, `omitted_files` | the first `MaxRestoreNames` of each, by path under the working directory; `omitted_files` each `{path, reason}` |

A fork whose checkpoint existed and could not be had still records
`session.error` `checkpoint_missing`, with the sentence "The files of
the session this one continues could not be restored from its
checkpoint; its working directory was rebuilt from the conversation's
record instead."

### What the model reads

The fold renders the record into the session's own thread where it
happened, after the result of the call that opened the machine
([[004-session-log]]), from `prompts/transcript/machine-rebuilt-v1.md`:

```
The working directory is on a new machine and was rebuilt from this conversation's record: what the write and edit tools wrote, the files an answer showed or linked, and files that were read in full. 28 files are back:
- package.json
- src/app.js
- and 26 more
A command changed these after the record last held them, so they may be out of date:
- package.json
These were there and could not be rebuilt, so they are missing:
- data/rows.csv
Nothing that only a command made is back: generated files, downloads, installed packages and build output are missing, and so is every process that was running. 14 commands ran after the first of these files was made. Regenerate what you need before you rely on it, and tell the person what is back and what is not.
```

A rebuilt file hashes to what the log recorded for it, so the tools'
rule for a file that changed since it was read ([[008-tools]]) lets
the agent edit it without reading it again where the new machine's
working directory is at the path the old one's was, as a session's
sandboxes are; an unverified file is refused there until it is read.

### Checkpoints first

A fork whose fork point kept a checkpoint the runner can reach is
restored from it, as before, and nothing is replayed: a checkpoint
holds every file, a command's included. The replay is what a session
with no checkpoint has, and what a fork gets when its checkpoint is
missing.

### A copy in the session's folder first

A session that keeps its files in a folder of a files store keeps the
latest state of its working directory there too
([[073-a-working-directory-kept-in-the-sessions-folder]]). Its new
machine is restored from that copy first, a command's files included,
and this replay runs over the copy: the copy stands in at the point it
was saved, and only the events after it, and the files the copy does
not hold, come from the log. Such a machine is recorded with
`restore {source: "folder"}`. A copy that cannot be written at all
leaves the machine to this replay alone, recorded `source: "log"` with
`copy {failed}`.

### Decisions

| Choice | Picked | Weighed against |
|---|---|---|
| What gives the files back | the session's own log | a checkpoint of every session: a session with no repository has no place to keep one that outlives its sandbox; an export of the workspace before a sandbox is deleted: the sandbox that failed cannot be read, and the one whose lifetime ended is gone |
| When | at the machine's open, before the call that opened it | when the person asks: the agent meets an empty directory first and acts on it |
| A read as a source | a read of the whole file that hashes to its recorded hash | the write and edit calls alone: a file a command made and the agent then edited would not come back, and a file a command changed would come back stale |
| A file that does not verify | put back and named as possibly out of date | left out: the agent's own last version is closer to the file than nothing |
| A file whose edit does not apply | left out and named | put back as it was before the edit: it would read as current and is known not to be |
| Which machines | ones that say their open made them new | every machine a fork starts on: a directory on a person's host may hold newer files than the log |
| The record | reason `restored` with a `restore` member | a reason of its own: a client already reads `restored` as files put back, and the member says from what |

## Not in this spec

A session with a folder, whose copy of its working directory comes first
([[073-a-working-directory-kept-in-the-sessions-folder]]). Restoring a
session's own latest checkpoint into a machine that
replaced a lost one ([[034-checkpoints-and-rewind]]), and delivering
its repositories into it again ([[019-git]]): such a machine gets the
files its log holds and no clone. Running the commands again to
regenerate what they made. A rebuild a person asks for of a session
whose machine is still there. How a client shows the record.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The replay gives back a written file, each edit applied in order, a file a command made from the read of the whole of it, with a last newline or without, an image from its read, and an empty file, each byte for byte as the machine held it, and counts the commands since the first file | `harness/tools.TestRebuildReplaysWritesEditsAndFullReads` | built |
| A file read only in part, or edited with no content in the log, is left out `unknown_content`; a file a command changed between two edits is left out `edit_not_applied`; a file a command changed after its last write is put back and marked unverified; a read of the whole of each gives it back; a failed call gives nothing | `harness/tools.TestRebuildNamesWhatItCannotGiveBack` | built |
| A `files.kept` gives each file its blob's bytes and an edit after it applies to them; a redacted event gives nothing and a file whose latest write was redacted is not rebuilt from an older one; a machine recorded as `replaced` empties the rebuild; a path outside the working directory is no file of it | `harness/tools.TestRebuildTakesKeptFilesAndHonorsRedactionsAndLosses` | built |
| A rebuild holds at most `MaxRebuiltFiles` files and `MaxRebuiltBytes` bytes, leaves a file past either out as `limit`, and takes it once there is room | `harness/tools.TestRebuildHoldsToItsBounds` | built |
| A machine made new in the place of one the session lost comes back with the written and edited files byte for byte and the edited file whose base a command made and the agent read, file by file and as one archive; the file read in part is named, the one only a command made is absent; the machine is recorded `restored` with the counts and the paths and no `machine_lost`; the model reads what is and is not back; and the agent edits a restored file without reading it again | `runner.TestAReplacedMachineIsRebuiltFromTheLog`, `internal/hosted.TestAReplacedSandboxIsRebuiltFromTheLog` | built |
| A rebuild the machine does not take leaves it `replaced` with `machine_lost`, whose detail says why; a session whose log holds no file is `replaced` as before | `runner.TestARebuildThatCannotBeWrittenIsALoss`, `runner.TestAReplacedMachineIsRecordedAsALoss`, `internal/hosted.TestAFailedSandboxAnOlderCoreDoesNotStartIsReplacedAndSaid` | built |
| A fork's first machine, made new and with no checkpoint, is rebuilt from its copied log and recorded `restored`, and its model reads it; a fork on a machine that does not say it is new is left as it is | `runner.TestAForksFirstMachineIsRebuiltFromTheLog` | built |
| A fork whose fork point kept a checkpoint is restored from it as before, with no rebuild | `runner.TestAForkRestoresTheForkPointsFiles`, `runner.TestAForkRestoresFromTheRepository` | built |
| The fold renders a rebuilt directory's record with its lists and counts in the session's own thread, and a checkpoint's restore as nothing | `session.TestARebuiltWorkingDirectoryIsSaid`, `prompts.TestEveryTextRendersItsCurrentBytes` | built |
| The OpenAPI document states `restore`, its members, the omitted reasons and the bounds | `internal/server.TestTheDocumentStatesKeptImages`, `internal/server.TestOpenAPIIsGenerated` | built |

## Outcome

Built on 2026-10-11, in no release yet, and not yet run against a
model gateway or a Cella installation; the archive path is proven
against the stub Cella. Every criterion has its test. What shipped
differs from the first draft in these points:

- **A read of the whole file updates a file the log already holds**,
  not only one it holds nothing of: a file a command changed after the
  agent wrote it comes back as changed once the agent read it in full.
- **A read of an image gives the image back**, since the result carries
  its bytes.
- **A redacted result drops its file**, as a redacted call does: with
  the result gone the log no longer says whether the write happened.
- **Deletes and moves are not honored**, because no tool of the
  registry records one.
- **A rebuilt file is written 0644.**
- **Amended on 2026-10-13** by
  [[073-a-working-directory-kept-in-the-sessions-folder]]: a session
  with a folder is restored from the copy of its working directory first,
  and the replay runs over it; a file the copy gave keeps its mode.
