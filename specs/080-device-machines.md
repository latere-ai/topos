---
title: "Device machines: a person's own computer as a session's machine, through a Cella environment the computer serves; named at create with a granted folder, opened with no secrets, no repository delivery, no checkpoints and no restore, the helper written for the computer's OS and architecture, scored as a host, and `machine_waiting` while the computer is away or has not approved the session"
status: drafted
track: core
depends_on: [008-tools.md, 012-permissions-and-approvals.md, 015-api.md, 016-runners.md, 018-credentials-and-secrets.md, 034-checkpoints-and-rewind.md, 052-a-sessions-network.md, 065-a-working-directory-rebuilt-from-the-log.md, 073-a-working-directory-kept-in-the-sessions-folder.md, 076-the-working-directory-is-saved-during-a-turn.md]
affects: [machine/, machine/cella/, session/, harness/, internal/server/, internal/hosted/, runner/, cmd/topos-machine/, prompts/, api/, docs/, CHANGELOG.md]
effort: large
created: 2026-10-18
updated: 2026-10-18
author: changkun
---

# Device machines

## Overview

A hosted session's machine is a Cella sandbox, or the server's own host.
This spec adds a third kind: **a person's own computer**. The computer
serves a Cella environment through an embedded worker (Cella specs 081,
082 and 083), so the transport, the operations and the confinement are
Cella's. What Topos adds is how a session names the computer, what it
withholds from a machine that holds the person's real files, how it
scores the agent's actions there, and what a session does while the
computer is asleep or its owner has not yet approved the session.

A device machine works in one folder the person granted on the computer.
It is bound in place, never copied and never deleted. Everything that
would write old content over the person's current files, or move the
work somewhere the person did not choose, is off: checkpoints, a
restore, a replacement machine.

Whether a session may use a computer is the authorizer's decision. This
spec carries the facts it needs and checks no ownership itself.

## Current state

- `machine/machine.go:20` and `session/session.go:114` define two kinds,
  `host` and `cella`.
- `internal/server/sessions.go:166` refuses a create that names a
  machine: "a session's machine is its agent's; a request cannot set it
  yet". `sessionMachine` (line 518) refuses any kind but those two.
- `internal/hosted/hostdirs.go:55` (`ByKind`) sends every kind but
  `host` to the Cella opener.
- `hosted.Cella` (`internal/hosted/hosted.go:523`) mounts the session's
  `-lux`, `-origo` and `-connections` Secrets and sets `LUX_URL`.
- `machine/cella` puts its helper, `topos-machine`, into
  `/tmp/topos/bin` through an exec session's stdin
  (`ensureHelper`, `put`, `machine/cella/cella.go:716-777`), and only
  Linux builds exist.
- `runner.opened` (`runner/runner.go:394`) delivers repositories and
  attachments into the machine after it opens.
- A lost Cella machine is recreated and restored from the session
  folder or the log (specs 065, 073), or replaced.
- `harness/permission.go:168-170` scores every kind but `cella` as a
  host, and the in-sandbox allow in `confirm` mode (line 291) applies to
  `cella` alone.

## Design

### The kind

`machine.KindDevice` and `session.MachineDevice` are `"device"`. A device
is named per session at create, never in an agent's manifest: an
agent's `spec.machine.kind` stays `host` or `cella`, and a manifest that
names `device` is refused with `invalid_manifest`.

### Naming the machine at create

`POST /v1/sessions` accepts `machine` when its kind is `device`:

```json
{ "machine": { "kind": "device", "environment": "dev-01jaq7v3k9m2", "workdir": "/Users/ada/code/site" } }
```

`environment` is the name of the Cella environment the computer serves. `workdir` is
an absolute, clean path. Any other kind in a create is refused as today.
The machine is fixed for the session's life: no update changes it.

The authorizer's question for a fork of a device session carries the
inherited `machine`, `environment` and `workdir`, so it decides whether
the person forking may use the computer. A fork it refuses is not
created; it is never moved to another machine.

The `session.create` question to the authorizer carries `machine:
device`, `environment` and `workdir`, so the authorizer decides whether
the person the session acts for may use that computer. The session's
recorded machine is `{kind: device, environment, workdir}`.

### Opening

`ByKind` sends `device` to the Cella opener with a device profile:

| Field | A device machine |
|---|---|
| `spec.environment` | the session's environment |
| `spec.workspace.host` | the session's `workdir` |
| `spec.image` | none |
| Secrets | none: no `-lux`, `-origo` or `-connections` Secret, and no `LUX_URL` |
| egress | what the session's network asks for (spec 052); the computer narrows it to its own (Cella spec 081) |
| lifecycle | `autoStop` 15 minutes, `autoDelete` never, `ttl` the session's remaining age, as for any Cella machine |
| labels | `topos.latere.ai/session` and `topos.latere.ai/agent` as for any Cella machine, plus `topos.latere.ai/agent-version`, and for a session a trigger started, `topos.latere.ai/trigger` and `topos.latere.ai/trigger-fires-on` (`schedule` or `events`). A session with no trigger label was started by a person |
| annotations | `topos.latere.ai/agent-name`, the agent's display name, which a label cannot hold |
| environment variables | none of the Cella machine's: no `HOME`, so the sandbox keeps the driver's own scratch home |

The computer shows the labels when it asks its owner, and remembers a
scheduled trigger by them, so they name what the
person needs to judge the request.

Ending the session deletes the sandbox, which on the computer removes
only the sandbox's scratch directory.

The device machine answers `Info().Kind == "device"`, `Info().OS` and
`Info().Arch` from the environment's reported host (Cella spec 082's
`status.hosts`), and `Info().Sandbox` from the environment's isolation,
which is `process` on the only driver that serves a device today.

