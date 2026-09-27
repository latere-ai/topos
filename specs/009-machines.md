---
title: "Machines: the Machine interface, the host directory and its worktrees, the Cella sandbox"
status: drafted
track: core
depends_on: [001-architecture.md, 002-scaffold-and-configuration.md, 004-session-log.md]
affects: [machine/, machine/host/, machine/cella/, cmd/topos-machine/]
effort: large
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Machines

## Overview

A machine is where a session's tools act: a directory on the runner's
host, or a Cella sandbox. This spec owns the `Machine` interface, the
host machine (its working directory and roots, `os.Root` confinement,
the credential deny-list, the person's environment, and a git worktree
per session when another session already writes the checkout), and the
Cella machine (creation on the Environment the session names, including
one whose workers run on the developer's own infrastructure, exec with
streaming, the file routes, and a lifetime tied to the session that
survives runner restarts). The host is phase 1; the Cella machine is
phase 3.

## Current state

v0.7.0's `sandbox.Provider` (v0.7.0 spec 001) mapped each sandbox to a
temporary directory, re-rooted absolute paths, and ran commands with a
fixed `PATH` and `HOME=/tmp`; a delegated agent got a fresh empty
sandbox (probe failures 5 and 6 in [[005-harness-loop]]). The
retired laptop client ran commands in a host sandbox that inherited the
whole environment and confined nothing. The Cella provider of v0.7.0
spec 010, rebuilt on the Cella core by v0.7.0 spec 032, is the base of
`machine/cella`: it uses `latere.ai/x/cella/client`, holds the create
until the sandbox runs, sends an explicit egress boundary, and refuses
fields the core does not serve. v0.7.0 spec 011's delivery of secrets
into a sandbox is retired: no credential enters a machine
([[018-credentials-and-secrets]]).

## Design

### The interface

```go
type Machine interface {
	Info() Info // Kind, ID, Workdir, OS, Arch, Environment
	Roots() []string
	SpillDir() string
	Exec(ctx context.Context, r ExecRequest) (ExecResult, error)
	ExecStream(ctx context.Context, r ExecRequest) (ExecStream, error)
	ReadFile(ctx context.Context, path string) (io.ReadCloser, error)
	WriteFile(ctx context.Context, path string, r io.Reader, mode fs.FileMode) error
	Stat(ctx context.Context, path string) (FileInfo, error)
	List(ctx context.Context, path string) ([]FileInfo, error)
	Remove(ctx context.Context, path string) error
	Rename(ctx context.Context, from, to string) error
	Search(ctx context.Context, q SearchRequest) (SearchResult, error) // grep and glob
	Release(ctx context.Context, end bool) error
}
```

`ExecRequest` has `Command` (run by `/bin/sh -c`), `Dir`, `Env`
(additions), `Stdin`, `Timeout` and `Background`. `Search` runs the
Go-native grep and glob of [[008-tools]] where the files are, so both
machines give the same results. `Release(ctx, false)` lets go of the
machine while the session is idle; `Release(ctx, true)` removes it when
the session ends.

### The host machine

| Aspect | Rule |
|---|---|
| working directory | the directory the session was started in, or its `machine.workdir`, resolved with symlinks evaluated; recorded in `session.machine` |
| roots | the working directory, the attached memory store directories ([[020-memory-stores]]), the spill directory `$TOPOS_DATA_DIR/sessions/<id>/spill`, and the extra roots of the agent's `spec.machine.roots` ([[003-manifest]]) |
| confinement | every file tool opens paths through an `os.Root` per root, so `..` and symlinks cannot leave it; a path in no root is refused |
| credential deny-list | the paths below are refused by the file tools inside any root and made unreadable to commands by the host sandbox of [[012-permissions-and-approvals]]; nothing in a manifest or a hook removes an entry |
| environment | commands get the environment of the process that started the runner, the person's own `PATH` and `HOME` included, minus the variables the deny-list names; named secrets appear as placeholders ([[018-credentials-and-secrets]]) |
| processes | each command runs in its own process group, killed on timeout, cancel, and at session end for background jobs |

