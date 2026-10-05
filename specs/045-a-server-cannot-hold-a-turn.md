---
title: "A server cannot hold a turn: a foreground command that starts a server moves to the background after a grace, with its port, pid and log"
status: complete
track: core
depends_on: [008-tools.md, 009-machines.md]
affects: [machine/, machine/host/, machine/cella/, cmd/topos-machine/, harness/tools/, prompts/]
effort: small
created: 2026-10-05
updated: 2026-10-05
author: changkun
---

# A server cannot hold a turn

## Overview

A model asked to serve what it built runs `python3 -m http.server 8000`
with `bash` in the foreground, though the tool's description tells it
to pass `background: true` for a server. The call then holds the turn
until the command's timeout, 120 seconds by default, and the person
sees the session running and nothing else; at the timeout the server
is killed, so the wait bought nothing. This spec makes the machine
notice a server: a foreground command still running after a short
grace whose process group listens on a TCP port it chose is moved to
the background, as if it had been started there, and the call returns
at once and says so.

## Current state

`bash` runs a foreground command through `Machine.Exec` and waits for
its end or its timeout ([[008-tools]]); a `background` call starts a
detached job whose output goes to a job log in the spill directory and
returns its pid and log ([[009-machines]]). Nothing tells the two apart
once a command has started: a foreground server runs to its timeout and
is killed with its process group.

## Design

### The rule

A foreground `bash` call sets `ExecRequest.ServerGrace` to
`BashServerGrace`, 3 seconds. Once the command has run that long and is
still running, the machine reads the TCP sockets in state LISTEN held
by the processes of the command's process group, and reads them again
every `machine.ServerPoll`, 500 milliseconds, until the command ends.
When one of them listens on a port below the low bound of the system's
ephemeral range, the command is moved to the background.

The ephemeral range is where the system puts a listener bound to port
0, as `httptest`, vitest and pytest's servers are, and they listen for
the whole run of a test suite. A port there does not count, so a test
run stays in the foreground: a server meant to be reached listens on a
port it chose. The low bound is
`/proc/sys/net/ipv4/ip_local_port_range`'s first number on Linux,
32768 when that file cannot be read, and the sysctl
`net.inet.ip.portrange.first` on macOS, 49152 when it cannot be read.

Every other long command keeps the timeout it had. A command that
listens for less than the grace and ends is never looked at; one that
stays in the foreground after the grace is looked at again at each
poll, so a server that binds late still moves.

### Reading the group's sockets

`machine.ServerPorts(ctx, pgid)` answers the ports, sorted and without
repeats. On Linux it reads `/proc`: the processes whose `stat` names
`pgid` as their group, the `socket:[inode]` links of their `fd`
directories, and the rows of `/proc/net/tcp` and `/proc/net/tcp6` in
state `0A` with one of those inodes. A process that exits between the
listing and the read, or whose descriptors this user may not read, is
passed over; a socket table that cannot be read is an error. On macOS
it runs `/usr/sbin/lsof -nP -a -g <pgid> -iTCP -sTCP:LISTEN -Fn`, whose
exit status 1 with nothing printed is no listener. Other systems,
Windows among them, answer `ErrServersUnseen` and `SeesServers` is
false, so the check never runs there.

Cella's sandboxes run under the runtime class an operator declares,
runc unless one is named; gVisor and a VM runtime both serve `/proc`
and its socket tables, so the helper's read is the same in each.

### The move

On the host machine, a command run directly keeps its output pipe: the
stream the caller reads ends at once, without the pipe closing, the
pipe drains into a new job log in the spill directory's `jobs`
directory, where background jobs' logs are, and the group joins the
machine's background jobs, which end with the session. The log ends
with the host's exit line, `[job <pid> exited with code <n>]`, once the
shell has exited and the pipe is drained. The command's final directory
is not waited for, since the shell has not exited, so the thread keeps
the directory it had. A machine released meanwhile keeps no job, and
the command ends as a canceled foreground command does.

