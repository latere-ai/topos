# Searching sessions: finding a session by what was said in it

`GET /v1/sessions/search` finds the sessions a caller may list by the
words of their conversation: what a person wrote, what the agent
answered, and a person's answers to the agent's questions. Each session
found comes with its newest matches, an excerpt around each with the
matched words marked, and the sequence of the event in the log, so a
client can open the session at that message. This page is the contract
a client follows. Everything here is on the wire of `/v1`, and
`api/openapi.yaml` states the same route under `searchSessions`.

## Asking

```http
GET /v1/sessions/search?q=pricing%20plans&limit=10
```

| Parameter | |
|---|---|
| `q` | the words to search for, required, at most 200 characters |
| `limit` | how many sessions a page holds, 1 to 20; 20 when absent |
| `cursor` | the `next_cursor` of the page before |
| `agent`, `status`, `runner`, `archived` | the filters of `GET /v1/sessions`, with its defaults: archived sessions are left out unless `archived` asks for them |

A `q` that is blank, holds no word (only marks and spaces), or is longer
than 200 characters is `invalid_request`, and so is a `limit` outside 1
to 20.

## What matches

- Each word of `q` is found as the start of a word, whatever its case:
  `pric` finds "Pricing".
- Characters of Han, Hiragana and Katakana typed in a row are found as
  those characters in a row: 北京 finds 我在北京工作, and not 北方的京城.
  Those scripts write no spaces between words, so each character counts
  as a word.
- A message matches when it holds every word of `q`.

What is searched is the text a person sees in the conversation: the text
of a message's blocks and of an answer, on the session's own thread,
read as rendered markdown, so `**bold**` is "bold" and a link is its
label without its address. A message is searched in its first 64 KiB. A
tool's output, the model's thinking, a subagent's thread, a file and a
redacted message are never searched.

## The answer

```json
{
  "items": [
    {
      "session": {"id": "ses_01...", "title": "Plans for the new office", "updated_at": "2026-10-05T09:12:00Z"},
      "matches": [
        {
          "seq": 12,
          "event_id": "evt_01...",
          "type": "agent.message",
          "time": "2026-10-05T09:11:40Z",
          "excerpt": [
            {"text": "…we compared the "},
            {"text": "pricing", "match": true},
            {"text": " of three "},
            {"text": "plans", "match": true},
            {"text": " for a team of twelve…"}
          ]
        }
      ]
    }
  ],
  "next_cursor": "ses_01..."
}
```

- `session` is the session as `GET /v1/sessions` answers it, shortened
  here. Sessions come newest first by id, the list's order, and
  `next_cursor` is present while more follow; a `Link` header carries
  the next page's address too.
- `matches` are the session's 3 newest matching events at most, newest
  first. `seq` is the event's sequence in the log: read the log with
  `GET /v1/sessions/{id}/events` and show the event of that sequence.
- `excerpt` is at most 160 characters of the message around its first
  match, as fragments to join. A fragment with `match` true is a part
  the query matched; show it emphasized. A message cut at its start
  begins with `…`, and one cut at its end ends with it, each in a
  fragment that is no match.

## Who sees what

The search looks only where `GET /v1/sessions` would for the same
caller and filters: the route asks the authorizer `session.list` with
the list's fields. It then asks `session.read` of each session it
found, the question reading its log asks, and leaves out a session the
caller may not read. A page can therefore hold fewer sessions than its
`limit` while `next_cursor` is still present; keep paging until it is
absent.

## Forks and continuations

A fork starts with a copy of its parent's events, at the same
sequences. When a conversation went on in a fork, the fork and the
session it came from both hold its earlier messages and both match. A
client that shows a conversation as its newest session keeps the newest
of the two; the sequence of a match before the fork point names the
same message in both.