The credential deny-list:

| Where | Entries |
|---|---|
| the home directory | `.ssh/`, `.aws/`, `.azure/`, `.config/gcloud/`, `.kube/config`, `.docker/config.json`, `.netrc`, `.git-credentials`, `.npmrc`, `.pypirc`, `.gnupg/`, `.password-store/`, `.config/gh/hosts.yml`, `$TOPOS_DATA_DIR/credentials/` |
| any root | `.env` and `.env.*` except `.env.example`, `.env.sample` and `.env.template`; `*.pem`, `*.key`, `*.p12`, `*.pfx`; `id_rsa*`, `id_ecdsa*`, `id_ed25519*` |
| the environment | names ending `_TOKEN`, `_SECRET`, `_PASSWORD`, `_API_KEY` or `_PRIVATE_KEY`; `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`; `TOPOS_MODELS_KEY`, `TOPOS_TOKEN` |

### Worktrees on the host

Two sessions never write the same working directory. When a session
starts in a git checkout, the host machine reads the owner file
`<git dir>/topos/owner` of that checkout's own git directory. If it
names another session that has not ended, the new session gets a
worktree:

1. `git worktree add -b agents/<agent>/<session> <TOPOS_DATA_DIR>/worktrees/<session> HEAD`,
   where `<agent>` is the agent's name and `<session>` the `ses_` id.
2. The session's working directory is the worktree; `session.machine`
   records it and the branch.
3. The worktree's own git directory gets an owner file naming the
   session.

Otherwise the session writes the checkout in place and writes its own
id into the owner file, which is removed when the session ends. A
thread spawned with `isolation: worktree` gets a worktree the same way
on the branch `agents/<agent>/<session>.<thread>`
([[013-threads-and-subagents]]).

At the session's end a worktree is removed with `git worktree remove`
when it has no uncommitted change and its branch is pushed (its
upstream contains its head) or merged (its head is an ancestor of the
branch it was created from). Otherwise it stays until 30 days after
the session ended and is then removed, keeping the branch; its
uncommitted files are in the session's last checkpoint
([[034-checkpoints-and-rewind]]). `topos sessions prune` removes the
rest on demand ([[024-client-cli-skill]]). The host needs `git` 2.30 or
later on `PATH` for worktrees and checkpoints; without it a session
still runs in place, and `session.machine` says checkpoints are off.

### The Cella machine

`machine/cella` creates one sandbox per session through
`latere.ai/x/cella/client` at `TOPOS_CELLA_URL`, with the session's
own credential ([[018-credentials-and-secrets]]); unset, a session
asking for a `cella` machine is refused with `machine_unavailable`.

| Cella Sandbox manifest (`v1beta1`) | From |
|---|---|
| `metadata.name` | `ses-` and the session's ULID in lowercase, so a runner finds the sandbox by name |
| `metadata.labels` | `topos.latere.ai/session`, `topos.latere.ai/agent` |
| `spec.environment` | the session's `machine.environment`, which may name an Environment whose workers the developer runs |
| `spec.image` | the agent's `spec.machine.image`, default Cella's `base` |
| `spec.resources` | the agent's `spec.machine.resources` |
| `spec.network.egress` | `allowlist` with the hosts of the agent's `spec.machine.egress`, the session's repositories' git host ([[019-git]]) and its named secrets' hosts |
| `spec.secrets` | the session's named secrets ([[018-credentials-and-secrets]]) |
| `spec.lifecycle` | `persistent`, `autoStop: 15m`, and a `ttl` of the session's remaining age as a backstop |

The create is held until the sandbox runs (`CreateSandbox` with the
wait option). `bash` runs through `ExecSession`, which streams output
frames and the exit code; short commands use `Exec`. The file tools
use `FileGet`, `FilePut`, `FileStat`, `FileList`, `FileRemove`,
`FileMkdir` and `FileMove`; repository delivery and checkpoint restore
use `ImportTar` and `ExportTar`. At creation the machine uploads the
helper `topos-machine`, a static binary of this module
(`cmd/topos-machine`, built for `linux/amd64` and `linux/arm64` and
embedded in the runner), to `/tmp/topos/bin/`, and `Search` runs it,
so grep and glob need no binary in the image. The spill directory is
`/tmp/topos/spill`, outside the working directory.

