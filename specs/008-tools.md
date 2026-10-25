---
title: "Tools: the built-in set, schemas and descriptions, paths, output caps and spill files, the repeat rule"
status: in-progress
track: core
depends_on: [001-architecture.md, 004-session-log.md, 009-machines.md]
affects: [harness/tools/, prompts/tools/, prompts/results/, test/tasks/instructions/]
effort: large
created: 2026-09-27
updated: 2026-10-25
author: changkun
---

# Tools

## Overview

A tool is a definition the model sees (a name, a description and a
JSON Schema for its input), properties the harness reads, and code that
acts through the session's machine. This spec owns the `Tool`
interface, the registry, the built-in set (`read`, `write`, `edit`,
`bash`, `grep`, `glob`, `web_fetch`, `todo`), client-executed tools,
how paths resolve, the output cap and spill files, the outcomes a
result can carry, and the rule that a call with no result is never run
again. Each description is a file that an instruction test measures.

## Current state

v0.7.0 spec 001 shipped `bash`, `read_file`, `write_file`,
`edit_file`, `grep` and `glob`; v0.7.0 spec 031 selected them by
family (`read`, `write`, `exec`) and made the offered registry and the
dispatch agree, which [[013-threads-and-subagents]] keeps as narrowing.
Two probe failures were here: the file tools promised absolute paths
and the local sandbox re-rooted them, and `bash` ran with a fixed
`PATH` and `HOME=/tmp`, so `go` was not found. No tool capped its
output and no description was ever measured.

## Design

### The interface

```go
type Tool interface {
	Definition() Definition // Name, Description, InputSchema
	Properties() Properties
	Run(ctx context.Context, c Call) (Result, error)
}

type Properties struct {
	Parallel    bool   // may run beside other parallel calls in a step
	Repeatable  bool   // built-ins only; see the repeat rule
	Effect      Effect // EffectNone, EffectRead, EffectWrite, EffectExternal
	Client      bool   // executed by the client, not the runner
	OutputLimit int    // bytes; 0 means the default
}
```

A `Call` carries the `tool_use` id, the validated input, the machine,
the thread's fold (read only), and a spill writer. `Run` returns
content blocks, an error flag and an outcome; a Go error is reserved
for a failure of the harness and becomes the outcome `error` with the
message. `Effect` is the input to [[012-permissions-and-approvals]]'s
risk features: `read` and `none` stay on the machine and change
nothing, `write` changes the machine, `external` reaches outside it.

### The built-in set

| Name | Parallel | Effect | Input | Limits |
|---|---|---|---|---|
| `read` | yes | read | `path`, `offset` (first line, from 1), `limit` (lines, default 2000) | lines over 2000 characters are cut; PNG, JPEG, GIF and WebP up to 5 MiB return an image block; a binary file or a directory is an error |
| `write` | no | write | `path`, `content` | creates parent directories; an existing file must have been read or written by this thread at its current content |
| `edit` | no | write | `path`, `old_string`, `new_string`, `replace_all` (default false) | `old_string` must occur exactly once unless `replace_all`; the same current-content rule as `write` |
| `bash` | no | write | `command`, `timeout_ms` (default 120000, at most 600000), `background` (default false), `description` | stdout and stderr combined in order, the exit code; on timeout the process group is killed |
| `grep` | yes | read | `pattern` (RE2), `path`, `glob`, `output_mode` (`files_with_matches` default, `content`, `count`), `context` (lines), `case_insensitive`, `multiline`, `head_limit` (default 250) | honors `.gitignore`, skips `.git` and binary files |
| `glob` | yes | read | `pattern` (`**` matches any depth), `path` | newest first by modification time, at most 1000 paths |
| `web_fetch` | yes | external | `url` (`http` or `https`) | 30 second timeout, 5 redirects, 10 MiB read; HTML converted to text |
| `todo` | yes | none | `todos`: a list of `{id, content, status}`, status `pending`, `in_progress` or `completed` | replaces the thread's list; at most 100 items |

