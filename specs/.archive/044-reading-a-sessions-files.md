---
title: "Reading a session's files: one route answers a file of the working directory as a download, read from the machine while it runs"
status: complete
track: core
depends_on: [006-identity.md, 008-tools.md, 009-machines.md, 015-api.md, 018-credentials-and-secrets.md, 034-checkpoints-and-rewind.md]
affects: [internal/server/, internal/hosted/, machine/, machine/cella/, harness/tools/, cmd/toposd/, api/]
effort: small
created: 2026-10-05
updated: 2026-10-05
author: changkun
---

# Reading a session's files

## Overview

An agent writes a page, a chart or a report in its session's machine
and tells the person where it is: `/workspace/poetry.html`, a path on a
machine the person never reaches. The API serves a session's log, its
blobs and its stream, and no file the agent made. This spec adds one
route that answers a file of the session's working directory as it is
now, to anyone who may read the session, bounded in size and labeled a
download, so a client can show the file beside the answer that made it.

## Current state

`GET /v1/sessions/{id}/blobs/{digest}` answers what the session stored:
a message's attachments and the agent bundle ([[015-api]]). A file a
tool wrote lives only in the machine ([[009-machines]]). A `write` or
`edit` result's `meta` names the file's absolute path and its hash
([[008-tools]]), so a client knows which files a turn changed but has
no way to read them. A checkpoint commits the working directory at the
end of each turn ([[034-checkpoints-and-rewind]]), but a hosted session
whose working directory is not a checkout keeps no checkpoint, since
the server sets no session repository for its runners: a chat session
has nothing to read a file back from once its sandbox is gone.

## Design

### The route

`GET /v1/sessions/{id}/files?path=<path>` asks `session.read`, the
question every read of a session asks, with no new field, so an
authorizer that answers the session's other reads answers this one.
`path` is absolute, as a write or edit result's `meta.path` names it,
or relative to the working directory. `HEAD` answers the same headers
without the bytes.

| Answer | When |
|---|---|
| 200 with the file's bytes | the file exists and is at most `MaxFileBytes`, 10 MiB |
| `invalid_request` (400) | no `path`, two of them, a path outside the working directory, a directory, or a path the credential deny-list names |
| `not_found` (404) | a session the caller may not read, or no such file |
| `file_unavailable` (409) | the session's machine is not running: never opened, stopped while the session was idle, or gone |
| `file_too_large` (413) | a file past `MaxFileBytes`, with `size` and `limit` in the details |

The session is read and asked about before the path is looked at, so a
caller who may not read the session hears `not_found` whatever the path.

### The answer

The answer is a download, never a page of the API's origin, whoever
opens it and however:

| Header | Value |
|---|---|
| `Content-Type` | the extension's media type, else the one the first 512 bytes give |
| `Content-Length` | the file's size at its stat |
| `Content-Disposition` | `attachment` with the file's name, which a name with a control or a bidirectional character leaves out |
| `X-Content-Type-Options` | `nosniff` |
| `Content-Security-Policy` | `sandbox; default-src 'none'` |
| `Cache-Control` | `no-store` |

A file that grew between its stat and its read is answered at the
length the header promised; one that shrank ends the answer early.

### Where the bytes come from

`server.Options.Workspaces` reads a session's working directory as a
`machine.FileReader`, the read half of a machine, and never starts or
creates a machine: one that does not run is `machine.ErrNotRunning`,
which the route answers `file_unavailable`. `hosted.Workspaces` is the
one `toposd` runs:

| Session's machine | Read |
|---|---|
| Cella | `cella.Peek` finds the sandbox by name and reads its workspace through Cella's file routes while it is `Running`. A sandbox that does not exist or is in another phase is not running, and one that stops between the find and the read is too. Only the workspace is reachable: the helper, the spill directory and the rest of the sandbox are outside |
| host (`TOPOS_HOST_SESSIONS=on`) | the session's directory under the data directory, through an `os.Root`, so a symlink cannot lead out of it; a session that never ran on this host has none |
| any other, or a server with neither | not running |

