---
title: "Hosted checkpoints at the git host: a hosted session keeps its checkpoints at its repository, and a fork restores its files after the sandbox is gone"
status: complete
track: core
depends_on: [004-session-log.md, 009-machines.md]
affects: [runner/checkpoint/, runner/, session/, harness/, cmd/toposd/, internal/config/, internal/server/, api/, docs/]
effort: medium
created: 2026-10-01
updated: 2026-10-01
author: changkun
---

# Hosted checkpoints at the git host

## Overview

A hosted session runs its tools in a Cella sandbox, and its checkpoints
([[034-checkpoints-and-rewind]]) are commits in the repository of the
sandbox's working directory. The sandbox is deleted at the session's
end, and the checkpoints with it, so a fork of an ended hosted session
([[017-external-runners-handoff-fork]]) has the conversation and the
session's repositories but none of the files the session changed. This
spec keeps a hosted session's checkpoints past its sandbox, at the
repository on the git host the session works in, and has a fork restore
the fork point's files from there when its own sandbox opens.

## Current state

Every turn end takes a checkpoint (`runner/checkpoint`), in the working
directory's repository or in a session repository under the runner's
checkpoint directory. A fork restores the fork point's checkpoint when
its machine's repository or the parent's session repository holds the
commit (`runner/fork.go`). A hosted runner sets no checkpoint
directory, so a hosted session takes checkpoints only when its working
directory is a checkout, which it is when the session names a
repository, and they live in the sandbox alone. Spec 034 named the git
host as the place a cloud checkpoint goes, pushed by the runner with its
own token from a bundle the sandbox's helper writes; that push is not
built, and toposd's image carries no `git` to push with.

## Design

### Where a hosted checkpoint lives

When the session's working directory is a checkout of the session's
first repository, and that repository is on the installation's git host
(its URL under `TOPOS_ORIGO_URL`), the runner keeps the session's
checkpoints at that repository under one ref:

| Ref at the git host | Points at |
|---|---|
| `refs/topos/checkpoints/<session>/latest` | the commit of the session's latest checkpoint |

One ref per session is enough. The checkpoints of the session's own
working directory form one chain: each turn's checkpoint has the
previous turn's as its first parent, and a rewind does not break it,
since the turn after a rewind chains to the turn before it. Every
earlier turn's checkpoint is therefore reachable from the latest, and a
fork at any turn boundary finds its commit there. The per-turn refs stay
in the sandbox's repository; `latest` is not a number, so it never
clashes with them. A thread's worktree chain is not kept: a fork
restores the session's own working directory.

A session with no repository, one whose first repository is on another
host, and every session of a runner without `TOPOS_ORIGO_URL` keep their
checkpoints in the machine alone, as before. A session's further
repositories, cloned into directories of their own inside the working
directory, are in its checkpoint only as the commits they were at, as
git records a repository inside another, so their uncommitted files are
not kept and a fork has them at the refs the session names.

### Pushing it

At the end of every turn of the session's own thread, once the
checkpoint is committed and before the `session.status` that ends the
turn is appended, the runner pushes it from the machine:

```
git push --quiet --no-verify -o origo.event=off <repository URL> +<commit>:refs/topos/checkpoints/<session>/latest
```

| Part | Why |
|---|---|
| run in the sandbox | it presents the session's git host credential through the egress gateway, as the agent's own pushes and the delivery's clone do ([[019-git]]); no credential reaches a command line or the log |
| the repository's URL, not a remote name | the agent may change its checkout's remotes; the checkpoint goes to the repository the session names |
| forced | the session's log, not the ref, records which commit each turn kept; a chain that starts again, as after a lost sandbox, moves the ref to it |
| `-o origo.event=off` | the git host records no push event for it (Origo spec 008), so a checkpoint fires no trigger subscribed to the repository's pushes ([[022-triggers]]); a git host that takes no push options gets the same push without it |
| `GIT_TERMINAL_PROMPT=0` | a machine with no credential for the host fails at once instead of waiting for one |
| at most 15 minutes | the bound of a repository's delivery, since the turn's end waits for it |

When the push succeeds, the checkpoint the `session.status` carries
names the repository that holds it: `{"ref", "commit", "remote"}`, where
`remote` is the repository's URL as the session names it
([[004-session-log]]). A push that fails leaves `remote` out and is no
error of the turn: the session may read the repository and not write
it, the git host may refuse the size, or the network may fail. The
checkpoint is still taken, rewind still works while the sandbox lasts,
and a later fork says what it could not restore.

This replaces two lines of the drafted design: spec 034's "in the cloud
the runner pushes, never the sandbox", from a bundle with the runner's
own token, and spec 019's rule that the sandbox creates no checkpoint
ref. toposd's image has no `git`, and the sandbox's token reaches every
ref of the repository anyway until the git host decides pushes by ref.
What holds instead: a workload cannot make a fork restore any files but
the ones its checkpoint recorded, since a fork restores the commit its
copied log names, by id, or nothing. A workload that moves or deletes
the ref can at most deny its own session's forks the files.

