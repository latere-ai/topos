---
title: "Files a person brings into a message: a file of the person's own files named by source and read at the send with the sender's own token, a short text file the model reads in its request without a machine, and Word, Excel and PowerPoint documents the read tool reads as text"
status: testing
track: core
depends_on: [008-tools.md, 015-api.md, 056-editing-a-message-and-the-branches-of-a-session.md, 061-a-messages-shape-and-the-route-an-allow-names.md, 063-a-messages-opening-for-an-authorizer-that-routes-by-it.md, 068-a-sessions-files-in-a-files-store.md]
affects: [session/, harness/, harness/tools/, prompts/, internal/server/, internal/filestore/, cmd/toposd/, test/stubs/arcastub/, api/openapi.yaml]
effort: medium
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# Files a person brings into a message

## Overview

A person who asks an agent about a file today sends the file's bytes in
the message, base64, at most 5 MiB, and the model reads only a line that
names the file's path, type and size. Three things follow.

1. **A file the person already keeps is sent again.** The person's own
   files are at the files API a session's folder is at (spec 068), yet a
   client has to download a file and upload it into the message. A large
   file does not fit the message's body at all.
2. **A short text file costs a machine.** A message with a 2 KiB CSV
   and the question "what is the total?" starts the session's machine so
   that the model can call `read`, although the whole file would fit in
   the request.
3. **An office document reads as binary.** `read` returns pictures as
   images and text as text, and refuses a Word, Excel or PowerPoint file
   with a hint to use bash, where the sandbox may hold no tool that reads
   one.

This spec adds a third form of a message's file, `source`, which names a
file of the person's own files that the core reads at the send; puts a
short text file's text into the model's request after the line that
names it; and has `read` return the text of Word, Excel and PowerPoint
documents and their OpenDocument counterparts.

## Current state

- A `user.message` takes inline images in `content` and files in
  `attachments` (`internal/server/attachments.go`): each `{name,
  media_type, data}`, or, in a fork's message, `{name, blob}`, a file a
  message of the forked session attached (spec 056). `checkAttachments`
  holds a message to `MaxAttachments` files of at most
  `MaxAttachmentBytes`, each a name of one path segment, no two alike.
- The files are stored once the authorizer allows the send, before the
  event is appended (`storeAttachments`): in the session's folder of a
  files store at `attachments/<event id>/<name>` with the session's own
  token, or as blobs of the session where it keeps no folder (spec 068).
  The record is `{name, media_type, size, blob, path, ref}`.
- The send's question carries the message's shape, `attachments` the
  count of its files and images (spec 061, `shapeOf`), and its opening
  (spec 063).
- The fold renders a message's files as one line each,
  `transcript/attachments-v1` (`session/fold.go`). The runner writes the
  files into the working directory when the machine opens
  (`harness/attachments.go`).
- `read` (`harness/tools/read.go`) answers a file whose first 8000 bytes
  hold a NUL with `results/read/binary-v1`.

## Design

### 1. A file of the person's own files on a message

**The form.** A file of `attachments` is one of three:

| Form | Where |
|---|---|
| `{"name", "media_type"?, "data"}` | a send and a fork's message |
| `{"name", "blob"}` | a fork's message |
| `{"name", "source": {"owner", "path", "version"?}}` | a send and a fork's message |

A file names exactly one of `data`, `blob` and `source`, and a `source`
names no `media_type`. `owner` is the files API's owner segment, `me` or
an owner's id; `path` is the file's path in that space as the files API
reads it, its plane included (`files/Reports/q3.csv`), the path of
`GET {files API}/files/{owner}/{path}`; `version`, when present, a
version of the file from 1 upward, read as `?version=N`. An owner that
is empty, longer than `MaxAttachmentName` bytes, or holds a control or
a bidirectional character, a path that is empty, longer than
`MaxSourcePath` bytes, not clean (a leading `/`, an empty, `.` or `..`
segment), not UTF-8 or holds a control or a bidirectional character, and
a version below 1, are `invalid_request`. A source counts toward
`MaxAttachments` and keeps the name rules of every file.

**The read.** The core reads a source once the authorizer allowed the
send and before the event is appended, at the files API of the session's
folder, the folder's `url`, which names the address the allow gave and
no address of the client's: `GET {url}/files/{owner}/{path}` with the
`Authorization` header `Bearer` and the send's own bearer, the token the
request was authenticated with. A fork's message is read at the fork's
folder after the fork's send is allowed, with the fork request's bearer.
The read follows no redirect.

