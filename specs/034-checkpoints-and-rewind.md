---
title: "Checkpoints and rewind: each turn's working directory committed, the session repository, rewind, what fork and handoff restore"
status: drafted
track: core
depends_on: [004-session-log.md, 005-harness-loop.md, 009-machines.md]
affects: [runner/checkpoint/, machine/host/, machine/cella/]
effort: medium
created: 2026-09-27
updated: 2026-10-02
author: changkun
---

# Checkpoints and rewind

## Overview

Isolation and history are two things. A worktree isolates parallel
work ([[009-machines]], [[013-threads-and-subagents]]); a checkpoint
records the history inside one working directory. At the end of every
turn the runner commits the session's working directory, uncommitted
files included, with ignored files and the credential deny-list left
out. When the directory is a checkout of a repository the commit goes
to that repository's refs under `refs/topos/checkpoints/`; otherwise it
goes to a session repository the agent never sees. Rewind restores a
turn's files and records it; a fork restores the files of the turn it
forks at; a handoff moves through the same checkpoints, so it needs no
git remote of the user's. The host is phase 1; the Cella sandbox is
phase 3.

## Current state

v0.7.0 kept no history of a working directory. The earlier design of
this core snapshotted the working tree only for handoff, under a ref
named per worktree; that ref is not carried, and the per-turn
checkpoint below replaces it. Nothing is borrowed.

## Design

### What a checkpoint is

A checkpoint is a git commit of the working directory's tree at the
end of a turn, taken by the runner as its own bookkeeping and never as
an action of the agent's. Its first parent is the previous turn's
checkpoint of the same working directory, so the checkpoints of a
session form a chain. Each working directory has its own chain: the
session's, and each thread's with `isolation: worktree`.

| Working directory | Ref |
|---|---|
| the session's | `refs/topos/checkpoints/<session>/<turn>` |
| a thread's worktree | `refs/topos/checkpoints/<session>/threads/<thread>/<turn>` |

`<session>` is the `ses_` id, `<thread>` the thread's id and `<turn>`
the session's turn number in decimal. The `session.status` that ends
the turn carries the ref and the commit ([[004-session-log]]), and the
checkpoint is durable before that event is appended.

### Taking one

The runner writes the tree with a temporary index, so the person's and
the agent's index, HEAD and branches are untouched:

1. `GIT_INDEX_FILE=<tmp> git add -A -- . <exclusions>`, which honors
   `.gitignore`, with the credential deny-list of [[009-machines]] as
   exclusion pathspecs; a file over 100 MiB is left out and named in
   the commit message's `Topos-Excluded:` trailer.
2. `git write-tree`, then `git commit-tree <tree> -p <previous>`, with
   the message `topos checkpoint <session> turn <n>` and the trailers
   `Topos-Session` and `Topos-Agent` ([[019-git]]).
3. `git update-ref <ref> <commit>`.

A turn that changed nothing reuses the previous tree and still gets
its ref, so every turn has one.

### Where the objects live

| The directory | Objects and refs in |
|---|---|
| a checkout of a repository | that repository; the refs are not reachable from HEAD, a branch or a tag, and git's default fetch and push refspecs carry none of them |
| not a checkout, on the host | a bare session repository, `$TOPOS_DATA_DIR/checkpoints/<session>.git`, used as `GIT_DIR` with the directory as the work tree |
| not a checkout, in a Cella sandbox | a bare session repository at `/topos/checkpoints.git` inside the sandbox, outside the working directory; the runner copies each turn's checkpoint out and pushes it to a repository the git host at `TOPOS_ORIGO_URL` holds for the session's owner and names by the session id |
| a checkout in a Cella sandbox | the checkout's repository; when the checkout is the session's first repository and that repository is on the git host at `TOPOS_ORIGO_URL`, the runner also pushes each turn's checkpoint there, to the one ref `refs/topos/checkpoints/<session>/latest` ([[035-hosted-checkpoints-at-the-git-host]]) |

In the cloud the push to the git host runs in the sandbox, with the
session's git host credential that the egress gateway puts on the
request ([[035-hosted-checkpoints-at-the-git-host]]): toposd's image
carries no `git` to push with, and the sandbox's token reaches every ref
of the repository until the git host decides pushes by ref. A workload
can shape the files a checkpoint records, as it shapes the working
directory, and can move the ref at the git host, but cannot make a fork
restore other files than the checkpoint recorded: a fork restores the
commit its copied log names, by id, or nothing. A cloud
checkpoint restored onto a host runs there under the host sandbox, or
in `plan` or `confirm` where the host has none
([[012-permissions-and-approvals]]). Without `TOPOS_ORIGO_URL` a cloud
session with no repository keeps checkpoints only inside its sandbox
and cannot be handed off ([[002-scaffold-and-configuration]]). A host
without `git` keeps no checkpoint and says so in `session.machine`
([[009-machines]]).

### Rewind

