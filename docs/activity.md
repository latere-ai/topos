# A day's activity: who used the agents, from which application

An installation that serves several applications over one Topos wants to
know on which days people used the agents and from which application,
without reading what they said. Topos records the application on every
message and counts a day on request. This page is the contract a client
follows; `api/openapi.yaml` states the same route under
`getSessionActivity`.

## The client on a sender

Every sender the server records for a request carries `client`, the
OAuth client the request's token was issued to: the token's `client_id`
claim, or its `azp` when it has no `client_id`. The server writes it from
the token it verified, never from the request's body, so an application
cannot claim to be another. A token that names no client writes none,
and a trigger's message has none.

```json
{"sender": {"subject": "https://login.example|alice", "kind": "person", "client": "web"}}
```

A conversation started in one application and continued in another keeps
each message's own client.

## Asking for a day

```http
GET /v1/sessions/activity?day=2026-10-20
```

| Parameter | |
|---|---|
| `day` | the day counted, `YYYY-MM-DD`, from 00:00 to 24:00 UTC, required; from 2000-01-01 up to today, which answers what the log holds so far |
| `sender` | one rendered subject, to read that sender's counts alone |
| `limit` | how many senders a page holds, 1 to 500; 50 when absent |
| `cursor` | the `next_cursor` of the page before |

Each item is one sender, in subject order:

```json
{
  "items": [
    {
      "sender": "https://login.example|alice",
      "messages": [{"client": "desk", "count": 1}, {"client": "web", "count": 3}],
      "sessions": [{"client": "web", "count": 1}],
      "answers": 3
    }
  ]
}
```

| Count | What it counts |
|---|---|
| `messages` | the messages a person sent that day on a session's own thread, by the client that sent each; a trigger's message, a subagent's and one later redacted are not counted |
| `sessions` | the sessions a person created that day, by the client that created each; a session a trigger started is not counted, and a fork is one its caller created |
| `answers` | the turns of the day that ended normally on a session's own thread, each counted for the person whose message the turn answered; a turn that stopped on an error, a limit, an interrupt, a question or an approval is not counted |

A rollout's runs count for no one. A sender with none of the three that
day is not on the page.

## Who may ask

The route asks `session.activity` once, about the collection, with
`day`, and `sender` when the request names one. An installation's
authorizer decides who may read it. Without one, the admins of
`TOPOS_ADMIN_SUBJECTS` may read every sender's counts, and anyone may
read their own by naming themselves as `sender`.
