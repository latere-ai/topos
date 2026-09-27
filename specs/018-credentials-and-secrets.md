---
title: "Credentials, connections and secrets: write-only credentials, the agent's key, injection outside the machine, scrubbing, named secrets, the input check, redaction, session scope"
status: drafted
track: core
depends_on: [001-architecture.md, 002-scaffold-and-configuration.md, 003-manifest.md, 004-session-log.md, 006-identity.md, 009-machines.md]
affects: [internal/credentials/, internal/egressproxy/, session/inputcheck/, runner/, machine/cella/]
effort: large
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Credentials, connections and secrets

## Overview

A credential is a secret a session uses to reach one connection. It is
a Topos object, written once, never returned, never placed in an event
and never placed in a machine (invariant 5 of [[001-architecture]]).
This spec owns the Credential object and its encryption at rest, the
agent's key and how a session acts with it (invariant 8), connections
configured as the person running the agent or owned by the agent,
per-call resolution and injection outside the machine, scrubbing of
known values from tool output, named secrets as placeholders, the input
check a client runs before a message leaves the machine, the redaction
of an event's content, and the session scope with its narrowing and
widening rules (the scope half of invariant 6).

## Current state

v0.7.0 spec 011 delivered vault secrets into a sandbox without
plaintext; that is retired, because nothing here places a secret in a
machine at all. The retired hosted service reached other services
through per-product actor audiences, a Cella grant mode and stored
service-account secrets under an encryption key that was never set, so
no unattended session could act; none of that is carried. Borrowed:
envelope encryption under a list of keys where the first wraps and all
are tried, as Lux custodies provider credentials, and Cella's Secret
kind, whose placeholder its egress gateway substitutes at the last hop.

## Design

### The Credential object