The harness adds `question` to the session's own thread when an agent
names it, a tool of its own and not a built-in, so the default set
stays the table's eight ([[039-questions]]). The hosted runner adds
`publish` the same way, to the session's own thread of a session in a
sandbox, when the installation has an app host
([[043-publishing-a-folder]]). The harness adds `request_connection` to
the session's own thread while the session has a service the person
may be asked to connect ([[069-connections-an-allow-names]]).

`web_search` is a built-in an agent holds only by naming it
(`tools.OptIn`), so it is in no default set ([[047-web-search]]):

| Name | Parallel | Effect | Input | Limits |
|---|---|---|---|---|
| `web_search` | yes | none | `query` (1 to 400 characters), `max_results` (1 to 10, default 5) | the search service's 30 second timeout and 1 MiB answer; at most 300 characters of a title and 1,000 of a snippet |

`history` is one too, run by the harness over the calling thread's own
log, so it never opens a machine ([[084-reading-back-what-the-context-let-go]]):

| Name | Parallel | Effect | Input | Limits |
|---|---|---|---|---|
| `history` | yes | none | one of `from` and `to` (from 1, either optional), `tool_use_id`, or `query` with `from` and `to` | whole entries within the output limit and never spilled; a query answers at most 20 matches of 400 characters |

`grep` and `glob` are Go-native and need no external binary. A
result's `meta` ([[004-session-log]]) is the tool's record for later
calls of its thread, and `tools.StateOf` folds the thread's metas from
the log into the `State` each call receives: `{path, sha256}` from
`read`, `write` and `edit`, `{dir, exit_code}` from `bash`, `{todos}`
from `todo`. The current-content rule of `write` and `edit` reads that
state, not memory: a write proceeds only when the file's current hash
equals the last one this thread recorded for that path, or the file
does not exist. Otherwise the result is
`<path> changed since it was last read; read it again before writing.`

`bash` keeps a persistent working directory: each result records the
shell's final directory, taken with `pwd -P` written to a separate file
descriptor so output is not mixed, and the next `bash` call of the
thread starts there, read from the log. Environment variables do not
persist between calls. A `background` call starts the command detached
in its own process group, writes its output to a job log in the spill
directory, and returns at once with the pid and the log's path; the
model reads the log with `read` and stops the job with `bash`. The
machine kills background jobs when the session ends.

A command that ends with exit code 137 was killed, and its result says
so after the code: that a machine out of memory kills a process this
way, that nothing says this is why, and that the machine still runs
with its files. A command whose machine stopped under it
([[009-machines]]) has no exit of its own: its result is `error`, with
what it wrote, that it did not finish, that the machine ran out of
memory only when its provider said so, and where the machine stands:
running again with the files of the working directory kept and every
process gone, so the command can be run again; lost, with its files
out of reach; or not running yet, with the reason, nothing deleted and
the next command starting it. The texts are
`prompts/results/bash/killed-v1.md` and
`prompts/results/bash/machine-stopped-v1.md`. A foreground
command still running after 3 seconds whose process group listens on a
port it chose, a server, is moved to the background the same way and
the call says so ([[045-a-server-cannot-hold-a-turn]]).

`web_fetch` runs inside the machine's network boundary, never from the
runner's own network. `harness/tools` dials nothing: the tool reaches
the network only through `machine.Fetcher`, an optional interface a
machine implements, whose `Fetch` enforces the limits above (30
seconds, 5 redirects to `http` or `https` only, 10 MiB). The host's
fetch runs from the host's own network, which the host sandbox's
network rule governs ([[012-permissions-and-approvals]]); a Cella
machine's runs in the sandbox, whose egress Cella's gateway decides. A
machine that does not implement `Fetcher` has no web access, and the
call's result says so.

Memory sync is a built-in tool too, `memory_sync`, defined with its
stores in [[020-memory-stores]]; subagents are started and continued
with `agent` and `send_message`, built as `spawn` and `message`, and
the advisor is `advisor`, all from [[013-threads-and-subagents]].

### Client-executed tools

An agent may declare a tool with `client: true` ([[003-manifest]]): a
name, a description and an input schema, and no code in the runner.
The harness validates and scores its calls like any other, appends the
`agent.tool_use` with `client` true, and stops idle with `tool_result`;
the client appends `user.tool_result` for each, and the turn resumes.
Nothing times out: the wait is durable.

