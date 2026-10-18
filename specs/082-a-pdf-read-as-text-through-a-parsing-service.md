---
title: "A PDF read as text through a parsing service: read sends a PDF of the session's folder to the parsing service the installation configures, with the session's own token, returns the pages asked as Markdown, keeps the parse for the session, and counts what its model reads cost"
status: in-progress
track: core
depends_on: [007-models.md, 008-tools.md, 018-credentials-and-secrets.md, 047-web-search.md, 068-a-sessions-files-in-a-files-store.md, 073-a-working-directory-kept-in-the-sessions-folder.md, 079-files-a-person-brings-into-a-message.md]
affects: [harness/parsing/, harness/tools/, internal/parser/, internal/filestore/, test/stubs/arcastub/, internal/hosted/, internal/config/, internal/server/, cmd/toposd/, prompts/, docs/]
effort: medium
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# A PDF read as text through a parsing service

## Overview

A person attaches a PDF, a paper, a contract or a scanned invoice, and
asks about it. `read` answers that the file is binary, and the sandbox
may hold no tool that reads a PDF: the model guesses, or gives up.
Spec 079 left the choice of how to read a PDF to the owner, later. The
owner chose a parsing service: a core of the installation that reads a
document's pages, from the text a PDF carries where it has one and with
a model where it has none, and answers them as Markdown.

This spec has `read` send a PDF of the session's folder of a files
store to the parsing service the installation configures,
`TOPOS_PARSING_URL`, with the session's own token, and return the pages
the call asks for as Markdown. The parse is kept for the session, so a
second read of the same bytes reaches no model, and what the service's
model reads cost is priced and counted in the session's spend.

## Current state

- `read` (`harness/tools/read.go`) answers a file whose first 8000 bytes
  hold a NUL with `results/read/binary-v1`, a PDF among them, and reads
  Word, Excel and PowerPoint documents as text (spec 079).
- A person's attachment is written to the session's folder at
  `attachments/<event id>/<name>`, its record naming its `ref`, the same
  path, and `blob`, the digest of its bytes (spec 068), and the runner
  writes it at that path in the working directory. The copy of the
  working directory is kept under `work/<path>`, each file's digest in a
  `work.saved` (spec 073).
- A tool holds no credential: the hosted builder gives a tool the
  services it reaches, with the drive's token source
  (`internal/hosted`), as `web_search` and `publish` are given.
- A `tool.result` carries `cost_usd_micro`, which the session's spend
  counts (spec 047).

## Design

### The service

The parsing service is an HTTP API under one base URL,
`TOPOS_PARSING_URL`, which this spec calls `{parsing}`. Every request
carries `Authorization: Bearer` and the session's own token for the
audience `lectio`, workload `session`, asked of the drive's token source
at each request, so a renewed token reaches the next one and no token
reaches the sandbox. The calls:

| Call | Request | Answer |
|---|---|---|
| submit | `POST {parsing}/parses`, `Prefer: wait=N` | 200 with a parse that ended within the wait, 202 with one that has not |
| get | `GET {parsing}/parses/{id}` | 200 with the parse |
| pages | `GET {parsing}/parses/{id}/pages` | 200 with `{pages: [{number, state, source}]}` |
| page | `GET {parsing}/parses/{id}/pages/{n}` | 200 with `{number, source, model, reused, usage}` |
| document | `GET {parsing}/parses/{id}/document?format=markdown&page_breaks=true&pages=P` | 200 with the pages' Markdown, `text/markdown`; 409 `document_not_ready` before the parse assembled its pages |

A submit's body:

```json
{
  "source": {"url": "<presigned GET of the file>"},
  "origin": {"store": "storage", "path": "<owner, escaped as one segment>/<folder path>/<ref>"},
  "pages": "1-500",
  "allow_failed_pages": 500,
  "deadline": "270s"
}
```