On a Cella machine the helper's `run` mode takes `-server-grace` and
`-jobs`, and the machine passes the spill directory's `jobs`
directory. On a move the helper's own pump stops reading, a log pump,
the helper itself in its `pump` mode in a session of its own, takes the
pipe and appends it to the job log, the command's input ends, and the
exit frame says `moved` with the pid, the log and the ports. The helper
then exits without killing the group, and the log pump outlives it as a
background job outlives the helper that started it; the log ends with
`[job <pid> output ended]`, since the log pump is not the shell's
parent and does not learn its exit code.

A sandboxed host, which runs each command as a stage of a host sandbox
driver, does not move a server: the stage's processes are the driver's
to see, not the machine's, and the command keeps its timeout.

### What the model reads

A moved call is an `ok` result: the output so far, then
`results/bash/moved-v1`, which says the command was still running after
the grace and listening on its port or ports, that it was moved to the
background as pid N and keeps running, that its output so far is above
and what it writes from now on goes to the log, and how to read the
log and stop it. The result carries no directory and no exit code. The
tool's description, `tools/bash-v2`, says that a foreground server
moves after 3 seconds and that a listener on a port the system picked
does not count.

When the check could not be had, because the group's sockets could not
be read or the job log could not be made, the command goes on in the
foreground and `ExecResult.ServerErr` says why; a call that then passes
its timeout adds `results/bash/unchecked-v1` to the timeout's sentence,
so the model learns why its server held the call.

## Not in this spec

A server that leaves its process group, as one started with `setsid`
or a daemon that forks away, already returns the call itself. A server
in another network namespace than the machine's. A foreground script
that starts a server with `&` and then runs a long command beside it is
moved like any server once the grace passes; its output goes on in the
log the result names.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A foreground `bash` command that listens on a chosen port returns within the grace and a poll, says it moved with its port, pid and log, keeps serving, writes what follows to the log, and ends with the session | `harness/tools.TestBashMovesAServerToTheBackground` | built |
| A listener on a port the system picked stays in the foreground to its end; a command that never listens is unchanged; a server past its timeout before the grace is killed as before | `harness/tools.TestBashLeavesAListenerTheSystemPlacedInTheForeground`, `harness/tools.TestBashACommandThatNeverListensIsUnchanged`, `harness/tools.TestBashLeavesAServerToItsTimeoutWhenTheGraceIsLonger` | built |
| The Linux read of a group's listening ports from `/proc`, the ephemeral bound and its default, and lsof's output are parsed to the ports below the bound | `machine.TestProcServerPorts`, `machine.TestStatGroup`, `machine.TestLsofPorts`, `machine.TestServerPortsOfThisProcess` | built |
| The helper moves a server, hands its output to a log pump that outlives it, and answers the move; a move it cannot make leaves the command to its timeout and says why; the log pump ends its log | `cmd/topos-machine.TestRunMovesAServerToTheBackground`, `cmd/topos-machine.TestRunKeepsAServerItCannotMove`, `cmd/topos-machine.TestRunRefusesAServerGraceWithoutJobs`, `cmd/topos-machine.TestPump` | built |
| A Cella machine asks the helper for the move and returns its port, pid and log, with the server's later output in the job log | `machine/cella.TestAServerMovesToTheBackground` | built |
| The texts state the grace the tool enforces, and render as pinned | `harness/tools.TestToolTextStatesTheLimitsItEnforces`, `prompts.TestEveryTextRendersItsCurrentBytes` | built |

## Outcome

Built as designed on 2026-10-05. The tests above pass on macOS, where
lsof reads the sockets, and on Linux in a container, where `/proc`
does. In the listener tests, the call returned 0.30 to 0.37 seconds
after it started with a grace of 300 milliseconds, the first read
finding the server; the helper answered 0.20 to 0.25 seconds after the
start with a grace of 200 milliseconds.

Two points the design left open were settled. The log of a server the
helper moved ends with `[job <pid> output ended]` rather than the exit
line a background job's log gets, since the log pump that writes it is
not the shell's parent. A failed read of the sockets is not shown to the
model unless the command then passes its timeout, when it is the reason
the server was not moved.
