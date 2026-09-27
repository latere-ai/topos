---
title: "Review and graded iteration: critic threads over an artifact, a rubric and a grader thread"
status: vague
track: core
depends_on: [013-threads-and-subagents.md, 025-task-suite.md]
affects: [harness/]
effort: large
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Review and graded iteration

## Overview

Two patterns on threads that the core may make first-class. Review:
several critic threads examine one artifact (a diff, a document, a
plan) and the parent weighs what they find. Graded iteration: a rubric
and a grader thread send work back to the thread that made it until it
passes or a limit is reached. Both are compositions of
[[013-threads-and-subagents]]'s spawn and message edges; what this spec
would add is the vocabulary and the stopping rules, so a caller does
not rebuild them.

## Current state

The adversarial review engine of v0.7.0 (v0.7.0 specs 013 to 018 and
022 to 027) drove other vendors' command-line agents and left the core
for `latere.ai/x/adversarial`. Its attack grammar, its ledger of
findings, its steady-state stopping rule and its contention score are
the ideas to borrow, as core patterns on threads rather than as an
engine over external processes. Nothing is built.

## Design

The intended shape, to be settled against a caller:

- a review is a set of critic threads spawned over one artifact, each
  with a stance from the attack grammar, whose findings go into a
  ledger kept in the log, with a contention score over the findings;
- review stops at the steady state: a round that adds no new finding
  above a severity;
- graded iteration is a grader thread holding a rubric, messaged with
  each attempt, whose answer is pass, or fail with the reasons that go
  back to the worker thread, up to a limit of rounds and a budget;
- both are declared in the Agent manifest and their outcomes are
  events or tool results the fold already renders.

What decides it: a caller with a real artifact and a quality bar, the
task suite gaining review tasks with checkers, and whether an outcome
and grader primitive earns its place over the plain threads of 013
after phase 3.

## Not in this spec

Threads and narrowing ([[013-threads-and-subagents]]); running other
vendors' agents, which stays in `latere.ai/x/adversarial`.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The design is decided against a caller with an artifact and a bar, and this spec is drafted with its names and tests | a drafted revision of this spec | not built |
