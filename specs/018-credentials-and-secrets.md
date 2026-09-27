---
title: "Credentials, connections and secrets: write-only credentials, the agent's identity and its session tokens, injection outside the machine, scrubbing, named secrets, the input check, redaction, session scope"
status: in-progress
track: core
depends_on: [001-architecture.md, 002-scaffold-and-configuration.md, 003-manifest.md, 004-session-log.md, 006-identity.md, 009-machines.md]
affects: [internal/credentials/, internal/identity/, internal/egressproxy/, session/inputcheck/, runner/, machine/cella/, manifest/, internal/hosted/, internal/runnerapi/, internal/runnerrole/, internal/server/, internal/config/, test/stubs/]
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
agent's identity and the short-lived tokens a session acts with
(invariant 8), connections
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
no unattended session could act; none of that is carried. Until this
spec lands, a hosted session acts with the installation's one model
key and one Cella bearer. Borrowed:
envelope encryption under a list of keys where the first wraps and all
are tried, as Lux custodies provider credentials, and Cella's Secret
kind, whose placeholder its egress gateway substitutes at the last hop.

Built so far, for the hosted path: the identity provider's client
(`internal/identity`), the minter of a session's tokens and generated
Lux keys (`internal/credentials`), the `TokenSource` of a drive
(`runner`), the lease-checked token route (`internal/runnerapi`,
`internal/runnerrole`), a session's own model key, Cella token and
sandbox Secrets (`internal/hosted`), the identity's lifecycle at apply,
archive and reconcile (`internal/server`), the variables, and stubs of
the identity provider and the authorizer's key routes (`test/stubs`).
The Credential object and its encryption, connections, scrubbing, named
secrets, the input check and the scope route are not built.

## Design

### The Credential object

| Field | Meaning |
|---|---|
| `id` | `cred_…` ([[004-session-log]]) |
| `name` | a DNS label, unique per owner |
| `kind` | `token` (a bearer or API key sent in a header), `oauth_refresh` (a refresh token the runner trades for access tokens) |
| `owner` | the subject or organization that owns it |
| `hosts` | the hosts the value may be sent to |
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

### The agent's identity and its session tokens

An agent holds no key. Every agent applied to a server with an identity
provider is its own identity there, recorded with its owner: the person
for a personal agent, the organization for an organization's agent
([[003-manifest]]). Subagents, forks, sessions and trigger firings are
not identities; they act as the agent they belong to
([[013-threads-and-subagents]]). The identity is created when the agent
is first applied and disabled when it is archived. An agent a person
runs locally with `topos` acts as that person and has no identity.
The agent's status keeps the owner its identity was created for, the
one the authorizer's `agent.create` allow names, and every later version
carries it, so a session of an organization's agent names the
organization, not its applier. An agent cannot run without the models
it names, so a session's permissions include `lux:model.use` on each of
them without the author listing it.
The owner is what makes an agent personal or an organization's, so the
manifest's `identity` field is removed ([[003-manifest]]).

| Agent | Its identity | Reach | Spend |
|---|---|---|---|
| a personal agent | owned by the person | at most the person's, narrowed to the agent's `permissions` | the person's wallet |
| an organization's agent | owned by the organization | the agent's `permissions`, capped per session by the initiator | the organization's allowance, per the authorizer |
| a session of an external runner | the developer's own credential for the append ([[017-external-runners-handoff-fork]]) | the developer's | the developer's |

The installation holds one credential of its own at its identity
provider, as every service does; no runner and no machine holds it.
toposd asks the identity provider for a token for a hosted agent: its
subject is the agent's identity, its audience one core, its lifetime at
most 15 minutes, and it carries the session as a claim, `session`, with
the session's id and the workload, `session` for the runner's own calls
and `sandbox` for a sandbox's. The identity provider mints only for the
agents the installation hosts; toposd asks only for a session whose
lease the requesting runner holds at its current generation
([[016-runners]]). The session claim is authority, not attribution: a
core forwards the verified claims to the installation's authorizer
([[006-identity]]), which checks on every decision that the session is
live and applies its scope, the initiator cap and the agent's
permissions, so a token of an ended session is refused before it
expires, and a token for a session the authorizer never allowed opens
nothing.

toposd speaks to the identity provider through one interface, which
an installation without one does not configure:

| Call | When | What toposd sends |
|---|---|---|
| create | an apply of an agent that has no identity yet, after the authorizer allowed it; a refusal or an unanswered call refuses the apply | the agent's id as the identity provider's `ref`, its name, the owner, and the applier; the answer's subject is kept as the agent's `status.identity` ([[003-manifest]]) and carried to every later version |
| archive | the archive route, before toposd archives the agent, so a failed call leaves the agent as it was and a retry is idempotent | the agent's id, with the confirmation `permanent: true` |
| disable | once the archived agent has no session that has not ended: at the archive itself, and in a pass at start and on every reaper pass ([[014-store]]) over the provider's list of archived identities | the agent's id and subject, with `permanent: true` |
| mint | a runner's token request, above | the subject, the audience and the `session` claim |