| Field | Meaning |
|---|---|
| `id` | `cred_…` ([[004-session-log]]) |
| `name` | a DNS label, unique per owner |
| `kind` | `agent_key` (an agent's key), `token` (a bearer or API key sent in a header), `oauth_refresh` (a refresh token the runner trades for access tokens) |
| `owner` | the subject or organization that owns it; an `agent_key` is owned by its agent |
| `hosts` | the hosts the value may be sent to; empty for `agent_key`, whose use is the token trade below |
| `connection` | the Connection a person's own credential answers for, when it is one |
| `created_by`, `created_at`, `last_used_at`, `expires_at` | bookkeeping; `expires_at` optional |
| `status` | `active` or `revoked` |

The value is accepted once, in the create request of [[015-api]], and
no route, event, sink record, log line or error returns it. A value
shorter than 8 bytes is refused with `credential_value_too_short`,
because scrubbing cannot match it safely. Revoking deletes the
ciphertext and keeps the row.

### Encryption at rest

Each value is sealed with a fresh 256-bit data key under AES-256-GCM
with a random 96-bit nonce, and the credential's id and owner as
additional data, so a ciphertext moved to another row does not open. The data key is
wrapped with AES-256-GCM under the first key of `TOPOS_CREDENTIALS_KEY`
([[002-scaffold-and-configuration]]) and stored with that key's id, the
first 8 bytes of its SHA-256. Opening tries the key whose id matches,
then every key. Rotation is prepending a key: `serve` rewraps, in the
background and in batches, every data key not wrapped under the first
key, and `toposd check` reports how many remain
([[028-release-and-installation]]). With the variable unset, every
credential route answers `credentials_unavailable`.

### The agent's key

| Agent | Acts as | Its key |
|---|---|---|
| `identity: agent`, an organization's agent ([[003-manifest]]) | its own agent identity, a principal the installation's identity provider holds for the agent | a key narrowed to the agent's `permissions`, stored once as an `agent_key` credential; revoked when the agent is archived |
| `identity: person`, a personal agent | the person, narrowed; audit reads "agent X, acting for you" | one of the person's keys narrowed to the agent's `permissions`, stored the same way |
| any agent run by an external runner | the runner's own credential for the append | the developer's, never stored by Topos ([[017-external-runners-handoff-fork]]) |

The core stores the key a client provisioned when it applied the agent
with its own authority; it mints no key at an identity provider. Every
session of the agent acts with that key, whoever starts it and whoever
writes to it. The runner resolves it per outbound call through a
`TokenSource(ctx, audience) (token, expiry, error)` built for the
session: it trades the key at its issuer for a token of 15 minutes per
core audience, caches each until 2 minutes before its expiry, and
presents it as a bearer; at Lux's model doors it presents the key's
token the same way, so money follows the key ([[007-models]]). A local
issuer implements the same `TokenSource` for a self-hoster. The runner
sets the session attribution field of `latere.ai/x/pkg/authz`'s
envelope on every call it makes, an additive pkg change that lands with
this spec, so an installation's authorizer knows which session asks.

### Connections

A Connection ([[003-manifest]]) names a service, its hosts and a mode.

| Mode | Credential used |
|---|---|
| `agent` | the Connection's own credential, one for every session |
| `person` | the credential of the person running the session, its initiator: a `token` or `oauth_refresh` that person owns with `connection` set to the Connection; absent, the call fails with `connection_not_connected` and the client offers to connect |

Injection is always outside the machine. For a call the runner makes
itself (an HTTP MCP server, [[021-mcp-servers]]), the runner adds the
header of the Connection's `inject` to that request only, toward the
Connection's hosts only. For traffic from a Cella sandbox, the runner
applies one Cella Secret per session and Connection, whose value is the
resolved credential and whose scope is the Connection's hosts, and
names it in the sandbox's manifest ([[009-machines]]); the sandbox holds
the placeholder and Cella's egress gateway substitutes it. The runner
rotates a Secret's value before the token in it expires.

### Scrubbing known values

The runner keeps the set of exact values it resolved for the session:
the key and its tokens, connection credentials, named secret values.
Before a `tool.result`, a hook's output or a `session.error` detail is
appended or reaches the model, every occurrence of a value, and of its
standard base64, URL-safe base64 and percent-encoded forms, is replaced
by `[redacted:<credential name>]`. The set lives in memory only and is
dropped with the lease.

### Named secrets

A person adds a named secret through a client: a name, a value and the
hosts it is for. The agent sees `$NAME` in its environment, holding a
placeholder, and the value is substituted only on requests to those
hosts.

| Machine | Mechanism |
|---|---|
| a Cella sandbox | a Cella Secret per named secret, with placeholders random per sandbox, substituted by the egress gateway |
| the host | the local proxy `internal/egressproxy`, built from `latere.ai/x/cella/egress`: commands reach the network through it under the host sandbox's network rule ([[012-permissions-and-approvals]]); it terminates TLS only for the named hosts with a per-session authority placed in the trust variables, substitutes placeholders in request headers toward those hosts and nowhere else, and reads the values from the operating system's keychain at session start; Topos writes no value to disk |

### The input check

`session/inputcheck.Check(text) []Finding` reports, by offset and kind
and never by value: well-known token formats (`sk-ant-`, `sk-`,
`ghp_`, `github_pat_`, `gho_`, `xoxb-`, `xoxp-`, `lux_`, `AKIA` and 16
uppercase characters, a PEM `PRIVATE KEY` block, a three-part JWT) and
high-entropy strings (24 or more characters of a base64 alphabet at 4.0
bits per character or more, or of hex at 3.0 or more). A client runs it
before a `user.message` leaves the machine: it warns, offers to store
the value as a named secret and send `$NAME` instead, and never blocks.
The manifest resolver runs the same function and refuses
([[003-manifest]]).

### Redaction

A pasted secret has reached the model provider once sent; the design
claims no guarantee and gives a way to limit the damage. `session.redact`
([[006-identity]]) is allowed by the owner policy to the session's
initiator and to admins; the route is [[015-api]]'s. The mechanics are
[[004-session-log]]'s: the event is tombstoned, the blobs it named are
deleted, and the runner compacts before the next request with `cause`
`redaction`. The client then tells the person that the value reached
the model provider and should be rotated.

### Session scope

A session runs within a scope, a list of grants `{action, resource}` in
the grammar of the agent's `permissions` ([[003-manifest]]). The
installation's authorizer refuses a `session.create` whose agent's
permissions exceed what the initiator could do, and a `session.send`
from a sender who could not have started the session; the scope starts
as the agent's permissions, capped by the initiator.

| Change | Who | Bound |
|---|---|---|
| narrow | anyone who may send | none |
| widen | anyone who may send | only within the agent's permissions and the widener's own rights; until the end of the turn by default, or for a set number of minutes, or standing when asked |
| grant an irreversible action (a push to a protected branch, a production deploy) | nobody | never grantable; stays a per-action confirmation ([[012-permissions-and-approvals]]) |

Each change is a `session.scope_changed` event ([[004-session-log]]);
a widening that lapses is reverted by the runner with another one.
toposd records and serves the scope and decides nothing about it: the
authorizer, which learns the session from the envelope's attribution,
reads it. After any change the runner drops its cached tokens and takes
fresh ones, so no cached allow crosses a change.

### Error codes

| Code | Meaning |
|---|---|
| `credentials_unavailable` | `TOPOS_CREDENTIALS_KEY` is unset |
| `credential_value_too_short` | a value under 8 bytes |
| `connection_not_connected` | a `person` connection with no credential of the initiator's |
| `scope_widening_refused` | a widening beyond the agent's permissions or the widener's rights, or of an irreversible action |

## Not in this spec

The routes ([[015-api]]); the action vocabulary and the owner policy
([[006-identity]]); git credentials and ref rules ([[019-git]]); memory
backends' credentials ([[020-memory-stores]]); the threat model of a
compromised runner ([[027-security]]); how a client provisions an
agent's key at its identity provider.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Canary credentials used by the suite's cloud tasks appear in no event, blob, sink record, log line, sandbox environment, sandbox file or process list | `TestNoCredentialInAnyEventOrMachine` in the e2e tier | not built |
| No route returns a credential's value, and a value under 8 bytes is refused | `TestCredentialValueIsWriteOnly` | not built |
| A value sealed under an old key opens after a new key is prepended, the background rewrap moves it, and a ciphertext copied to another row does not open | `TestCredentialsEnvelopeRotation`, `TestCiphertextBoundToItsRow` | not built |
| Every call a session makes to a core carries a token traded from the agent's key, and none carries toposd's own identity | `TestSessionCallsCarryTheAgentKey` | not built |
| A `person` connection uses the initiator's credential, and fails with `connection_not_connected` when there is none | `TestPersonConnectionUsesInitiatorsCredential` | not built |
| A value the runner holds, and its base64 and percent-encoded forms, are replaced in tool output before the log and the model | `TestScrubbingKnownValues` | not built |
| On the host, a named secret is substituted only on requests to its hosts, and a request elsewhere carries the placeholder | `TestHostProxySubstitutesOnlyNamedHosts` | not built |
| The input check finds each listed format and a high-entropy string, reports no value, and never blocks the message | `TestInputCheckFindsTokensAndNeverBlocks` | not built |
| A widening beyond the widener's rights is refused, a lapsed widening is reverted, every change is an event, and the next call after a change uses a fresh token | `TestScopeWideningBoundedAndRecorded`, `TestScopeChangeTakesFreshToken` | not built |
