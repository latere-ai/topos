---
title: Architecture
status: current
track: adversarial
updated: 2026-07-08
author: changkun
---

# Architecture

## What Adversarial Review is

Adversarial Review is an **adversarial-review engine and protocol**, a capability
of the Topos runtime. It is not a standalone tool. An embedder gives it a proposer
(the implementation agent, defending a change) and one or more critics (attacking
the change), and it runs a bounded, multi-round debate over the diff and returns a
summary of what survived.

The core idea, from adversarial-debate theory: a change is more trustworthy when
an independent adversary tries and fails to break it than when a single reviewer
nods along. Adversarial Review operationalizes that as a forked, cross-examining
debate whose only human-facing output is the set of unresolved disputes.

## The fork/debate model

```
                  the coding agent's session (the "root")
                            |
                            |  fork per critic (root transcript untouched)
                 +----------+----------+
                v          v           v
            fork-1      fork-2  ...  fork-N
          proposer <==> critic       ...
            clone    rounds
                |
                v  round files + ledger written to disk
     <StateDir>/sessions/<id>/forks/critic-i/rounds/r{1,2,...}-{critic,proposer}.md
                            |
                            v
                     summary.md  (contention-scored headline + open leaves)
```

- Each critic runs in its own fork; the engine never writes into the root session.
- Within a fork, rounds alternate: odd rounds are the critic, even rounds are the
  proposer. One user-facing "turn" is a critic message plus a proposer reply, so it
  is two internal rounds.
- Each critic declares its own attack topic in round 1 and is then locked to it;
  later forks are told which topics are taken so they pick a different angle.
- Attacks carry stable IDs and are tracked in an append-only ledger. The surfaced
  headline is chosen by a pure contention score, with no model judging that layer.

The protocol itself is [Debate protocol](026-protocol.md).

## Component map

| Layer | Package | Role |
|---|---|---|
| Public API | `adversarial` | The embedder contract: `Engine`, `Proposer`, `Critic`, `Verifier`, result types. See [engine-api.md](024-engine-api.md). |
| Backends | `adversarial/{claude,critic}` | Ready-made proposer/critic implementations over the claude CLI and the Topos-native runtime. See [backends.md](023-backends.md). |
| Inputs | `adversarial/input` | Locate the Claude transcript, compute the working-tree diff. See [inputs.md](025-inputs.md). |
| Orchestration | `adversarial/internal/round` | The real round loop, termination detection, signal handling. |
| Protocol | `adversarial/internal/critic`, `adversarial/internal/ledger` | Aspect prompts, attack format + parser, the attack ledger. |
| Agents | `adversarial/internal/agent` | Subprocess drivers for the `claude` and `codex` CLIs. |
| Persistence | `adversarial/internal/state` | Atomic on-disk session layout. See [session-format.md](027-session-format.md). |
| Output | `adversarial/internal/summary`, `adversarial/internal/ansi` | Contention scoring, `summary.md` render, progress styling. |

`adversarial` re-exports the engine over the `adversarial/internal/*` packages; its
types carry no `internal/` dependency so out-of-module callers can satisfy the
interfaces. Only `adversarial/critic` imports the Topos runtime root
(`latere.ai/x/topos`), enforced by a boundary test, so the Topos-native runtime
stays an opt-in backend and the adversarial core stays runtime-agnostic.

## Consumers

Adversarial Review is embedded, not run directly:

- **`latere review`** (latere-cli) is the developer CLI: it forks the real Claude
  Code session as the proposer and routes critics through Lux on the user's Latere
  identity.
- **wallfacer** and the **agents platform** embed the engine inside the Topos
  world, running critics through the governed runtime (model routing via Lux, Cella
  sandboxes, lineage).

The canonical embedding pattern is in [Engine API](024-engine-api.md).

## Non-goals

- **Not a standalone binary.** Adversarial Review is a capability of the Topos
  runtime, imported as a library. The developer CLI lives in latere-cli as
  `latere review`; this tree ships no installable of its own.
- **The proposer is not pluggable onto arbitrary runtimes.** It depends on
  `claude --resume <id> --fork-session` to reconstitute the real coding session;
  see [Backends](023-backends.md) for why. Critics are the pluggable layer.
- **No model judging of the surfacing layer.** Headline selection is a pure
  contention score. Models argue; they do not rank the output.
