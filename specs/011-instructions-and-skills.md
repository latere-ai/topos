---
title: "Instructions and skills: the versioned harness prompt, the context block, project instruction files, Agent Skills"
status: drafted
track: core
depends_on: [004-session-log.md, 005-harness-loop.md, 009-machines.md]
affects: [harness/, harness/prompt/, test/tasks/instructions/]
effort: medium
created: 2026-09-27
updated: 2026-09-27
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
and progressive disclosure. Their order and cache placement are
[[010-context]]'s.

## Current state

v0.7.0 sent no system prompt: the model was never told its working
directory, platform, date or path rules, and the only injected text was
the peer directory of v0.7.0 spec 009, which is retired (probe failure
4 of [[005-harness-loop]]). The retired laptop client sent a two-line
prompt and read no project instruction file. v0.7.0 bundled a skills
catalog of its own, which Agent Skills folders replace. Nothing is
borrowed.

## Design

### The harness prompt

The harness prompt is `harness/prompt/harness-v<N>.md`, embedded in
the build, and its version `harness/<N>` is the `prompt_version` of
every `model.request` ([[004-session-log]]). A change of wording is a
new file and a new version, never an edit of a released one, and every
released version stays embedded so a replay can rebuild an old
request. A new version merges only with the instruction tier's result
against the previous one ([[025-task-suite]]).

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

The conditional sections render from `prompt.Options`, set per
session: the machine's section from its kind, `threads` when the agent
has subagents, and `git` and `memory` by the runner from the log. The
git section renders when the latest `session.machine`'s context block
has a `Git` line, and the memory section when a `memory.attached`
exists, so a later machine outside a repository drops the git section
again. The compaction prompt is `harness/prompt/compact-v<N>.md`,
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
sender prefix of messages ([[004-session-log]]); tool descriptions
([[008-tools]]); the compaction prompt ([[010-context]]); what memory
stores are ([[020-memory-stores]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every request carries the harness prompt and the context block, and `model.request` records the prompt version | `harness.TestATurnRunsToolsAndEnds`, `runner.TestDriveAttachesTheMachineAndRunsATurn` | built |
| The prompt renders only the sections that apply, a version that does not exist is refused, and the compaction prompt renders | `harness/prompt.TestRenderIncludesOnlyTheSectionsThatApply`, `harness/prompt.TestCompact` | built |
| The runner sets the git and memory sections from the log, and a later machine outside a repository drops the git section | `runner.TestAttachedSetsThePromptSections` | built |
| A released harness prompt file is never changed: its hash is pinned in the test | `TestReleasedPromptsAreImmutable` | not built |
| The context block reports the branch, the git counts and five commits in the documented lines, is recorded once in `session.machine`, and is the same on a second drive | `runner.TestTheContextBlock`, `runner.TestDriveAttachesTheMachineAndRunsATurn` | built |
| Instruction files are found from the repository root to the working directory, `AGENTS.md` before `CLAUDE.md`, the person's first and the nearest last, cut at 64 KiB each and 256 KiB in all; a Cella machine reads no personal file | `runner.TestDriveAttachesTheMachineAndRunsATurn`, `runner.TestAttachInARepository`, `runner.TestChainAndSkills` | built |
| The skills index lists each source's skills with the first source winning a name, refuses a skill without valid frontmatter, and holds at most 100 | `runner.TestAttachInARepository`, `runner.TestChainAndSkills`, `runner.TestLocalSkillsReportsAnUnreadableFolder` | built |
| The model can read a listed `SKILL.md` through a read-only root | `TestSkillFoldersAreReadOnlyRoots` | not built |
| A project `AGENTS.md` that requires a changelog line, and a skill that defines a release-notes format, each change the agent's output in an instruction test against a real model | `test/tasks/instructions/agents-md` and `test/tasks/instructions/skill` in the instruction tier | not built |
