---
title: "The client, the topos command and the agent skill: the API client, print mode, the supported import set"
status: drafted
track: core
depends_on: [002-scaffold-and-configuration.md, 004-session-log.md, 005-harness-loop.md, 016-runners.md]
affects: [client/, cmd/topos/, internal/toposcli/, skills/topos/, examples/embed/]
effort: medium
created: 2026-09-27
updated: 2026-10-02
author: changkun
---

# The client, the topos command and the agent skill

## Overview

Three things a developer or an agent uses to drive the core from
outside. `client` is the typed client of toposd's API, and the
`session.Store` an external runner appends through. `topos` is the
core's scripting and test command: it runs a session in print mode in
the working directory with no server, attaches to a server's session,
sends to it, answers a confirmation, and applies manifests; it has no
interactive terminal. `skills/topos/SKILL.md` teaches an agent to use
`topos`. This spec also names the packages an embedder may import, and
holds the examples to that set. The local half is phase 1; the client
half phase 2.

## Current state

v0.7.0 had no command; its root package was the supported surface and
was bypassed by every consumer, and each consumer copied wire structs
by hand. The retired laptop client ran a local loop that kept nothing,
with no permission prompt and no interrupt. Nothing is borrowed; the
shape of `client` follows the clients of Lux and Cella: one method per
route, the response bytes as they arrived, and one dialed address.

## Design

### The client package

| Rule | Value |
|---|---|
| construction | `client.New(baseURL string, ts TokenSource, opts ...Option)`; the base URL includes a base path when one is set ([[030-shared-origin]]) |
| methods | one per route of [[015-api]] and [[017-external-runners-handoff-fork]], returning the decoded value and the response bytes |
| paging | `next_cursor` followed by an iterator; one call is one request |
| errors | the `latere.ai/x/pkg/httpjson` envelope decoded into `*client.Error` with `Code`, `Message`, `Detail`, `Status` |
| bearer | read from the `TokenSource` on every request |
| retries | none in the client, except that a stream reconnects from its last sequence |
| dials | the base URL only |

`client.Store` implements `session.Store` over the API for an external
runner: `Append` is the append route, `Watch` merges the event stream
with the inbox, blobs go through the blob route, and `Acquire` succeeds
when the caller is the session's writer and its `Lost` channel fires
when a handoff takes the write away ([[017-external-runners-handoff-fork]]).
It passes `session/storetest` against a test server.

### The topos command

| Command | Does |
|---|---|
| `topos run [<prompt>]` | runs a turn: with `TOPOS_URL` unset, a local session in the working directory over the directory store; with it set, a server session. Reads the prompt from stdin when none is given |
| `topos run --session <id> [<prompt>]` | continues a session with another message |
| `topos confirm <session> <tool_use_id> allow\|deny [--note <text>] [--remember <pattern>]` | appends `user.tool_confirmation` and continues the turn |
| `topos attach <session> [--from-seq <n>]` | streams a server session's events, replay then live, until it is idle |
| `topos send <session> <text>`, `topos interrupt <session>` | appends a `user.message` or a `user.interrupt` |
| `topos apply -f <file>` | resolves and applies manifests ([[003-manifest]]) to the server, or to the local state when `TOPOS_URL` is unset |
| `topos sessions list`, `show <id>`, `prune` | lists and shows sessions; `prune` removes the worktrees and directories of ended sessions ([[009-machines]]) |
| `topos fork <session> [--at <seq>] [--dir <path>]` | [[017-external-runners-handoff-fork]] |
| `topos rewind <session> <turn>` | [[034-checkpoints-and-rewind]] |

`topos run` takes `--agent <file or name>`, `--model <name>`,
`--mode plan|confirm|progressive`, `--max-cost <usd>`, `--dir <path>`
and `--output text|json|stream-json`. Without `--agent` it uses
`$XDG_CONFIG_HOME/topos/agent.yaml` when present, otherwise a built-in
agent with every built-in tool, `confirm` mode, and an egress list of
the common package registries; a run with no model from any of these is
a usage error `no model: pass --model or --agent`. The model connection
is `TOPOS_MODELS_URL` and `TOPOS_MODELS_KEY` ([[002-scaffold-and-configuration]]).

