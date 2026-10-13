---
title: "A session's files in a files store: an allow of a create names a folder at an Arca-compatible files API, a message's attachments and an answer's kept files are written there by the session and named by ref, the blob route reads them by digest as a stream, and a fork copies them on the server"
status: testing
track: core
depends_on: [015-api.md, 017-external-runners-handoff-fork.md, 018-credentials-and-secrets.md, 055-images-an-answer-shows.md, 056-editing-a-message-and-the-branches-of-a-session.md, 058-what-a-create-allow-attaches.md, 064-files-an-answer-links.md, 065-a-working-directory-rebuilt-from-the-log.md]
affects: [authorizer/, session/, harness/, internal/filestore/, runner/, internal/hosted/, internal/server/, internal/identity/, internal/config/, internal/store/postgres/, session/dir/, cmd/toposd/, test/stubs/arcastub/, test/stubs/idpstub/, api/openapi.yaml, docs/configuration.md]
effort: large
created: 2026-10-12
updated: 2026-10-13
author: changkun
---

# A session's files in a files store

## Overview

A file a person attaches to a message, an image an answer shows and a
file an answer links are blobs of the session today
([[015-api]], [[055-images-an-answer-shows]],
[[064-files-an-answer-links]]). Each is held whole in memory while it
is written and read, a linked file is bounded at 16 MiB and an answer's
linked files at 32 MiB together, a fork reads every one into memory to
copy it, and nobody finds them outside the answer that named them.

An installation that runs a files service, a store people browse and
share their own files in, wants a conversation's files there: in a
folder of the conversation's own, in the space of whoever owns it,
counted and shown like any other file. This spec gives the core a
**files store** beside its blob store, with one backend for an
Arca-compatible files API, used for those three kinds of file and no
other. The core's own records, model requests and responses, an
agent's bundle, instructions, stay blobs.

The authorizer decides where a session's files go: its allow of
`session.create`, and of `session.fork`, may name a folder. A session
whose allow names none, and an installation that configures no files
store, run exactly as before. The open core depends on no files
service.

## Current state

- `harness.Log.PutBlob` takes a stream and every store reads it whole;
  `harness.BlobReader` answers a blob as a stream from a whole body.
- An attachment is stored by the API process at the send
  (`storeAttachments`), a kept file by the harness when its step is
  committed (`KeepFiles`), both through the session's blob store.
- `GET /sessions/{id}/blobs/{digest}` reads the blob store and answers
  with no `Content-Length`.
- A fork reads every blob its copied events name into one map and
  writes them with the fork ([[017-external-runners-handoff-fork]]).
- The core mints a session's own token for an audience and a workload
  ([[018-credentials-and-secrets]]) and authenticates as its host client
  with no audience; it holds no token of its own for any other service.

## Design

### The folder an allow names

An allow of `session.create` or `session.fork` may carry one more
limits member:

```json
{"folder": {"url": "https://files.example.com/v1",
            "owner": "https://issuer.example|8f1c",
            "path": "files/sessions/ses_01JA3B"}}
```

| Member | Meaning | Checked |
|---|---|---|
| `url` | the root of an Arca-compatible files API, under which `files/...` and `uploads` are served | an absolute `http` or `https` URL with no user, query or fragment |
| `owner` | the space the folder is in, as the files API names it | present, no control character |
| `path` | the folder, a path of that space | relative and clean: at least two segments, none empty, `.` or `..`, no control character, no trailing slash |

The core keeps the member with the session record, reads nothing into
its parts and learns nothing else about spaces. A member that fails a
check is the allow's error, `authorizer_unavailable`, as any member is.
An allow that names a folder on a server with no files store
(`TOPOS_FILES_AUDIENCE` unset) is refused the same way: the authorizer
asked for something this server cannot do, and a session on blobs would
put its files where the installation does not look for them.

The session object answers `folder: {"owner", "path"}`, the member
without its address, from the first file the session stores there; a
session that has stored none answers no `folder`. A fork's copied files
are stored files, so a fork of a session with files answers its own
folder from its creation.