The owner is the authorizer's to name, because a core decides from no
claim of a token ([[006-identity]]): the allow of the apply carries it
as the limits member `owner`, and an allow that names none makes the
applier, as a person, the owner. toposd holds
its own token from the identity provider until 2 minutes before its
expiry and reads its client secret from a file at each fetch, so the
secret rotates in place. The variables are [[002-scaffold-and-configuration]]'s;
the identity provider's variables need the installation's authorizer,
since only it can check the `session` claim.

A runner reaches its session's credentials through a `TokenSource`
built for each drive: `Token(ctx, audience, workload)` answers a value
and its expiry. In `serve` it calls toposd's minter in process, which
refuses once the drive's lease is lost; in the runner role it asks the
internal listener ([[016-runners]]), which answers only at the lease's
current generation. The audience `lux` answers the session's Lux key
for the workload, and any other audience a token. Each answer is cached
until 2 minutes before its expiry. After a `session.scope_changed` or a
lost lease every cached answer is dropped. An installation that mints
nothing for an audience says so (`not_minted`), and the runner then
uses the installation's own credential below. A session of an agent
with no identity, on an installation with an identity provider, fails
its turn with `agent_identity_missing`.

### Model access

Lux's model doors take Lux keys, so a session reaches models with
short-lived Lux keys rather than tokens. At the first claim the runner
asks, through toposd, the installation's authorizer for two keys for
the session, which the authorizer has Lux create: one for the runner's
own requests and one for the session's sandbox, each marked with the
session and its workload, lasting at most the session's lease plus 15
minutes and renewed with it, and carrying the session's budget
([[007-models]]). toposd generates each value, `lux_` and 40 random
characters of `[A-Za-z0-9_-]` as Lux mints its own, and sends the
authorizer only its SHA-256 (`PUT` of the session's key for the
workload); the value lives in toposd's memory and in the runner's. A
request for a key with less than 10 minutes left sends the same hash
again, which renews it; a new lease generation or a new toposd process
generates a new value, whose hash replaces the old key, so a runner
that lost its lease loses its key at the next claim. The runner
presents its key as the model connection's credential, asked again for
every model request. The sandbox's key is a Cella Secret named after
the sandbox with `-lux`, mounted as `LUX_KEY` and scoped to Lux's host,
and to its model doors' path once Cella scopes a Secret by path; the
sandbox holds a placeholder that Cella's egress gateway swaps in
([[009-machines]]), and the runner applies the Secret again whenever
the value changes. It opens nothing but models, because no other core
accepts a Lux key. Lux enforces the budget per key and records each
call against the session and its workload, so the ledger keeps a
session's own spend apart from its sandbox's
([[023-events-and-observability]]).

### Without an identity provider

A self-hosted installation with no identity provider acts with its own
credentials: `TOPOS_MODELS_KEY` for models and the file of
`TOPOS_CELLA_TOKEN_FILE` for Cella ([[002-scaffold-and-configuration]]).
Every session acts as the installation, with no per-agent reach and no
per-session binding; the operator's choice of those credentials is the
bound. An installation whose authorizer creates session keys refuses to
start with `TOPOS_MODELS_KEY` set, and one with an identity provider
with `TOPOS_CELLA_TOKEN_FILE` set, so neither installation credential
is used beside the per-session ones.

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
its tokens and Lux keys, connection credentials, named secret values.
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
| widen | whom the authorizer allows | within the agent's permissions and the widener's own rights; how long a widening lasts is the authorizer's decision |
| grant a flagged action, one a core marks irreversible | nobody, by a widening | only a one-shot step-up grant for one action and resource ([[012-permissions-and-approvals]]) |

Each change is a `session.scope_changed` event ([[004-session-log]]);
a widening that lapses is reverted by the runner with another one.
toposd records and serves the scope and decides nothing about it: the
authorizer, which learns the session from the token's `session` claim,
reads it. After any change the runner drops its cached tokens and takes
fresh ones, so no cached allow crosses a change.

### Error codes

| Code | Meaning |
|---|---|
| `credentials_unavailable` | `TOPOS_CREDENTIALS_KEY` is unset |
| `credential_value_too_short` | a value under 8 bytes |
| `connection_not_connected` | a `person` connection with no credential of the initiator's |
| `scope_widening_refused` | a widening beyond the agent's permissions or the widener's rights, or of an irreversible action |
| `identity_refused` | the identity provider refused to create or archive the agent's identity; the detail names its code |
| `identity_unavailable` | the identity provider did not answer an apply's or an archive's call; try again |
| `agent_identity_missing` | a session's turn on an installation with an identity provider, of an agent that has no identity there |

## Not in this spec

