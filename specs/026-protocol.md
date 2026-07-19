---
title: Debate protocol
status: current
track: adversarial
updated: 2026-07-08
author: changkun
---

# Debate protocol

This is the contract every backend follows. It is independent of which model or
runtime backs the proposer and critics: the same markdown attack format, the same
dispositions, the same ledger, the same termination rules. A backend swap changes
who produces the text, never the protocol. Implemented across
`adversarial/internal/critic` (format, aspects, parser),
`adversarial/internal/ledger` (attack ledger), `adversarial/internal/round`
(loop, termination), and `adversarial/internal/summary` (surfacing).

## Roles and rounds

Each critic runs in its own **fork**. Within a fork, rounds alternate:

- **Odd rounds are the critic.** It attacks the diff.
- **Even rounds are the proposer.** It defends, concedes, or fixes.

One user-facing **turn** is a critic message plus a proposer reply, so it is two
internal rounds. Callers set the round cap as `2 x turns`. Forks run serially;
before each fork the orchestrator collects the topics already claimed so the next
fork is steered to a fresh angle.

## Aspects: auto-declare, then lock

A critic picks its own attack topic ("aspect", e.g. security, perf,
internal-consistency, evidence-gap). In round 1 the critic runs in **auto** mode
and declares its aspect on the `aspect:` line. The orchestrator captures that
declaration and switches the fork to **locked** mode, so rounds 3+ stay on the
same topic instead of wandering. There is no fixed catalog; the prompts live in
`adversarial/internal/critic/aspects.go`.

## Critic output format

A critic round is a markdown document:

```
# Critic <i> - round <r> attacks

aspect: <declared-aspect>

## c<i>-<seq> [<path>:<line>]

claim: <what is wrong>

expected violation: <the concrete failure it causes>

reproduction:
```
<commands or steps that trigger it>
```
```

- Attack IDs are `c<critic-index>-<sequence>`, stable for the life of the fork.
- Each attack cites a `path:line` location and states a claim, an expected
  violation, and a reproduction.
- An empty document (header + `aspect:` line, nothing else) is the critic saying
  "nothing new"; the orchestrator reads that as steady state.

## Dispositions

From round 3 on, the critic is replying to the proposer, not writing a fresh
round. The proposer's even-round reply dispositions each prior attack with a line
beginning:

- `concede c<i>-<seq>` - the flaw is real; the proposer fixes it (the changed
  files are attached to the concession).
- `rebut c<i>-<seq>` - the proposer argues the attack is wrong.
- `push-back c<i>-<seq>` - the proposer partially disputes or asks for
  specificity.

The critic then dispositions each of its own prior attacks by reusing the same ID
with a header marker:

- `(re-attack)` - the defense did not fix it; refine the attack to defeat the
  defense specifically.
- `(withdraw)` - the defense convinced the critic, or the concession's fix is
  real; add a one-line `reason:`.
- drop - omit the section entirely (rare, when neither fits).

New flaws found later use a new ID, one past the highest sequence used in the
fork. Reusing an old ID for a new claim is a protocol violation; the mediator
renames it and the ledger connection is lost.

## The attack ledger

Every attack transition is appended to `attacks.jsonl` (schema
`adversarial.attack.v0`, `adversarial/internal/ledger`). A record carries the
attack ID, critic index, aspect, claim,
location, `round_introduced`, `round_last_touched`, `rounds_survived`,
`re_attacked`, `status`, and concession-file references. Statuses:

- `open` - introduced, not yet resolved.
- `conceded` - the proposer accepted and fixed it.
- `rebutted` - the proposer disputed it (still contested).
- `withdrawn` - the critic dropped it after the defense.
- `unresolved` - still `open` or `rebutted` when the run ended.

Bodies larger than 64 KiB spill to files; the ledger keeps a path reference.

## Termination

A fork or run stops on the first of:

- **steady-state** - after at least three critic rounds, the last two produced
  zero new attacks and zero re-attacks (the debate has converged, per fork).
- **max-turn** - a fork reaches its round cap with nothing else firing.
- **cost-cap** - the soft token budget across forks is exceeded; the whole run
  stops.
- **malformed-output** - two consecutive critic rounds are unparseable.
- **interrupted** - the context is cancelled (SIGINT/SIGTERM). Finalize still
  runs so completed rounds persist. Per-call deadlines live in a child context so
  a genuine model timeout is not misread as an interrupt.

## Surfacing

At the end, any attack still `open` or `rebutted` becomes `unresolved`. The
headline is the highest-**contention** unresolved attack, scored purely as
`rounds_survived + (1 if re_attacked)` - no model judges this layer. `summary.md`
renders that headline plus the remaining open leaves and the resolved set. The
score and render live in `adversarial/internal/summary`; the on-disk artifacts are
[Session format](027-session-format.md).
