---
title: "Peers and authored graphs: declared message edges between siblings, and graphs declared up front"
status: vague
track: core
depends_on: [003-manifest.md, 013-threads-and-subagents.md, 025-task-suite.md]
affects: [harness/, manifest/v1/]
effort: large
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Peers and authored graphs

## Overview

The last two stages of the session's graph. [[013-threads-and-subagents]]
builds threads, spawn edges and message edges from a parent to the
threads it spawned. Stage four adds peers: message edges declared
between sibling threads, so two subagents can exchange data without
their parent relaying it. Stage five adds authored graphs: a graph
declared up front, with its nodes, its spawn and message edges and its
routing, that compiles onto 013's threads and messages rather than
being a second runtime.

## Current state

v0.7.0's regions, topologies, mesh discovery and region graphs (v0.7.0
specs 004, 005, 009 and 012) are retired concepts; what survives is
the idea that a graph of agents can be written down. Applications that
compose several sessions into a pipeline own that composition and may
lower onto this once it exists. Nothing is built.

## Design

The intended shape:

- a peer edge is declared in the Agent manifest between two named
  subagents; it lets one thread `message` the other, carries data and
  never authority, and adds no spawn right;
- a `Graph` kind joins `manifest/v1` ([[003-manifest]]): nodes are
  agents, edges are spawn and message edges, routing says which node
  receives the session's input and which node's output ends a turn;
- a Graph compiles into the session's own thread plus spawned threads
  and declared message edges, so the log, the fold, narrowing along
  spawn edges and the depth limit apply unchanged.

What decides it: multi-agent tasks in the suite that the stage three
orchestrator cannot pass, and a caller whose pipeline lowers onto a
declared graph.

## Not in this spec

Threads, spawn and message ([[013-threads-and-subagents]]); review and
grading ([[031-review-and-graded-iteration]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The design is decided against suite tasks and a caller, and this spec is drafted with the Graph kind's fields and tests | a drafted revision of this spec | not built |
