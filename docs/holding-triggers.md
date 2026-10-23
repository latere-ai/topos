# Holding every trigger at once

A person can pause every trigger they own with one switch, and resume
them with the same: a routine, a watch on events and every wake an agent
set. This page is the contract a client follows to show and flip that
switch. Everything here is on the wire of `/v1`.

## Reading and setting the hold

```http
GET /v1/triggers/hold
```

answers the caller's hold:

```json
{"held": true, "since": "2026-10-23T09:00:00Z"}
```

`since` is absent while nothing is held.

```http
PUT /v1/triggers/hold
{"held": true}
```

holds every trigger the caller owns, and `{"held": false}` releases
them; both answer as `GET` does. A second hold keeps the first `since`.
The hold is the person's across every context their tokens name, as
their triggers are theirs.

## What the hold does

While a person is held:

- no trigger of theirs fires on its schedule, and nothing is recorded:
  its counts stay as they were;
- the fire route answers `conflict` for each of their triggers, as for a
  suspended one, so a delivered event is dropped and not retried;
- a one-shot trigger whose time passes, a wake among them, keeps it;
- an agent's `trigger` and `wake` calls go on as without the hold: a wake
  set while held keeps its time, and the call's result tells the agent
  that every trigger is paused.

On release, a schedule starts from then, so it fires nothing it missed,
and a one-shot fires within the minute, or is `skipped_late` once past its
`maxAge`. A wake that fires late says so to the agent with when it was
due. A trigger keeps its own `suspend` through a hold and its release,
so resuming everything never resumes what the person paused one by one.

## What the authorizer is asked

Both routes ask `trigger.hold` with a resource of kind `trigger` that
names the `owner`, the caller, and, for the `PUT`, `held`. An
authorizer that does not know `trigger.hold` denies it, and the routes
answer `forbidden`; roll the authorizer that answers it first.

No trigger may be named `hold`: the route takes the name, and an apply of
one is refused `invalid_manifest`.