### Paths

Every path is the machine's own. An absolute path is used as given and
is never re-rooted. A relative path resolves against the session's
working directory for the file tools, and against the persistent
directory for `bash`. The host confines the file tools to the roots of
[[009-machines]]; a path outside them is refused with
`<path> is outside the working directory.`

### Output cap and spill files

A result's text is capped at the tool's output limit, 32 KiB by
default and set per tool in the agent's `spec.tools[].outputLimit`
([[003-manifest]]). Past the cap, the whole output is written to a file
in the machine's spill directory, which is outside the working
directory so that checkpoints and git never see it and the file tools
can read it, and the result carries the first 20 KiB, the line
`[... <n> bytes omitted; the full output is in <path> ...]`, and the
last 10 KiB. The `tool.result` records `spill` with the path and the
full size.

### Outcomes

| Outcome | Is error | Meaning |
|---|---|---|
| `ok` | no | the tool ran and succeeded; `bash` with a non-zero exit is `ok` with the code in the text |
| `error` | yes | the tool ran and failed |
| `timeout` | yes | the call passed its timeout and was killed |
| `unknown_tool` | yes | no such tool ([[005-harness-loop]]) |
| `invalid_input` | yes | the arguments were not JSON, or the input did not match the schema ([[005-harness-loop]]) |
| `denied` | yes | a person denied the call; the note is in the text |
| `blocked` | yes | the verdict was block; the reason is in the text |
| `canceled` | yes | an interrupt or a shutdown canceled the call while it ran |
| `unknown_effect` | yes | the runner stopped while the call ran; the text says its effects are unknown and to inspect the machine |
| `unanswered` | no | a `question` call no option was chosen in: every entry of the answer empty, a message in place of an answer, or a session nobody attends; the model decides ([[039-questions]]) |

Result text is written for the model: what happened, in one or two
sentences, and what to do next when there is something to do.

### The repeat rule

A call whose `agent.tool_use` is durable and whose `tool.result` is
not is never run again. A resumed runner closes it with
`unknown_effect` ([[016-runners]]) and the model inspects the machine,
which it does well (`git status`, reading the files it was writing).
`Repeatable` is the one exception and is a property of built-ins that
are idempotent by construction; the only one is `memory_sync`, whose
writes carry version preconditions. There is no effect classification
for tool authors, and MCP and custom tools are never repeated: the
registry refuses to register a non-built-in with `Repeatable` set.

### Descriptions and instruction tests

Each built-in's description is the file `prompts/tools/<name>-v<N>.md`,
embedded in the build with every other text a model reads
([[011-instructions-and-skills]]). The texts a tool writes into its
results, such as the current-content refusal, the refusal of a path
outside the roots and the spill line above, are files of
`prompts/results/`: templates rendered with the values they name, the
path, a count, a limit. A description or a result text changes wording
as a new version of its file, never as an edit of a released one. Every
description has at least one instruction test in
`test/tasks/instructions/<name>/`: a small task whose checker passes
only when the model used the tool as the description says (an `edit`
with a unique `old_string`, a `grep` with `output_mode` `content`, a
`bash` call that uses `background` for a server). The tests run against
a real model in the task suite's instruction tier ([[025-task-suite]]);
a change to a description ships with the tier's result for that tool.

### Names a model reads

A model chooses and calls a tool by its name, its inputs' names and its
description, and reads names it already knows more reliably. A new
tool, or a tool renamed, is named by these rules, in order:

1. Where Claude Code has a tool for the same job, its name, in this
   core's lowercase with words joined by `_`: `Agent` is `agent`,
   `Bash` is `bash`, `Read`, `Edit`, `Write`, `Grep` and `Glob` are
   `read`, `edit`, `write`, `grep` and `glob`, `WebFetch` and
   `WebSearch` are `web_fetch` and `web_search`, and `SendMessage` is
   `send_message`. Its input names are reused where they fit, as
   `send_message` takes `to` and `message`.
2. Otherwise, a plain verb for what the tool does, with its object
   where the verb alone is unclear (`publish`, `notify`, `wake`,
   `list_sessions`, `run_workflow`).
