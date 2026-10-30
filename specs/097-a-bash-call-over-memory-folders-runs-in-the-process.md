---
title: "A bash call over memory folders runs in the process: a line whose every program is one Cella's shell package provides and whose every path is in a memory folder runs on the memory machine with no sandbox, decided before anything runs; every other line runs on the session's machine as before; and a log line and two counters say which call opened the machine and why"
status: testing
track: core
depends_on: [008-tools.md, 012-permissions-and-approvals.md, 023-events-and-observability.md, 048-the-machine-starts-at-the-first-tool-call.md, 093-memory-folders-the-file-tools-reach.md, 094-a-memory-folder-of-past-conversations.md]
affects: [memory/arca/, harness/, harness/tools/, prompts/, internal/hosted/, internal/server/, api/openapi.yaml, cmd/toposd/, docs/memory-folders.md, .lateregate.yaml]
effort: medium
created: 2026-10-30
updated: 2026-10-30
author: changkun
---

# A bash call over memory folders runs in the process

## Overview

A session with memory folders ([[093-memory-folders-the-file-tools-reach]])
reaches them with `read`, `write`, `edit`, `glob` and `grep`, which run
on the runner and open no machine. Agents reach for `bash` all the same:

```sh
cd /memory/user && ls && cat nikita-heumann-your-latere-ai-cofounder.md
```

Before this spec that call opened the session's sandbox, a wait of
about sixteen seconds on a session that had none, and then failed: the
folders are served by the runner, and the sandbox does not see them.

This spec runs such a line on the runner, over the same folders, with
no sandbox. The shell is `latere.ai/x/cella/shell` (Cella spec 088,
"A shell over served files"): a POSIX interpreter whose programs are
its own, over a file system its caller serves, which decides before
anything runs whether a line is one it can run. The memory machine is
that caller. A line it cannot run goes to the session's machine as
every bash call did, and nothing of it has run in the process.

The agent sees one bash tool. The result is the shape every machine
answers: the output, standard error in its place, the exit code, and
the directory the line ended in. The shell's messages are dash's and
the programs' are GNU's, so a line answers as a sandbox's `/bin/sh -c`
would, and no result names where it ran.

## Current state

Before this spec, the memory machine refused every command, and the
harness sent every bash call to the session's machine, starting it when
the call's block began to stream (spec 048).

## Design

### Where a bash call runs

The harness asks the memory machine about each bash call of a session
with memory folders, with the line and the directory the call starts
in, the session's persistent directory:

| The call | Runs on |
|---|---|
| `background: true` | the session's machine, which alone keeps a job |
| a line the shell runs from that directory | the memory machine, in the process |
| any other line | the session's machine |

A line the shell runs is one whose every program is one it provides,
every option one that program takes, and every path in a memory folder
or `/memory` itself: `ls`, `cat`, `head`, `tail`, `wc`, `grep`
(`egrep`, `fgrep`), `find`, `sort`, `uniq`, `cut`, `mkdir`, `rm`, `mv`,
`cp`, `touch`, and the builtins `cd`, `pwd`, `echo`, `printf`, `test`,
`[`, `true`, `false`, `:`, `exit`, `set`, `read`, `break` and
`continue`, joined by pipes, lists, `if`, `for`, `while`, `case`,
subshells and redirections. Cella spec 088 holds the options each
program takes and the reasons a line is refused: `program`, `option`,
`path`, `expansion`, `construct` and `syntax`. A command substitution,
a variable the line does not set, a program of any other name, a path
outside the folders (`/tmp` included), and a relative path while the
directory is the sandbox's are refused, so the line runs on the
machine.

The decision is made on the line alone, before the call runs: a line
never starts in the process and moves to the machine halfway, since
what it changed in a folder cannot be undone. The memory machine checks
the line again when it runs it and answers a line it refuses as a
command it does not run.

In a session with memory folders the harness does not start the
session's machine when a bash call's block begins: the line has not
streamed yet, and one that reaches the folders alone needs none. The
call opens the machine when it runs, if it needs it, as a file tool's
does (spec 093).

A bash call keeps a command's verdict. The memory verdict of spec 093,
allowed in `confirm` as a memory the person can review, stays the file
tools': one line can change many files at once.

### The folders as the shell sees them

`/memory` is served read only, and each folder at `/memory/<name>`,
read only unless its access is `read_write`. A folder of conversations
(spec 094) is read only. A read-only folder refuses a change before
any request, with `Read-only file system`, as a disk mounted read only
does.

Every read and write is the memory machine's own, with the session's
token and the versions spec 093 keeps:

| The line | The files API |
|---|---|
| a listing or a stat | the folder's listing, read once for the line and changed by the line's own writes; a file past `memory.MaxListed` is stat with `HEAD` |
| a read | `GET`, kept by its checksum; a file the line's listing names at a checksum the session holds is not read again |
| a write of a file the session read | `PUT` with `If-Match` on the version it read |
| a write of a file the session never read | the version the line's listing or a stat answers now, then `PUT` with `If-Match`, or `If-None-Match: *` for a file not there |
| a write of no bytes | the file forgotten, `DELETE` with `If-Match`, as spec 093's empty write; nothing for a file not there |
| `rm` | the file forgotten |
| `mv` | a write at the new path, then the old one forgotten |
| `mkdir` | nothing sent: a directory is there while a file is under it, and one the line makes lasts for the line |

`touch` of a missing file therefore makes none, and `: > notes.md`
forgets `notes.md`. A directory a folder is mounted at, `/memory` or a
folder itself, cannot be removed or moved: `Device or resource busy`.

An error of the store is printed in the shell's words for its kind:

| The store | The line prints |
|---|---|
| `412`, the file changed since the session read it | `changed since it was last read; read it again before writing`, the file tools' words |
| a refusal with its error envelope | its message and its words, as the file tools print them |
| a `5xx`, no answer, or an answer that does not decode | `Input/output error` |
| a file past `memory.MaxFileBytes` | `File too large` |

A line runs under the call's timeout and the turn's context: past the
timeout it is stopped and answered as a timed-out command, and a cancel
answers it canceled. Its messages begin `/bin/sh: 2:`, as a machine's,
whose shell runs the line after the line that reports its directory.
A result past the tool's bound is written to a spill file on the
session's machine, as a file tool's long result on the memory machine
is: that call opens the machine to write it.

### What a line takes of toposd

A line on the memory machine runs inside toposd, beside every other
session's calls, so what one line can take of the process is bounded
whatever the model writes. The bounds are the shell's own (Cella spec
088), at its defaults: 50 000 steps, 10 seconds, a value of 256 KiB,
1 MiB of variables, 16 MiB held at once, 64 MiB of files read and
50 000 names listed, with a line refused before it runs when it is
longer than 64 KiB, nests deeper than 64, has a command of more than 16
expansions, more than 32 pipes or a pattern of more than 8 wildcards.
`Time` and `Steps` are taken between statements, and the sizes hold
one statement's cost to a few seconds. A line that reaches a bound
stops where it is and answers the shell's message with status 2:

```text
/bin/sh: 2: the line ran more than 50000 steps
```

It is a command that ran, not a timed-out one, and it never moves to
the session's machine halfway. What it changed before the bound stays
changed, and the command that reached the bound writes no file. A line refused for its size runs on the
session's machine, counted with the reason `size`, as any line the
shell does not run.

The memory machine adds two bounds of its own:

- **Output.** A line keeps at most `memory.MaxFileBytes`, 1 MiB, of
  output, against a machine's 64 MiB. The tool shows 32 KiB of a result
  and spills the rest, so a spill still holds a whole long listing or
  a whole file read, and what is past 1 MiB is dropped as what is past
  64 MiB is on a machine.
- **Lines at once.** At most half the process's processors, and at
  least one, run lines at once. A line past them waits for a place
  under its own timeout and the turn's context, and its 10 seconds
  start once it has one.

### The working directory

A line that ends in a folder leaves the session's persistent directory
there, as any line does, so `cd /memory/user` and then `ls` lists the
folder. A later bash call that runs on the session's machine while the
directory is in a folder starts in the machine's working directory,
since the machine has no such directory, and its result begins

```text
[/memory/user is in a memory folder, which the programs of this command do not see; the command ran in the working directory /workspace]
```

The directory that command ends in replaces the folder's.

### What the model is told

`context/attachments-v3` replaces version 2's "commands do not" with
what is now true:

```text
Memory folders, kept across sessions. read, write, edit, glob and grep reach them, and so does a command that uses only ls, cat, head, tail, wc, grep, find, sort, uniq, cut, mkdir, rm, mv, cp, touch and the shell's builtins; any other program does not see them. Write a file empty to forget it:
```

It names the programs so a model does not reach for `sed` or `python`
over a folder and meet a sandbox that does not see it. It says nothing
of where a command runs.

### The record of a machine opened

A machine opened on demand opens at most once in a drive: a new
sandbox, a stopped one started, or a running one reached again. The
harness records the call that opened it, once per drive:

- a line of the runner's log, `a tool call opened the session's
  machine`, with the session, the thread, the turn, the step, the tool,
  the call's id, the reason and, for a bash call, the program the shell
  named;
- the counter `latere.topos.machine.opens` with `tool` and `reason`.

For a bash call the reason is the shell's, or `background`; for a call
on the memory machine whose long result opened the machine to spill,
`spill`; for any other tool, `tool`.

The counter `latere.topos.bash.runs` counts each bash call of a session
with memory folders, with `where`, `process` or `machine`, and for the
machine its `reason`. Both counters' attributes are closed sets; the
program, which is any word a line names, is in the log line alone. They
are exported through `latere.ai/x/pkg/otel` with the server's other
metrics.

No event carries this. `session.machine` records a machine attached or
changed for the session's clients, not each drive's open, and why a
machine opened is the installation's figure, read from its logs and
metrics.

### The follow-on: a workspace served from Storage

The memory folders are files no sandbox has. The follow-on is the
session's working directory served the same way: its files kept in the
installation's files store, a sandbox's disk a cache of them, so a line
that only lists, reads and writes files there runs in the process too,
and a line that runs a program runs in a sandbox that sees the same
files. The shell already decides between the two. It is not built here,
and these questions are open:

- On Cella's side (Cella spec 088, "The environment side of a shared
  directory"): a volume filled from a files service when a sandbox
  starts and written back as it changes; who writes last when the
  process and a running sandbox change one file, and how the sandbox
  learns of a change it did not make; whether a write back is a version
  with the same `If-Match`.
- On Topos's side: the bounds a workspace needs that a memory folder
  does not, files past 1 MiB, more than 2,000 files, a repository's
  `.git`; whether the copy spec 073 keeps of the working directory is
  that store or a second one; and whether a line that names both
  `/workspace` and a program waits for the sandbox's copy to be current.
- How many calls it would serve: `latere.topos.bash.runs` counts the
  calls declined for `path`, which include the lines a served workspace
  would run, beside those that name a path outside any folder.

## Decisions

| Decision | Chosen | Alternatives and why not |
|---|---|---|
| Where the shell is | Cella's public `latere.ai/x/cella/shell`, with its own file system interface, mounts and read-only mounts | a package of Topos: the shell is useful to any program that serves files, and the follow-on puts it beside the sandbox it stands in for |
| When it is decided | before the call runs, on the line alone | start in the process and move to the machine at the first program the shell lacks: what the line changed by then cannot be undone |
| Who asks the shell | the memory machine, through `Runs`, so the harness tree imports no interpreter | the harness calling the shell: the harness would reach the interpreter in every binary, and the mounts are the memory machine's |
| The working directory | kept in a folder; a call the machine runs from there starts in the working directory with a note | never keep a folder as the directory: `cd /memory/user` then `ls` would list the workspace, which no shell does; send the folder's directory to the machine: one failed command per call, and spec 008's note would say the folder no longer exists |
| Starting the machine | not at a bash block's start in a session with memory folders | start it there: a session that only reads its memory with bash would get a sandbox for nothing; the cost is that a drive's first bash call that needs the machine waits for its open after its line streams |
| The verdict | a command's | the memory verdict: one line can forget or move many files, which the person reviews as a command |
| Writing a file the session never read | the version the file is at now, then `If-Match` on it | refuse it until it is read, as `write` does: a redirection means replace, and a shell whose `>` is refused reads as broken; the `If-Match` still refuses a change made between the look and the write |
| A file of no bytes | forgotten, as spec 093's empty write; `touch` of a missing file makes none | keep an empty file: the store's rule is that an empty write forgets, and two rules for one path would let `: >` keep what `write` forgets; refuse the truncation: `: > f` is how a shell empties a file |
| A move | a write at the new path, then a forget | the store's server-side copy: not among the calls spec 093 makes, and the write and the forget carry the preconditions the session's versions give |
| The record | a log line and two counters | a member on `session.machine`: clients read that event, it is not appended at each drive's open, and why a machine opened is the installation's question |
| The counters' attributes | tool, where and reason, each a closed set | the program as an attribute: any word of a line, an unbounded series |
| What the model is told | `context/attachments-v3`, naming the programs | keep version 2: it says commands do not reach the folders, which is no longer true; say commands reach them: a model would run `python` over a folder and find nothing |
| What a line takes of toposd | the shell's bounds on steps, time, values, bytes held, read and listed, with sizes refused before the line runs | send every loop to the session's machine: it bounds nothing else, since one command's expansions, a pipe into `sort` and a pattern over a large folder take memory without a loop, and a `for` over a folder's files is the commonest loop over notes; the call's timeout alone: it bounds time, not memory, and a line doubling a value reaches 110 MB in 17 ms; Go's memory limit: it is the process's, and a line past it slows every session instead of stopping |
| A line past a bound | stops with the shell's message and status 2 | answer it timed out: it did not run out of time, and a model reads a timeout as a reason to run it again with more; hand it to the machine: what it changed by then cannot be undone |
| The output a line keeps | `memory.MaxFileBytes`, 1 MiB | 64 MiB as a machine keeps: a machine's output is in its own memory, the memory machine's in toposd's; 32 KiB, the tool's bound: nothing would spill, and the tail of a long listing, what a model reads most, would be lost |
| Lines at once | half the process's processors, at least one, waiting under the call's timeout | no cap: the bounds hold one line, not a session that sends many; one at a time: a session's line over memory would wait on another's store calls; refuse past the cap: the model would read a busy process as a broken shell |

## Roll order

1. Cella tags the release that holds `shell` (Cella spec 088).
2. This release requires it and ships the routing, the adapter, the
   prompt and the record. Nothing on the wire changes; the API's
   sentence on what reaches the folders is the one documentation
   change.

## Not in this spec

The shell itself, its programs, their options and their output: Cella
spec 088. A workspace served from Storage, beyond its questions above.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The production line, `cd /memory/user && ls`, a relative path after `cd`, and a write into a read-only folder run in the process; a pipe to an unknown program, a command substitution, a path outside `/memory`, a background job and an option a program does not take run on the machine; deciding sends nothing | `memory/arca.TestRuns`, `harness.TestTheRouteOfABashCall` | built |
| The production line lists and reads with one listing and one read, opens no machine, and ends in the folder; a later line starts there | `memory/arca.TestALineOverTheMemoryFolders` | built |
| A write sends `If-Match` on the version read, or on the version a look answers for a file never read; a write of nothing forgets and `touch` makes nothing; `mv`, `cp`, `mkdir` and `rm` work; a read-only folder and `/memory` refuse every change before any request; a file changed since the read is refused in the file tools' words | `memory/arca.TestALineChangesTheMemoryFolders` | built |
| A store's refusal, a store not reached, and a file past the bound are printed in the shell's words | `memory/arca.TestTheStoreRefusesALine` | built |
| A folder past the listing's bound is stat and written on the version a stat answers | `memory/arca.TestAFolderPastTheListing` | built |
| A folder of conversations is listed and read, and changes nothing | `memory/arca.TestAFolderOfConversationsOnALine` | built |
| A background command, one with input or variables, and a refused line are not run in the process | `memory/arca.TestWhatALineDoesNotRun` | built |
| A line past its timeout is timed out and a canceled one canceled, whether it waits on the store or for a place; a line waits for a place and runs once it has one | `memory/arca.TestALineThatRunsPastItsTimeout` | built |
| An endless loop stops with the shell's message and status 2 and opens no machine; a line keeps 1 MiB of output; half the processors run lines at once | `memory/arca.TestALineIsBounded` | built |
| Each bound stops a hostile line in seconds with a bounded allocation: doubling by assignment and by `read`, a 16 MiB line with no break read through a `<` or a pipe, a `printf` field wider than 16 MiB, a pipe into `sort` and `uniq`, an endless loop, deep nesting, wide expansion, and a pattern over 2 000 files | Cella's `shell.TestADoublingValueStops`, `TestADoublingReadStops`, `TestALongLineStopsItsRead`, `TestPrintfIsBounded`, `TestAPipeIntoSortOrUniqStops`, `TestAnEndlessLoopStops`, `TestDeepNestingIsRefused`, `TestWideExpansionIsRefused`, `TestAGiantGlobStops` | built |
| In a turn, a memory-only bash call neither starts nor opens the machine and answers as a machine's shell; a call that runs a program opens it, starts in the working directory with the note, and is logged and counted with its reason and program | `harness.TestABashCallOverMemoryOpensNoMachine` | built |
| A long line over memory opens the machine to spill alone, recorded as `spill` | `harness.TestALongLineOverMemorySpills` | built |
| The counters count by their closed attributes | `internal/hosted.TestTheMeterCountsRunsAndOpens` | built |
| The model is told which commands reach the folders | `prompts.TestEveryTextRendersItsCurrentBytes`, `harness.TestTheAttachmentsBlockNamesMemory` | built |
| On a server, a session lists and searches its memory with bash and creates no sandbox | `cmd/toposd.TestMemoryAcrossSessions` | built |
| Each program's output, messages and exit status match GNU's | Cella's `shell.TestGNU` over `shell/testdata/gnu.golden` | built |

## Outcome

Built on 2026-10-30 on Cella's `shell` package: the memory machine's
`Runs` and `Exec`, its file system over the memory folders, the
routing, the working directory's note, `context/attachments-v3`, and
the log line and counters, and the bounds on what one line takes of
toposd: the shell's, a megabyte of output, and half the processors at
once. Every criterion above is proven by its test. The status is `testing` until a release carries it.
