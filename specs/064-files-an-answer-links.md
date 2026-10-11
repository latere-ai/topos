---
title: "Files an answer links: each local file an agent message links is kept from the machine as a blob of the session, recorded by files.kept beside the images the message shows, and answered by the blob route as a download"
status: testing
track: core
depends_on: [044-reading-a-sessions-files.md, 055-images-an-answer-shows.md]
affects: [session/, harness/, runner/, machine/cella/, internal/server/, internal/runnerapi/, session/storetest/, api/openapi.yaml]
effort: medium
created: 2026-10-11
updated: 2026-10-11
author: changkun
---

# Files an answer links

## Overview

An agent fetches seven papers with a script, packs them into an
archive, and answers with links to what it made:

```markdown
All seven papers are in the working directory.

[Download all papers as a ZIP](arxiv-papers.zip)

- [Continual graph memory](arxiv-papers/2610.02945-continual-graph-memory.pdf)
```

Nothing in the session's log names those files. A file a command writes
is not in the log by name ([[008-tools]]), and `files.kept` keeps only
the images an answer shows ([[055-images-an-answer-shows]]). A client
that draws the answer has a link with nothing behind it: the files
route ([[044-reading-a-sessions-files]]) reads the file only while the
session's machine runs and refuses one past its bound, a copy of the
conversation shown to someone else holds none of it, and the file is
gone once the machine is.

This spec keeps a linked file with the answer the way an image is
kept. When a step of the session's own thread is committed, the runner
reads each local file the step's `agent.message` links from the open
machine, stores it as a blob of the session, and records it in the same
`files.kept` that records the message's images. The blob route answers
it as a download. Nothing about the model's work changes: it links a
file it made, as it already does.

| Capability | What a client reads |
|---|---|
| session file | the `files.kept` event after an `agent.message`, whose `files` hold an entry for each linked file; `GET /v1/sessions/{id}/blobs/{digest}` answers the file as a download |

## Current state

`harness/keep.go` reads the Markdown images and the HTML `img` sources
of an `agent.message`, keeps the first `MaxImages` of them under the
bounds of an image, and appends one `files.kept {message, files,
skipped}` for the message ([[055-images-an-answer-shows]]). A link
that is no image is not read: that spec left files of other kinds to a
later one. A client learns of a file that is no image only from the
result of a `write` or an `edit` call, and reads it through the files
route, which answers at most 10 MiB while the machine runs
([[044-reading-a-sessions-files]]). The blob route already answers a
blob that is no image as an `application/octet-stream` attachment with
`nosniff` and the sandboxing policy.

## Design

### Which files

The runner reads the text blocks of each `agent.message` of the
session's own thread with the CommonMark parser that finds its images,
in one pass, so a reference inside a code span or a fenced block is
text and names nothing.

| Reference | Kept |
|---|---|
| a Markdown inline link, `[text](dest)` or `[text](<dest> "title")`, and a reference-style link whose definition is in the same message | yes, as a linked file |
| an HTML `a` element's `href` in the message's text | yes, as a linked file |
| a destination with no scheme (`report.pdf`, `./out/report.pdf`, `/work/out/report.pdf`) or the `file:` scheme | yes, after percent-decoding, with any query and fragment dropped, as for an image |
| a destination with the prefix `sandbox:` before a path, `sandbox:/work/report.pdf` or `sandbox:report.pdf` | yes: some models write the prefix before a path of the machine, and what follows it is read as a destination with no scheme |
| a `sandbox:` prefix before a destination with a scheme of its own, or before nothing | no, and nothing is recorded for it |
| a destination with another scheme (`https:`, `mailto:`, `data:`), and an autolink, which always has one | no: not a file of the machine, and nothing is recorded for it |
| a destination the message both links and shows as an image | once, as the image, at its first mention, under the image's rules alone |

A link is kept whatever the file is: a report, an archive, a
spreadsheet, a source file a coding answer points at. A relative
destination resolves against the working directory and an absolute one
is kept only inside it, a `sandbox:` path included, by the rules and
the read of [[055-images-an-answer-shows]]. A path the credential
deny-list names ([[018-credentials-and-secrets]]) is skipped
`unavailable`, so a link never copies a credential file into the log.
The same destination linked twice in one message is kept once.

### When