The origin names the file's place in the session's folder: the folder's
owner as the folder record names it, escaped with `url.PathEscape`, the
folder's path, and the ref inside it, as an authorizer that admits a
session's parse of its own folder reads it. The deadline is a whole
number of seconds.

A parse is `{id, state, progress: {pages_total, pages_done,
pages_failed}, usage: {input_tokens, output_tokens}, error: {code,
detail}}`, state `queued`, `running`, `succeeded`, `failed` or
`canceled`. A refusal is the error envelope `{"error": {"code",
"message"}}`.

### Which PDF

A file whose bytes begin with `%PDF-` is a PDF, whatever its name. It is
read through the service when all of these hold:

- the installation sets `TOPOS_PARSING_URL`, mints session tokens, and
  the session keeps a folder of a files store; otherwise `read` answers
  a PDF as it does today, the binary answer alone;
- it is at most `ReadMaxPDF` bytes, `parsing.MaxFileBytes` (256 MiB),
  the parsing service's own bound on one file, which a file a person
  brings is held to as well (spec 081), stated by the machine before any
  byte is read;
- the session's folder holds the same bytes for the file's path: a
  person's attachment whose record names the path relative to the
  working directory, a ref, and the digest of the bytes `read` read, or
  the working directory's copy of that path with that digest. A file
  the agent made since the copy was last saved is not in the folder.

The service fetches the file itself, with no credential: the runner asks
the files API for a presigned GET, `GET {files}/files/{owner}/{folder
path}/{ref}?inline=0` with the session's own files token, which answers
302 for a file of any size, and sends the `Location` without following
it. The URL is asked just before each submit, since it expires within
minutes.

### The read

1. **The parse.** The parse of the bytes is, in order: the one this
   harness kept for their digest; the one a `tool.result` of the thread
   recorded for them, read again with get when it `succeeded`; or a new
   one. A kept parse a get answers 404, or that did not succeed, is
   submitted again. A new parse reads at most `parsing.MaxPages` (500)
   pages, every page of a shorter document; `pages_total` is the
   document's count when below it, and "500 or more" at it. Two reads of
   the same bytes at once share one parse.
2. **The wait.** A submit waits up to 60 seconds with `Prefer: wait`,
   and then the parse is read with get every second, doubling to ten,
   until it ends. One read takes at most `parsing.Timeout`, 5 minutes,
   from the locate to the text; the submit's `deadline` is the time left
   less 30 seconds, so the service stops reading, and stops spending, by
   the time the read gives up, and the 30 seconds remain to price it. A
   page another parse of the same bytes already read is reused by the
   service, so a read submitted again after a timeout pays for the pages
   it had not read alone.
3. **The pages.** `pages` names the pages to return, `1-5,7` or `9-`,
   each range from 1 and closed or open; it defaults to the first
   `parsing.DefaultPages` (5). Pages past the document's end are left
   out, and a selection that names none of them is an error naming the
   document's count. The document call returns the pages' Markdown,
   each page opened by `<!-- page N -->`; a 409 `document_not_ready`
   is asked again on the poll's schedule. At most `parsing.MaxText`
   (8 MiB) of text is read; more is an error that asks for fewer pages.
4. **The answer.** The result opens with
   `results/read/pdf-v1`, which names the document's pages, says it was
   read as text through the parsing service and which pages follow,
   and how many could not be read; then the Markdown as numbered
   lines, bounded as a text file's are (`offset`, `limit`, the line
   cut, the spill); then `results/read/more-pages-v1` when pages follow
   the last shown, naming the next `pages` to ask. Its `meta` is the
   file's path and digest and `parse`: `{id, pages, more, shown}`, and
   `unpriced` when its cost could not be priced.

### Cost