`rewind(session, turn)` restores the working directory to the files of
turn `turn`'s checkpoint. It is allowed only while the session is
idle. The runner takes the lease, takes a checkpoint of the current
state if it differs from the last turn's (so nothing is lost), restores
the target tree into the working directory (files the target lacks are
removed, except ignored files and the deny-list, which rewind never
touches), and appends `session.rewound` with `to_turn`, the restored
`checkpoint`, the `saved` checkpoint of the state it replaced, and
`by`. The conversation is not cut: the fold tells the model the
directory was restored ([[004-session-log]]), and the next turn starts
from those files. Locally it is `topos rewind <session> <turn>`; on a
server `POST /v1/sessions/{id}/rewind` ([[015-api]]).

### What fork and handoff restore

| Operation | Restores |
|---|---|
| fork at a sequence | the checkpoint named by the `session.status` at that sequence ([[017-external-runners-handoff-fork]]), when the new session's machine reaches it: from the working directory's repository, from the old session's repository under the runner's checkpoint directory, or, for a hosted session, by its id from the repository on the git host that kept it ([[035-hosted-checkpoints-at-the-git-host]]); one it reaches nowhere leaves the fork `attached` with `session.error` `checkpoint_missing` |
| handoff | the latest checkpoint, pushed by the writer that hands off and fetched by the one that takes over |
| a lost sandbox | the latest checkpoint ([[009-machines]]) |

Restoring into a fresh directory is `git read-tree` of the checkpoint
into a temporary index and `git checkout-index -a` into the directory;
into a sandbox, the same inside it, or `ImportTar` of the tree when the
image has no `git`.

### Retention

A session's checkpoint refs and its session repository are deleted with
the session. An installation may prune the refs of ended sessions
after a retention it sets; the core's default keeps them until the
session is deleted. A hosted session's `latest` ref at the git host is
the exception: no credential of the session outlives it, so it stays
until the repository is deleted or the ref is deleted
([[035-hosted-checkpoints-at-the-git-host]]).

### Error codes

| Code | Meaning |
|---|---|
| `rewind_not_idle` | rewind asked for while the session runs |
| `checkpoint_missing` | the turn has no checkpoint to restore, or a fork's first machine could not have the fork point's |

## Not in this spec

Worktrees ([[009-machines]]); the ref rules at the git host and commit
attribution ([[019-git]]); the fork and handoff procedures
([[017-external-runners-handoff-fork]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A session with no repository rewinds to an earlier turn and gets that turn's files back, and a second rewind to the saved checkpoint restores the later state | `runner.TestCheckpointsAndRewind`, `runner/checkpoint.TestCheckpointsOutsideARepository`, `internal/toposcli.TestRewindFromTheCommand`; `TestRewindWithoutARepository` for the second rewind | partly built: a second rewind to the saved checkpoint is not tested |
| In a checkout, a session's checkpoints are never visible in the agent's own git history: `git log`, `git log --branches --tags --remotes`, `git branch -a`, `git status` and a default clone and push show none of them | `runner/checkpoint.TestCheckpointsInACheckoutLeaveHeadAndIndexAlone`; `TestCheckpointsInvisibleToGitHistory` for the rest | partly built: no branch or tag holds a checkpoint; `git log`, `git status`, a clone and a push are not tested |
| Taking a checkpoint leaves the index, HEAD and every branch unchanged, and excludes ignored files and every deny-list entry | `runner/checkpoint.TestCheckpointsInACheckoutLeaveHeadAndIndexAlone`, `runner/checkpoint.TestCheckpointsOutsideARepository` | built |
| Every turn end carries a checkpoint ref and commit, including a turn that changed nothing | `harness.TestTheHarnessTakesAndChainsCheckpoints`, `runner.TestCheckpointsAndRewind`, `runner/checkpoint.TestCheckpointsOutsideARepository` | built |
| An isolated thread's worktree has its own chain under `threads/<thread>/` | `TestThreadCheckpointChain` | not built |
| Rewind is refused while the session runs, with `rewind_not_idle` | `runner.TestCheckpointsAndRewind` | built |
| A cloud session with no repository pushes each checkpoint to its session repository, and a new sandbox restores the latest one | `TestCloudCheckpointRestore` in the Cella tier | not built |
| A session whose sandbox was deleted gets a new one restored from its latest checkpoint, recorded as `session.machine` reason `restored` | `TestCellaMachineRestoredFromCheckpoint` | not built |

## Outcome

Shipped in v0.9.0 (2026-09-29): every turn ends with a checkpoint under
`refs/topos/checkpoints/<session>/<turn>`, written through a temporary
index, leaving out credential files and files over 100 MiB, kept in a
session repository under the data directory outside a checkout; `topos
rewind` saves the current state, restores a turn's files and tells the
model. A fork restores its fork point's checkpoint (v0.9.7, 2026-10-01,
[[017-external-runners-handoff-fork]]), and a hosted session in a
private repository keeps its checkpoints at the git host (v0.10.0,
2026-10-02, [[035-hosted-checkpoints-at-the-git-host]]).

Open: the API's rewind route, an isolated thread's own chain, the
session repository of a cloud session with no repository, a deleted
sandbox restored from its checkpoint, and the tests the partly built
rows name.

The status stays `drafted` while [[005-harness-loop]] is open, since the
gate starts no spec before its dependencies close.