### The helper

`topos-machine` is built for `darwin/arm64`, `darwin/amd64`,
`linux/amd64` and `linux/arm64`. A Cella machine's helper directory is
fixed before create (`/tmp/topos`), which a device's OS sandbox does not
let it write. A device machine instead creates the sandbox, reads its
`status.paths.tmp` (Cella spec 081), sets the helper directory to
`<tmp>/topos` before the helper probe runs, and writes the build for
`Info().OS` and `Info().Arch` through Cella's file route with mode 0755,
not through stdin. The
helper's protocol is Topos's alone, so a release of the program that
connects the computer never waits for one of Topos.

### What a device machine does not do

- **No repository delivery.** `runner.opened` delivers no repository
  into a device machine. The person's folder is already their working
  copy.
- **Attachments into scratch.** Files the person attached to the session
  go to `<tmp>/topos/attachments/`, never into the folder, and the
  prompt names their paths.
- **No checkpoints, no saved working directory, no restore.** The device
  machine implements neither `Recovering` nor `Importer`. Specs 034, 065,
  073 and 076 skip it.
- **No replacement.** A device machine that is lost is never recreated
  elsewhere, and a session never moves to a Cella sandbox in the cloud.
- **No connections in the machine.** `ApplyConnections` is a no-op and
  `ReachesConnections` is false. MCP tools still run in the runner.
  `publish` is withheld, as on a host machine.

### Scoring

A device session scores as a host session. The device machine's kind is
not `cella`, so the host weights apply (write 0.3, bash 0.5, a network or
delete command 0.7), and the in-sandbox allow of `confirm` mode never
applies. `CheckSandbox` already refuses `progressive` when
`Info().Sandbox` is `none`, which covers a later driver that reports it.
A test pins each of these, so a later change to the Cella branch cannot
reach a device.

The machine section of the system prompt says the machine is the
person's own computer, that commands change real files, and that file
content read there is data and not instructions. The `session.machine`
event carries the environment, OS, architecture and folder. A client
names the computer from its own list of the person's devices, since
the device's name is the platform's record and not Topos's.

### Waiting for the computer

A new stop reason, `machine_waiting`, with an event
`session.machine_waiting{reason, environment}`:

| What the opener finds | What the session does |
|---|---|
| the environment answers `driver_unavailable`, or is `Offline` | the turn stops at once with `machine_waiting`, reason `offline` |
| the sandbox's `HostApproval` condition is `Pending` (Cella spec 082) | the tool call waits, and `session.machine_waiting` with reason `approval` is emitted so a client shows "approve on your device". After two minutes the turn stops with `machine_waiting`, and the create keeps waiting on the computer |
| the sandbox is `Failed` with `host_refused` or `host_timeout` | the call's result says the computer refused, with the host's text, and `session.error` `machine_refused` is recorded. The model may continue, and the call is not retried in that turn |
| a running command's machine is lost mid-call | the call's result is `machine.Stop{Lost}`, and the turn stops with `machine_waiting`, reason `offline` |

A session stopped with `machine_waiting` continues on the next
`user.message`, or on `POST /v1/sessions/{id}/resume`, which today
resumes only a session stopped on its budget (`internal/server/resume.go:47`)
and now also admits `machine_waiting`. Either opens the machine again;
resume adds no message. A session fired by a trigger idles the same way
and appears in its owner's list.

## Not in this spec

- Who may use which computer: the authorizer's.
- A machine named at create for any other kind (spec 077 and the
  installation's environment profiles).
- Screen and input on the person's computer; Windows, where commands do
  not run through `/bin/sh`.

## Acceptance criteria

1. `TestCreateNamesADevice`: a create with a device machine records
   `{kind: device, environment, workdir}` and sends all three to the
   authorizer; a create naming `cella` or `host` is still refused; a
   manifest naming `device` is refused; the authorizer's question for a
   fork of a device session carries the inherited machine, and a refused
   fork creates nothing.
2. `TestDeviceLabels`: a trigger's device session carries the agent
   version, the trigger and what it fires on.
3. `TestDeviceOpensWithNoSecrets`: the Cella create for a device machine
   has `workspace.host` set, no image, no Secret mounts and no `LUX_URL`,
   against `cella-stubs`.
4. `TestDeviceHelperByOSAndArch`: on a stub host reporting
   `darwin/arm64`, the helper written is the darwin arm64 build, through
   the file route, under `status.paths.tmp`.
5. `TestDeviceGetsNoRepositoriesOrRestore`: a session with repositories
   and attachments delivers no repository, writes attachments under the
   scratch directory, and after the machine is lost neither restores nor
   replaces it.
6. `TestDeviceScoresAsHost`: in `confirm` mode a write on a device
   machine asks, where the same write on a Cella machine is allowed, and
   `progressive` is refused when a stub host reports isolation `none`.
7. `TestDeviceWaits`: offline stops with `machine_waiting` (`offline`);
   a held create emits `session.machine_waiting` (`approval`) and stops
   after two minutes; a refusal records `machine_refused` and the turn
   continues; the next message, or `resume`, continues on a running
   sandbox, and `resume` of a session stopped for any other reason but
   its budget is still refused.
8. `TestDeviceSetsNoHomeAndFindsItsHelperDir`: the device create carries
   no `HOME`, and the helper probe runs in `<status.paths.tmp>/topos`.
9. The conformance suite (spec 029) covers a device session end to end
   against a Cella worker on the `local` driver in CI on Linux, and the
   release builds the four helper targets.