### Where a file goes

| Path under the folder | Holds | Written |
|---|---|---|
| `attachments/<event id>/<name>` | a file a person attached to the message whose event id it is, the path the runner writes it at in the working directory | by the API process, before the message is appended |
| `results/<turn>/<path>` | a file an answer of that turn showed or linked, at its path inside the working directory | by the harness, when the answer's step is committed |
| `work/<path>` | the latest state of the working directory ([[073-a-working-directory-kept-in-the-sessions-folder]]) | by the harness, at the end of a turn that changed files, in place |

Both writes are create-only (`If-None-Match: *`), so a written file is
never changed: an old answer keeps what it handed over whatever later
turns do. An attachment record and a `files.kept` entry gain **`ref`**,
the file's path relative to the session's folder, beside `blob`, its
digest, which stays the name a client reads a file by. An entry with no
`ref` is a blob, as every entry before this spec is.

### The files store

`harness.Files` is the interface, beside the harness's blob seam, so a
harness that dials nothing names it; its one backend is in a package of
its own, `internal/filestore`, that dials only the files API it is
given:

| Method | Does |
|---|---|
| `Put(ctx, ref, mediaType, size, r)` | writes exactly `size` bytes of `r` at `ref`, create-only, as they are read, and answers their digest |
| `Overwrite(ctx, ref, mediaType, size, r)` | `Put` in place of whatever `ref` holds ([[073-a-working-directory-kept-in-the-sessions-folder]]) |
| `Open(ctx, ref)` | answers the file at `ref` as a stream, with its length |
| `Delete(ctx, ref)` | removes the file at `ref` for good; nothing there is no error |
| `CopyFolder(ctx, from, dir)` | copies the folder `dir` of another folder of the same space and files API into the same place of this one, on the server |

A file goes when a redaction leaves no live event naming it ("Reading")
and when the working directory no longer holds it
([[073-a-working-directory-kept-in-the-sessions-folder]]); a session's
folder goes with the session, by the installation that named it.

`filestore.Arca` is that backend, a client of Topos's own: an Arca-compatible files API exports none. Each call:

| Call | Request | Headers |
|---|---|---|
| a put of at most `filestore.InlineBytes`, 16 MiB, the files API's single-request bound by default | `PUT {url}/files/{owner}/{path}/{ref}` | `Authorization: Bearer <token>`, `Content-Type`, `Content-Length`, `If-None-Match: *`; an overwrite sends no `If-None-Match` |
| a larger put | `POST {url}/uploads` `{"owner", "path", "size", "content_type"}`, then `PUT` each of `part_urls` with no `Authorization`, then `POST {url}/uploads/{id}/complete` `{"parts": [{"n", "etag"}]}`; a failure after the open is `DELETE {url}/uploads/{id}` | the bearer on the three API calls, `Content-Length` on each part, `If-None-Match: *` on the completion |
| a read | `GET {url}/files/{owner}/{path}/{ref}`; a redirect is followed once, to an `http` or `https` address, with no `Authorization` | the bearer on the first request alone |
| a copy | `POST {url}/files/{owner}/{from path}/{dir}` `{"copy_to": "{path}/{dir}", "recursive": true}` | the bearer |
| a delete | `DELETE {url}/files/{owner}/{path}/{ref}?permanent=1`, so the file is kept in no trash; `404` is a file already gone | the bearer |

`{owner}` and each segment of a path are percent-encoded. A refusal of
the files API is a `harness.FileError` with its status and code; a
create-only write onto a taken path is `harness.ErrFileExists`, a read
or a copy of nothing is `harness.ErrFileNotFound`, and a source of
another size than its write named is `harness.ErrFileSizeChanged`.

### Credentials

`TOPOS_FILES_AUDIENCE`, unset by default, configures the files store:
the audience of every token the core presents to a folder's files API.
It needs `TOPOS_IDENTITY_URL`.

