Offers the person you work for standing work: an ask you then carry out again, with nobody waiting, in a new conversation each time, on a schedule or when an event arrives. It also lists the triggers the person holds of you, and changes, pauses, resumes or deletes one of them.

Offer one when the person asks for something that repeats, such as "every Monday", "each morning" or "remind me weekly", or for something to happen when an event arrives, such as "when an email comes in" or "whenever someone pushes to main", or when they tell you to keep doing a task. Do the ask now as well when it makes sense to.

A propose, change, resume or delete call waits for the person's answer every time. Only the person who started this conversation answers it, and the trigger is theirs: it runs as them. When they say no, nothing changes. When they answer with words instead, their words are in the call's result: call again from what they said, or leave it. Do not offer the same thing again in this conversation after a no.

A pause runs at once, since it only stops work: pause a trigger when the person asks you to, or when its runs no longer serve them, and tell them what you paused and why.

Before you propose, call list to see what the person already holds, and do not offer a trigger they have: change it instead.

Inputs:
- `action`: `list`, `propose`, `change`, `pause`, `resume` or `delete`.
- `id`: for `change`, `pause`, `resume` and `delete`, the trigger's id from list, `trg_` and the rest.
- For `propose`, and for `change` with only what changes:
  - `name`: the trigger's name, as the person would call it, at most 60 characters: Weekly sign-ups, Morning headlines.
  - `ask`: what each run asks, written as a message from the person to you, complete on its own: each run starts a new conversation that knows nothing of this one.
  - `why`: one sentence, at most 200 characters, that says the schedule or the event back and what the person gets each time: "Every Monday at 9:00, Berlin time, I count last week's sign-ups and tell you what changed." For `pause`, why you pause it.
  - `schedule`: five cron fields of a minute and an hour, every day or on the days of the week listed: `0 9 * * 1` is every Monday at 9:00, `30 7 * * *` every day at 7:30, `0 18 * * 1,3,5` Monday, Wednesday and Friday at 18:00. A schedule runs at most once a day.
  - `timeZone`: with a schedule, the IANA time zone the time is read in, such as `Europe/Berlin`: the person's, from what they said or your context.
  - `on`, in place of `schedule`: the events each run starts on. `{"product": "mail", "verbs": ["received"]}` is every email that arrives at the person's address, `{"product": "webhook", "verbs": ["received"]}` every call of the person's link, and a repository's pushes are its product and verb, such as `{"product": "github", "verbs": ["push"], "resources": ["owner/name"]}`. Each run reads the event that started it.
  - `notify`: `always` to bring every run's result to the person, or `judged` to bring it only when you call notify in the run, so a run that finds nothing new stays quiet. Absent, a schedule is `always` and events are `judged`.

`list` answers at most 50 triggers, newest first.
