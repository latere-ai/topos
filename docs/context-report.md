# The context report: how full a conversation is

Every request the agent sends a model records how much of the model's
input window it used and what filled it, and the session keeps the
latest. A client shows it as a bar: what the instructions, the tools,
the conversation, the files the person brought and the summary of
earlier history take, and what is free. Everything here is on the wire
of `/v1`.

## On each request

A `model.request` of a step or a compaction carries `context`:

```json
"context": {"window": 200000, "used": 48210, "source": "provider",
            "parts": [{"part": "instructions", "tokens": 6120}, {"part": "tools", "tokens": 4380},
                      {"part": "messages", "tokens": 31050}, {"part": "files", "tokens": 4900},
                      {"part": "summary", "tokens": 1760}, {"part": "free", "tokens": 151790}]}
```

| Field | Meaning |
|---|---|
| `window` | the model's input window, from the installation's catalog; 0 when the catalog names none |
| `used` | the input tokens the request used |
| `source` | `provider` when the model's response counted them, `estimate` when it did not, as for a request that failed or was interrupted |
| `parts` | always in this order: `instructions` (the system prompt), `tools` (the tool definitions), `messages` (the conversation), `files` (the files a person brought that the request names and carries), `summary` (the summary that stands for compacted history), `free` |

The parts are the harness's estimates, scaled so that all but `free`
add up to `used`; `free` is the window less `used`. A request that asks
a model for the session's title records none.

## On the session

The Session's `context_report` is the `context` of the latest request of
the session's own conversation, absent before any. A subagent's requests and a
title's leave it. It is not the Session's `context`, which holds the
text an allow attached at create. The sessions list answers it as it
answers every Session field.