Output: `text` prints the session's final agent text to stdout and one
line per tool call to stderr, `<tool> <summary> [<verdict>]`; `json`
prints one object with `session_id`, `status`, `stop_reason`,
`detail`, `text` (the final agent text) and `pending` (the `tool_use` ids
waiting for a confirmation); `stream-json` prints every appended
event as one JSON line, and deltas when `--deltas` is set. When the
turn stops at an ask, `topos run` prints the pending calls and exits 3,
and `topos confirm` continues it. The first `SIGINT` appends
`user.interrupt` and waits for the turn to stop; a second exits at
once, which releases the lock.

| Exit code | The turn ended |
|---|---|
| 0 | `end_turn`, or the session ended `completed` |
| 1 | `error`, or the session ended `failed`, or a failure of the command |
| 2 | usage |
| 3 | waiting for a person: `tool_confirmation` or `tool_result` |
| 4 | at a limit: `budget`, `turn_limit` or `output_limit` |
| 5 | `interrupted` |

### The skill

`skills/topos/SKILL.md` is an Agent Skill ([[011-instructions-and-skills]])
that teaches an agent to run a subtask with `topos run --output json`,
to read the exit code, to answer or report a pending confirmation, and
to apply a manifest. Its commands are checked against the command's
flag table by test.

### The supported import set

An embedder imports `harness`, `harness/tools`, `runner`, `session`,
`session/dir`, `machine`, `machine/host`, `machine/cella`, `models`,
`models/dialect`, `manifest/v1` and `client`, and never a package under
`internal/`. The programs in `examples/` use only this set, and
`examples/embed` runs a session in process with a machine and a model
it constructs.

## Not in this spec

