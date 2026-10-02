---
title: "Permissions, approvals and hooks: the boundary, the layers, the risk score and verdict, the modes"
status: drafted
track: core
depends_on: [001-architecture.md, 004-session-log.md, 005-harness-loop.md, 008-tools.md, 009-machines.md]
affects: [harness/, machine/host/]
effort: large
created: 2026-09-27
updated: 2026-10-02
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
| a Cella sandbox | the sandbox: its files are disposable, it holds no credential, its network leaves only through Cella's egress gateway | inside, `bash` runs freely; outside, typed points: the egress allowlist and credential substitution (Cella), the agent's permissions and the session scope at every core (the installation's authorizer), the git host's ref rules ([[019-git]]), the scope each of the sandbox's tokens is minted with, which leaves out every action that cannot be undone, and the step-up below, which asks before one is granted |
| a server's host ([[009-machines]]) | the host sandbox, mandatory, around each command, with a directory of the session's own and none of the server's environment | as on a person's host; the sandbox holds between the sessions of different people as well |
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
   merged from the authorizer's `limits` in the decision at session
   start and the agent's `spec.approvals`, so neither side can loosen
   the other: `always_confirm` is the union of the two, `always_allow`
   the intersection when both give one and the one given otherwise,
   the mode the stricter, and each threshold the lower. The merged
   policy is the Session's `policy` ([[004-session-log]]), so every
   runner applies the same lists. Read-only tools are on
   `always_allow` unless a list says otherwise. `always_confirm` wins
   over `always_allow`.
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
| effect `write` inside a Cella machine, by a file tool, or by `bash` in a sandbox that holds no swapped-in credential | 0.1 |
| `bash` in a Cella machine whose sandbox holds a swapped-in credential toward a core or another host, since its commands can act outside the machine through it | 0.4 |
| an `approval.requested` for an action a core flags as irreversible, or for a request matching an egress pattern | 0.9 |
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
| `confirm` | on `always_allow`, a session pattern, or effect `none` or `read`: `allow`; a call inside a Cella machine, `bash` included: `allow`, since the step-up below bounds what it can do outside; otherwise `ask` |
| `progressive` | score below `flag_at` (0.3): `allow`; below `ask_at` (0.5): `flag`; below `block_at` (0.9): `ask`; otherwise `block` |

A person names `confirm` manual and `progressive` auto, the names other
harnesses use for the same two experiences: `topos run --mode` takes
`manual` and `auto` and records `confirm` and `progressive`, which stay
the only names in a manifest, an event, and the API. With a decision
service ([[037-decision-services]]), manual mode is how it learns, every
answer a label, and auto mode is where its suggestions decide inside the
rules.

A call on `always_confirm` is `ask` in `confirm` and `progressive` and
`block` in `plan`. The thresholds come from the authorizer's `limits`
or the agent's `spec.approvals.thresholds`, and a learned source may
later set them per person and per agent. The order of evaluation is:
hard boundary, `always_confirm`, the mode, then hooks. Verdicts are
ordered allow, flag, ask, block from most to least permissive, and the
final verdict is the least permissive of the mode's and every hook's.
`agent.tool_use` records the verdict, the reason, and the mode. A
block's reason is also the result the model reads, so the reasons a
block gives are files of `prompts/results/call/`
([[011-instructions-and-skills]]).

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
one reads the log: when a `session.status` `running` other than its
own claim follows the confirmation, an earlier runner may have started
the call, and it is closed with `unknown_effect` ([[016-runners]]);
when only its own does, the call never started and runs. A confirmed
call for a tool the registry no longer has is answered `unknown_tool`.

### Step-up at the egress gateway

A command in a Cella sandbox reaches other services only through
Cella's egress gateway, which swaps in the credential for each
destination ([[018-credentials-and-secrets]]). A classifier reads the
command's text and cannot see what a script it runs does, so what the
command can do outside is bounded by scope, and a step-up asks before
the scope widens. The mechanism is the same for every destination; no
tool exists per action.

1. **Flagged actions.** Each core's action vocabulary flags the actions
   that cannot be undone: force-updating or deleting a ref, deleting an
   object, a sandbox or a session, a production deploy. The authorizer
   mints each of a sandbox's tokens for one destination, within the
   session's scope and without the flagged actions.
2. **Egress patterns.** For third-party hosts, whose credentials carry
   no such scope, `always_confirm` takes egress patterns
   `egress(<METHOD> <host><path glob>)`, for example
   `egress(DELETE api.stripe.com/**)`, which Cella's egress matches on
   every request that carries a swapped-in credential.
3. **Refusal.** A request that needs a flagged action is refused by the
   core, and one matching an egress pattern by Cella's egress, with
   HTTP 403 and the code `confirmation_required`. Cella records the
   refusal on the sandbox: the destination, the action and resource or
   the method, host and path, and the time.
4. **Request.** After each `bash` call, and while a background job
   runs, the runner reads the sandbox's new refusals and appends one
   `approval.requested` for each, attributed to the call that caused
   it, scored and given a verdict by the layers above. `allow` (an
   `always_allow` pattern that names it) asks the authorizer for the
   grant at once; `ask` sends the session idle with
   `tool_confirmation` once the call's result is in; `block` records
   it, and the model reads that the request stays refused.
5. **Grant.** A `user.tool_confirmation` naming the `approval_id`
   answers it. On `allow` the runner asks the authorizer for a one-shot
   grant of exactly that action on that resource, or that method, host
   and path, on the sandbox's token, lasting at most five minutes and
   consumed by the first request it admits, and appends
   `approval.decided`; the model reads that it may retry. On `deny`
   the model reads the note.

An approval is as durable as an ask: nothing times it out, and a
runner that claims the session after a restart reads the pending
approvals from the log. Without an authorizer nothing can grant a
step-up: a sandbox holds what the operator's Cella secrets allow, and a
request refused `confirmation_required` stays refused.

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
| `grant_failed` | the authorizer refused or could not issue an approved step-up grant; `session.error`, and the model reads that the request stays refused |

## Not in this spec

The agent's permissions, the session scope and key narrowing
([[006-identity]], [[018-credentials-and-secrets]]); the git host's ref
rules ([[019-git]]); the deny-list's entries ([[009-machines]]); where
the lists live in the authorizer's decision ([[006-identity]]); the
console that answers an ask. Outside this repository: the flags on each
core's irreversible actions (each core's vocabulary); Cella's egress
matching egress patterns, swapping credentials by host and path, and
recording refusals on the sandbox for its creator to read; and the
authorizer's one-shot grant.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A `bash` call on the host cannot write outside the working directory or read a deny-listed credential file in any mode, including when a person allowed the call and when an allow pattern matches it | `TestHostSandboxConfinesBash`, one subtest per mode and mechanism available on the runner | not built |
| The rule-feature score of each row of the score table, with source `rules/1` and its features, including the command-text hint and the egress host of `web_fetch` | `harness.TestScoreFollowsTheRuleFeatures` | built |
| A tool call's `agent.tool_use` carries its score, its source and its verdict | `harness.TestConfirmationsAndDenials` | built |
| Each mode gives the table's verdict, patterns match by tool and glob (a single `*` stays in a path segment, `domain:` matches a fetch host), `always_confirm` asks in `confirm` and `progressive` and blocks in `plan`, and the thresholds move the progressive bands | `harness.TestDecideAppliesTheModeAndTheLists`, `harness.TestGlobRegexpQuotesItsText`, `harness.TestPlanModeBlocksWrites` | built |
| Verdicts are ordered allow, flag, ask, block, and the stricter of two is the less permissive | `harness.TestStricterOrdersVerdicts` | built |
| No hook and no mode can raise a verdict: a hook answering `continue` to a blocked call leaves it blocked, and a hook that replaces the input gets the new input scored again | `TestHookCannotWiden`, `TestHookReplacedInputIsRescored` | not built |
| An ask pauses the session idle `tool_confirmation`, and a claim with no answer keeps it waiting and sends no request | `harness.TestAnUnansweredAskKeepsWaiting` | built |
| An ask holds no lease and never times out into a deny | `TestAskIsDurableAndNeverDenies` | not built |
| A confirmation survives a runner restart: the confirmed call runs exactly once when no runner started it, is closed `unknown_effect` when one may have, a denied call is answered `denied` with the note, and a confirmed call for a tool that is gone is `unknown_tool` | `harness.TestConfirmationsAndDenials`, `harness.TestAConfirmedCallAnEarlierRunnerMayHaveStarted`, `harness.TestAConfirmedCallForAToolThatIsGone` | built |
| A command hook receives the documented payload, exit code 2 blocks with stderr as the reason, and a timeout blocks | `TestCommandHookContract` | not built |
| `progressive` on a host without a sandbox is refused with `sandbox_unavailable` | `harness.TestProgressiveNeedsASandbox`; the CLI refuses it on a Windows host, which records `sandbox: none` ([[009-machines]]); a Linux or macOS host, whose sandbox the CLI does not yet apply, records no driver and is not refused | built for Windows hosts |
| `remember` adds a pattern that the next turn, on a fresh harness, applies to the next matching call | `harness.TestConfirmationsAndDenials` | built |
| The authorizer's and the agent's lists merge so neither loosens the other: confirm is the union, allow the intersection, thresholds the lower, and the merged policy is in the Session | `harness.TestPolicyMergeNeverLoosens`, `internal/server.TestLimitListsReachThePolicy`, `harness.TestTheSessionsPolicyDecides` | built |
| A sandbox command refused `confirmation_required` yields one `approval.requested` scored 0.9; an allow grants a one-shot scope the retried request uses once, a deny leaves it refused, and a restart between the two keeps the approval pending | `TestEgressStepUp` over the stub Cella and a stub authorizer | not built |
| `bash` in a sandbox holding a swapped-in credential scores 0.4 | `harness.TestScoreFollowsTheRuleFeatures` | not built |

## Outcome

Shipped in v0.9.0 (2026-09-29): the rule-feature score `rules/1`, the
four verdicts under `plan`, `confirm` and `progressive`, the lists and
thresholds merged with the authorizer's, confirmations that survive a
restart, and `remember`. A Windows host records `sandbox: none` and
refuses `progressive`. v0.11.0 (2026-10-02) decides every call through
the `Decider` of [[037-decision-services]], and the verdicts are those
of `latere.ai/x/pkg/verdict`.

Open: the operating-system sandbox on a macOS or Linux host (its driver
is in `machine/host`, and `topos` does not apply it yet), hooks, an ask
that holds no lease, the egress step-up, and the score of a sandbox that
holds a swapped-in credential.

The status stays `drafted` while [[005-harness-loop]] and [[008-tools]]
are open, since the gate starts no spec before its dependencies close.
