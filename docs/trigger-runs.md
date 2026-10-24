# A trigger's runs: what each did, what it cost, and when it fires next

A client that shows a person what their triggers will cost, by the end
of the week or the month, needs what each trigger did and what it will
do. One read answers it for every trigger a person owns: each run since
a time with its cost, the cost of a typical run, and the times it fires
next. The forecast itself is the client's: what a run will cost against
the person's credit is not something Topos knows. Everything here is on
the wire of `/v1`.

## Reading the runs

```http
GET /v1/triggers/runs?since=2026-09-26T10:00:00Z&until=2026-11-09T10:00:00Z
```

answers the triggers `GET /v1/triggers` would answer, paged the same way
by `limit` and `cursor` and narrowed by `owner`, each with its runs:

```json
{
  "items": [
    {
      "trigger_id": "trg_01M34HGBA0JR98WKKXWZGAX6VA",
      "name": "on-release",
      "created_at": "2026-09-22T12:30:00Z",
      "fires_on": "event",
      "suspended": false,
      "max_age": "1h",
      "wake": false,
      "runs": [
        {"firing_id": "frg_01M34J2N80QKPCFHVXPHHQRHGQ", "at": "2026-09-22T12:40:00Z", "outcome": "started",
         "session_id": "ses_01M34J2N81KBW8FS0WCBFSJCGZ", "wake": false, "cost_usd_micro": 1200, "ended": true}
      ],
      "more_runs": false,
      "per_run_usd_micro": 1200,
      "fires": [],
      "more_fires": false
    }
  ]
}
```

| Parameter | When absent | Bounds |
|---|---|---|
| `since` | 28 days ago | from 42 days ago to now: the record keeps six weeks |
| `until` | 7 days from now | from now to 62 days from now |

A time outside its bounds, or one that is no RFC 3339 time, is
`invalid_request`.

## A run

A run is a firing a person would count:

| `outcome` | What happened | `cost_usd_micro` |
|---|---|---|
| `started`, `continued` | the firing started a session or continued one, and the agent worked | what that turn cost, from its message to the status that closed it |
| `skipped_budget` | the trigger's weekly allowance was reached, so nothing ran (see `docs/allowances.md`) | 0 |
| `refused` | the installation's authorizer refused it; `reason` says why | 0 |

`ended` says the turn closed, so its cost is final. A turn still running
answers what it has cost so far. A firing skipped as late or busy, or
still waiting for the person, is not a run.

Which runs a trigger answers:

- a trigger that fires on a schedule or on events answers its own runs,
  and the runs of the wakes an agent set in the sessions it started,
  which count toward its allowance (`"wake": true`);
- a wake answers the wake runs of its session: every wake the agent set
  there, each of which was its own trigger until it fired.

`runs` holds at most 3000, the newest; `more_runs` says older ones were
left out.

## The cost of a typical run

`per_run_usd_micro` is the mean cost of the trigger's latest five runs
that worked and ended, its own firings only (for a wake, its session's
wake runs), from everything the record keeps, whatever `since` is. It is
absent before the first such run, so a client has nothing to estimate
from and says so.

## When it fires next

`fires` are the times the trigger fires after now and before `until`,
soonest first, as the server would fire them:

| `fires_on` | `fires` |
|---|---|
| `schedule` | its schedule's times, in its time zone |
| `at` | its one time; for one past its time that waits, now when it waits for its allowance, or when the limit that refused it resets |
| `event` | none: it fires when something arrives. Read how often it ran from `runs` and how old it is from `created_at` |

A suspended trigger has none. The hold over every trigger
(`docs/holding-triggers.md`) is not applied: the times are what would
fire once it is released. At most 3000 are answered; `more_fires` says
later ones were left out.

`max_age` is the trigger's `spec.maxAge`: how late a firing may still
run after its time, so how long a one-shot that waits may wait before it
is skipped as late.

## A trigger that continues one session

`target` names the one session a wake, or a trigger naming
`session.target`, continues, and `target.trigger_id` the trigger that
started that session, absent for a session a person started. A client
that splits what a person spent themselves from what ran while they were
away reads where each wake's turns belong from it.

## When the trigger was set up

`created_at`, and `status.createdAt` of every trigger the API answers,
is the trigger's first apply. Applying it again, to pause it, resume it
or change it, moves `status.version` and leaves `createdAt`.
