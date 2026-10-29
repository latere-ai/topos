# A trigger's reach, and a run per scope

Some standing work needs far less than its agent can do: a nightly pass
over what a person keeps in mind needs their notes and nothing else. A
trigger's `spec.reach` names a kind of work, which the installation's
authorizer maps to what the trigger's runs may reach. Some such work
also divides: once for the person and once for each of their projects,
each in its own session, and not at all on a night with nothing new.
With `split: scope`, each firing asks the authorizer which scopes are
due, starts one session for each, and records a run that cost nothing
when none is. This page is the contract an authorizer and a client
follow. Everything here is on the wire of `/v1`.

## Writing it

```yaml
apiVersion: topos.latere.ai/v1
kind: Trigger
metadata: {name: tidy-memory}
spec:
  agent: assistant
  schedule: "0 3 * * *"
  timeZone: Europe/Berlin
  reach: {kind: memory_tidy, split: scope}
  session:
    message: "Tidy what you remember."
```

| Field | What it holds |
|---|---|
| `reach.kind` | a name of lower-case letters, digits and underscores, a letter first, at most 32: what the authorizer maps; Topos decides nothing from it |
| `reach.split` | `scope`, or absent for one run per firing |

An apply is refused `invalid_manifest`, naming the field, for a kind
outside that shape (`spec.reach.kind`), a split other than `scope`, and
a split trigger that fires on events, at `at` or at `times`, or names
`policy: continue`, `session.target` or `session.key`
(`spec.reach.split`). A split trigger fires on a schedule and starts a
new session per scope.

`trigger.create` and `trigger.update` carry `reach: {kind, split}`,
`split` left out when absent, so the authorizer refuses a kind it does
not know, or one the person may not use, before the trigger is stored.
Every session a firing starts carries `reach` on its `session.create`:
`{kind}` for a trigger with no split, and `{kind, scope, since}` for a
run of one scope.

## Which scopes are due

At each firing of a split trigger, after it is judged late or not, its
message is held to its size and its weekly allowance is read, and before
any session starts, Topos asks `trigger.runs` as the trigger's owner:

```json
{
  "action": "trigger.runs",
  "resource": {
    "kind": "trigger",
    "id": "trg_01M34HGBA0JR98WKKXWZGAX6VA",
    "fields": {
      "owner": "https://login.example.com|alice",
      "name": "tidy-memory",
      "agent": "agent_01J9Z3Q4W8KX6T0M2V5N7R1B3C",
      "reach": {"kind": "memory_tidy", "split": "scope"},
      "firing": {"id": "frg_01M34J2N80QKPCFHVXPHHQRHGQ", "at": "2026-10-29T02:00:00Z", "origin": "schedule"},
      "last": {"you": "2026-10-28T02:00:00Z", "projects/prj_01JA4R": "2026-10-21T02:00:00Z"}
    }
  }
}
```

`firing.origin` is `schedule`, or `manual` for a fire of the trigger by
hand. `last` is the time of each scope's latest run of this trigger that
started a session, among the six weeks of runs Topos keeps; `{}` when
there is none.

The allow's `limits` name the scopes due, in the order their sessions
start, each with the time since which its run reads when the authorizer
names one:

```json
{"runs": [{"scope": "you", "since": "2026-10-28T02:00:00Z"}, {"scope": "projects/prj_01JA4R"}]}
```

| Answer | The firing |
|---|---|
| an allow with no runs, or no limits | `skipped_nothing_new`: no session, no model call, a run that cost nothing |
| an allow with runs | a session per scope, below |
| more than 16 runs, a scope outside `^[A-Za-z0-9][A-Za-z0-9_./:-]{0,127}$`, a scope named twice, or limits that do not read | `refused` with reason `runs_invalid` |
| a deny | `refused` with the deny's reason, and when its limit resets for a deny whose limits name `resets_at` |
| no answer | `failed` |

An authorizer that knows no kind, the owner policy of an installation
with no authorizer among them, allows with no limits, so every firing of
a split trigger records `skipped_nothing_new` there.

## A session per scope

Each scope due starts a session keyed by its scope, its `session.create`
carrying `reach: {"kind", "scope", "since"}` beside `trigger_id`,
`firing_id` and `attended`, `since` left out when the answer named none.

- A scope whose last session is still active is passed over under
  `skipIfActive`, which holds by default, and recorded nowhere; the next
  firing asks again.
- A scope whose start the authorizer refuses is a `refused` run with its
  reason; the other scopes start.
- A scope whose start fails is recorded nowhere; the other scopes stand,
  and the next firing asks again, since that scope's last run did not
  move.
- `maxActive` holds back no scope: the answer, at most 16 runs, bounds a
  firing.

The firing records `started` with its first session; `refused` with the
first refusal's reason when no scope started and one was refused;
`failed` when every start it tried failed; and `skipped_active` when
every scope was passed over.

Every session of one firing counts toward the trigger's weekly allowance
together (see `docs/allowances.md`): each turn a firing's message began,
in any of its sessions, is a look of the trigger.

## The runs

`GET /v1/triggers/runs` (see `docs/trigger-runs.md`) answers each scope's
run of a firing apart, each with its `scope`, its session and what that
session's turn cost:

```json
{"firing_id": "frg_01M34J2N80QKPCFHVXPHHQRHGQ", "scope": "you", "at": "2026-10-29T02:00:00Z", "outcome": "started",
 "session_id": "ses_01M34J2N81KBW8FS0WCBFSJCGZ", "wake": false, "cost_usd_micro": 900, "ended": true}
```

A firing with nothing due is one run of no scope, `skipped_nothing_new`,
ended at a cost of 0. The trigger's `status.counts.skippedNothingNew`
counts those firings.

## What Topos leaves to the installation

What a kind reaches and how a run is held to it, what a scope is and
when one is due, and where a run writes are the authorizer's. A session's
memory folders (`docs/memory-folders.md`) are one way an authorizer gives
a run the files it works in.