The Cella read presents the credential the session's drive presents
([[018-credentials-and-secrets]]): the session's own Cella token when
the installation mints one, and `TOPOS_CELLA_TOKEN_FILE`'s bearer
otherwise. The read holds no lease, so it asks the minter for the Cella
audience alone; a Lux key minted without a lease would replace the key
the session's drive holds. The installation's authorizer already binds
the session's token to `sandbox.read` and `sandbox.exec` on the
session's sandbox, which are the calls the read makes.

### When the machine is not running

The file is read live. A sandbox Cella stopped after its idle time
keeps its workspace, and the file is readable again once a turn runs
the sandbox; the route does not start it, since a read must not spend
a machine's time. A session that keeps checkpoints at a repository
could have its file read from the last one; that read is not built
here, and a chat session keeps none.

### What a client shows beside a file

A `write` or `edit` result's `meta` gains `size`, the length in bytes
of the file it left, so a client lists the files a turn changed with
their sizes without reading any of them.

### API changes

| Route | Change |
|---|---|
| `GET /v1/sessions/{id}/files` | new: a file of the working directory as a download, `session.read` |
| `POST /v1/sessions/{id}/events` | a write or edit `tool.result`'s `meta` carries `size` |

## Not in this spec

A listing of the working directory; a write through the API; a read
from a checkpoint; a file past 10 MiB; starting a stopped sandbox to
read it.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A file is answered by its absolute or relative path with its media type, its length, an attachment disposition with its name, nosniff, the sandboxing policy and no-store; `HEAD` answers the headers alone | `internal/server.TestAFileIsAnsweredAsADownload` | built |
| No path, two, a directory, a path outside the working directory and a credential path are `invalid_request`; a missing file and another's session `not_found`; a file past the bound `file_too_large` with its size and the limit; none of them reads the file | `internal/server.TestAFileReadIsRefused` | built |
| A machine that does not run, and a server with no reader, are `file_unavailable`; any other failure is the server's | `internal/server.TestAFileOfAMachineThatDoesNotRunIsUnavailable` | built |
| The route asks `session.read` and its example is what it answers | `internal/server.TestEveryRouteAsksItsAction`, `internal/server.TestTheExamplesAreWhatTheRoutesAnswer` | built |
| A running sandbox's workspace is read by absolute and relative paths; the spill directory, paths outside the workspace and credential names are refused; a stopped, deleted or never-made sandbox is not running and is never started or created | `machine/cella.TestPeekReadsTheWorkspaceOfARunningSandbox`, `machine/cella.TestPeekRefusesWhatIsNotTheWorkspaces`, `machine/cella.TestPeekNeverWakesTheSandbox`, `machine/cella.TestPeekTellsASandboxThatStoppedMidRead`, `machine/cella.TestPeekRefusals` | built |
| The read presents the session's own Cella token when the installation mints one and the bearer otherwise, and a failed mint is the read's failure | `internal/hosted.TestWorkspacesReadARunningSandboxWithTheSessionsToken`, `internal/hosted.TestWorkspacesRefuseWhatTheyCannotReach` | built |
| A host session's file is read inside its directory, and a symlink out of it, a credential name and a path outside are refused | `internal/hosted.TestWorkspacesReadAHostSessionsDirectory` | built |
| A write and an edit result name the size of the file they left | `harness/tools.TestWriteRequiresCurrentContent`, `harness/tools.TestEditRequiresUniqueMatch` | built |
| Through toposd over the stubs: the file the agent wrote is read at the path its result names, with its size there, as a download, and a stopped sandbox is `file_unavailable` and stays stopped | `cmd/toposd.TestAFileTheAgentWroteIsReadThroughTheAPI` | built |

## Outcome

Built as designed on 2026-10-05. The route reads through
`server.Options.Workspaces`, which `toposd` sets to `hosted.Workspaces`
over its Cella URL, its bearer, its minter and, with host sessions on,
its data directory. The end-to-end test runs `toposd` over the stub
Cella and the stub model: the agent writes `poetry.html`, the person
reads it at the result's path, and once the stub stops the sandbox the
read is `file_unavailable` with the sandbox still stopped.

Two points the design left open were settled. A read past the bound is
refused rather than cut, so a client never shows half a file as the
whole. A path the deny-list names is `invalid_request` rather than
`not_found`, since the person who may read the session may know the
file is there; it holds credentials, which no reader of the session
reads.