| The files API answers | The message is |
|---|---|
| 200 with at most `MaxSourceBytes` bytes | taken |
| 200 with more | `attachment_too_large` |
| a redirect | `attachment_too_large`: an Arca-compatible files API redirects a file past its inline bound to a presigned address, and a public file at any size |
| 401, 403 or 404 | `attachment_source_not_found`, 404, whichever of the three it was: the answer does not say whether the file exists |
| anything else, or no answer | `files_unavailable` |

`MaxSourceBytes` is `filestore.InlineBytes`, 16 MiB, the bound up to
which the files API answers a read in one response. A session that
keeps no folder refuses a message with a source as `invalid_request`,
since the source is read where its folder is and kept there, and a
server whose files store reads no source refuses it as
`files_unavailable`.

**Kept as a data file is.** The media type is the files API's
`Content-Type`, unless it is empty or `application/octet-stream`, in
which case it is sniffed from the bytes as a data file's is. From there
the file is a data file: written to the session's folder at
`attachments/<event id>/<name>`, recorded with its size, digest, path
and ref. Its record also names `source`, `{owner, path, version}` as the
message named it, and no bytes. A message refused at the read appends
nothing and writes nothing to the folder.

**The authorizer counts it.** A source is one of the message's files:
the send's and the fork's `attachments` count it (spec 061).

**The wire is not the mechanism.** A client names what it attaches,
`source`; how the core is allowed to read it is the core's. The read
could later move to another credential without a client changing a
byte.

### 2. A short text file the model reads in its request

**Marked at the send.** When a message's files are checked, a file is
marked `inline: true` on its record when all of these hold:

- its media type is text-like: `text/*`, `application/json`,
  `application/xml`, `application/yaml` or `application/x-yaml`, or its
  name ends with `.md`, `.txt`, `.csv`, `.tsv`, `.json`, `.yaml`, `.yml`
  or `.xml`, case aside;
- its bytes are UTF-8 and hold no NUL;
- it is at most `InlineTextBytes`, 64 KiB;
- the message's marked files before it and it together are at most
  `InlineMessageTextBytes`, 192 KiB, taken in the message's order, so a
  file that would pass the bound is not marked and a later smaller one
  may be.

A source is marked as a data file is, once it is read. A fork's file
kept by its blob keeps the mark of the record it names, and counts
toward the fork message's bound.

**Read when the request is built.** The text is never in the event or
the session's record. When the harness builds a model request, the fold
renders each marked file's text after the line that names it, a block of
`transcript/attachment-text-v1` that names its path and holds the text
between fences longer than any run of backticks in it. The harness reads
the file where it is kept, as the runner reads it to deliver it: the
session's folder with the folder the runner reaches, or the session's
blobs. A read of more bytes than the record's size, of bytes whose
digest is not the record's, or of bytes that are not UTF-8 is not used.
Each text read is kept by its digest for the life of the harness, so a
long session reads a file once and not at every step; a file that cannot
be read is named by its path alone for the rest of the turn, the turn
runs, and the next turn tries it again.

**Every request alike.** Every request the harness builds from a fold
carries the texts: a step's, a request built again after a compaction, a
compaction's summary request, the advisor's transcript, and a replay
(spec 007), so a replay builds the request the step sent.

### 3. Office documents read as text

`read` reads a file whose name ends with `.docx`, `.xlsx`, `.pptx`,
`.odt`, `.ods` or `.odp` and whose bytes are a zip archive as its text,
with the standard library's `archive/zip` and `encoding/xml`:

| File | Text |
|---|---|
| `.docx` | each paragraph of `word/document.xml` a line, a table row's cells joined by tabs |
| `.xlsx` | each sheet in the workbook's order under a line naming it, each row a line of its cells joined by tabs, a skipped column an empty field; shared strings, inline strings, booleans and numbers resolved |
| `.pptx` | each slide in the presentation's order under a line naming its number, each paragraph a line |
| `.odt`, `.ods`, `.odp` | the same from `content.xml`: paragraphs and headings, a spreadsheet's tables as sheets, a presentation's pages as slides |

