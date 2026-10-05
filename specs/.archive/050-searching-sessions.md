---
title: "Searching sessions: one route finds the caller's sessions by the words of their messages and answers, with an excerpt of each match and its place in the log"
status: complete
track: core
depends_on: [004-session-log.md, 006-identity.md, 014-store.md, 015-api.md]
affects: [session/, session/storetest/, internal/store/postgres/, internal/server/, api/openapi.yaml, docs/]
effort: medium
created: 2026-10-05
updated: 2026-10-05
author: changkun
---

# Searching sessions

## Overview

A person keeps many sessions and returns to one by what was said in it:
the plan they asked for last week, the answer that named a library. The
list finds a session by its title alone. This spec adds one route that
searches the sessions a caller may list by the text of their messages
and the agent's answers, answers each session found with its newest
matches, an excerpt around each and the event's sequence, so a client
can open the session at the message, and keeps the same contract on
every store: Postgres through an index, the directory and memory stores
by reading each log.

## Current state

`GET /v1/sessions` filters by agent, status, runner and archived, and
`GET /v1/sessions/summary` counts what it would list ([[015-api]]).
Neither reads a log. A client that wants a session by its content reads
every session's events and searches them itself, one request per
session, through the caller's rate limit. The Postgres store keeps each
payload as text and filters on the header's columns only
([[014-store]]); no column holds a message's words.

## Design

### The route

`GET /v1/sessions/search?q=<words>` takes the list's filters, `agent`,
`status`, `runner` and `archived`, with their defaults and their
refusals, and pages by `limit` and `cursor`.

| Parameter | Reads |
|---|---|
| `q` | the words to search for, required, at most `MaxSearchQuery` (200) runes |
| `limit` | sessions per page, 1 to `MaxSearchResults` (20), `MaxSearchResults` when absent |
| `cursor` | the `next_cursor` of the page before |
| `agent`, `status`, `runner`, `archived` | as `GET /v1/sessions` reads them |

The answer is a page, `items` and `next_cursor`, with the next page's
address in a `Link` header, as every list answers:

```json
{"items": [{"session": {"id": "ses_...", "title": "...", "...": "..."},
  "matches": [{"seq": 12, "event_id": "evt_...", "type": "agent.message", "time": "2026-10-05T09:00:00Z",
    "excerpt": [{"text": "…we compared the "}, {"text": "pricing", "match": true}, {"text": " of three plans…"}]}]}],
 "next_cursor": "ses_..."}
```

- An item is one session, as the list answers it, and the sessions are
  ordered newest first by id, the list's order. The cursor is the last
  session's id, the list's shape.
- `matches` are the session's newest `MaxSearchMatches` (3) matching
  events, newest first by sequence. `seq` is the event's place in the
  log, by which a client shows the message in the conversation.
- `excerpt` is at most `SearchExcerpt` (160) runes of the message around
  its first match, as fragments: `match` is true on each part the query
  matched and absent elsewhere. The window starts a third of its room
  before the first match and moves to fall between two words where one
  is within a few runes. A cut start begins the first fragment with `…`
  and a cut end ends the last with it, each in a fragment that is no
  match. A message in which the search cannot place the match is
  excerpted from its start.

| Refusal | When |
|---|---|
| `invalid_request` | `q` absent or blank; `q` holding no word, only marks and spaces; `q` longer than `MaxSearchQuery` runes; `limit` outside 1 to `MaxSearchResults`, naming the bound; a filter the list refuses |
| `authorizer_unavailable` | a question the authorizer gives no decision on |

### Authorization

The route asks `session.list` first, with the list's fields
([[006-identity]], [[036-organization-owners]]), and looks only where
the list would: the agents of the caller's context, narrowed to the
owners the decision names. Then it asks `session.read` of each session
it found, with the resource every read of a session carries, since an
excerpt is the session's content and `session.read` is the question a
read of its log asks. A session the caller may not read is left out of
the page; a page may then hold fewer sessions than its limit while a
`next_cursor` remains. A read the authorizer cannot decide fails the
request. The route asks nothing new, so an authorizer that answers the
list and the read answers the search, and its allows are cached as
theirs are.