| Who | Token |
|---|---|
| the API process, writing an attachment and copying a fork's folders | the session's own token for the audience, workload `session`, minted as the core mints one for Cella and the git host |
| the harness, writing a kept file and reading an attachment it delivers | the same, through the drive's token source; a runner in its own process asks the server for it as for any audience |
| the API process, reading a file for the blob route | the core's own token for the audience: the client credentials grant of `TOPOS_IDENTITY_CLIENT_ID`, so a session that ended, or whose agent was archived, stays readable |

The sandbox workload gets nothing for the files API, and no credential
for it ever reaches a machine.

### Writing

**An attachment.** With a folder, each file of a message is put at
`attachments/<event id>/<name>` before the message is appended, and
its record carries `ref`. A put the files API refuses for the file's
size is `attachment_too_large`, and any other failure is
`files_unavailable`; either refuses the message, and nothing is
appended. A fork's message that names a file of the session forked by
its blob carries that file's `ref`, which the fork's copy makes
resolve.

**A kept file.** With a folder, the harness reads each image and
linked file as before and puts it at `results/<turn>/<path>`, `<path>`
its resolved path made relative to the working directory, with the
entry's `ref`. The bounds of a linked file become the files API's own:
neither `MaxKeptFileBytes` nor `MaxKeptFilesBytes` applies, and
`MaxKeptFiles` and the image bounds stay. A linked file streams from
the machine to the files API with its size from its `Stat`, and a file
whose size changed between the two fails its put. A put the files API
refuses is a skipped entry, `too_large` for a refusal of its size and
`unavailable` for any other, and the turn goes on.

A path taken already, as when two answers of one turn link one file,
or a keep sent again after a crash between the put and the append, is
compared rather than written: the machine's file and the file at the
path are each read through a digest, and an equal pair is kept with the
existing `ref`, while a file that changed between the two is skipped
`unavailable`, since `results/<turn>/` is written once.

The keep's bound grows with the bytes: with a folder, each file's read
and put is bounded by the keep's bound plus the time its size takes at
1 MiB a second, in place of one bound for the message.

### Reading

**By digest.** `GET /sessions/{id}/blobs/{digest}` answers by digest as
before. The core keeps a digest index, `(session, digest, ref)`, one
row for each file of an appended event that carries `ref`, written in
the append that records the file and removed with the session; a
redaction that leaves no event naming a digest removes its rows, and
the route then answers the digest `not_found`.

**A redaction deletes the file.** Once a redaction is written, the API
process deletes from the session's folder, for good and with the
session's own token, each file whose ref the log recorded before the
redaction and no live event records after it. Two answers of one turn
that link one file share its ref, and so do a fork's copied message and
the fork's own message that attaches the same file by its digest: the
file goes only once no live event names its ref. A fork's folder holds
its own copy, so a redaction in the session forked deletes nothing of
the fork's. The redaction is the record and stands whatever becomes of
a delete: a delete the files service refuses, as it refuses a session's
token once the session has ended, is logged with its ref, and the file
stays in the folder until the folder goes. A redaction sent again plans
nothing and deletes nothing more.

A digest the index names is read from the folder with the core's own
token and streamed with the files API's `Content-Length`; the other
headers are as today, the first bytes still choosing an image or a
download. A digest it does not name is read from the blob store, as
before. A read the files API fails is `files_unavailable`, and one it
answers as missing `not_found`. No file is held whole.

**On a machine.** The runner writes an attachment with `ref` into the
working directory from the folder, with the session's token, and a
rebuild of a working directory from the log ([[065-a-working-directory-rebuilt-from-the-log]])
reads a kept file with `ref` from the folder, within the rebuild's
bounds.

### A fork

