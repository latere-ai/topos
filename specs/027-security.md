---
title: "Security and threat model: assets, boundaries, adversaries, and the test or invariant that holds each threat"
status: drafted
track: core
depends_on: [001-architecture.md, 009-machines.md, 012-permissions-and-approvals.md, 016-runners.md, 018-credentials-and-secrets.md]
affects: [docs/security.md, SECURITY.md]
effort: medium
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Security and threat model

## Overview

What the core protects, from whom, at which boundary, and which test
or invariant holds each threat. The design places policy where an
effect crosses a boundary as a typed request, not on the text of a
command, and treats every tool output as untrusted input to the model.
It assumes the workload in a sandbox is hostile: code the agent wrote
or fetched may try to use whatever the sandbox can reach, to act
outside its session, to spend, and to persist. This spec adds no
mechanism; it indexes the ones the other specs build
and is complete when every row names a passing test.

## Current state

v0.7.0 had no threat model; its host sandbox confined nothing and its
permission hooks allowed by default. The retired hosted service kept
credentials under a key that was never set. Nothing is borrowed.

## Design

### Assets

| Asset | Where it lives |
|---|---|
| agent keys | the installation's authorizer; never toposd, a runner or a machine ([[018-credentials-and-secrets]]) |
| connection credentials, named secrets | encrypted in the store; swapped in at Cella's egress, never inside a sandbox ([[018-credentials-and-secrets]]) |
| short-lived tokens | a runner's memory while it holds a session's lease, each bound to that session and to one destination ([[018-credentials-and-secrets]]) |
| the installation's own keys: the local issuer key, the credentials key, the database URL | the server's configuration, which no command sees ([[009-machines]]) |
| session logs and blobs | the store ([[004-session-log]], [[014-store]]) |
| the person's host: files, credential files, processes | the host machine ([[009-machines]]) |
| repositories and their protected branches | the git host ([[019-git]]) |
| memory stores | the directory or Arca backend ([[020-memory-stores]]) |

### Boundaries

| Boundary | Separates | Held by |
|---|---|---|
| the Cella sandbox | agent-written code from everything else; no credential inside, egress only through Cella's gateway | Cella, and invariant 5 of [[001-architecture]] |
| the host sandbox | commands from files outside the roots, from credential files, from unnamed network hosts | [[012-permissions-and-approvals]] |
| the runner | credentials and the lease from the machine | [[016-runners]], [[018-credentials-and-secrets]] |
| the API | people and programs from the store, through verification and the authorizer | [[006-identity]], [[015-api]] |
| the internal listener | runners from the public API, with the runner token | [[016-runners]] |
| a sandbox's tokens | a workload from every destination and action its session was not allowed | the authorizer's mint per destination and Cella's egress by host and path ([[018-credentials-and-secrets]], [[012-permissions-and-approvals]]) |
| a server's host sandbox | one person's host session from another's, and from the server's own files and environment | [[009-machines]] |

### Threats

