---
title: "A file a person brings past 16 MiB: a source the files API redirects is read once at the https address it names, without the bearer, spooled to disk while it is hashed, written through an upload session, and delivered to a Cella machine as a stream"
status: drafted
track: core
depends_on: [009-machines.md, 068-a-sessions-files-in-a-files-store.md, 079-files-a-person-brings-into-a-message.md]
affects: [internal/filestore/, internal/server/, cmd/toposd/, machine/cella/, api/openapi.yaml]
effort: medium
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# A file a person brings past 16 MiB

## Overview

A message may name a file of the person's own files by `source` (spec
079), and the server reads it at the send. The read follows no
redirect, and an Arca-compatible files API answers a file past its
inline bound, 16 MiB by default, with a redirect to a presigned address
on its object store, so a person cannot bring a file past 16 MiB, and a
message's sources are held to 32 MiB together because the server holds
them in memory before it stores them. A scanned report, a slide deck
with pictures or a spreadsheet export is often larger.

This spec reads such a file at the presigned address, holds it on disk
rather than in memory while the send is checked, writes it to the
session's folder through the upload session the files store already
uses for a large file, and delivers it to a Cella machine as a stream.
The bound of one file becomes 256 MiB, a parsing service's own per-file
bound, so a document a person brings can still be parsed.

## Current state

- `filestore.ReadSource` sends the read with the sender's bearer,
  refuses any 3xx as `ErrSourceTooLarge`, and answers the body read
  whole into memory, at most `max` bytes.
- `internal/server` bounds a source by `MaxSourceBytes`,
  `filestore.InlineBytes` (16 MiB), and a message's sources by
  `MaxMessageSourceBytes`, 32 MiB together; `readSources` keeps each
  source's bytes on its `file`, and `putAttachments` and `prepareFork`
  write them with one `Put` from a byte buffer.
- `filestore.Arca.Put` already writes a file past `InlineBytes` through
  an upload session whose parts go straight to the bucket, reading the
  source as a stream.
- `harness.DeliverAttachments` streams `Files.Open` into the machine's
  `WriteFile`; the Cella machine's `WriteFile` reads a body that does
  not seek into memory whole, so that a write it sends again after the
  sandbox was woken sends the same bytes. The runner's restore of the
  working directory writes the same way.
- In a hosted installation the root file system is read-only, and the
  data directory, `TOPOS_DATA_DIR`, is the one writable directory.

## Design

### 1. The read follows one redirect, to https, without the bearer

`ReadSource` sends the read to the files API as before. An answer of
3xx is followed once: its `Location`, resolved against the request,
must be an `https` URL with a host, and is read with a new request that
carries no `Authorization` header and no other header of the first, so
the sender's bearer never reaches another host; the presigned address
carries its own credential. An answer of that second read that is a
redirect again, or anything but 200, and a `Location` that is missing
or not `https`, are failures of the files service, `files_unavailable`.

`ReadSource` answers a stream the caller closes, its length (-1 where
the answer names none), and the `Content-Type` of the answer that
carries the bytes. A declared length past `max` is
`ErrSourceTooLarge` before a byte is read, and the stream fails with
`ErrSourceTooLarge` once it would pass `max`, whatever the answer
declared.

### 2. A source is spooled, then written

`readSources` reads each of a message's sources, in order, before
anything is written, as spec 079 has it, and streams each one into a
spool while it hashes it with SHA-256 and counts it. A source of at
most `session.InlineTextBytes` stays in memory, as a data file does,
so the inline rule of spec 079 reads its bytes; a larger one goes to a
temporary file under the spool directory. Its media type is the files
API's, or the one its first 512 bytes give.

A source is read twice, once into the spool and once into the folder,
for three reasons:

1. **A refused message writes nothing.** Every source of a message is
   read and held to its bounds before the first is written, so a
   second source refused leaves no first file in the folder.
2. **The record comes first.** A message's record names each file's
   size and digest, and a fork's events are built before the fork
   exists, while the fork's folder is written with the fork's own token
   once its record is made.
3. **An upload session needs the size.** It is opened with the file's
   exact length, which an answer with no length does not give.

The file is then written from the spool with `Put`, in one request at
or below `filestore.InlineBytes` and through an upload session above
it, and `Put`'s digest must be the spool's, or the write is
`files_unavailable`. Data files in the event body, base64 and at most
5 MiB, are unchanged.

