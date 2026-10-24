# Allowances: what a watch may spend in a week

A trigger may name a weekly allowance: the most its runs may cost in any
7 days. The server counts what the runs cost from the session logs,
starts no run while a week's runs have spent the allowance, and starts
them again once the oldest leave the 7 days, with nothing to resume by
hand. It also tells each run how the week is going, so the agent can
look less often before the server has to stop it. This page is the
contract a client follows to set an allowance and show the week.
Everything here is on the wire of `/v1`.

## Setting one

```yaml
apiVersion: topos.latere.ai/v1
kind: Trigger
metadata: {name: morning-headlines}
spec:
  agent: reader
  schedule: "0 7 * * *"
  timeZone: Europe/Berlin
  budget: {perWeek: 500000}
  session: {message: "Summarize this morning's headlines."}
```

`budget.perWeek` is in USD micros, the unit the session's spend is
counted in: `500000` is fifty cents a week. It is above zero; `0` or a
negative number is `invalid_manifest` at `spec.budget.perWeek`. A
trigger without `budget` has no allowance, as before.

When an agent offers standing work, the `trigger` tool's `perWeekUsd`
is the agent's estimate of a week of its runs, in USD, such as `0.40`,
written as `spec.budget.perWeek`. The agent estimates it from its model's
price and a run's likely size, and says it back in `why`. A client shows
the amount in the offer, read from the held call's input. A change that
names `perWeekUsd` replaces the allowance; one that does not keeps it.

## What counts

A run is a turn a trigger's message began: from that `user.message`
through the status that closes the turn, its subagents' requests and any
charged tool calls included. A trigger's week holds:

- every run its own firings began, in the sessions it started and the
  ones it continued;
- every run a wake began in a session it started, so a routine's
  follow-ups count toward the routine.

What a session spends after the person answers it, and on a turn the
person began, is theirs and counts toward no allowance. A wake in a
session a person started counts toward none; the session's own budget
bounds it.

## At the allowance

A firing while the week's spend is at or past the allowance starts
nothing and records the outcome `skipped_budget`, counted in
`status.counts.skippedBudget`. A schedule fires again at its next time,
a redelivered event is decided again, and a one-shot, a wake included,
keeps its time and is tried each minute until the week allows it or its
`maxAge` passes. The fire route answers a skipped firing `200` with the
firing, as it answers every other outcome.

## A one-shot refused until a limit resets

An installation's authorizer may refuse a firing for a limit of its own
that resets, such as a share of the person's credit for work that runs
while they are away. When its deny names when the limit resets, as
`limits.resets_at`, a one-shot, a wake included, is not spent: its firing
is recorded `refused` with the deny's reason, the trigger keeps its time,
and the minute loop tries it once more when the limit resets, judged late
by its own time, so its `maxAge` still bounds the wait. The authorizer is
asked again then, not at every pass. A deny that names no reset spends the
one-shot as before.

`trigger.create` and `trigger.update` carry `budget:
{"per_week_usd_micro": n}`, so an installation's authorizer can refuse a
trigger without an allowance, or one above what the person may spend.

## The week

The server writes the trigger's week at each firing that counts toward
it, before that firing's run:

```json
"spend": {"windowDays": 7, "spentUsdMicro": 320000, "perWeekUsdMicro": 500000,
          "perLookUsdMicro": 80000, "expectedLooks": 3, "projectedUsdMicro": 560000}
```

| Field | Meaning |
|---|---|
| `spentUsdMicro` | what the runs counted toward the trigger cost in the last 7 days |
| `perWeekUsdMicro` | the allowance; absent when the trigger has none |
| `perLookUsdMicro` | the mean cost of the latest five runs that ended; absent before any has |
| `expectedLooks` | the runs expected from that firing, its own included, to the end of the window the oldest counted run opened |
| `projectedUsdMicro` | spent plus a run's cost times the expected runs: what that window is on course to cost |

`status.spend` reads as of the trigger's last firing. A client shows
spent against the allowance, and marks a watch whose projection passes
the allowance as ahead of pace.

The firing's `user.message` carries the same numbers as `spend`, with
`trigger_id` the trigger they count toward, which for a wake is the
trigger that started its session. The agent reads them in one line
before the message, and, when the watch is ahead of pace, is told to
keep the run short, to set its next wake later, or to offer a change
that runs less often, which the person confirms.