The service charges its model's reads to the session's owner; its parse
names token counts but no model and no cost. The read prices them: when
the parse's `usage` names no tokens, it cost nothing, a PDF read from
its own text layer, or every page reused, and the result carries
`cost_usd_micro` 0. Otherwise the pages call lists the pages, and each
page whose `source` is `reader` is read with the page call; one that is
not `reused` is priced with `models.Cost` at its `model`'s figures, the
embedded catalog's overlaid by those the installation's Lux door serves
for the model, read with the session's own model key. The sum is the
result's `cost_usd_micro`, which the session's spend counts. A model no
source prices leaves the cost unset, `unpriced` on the meta, and a log
line naming the model. A read that reused a kept parse ran none, and
carries no cost. A parse that ended failed is priced as one that
succeeded, so what it read is counted.

### Failures

A read through the service never fails the turn. A file not in the
folder, one past `ReadMaxPDF`, a presign the files API refuses, a token
that cannot be had, a refusal of the service, a parse that ended
`failed` or `canceled`, and a read past `parsing.Timeout` are each the
binary answer and `results/read/pdf-unread-v1`, one sentence that says
the PDF could not be read as text and why, outcome `error`. A parse that
failed or timed out carries what it cost. A call whose context ends is
answered as a file error is.

### Configuration

| Variable | Read by | Default | Meaning |
|---|---|---|---|
| `TOPOS_PARSING_URL` | `serve`, `runner` | unset | the parsing service `read` sends a PDF of the session's folder to, an absolute http or https URL; unset, `read` answers a PDF as a binary file |

The session's token for the service is minted for the audience
`lectio`, which the installation's identity provider and authorizer
must issue and admit: `parse.create` of a parse whose origin is a file
of the session's own folder, and reads of the session's own parses.

## Decisions

| Choice | Picked | Weighed against |
|---|---|---|
| What a parse reads | the whole document, up to 500 pages, once | the pages asked alone: the service names no page count of a parse of a range, and each new range is a new submit and a new wait |
| Where the file comes from | a presigned GET of the session's folder, the folder's own bytes checked against the machine's by digest | an upload of the machine's bytes: a second copy of every document in the service's storage, and a body the size of the file through the runner |
| A kept parse | in memory for the harness's life, and the parse's id on the `tool.result`'s meta for a later drive | the service's own lookup by origin: it filters by path alone, and a meta is how a thread already keeps its state |
| The cost | priced per page at the page's model | the parse's tokens at the default reader's model: an organization's reader pins another model, and the service names none on the parse |
| A failure | the binary answer and a sentence | a failed turn: the file is still in the working directory, and bash may read it |
| `pages` on an installation with no service | not in the schema | always in it: a model offered `pages` would ask for a PDF's pages where none can be read |

## Compatibility

An installation without `TOPOS_PARSING_URL` offers `read` as before,
`tools/read-v2` and its schema. With it, `read` is `tools/read-v3`,
whose schema adds `pages`. A log written before this spec has no
`parse` meta and reads as before. A client that reads a `tool.result`'s
meta may ignore `parse`.

## Not in this spec

- A PDF the folder does not hold: one the agent made or downloaded
  since the working directory's copy was last saved, and any PDF of a
  session that keeps no folder.
- A PDF past 500 pages beyond its first 500, and a document of another
  kind than PDF through the service.