### What a fork restores, and when

A fork's first machine opens when its first tool acts on it, as every
hosted session's does ([[009-machines]]); a fork that only talks fetches
nothing. When it opens, the runner delivers the session's repositories,
the fork's copies of its parent's, and then restores the checkpoint the
`session.status` at the fork point names:

1. When the working directory's repository or the parent's session
   repository holds the commit, it is taken from there, as on the host.
2. Otherwise, when the checkpoint names a `remote` and that remote is
   the fork's own first repository, the runner fetches the commit by its
   id from that repository: `git fetch --no-tags <repository URL>
   <commit>`. A git host that serves an object by its id, as protocol
   v2 does, gives it; from one that does not, the runner fetches the
   parent's `latest` ref and has the commit when it is reachable from
   there. A remote that is not the fork's own repository is never
   fetched from: the copied log is input, not authority, and does not
   point the sandbox at another repository.
3. The commit becomes the fork's checkpoint of that turn, its files are
   checked out over the delivered checkout, and the machine is recorded
   with `session.machine` reason `restored` and the checkpoint. The
   fork's next checkpoint chains to it, and the fork keeps its own
   `latest`.

A file the checkout holds and the checkpoint lacks is left in place: a
checkpoint leaves out ignored, deny-listed and oversized files that the
checkout may track, so a file missing from it is no deletion. The
fork's branch starts at the ref the session names, not at the parent's
branch ([[017-external-runners-handoff-fork]]), so the restored files
show as changes on it.

### When it is missing or too large

The fork point's checkpoint is missing for the fork when the parent
kept it in its sandbox alone (no `remote`), when the fetch fails, and
when the commit is not where the checkpoint says. The fork still
starts: it has the conversation and its repositories, its machine is
recorded with reason `attached`, and beside it the runner appends a
`session.error` with code `checkpoint_missing`, not retryable, whose
message names the turn and the reason. The call that opened the machine
runs as it would have.

A file over 100 MiB is never in a checkpoint
([[034-checkpoints-and-rewind]]). The git host's own limits bound the
rest, such as Origo's per-push limit and the repository's quota: a push
over them is refused, that turn's checkpoint has no `remote`, and a fork
at that turn says `checkpoint_missing`. The core sets no limit of its
own on a pushed checkpoint.

A fork point with no checkpoint at all, as in a hosted session with no
repository, restores nothing and appends nothing: there were no files
kept.

### Authorization

The git host decides both directions. On an installation whose git host
asks an authorizer, that authorizer decides with the scope it put on
each session's token at its create ([[018-credentials-and-secrets]]):

| Who | Needs | Decided as |
|---|---|---|
| the session, pushing its checkpoint | `repo.write` on the repository in the session's scope | any push of the session's; a session whose initiator may only read the repository pushes none, and its forks say `checkpoint_missing` |
| a fork, fetching it | `repo.read` on the repository in the fork's own scope | any fetch of the fork's; the fork is decided as a create of the forker with the parent's repositories, so its scope reads the repository only when the forker may, and the fork is allowed only to a caller who may read the parent (`session.read`, then `session.fork`) |

No decision of the authorizer changes. What does change is who can read
a session's working files after it ends: the git host authorizes per
repository, not per ref, so anyone who may read the repository may list
`refs/topos/checkpoints/` and fetch what they point at, as they may
fetch the session's branch. A session that works in a public repository
publishes each turn's working files, uncommitted ones included, without
its ignored and deny-listed ones. Narrowing that to the people who may
also read the session takes the git host hiding `refs/topos/` from its
ref advertisement while it still serves a commit by its id, which only
the session's log names; that is the git host's change and not built.

### Cost and retention

| Cost | Size |
|---|---|
| refs | one per session that works in a repository on the git host; Origo holds up to 100 000 refs per repository |
| objects | what the session changed, compressed and shared with the repository's own history, counted against the repository's quota at the git host |
| transfer | one push per turn of what changed since the previous checkpoint, and one fetch per fork that opens a machine |
| latency | the push is part of each turn's end |

The ref stays at the git host until the repository is deleted or
someone who may write it deletes the ref (`git push <repository URL>
--delete refs/topos/checkpoints/<session>/latest`). Deleting the session
does not delete it: no credential of the session outlives the session,
and the server holds none of its own for the git host. An installation
that wants a deleted session's checkpoint gone prunes
`refs/topos/checkpoints/<session>/` itself; the core builds no such
pruning. Spec 034's rule that checkpoints go with the session holds for
checkpoints on a host and in a sandbox, not for these.

### What the API states

The fork route's description in the OpenAPI document says that a
hosted session working in a repository on the git host keeps its
checkpoints there and that a fork restores the fork point's files from
there, so a client can tell a core that restores a hosted fork's files
from one that does not, and promise the files only where they are
restored.

