# Cost so far and progress: following a session while it works

A client that shows a session while it works can show what the session
has cost so far and how far the agent is through its to-do list, both
exact from the event stream alone. Read the session once, then follow
its stream: nothing needs reading again after each step. Everything here
is on the wire of `/v1`.

## What a session has cost so far

The Session's `budget` holds its spending in millionths of a US dollar:

```json
"budget": {"max_cost_usd_micro": 5000000, "spent_cost_usd_micro": 15400, "carried_cost_usd_micro": 1200}
```

| Field | Meaning |
|---|---|
| `spent_cost_usd_micro` | what the session's log cost through the session's `last_seq` |
| `carried_cost_usd_micro` | for a fork, the part of `spent_cost_usd_micro` its copied events cost; absent on a session no fork made |
| `max_cost_usd_micro` | the session's cap, when it has one; it holds the session's own spending |

Two event types add to the spending, and each carries what it added as
`cost_usd_micro` in its payload:

- `model.request`: every request to a model, the session's own and each
  subagent's, a compaction's and a title's included. A request the
  installation could not price carries none.
- `tool.result`: a call a service charged for, such as a `web_search` or
  a PDF that `read` took as text through the parsing service. A redacted
  result keeps its cost in its tombstone.

No other event adds to it. To keep the spending current, read the
session, follow the stream from `last_seq + 1`, and add the
`cost_usd_micro` of each `model.request` and `tool.result`:

```
spent = session.budget.spent_cost_usd_micro - session.budget.carried_cost_usd_micro
for each event from session.last_seq + 1:
    if event.type is "model.request" or "tool.result":
        spent += event.payload.cost_usd_micro (0 when absent)
```

Less what a fork carried, as above, the sum is the session's own
spending, the work done since the fork; the next read of the session
reports it as `spent_cost_usd_micro` less `carried_cost_usd_micro`. Add
`carried_cost_usd_micro` back to show what the conversation cost in
all.

## How far the to-do list is

An agent that plans several steps keeps a to-do list with its `todo`
tool. Each call replaces the whole list, and the call's `tool.result`
records the list as it now stands in its `meta`:

```json
"meta": {"todos": [{"id": "1", "content": "Read the listings", "status": "completed"},
                   {"id": "2", "content": "Compare the prices", "status": "in_progress"},
                   {"id": "3", "content": "Write the summary", "status": "pending"}]}
```

`status` is `pending`, `in_progress` or `completed`. The latest `todos`
of a conversation is its list: the session's own, or a subagent's, which
the event's `subagent` names. A call the tool refused records no `todos`
and leaves the list as it was, and an emptied list is `"todos": []`. The
completed items of all of them, "1 of 3", is how far the agent is.

The schema is `TodoResultMeta` in `api/openapi.yaml`.

## An agent that keeps no to-do list

An agent that keeps no list records no count. Show what it is doing
from its latest tool call, and its cost so far as above.
