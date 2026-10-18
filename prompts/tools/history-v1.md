Reads back this conversation as the session's log keeps it. Each entry is one event, headed by its number, the time it was written and who wrote it: a person's message with the names of its files, your own message, a call you made with its input, what a call returned, a person's answer to a question, an interrupt, a change of the title.

Your context does not keep everything. An old tool result may be cleared to save context, and the older part of a long conversation replaced by a summary; the log keeps every event as it was written. Use history when you need exactly what your context no longer holds: a value a person gave, the exact text of an error, a page or a file you read earlier. A cleared result names its call's id, and a summary names the numbers of the events it covers. The log also holds what the conversation never showed you, such as the time each message was sent.

Inputs, one of three reads:
- `{"tool_use_id": "toolu_..."}`: one call and its result.
- `{"from": 120, "to": 180}`: the events with those numbers. Either bound may be left out.
- `{"query": "ECONNREFUSED"}`: the events whose text holds the words, case ignored, each with the text around its first match. `from` and `to` narrow it to a range. An answer holds at most 20 matches, each with at most 400 characters of its text.

You read your own conversation, never another's. An answer is bounded in size, and one that stops early ends with the input that reads on. An event removed from the log reads as removed, with nothing of what it held.

Each read is a call: do not read back what your context still holds. Run a tool again only for a result that may have changed since, not to see an old one again.
