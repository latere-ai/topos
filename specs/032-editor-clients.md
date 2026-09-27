---
title: "Editor clients: serving a session to editors over the Agent Client Protocol"
status: vague
track: core
depends_on: [004-session-log.md, 016-runners.md, 024-client-cli-skill.md]
affects: [cmd/topos/, internal/toposcli/]
effort: medium
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Editor clients

## Overview

Editors reach coding agents through the Agent Client Protocol: the
editor starts an agent process and exchanges JSON-RPC messages with it
over stdio for prompts, streamed output, tool call reports,
permission requests and file access. Serving that protocol from
`topos` would let an editor drive a Topos session, local or on a
server, and show its log, with no editor-specific code in the core.

## Current state

Nothing is built, and v0.7.0 had nothing of the kind. The mapping
looks thin: a prompt is a `user.message`, streamed output is the
harness's deltas, a tool call report is `agent.tool_use` and
`tool.result`, a permission request is an ask answered by
`user.tool_confirmation` ([[004-session-log]]).

## Design

The intended shape: `topos acp` as a mode of the command that speaks
the protocol on stdio, runs a local session over the directory store
or attaches to a server session through `client`, maps the protocol's
messages onto the session's events one to one, and uses the editor's
file access only where the protocol requires it, keeping the machine
of [[009-machines]] as the place tools act.

What decides it: an editor integration someone asks for, the
protocol's stability at that time, and whether its file and terminal
capabilities can be served without moving tool execution out of the
machine.

## Not in this spec

The interactive terminal, which is a client outside this module; the
session log and the runner ([[004-session-log]], [[016-runners]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The design is decided against an editor integration a caller wants, and this spec is drafted with its mapping and tests | a drafted revision of this spec | not built |