The routes ([[015-api]]); the runner that `topos run` drives
([[016-runners]]); the interactive terminal a person uses, which is a
client built on this package outside the module.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `topos run` runs a turn of a local session in the working directory with no server against the stub Lux: the model's `read` reaches the file, the answer is on stdout, each tool call is a line on stderr, the credential is `TOPOS_MODELS_KEY`, and `--session` continues the session with `--output json` | `internal/toposcli.TestRunATurnInTheWorkingDirectory` | built |
| `topos run` runs a task of the suite end to end with no server, against the stub Lux, and exits 0 with the checker passing | `TestCLIRunsASuiteTaskWithNoServer` in the e2e tier | not built |
| The prompt is read from stdin when none is given, and a scripted connection runs a scripted session | `internal/toposcli.TestAPromptFromStdin`, `internal/toposcli.TestAScriptedRun` | built |
| Each exit code of the table is returned for its stop reason, and the data directory follows `TOPOS_DATA_DIR`, then `XDG_STATE_HOME`, then the home directory | `internal/toposcli.TestExitCodes` | built |
| A usage error (no command, an unknown one, a bad flag, mode, output or cost, no prompt, no model, no model URL, a server URL, missing confirm arguments) exits 2; an unknown model, a missing session and a lost stdout exit 1 | `internal/toposcli.TestUsageErrors`, `internal/toposcli.TestLostOutputExitsOne`, `cmd/topos.TestNoCommandIsAUsageError`, `cmd/topos.TestAnUnknownCommandIsAUsageError`, `cmd/topos.TestVersionPrintsTheIdentity` | built |
| An ask stops `topos run` with exit 3 naming `topos confirm`, and `topos confirm` continues the same turn; a denial with a note runs nothing and the turn continues | `internal/toposcli.TestAConfirmationRoundTrip`, `internal/toposcli.TestConfirmPathsAndDenial` | built |
| `--max-cost` below one step's cost stops the run at the budget with exit 4 | `internal/toposcli.TestStreamJSONAndLimits` | built |
| Two SIGINTs exit at once with exit 5 | `internal/toposcli.TestTwoInterruptsExitAtOnce` | built |
| The first SIGINT alone ends the turn `interrupted` and leaves the session resumable | `TestCLIFirstInterruptEndsTheTurn` | not built |
| Each line `--output stream-json` prints is one appended event | `internal/toposcli.TestStreamJSONAndLimits` | built |
| `--output stream-json` prints every appended event exactly once, in sequence | `TestCLIStreamJSONIsCompleteAndOrdered` | not built |
| `topos rewind` restores a turn's files from the command | `internal/toposcli.TestRewindFromTheCommand` | built |
| `attach`, `send`, `interrupt`, `apply`, `sessions` and `fork` work as the command table says | one test per command in `internal/toposcli` | not built |
| `run --agent <file>` runs the file's first Agent ([[003-manifest]]) with its model, instructions, tools, mode, subagents, budget and limits, and `--session` and `topos confirm` continue the session with the same agent from its bundle blob without the file; a bundle that does not match its digest, or is gone, stops the run | `internal/toposcli.TestRunAnAgentManifest`, `internal/toposcli.TestTheManifestModeHoldsAcrossAConfirmation`, `internal/toposcli.TestAnAgentSpawnsItsSubagent` | built |
| `--model` and `--mode` replace the agent's `spec.model` and mode | `internal/toposcli.TestTheModelFlagReplacesTheManifestModel`, `internal/toposcli.TestTheManifestModeHoldsAcrossAConfirmation` | built |
| Without `--agent`, `$XDG_CONFIG_HOME/topos/agent.yaml`, or `$HOME/.config/topos/agent.yaml` when `XDG_CONFIG_HOME` is unset, runs when present, and the built-in agent otherwise | `internal/toposcli.TestTheDefaultAgentManifest` | built |
| A refused manifest, a missing file, `--agent` with `--session`, a file with no Agent, and a field a local run does not apply yet (hooks, client tools, output limits, the advisor, skills, MCP servers, memory stores, connections, repositories, a Cella machine) exit 2 before a session is created | `internal/toposcli.TestAgentManifestRefusals` | built |
| `run --agent <name>` runs an agent applied to the local state | `TestRunAnAppliedAgentByName` | not built |
| `topos apply` resolves a manifest's references through the local state's `Lookup` when `TOPOS_URL` is unset and sends the server a manifest it can resolve (instructions inlined) when it is set, and an agent applied both ways pins the same stored objects at the same versions | `TestApplyResolvesLikeTheServer` | not built |
| `client.Store` passes `session/storetest` against a test toposd | `TestClientStoreConformance` | not built |
| A client error decodes into `*client.Error` with the envelope's code and detail | `TestClientDecodesErrors` | not built |
| The examples import only the supported set | `TestExamplesImportOnlyTheSupportedSet` | not built |
| Every command in the skill exists in the command's flag table | `TestSkillCommandsExist` | not built |

## Outcome

Shipped as designed in v0.9.0 (2026-09-29): `topos run`, `topos confirm`
and `topos rewind` over the directory store with no server, the exit
codes, `--output stream-json`, `--max-cost`, and `--agent` with a
manifest file or the default `agent.yaml`. v0.11.0 (2026-10-02):
`--mode` also takes `manual` and `auto`, and `topos run` consults a
decision service ([[037-decision-services]]). The names `manual` and
`auto` are an addition to the command table, which lists the three modes
of [[012-permissions-and-approvals]].

Open: the `client` package, the commands `attach`, `send`, `interrupt`,
`apply`, `sessions` and `fork`, an applied agent run by name, a suite
task run end to end from the command, the first SIGINT alone,
stream-json's completeness, the examples and the agent skill.

The status stays `drafted` while [[005-harness-loop]] and
[[016-runners]] are open, since the gate starts no spec before its
dependencies close.
