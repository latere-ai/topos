---
title: "Rollouts: one call runs N unattended sessions of one agent version on one task, with the model and reasoning set at create, the machine named, checks run in each machine before it is released, evidence kept in each session's own folder, every request on Keys the installation's authorizer issues with the rollout's labels, sessions left out of the default list, and one report and one cancel for the whole set"
status: in-progress
track: core
depends_on: [003-manifest.md, 004-session-log.md, 008-tools.md, 009-machines.md, 015-api.md, 025-task-suite.md, 049-reasoning.md, 068-a-sessions-files-in-a-files-store.md, 073-a-working-directory-kept-in-the-sessions-folder.md, 075-a-file-a-session-reads-and-cannot-change.md]
affects: [api/openapi.yaml, internal/server/, session/, runner/, machine/, internal/filestore/, test/tasks/, docs/api.md]
effort: large
created: 2026-10-17
updated: 2026-10-22
author: changkun
---

# Rollouts

## Overview

A rollout runs one agent version on one task N times: N sessions, each
unattended, each in its own machine, each ending when its one turn
ends. It is the unit an evaluation or a training loop asks for, and it
is the caller that open question 4 of the core's plan, an outcome and
grader primitive, was waiting for.

The first caller is [Examen](https://github.com/latere-ai/examen), the
open core for experiments on agents. Its execution spec (Examen spec
005) lists fifteen requirements, R1 to R15, that its `topos` executor
needs; this spec answers each. Examen owns the experiment, the grading
of finished work and the statistics. Topos owns running the sessions,
and with this spec, running a task's checks in the machine the work was
done in.

## Current state

Everything a rollout is made of exists for one session at a time:
agent versions with digests, `attended: false`, `end_on_idle`,
`metadata` (at most 32 pairs, listable by `metadata.<key>=<value>`),
`budget.max_cost_usd_micro`, `limits`, repositories at a commit,
`Idempotency-Key` on every `POST`, the event log and its stream,
`model.request` with usage and cost, `session.model_changed`, per-session
Lux keys from the installation's authorizer, and a session folder in an
Arca-compatible files API (spec 068), which holds `attachments/`,
`results/<turn>/` and, since spec 073, `work/`. A caller can approximate
a rollout today by creating sessions one by one and finding them by
metadata, and Examen's executor does that until this spec is built.

What is missing: a create for N, a parallelism bound, a model set at
create rather than by `PATCH` after it, tool narrowing, a machine named
by the caller, an Arca workspace, input files, a step bound, checks
with the task's references delivered only to them, evidence recorded
with its digests, outputs kept as files and directory archives, a
Budget the rollout names with the rollout's labels on its sessions'
Keys, the Lux request id on each request, the image digest on
`session.machine` (`AttachedMachine` in `session/event.go` names a
kind, an id and an environment, and no image), a rollout id on each
session with the session list leaving them out by default, one report
and one cancel.

## Design

### The resource

`POST /v1/rollouts` creates a rollout; `GET /v1/rollouts/{id}` reports
it and every session (R12); `POST /v1/rollouts/{id}/parallelism`
changes its bound (R2); `POST /v1/rollouts/{id}/cancel` stops it, or
named sessions of it (R13). The four routes ask the authorizer
`rollout.create`, `rollout.read`, `rollout.update` and `rollout.cancel`.
A rollout belongs to the subject that created it, and every question
the authorizer is asked about it or its sessions carries that subject,
as a session's do. `rollout.create`'s resource carries the rollout's
`budget` and `labels`, so an installation's authorizer can hold a
rollout to the experiment it belongs to (platform spec 214).

```
POST /v1/rollouts
Idempotency-Key: <caller key>
{
  "agent": "sha256:...",                 // R1: the version digest
  "n": 20,
  "parallelism": 8,                      // R2
  "model": {"name": "...", "reasoning": "high"},   // R3
  "tools": ["read", "edit", "bash"],     // R4: a subset of the version's
  "machine": {"image": "sha256:...", "resources": {...}, "network": {...}},  // R5
  "message": "the task's instruction",
  "repositories": [{"url": "...", "ref": "<full commit>"}],
  "workspace": {"arca": {"space": "...", "path": "...", "digest": "sha256:..."}},  // R6
  "inputs": {"space": "...", "path": "...",              // R15
             "files": [{"name": "invoice.pdf", "sha256": "..."}]},
  "limits": {"turn_timeout": "30m", "max_age": "35m", "max_steps": 200},  // R7
  "checks": {                            // R8
    "files": {"space": "...", "path": "...", "digest": "sha256:..."},
    "reference": {"space": "...", "path": "...",
                  "files": [{"name": "expected", "sha256": "..."}]},
    "commands": [{"name": "tests", "command": ["./run-tests"], "workdir": ".",
                  "env": {"MODE": "strict"}},
                 {"name": "fields", "command": ["/examen-compare", "json_fields", "{...}"],
                  "image": "sha256:..."}],
    "timeout": "10m",
    "stdout_bytes": 65536
  },
  "evidence": {                          // R9
    "paths": ["report.md"],
    "outputs": ["fields.json", "workspace/"],
    "limits": {"max_files": 10000, "max_bytes": 1073741824, "file_keep_max_bytes": 16777216}
  },
  "budget": "eb-...",                    // R10: the Lux Budget the Keys draw on
  "labels": {"examen.experiment": "exp_...", "examen.cell": "cel_..."},  // R10
  "sessions": [{"index": 0, "trial": "tri_...", "metadata": {...}}, ...]
}
```

### Answers to R1 to R15

| | Requirement | Design |
|---|---|---|
| R1 | One call creates N sessions of one version named by digest, idempotent for the rollout's life, unattended, one turn each, no session waits for a person | `POST /v1/rollouts` as above. The `Idempotency-Key` is held for the rollout's life rather than 24 hours: the same key and body answer the existing rollout, other content is `409 idempotency_conflict`. Each session is created with `attended: false` and `end_on_idle: true`, and carries `rollout`, its rollout's id, on its resource and in its record. A tool call that would wait for `tool_confirmation` is refused at once as a person's deny would be, and the refusal is in the log |
| R2 | Parallelism, raised and lowered while it runs | At most `parallelism` sessions are running; the rest are `pending`, hold no machine, and start in index order. Lowering it lets running sessions finish and starts no new one until the count is below the bound |
| R3 | Model and reasoning set at create, run as named | `model` overrides the version's from the first request and is recorded on each session; the version and its digest are unchanged. An authorizer that would answer another model refuses the rollout with `model_not_allowed`, never substitutes, and no session changes model mid-turn |
| R4 | Tool narrowing | `tools` must be a subset of the version's; a name outside it is `400 unknown_tool`. A tool outside the subset is not offered to the model |
| R5 | The machine | `machine` names the Cella sandbox template by image digest, resources and network policy, in place of the agent's machine. The authorizer may refuse a template; it may not change one |
| R6 | An Arca workspace | `workspace.arca` names a folder and its content digest, copied into the working directory before the first step. A digest mismatch fails the session before the agent starts with `workspace_digest_mismatch` |
| R7 | A step bound | `limits.max_steps` bounds the model responses of the session's own thread. Reaching it ends the turn with stop reason `step_limit`, distinct from `turn_limit`, the wall-clock deadline |
| R8 | Checks in the machine | After the turn ends: every process the turn started is stopped, and the machine's network is set to `none` for the whole check phase, so a check reaches nothing outside the machine; the checks' files (`checks.files`, a pinned Arca folder) are delivered read-only outside the working directory (as spec 075 delivers a read-only attachment); so is each command's `image`, when it names one, mounted read-only with its `command[0]` run from that mount; and so are the task's references (`checks.reference`, an Arca folder whose files each carry their SHA-256), delivered at `/examen/reference/`, which no process of the turn could read; the checks' files and the references are read from the files store with toposd's own server-side identity, acting for the rollout, never with the token of the session's workload, which sits in the machine the agent's code ran in, so an installation's files store authorization grants that read to toposd's identity alone; the session's log is written into the machine as JSONL and the final message as a file; the commands run in order in the working directory the agent left, each with its own `env` and with variables naming where the machine holds the working directory, the log, the final message, the inputs, `/examen/outputs/`, the references, the checks' files and the command's image, under the names Examen spec 006 gives them, without the session's model key in their environment, under `checks.timeout` in total. Each check's exit code, the first `stdout_bytes` of stdout, the tail of stderr, its duration, and whether it started, timed out or was not run are recorded as a `check.ended` event. The machine is released after the last check. This is the core's one check runner: spec 025's suite runs its checks through it (below) |
| R9 | Evidence | The evidence stays in the session's own folder (spec 073); nothing is copied to a folder the caller names. The session writes once, with its own token, which acts as the rollout's owner, under `evidence/` of its folder: the log as JSONL, the final message, the named `paths` as the turn left them, the files under `/examen/outputs/` that `evidence.outputs` names, each entry ending in `/` a directory carried as one tar archive, and one output manifest listing every output file, a directory's files each, with its path, size, SHA-256 and whether it was kept, all within `evidence.limits`: a file over `file_keep_max_bytes` is listed by digest and size only, and past `max_files` or `max_bytes` no more files are kept and the manifest records which limit was passed and the files and bytes the outputs held. Outputs are read before the checks run, as the agent left them. The session also keeps each check's whole stdout and stderr. It records each object's path in its folder, SHA-256 and size in an `evidence.written` event, which the report carries. A failed write fails the session with `evidence_write_failed` |
| R10 | The Budget and the Keys | The rollout names `budget`, the Lux Budget every model request of its sessions and of their machines draws on. The sessions get their Keys from the installation's authorizer, as every session does: each Key lists that Budget and any Budget the installation adds above it, and carries the rollout's `labels` and the session's. An authorizer that will not issue Keys listing the named Budget refuses the rollout. In Latere the Keys also list the wallet, an organization agent's line and the session's own Budget (platform spec 214). The report names each session's Key ids. No session runs on a Key outside that Budget, or on a Key that allows an unpriced model |
| R11 | The request join | Each `model.request` records the `Lux-Request-Id` of the response it read |
| R12 | One report | `GET /v1/rollouts/{id}` answers every session: id, trial and index, status, stop reason, `session.error` code, Key ids, the times its turn and its checks started and ended, check results, evidence objects with their paths, digests and sizes. A `?since=<cursor>` form returns only changed sessions |
| R13 | Cancel | `POST /v1/rollouts/{id}/cancel` interrupts every running turn and ends its session `canceled`, starts no pending one, and stops the rollout's Keys at once. With `{"sessions": [...]}` it stops those alone |
| R14 | The machine on the record | `session.machine` names the image digest the machine ran: `AttachedMachine` gains `image`, the digest, beside `kind`, `id` and `environment`, so a session read later has a pinned environment |
| R15 | Input files | The rollout may name `inputs`, an Arca folder whose files, each with its SHA-256, are read with toposd's own server-side identity, as R8's are, and delivered under `examen/inputs/` of each session's working directory before the first step; a digest that does not match fails the session before the agent starts with `input_digest_mismatch`, a code the report carries, as R6 does for a workspace |

### What stays the same, and the session list

A rollout's sessions are ordinary sessions: their logs, streams, files
and costs read as any session's do, and an operator can inspect one by
id. Forks of a rollout session are allowed and are not part of the
rollout.

They are left out of the session list by default, so a person's list
of conversations is not filled with trials. `GET /v1/sessions` gains a
`rollout` parameter, as `archived` and `trigger` are: absent, it leaves
out every session that carries a rollout id; a rollout's id lists that
rollout's sessions; `any` lists both. A list by `metadata.project` or
any other filter keeps the same default.

### A run is isolated

A run executes a task's instruction, and a task may come from a suite
another person shared. A run's session is created as its initiator, so
without a rule of its own it would be given what that person's own
sessions are given: their standing instructions (spec 053), the context
and the repositories an allow attaches (spec 058), and their connections
(spec 069), with the connection service's token in its sandbox. An
experiment also needs every run to start with nothing but its agent's
definition and its task. So a session that carries a `rollout` reaches
none of its initiator's accounts or context:

- **The create.** Whatever its allow names, the session holds no
  `instructions`, no `context`, no repository the allow attaches and no
  `connections`; the server logs what it left out. Its network, its
  folder (spec 068), where its files are kept for the caller to grade,
  and the repositories its request names, its task's workspace, are its
  allow's and its request's as any session's.
- **A send.** A send's allow changes no connection of it, and appends no
  `session.connections_changed`.
- **The machine.** Its sandbox mounts no Secret of the connection
  service, so it holds no `CONNECTIONS_TOKEN`, and is given no
  connection's address. Its model key and its git host's token are
  mounted as for every session; the installation's authorizer decides
  what the git host's token reaches.
- **The minter.** toposd mints a run no token for the connection
  service's audience, `TOPOS_CONNECTIONS_AUDIENCE`, of either workload,
  refusing it with the setup code `run_isolated`.
- **The prompt.** The harness renders no initiator's instructions, no
  attached context and no connections block for it, and offers it no
  `request_connection`, even from a header written before this rule.

Memory stores (spec 020) are not built; when they are, a run attaches
none. The installation's authorizer holds the same rule on its side: in
Latere it attaches a run nothing of its starter's and refuses its tokens
at the connection service (platform spec 214).

### The task suite and one check runner

Spec 025's task suite and release bar are the core's own evaluation,
and the Examen design moves them onto Examen once its bars exist: the
tasks become an Examen suite and the release check calls an Examen bar.
This spec does not depend on that move.

There is one check runner either way, R8's. Today `tasks.Run`
(`test/tasks/run.go`) drives one session at a time on the test's host,
and `tasks.Evaluate` (`test/tasks/check.go`) evaluates its checks there:
the assertions of a `check.yaml` and a `check.sh` script. Once R8 is
built, `tasks.Run` submits each task as a rollout and becomes R8's
caller: a `check.sh` is an R8 command, and a `check.yaml` is evaluated
by a command R8 runs in the machine, which has the log and the final
message as files.

## Not in this spec

Grading finished work, comparing results and any statistic: Examen's.
Running many copies of an agent program packaged as an image: Cella's
SandboxSet (Cella spec 020).

## Acceptance criteria

- One `POST /v1/rollouts` with `n: 20` creates twenty sessions of the
  named version digest, each carrying the rollout's id, and a repeat
  with the same key and body answers the same rollout after 24 hours.
- With `parallelism: 4`, no more than four sessions run at once,
  pending sessions start in index order, and raising it to 8 starts
  four more without waiting for one to end.
- Every request of every session uses the rollout's model and reasoning;
  an authorizer that answers another model refuses the create.
- A tool outside `tools` is never offered; a name outside the version's
  tools is refused at create.
- A session reaching `max_steps` ends with stop reason `step_limit`.
- A check sees the working directory as the agent left it, has no model
  key in its environment and no network, cannot change the checks' files, runs a
  command that names an `image` from that image's read-only mount, and
  finds each delivered path under its variable; its exit code and
  stdout head are in a `check.ended` event.
- Evidence objects land under `evidence/` of each session's own folder
  with digests and sizes matching their content, the report names each,
  and a failed write fails that session with `evidence_write_failed`.
- A directory output is kept as one archive with the output manifest; a
  file over `file_keep_max_bytes` is in the manifest by digest and size
  alone; past `max_files` or `max_bytes` no more files are kept and the
  manifest names the limit.
- Every model request of a rollout is made with a Key the authorizer
  issued listing the rollout's `budget` and carrying the rollout's
  labels, a rollout whose Budget the authorizer will not list is
  refused, and each `model.request` carries its `Lux-Request-Id`.
- `GET /v1/sessions` leaves rollout sessions out, and `rollout=<id>`
  lists that rollout's.
- Cancel ends every running session `canceled` and starts no pending one.
- `session.machine` of a rollout session names the image digest.
- A rollout naming an input folder delivers each file under `examen/inputs/` before the first step, and a file whose digest does not match fails that session `input_digest_mismatch` before the agent starts.
- A process of the turn cannot read the task's references, which arrive at `/examen/reference/` only after the turn's processes are stopped.
- The checks' files, the references and the input files are read with toposd's server-side identity, and a read of them with a session workload's token is refused by a files store that grants them to toposd alone.
- The task suite's checks run through R8, and no second check runner
  remains.
- A session that carries a rollout holds no instructions, context,
  attached repository or connection whatever its allow names, a send
  changes none of its connections, its sandbox mounts no Secret of the
  connection service, toposd mints it no token for that service, and its
  requests render none of them.

## Outcome

In progress. The session pieces a caller needs to run a rollout as one
session per run are built; the rollout resource is not.

| Requirement | State |
|---|---|
| R1 | Partly met: a create names `rollout`, the caller's id for its run, which the session keeps and `session.create` carries; `attended: false` answers a question at once as unanswered, and a run refuses a call that would wait for a confirmation at once, as a person's deny, so it never goes idle on one. There is no `POST /v1/rollouts`: a caller creates one session per run, under its own `Idempotency-Key`, which holds for 24 hours rather than the rollout's life |
| R3 | Met for a session: a create's `model {name, reasoning}` runs from the first request, and an allow that names another model or level is `model_not_allowed` |
| The session list | Met: the list, the search and the summary leave a rollout's runs out unless `rollout` names it or `any` |
| A run is isolated | Met (v0.51.1): a run takes none of its initiator's instructions, context, attached repositories or connections, its sandbox holds no connection service token, and none is minted for it. Before, a run was given what its initiator's own sessions are |
| R2, R4 to R15 | Not built |

Held by `server.TestARolloutsRunsAreLeftOutOfTheList`,
`server.TestACreateRunsTheModelItNames`,
`harness.TestARolloutsRunRefusesACallThatWouldAsk`, the store
conformance test `ListByRollout`, and for the isolated run
`server.TestARunTakesNoneOfItsInitiatorsAccounts`,
`harness.TestARunIsToldNothingOfItsInitiator`,
`hosted.TestARunsSandboxMountsNoConnectionsSecret` and
`credentials.TestARunIsMintedNoConnectionsToken`.