Lifetime: the sandbox is the session's. While the session is idle for
15 minutes Cella stops it and keeps its workspace; the next runner that
claims the session finds it by name and starts it. When the session
ends the runner deletes it. When a runner finds the sandbox gone, it
creates one again, restores the latest checkpoint
([[034-checkpoints-and-rewind]]), and appends `session.machine` with
reason `restored`; with no checkpoint to restore it appends
`session.machine` with reason `replaced` and a `session.error`
`machine_lost`, so the model and the person know the files are gone.
The control plane never dials a sandbox or a worker: every call goes to
Cella's API, and a self-hosted Environment's workers connect out to
Cella (invariant 10 of [[001-architecture]]).

### Error codes

| Code | Retryable | Meaning |
|---|---|---|
| `machine_unavailable` | no | the machine kind is not configured, or Cella refused the Environment or the image |
| `machine_lost` | yes | the session's sandbox was gone and no checkpoint could restore its files |

## Not in this spec

The host's operating-system sandbox policy
([[012-permissions-and-approvals]]); checkpoints and restore
([[034-checkpoints-and-rewind]]); what the tools do ([[008-tools]]);
repository delivery and git credentials ([[019-git]]); named secrets
([[018-credentials-and-secrets]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A symlink or `..` path out of the working directory is refused by every file operation of the host, and a path outside every root answers `ErrOutside` | `machine/host.TestFilesAreConfinedToTheRoots`, `machine/host.TestEveryFileOperationRefusesOutsideAndDenied`, `machine/host.TestRemoveAndRename` | built |
| The host's roots are the working directory, the spill directory and the extra roots, each resolved; a missing extra root is refused at open | `machine/host.TestInfoAndRoots`, `machine/host.TestOpenValidates`, `machine/host.TestOpenRefusesAMissingExtraRoot` | built |
| Every deny-list path inside a root is refused by `read`, `write` and `edit` and skipped by search, and the deny-listed variables are absent from a `bash` command's environment while `PATH` is the person's | `machine.TestDenyList`, `machine/host.TestTheDenyListHoldsInsideARoot`, `machine/host.TestExec` | built |
| Commands run in their own process group, time out and cancel, stream their output, and background jobs end with the session | `machine/host.TestExecTimeoutAndCancel`, `machine/host.TestExecStream`, `machine/host.TestBackgroundJobsEndWithTheSession` | built |
| Search is Go-native where the files are: grep honors `.gitignore` at and above the root, has its three output modes, and glob lists newest first | `machine.TestGrepFilesHonorsGitignore`, `machine.TestGrepBelowTheRootHonorsParentIgnores`, `machine.TestGrepModes`, `machine.TestGlobNewestFirst`, `machine.TestSearchRefusesBadRequests` | built |
| A second session started in a checkout another running session writes gets a worktree on `agents/<agent>/<session>`; three sessions give three worktrees on three branches | `TestSecondSessionGetsAWorktree`, `TestThreeSessionsThreeWorktrees` | not built |
| An ended session's worktree whose branch is merged and clean is removed, and one with uncommitted changes is kept | `TestWorktreeRemovalRule` | not built |
| A Cella machine is created on the Environment the session names, including one served by a worker outside the stub cluster, and the runner opens no connection to the worker | `TestCellaMachineOnNamedEnvironment` in the Cella tier | not built |
| A runner restarted mid-session finds the sandbox by name, starts it if stopped, and continues; a deleted sandbox is recreated with the latest checkpoint and `session.machine` reason `restored` | `TestCellaMachineSurvivesRunnerRestart`, `TestCellaMachineRestoredFromCheckpoint` | not built |
| The suite's recorded tool calls give the same results on the host and on a Cella machine | `TestMachineParity` in the e2e tier | not built |
| `bash` output on a Cella machine arrives as it is produced, not only at exit | `TestCellaExecStreams` | not built |
