---
title: "Permissions, approvals and hooks: the boundary, the layers, the risk score and verdict, the modes"
status: drafted
track: core
depends_on: [001-architecture.md, 004-session-log.md, 005-harness-loop.md, 008-tools.md, 009-machines.md]
affects: [harness/permission/, harness/, machine/host/]
effort: large
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Permissions, approvals and hooks

## Overview

Agents do most of their work through `bash`, and a command's text does
not say what it will do: pipes, subshells and scripts the agent wrote a
moment earlier defeat any rule written against the string. So policy
sits where an effect has to cross a boundary as a typed request, and
not on the command. On a Cella machine the sandbox is the boundary; on
the host it is an operating-system sandbox around every command. Above
the boundary, approvals come in layers, none trusted alone: an
organization's always-confirm and always-allow lists, a risk score on
every tool call recorded with its source, and a verdict of allow, flag,
ask or block, where ask pauses the session durably and never silently
denies. Three modes decide how the layers ask. Hooks may deny or modify
a call and never widen one.

## Current state

v0.7.0's hook bus was allow-by-default, and 17 of its 29 declared hook
events were never dispatched; its merge rule,
`net_allow = hook_allow AND NOT deny_rule_matched`, is kept here as the
rule that a hook cannot widen. v0.7.0 spec 031's grants are the tool
narrowing of [[013-threads-and-subagents]]. The retired hosted service
defined the verdict flag (allow, and mark for review) for its
governance, which this spec carries; its modes ask, edit and auto are
replaced. The retired laptop client ran every command unconfirmed.

## Design

### The boundary

