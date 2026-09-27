---
title: "MCP servers: stdio on the host and in the sandbox, streamable HTTP anywhere, tool naming, credentials"
status: drafted
track: core
depends_on: [003-manifest.md, 008-tools.md, 009-machines.md, 012-permissions-and-approvals.md, 018-credentials-and-secrets.md]
affects: [harness/tools/mcp/]
effort: medium
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# MCP servers

## Overview

An agent reaches tools outside the built-in set through Model Context
Protocol servers named in its `spec.mcpServers` ([[003-manifest]]).
`harness/tools/mcp` connects to each, lists its tools, and registers
each one in the thread's registry, where it is offered, validated,
scored, permission-gated and capped like a built-in. A stdio server
runs as a process on the session's machine; a streamable HTTP server
can be anywhere. A server's credential comes from a connection and is
injected outside the machine.

## Current state

v0.7.0 had no MCP client. Nothing is borrowed.

## Design

### Transports

| Transport | Declared by | Runs |
|---|---|---|
| stdio | `command`, `args`, `env` | a process on the session's machine: on the host inside the host sandbox ([[012-permissions-and-approvals]]), on a Cella machine inside the sandbox through a streamed exec ([[009-machines]]); never in a hosted runner's own process space |
| streamable HTTP | `url`, `connection` | a request per message from the runner to the URL, with the connection's credential injected by the runner |

`env` values are plain configuration; a secret there is refused at
resolve ([[003-manifest]]). A stdio server that needs a secret gets a
named secret's placeholder, substituted at the machine's egress
([[018-credentials-and-secrets]]).

### Tools

At a session's first claim the harness connects to each server,
completes the protocol's initialization, and lists its tools. Each
tool is registered as `mcp__<server>__<tool>`, with the server's
description and input schema; a schema using a keyword the validator
of [[005-harness-loop]] does not cover is registered with that keyword
ignored and the server named in `session.error` `mcp_schema_reduced`.
The tool set is part of the request's tool definitions, and its hash is
the `model.request`'s `tools_sha256` ([[004-session-log]]); a server
whose tool list changes mid-session applies from the next turn.

| Property | Value |
|---|---|
| `Parallel` | yes, unless the server marks the tool otherwise |
| `Effect` | `external` |
| `Repeatable` | never ([[008-tools]]) |
| output | text and image content, capped and spilled as [[008-tools]] |

A tool call is validated, scored and given a verdict like any other,
and appears in patterns by its full name
(`mcp__github__create_issue`, [[012-permissions-and-approvals]]). A
call with no result is never run again. A server that fails to start,
or stops answering, answers each of its calls with outcome `error`
naming the server, and the rest of the session continues.

### Narrowing

A subagent is offered an MCP tool only when its parent holds it
([[013-threads-and-subagents]]). A server's process, when stdio, is
shared by the threads of one machine.

### Error codes

| Code | Meaning |
|---|---|
| `mcp_unavailable` | a server could not be started or reached at session start |
| `mcp_schema_reduced` | a tool's schema used a keyword the validator ignores |

## Not in this spec

Connections and credential injection ([[018-credentials-and-secrets]]);
the verdict ([[012-permissions-and-approvals]]); the manifest fields
([[003-manifest]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| An MCP tool is offered, called, and permission-gated like a built-in: it carries a score and verdict, an ask pauses the session, and a block does not run it | `TestMCPToolGatedLikeABuiltin` against a stub server | not built |
| A stdio server on the host runs inside the host sandbox, and on a Cella machine inside the sandbox | `TestStdioServerRunsOnTheMachine` | not built |
| An HTTP server's credential is injected by the runner and appears in no event and no machine | `TestHTTPServerCredentialOutsideMachine` | not built |
| An MCP call with no result is closed `unknown_effect` after a runner restart and never repeated | `TestMCPCallNeverRepeated` | not built |
| A server that stops answering fails its calls and not the session | `TestDeadServerFailsOnlyItsCalls` | not built |
