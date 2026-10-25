# A trigger that fires at a list of times

A person sometimes wants standing work at times no schedule names:
Sunday at 17:40, Wednesday at 09:00 and the Monday after at noon. A
trigger's `spec.times` lists them, and the trigger fires once at each,
as a one-shot's `spec.at` fires once at its time. This page is the
contract a client follows to write such a trigger, show it and read it
back. Everything here is on the wire of `/v1`.

## Writing it

`spec.times` is a list of 1 to 100 RFC 3339 times with an offset, in
place of `schedule`, `on` and `at`; a trigger sets exactly one of the
four.

```yaml
apiVersion: topos.latere.ai/v1
kind: Trigger
metadata: {name: fares}
spec:
  agent: assistant
  timeZone: Europe/Berlin
  times: ["2026-10-26T17:40:00+01:00", "2026-10-29T09:00:00+01:00", "2026-11-03T12:00:00+01:00"]
  session:
    message: "Look at the fares to Lisbon."
```

The list is kept as written, in its order and with its offsets, and the
trigger fires at its times soonest first. `spec.timeZone` is the zone a
client shows them in; each time's own offset is what fires.

An apply is refused `invalid_manifest`, naming the field, for an empty
list or one of more than 100, for two entries of the same instant
(`spec.times[1]` names the later one), and for a list with no time
after now. A time already past may stay in the list and never fires, so
a client can send back the list it read, with a pause, a resume or any
other change. Once every time has passed, the trigger is gone (below),
and a new list is a new trigger.

`trigger.create` and `trigger.update` carry `times`, each in RFC 3339 in
UTC, in the list's order.

## How each time fires

| | |
|---|---|
| each time | one firing of its own, recorded with that time, and fired once however often the minute loop passes |
| a time fired late | `skipped_late` past `spec.maxAge`, or once the next listed time has come, which then fires |
| a time at the trigger's weekly allowance | `skipped_budget`, tried each minute until the week allows it, its `maxAge` passes, or the next listed time comes |
| a time an installation refused for a limit that resets | recorded `refused`, and tried once more when the limit resets, no later than the next listed time; at that time the next listed time fires in its place |
| a time held while its session waits for a person | sent once the session runs on, as any held firing is; the trigger keeps waiting for its later times meanwhile |
| a manual fire | runs once now, and spends no listed time |

`status.nextFireAt` is the next listed time to fire, or, while one
waits, when it is tried again.

The firing of the latest listed time spends the trigger, as a
one-shot's firing does: whether it started or continued a session, was
skipped, or was refused for good, the server deletes the trigger with
its firings. The firings of earlier times keep it.

## Reading it back

`GET /v1/triggers/runs` (see `docs/trigger-runs.md`) answers such a
trigger with `fires_on: "times"`, its runs as any trigger's, and as
`fires` its listed times still to come before `until`; while a time
waits, its next try comes first.
