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
| processes | each command runs in its own process group, killed on timeout, cancel, and at session end for background jobs; a script past 64 KiB is handed to `/bin/sh` as a file of its own in the spill directory instead of as its `-c` argument, because Linux refuses one argument past 128 KiB, and the file is removed when the command or job ends |

The credential deny-list:

| Where | Entries |
|---|---|
| the home directory | `.ssh/`, `.aws/`, `.azure/`, `.config/gcloud/`, `.kube/config`, `.docker/config.json`, `.netrc`, `.git-credentials`, `.npmrc`, `.pypirc`, `.gnupg/`, `.password-store/`, `.config/gh/hosts.yml`, `$TOPOS_DATA_DIR/credentials/` |
| any root | `.env` and `.env.*` except `.env.example`, `.env.sample` and `.env.template`; `*.pem`, `*.key`, `*.p12`, `*.pfx`; `id_rsa*`, `id_ecdsa*`, `id_ed25519*` |
| the environment | names ending `_TOKEN`, `_SECRET`, `_PASSWORD`, `_API_KEY` or `_PRIVATE_KEY`; `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`; every name starting `TOPOS_`, since a server's configuration holds its signing key, its credentials key and its database URL |

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

### Host sessions on a server

A server role, `serve` or `runner`, runs a hosted session on its own
host only when the operator sets `TOPOS_HOST_SESSIONS=on`; otherwise a
session asking for a `host` machine is refused with
`machine_unavailable`. A server's host is shared by every principal
the installation serves, so the person's-own-host rules above do not
carry over, and these replace them:

| Aspect | Rule |
|---|---|
| sandbox | mandatory. With the switch on, the role refuses to start unless `latere.ai/x/pkg/hostsandbox`'s preflight passes and a probe command runs inside the sandbox and is denied a read of the data directory; a binary that is present is not proof, since Bubblewrap in a container without user namespaces fails at launch. A server session never runs with `sandbox: none` |
| working directory | `$TOPOS_DATA_DIR/host-sessions/<ses_id>/`, created empty for the session, with the session's repositories cloned into it ([[019-git]]); its spill directory `<ses_id>.spill` and the directory of its stage logs `<ses_id>.stages` lie beside it; the three are removed when the session ends on the host that ran it, and by the delete of a session that had not ended, when the serving host holds them |
| roots | the working directory, the spill directory and the attached memory directories; an agent whose `spec.machine.roots` or `spec.machine.readPaths` is set is refused `machine_unavailable`, since those name the server's own paths |
| reads | the roots, the toolchain and system directories, and nothing else under `TOPOS_DATA_DIR`: no other session's directory, no store, no credential file; the files `TOPOS_CELLA_TOKEN_FILE` and every other configured file are denied |
| environment | only `PATH`, `LANG`, `TZ`, `TERM=dumb` and `HOME` set to the working directory, then the session's placeholders ([[018-credentials-and-secrets]]), and what the sandbox adds: `TMPDIR`, a directory in the session's spill directory rather than srt's `/tmp/claude` that every stage on the host shares, and the variables of its allowlist proxy; the server's own environment never reaches a command |
| network | the hosts of the agent's `spec.machine.egress` through the sandbox's allowlist proxy, as on a person's host; `web_fetch` reaches the same hosts, a host name or a subdomain of a `*.` entry, checked on the URL and on every redirect |
| worktrees | none: each session has its own directory, so the owner file and worktree rules above do not apply; isolated threads still get worktrees inside it |

`session.machine` records the sandbox driver in `sandbox`, `host` for
the srt driver. Each command is one stage of the driver: its output
goes to a log outside every root, which the machine streams; a timeout
stops it at once and a cancel with the host's grace; what it leaves in
its process group is killed once it exits; its input and its final
directory travel as files in the spill directory; and a background job's
log is copied into the spill directory's job log. `progressive` is
available, since the sandbox is always present
([[012-permissions-and-approvals]]).

### The Cella machine

`machine/cella` creates one sandbox per session through
`latere.ai/x/cella/client` at `TOPOS_CELLA_URL`, with the session's
own credential ([[018-credentials-and-secrets]]); unset, a session
asking for a `cella` machine is refused with `machine_unavailable`.
Every call carries an instrumented client on HTTP/1.1, which the exec
socket's upgrade needs, and bounds no call: a held create and a
command's session are bounded by their contexts.