| Threat | What bounds it | Held by |
|---|---|---|
| tool output carries instructions that steer the model (prompt injection from a file, a web page, an MCP result) | the model can only act through tools; every call is scored and gated; the hard boundaries hold whatever the model is persuaded to do; credentials never reach the model | [[012-permissions-and-approvals]] `TestHostSandboxConfinesBash`; [[018-credentials-and-secrets]] `TestNoCredentialInAnyEventOrMachine` |
| an agent reads a credential file on the host | the deny-list in the file tools and in the host sandbox, in every mode | [[009-machines]] `TestHostDenyList` |
| an agent widens its own authority through a hook, a mode, a pattern or a subagent | verdicts only narrow; spawn edges only narrow; messages carry no authority | invariant 6; `TestHookCannotWiden`, `TestNarrowingAlongSpawnEdges` |
| an agent pushes to a protected branch | the git host's ref rules | [[019-git]] `TestPushToProtectedBranchRefused` |
| a credential leaks into a log, an event or tool output | the runner scrubs the values it holds before append; no event field holds one | invariant 5; `TestNoCredentialInAnyEventOrMachine` |
| a secret pasted into a message | the input check warns and offers a named secret; redaction tombstones the event and compacts past it; the person is told to rotate | [[018-credentials-and-secrets]], [[004-session-log]] `TestRedactionTombstonesAndRequiresCompaction` |
| a manifest carries a secret | strict decoding and the input check at resolve | [[003-manifest]] `TestManifestHoldingASecretIsRefused` |
| a compromised runner | it holds no agent key, only the short-lived tokens of the sessions whose leases it holds, each bound to its session and destination and lasting minutes; a token is minted only for the holder of a session's current lease; a lost lease fences its appends within 60 seconds | [[016-runners]] `TestSecondWriterIsRefused`; [[018-credentials-and-secrets]] |
| a hostile workload uses a swapped-in credential at another core or against another session | each token is minted for one destination within the session's scope, bound to the session and marked workload `sandbox`; egress swaps it only toward that destination's host and path, so a model-only token never reaches Cella, Origo or Arca | [[018-credentials-and-secrets]] `TestASandboxTokenReachesOnlyItsDestination` |
| a hostile workload runs an irreversible action from a script the classifier cannot read | sandbox tokens carry no flagged action; the step-up at egress asks before a one-shot grant | [[012-permissions-and-approvals]] `TestEgressStepUp` |
| a hostile workload rewrites another session's branch or an earlier checkpoint | a session writes only its own branches; only the runner creates checkpoint refs, and none is updated | [[019-git]] `TestASessionWritesOnlyItsOwnBranches`, [[034-checkpoints-and-rewind]] `TestCheckpointRefsAreTheRunners` |
| a hostile workload spends the agent's budget through model calls | Lux enforces the session's allowance on every call, sandbox calls included, and a refusal stops the session `budget` | [[007-models]] `TestACoresSpendRefusalStopsTheTurn` |
| a hostile workload carries data to another initiator, or persists an injected instruction, through memory | stores are partitioned by initiator; a shared store is an admin's choice with a declared audience, and its writes are attributed and versioned | [[020-memory-stores]] `TestMemoryPartitionsByInitiator` |
| a hostile workload reads the value of a credential | no value is inside the sandbox; a minted token is swapped into headers only, never a body, and only toward its destination | [[018-credentials-and-secrets]] `TestNoCredentialInAnyEventOrMachine` |
| a subagent reaches the authority of the agent it names | a thread acts with the session's credentials; the named agent's identity and permissions are ignored and recorded | [[013-threads-and-subagents]] `TestASubagentActsWithTheSessionsCredentials` |
| a host session on a server reads another session's files or the server's keys | the mandatory host sandbox, a directory per session, none of the server's environment | [[009-machines]] `TestHostSessionsOnAServer` |
| a log written by an external runner steers a hosted runner that continues it | the log is input, never authority: the hosted runner takes policy, scope and budget from the Session and the authorizer, not from events | [[017-external-runners-handoff-fork]] |
| a second writer corrupts a session | one writer by lease and generation; a takeover forks | invariant 3; `TestKillRunnerMidTurnLosesNoEvent` |
| a call repeated after a crash duplicates an effect | no call without a result runs again, except `memory_sync` with preconditions | [[016-runners]] `TestRecoveryRules` |
| the authorizer is down | every decision is a refusal | invariant 7; [[006-identity]] `TestAuthorizerDownIsRefusal` |
| the control plane reaches into a developer's network | it dials no runner and no worker | invariant 10; `TestServerOpensNoConnectionToARunner` |
| a stolen API token | issuer verification with the age bound; the authorizer's answer per action | [[006-identity]] |
| the model is the scripted one in production | server roles refuse it | invariant 9; `TestServeRefusesScriptedModel` |

### Out of scope

A sandbox escape from Cella is Cella's threat model. A model
provider's handling of what it is sent is the provider's: anything in
a message or a tool result reaches it. The person's own host account
is trusted by the host machine; the host sandbox narrows commands, not
the person.

### Reporting

`SECURITY.md` names the reporting address and the response times. A
report of a working exploit is handled privately until a fix ships.

## Not in this spec

Each mechanism, which the spec named in its row builds.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every threat row names a test or an invariant, and every named test exists in the tree | `TestThreatModelNamesExistingTests`, which reads this spec's table | not built |
| `docs/security.md` renders the assets, boundaries and threats of this spec for operators | `TestSecurityDocumentMatchesSpec` | not built |
