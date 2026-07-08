---
title: Session format
status: current
updated: 2026-07-08
author: changkun
---

# Session format

A debate writes an auditable record to disk under a caller-provided `StateDir`.
The engine invents no default path and writes nothing into the reviewed repo; each
consumer owns its location. latere-cli writes under `$XDG_STATE_HOME/latere/reviews/<repo>/`
(fallback `~/.local/state/latere/reviews/<repo>/`); wallfacer writes under its
server data dir. The layout is a stable, versioned contract: an embedder or a tool
can read it without linking the capability. Written by `adversarial/internal/state`
(atomic) and `adversarial/internal/summary` (the terminal render); a run lives
under `sessions/<id>/` where the id is `<YYYYMMDDTHHMMSSZ>-<rand6>`.

## Layout

```
<StateDir>/
  log.jsonl                      # cross-session run log
  sessions/<id>/
    start.json                   # adversarial.start.v0: proposer/critic refs, task, diff snapshot, config, versions
    end.json                     # termination, stats, headline reference, exit code, summary path
    summary.md                   # contention-scored headline + open leaves + resolved set
    attacks.jsonl                # the attack ledger, one line per transition (adversarial.attack.v0)
    log.jsonl                    # per-session log
    forks/critic-<n>/
      diff                       # the diff this fork reviewed
      stats.json                 # adversarial.fork-stats.v0: topic, rounds, termination, per-role usage
      proposer-state.json        # adversarial.proposer-state.v0: agent, fork_session_id
      rounds/r<k>-critic.md      # each critic round (the attack document)
      rounds/r<k>-proposer.md    # each proposer round (defense + modified-files footer)
```

## The key files

- **`start.json`** (`adversarial.start.v0`) - the run's inputs: proposer and critic
  agent references, the task context, a diff snapshot (range, changed lines,
  files, patch path), the config (fork count, round cap, cost cap, models), and
  engine/Go versions.
- **`end.json`** - the terminal record: termination reason, aggregate stats,
  headline reference, exit-code decision, and the `summary.md` path. Its presence
  is how a reader tells a finished run from a running one.
- **`summary.md`** - the human-facing output: the highest-contention unresolved
  attack, the remaining open leaves, and the resolved set with dispositions.
- **`attacks.jsonl`** - the ledger (schema `adversarial.attack.v0`), one append per
  attack transition; see [Debate protocol](protocol.md). Bodies over 64 KiB spill
  to files and the record keeps a path reference.
- **`forks/critic-<n>/`** - per-fork detail: the reviewed diff, `stats.json`
  (`adversarial.fork-stats.v0`: topic, rounds, termination, per-role token usage),
  the proposer fork-session id, and every round's markdown.

## Write discipline

State files are written atomically: a temp file is written and fsynced, then
renamed over the target and the parent directory is fsynced, so a crash never
leaves a half-written `start.json`/`end.json`/round file. The append-only
`attacks.jsonl` and `log.jsonl` are not fsynced per line, trading a small
durability window for throughput on the hot path. Every record carries a `schema`
field (`adversarial.<kind>.v0`) so the format can evolve without silently
misinterpreting old sessions.