When the step is committed, in the keep that reads the step's images,
so a client has one `files.kept` per `agent.message`, at the place it
has today: after the step's `tool.result` events and before the next
request or the turn's `session.status`. A step that waits on a person
keeps its linked files with its images, in the claim that answers its
calls. The keep reads only a machine that is open and never opens one
to keep a file: in a turn that opened none, a link is skipped
`no_machine`.

### What is kept

| Check | Bound | Constant |
|---|---|---|
| linked files kept from one message | at most 16, counted apart from its images; a link past it is skipped `limit` | `MaxKeptFiles` |
| one linked file's bytes | at most 33,554,432 (32 MiB), read by `Stat` before the read; a file past it is skipped `too_large` | `MaxKeptFileBytes` |
| the linked files of one message together | at most 67,108,864 bytes (64 MiB); a file that would pass it is skipped `limit`, and a smaller one after it is still kept | `MaxKeptFilesBytes` |
| the kind | a regular file of any type; a directory, a named pipe or a device is skipped `not_a_file` | none |
| the media type | the type the file name's extension names, and otherwise the type its leading 512 bytes give | `SniffBytes` |

The count bounds the reads of the machine as the count of images does:
the first `MaxKeptFiles` local links are read, whatever becomes of
them, and every later one is skipped `limit`. A message therefore
keeps at most `MaxImages` images and `MaxKeptFiles` linked files. Each
bound is one constant of the code, referenced wherever it applies and
never restated.

The media type is the rule the files route answers a file by
([[044-reading-a-sessions-files]]), one function that both call, so a
client labels a kept file as it labels the same file read live. It is a
label for the client: the blob route never answers a blob by it.

A kept file is a blob of the session, addressed by its SHA-256
([[014-store]]): a file linked again unchanged is stored once.

### The event

`files.kept` and its position are those of
[[055-images-an-answer-shows]]. A linked file is one more entry of
`files`, with the members of an image's entry:

```json
{"type": "files.kept", "payload": {
  "message": "evt_01J9Z3Q4W8KX6T0M2V5N7R1B3C",
  "files": [{"path": "arxiv-papers.zip", "resolved": "/work/arxiv-papers.zip",
             "blob": "sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
             "media_type": "application/zip", "size": 11702886, "width": 0, "height": 0}],
  "skipped": [{"path": "arxiv-papers", "reason": "not_a_file"}]}}
```

| Member | For a linked file |
|---|---|
| `path` | the destination as the message wrote it, the `sandbox:` prefix included, so a client matches the link it draws |
| `resolved` | the absolute path read, inside the working directory |
| `media_type` | by the file's name, then by its leading bytes, as above |
| `size` | the file's bytes |
| `width`, `height` | 0, whatever the file's bytes are; only a file the message shows as an image carries a size |

An image's entry is unchanged. A client tells the two apart by the
reference it draws, or by `width`, which is positive for every image.

| `reason` | Meaning |
|---|---|
| `not_a_file` | a link to something that is no regular file, such as a directory; new in this spec |
| `not_an_image` | an image reference whose file is no image, as before; a link is never skipped for its type |
| `too_large` | a linked file past `MaxKeptFileBytes`, or an image past its own bounds |
| `limit` | a link past `MaxKeptFiles`, a linked file past what `MaxKeptFilesBytes` leaves its message, or an image past `MaxImages` |
| `not_found`, `outside_workdir`, `no_machine`, `unavailable` | as for an image |

### How the bytes move

The harness hands a linked file to the store as it reads it from the
machine and holds none of it but the leading bytes that type it. No
store takes a blob as a stream: the directory store, the Postgres
store, an object store behind either, and the client of a runner in
its own process each hold a blob whole while they write it, and a
database store holds it once more as the statement's parameter. One
linked file therefore costs the process that keeps it about twice its
size while it is kept, files are kept one at a time, and a process that
drives several sessions pays for each of them at once. The bounds are
set by that cost: at 32 MiB a file, a server that drives four sessions
in one process spends at most about 256 MiB on four files kept in the
same moment, which a process limited to 1 GiB affords. A file that grew
past its bound between its `Stat` and its read fails the store's read,
and nothing of it is stored.