Both questions are asked because they may differ: the list's scope is
where a search may look, and the read is what reveals content. For an
installation whose authorizer reaches the same sessions by both, as one
that lists a person's own sessions in their personal context and every
session of an organization to its owners and admins does, the read
leaves nothing out and costs one cached question per session found.

### What is searched

| Searched | Not searched |
|---|---|
| `user.message`: its text blocks | a tool's output, `tool.result` and `user.tool_result` |
| `agent.message`: its text blocks | the model's thinking blocks, its tool calls |
| `user.answer`: each answer's chosen labels and its words | an event of a subagent's thread, `thread.message` |
| | a redacted event, a file, a blob, an attachment's bytes |

Only events of the session's own thread are searched (`thread` empty),
the conversation a person had. The text is read as a person reads it
rendered: a code fence's line, a rule and a table's rule are dropped; a
heading's, a quote's and a list item's marks at a line's start, `**`,
`__`, `~~` and backticks are removed; a link and an image are their
label, without the address; every run of whitespace is one space. The
plain text is searched in its first `MaxSearchText` bytes (64 KiB), cut
on a rune's start, which keeps a message's index entry well inside the
1 MB a Postgres `tsvector` holds, so no append fails on a long message.
The index and the excerpt read this one text, so what matches is what a
person sees.

### How words match

A text's words are its runs of letters, digits and marks, each
lowercased rune by rune; every character of Han, Hiragana and Katakana,
the prolonged sound mark among them, is a word of its own, since those
scripts write no spaces between words. Everything else separates words.
A query is read the same way: each word outside those scripts is a term
matched as the start of a word, so `pric` finds "pricing"; each run of
those characters typed without a break is one term matched as the same
characters in a row, so 北京 finds 我在北京工作 and not 北方的京城; a
term typed twice is one. A message matches when it holds every term.

Three limits of Postgres's `tsvector` hold on every store, so a store
that scans finds what the index finds: a word of 2047 bytes or more is
not searched, a message is searched in its first 16383 words, and a
phrase is found among each of its characters' first 256 occurrences in
the message.

### The stores

`session.Searcher` is an optional interface of a store, as
`Summarizer` is. `session.Search` uses a store's own where it has one,
and `session.ScanSearch` otherwise, which lists the scope newest first a
page at a time and reads each session's log; the memory and directory
stores search so. A session deleted between its list and its log is
passed over.

The Postgres store keeps each searched event's words in `events.search`,
a `tsvector`, under a GIN index (migration 0007):

| Write | `search` |
|---|---|
| an append | `to_tsvector('simple', <words>)` for a searched event, the words one space apart, so each Han or kana character is a lexeme and two words in a row stand at two positions in a row; NULL for every other event |
| a redaction | NULL, in the statement that writes the tombstone |
| an event written before the column, or by a replica of an earlier release during a rollout | indexed in the background: at the store's open and every ten minutes, reading the rows a partial index of unindexed searched events holds, a batch of 500 at a time; an event with no text is indexed as empty, so it is not read again, and one redacted meanwhile is left without an entry |

A query is a `to_tsquery('simple', ...)` built from the terms: a word
as a prefix, `'w':*`; a run of characters as a phrase, `('北' <-> '京')`;
the terms joined by `&`, each lexeme quoted with its quotes and
backslashes doubled. The search reads the sessions of the scope with an
event whose entry matches, by the same filter condition the list pages
through, then the newest `MaxSearchMatches` matching events of each. A
redacted event is passed over whatever its entry holds, since a replica
of an earlier release redacts without clearing it. Excerpts are built
from the events' payloads by the same function on every store: the
index says which events match, and the text says where.

### Forks

A fork copies its parent's events to the fork point with their
sequences ([[017-external-runners-handoff-fork]]), so a session and the
session it was forked from both hold those messages and both match.
That is the log as it is; a client that shows a conversation as its
newest session folds the two.

### API changes

| Route | Change |
|---|---|
| `GET /v1/sessions/search` | new: the caller's sessions by what was said in them, `session.list` then `session.read` of each found |

