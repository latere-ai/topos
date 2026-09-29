---
title: "Instructions and skills: the versioned harness prompt, the context block, project instruction files, Agent Skills"
status: drafted
track: core
depends_on: [004-session-log.md, 005-harness-loop.md, 009-machines.md]
affects: [harness/, runner/, prompts/, test/tasks/instructions/]
effort: medium
created: 2026-09-27
updated: 2026-09-30
author: changkun
---

# Instructions and skills

## Overview

What the model is told before the conversation: the harness prompt, a
versioned file that says how an agent works in this harness; the
context block, which says where it is (working directory, platform,
date, machine, git state); the agent's own instructions; the project's
instruction files, found from the working directory up to the
repository root; the index of the Agent Skills it may load; and a note
for each attached memory store. This spec owns the text each of these
renders to, the search order of instruction files, the skill sources,
and progressive disclosure, and the `prompts` tree that holds every
text a model reads as a versioned file. Their order and cache placement
are [[010-context]]'s.

## Current state

v0.7.0 sent no system prompt: the model was never told its working
directory, platform, date or path rules, and the only injected text was
the peer directory of v0.7.0 spec 009, which is retired (probe failure
4 of [[005-harness-loop]]). The retired laptop client sent a two-line
prompt and read no project instruction file. v0.7.0 bundled a skills
catalog of its own, which Agent Skills folders replace. Nothing is
borrowed.

## Design

### The prompts tree

Every text the core writes for a model to read is a file under
`prompts/`, embedded in the build by the `prompts` package, which
imports nothing else of the module ([[001-architecture]]). A text's
name is its path without the extension, and ends in its version:
`results/files/changed-v1`. A static text is plain; a dynamic one is a
`text/template` rendered with the values it names, a path, a count, a
branch, and a value it names that the caller does not pass fails the
render instead of rendering empty. Rendering reads nothing but the
embedded files and its arguments, with no clock and no map order, so
the same inputs give the same bytes on every run and a fold stays
byte-identical ([[004-session-log]]). Callers name each text by a
constant of the package, and a test checks every call: the file exists,
and the data holds exactly the keys the template reads.

| Directory | Holds |
|---|---|
| `harness/` | the harness prompt and its sections |
| `compact/` | the compaction prompt ([[010-context]]) |
| `advisor/` | the advisor's instructions and the request that carries the caller's transcript ([[013-threads-and-subagents]]) |
| `tools/` | the descriptions of the built-in tools and of `spawn`, `message` and `advisor` ([[008-tools]], [[013-threads-and-subagents]]) |
| `transcript/` | the texts the fold writes into a transcript ([[004-session-log]]) |
| `results/` | the results the harness, the tools and the thread tools write, by producer ([[005-harness-loop]], [[008-tools]], [[012-permissions-and-approvals]], [[013-threads-and-subagents]]) |
| `context/` | the context block, the instruction file wrapper and its cut lines, the skills index, and the memory note |

A change of wording is a new file with the next version, never an edit
of a released one, and every released version stays embedded so a
replay can rebuild an old request. The SHA-256 of every released file
is pinned in the package's tests, and each text's rendered bytes are
pinned beside it, so a change of wording shows as a test diff.

### The harness prompt

The harness prompt is `prompts/harness/harness-v<N>.md`, and its
version `harness/<N>` is the `prompt_version` of every `model.request`
([[004-session-log]]). Its conditional sections are files of their own,
`prompts/harness/<section>-v<N>.md`, and each version of the harness
prompt names the section versions it includes, so a released version
renders the same text for as long as it is embedded. A new version
merges only with the instruction tier's result against the previous
one ([[025-task-suite]]).

| Section | Says |
|---|---|
| the agent | it is an agent working through tools on a machine, for the people who send it messages, and each message says who sent it |
| working | read before editing; prefer `edit` for changes and `write` for new files; run the project's own tests; report what was done and what was not |
| paths | paths are the machine's own, absolute paths are used as given, relative paths resolve against the working directory |
| the machine | whether it is the person's host or a disposable sandbox, and what that means for destructive commands |
| permissions | a denied or blocked call is an answer, not an obstacle: do not route around it, say what was refused and ask |
| after a stop | a result with outcome `unknown_effect` means the runner stopped while the call ran: inspect the machine (for example `git status`) before repeating anything |
| threads | present only when the agent has subagents: what `spawn`, `message` and `advisor` do ([[013-threads-and-subagents]]) |
| memory | present only when a store is attached: memory is files in the named directories, read and written with the file tools |
| git | present only in a repository: commit on the session's branch, never push a protected branch |