The result opens with a line that says what kind of document the file
is and that its text follows without formatting or images, then the
text's lines numbered and bounded as a text file's are (`offset`,
`limit`, the line cut), and its `meta` names the file's own digest. A
document past `ReadMaxDocument` bytes, an archive or a part that does
not read, and a part past `ReadMaxDocumentPart` bytes decompressed are
answered as a binary file is today. A PDF is answered as it is today;
reading a PDF's text needs a library this core does not carry, and
whether to take one is the owner's decision, later.

## Decisions

| Choice | Picked | Weighed against |
|---|---|---|
| The credential of a source's read | the sender's own bearer, within the request | the session's own token and a rule of the installation's authorizer that admits a read the message names (see below) |
| A redirect from the files API | refused, `attachment_too_large` | followed to the presigned address: a file past 16 MiB in the core's memory at a send, and a public file's address would be one of the files API's choosing |
| A 401, 403 and 404 | one code, 404 | each its own: a caller would learn which files of another owner exist |
| Where a text's eligibility is decided | at the send, marked on the record | at each request: the request would depend on what a read answers that day, a log written before this spec would render differently when built again, and a replay could not tell which texts its request held |
| The text in the log | never; read when the request is built | kept in the event: a person's file would be copied into every store that holds the log |
| The harness's credential for the read | the folder the runner reaches, the session's own token | the core's own read token, as the blob route reads with: the harness is a library that may run outside the server, and holds no service credential; a turn runs only while the session's token is good |
| A read failure | the path line alone, the turn runs | a refused turn: the file is still in the working directory once the machine opens |
| An office document's reader | the standard library | a dependency per format, for formatting this text drops anyway |
| A PDF | binary, as today | a PDF library: the owner's decision, later |

**A deviation the owner decides.** The core reads a source with the
sender's own token, inside the request that names it. Nothing is
stored: the token is used for the one read and dropped with the request.
The message is the consent: the person who reads the file is the person
who sends it, at that moment. And a key's narrowing holds: a token
minted from a key that may not read the file is refused by the files
API, not by a rule the core would have to keep in step with it. The
alternative is to read with the session's own token, with a rule of the
installation's authorizer that admits a read of a file a message of the
session names; it keeps every read of the files API to one credential,
the session's, at the price of a rule that must know what a message
named, and a session that could read what its sender may not. This is a
deviation of authorization, and the owner decides it; the wire contract
does not depend on it.

## Compatibility

A client that sends no `source` sees no change. A client that reads
records reads `source` and `inline` when present and may ignore both. A
core before this spec refuses a `source` as an unknown member,
`invalid_request`, and renders no text. A log written before this spec
has no marked file, and its requests build as before.

## Not in this spec

- A source past 16 MiB, through the files API's presigned address.
- A source from any other service than the files API of the session's
  folder.
- A source on a session's create, which takes no files.
- Reading a PDF's text.
- The text of a file an answer links, or of a file in the working
  directory.

## Acceptance criteria