## Not in this spec

Ranking by relevance; a search across the sessions of every context a
person belongs to; stemming, synonyms or a language's own dictionary;
searching tool outputs, files or attachments; a match inside a word
other than at its start; a highlight inside the model's thinking.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A text's words, a query's terms, a word's prefix, a phrase of Han and kana characters, every term required, case folded, Postgres's word, position and per-word limits mirrored | `session.TestTokenizeReadsWordsAndSpacelessCharacters`, `session.TestTokenizeKeepsRuneOffsetsAndPostgresLimits`, `session.TestParseQuery`, `session.TestQueryMatches`, `session.TestAPhraseIsFoundAmongAWordsFirstOccurrences` | built |
| Markdown's marks and a link's address are not searched, a message is searched in its first `MaxSearchText` bytes, and only the text blocks of messages and the words of answers on the own thread are read | `session.TestPlainTextDropsMarkdownsMarks`, `session.TestSearchTextReadsWhatAPersonSaw` | built |
| An excerpt is at most `SearchExcerpt` runes around the first match, between words, every match in it marked, an ellipsis at each cut in a fragment of its own, and a text without a placed match excerpted from its start | `session.TestExcerptMarksEveryMatchInAShortText`, `session.TestExcerptCutsAroundTheFirstMatchBetweenWords`, `session.TestExcerptEllipsisStandsApartFromAMatchAtACut`, `session.TestExcerptOfATextWithoutAMatchIsItsStart` | built |
| Every store finds the same sessions and events: prefix, every term, case, a Han phrase, markdown, tool output, a subagent's thread, thinking and a redacted message, the scope's owners, agents, status and archive, the list's order and cursor, `MaxSearchMatches` newest first, `MaxSearchText`; the Postgres store's index answers what the scan answers | `session/storetest` `SearchMatches`, `SearchBounds`, `SearchScope`, run by the memory, directory and Postgres stores | built |
| Postgres indexes at the append, clears an entry at a redaction, indexes what an earlier release wrote and leaves a redacted event unindexed, passes over a redacted event's stale entry, and names the searched types in its backfill and its partial index | `internal/store/postgres.TestSearchIndexesWhatAnEarlierReleaseWrote`, `TestRedactClearsTheEntry`, `TestSearchPassesOverARedactedEventsStaleEntry`, `TestTheBackfillReadsTheSearchedTypes`, `TestTSQueryNamesEveryTerm` | built |
| The route asks `session.list` then `session.read` of each session found, finds the caller's own sessions only, leaves out a session the read denies while the cursor goes on, fails on a read the authorizer cannot decide, pages by its limit, and refuses its bounds naming them | `internal/server.TestASearchFindsTheCallersSessionsByWhatWasSaid`, `TestASearchLeavesOutASessionTheCallerMayNotRead`, `TestASearchPagesByTheLimit`, `TestASearchRefusesWhatItCannotRead`, `TestEveryRouteAsksItsAction`, `TestAuthorizerDownIsRefusal` | built |
| The document's example is what the route answers | `internal/server.TestTheExamplesAreWhatTheRoutesAnswer`, `TestOpenAPIIsGenerated` | built |
| Through toposd on its data directory, a search finds a session by its first message at its sequence | `cmd/toposd.TestServeAnswersTheAPIAsASelfHoster` | built |

## Outcome

Built as designed on 2026-10-05. The Postgres tier, `go test -tags
postgres ./internal/store/postgres/`, ran against Postgres 17 with every
search case passing and none skipped; the conformance cases compare the
index's answer with the scan's for each query they ask.

Two points the design left open were settled. The Postgres store's list
and summary now build their filter condition from the same function the
search does, so the three read one scope; a filter left unset adds no
condition rather than a parameter compared with an empty value. Postgres
splits no Han text into words, so the words of a searched event are
computed in Go and handed to `to_tsvector` one space apart; the
`'simple'` configuration then keeps each character as its own lexeme and
a run of them is a phrase, with no extension and no dictionary of a
language.
