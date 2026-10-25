Sends a message to a subagent you started with `agent` and returns its answer. The subagent goes on from what it already did, so the message needs only what is new: more work, a correction, or a question about its answer.

Inputs:
- `to`: the subagent's id, which the result of the `agent` call named.
- `message`: the message.
- `end`: optional. `true` ends the subagent after this answer, and it takes no more messages.