| # | Criterion | Test | State |
|---|---|---|---|
| 1 | A message's source is read from the files API of the session's folder with the send's own bearer, at `?version=N` when it names a version, stored in the folder as a data file is, and recorded with `source`; the media type is the files API's, or sniffed when it is empty or `application/octet-stream` | `internal/server.TestASourceIsReadWithTheSendersToken` | built |
| 2 | A source the files API answers 401, 403 or 404 is `attachment_source_not_found` 404 with one body for the three, a redirect and a body past `MaxSourceBytes` are `attachment_too_large`, any other failure `files_unavailable`; a refused message appends nothing and writes nothing to the folder | `internal/server.TestASourceTheFilesAPIRefuses`, `internal/filestore.TestReadSource` | built |
| 3 | A file names exactly one of `data`, `blob` and `source`; a source beside a `media_type`, with an owner, a path or a version out of its rules, and on a session with no folder is `invalid_request`, and on a server whose files store reads no source `files_unavailable`; sources count toward `MaxAttachments` | `internal/server.TestASourcesRules` | built |
| 4 | A fork's message with a source is read with the fork request's bearer at the fork's folder and kept in the fork's folder; a file kept by its blob keeps its record's source and mark; a refused read leaves no fork | `internal/server.TestAForksMessageReadsASource` | built |
| 5 | The send's and the fork's question count a source in `attachments` | `internal/server.TestASourceCountsAsAnAttachment`, `TestAForksMessageReadsASource` | built |
| 6 | A text-like UTF-8 file of at most 64 KiB is marked `inline`; a larger one, one past the message's 192 KiB, a binary one and one that is not UTF-8 are not; a source is marked once read; a fork's kept file keeps its mark | `session.TestWhatAModelMayReadInline`, `internal/server.TestATextFileIsMarkedInline`, `TestASourceIsReadWithTheSendersToken`, `TestAForksMessageReadsASource` | built |
| 7 | A request carries a marked file's text after the line that names it, fenced, read from the folder or the blobs and read once per harness, with no machine opened; an unmarked file, a read that fails and bytes that are not the record's leave the path line alone, and the next turn tries the failed read again | `session.TestAFoldRendersAMarkedFilesText`, `harness.TestAShortTextFileIsReadInTheRequest`, `harness.TestAFileReadInlineIsTheRecords` | built |
| 8 | A replay builds a request that carried a file's text again and matches | `harness.TestAReplayCarriesAFilesText` | built |
| 9 | `read` returns the text of a `.docx`, `.xlsx`, `.pptx`, `.odt`, `.ods` and `.odp`, with the kind line, numbered and bounded lines, and the file's digest; a malformed one is the binary answer | `harness/tools.TestReadReadsOfficeDocuments`, `TestReadFallsBackOnABrokenDocument`, `TestCellReferencesNameTheirColumn` | built |
| 10 | The API document describes `source`, the record's `source` and `inline`, and `attachment_source_not_found` | `internal/server.TestOpenAPIIsGenerated` | built |
| 11 | End to end on the stubs: toposd reads a message's source with the sender's own bearer, keeps it in the session's folder with the session's own token, and the model's request carries its text with no sandbox created | `cmd/toposd.TestAMessageBringsAFileOfThePersonsFiles`, `test/stubs/arcastub.TestTheStubReadsAVersion` | built |

## Outcome

Built on 2026-10-18 on the branch `brought-files`, in no release yet,
and run against the stub files API, the stub identity provider and the
stub Cella alone, not against an installation's files service. Every
criterion has its test. Where the build differs from the design as first
written, or adds to it:

- **An owner may hold a `/`.** An owner's id can be an issuer-qualified
  subject, which holds one; the owner is sent as one path segment,
  escaped, as the files store sends its own.
- **A Content-Type that is no media type is sniffed** as an empty one
  is.
- **A server whose files store reads no source** refuses one as
  `files_unavailable`: `server.Files.Source`, which toposd sets with
  `filestore.ReadSource` beside its `Write` and `Read`.
- **A file kept by its blob keeps its record's source** beside its mark,
  so a fork's message still names where the bytes came from.
- **The inline rule lives in `session`.** `session.Inlinable`,
  `session.TextLike`, `InlineTextBytes` and `InlineMessageTextBytes` are
  the one statement of the rule the server marks by and the harness
  bounds its read by.
- **The fold takes the texts.** `session.FoldWith` with
  `FoldOptions.Texts`, by digest, keeps the fold pure;
  `session.InlineFiles` names the marked files of the messages a fold
  renders, so the harness reads no file of a message a summary covers or
  another thread holds. Every fold of the harness goes through one
  method, so a step's request, a rebuild after a compaction, an
  estimate, a summary request, the advisor's transcript and a replay
  carry the same texts.
- **The harness keeps at most 16 MiB of texts** for its life; a file
  that cannot be read is logged, named by its path alone, and not read
  again in the turn.
- **`read`'s document text** is held to 8 MiB, an archive to 32 MiB
  (`ReadMaxDocument`) and a part to 64 MiB decompressed
  (`ReadMaxDocumentPart`); a markup compatibility Fallback is skipped so
  a text box is read once, a tab stop of a paragraph's properties is not
  a tab, and an OpenDocument repeat is held to 256. The lines that name
  the document's kind, a sheet and a slide are prompt texts
  (`results/read/document-v1`, `sheet-v1`, `slide-v1`), and the tool's
  description is version 2.
- **A path that is not UTF-8** cannot arrive through JSON, which decodes
  it to replacement characters; the check stays for the rule's sake.

Not done: a source past 16 MiB, through the files API's presigned
address; reading a PDF's text, the owner's decision, later; and the
credential of a source's read, which the owner decides (see Decisions).