### Error codes

| Code | Meaning |
|---|---|
| `checkpoint_missing` | on a fork's first machine: the fork point's checkpoint could not be had, so the fork starts from its repositories; the message names the turn and why |

## Not in this spec

The checkpoints of a hosted session with no repository, which spec 034
keeps in a session repository at the git host; restoring a lost sandbox
from the git host ([[034-checkpoints-and-rewind]]); handoff
([[017-external-runners-handoff-fork]]); the git host hiding
`refs/topos/` and deciding pushes by ref ([[019-git]]); pruning the refs
of deleted sessions; telling the model which files a fork could not
restore.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A checkpoint taken in a checkout that keeps its checkpoints at a repository is pushed there under the session's `latest` ref, with `origo.event=off` where the repository takes push options and without where it does not, and records the repository as `remote`; a thread's checkpoint is not pushed; a fresh clone fetches a kept checkpoint by id, or through `latest` over protocol v0, adopts it and restores its files | `runner/checkpoint.TestACheckpointIsKeptAtItsRepository`, `runner/checkpoint.TestAKeptCheckpointIsFetchedByID` | built |
| A push the repository refuses, or one to a repository that is not there, leaves `remote` out and the checkpoint taken with its ref on the machine; a commit the repository does not have is `ErrNotKept` with git's account | `runner/checkpoint.TestAPushTheRepositoryRefusesKeepsTheCheckpointLocal`, `runner/checkpoint.TestAKeptCheckpointIsFetchedByID` | built |
| A fork whose parent's machine is gone restores the fork point's files from the repository into a fresh clone, records `restored`, chains its next checkpoint to it and keeps its own `latest` | `runner.TestAForkRestoresFromTheRepository` | built |
| A fork whose checkpoint was kept on the parent's machine alone, whose copied checkpoint names another repository than its own, or whose repository lost the checkpoint, is `attached` with a `session.error` `checkpoint_missing` beside it that names the commit and why, and its turn runs; on a machine opened on demand, the call that opened it runs and answers | `runner.TestAForkWithoutItsKeptCheckpointSaysSo`, `internal/hosted.TestAHostedSessionKeepsItsCheckpointsAtTheGitHost` | built |
| Over the stub Cella and git's own http backend, a hosted session's checkpoint reaches the git host with the sandbox's placeholder credential, and a fork restores the file into its own sandbox at its first call | `internal/hosted.TestAHostedSessionKeepsItsCheckpointsAtTheGitHost` | built |
| Through toposd: a hosted session that works in a repository on the git host writes a file it never commits and ends, its sandbox is deleted, and the session `POST /v1/sessions/{id}/fork` starts, sent a message, reads the file at its first call in a sandbox of its own, recorded `restored`, and keeps its own checkpoint chained to it | `cmd/toposd.TestAContinuedHostedSessionHasItsFiles` | built |
| The fork route's description states that a hosted fork's files are restored from the git host | `internal/server.TestTheForkRouteStatesWhatItRestores` | built |
| A fork's bash starts in its own working directory, not in the directory its copied log reported on the parent's machine | `harness.TestAForkDoesNotStartBashInItsParentsDirectory` | built |

## Outcome

Built on 2026-10-01 as designed: `runner/checkpoint` pushes and
fetches (`Checkpointer.Remote`, `KeptRef`, `Fetch`), the runner keeps a
session's checkpoints at its first repository when that is under
`runner.Options.CheckpointHost`, which toposd sets from
`TOPOS_ORIGO_URL`, and a fork restores from there (`runner/fork.go`).
`session.CheckpointRef` carries `remote`, and the fork route's
description carries the sentence a client reads, `server.ForkKeptFiles`.

Divergences and additions:

- Any fork point's checkpoint the runner cannot restore is
  `checkpoint_missing` beside the machine and stops nothing, a failed
  checkout and a store read included. Before, a restore that failed
  after the commit was found answered the call that opened the machine
  with `checkpoint_missing` as an open error, and a checkpoint not found
  was recorded nowhere.
- Not specified and fixed: a fork's first `bash` call started in the
  directory its copied log's last call ended in, a directory of the
  parent's machine, which on a host or a Cella driver with per-sandbox
  paths is another directory or none. The harness now drops a bash
  directory that a copied result reported (`turn.toolState`).
- The core reports a failed push only by the missing `remote`; a session
  that may read and not write its repository would otherwise carry an
  error at every turn's end.

Not built, each owned elsewhere: the Cella tier's run against a real
Cella and git host ([[017-external-runners-handoff-fork]]'s
`TestCloudForkRestoresFiles`); the git host hiding `refs/topos/`;
pruning a deleted session's ref; the checkpoints of a hosted session
with no repository ([[034-checkpoints-and-rewind]]).
