# A formal model of a session's status

A Lean 4 model of how a session's `session.status` events are written, and
proofs about it. It needs no library beyond Lean's own.

```sh
cd formal
lake build
```

The toolchain is pinned in `lean-toolchain`; [elan](https://github.com/leanprover/elan)
installs it on first build. A build that succeeds has checked every
theorem: none uses `sorry`, and each depends only on Lean's standard
axioms (`#print axioms` lists `propext` and `Quot.sound`).

The gate builds it on every push: `go tool lateregate lean` runs
`lake build --wfail` here, which fails on a `sorry` or any other warning,
and installs elan itself on a machine with no `lake` on `PATH`.

## What it models

`SessionStatus/Diagram.lean` is the status diagram: `idle`, `running` and
`ended` with their stop reasons, which status may follow which, and what
the diagram promises on its own. `ended` is the only status nothing
follows, no status event leaves a session idle without a reason, and in a
log that keeps the diagram an ended session stays ended.

`SessionStatus/Writers.lean` models the writers that append statuses
concurrently to one session's log, and the store that takes their appends:

| Model step | Code |
|---|---|
| `acquire` | `runner.Queue.take` takes the session's lease |
| `check`, `checkEnded` | `Runner.drive` reads the session and refuses an ended one |
| `claim`, `claimRefused` | `Runner.running` appends `running` through `Log.claim` |
| `finishIdle`, `finishEnded` | `harness` `turn.finish` appends the turn's status; `end_on_idle` ends the session |
| `again`, `done` | `Runner.drive` claims again for pending input, or returns |
| `fenced`, `lapse` | a lease that lapses, and the store refusing its holder's fenced append |
| `endRead`, `endAppend`, `endRefused` | `POST /sessions/{id}/end`: its reads, then its append |
| `reap` | the reaper ends an idle session past `expires_at` |

The store checks no status. It takes an append only when the append names
the log's last sequence, and a runner's fenced append only while the
runner holds the lease; a server append names no lease. So a read followed
by an append that names the sequence the read saw is one atomic step, and
a read whose result a later, separate append acts on is two.

The model is parameterized by the checks the claim and the end route make
as their append lands. `shipped` holds the checks before the fix, and
`fixed` the checks after it.

## What it proves

| Theorem | Statement |
|---|---|
| `shipped_claim_after_end` | before the fix, a claim that read an idle session before an end appends `running` after the `ended` |
| `shipped_end_after_claim` | before the fix, an end whose read came before a claim lands on the running session, and the turn's `idle` follows the `ended` |
| `claim_fix_alone_breaks`, `end_fix_alone_breaks` | either change alone leaves one of the two |
| `fixed_keeps_the_diagram` | after the fix, every interleaving of any number of runners, end requests and reaper passes keeps the diagram |
| `fixed_stays_ended` | after the fix, a session that ended stays ended, with the reason it ended for |
| `fixed_turn_then_end`, `fixed_claims_again`, `fixed_hands_off_a_lapsed_lease`, `fixed_ends_on_idle`, `fixed_reaps` | the fixed checks still allow each append the diagram allows, so the safety proof is not vacuous |

## What it does not cover

The model is written by hand from the code; the gate checks that its
proofs hold, and nothing checks that the Go follows it. The two tests that reproduce the races in Go,
`runner.TestAClaimAfterAnEndAppendsNothing` and
`internal/server.TestAnEndThatLosesARaceToAClaimEndsNothing`, tie the
model's counterexamples to the code.

It leaves out events other than `session.status`. They advance the log's
last sequence too, which only makes more appends fail and be retried, so
leaving them out allows more interleavings, never fewer. It also leaves
out which input resumes which stop reason, threads, forks, and an
external runner's handoff. Its store is the Postgres store, the one that
fences a runner's append by the runner's lease; the directory and
in-memory stores fence none.

The claim's check reads only what others appended since the runner's
`Log` last read, so it relies on `Log.last` never passing an `ended` it
did not check. `drive` refuses an ended session before it starts the
`Log`, a claim and a title both refuse after an `ended` among what they
read, and a machine opened on demand appends during a turn, when the
proof shows no `ended` can land.