3. Never an operating-system word, in a name, an input or a
   description: spawn, thread, kill, reap, daemon, lease, lock, mutex,
   inbox, writer, fold, orchestrator, one-shot, a held call. Each names
   how a machine works, not what the tool does for the agent. A tool
   whose subject is the machine names the command a model types in the
   machine's words: `bash` says to stop a background job with
   `kill -TERM -<pid>`, and that a command past its timeout is stopped.

`todo` (Claude Code's `TodoWrite`) and `question` (its
`AskUserQuestion`) shipped before this rule and keep their names, as do
`history`, `advisor` and `trigger`. A renamed tool's old name keeps
working for one release, as [[013-threads-and-subagents]] keeps `spawn`
and `message`. A word of the log or the API that stays
(`thread.started`, a Session's `writer`) is explained where its spec
names it, and no text a model reads uses it. The descriptions
`read-v2` and `read-v3`, `write-v2`, `edit-v2`, `bash-v2` and `todo-v1`
said "this thread"; their next versions, `read-v4` and `read-v5`,
`write-v3`, `edit-v3`, `bash-v3` and `todo-v2`, say "you", as
`results/files/unread-v2`, the refusal `write` and `edit` quote, does.

### Texts a transcript renders

A text of `prompts/transcript/` renders an event of the log into the
transcript ([[004-session-log]]), at every request of the session, the
oldest event included. A new version of one therefore changes the words
of every past turn the model already read, and the prefix a provider
cached, where a new version of a tool's description changes only the
definitions. So a new version of a transcript text applies to sessions
created after the release that ships it, and a session renders the
versions it was created with for its whole life:

- The Session's header records `transcript`, the set of transcript text
  versions it renders, written at create; a session without it renders
  set 1, the texts every session renders today. A text chosen by the
  session's state rather than its age, as `cleared-v2` and `summary-v2`
  are for a thread that holds the history tool
  ([[084-reading-back-what-the-context-let-go]]), stays chosen so in
  every set.
- A session fork ([[017-external-runners-handoff-fork]]) and a
  trigger's fork of a conversation take their parent's set, so the
  history they copy reads as it did, and a tree's sessions share their
  cache.
- A replay renders a request at the session's set, as it renders the
  harness prompt at the version its `model.request` names.

The first text this rule holds back is `transcript/thread-message-v1`,
`Message from thread <name>:`, which heads a message a subagent
receives; its next version says the sender in plain words, in sessions
created with set 2. The harness prompt is not held back this way: a
build sets its version for every new request, and a session's prefix
changes with it once ([[011-instructions-and-skills]]).

## Not in this spec

Validation, grouping of parallel calls and cancellation
([[005-harness-loop]]); verdicts and the host's network rule
([[012-permissions-and-approvals]]); the machine's file and exec
routes, the credential deny-list and the environment
([[009-machines]]); `memory_sync` ([[020-memory-stores]]); MCP tools
([[021-mcp-servers]]); `agent`, `send_message` and `advisor`
([[013-threads-and-subagents]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The built-in set has the table's names, parallel flags and effects, each description is embedded, and each decodes its input | `harness/tools.TestBuiltinsFollowTheTable`, `harness/tools.TestBuiltinsDecodeTheirInput` | built |
| An absolute path inside the working directory is read and written as given, never re-rooted | `harness/tools.TestFileToolsUseMachinePaths` | built |
| `read` numbers lines, cuts long lines, returns images, and refuses a binary file or a directory | `harness/tools.TestReadNumbersLines`, `harness/tools.TestReadCutsLongLines`, `harness/tools.TestReadImages`, `harness/tools.TestReadRefuses` | built |
| `write` and `edit` refuse a file changed since this thread last read it, with the hash taken from the log, and a fresh harness enforces the same rule | `harness/tools.TestWriteRequiresCurrentContent`, `harness/tools.TestStateOfFoldsMetas`, `harness.TestMetaIsRecordedAndFoldedIntoState` | built |
| `edit` refuses an `old_string` that occurs zero or two times without `replace_all` | `harness/tools.TestEditRequiresUniqueMatch` | built |
| `bash` keeps its directory between calls across a harness restart, keeps no variable, and kills its process group on timeout | `harness/tools.TestBashPersistentDirectory`, `harness/tools.TestBashTimeoutKillsGroup`, `harness/tools.TestBashCanceled` | built |
| A command whose machine stopped under it reads as one that did not finish, with its output, a cause only when the provider named one, and where the machine and its files stand; a command killed on a machine that runs on reads with exit code 137 and what the code means | `harness/tools.TestBashSaysWhatBecameOfAMachineThatStopped`, `prompts.TestEveryTextRendersItsCurrentBytes` | built |
| A `background` command returns at once with a pid and a log path, keeps running across steps, and is killed at session end | `harness/tools.TestBashBackgroundJob`, `machine/host.TestBackgroundJobsEndWithTheSession` | built |
| A foreground server moves to the background after the grace with its port, pid and log, and a listener the system placed stays in the foreground ([[045-a-server-cannot-hold-a-turn]]) | `harness/tools.TestBashMovesAServerToTheBackground`, `harness/tools.TestBashLeavesAListenerTheSystemPlacedInTheForeground` | built |
| `grep` and `glob` run with no external binary | `harness/tools.TestGrepNeedsNoBinary`, `harness/tools.TestGlobNeedsNoBinary` | built |
| An output of 100 KiB is spilled: the result carries 20 KiB of head, the omission line and 10 KiB of tail, and the spill file in the machine's spill directory holds all 100 KiB | `harness/tools.TestOutputSpill`, `harness/tools.TestCap` | built |
| `todo` replaces the thread's list and validates its items | `harness/tools.TestTodoReplacesTheList`, `harness/tools.TestTodoValidates` | built |
| `web_fetch` reaches the network only through the machine's `Fetcher`, converts HTML to text, spills a long page, and answers a failure, a timeout and a cancel as results; the host's fetch holds to the redirect, scheme, size and time limits | `harness/tools.TestWebFetch`, `harness/tools.TestWebFetchFailures`, `harness/tools.TestHTMLText`, `machine/host.TestFetch`, `machine/host.TestFetchRefuses`, `machine/host.TestFetchTimeout` | built |
| `web_fetch` on a Cella machine runs inside the sandbox and never from the runner's network | `machine/cella.TestFetchRunsInTheSandbox` | built against the stub Cella |
| A client-executed tool stops the turn with `tool_result` and resumes on `user.tool_result` | `harness.TestClientToolsWaitForTheirResult` | built |
| Registering a non-built-in tool with `Repeatable` fails, and the registry refuses a bad name or a duplicate | `harness/tools.TestOnlyBuiltinsAreRepeatable`, `harness/tools.TestRegistryRefuses` | built |
| Every built-in's description file has at least one instruction test directory | `test/tasks.TestEveryToolDescriptionHasAnInstructionTest` | built |
| Each instruction test's checker passes its scripted solution and refuses its scripted wrong solution by an assertion on the log, not on the files | `test/tasks.TestScriptedSolutions` | built |
| Each built-in's instruction test passes against a real model in the instruction tier | `test/tasks.TestTheSuiteAgainstAModel` with the `instructions` tag | not built |
| A new version of a transcript text renders only in sessions created with the set that names it; a session created before it, and a fork of one, render the old text at every request and replay | `session.TestATranscriptTextKeepsItsSessionsSet` | not built |

## Outcome

Shipped in v0.9.0 (2026-09-29): the registry and its schema check, the
eight built-ins with versioned descriptions, paths that are the
machine's, the output cap with its spill file, client-executed tools,
the repeat rule, and one instruction test per built-in with a scripted
right and wrong solution. On a Cella machine `web_fetch` runs `curl` in
the sandbox. One change from the plan: `write` and `edit` also refuse an
existing file the thread never read, with a result that says so, and
their descriptions are at version 2, which names both refusals.

Open: the instruction tier has run against a real model once, on
`anthropic/claude-haiku-5.5` on 2026-10-19, to compare two harness
prompt versions ([[080-device-machines]]): each built-in's test passed
every run of both but `bash` and `edit`. The suite pins no model yet,
so the last criterion has no recorded pass on the pinned model, and the
status stays `in-progress` until it has.