The routes ([[015-api]]); the action vocabulary and the owner policy
([[006-identity]]); git credentials and ref rules ([[019-git]]); memory
backends' credentials ([[020-memory-stores]]); the threat model of a
compromised runner ([[027-security]]). Outside this repository: the
identity provider's agent identities and the tokens it mints for a
hosted agent with the `session` claim; the authorizer's session record
and its live check; Lux creating short-lived session keys; Cella's
egress swapping a Secret by host and path.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Canary credentials used by the suite's cloud tasks appear in no event, blob, sink record, log line, sandbox environment, sandbox file or process list | `TestNoCredentialInAnyEventOrMachine` in the e2e tier | not built: it needs the Credential object and a real Cella's egress gateway; `internal/hosted.TestSessionAndSandboxLuxKeys` holds that the sandbox's environment carries placeholders and no value |
| No route returns a credential's value, and a value under 8 bytes is refused | `TestCredentialValueIsWriteOnly` | not built: the Credential object and its routes ([[015-api]]) come after the hosted path |
| A value sealed under an old key opens after a new key is prepended, the background rewrap moves it, and a ciphertext copied to another row does not open | `TestCredentialsEnvelopeRotation`, `TestCiphertextBoundToItsRow` | not built: comes with the Credential object; `TOPOS_CREDENTIALS_KEY` is not read yet |
| Every call a session makes to a core carries a token whose subject is the agent's identity and whose `session` claim names the session and the workload, and none carries toposd's own identity | `cmd/toposd.TestSessionCallsCarryTheAgentsSessionToken`, in `serve` and in the runner role | built against the stub identity provider for the runner's calls to Cella and the sandbox's git host; Arca's memory token is [[020-memory-stores]]'s, and the same run against the real identity provider and authorizer waits for their deployment |
| toposd mints a token only for the holder of the session's current lease, and a runner that lost its lease gets none | `internal/credentials.TestTokensOnlyForTheLeaseHolder`, `internal/runnerrole.TestTokensOnlyForTheLeaseHolder` | built |
| An agent first applied with an identity provider gets its identity after the authorizer's allow, with the owner the allow names, the applier otherwise, and keeps its subject as `status.identity`; a refused or unanswered create refuses the apply; the archive route archives the identity with `permanent: true` before the agent; the archived agent's identity is disabled with `permanent: true` once no session of it is left unended, and not before | `internal/server.TestAgentIdentityLifecycle`, `internal/server.TestReconcileCatchesUp`, `internal/identity.TestTheHostsCalls` | built against the stub identity provider |
| A `TokenSource` caches each answer until 2 minutes before its expiry, answers nothing after its lease is lost, and a `session.scope_changed` drops what it holds | `runner.TestTokenSourceCachesAndDrops`, `runner.TestScopeChangeTakesFreshToken`, `internal/hosted.TestSandboxCredentialsAreRenewed` | built |
| The stub identity provider answers the host's token, create, archive, disable, list and mint with the host contract's codes, and the stub session keys answer the key routes | `test/stubs/idpstub.TestTheStubAnswersTheHostContract`, `test/stubs/keystub.TestTheStubAnswersTheKeyRoutes` | built |
| The identity provider's and the session keys' variables are read; each URL needs its partners and the authorizer, and neither installation credential is accepted beside them | `internal/config.TestTheCredentialVariables` | built |
| A manifest naming `spec.identity` is refused, and an agent's personal or organization standing follows its owner | `manifest.TestValidationRules`, `internal/server.TestAgentIdentityLifecycle`, `authorizer.TestDecodeLimitsReadsEveryMember` | built |
| A session reaches models with its own Lux key and its sandbox with a second one swapped in at egress; neither is the installation's key when an authorizer is configured | `internal/credentials.TestSessionAndSandboxLuxKeys`, `internal/hosted.TestSessionAndSandboxLuxKeys`, `internal/hosted.TestAnInstallationThatMintsNothingActsAsToday`, `internal/config.TestTheCredentialVariables` | built against the stub key routes, with the refusal of the installation's key tied to `TOPOS_SESSION_KEYS_URL` rather than to the authorizer; the swap itself is Cella's egress gateway's, the Secret is scoped by host until Cella scopes one by path, and the run against the real authorizer waits for its deployment |
| A `person` connection uses the initiator's credential, and fails with `connection_not_connected` when there is none | `TestPersonConnectionUsesInitiatorsCredential` | not built: needs the Credential object |
| A value the runner holds, and its base64 and percent-encoded forms, are replaced in tool output before the log and the model | `TestScrubbingKnownValues` | not built: comes after the hosted path |
| On the host, a named secret is substituted only on requests to its hosts, and a request elsewhere carries the placeholder | `TestHostProxySubstitutesOnlyNamedHosts` | not built: comes after the hosted path |
| The input check finds each listed format and a high-entropy string, reports no value, and never blocks the message | `TestInputCheckFindsTokensAndNeverBlocks` | not built: comes after the hosted path |
| A widening beyond the widener's rights is refused, a lapsed widening is reverted, every change is an event, and the next call after a change uses a fresh token | `TestScopeWideningBoundedAndRecorded`, `runner.TestScopeChangeTakesFreshToken` | partly built: the next call after a `session.scope_changed` takes a fresh token; the scope route that bounds and records a change is not built, and its bound is the authorizer's |