| Cella Sandbox manifest (`v1beta1`) | From |
|---|---|
| `metadata.name` | `ses-` and the session's ULID in lowercase, so a runner finds the sandbox by name |
| `metadata.labels` | `topos.latere.ai/session`, the session's id, and `topos.latere.ai/agent`, the agent's name |
| `spec.environment` | the session's `machine.environment`, which may name an Environment whose workers the developer runs; absent, Cella's default |
| `spec.image` | the agent's `spec.machine.image`; absent, Cella's default, `base` |
| `spec.resources` | the agent's `spec.machine.resources` |
| `spec.network.egress` | `allowlist` with the hosts of the agent's `spec.machine.egress`, the session's repositories' git host ([[019-git]]) and the scope hosts of its named secrets, each read from its `Secret`, lowercased, sorted and once each |
| `spec.secrets` | the session's named secrets, each under the variable its placeholder arrives in ([[018-credentials-and-secrets]]) |
| `spec.lifecycle` | persistent: `autoStop: 15m` and `autoDelete: never`, so a stopped sandbox keeps its workspace until the session ends, and a `ttl` of the session's remaining age as a backstop; a `ttl` under 15 minutes is the `autoStop` too, because Cella holds an idle stop inside the sandbox's life |

Open reads the sandbox by name. One that runs is used; one Cella
stopped is started; one queued, pending, starting, stopping or
recovering is read again until it runs; one that failed cannot be
started and is deleted and created again; one being deleted is waited
out and created again. A sandbox that is not there is created, and the
create is held until the sandbox runs (`CreateSandbox` with the wait
option); a create that loses a race with another runner's takes that
runner's sandbox. The machine reports whether it created the sandbox,
which is how a runner that expected one learns its files are gone.

The helper `topos-machine` is a static binary of this module
(`cmd/topos-machine`, built for `linux/amd64` and `linux/arm64` and
embedded in the runner, which hands the builds to `machine/cella` by
platform). On every open and every start, one `Exec` makes the spill
directory, reads the platform with `uname`, and reads the SHA-256 of
the helper already at `/tmp/topos/bin/topos-machine`; when it is
absent or another build, the machine uploads the build of that
platform. Cella's file routes serve the workspace alone, so the upload
travels as the input of an `ExecSession` that `head -c` reads to its
length, because the exec socket cannot end an input. A call that finds
no helper, which a start by another caller can leave behind, puts it
back and runs once more. The helper is reached through `/bin/sh`, so a
missing one answers exit 127 the same way on every driver.

| Operation | Through |
|---|---|
| `Exec`, `ExecStream` | an `ExecSession` running the helper's `run`: the command under `/bin/sh -c` in its own process group, its input sent as frames with an end frame, its output streamed as frames as it is written, its final directory in a frame of its own when asked for, and its exit; the helper kills the group on the timeout, on a kill frame the machine sends when the context ends, and when the session ends; Cella's own bound on the session lies past the command's timeout and the grace, at most Cella's hour; a script past 8 KiB travels as input frames before the command's input, because Cella bounds the session's first message with its body limit |
| `Exec` with `Background` | an `Exec` running the helper's `job`: the command detached in its own process group, its output in a job log in `/tmp/topos/spill/jobs/` that ends with the host's exit line; a script past 8 KiB is written to the jobs directory first, which the shell reads as its script and the job removes when it ends; the job ends with the sandbox. On the sandbox too, a script past 64 KiB reaches `/bin/sh` as a file, never as one argument |
| `ReadFile`, `WriteFile`, `Stat`, `List`, `Remove`, `Rename` in the workspace | `FileGet`, `FilePut`, `FileStat`, `FileList`, `FileRemove`, `FileMove`; a write with mode 0 keeps the file's mode, read with `FileStat` first; `Remove` refuses a directory with entries, as `os.Remove` does on the host, before Cella's route, which removes a tree |
| the same outside the workspace | the helper's `fs`, through `os.Root` over the root the path is in; the spill directory is reached this way |
| `Search` | an `ExecSession` running the helper's `search`, which reads the `SearchRequest` as JSON on its input and writes the `SearchResult` as JSON, running `machine.SearchFS` over the root the path is in, so grep and glob need no binary in the image and give the host's results |
| `Fetch` | a command in the sandbox running `curl` with the limits of `web_fetch` ([[008-tools]]), so Cella's egress gateway decides what it reaches; an image without `curl` answers an error |
| `ImportTar`, `ExportTar` | Cella's tar routes, for repository delivery ([[019-git]]) and checkpoint restore ([[034-checkpoints-and-rewind]]), below the workspace |