A read of a blob holds it whole as well, and a fork reads every blob
its copied events name before it writes them
([[017-external-runners-handoff-fork]]), so the bytes a session keeps
are what its fork holds at once. The bound of a message keeps that near
what its images alone could already reach.

The keep's reads and stores run past a canceled step under one bound,
the time the most bytes a message keeps take at 1 MiB a second: the
bytes of `MaxImages` images and `MaxKeptFilesBytes` together, 104
seconds. The bound is computed from those constants.

A runner in its own process stores a blob through the runner protocol
([[016-runners]]), whose blob route takes a body as large as
`MaxKeptFileBytes`; every other route of the protocol keeps its bound.

### The blob route

Unchanged. `GET /v1/sessions/{id}/blobs/{digest}` answers by the blob's
leading bytes: one of the four image formats inline as an image, and
any other blob, a PDF, an archive or an HTML page a message linked
among them, as an `application/octet-stream` attachment with `nosniff`
and `Content-Security-Policy: sandbox; default-src 'none'`. No type is
added to what the route answers inline, so a file an agent wrote never
opens as a document of the API's origin. A client offers the download
under the name and the type its `files.kept` entry gives.

### The files route

`GET /v1/sessions/{id}/files` answers a file of at most
`MaxKeptFileBytes`, in place of 10 MiB. The route copies a file from
the machine to the answer as it reads it and holds none of it but its
leading bytes, so the bound costs it no memory, and one bound means a
file too large to read live is one no `files.kept` keeps either.

### Redaction

As for an image: `files.kept` is redactable, a redaction of an
`agent.message` redacts the `files.kept` that names it in the same
append, and a blob no other event names is deleted. The redaction reads
the digests an event names and nothing of their type, so it holds for a
kept file of any type.

### Decisions

| Choice | Picked | Weighed against |
|---|---|---|
| What names a file | the answer's own link, read when the step is committed | keeping every file a step writes: a command's writes are not in the log by name, and most are not for the person; a tool the agent calls to hand a file over: one more call an agent forgets |
| Which links | every local link, of any type, under bounds | a list of document types: it goes stale, and no type is unsafe to keep, since the blob route answers every one that is no image as a download |
| The media type | the name's extension, then the leading bytes, the files route's rule | the bytes alone: a client would label a kept file other than the same file read live |
| A link to an image file | a linked file, with `width` and `height` 0, unless the message also shows it | the image's rules: a large picture linked for download would be refused at the 5 MiB an image is kept at |
| The count | 16, apart from the images | one count for both: an answer with a few pictures and a list of files would lose files to its pictures |
| The bytes | 32 MiB a file and 64 MiB a message | larger: every store holds a blob whole, and a fork holds a session's blobs together; smaller: an archive of a handful of papers is past 10 MiB |
| The `sandbox:` prefix | read as a path | refused as an unknown scheme: the file the model meant would not be kept, for a prefix that names nothing else |
| A step with no open machine | keeps nothing and opens nothing | opening the machine to keep a file: the session spends a machine's time on a read ([[044-reading-a-sessions-files]]) |

## Not in this spec

How a client draws a linked file, and what it shows for a skipped one.
A store that takes a blob as a stream, which would lift what a kept
file costs in memory, and a fork that copies blobs one at a time. A
bound on the bytes one session keeps in all. A link to a directory kept
as an archive of it. Files on the web that an answer links by URL: a
client's to fetch, and not the session's.

## Roll order