A fork's allow names the fork's own folder. For each folder of the
session forked that the copied events and the fork's message name,
`attachments/<event id>` and `results/<turn>`, and for the session's
`work` when it keeps a copy of its working directory
([[073-a-working-directory-kept-in-the-sessions-folder]]), the fork asks
one copy of it into its own folder, with its own token, after its record is
written and before its log is: the copied entries keep their `ref`s,
which then resolve in the fork's folder, and the append writes the
fork's index rows. A copy that fails deletes the fork, as a failed copy
of the log does, and the fork is `files_unavailable`. A session forked
whose copied events carry `ref`s, forked under an allow that names no
folder, is `files_unavailable`. A session on blobs forks as before.

A session and its fork are in one space, so the copy never crosses
one; a folder of another space or files API is refused before any
copy.

## Decisions

| Choice | Picked | Weighed against |
|---|---|---|
| Where the folder comes from | the allow, kept with the record | the core computing a space from the session's owner: a rule of the installation's, held twice |
| An allow naming a folder where no files store is configured | refused `authorizer_unavailable` | ignored, the session on blobs: the installation would look for files that are not there, and no one would see why |
| The index | rows written by the append that records a file | a scan of the log at each read: a long log read for every image; rows a writer adds beside the log: a remote runner would need a route of its own |
| A taken path | compared, kept when equal | a new path: breaks the layout a client reads; skipped at once: loses an answer's file to its own retry |
| A Storage refusal of a kept file | an existing reason, `too_large` or `unavailable` | a new reason: a client that knows the closed list would meet an unknown one |
| The single-request bound | the constant `filestore.InlineBytes`, the files API's default | a setting: the files API does not publish its own, and one constant serves the installations that keep the default |
| Reads | the core's own token | the session's: it stops with the session and with its agent's identity |
| A redacted file | deleted from the folder once no live event names it, with the session's token | left in the folder until the folder goes: a person would still find among their files what they removed from the conversation |

## Not in this spec

Moving the files of sessions that exist into a folder. Deleting a
session's folder, the installation's. An upload route at the core for
attachments past 5 MiB. `Range` requests.

## Roll order

1. A release with the files store, run with `TOPOS_FILES_AUDIENCE`
   set. With no allow naming a folder, nothing changes.
2. The authorizer names folders.

A runner older than step 1 does not write `ref`; a client that does not
read `ref` reads every file by digest as before.

## Acceptance criteria