**The spool is bounded and cleaned up.** Each spool file is removed
when the send or the fork that read it ends, whatever its outcome. The
spools of every message a server reads at once are held to
`MaxSpoolBytes` together; a source that would pass it is
`files_unavailable`, 503, which says to try again. The server creates
its spool directory at start and removes what a process that stopped
before cleaning up left there. toposd spools to `spool/` under
`TOPOS_DATA_DIR`; a program that embeds the server names its own, or
spools to the system's temporary directory and sweeps nothing.

### 3. The bounds

| Bound | Value | Why |
|---|---|---|
| `MaxSourceBytes` | 256 MiB | a parsing service's per-file bound by default, so a document a person brings can be parsed; an Arca-compatible files API takes an object up to 5 GiB by default |
| `MaxMessageSourceBytes` | 512 MiB | two files at the bound; a message's sources together |
| `MaxSpoolBytes` | 2 GiB | four messages at their bound at once on one server's disk |

Memory does not grow with a file: a source past 64 KiB is never held
in memory, and the copies between the read, the spool and the write
use fixed buffers. The disk does: a server holds at most
`MaxSpoolBytes` of spools at once, which the data directory's volume
must hold beside what else it keeps.

### 4. Delivery to a Cella machine streams

The Cella machine's `WriteFile` keeps a body that seeks as it is,
since it can be sent again. A body that does not seek is read into
memory only up to `replayBytes`, 1 MiB, which a small file of a restore
fits within and is sent as before. A larger one is sent once as a
stream, as an archive's import is: the machine wakes the sandbox
first, and the write is not sent again if the sandbox stops in
between; the delivery then fails, the file stays pending, and a later
turn writes it again (spec 015). A write outside the workspace, through
the machine's helper, is never a message's file, and keeps reading its
body into memory.

### 5. What the model reads

The line that names a file already says its size (spec 015), and a
file past `session.InlineTextBytes` is never marked `inline`, so no
large source reaches a model's request.

## Decisions

| Choice | Picked | Weighed against |
|---|---|---|
| A redirect's target | https only | http or https, as a read of the session's folder takes: the presigned address carries its credential in its query and the person's file in its body, and a files API behind https is never followed to plain http |
| A second redirect | refused | followed: the files API names one presigned address for a file, and a chain would read from an address the files API did not name |
| Where a source waits | on disk, under the data directory | in memory: 512 MiB a message on a server whose runners share 1 GiB; and streamed straight into the folder: a refused second source would leave the first written, and a fork's record needs the digest before its folder can be written |
| A spool past `MaxSpoolBytes` | `files_unavailable`, try again | waiting for room: a send holds its request open, and a client retries a 503 |
| A large body to a Cella machine | streamed once after a wake | spooled to disk to send it again: a second copy of the file on the runner's disk for a stop between the wake and the write, which a later turn's delivery already covers |

## Compatibility

A client sees no change but the bounds: a file it brings may be up to
256 MiB, a message's up to 512 MiB together, and a file the files API
redirects, which was `attachment_too_large`, is read. A public file,
which the files API also answers with a redirect, is read the same way.
An installation keeps a spool directory, which toposd makes itself.

## Not in this spec

- A data file in the event body past 5 MiB.
- A file a session's answer writes, which `Put` already streams.
- A presigned address on plain http, which a local files API on http
  hands out: such a source is `files_unavailable` past the files API's
  inline bound.

## Acceptance criteria

| # | Criterion | Test | State |
|---|---|---|---|
| 1 | A read the files API redirects is followed once to its https address with no `Authorization` header, a second redirect and a target that is not https are refused, and the body is a stream held to `max` | `internal/filestore.TestReadSourceFollowsOneRedirect` | |
| 2 | A source past the old 16 MiB bound is read through the redirect, written through an upload session, and recorded with its size and digest, with the server's memory not growing with the file | `internal/server.TestALargeSourceIsStreamedIntoTheFolder` | |
| 3 | A source past `MaxSourceBytes`, sources past `MaxMessageSourceBytes` together and spools past `MaxSpoolBytes` are refused with nothing written | `internal/server.TestAMessagesSourcesAreBoundTogether`, `TestTheSpoolIsBound` | |
| 4 | No spool file is left after a taken message, a refused one and a fork, and the server removes the spool files a stopped process left | `internal/server.TestSpoolFilesAreRemoved` | |
| 5 | The Cella machine writes a body that does not seek as a stream, with its memory not growing with the file | `machine/cella.TestALargeWriteIsStreamed` | |
| 6 | The API document says a source may be redirected and states the new bounds | `internal/server.TestOpenAPIIsGenerated` | |