- The figures and images of a page; the service's tables are Markdown.
- The service's price when it reports one; it reports none today.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The page selection reads `1-5,7` and `9-`, refuses a malformed one, clamps to the document, and formats the pages shown | `harness/parsing.TestSelect` | built |
| The client sends each call with the bearer its function answers at that call, `Prefer: wait` on the submit, and reads a parse, a refusal's envelope, a 404, a page and the document's Markdown within its bound | `internal/parser.TestAReadSubmitsWithTheOrigin`, `internal/parser.TestTheClientsFailures` | built |
| A read locates the file in the folder by path and digest, presigns it, submits with the origin, the pages bound and a deadline, polls to the end, and returns the pages' Markdown; a 409 `document_not_ready` is asked again | `internal/parser.TestAReadSubmitsWithTheOrigin` | built |
| A second read of the same bytes, and a read of a parse a meta recorded, run no parse and carry no cost; a kept parse the service answers 404 for is submitted again; two reads at once share one parse | `internal/parser.TestAParseIsKept` | built |
| A parse with no tokens costs 0; one with model reads is priced per page at each page's model, reused pages left out; an unpriced model leaves the cost unset; a model the embedded catalog does not price is priced at the Lux door's figures, read with the session's own model key | `internal/parser.TestTheCostOfAParse`, `internal/hosted.TestAReaderModelIsPricedAtTheDoorsFigures` | built |
| A refusal, a failed parse, a file not in the folder and a timeout are errors the tool answers with the binary answer and the sentence, a failed parse with its cost | `internal/parser.TestAReadsFailures`, `harness/tools.TestReadFallsBackWhenAPDFIsNotParsed` | built |
| `read` with a parser reads a PDF by its leading bytes, numbers its lines after the head line, says which pages follow and which to ask next, records `parse` on its meta, sets the cost, and offers `pages`; without one it answers a PDF as before with `tools/read-v2`; the description states the bounds from their constants | `harness/tools.TestReadReadsAPDFThroughAParser`, `harness/tools.TestReadWithoutAParserIsUnchanged`, `prompts.TestReadParsedDescriptionHoldsTheBounds`, `prompts.TestEveryTextRendersItsCurrentBytes` | built |
| `StateOf` folds a result's `parse` meta by the file's digest | `harness/tools.TestStateKeepsAParse` | built |
| The files store answers a presigned GET of a ref without following it | `internal/filestore.TestPresign` | built |
| The hosted builder gives `read` a parser when the URL is set, the session keeps a folder and the installation mints tokens, with the session's token for `lectio`; the file's place is read from the log | `internal/hosted.TestReadParsesAPDFOfTheFolder`, `internal/hosted.TestTheParsingReaderNeedsItsSettings` | built |
| `TOPOS_PARSING_URL` is checked at start and is in spec 002's table and the configuration page | `internal/config.TestParsingURL`, `internal/config.TestConfigurationTableMatchesTheSpec`, `internal/config.TestConfigurationPageNamesEveryRead` | built |
| The API document names `ReadResultMeta` | `internal/server.TestOpenAPIIsGenerated` | built |
| End to end on a server over the stub identity provider, authorizer, files API, Cella and Lux, with a stub parsing service: while the identity provider issues no token for `lectio`, a read of an attached PDF is the binary answer and the sentence, and the turn ends as any other; once it does, the service is reached with the session's own token for `lectio`, fetches the file from the folder at the presigned address, and the model reads the pages with the cost on the `tool.result` | `cmd/toposd.TestAPDFIsReadThroughTheParsingService` | built |
| A hosted session in production reads a PDF a person attached through the installation's parsing service | a recorded run | not built: the identity provider and the authorizer do not yet issue and admit the audience `lectio` for a session |

## Outcome

Built on 2026-10-18 on the branch `read-parsed`, in no release yet, and
run against a stub parsing service, the stub files API, the stub Cella
and the stub Lux, not against an installation's parsing service. Where
the build differs from the design as first written, or adds to it:

- **The origin names no version.** The design sent the bytes' digest as
  `origin.version`; the authorizer's rule for a session's parse reads
  the store and the path alone, so the origin is those two. A parse a
  later drive reuses is found by the id its result's meta records.
- **The parse's id, not the service's lookup, carries a parse across
  drives**: `tools.State.Parses`, folded from each `parse` meta by the
  file's digest, gives a later read the id it reads again.
- **A failed read is logged** by the hosted reader with the path, the
  digest, the parse and the service's detail, since the model reads one
  sentence and the operator needs the cause.
- **The pricing reads up to 8 pages' records at once**, and sums each
  model's tokens before pricing them, so a parse is rounded up once per
  model and not once per page.

Not done: a recorded run in production, which waits on the identity
provider issuing the audience `lectio` for a session and the authorizer
admitting `parse.create` of the session's own folder and reads of its
own parses.