One release. Every runner has known `files.kept` since the release
that added it ([[055-images-an-answer-shows]]), and this spec adds no
event type and no member: an entry of a linked file and the reason
`not_a_file` are new values. A client that draws only the references it
knows, an image by its `path`, meets entries it matches to no image and
skips them.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A step whose `agent.message` links files by a Markdown link, an `a` element, a reference-style link and the `sandbox:` prefix appends `files.kept` with each file's blob, its type by the configured rule's reading of its name and leading bytes, its size, and `width` and `height` 0, after the step's tool results and before the turn's status | `harness.TestAnAnswersLinkedFilesAreKept`, `runner.TestLinkedFilesAreKeptBeforeTheTurnCloses` | built |
| The media type is the files route's rule, one function: the name's extension, then the leading bytes | `runner.TestMediaTypeReadsTheNameThenTheBytes`, `runner.TestLinkedFilesAreKeptBeforeTheTurnCloses` | built |
| A link in a code span or a fenced block, an autolink, and a destination of another scheme keep nothing and record nothing; a destination's escapes, references, query and fragment are read as an image's are; a `sandbox:` prefix names a path only before a destination with no scheme | `harness.TestLinksAreReadAsCommonMarkReadsThem`, `harness.TestOnlyImageReferencesAreRead` | built |
| A destination both linked and shown as an image is kept once, as the image, and one that is no image is skipped `not_an_image` and not kept as a file | `harness.TestAFileShownAndLinkedIsKeptOnceAsTheImage` | built |
| A path outside the working directory, with the `sandbox:` prefix or without, a symbolic link out of it, a missing file, a directory, a file past `MaxKeptFileBytes` and a file the credential deny-list names are each skipped with their reason, and nothing of them is stored | `harness.TestKeptFileRefusals` | built |
| A message linking more files than `MaxKeptFiles` keeps the first ones and skips the rest `limit`, counted apart from its images; a file past what `MaxKeptFilesBytes` leaves the message is skipped `limit` and a smaller one after it is kept; a turn with no open machine skips a link `no_machine` | `harness.TestKeptFilesAreBounded` | built |
| A linked file is read from the machine only as the store reads it | `harness.TestALinkedFileIsHandedToTheStoreAsItIsRead` | built |
| A file that grew past a bound since its `Stat`, a read, a close or a store that fails, and a file gone before its read are skipped with their reason and not stored | `harness.TestAFileThatGrewOrFailedIsNotStored` | built |
| The keep's one bound is the time a message's most bytes take at its planned rate | `harness.TestTheKeepIsBoundedByItsBytes` | built |
| A step in a turn with no open machine keeps no linked file and opens no machine | `runner.TestKeepingALinkedFileOpensNoMachine` | built |
| A Cella machine's linked file is read through the sandbox's file routes, inside the workspace only, and a directory is `not_a_file` | `machine/cella.TestKeepReadsALinkedFileFromTheWorkspace` (stub Cella) | built |
| The blob route answers a kept PDF, archive and HTML page as an `application/octet-stream` attachment with `nosniff` and the sandboxing policy; redacting its `files.kept` removes the file, and redacting the `agent.message` redacts its `files.kept` in the same append and removes the file | `internal/server.TestAKeptFileIsADownloadRedactedWithItsAnswer`, `session/storetest`'s `RedactAnAnswerWithItsImages` for each store | built |
| The files route answers a file of `MaxKeptFileBytes` whole and refuses one byte more | `internal/server.TestAFileAtTheBoundIsAnswered`, `internal/server.TestAFileReadIsRefused` | built |
| A runner in its own process stores a blob of `MaxKeptFileBytes` through the runner protocol, and one byte more is refused | `internal/runnerrole.TestARunnerStoresAKeptFile` | built |
| The OpenAPI document states a linked file's entry, `not_a_file`, the `sandbox:` prefix, the bounds and the files route's bound | `internal/server.TestTheDocumentStatesKeptImages`, `internal/server.TestOpenAPIIsGenerated` | built |

## Outcome

Built on 2026-10-11, in no release yet, and not yet run against a
model gateway or a Cella installation; the Cella machine is proven
against the stub. Every criterion has its test. What shipped differs
from the first draft in these points:

- **The harness does not type a file itself.** The rule's second half
  reads a type from bytes with the standard library's content sniffer,
  which lives in a package that dials, and the harness tree reaches
  none ([[001-architecture]]). The rule is `runner.MediaType`, which
  the files route calls and a drive hands its harness as
  `Config.MediaType`; a harness given none types every linked file
  `application/octet-stream`.
- **No store takes a stream**, so the bounds are the ones the cost of
  holding a file whole allows, as the design above states. The harness
  still hands the file over as it reads it, which saves the one copy a
  buffer of its own would add.
- **The exported keep is `harness.KeepFiles`**, in place of
  `KeepImages`, with the media type rule as its last argument.
- **An image reference to something that is no regular file**, a named
  pipe as well as a directory, is `not_an_image`, as a link to one is
  `not_a_file`, so a keep never opens a file a read would wait on.
- **The keep's bound moved** from 30 seconds for a message's images to
  the 104 seconds its images and linked files take at 1 MiB a second.