The roots are the working directory (the sandbox's `spec.workdir`,
its workspace unless the session names another), the workspace, and
the spill directory `/tmp/topos/spill`, outside both. The deny-list's
base-name entries hold in every root and are hidden from search; the
home entries name nothing in a sandbox, which holds none of the
person's credentials. The sandbox's environment is Cella's, named
secrets arrive as placeholders, and no variable is removed. Each
failure answers what the host machine answers for it: a missing path
is `fs.ErrNotExist`, a missing start directory too, so `bash` falls
back to the working directory as on the host.

Lifetime: the sandbox is the session's. `Release(ctx, false)` leaves
it: while the session is idle for 15 minutes Cella stops it and keeps
its workspace, and a call on a machine whose sandbox Cella stopped
starts it and runs once more. The next runner that claims the session
finds it by name and starts it. `Release(ctx, true)` deletes it when
the session ends, and a delete Cella could not do leaves the machine
to be released again. A call that finds the sandbox gone answers
`machine_lost`, as every call does after it, until `Recreate` creates
the sandbox again with a fresh workspace. When a runner finds the
sandbox gone, it creates one again, restores the latest checkpoint
([[034-checkpoints-and-rewind]]) with `ImportTar`, and appends
`session.machine` with reason `restored`; with no checkpoint to
restore it appends `session.machine` with reason `replaced` and a
`session.error` `machine_lost`, so the model and the person know the
files are gone. The control plane never dials a sandbox or a worker:
every call goes to Cella's API, and a self-hosted Environment's
workers connect out to Cella (invariant 10 of [[001-architecture]]).

### Error codes