The conditional sections render from `prompts.HarnessOptions`, set per
session: the machine's section from its kind, `threads` when the agent
has subagents, and `git` and `memory` by the runner from the log. The
git section renders when the latest `session.machine`'s context block
has a `Git` line, and the memory section when a `memory.attached`
exists, so a later machine outside a repository drops the git section
again. The compaction prompt is `prompts/compact/compact-v<N>.md`,
versioned the same way ([[010-context]]).

### The context block

Computed when a machine attaches and recorded as the `context` of
`session.machine`, so it is the same on every runner:

```
<context>
Working directory: /home/p/src/app
Platform: linux/amd64
Machine: host
Date: 2026-09-27
Git: branch agents/reviewer/ses_01J9Z3P9D2F6H8K0M2Q4S6U8W0 at 1a2b3c4, 2 modified, 1 untracked
Recent commits:
- 1a2b3c4 parse: reject empty input
</context>
```

| Line | Value |
|---|---|
| `Working directory` | the machine's working directory |
| `Platform` | `GOOS/GOARCH` of the machine |
| `Machine` | `host`, or `Cella sandbox` with the Environment's name |
| `Date` | the date the machine attached, in UTC |
| `Git` | absent outside a repository; the branch, the short HEAD, and counts of modified and untracked paths |
| `Recent commits` | up to five, short hash and subject |

The block is the template `prompts/context/context-v<N>.md`; the
instruction wrapper, its cut lines, the skills index and the memory
note below are templates of `prompts/context/` too.

A machine opened on demand is recorded only when a tool first acts on
it ([[009-machines]]), so until then the log holds no context block.
A session that names repositories ([[019-git]]) carries, in the context
block's place, a block the harness renders from the session's resources
on every request while no `session.machine` is recorded:

```
<context>
Repositories, cloned the first time a file or command tool runs:
- https://git.example/acme/web.git at main, on branch agents/coder/ses_01J9Z3P9D2F6H8K0M2Q4S6U8W0, into the working directory
- https://git.example/acme/api.git, on branch agents/coder/ses_01J9Z3P9D2F6H8K0M2Q4S6U8W0, into api/ in the working directory
</context>
```

Each line is one repository in the session's order: its URL, `at` its
`ref` when it names one, the session's branch, and the directory the
runner delivers it into. The block is the template
`prompts/context/repositories-v<N>.md`. The first `session.machine`
replaces it with the machine's own context block, whose `Git` line
names the branch of the checkout the working directory holds; a
session without repositories carries neither until its machine
attaches. The block is rendered from the session header and the log
alone, so a rebuilt request carries the same bytes; the first requests
of a session an earlier build ran went without it, and a replay of them
builds it in and differs there ([[007-models]]).

### Project instruction files

The harness looks for instruction files in each directory from the
repository root (the git top level, or the working directory outside a
repository) down to the working directory, and in each directory reads
`AGENTS.md` and then `CLAUDE.md` where present. On the host it first
reads the person's own `$XDG_CONFIG_HOME/topos/AGENTS.md`. The files
render in that order, the person's first and the working directory's
last, so the nearest instructions come last. A file is at most 64 KiB
and all of them together 256 KiB; past either, the file is cut with a
line saying so. `@` imports are not followed. Each file is a blob of
the session and is recorded in `session.machine`'s `instructions` with
its path and hash, and renders as:

```
<instructions path="/home/p/src/app/AGENTS.md">
...
</instructions>
```

### Agent Skills

A skill is a folder holding `SKILL.md`, whose YAML frontmatter has a
`name` (lowercase letters, digits and hyphens, at most 64 characters)
and a `description` (at most 1024 characters), and any files the
skill's body refers to. Sources, in order, the first holding a name
winning:

