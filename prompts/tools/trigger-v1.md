Offers the person you work for a routine: an ask you then carry out again on a schedule, with nobody waiting, in a new conversation each time. It also lists the triggers the person already holds of you.

Offer one when the person asks for something that repeats, such as "every Monday", "each morning" or "remind me weekly", or when they tell you to keep doing a task on a schedule. Do the ask now as well when it makes sense to.

A propose call waits for the person's answer every time. Only the person who started this conversation answers it, and the routine is theirs once they say yes: it runs as them and they can change, pause or stop it. When they say no, nothing is made. When they answer with words instead, their words are in the call's result: offer again from what they said, or leave it. Do not offer the same routine again in this conversation after a no.

Before you propose, call list to see what the person already holds, and do not offer a routine they have.

Inputs:
- `action`: `list` or `propose`.
- For `propose`:
  - `name`: the routine's name, as the person would call it, at most 60 characters: Weekly sign-ups, Morning headlines.
  - `ask`: what each run asks, written as a message from the person to you, complete on its own: each run starts a new conversation that knows nothing of this one.
  - `why`: one sentence, at most 200 characters, that says the schedule back and what the person gets each time: "Every Monday at 9:00, Berlin time, I count last week's sign-ups and tell you what changed."
  - `schedule`: five cron fields of a minute and an hour, every day or on the days of the week listed: `0 9 * * 1` is every Monday at 9:00, `30 7 * * *` every day at 7:30, `0 18 * * 1,3,5` Monday, Wednesday and Friday at 18:00. A routine runs at most once a day.
  - `timeZone`: the IANA time zone the time is read in, such as `Europe/Berlin`: the person's, from what they said or your context.

`list` answers at most 50 triggers, newest first.
