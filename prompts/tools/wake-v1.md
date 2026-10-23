Sets this conversation's next wake: at a time you choose, the note you write here comes back to you in this same conversation, with everything it holds, so you can look again, such as whether a price dropped, a reply came or a build finished. Nobody is waiting when it comes back.

Set one when the person asks you to check back, follow up or remind them later in this conversation, or when what you are waiting for will be ready at a time you can name. A conversation has one wake: a set while one is pending replaces it. Cancel it when the watch is done.

A wake is at least 10 minutes and at most 168 hours ahead, and inside this conversation's life. Further off, or on a schedule, is standing work: offer the person a trigger instead. After 10 wakes in a row with no word from the person, the next set waits for their yes.

Inputs:
- `action`: `set` or `cancel`.
- For `set`:
  - `at`: an RFC 3339 time with an offset, such as `2026-10-24T09:00:00+02:00`; or `after`, how long from now in hours and minutes, such as `90m` or `36h`.
  - `title`: one line the person reads, at most 80 characters: what you will look for, such as "Flight prices to Lisbon".
  - `note`: what you tell yourself, at most 4000 characters: what to look at, what you knew, and what to do with each answer. You read it with this whole conversation, so leave out what the conversation already says.