| Code | Retryable | Meaning |
|---|---|---|
| `machine_unavailable` | no | the machine kind is not configured (a `host` machine on a server without `TOPOS_HOST_SESSIONS=on`, or one whose agent names `machine.roots` or `machine.readPaths`), the runner carries no helper for the sandbox's platform, or Cella refused the sandbox: its Environment, its image, a secret it names, or the session's credential; a Cella that could not answer, a 5xx, a 429 or no answer, is transient and carries no code |
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
| A script longer than one Linux argument may be runs in the foreground and the background and leaves no file behind | `machine.TestShellArgsRunsAScriptPastTheArgumentLimit`, `machine/host.TestAScriptPastTheArgumentLimitRuns`, `machine/cella.TestLongScripts` | built |
| Search is Go-native where the files are: grep honors `.gitignore` at and above the root, has its three output modes, and glob lists newest first | `machine.TestGrepFilesHonorsGitignore`, `machine.TestGrepBelowTheRootHonorsParentIgnores`, `machine.TestGrepModes`, `machine.TestGlobNewestFirst`, `machine.TestSearchRefusesBadRequests` | built |
| A second session started in a checkout another running session writes gets a worktree on `agents/<agent>/<session>`; three sessions give three worktrees on three branches | `TestSecondSessionGetsAWorktree`, `TestThreeSessionsThreeWorktrees` | not built |
| An ended session's worktree whose branch is merged and clean is removed, and one with uncommitted changes is kept | `TestWorktreeRemovalRule` | not built |
| With `TOPOS_HOST_SESSIONS=on`, a server refuses to start when the host sandbox's preflight or probe fails; a server session works in its own directory, cannot read another session's directory or the data directory, and gets none of the server's environment | `cmd/toposd.TestHostSessionsOnAServer`, `internal/hosted.TestNewHostRefusesASandboxThatDoesNotHold`, `internal/hosted.TestAHostSessionsMachine`, `internal/hosted.TestHostSessionsAreConfined`, `internal/config.TestHostSessions`; the confinement checks run where `srt` is installed and skip, naming it, elsewhere | built |
| A server without `TOPOS_HOST_SESSIONS=on` refuses a `host` session with `machine_unavailable`, and with it on refuses one whose agent names `machine.roots` or `machine.readPaths` | `cmd/toposd.TestServeRefusesAHostSessionWithoutTheSwitch`, `internal/server.TestHostSessionsBehindTheSwitch`, `internal/hosted.TestByKind` | built |
| A sandboxed host runs each command as a stage with the session's roots, denials, egress and environment, keeps the host's exec contract (output, exit code, final directory, input, timeout, cancel, process group, background jobs, a script past 64 KiB), and fetches only its egress hosts | `machine/host.TestExec`, `machine/host.TestExecTimeoutAndCancel`, `machine/host.TestExecStream`, `machine/host.TestBackgroundJobsEndWithTheSession`, `machine/host.TestAScriptPastTheArgumentLimitRuns`, `machine/host.TestAStageRunsWithTheSessionsPolicy`, `machine/host.TestAStrayChildEndsWithItsCommand`, `machine/host.TestASandboxedFetchReachesOnlyTheEgress` | built |
| A Cella machine creates the session's sandbox by name on the Environment the session names, with the labels, the allowlist with the secrets' hosts, the named secrets and the persistent lifecycle held inside the ttl, holds the create until it runs, and uploads the helper; each Cella refusal at open is `machine_unavailable` and a Cella that could not answer is not | `machine/cella.TestOpenCreatesTheSandbox`, `machine/cella.TestTheLifecycleStaysInsideTheTTL`, `machine/cella.TestOpenRefusals`, `machine/cella.TestCode` | built |
| A Cella machine is created on the Environment the session names, including one served by a worker outside the stub cluster, and the runner opens no connection to the worker | `TestCellaMachineOnNamedEnvironment` in the Cella tier | not built |
| Open finds the sandbox by name and uploads nothing again, starts one Cella stopped and waits for one starting, replaces a failed one as created, and takes the sandbox of a runner whose create won a race; a sandbox Cella stopped under a running machine is started by its next call, and a missing helper is put back | `machine/cella.TestOpenFindsTheSandboxByName`, `machine/cella.TestAStoppedSandboxIsStartedByTheNextCall`, `machine/cella.TestTheHelperIsPutBackWhenMissing`, `machine/cella.TestWaitsAndRaces` | built |
| A sandbox that is gone answers `machine_lost` on every call until `Recreate` creates it again, reported created with a fresh workspace, and `ImportTar` and `ExportTar` carry the workspace as tar | `machine/cella.TestALostSandboxIsRecreated`, `machine/cella.TestTar` | built |
| `Release(ctx, false)` leaves the sandbox to Cella's idle stop and `Release(ctx, true)` deletes it, after which every call answers `ErrReleased` | `machine/cella.TestRelease` | built |
| A runner restarted mid-session finds the sandbox by name, starts it if stopped, and continues; a deleted sandbox is recreated with the latest checkpoint and `session.machine` reason `restored` | `TestCellaMachineSurvivesRunnerRestart`, `TestCellaMachineRestoredFromCheckpoint` | not built |
| Commands on a Cella machine run under the helper in their own process group with their exit code, their final directory, an input that ends, a timeout and a cancel that end the group, and a script of any length; background jobs log to the spill directory | `machine/cella.TestExec`, `machine/cella.TestExecTimeoutAndCancel`, `machine/cella.TestAHelperThatDoesNotAnswerACancelIsClosed`, `machine/cella.TestBackgroundJobs`, `machine/cella.TestLongScripts`, `cmd/topos-machine.TestRunOutputAndExit`, `cmd/topos-machine.TestRunReportsTheFinalDirectory`, `cmd/topos-machine.TestRunDeliversInput`, `cmd/topos-machine.TestRunTimesOut`, `cmd/topos-machine.TestRunCancels`, `cmd/topos-machine.TestRunEndsTheProcessGroup`, `cmd/topos-machine.TestRunReadsAFramedScript`, `cmd/topos-machine.TestJob` | built |
| A Cella machine's file operations go through Cella's file routes in the workspace and through the helper outside it, with the host's errors, the deny-list and the roots | `machine/cella.TestFilesInTheWorkspace`, `machine/cella.TestFilesInTheSpillDirectory`, `cmd/topos-machine.TestFSWrite`, `cmd/topos-machine.TestFSStatListRemoveRename`, `cmd/topos-machine.TestFSRead` | built |
| Search on a Cella machine runs the Go-native grep and glob in the helper, where the files are | `machine/cella.TestSearch`, `cmd/topos-machine.TestSearch` | built |
| Searches, commands and file operations on a Cella machine over the stub Cella answer as the host machine does over the same files | `machine/cella.TestParityWithTheHost` | built |
| The suite's recorded tool calls give the same results on the host and on a Cella machine | `TestMachineParity` in the e2e tier | not built |
| `bash` output on a Cella machine arrives as it is produced, not only at exit | `machine/cella.TestCellaExecStreams` | built |
| A Cella machine's fetch runs `curl` inside the sandbox with the redirect, scheme, size and time limits of `web_fetch` | `machine/cella.TestFetchRunsInTheSandbox`, `machine/cella.TestFetchRefuses`, `machine/cella.TestFetchTimeout` | built |