| # | Source |
|---|---|
| 1 | the agent's `spec.skills` ([[003-manifest]]): a path on the machine, or a git URL at a ref fetched into the machine |
| 2 | the repository root's `.agents/skills/` and `.claude/skills/` |
| 3 | on the host, the person's `$XDG_CONFIG_HOME/topos/skills/` |

Disclosure is progressive. The system prompt carries only the index,
at most 100 skills:

```
<skills>
- name: release-notes
  description: Write the CHANGELOG section for a release from the commits since the last tag.
  path: /home/p/src/app/.agents/skills/release-notes/SKILL.md
</skills>
```

The model reads a skill's `SKILL.md` with `read` when the description
matches the task, and the files it refers to the same way; there is no
skill tool. Skill folders are added to the machine's roots read-only
([[009-machines]]).

### Memory notes

Each attached store ([[020-memory-stores]]) renders as one line:
`Memory store <name> (<access>) is at <path>: <description>`, where
`<access>` is `read-write` or `read-only`.

## Not in this spec

The order and cache breakpoints of these parts ([[010-context]]); the
sender prefix of messages ([[004-session-log]]); what the tool
descriptions and results say ([[008-tools]]), the compaction prompt
asks for ([[010-context]]) and the thread tools' texts say
([[013-threads-and-subagents]]), though their files are in the prompts
tree; what memory stores are ([[020-memory-stores]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every request carries the harness prompt and the context block, and `model.request` records the prompt version | `harness.TestATurnRunsToolsAndEnds`, `runner.TestDriveAttachesTheMachineAndRunsATurn` | built |
| The prompt renders only the sections that apply, a version that does not exist is refused, and the compaction prompt renders | `prompts.TestRenderIncludesOnlyTheSectionsThatApply`, `prompts.TestCompact` | built |
| Every text renders to its pinned bytes, the harness prompt in each of its sixteen combinations of sections; a value a template names and the data lacks fails the render; every call names a text that exists with exactly the keys its template reads; and no text holds an em dash | `prompts.TestEveryTextRendersItsCurrentBytes`, `prompts.TestTheHarnessPromptRendersItsCurrentBytes`, `prompts.TestAMissingKeyFailsLoudly`, `prompts.TestEveryCallNamesAText`, `prompts.TestNoTextHasAnEmDash` | built |
| The runner sets the git and memory sections from the log, and a later machine outside a repository drops the git section | `runner.TestAttachedSetsThePromptSections` | built |
| A session with repositories whose machine has not opened names each repository, its ref, the session's branch and its directory from its first request; a session without repositories names none; the first `session.machine` replaces the block with the machine's context | `runner.TestTheFirstMachineGetsTheSessionsRepositories`, `runner.TestAMachineOnDemandIsRecordedWhenAToolFirstActsOnIt`, `prompts.TestEveryTextRendersItsCurrentBytes` | built |
| A released prompt file, the harness prompt's among them, is never changed: its hash is pinned in the test | `prompts.TestReleasedPromptsAreImmutable` | built |
| The context block reports the branch, the git counts and five commits in the documented lines, is recorded once in `session.machine`, and is the same on a second drive | `runner.TestTheContextBlock`, `runner.TestDriveAttachesTheMachineAndRunsATurn` | built |
| Instruction files are found from the repository root to the working directory, `AGENTS.md` before `CLAUDE.md`, the person's first and the nearest last, cut at 64 KiB each and 256 KiB in all; a Cella machine reads no personal file | `runner.TestDriveAttachesTheMachineAndRunsATurn`, `runner.TestAttachInARepository`, `runner.TestChainAndSkills` | built |
| The skills index lists each source's skills with the first source winning a name, refuses a skill without valid frontmatter, and holds at most 100 | `runner.TestAttachInARepository`, `runner.TestChainAndSkills`, `runner.TestLocalSkillsReportsAnUnreadableFolder` | built |
| The model can read a listed `SKILL.md` through a read-only root | `TestSkillFoldersAreReadOnlyRoots` | not built |
| A project `AGENTS.md` that requires a changelog line, and a skill that defines a release-notes format, each change the agent's output in an instruction test against a real model | `test/tasks/instructions/agents-md` and `test/tasks/instructions/skill` in the instruction tier | not built |