| Machine | The boundary | What decides at it |
|---|---|---|
| a Cella sandbox | the sandbox: its files are disposable, it holds no credential, its network leaves only through Cella's egress gateway | inside, `bash` runs freely; outside, typed points: the egress allowlist and credential substitution (Cella), the agent's permissions and the session scope at every core (the installation's authorizer), the git host's ref rules ([[019-git]]), and the runner-held tools (MCP, deploys), where irreversible ones wait for a confirmation |
| the host | an operating-system sandbox around each command | the person, through the mode and the prompt; the sandbox holds when a prompt was answered carelessly or not at all |

The host sandbox is built on `latere.ai/x/pkg/hostsandbox`, which
renders Seatbelt on macOS and Bubblewrap with seccomp on Linux; on a
Linux host without Bubblewrap, Landlock (ABI 3 or later) is applied by
the command's helper process to itself before it executes the command.

| Policy | Rule |
|---|---|
| writes | only the machine's roots ([[009-machines]]), the session's temporary directory, and the user cache directory and the Go module cache, so builds work |
| reads | everything outside the home directory; inside it, the roots, the cache directories, the toolchain directories (`go`, `sdk`, `.cargo`, `.rustup`, `.nvm`) and the agent's `spec.machine.readPaths` ([[003-manifest]]) |
| credential files | the deny-list of [[009-machines]], unreadable in every mode |
| network | only the hosts of the agent's `spec.machine.egress`, through the sandbox's allowlist proxy |

A host with none of the three mechanisms records `sandbox: none` in
`session.machine`, and a session on it may run only in `plan` or
`confirm`; `progressive` is refused with `sandbox_unavailable`.

### The layers

1. **Hard boundaries**: the machine's boundary above, the agent's
   permissions and the session scope, the git host's ref rules. They
   hold whatever anyone answers.
2. **Lists**: `always_confirm` and `always_allow`, lists of patterns,
   from the authorizer's `limits` in the decision at session start, or,
   for a session with no authorizer, from the agent's `spec.approvals`.
   Read-only tools are on `always_allow` unless a list says otherwise.
   `always_confirm` wins over `always_allow`.
3. **Risk score**: a number in `[0, 1]` on every call, with its source.
4. **Verdict**: from the mode, the lists and the score, then narrowed
   by hooks.

### Patterns

A pattern is `<tool>` or `<tool>(<glob>)`. The glob is over the
command for `bash` (`bash(git status*)`), the path for file tools
(`write(docs/**)`), `domain:<host>` for `web_fetch`, and the tool's
full name for MCP tools (`mcp__github__create_issue`). `*` matches any
characters and `**` any path segments. A pattern saves a prompt and is
never a security claim: a match changes an ask into an allow, never
crosses a hard boundary, and never exempts a command from the host
sandbox.

### The risk score

The first source is rule features, `rules/1`. A model-based classifier
(`classifier/<model>`) and a learned per-person threshold
(`learned/<version>`) may follow, evidence-gated, in the same fields;
nothing in the design depends on them.

| Features of the call | Score |
|---|---|
| effect `none` or `read` | 0.0 |
| effect `write` inside a Cella machine | 0.1 |
| `memory_sync`, whose writes carry preconditions ([[020-memory-stores]]) | 0.1 |
| `write` or `edit` on the host (reversible by the turn's checkpoint) | 0.3 |
| `bash` on the host | 0.5 |
| `bash` on the host whose command names a network or deleting program (`curl`, `wget`, `ssh`, `scp`, `rsync`, `git push`, `rm -r`) | 0.7 |
| effect `external` to a host the agent's egress names | 0.4 |
| effect `external` otherwise, including runner-held MCP calls | 0.6 |

The command-text feature raises a score as a hint and is never a
boundary. `agent.tool_use` records `risk` as `{"score","source",
"features"}` ([[004-session-log]]).

### Verdicts and modes

| Verdict | Meaning |
|---|---|
| `allow` | the call runs |
| `flag` | the call runs and is marked for review |
| `ask` | the call waits for a person; the session goes idle with `tool_confirmation` |
| `block` | the call does not run; its result is outcome `blocked` with the reason |

| Mode | Verdict |
|---|---|
| `plan` | effect `none` or `read`: `allow`; anything else: `block` with `Plan mode: only read-only tools run.` |
| `confirm` | on `always_allow`, a session pattern, or effect `none` or `read`: `allow`; an effect that stays inside a Cella machine: `allow`; otherwise `ask` |
| `progressive` | score below `flag_at` (0.3): `allow`; below `ask_at` (0.5): `flag`; below `block_at` (0.9): `ask`; otherwise `block` |

A call on `always_confirm` is `ask` in `confirm` and `progressive` and
`block` in `plan`. The thresholds come from the authorizer's `limits`
or the agent's `spec.approvals.thresholds`, and a learned source may
later set them per person and per agent. The order of evaluation is:
hard boundary, `always_confirm`, the mode, then hooks. Verdicts are
ordered allow, flag, ask, block from most to least permissive, and the
final verdict is the least permissive of the mode's and every hook's.
`agent.tool_use` records the verdict, the reason, and the mode.

Ask never silently denies: nothing times out an ask, because an agent
that is refused routes around the refusal. An unattended session that
reaches an ask waits for a person or its age. A
`user.tool_confirmation` answers one call; with `remember` it also adds
a pattern to the session's allow list for the rest of the session,
recorded in the log so any runner applies it. Every decision, the
automatic ones included, is in the log with its score and the person's
answer, which is the approve and deny record a learned threshold needs;
nothing in the core sends that record anywhere, and its use outside
the session is the person's choice.

### Confirmation across a restart

An ask is durable: its `agent.tool_use` is in the log before the
session goes idle, and an idle session holds no lease. When the
confirmation arrives, the runner that claims the session runs the call.
If a runner stops after a confirmation and before the result, the next
one reads the log: when a `session.status` `running` follows the
confirmation, a runner may have started the call, and it is closed with
`unknown_effect` ([[016-runners]]); when none does, the call never
started and runs.

### Hooks

```go
type Hook interface {
	Name() string
	Handle(ctx context.Context, e HookEvent) (HookResult, error)
}
```

| Event | When | May |
|---|---|---|
| `pre_tool_use` | after validation and scoring, before the verdict applies | lower the verdict to `ask` or `block` with a reason, or replace the input, which is validated and scored again |
| `post_tool_use` | after a call returns, before its result is appended | replace the result's content, for example to remove a value |
| `turn_start`, `turn_end`, `pre_compact` | at those points | observe only |

Only these events exist and each is dispatched. A hook cannot raise a
verdict: a `pre_tool_use` answer of `continue` leaves it as it was. A
`pre_tool_use` hook that errors or passes its 30 second timeout blocks
the call with `hook <name> failed`; an observing hook's failure is a
`session.error` `hook_failed` and nothing else.

A command hook is configured in the agent's `spec.hooks` with an
`event`, a `matcher` pattern, a `command` and a `timeout`, and runs on
the session's machine, inside the host sandbox or the Cella sandbox:
a hook the agent could tamper with can only fail to narrow, which the
boundary still bounds. It receives one JSON object on stdin:

```json
{"event":"pre_tool_use","session_id":"ses_...","thread":null,"turn":3,"step":7,
 "tool":{"name":"bash","tool_use_id":"toolu_01","input":{"command":"make deploy"}},
 "risk":{"score":0.5,"source":"rules/1"},"verdict":"ask","mode":"progressive",
 "machine":{"kind":"host","workdir":"/home/p/src/app"}}
```

and answers on stdout with `{"decision":"continue"|"ask"|"block",
"reason","input"}` for `pre_tool_use` or `{"content"}` for
`post_tool_use`. Exit code 2 is `block` with stderr as the reason; any
other non-zero exit is a failure.

### Error codes

| Code | Meaning |
|---|---|
| `sandbox_unavailable` | `progressive` asked for on a host with no operating-system sandbox |
| `hook_failed` | an observing hook errored or timed out |

## Not in this spec

The agent's permissions, the session scope and key narrowing
([[006-identity]], [[018-credentials-and-secrets]]); the git host's ref
rules ([[019-git]]); the deny-list's entries ([[009-machines]]); where
the lists live in the authorizer's decision ([[006-identity]]); the
console that answers an ask.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A `bash` call on the host cannot write outside the working directory or read a deny-listed credential file in any mode, including when a person allowed the call and when an allow pattern matches it | `TestHostSandboxConfinesBash`, one subtest per mode and mechanism available on the runner | not built |
| Every tool call in a recorded session carries a score, a source and a verdict in its `agent.tool_use` | `TestEveryCallHasScoreAndVerdict` | not built |
| Each mode gives the table's verdict for each feature row, and `always_confirm` asks in `confirm` and `progressive` and blocks in `plan` | `TestModeVerdicts` as a table test | not built |
| No hook and no mode can raise a verdict: a hook answering `continue` to a blocked call leaves it blocked, and a hook that replaces the input gets the new input scored again | `TestHookCannotWiden`, `TestHookReplacedInputIsRescored` | not built |
| An ask pauses the session idle `tool_confirmation`, holds no lease, and never times out into a deny | `TestAskIsDurableAndNeverDenies` | not built |
| A confirmation survives a runner restart: the confirmed call runs exactly once when no runner started it, and is closed `unknown_effect` when one did | `TestConfirmationSurvivesRestart` | not built |
| A command hook receives the documented payload, exit code 2 blocks with stderr as the reason, and a timeout blocks | `TestCommandHookContract` | not built |
| `progressive` on a host without a sandbox is refused with `sandbox_unavailable` | `TestProgressiveNeedsASandbox` | not built |
| `remember` adds a pattern that a fresh runner applies to the next matching call | `TestRememberedPatternSurvivesRestart` | not built |