| # | Criterion | Test | State |
|---|---|---|---|
| 1 | The allow's `folder` is decoded and checked, kept on the record, answered as `{owner, path}` from the first stored file and never with its address; an allow naming one with no files store is `authorizer_unavailable` | `authorizer.TestAnAllowNamesAFolder`, `internal/server.TestTheAllowsFolderIsKeptAndAnsweredOnceUsed` |built |
| 2 | The Arca backend puts at or below the bound with `Content-Length` and `If-None-Match: *`, above it through an upload session whose parts carry no bearer, reads through a redirect that receives no bearer, copies a folder, and maps refusals | `internal/filestore.TestArcaPutsReadsAndCopies`, `TestArcaRefusals` and `TestArcaDistrustsWhatTheFilesAPIHandsOut` against `test/stubs/arcastub` |built |
| 3 | A message's attachments are put at `attachments/<event id>/<name>` with the session's token and carry `ref`; a refused put refuses the message and appends nothing | `internal/server.TestAttachmentsGoToTheSessionsFolder` |built |
| 4 | An answer's images and linked files are put at `results/<turn>/<path>` and carry `ref`; a linked file past 32 MiB is kept; a refusal is a skipped entry; a taken path with the same bytes keeps the existing `ref` | `harness.TestKeptFilesGoToTheFilesStore` |built |
| 5 | The blob route answers a digest the index names from the folder with the core's own token, as a stream with `Content-Length`, and the blob store's digests as before; a redacted file's digest is `not_found` | `internal/server.TestTheBlobRouteStreamsAFileOfTheFolder`, `session/storetest`'s `FileIndex` for each store |built |
| 6 | The runner delivers an attachment with `ref` from the folder, and a rebuild reads a kept file with `ref` from it | `harness.TestAnAttachmentWithARefIsDeliveredFromTheFolder`, `harness.TestATurnKeepsIntoItsFilesStore`, `runner.TestARebuildReadsKeptFilesFromTheFolder` |built |
| 7 | A fork copies each folder its copied events and its message name, with its own token, and its copied `ref`s resolve in its own folder; a failed copy leaves no fork | `internal/server.TestAForkCopiesItsFoldersOnTheServer`, `TestAFolderThatCannotBeOpened` |built |
| 8 | The core's own token is asked with the audience by the client credentials grant | `internal/identity.TestServiceTokenAsksTheAudience` |built |
| 9 | End to end, on the stubs: a message with an attachment and an answer that links a file land under the folder, the blob route answers both by digest as a stream, the core's own token reads them, and a fork reads them from its own folder | `cmd/toposd.TestASessionsFilesLiveInTheFilesStore` |built |
| 10 | A redaction deletes from the folder, for good and with the session's token, each file no live event names once it is written: a message's attachment, and an answer's file once both answers that link it are redacted; a fork's copy, and a file a fork's copied message still names, stay; a refused delete leaves the redaction written and the file in place. The backend overwrites in place and deletes with `permanent=1` | `internal/server.TestARedactionDeletesItsFilesFromTheFolder`, `internal/server.TestTheBlobRouteStreamsAFileOfTheFolder`, `session.TestARefGoesOnceNoLiveEventNamesIt`, `internal/filestore.TestArcaOverwritesAndDeletes`, `test/stubs/arcastub.TestTheStubDeletesForGood` | built |
| 11 | Every ref a fork's log records opens in the fork's own folder, and its blob route answers it, after the folder of the session it was forked from is emptied: a fork at the last turn, a fork before a later message that attaches that message's file again, and a fork of a fork whose own parent's folder is emptied too | `internal/server.TestAForksRefsResolveInItsOwnFolder` | built |

## Outcome

Built on 2026-10-12, in no release yet, and run against the stub files
API, the stub identity provider and the stub Cella alone, not against an
installation's files service. Every criterion has its test. Where the
build differs from the design as first written:

- **The interface is the harness's.** The harness and the session
  schema dial nothing (spec 001), so `Files` and its errors sit in
  `harness` beside `BlobReader`, and the client of the files API in
  `internal/filestore`. A runner that embeds the harness passes any
  implementation in `Config.Files`.
- **The digest index is written by the append.** The append that
  records a file with a `ref` writes its row in the same transaction,
  in Postgres the table `session_files` (migration 0011); the directory
  and memory stores answer the same question from the log
  (`session.FileRefOf`). A runner in its own process needs no route of
  its own for it.
- **A fork's message that names a parent's file by blob keeps the
  parent's `ref`**, under the parent message's event id, and the fork
  copies that folder; the record's `path`, where the runner writes the
  file, is under the fork message's own id. Ref and path then differ.
- **A runner that drives a session with a folder and reaches no files
  store** fails the turn's setup with `files_unavailable`, rather than
  keeping the session's files as blobs where its installation does not
  look for them.
- **Redaction** removed the index rows of a digest no live event names,
  so the blob route answered it `not_found`, and left the file in the
  folder. Since 2026-10-13 the API process also deletes the file from
  the folder (criterion 10).
- **A fork's refs** were checked on 2026-10-13 for a ref that still
  resolves against the parent's folder: every read, the blob route, the
  runner's delivery of an attachment and a rebuild's read of a kept
  file, opens the session's own folder, and a fork copies each folder its
  refs name, a re-attached file's from after the fork point included.
  None reads the parent's; criterion 11 holds it with the parent's
  folder emptied.
- **What is left for an installation.** The single-request bound is a
  constant, 16 MiB, since the files API does not publish its own; an
  installation that lowers it refuses a put between the two as
  `object_too_large`. `results/<turn>/` is written once a turn, so a
  file that changed between two answers of one turn keeps the first
  answer's copy and the second answer's link is skipped `unavailable`.
