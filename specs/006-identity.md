---
title: "Identity: subjects, verification, the authorizer question, the action vocabulary, the owner policy, the local issuer"
status: dispatched
track: core
depends_on: [001-architecture.md, 002-scaffold-and-configuration.md, 004-session-log.md]
affects: [authorizer/, internal/auth/, internal/token/, internal/config/]
effort: medium
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Identity

## Overview

`toposd` knows who is calling and asks someone else what they may do.
Who: a bearer token from any OpenID Connect issuer the operator lists,
or from the local issuer, verified through
`latere.ai/x/pkg/authkit/jwt`, rendered as the subject `<iss>|<sub>`.
What: one question per action to the installation's authorizer through
`latere.ai/x/pkg/authz`, with the resource the action acts on, and the
built-in owner policy when no authorizer is configured. toposd decides
nothing about a person (invariant 7 of [[001-architecture]]). This
spec owns the subject form, the verification rules, the action
vocabulary the `authorizer` package publishes, the `limits` an allow
may carry, the owner policy, and the local issuer with `toposd token`.

## Current state

v0.7.0 was a library and verified nobody. The retired hosted service
decided from token claims directly, ran its own OIDC client, and
published no action vocabulary, so no installation-wide authorizer
could govern it. Nothing of that is carried. The shape here is the one
Lux's and Cella's identity specs use: the shared verifier, the shared
authorizer envelope with its client, cache, retry, stub and conformance
suite, and a core that adds only its vocabulary and its resource
shapes.

## Design

### Subjects and verification

| Rule | Value |
|---|---|
| issuers | `TOPOS_OIDC_ISSUERS`, each read at start through its discovery document; an issuer that does not answer at start stops the start ([[002-scaffold-and-configuration]]) |
| audience | a token must carry one of `TOPOS_OIDC_AUDIENCE`, default `topos` |
| age | a token older than authkit's `MaxTokenAge` by its `iat`, or with no `iat`, is refused whatever its `exp` |
| transport | `https` issuers only, except those in `TOPOS_OIDC_INSECURE_ISSUERS` and loopback |
| grants | the validator reads grants (`ReadsGrants` true): a narrowed key's grants restrict the authorizer's answer through `authz.Restrict` |
| subject | `authz.Subject(iss, sub)`: the issuer, a vertical bar, and the `sub` claim; the one form every event, owner and sender carries |

A request with no bearer, a bad signature, an unknown issuer, a wrong
audience or an expired token answers `unauthenticated`
([[015-api]]). Runner routes on the internal listener take
`TOPOS_RUNNER_TOKEN`, not a person's token ([[016-runners]]).

### The question

Every API action asks one question, the `authz.Request` envelope:
`subject`, `issuer`, `sub`, the verified `claims` verbatim, the
`action` below, the `resource` (`kind`, `id`, and the fields the
table names), and the caller's request id, address and user agent.
The client, its cache (an allow for the answer's `ttl`, default 60 s,
at most 600 s; a deny for 5 s), its retry and its failure rule are
`authz.Client`'s. An answer toposd could not get is a refusal,
`authorizer_unavailable`, never an allow. A denied read, get or list
of one object answers `not_found`, so a deny does not disclose that the
object exists; a denied mutation of an object the caller may read
answers `forbidden`.

### The action vocabulary

The `authorizer` package publishes the vocabulary as
`authz.Vocabulary` with the core name `topos`, one constant per
action, the resource kind each acts on, and the heading per kind
(Agents, Sessions, Triggers, Credentials, Memory stores). An action
never changes its string and never disappears; a resource shape only
gains fields.

| Action | Kind | Resource fields | Asked at |
|---|---|---|---|
| `agent.create`, `agent.read`, `agent.list`, `agent.update`, `agent.archive` | `agent` | `name`, `owner`, `identity` | the agent routes of [[015-api]] |
| `session.create` | `session` | `agent`, `agent_version`, `agent_owner`, `runner`, `machine`, `initiator` | session create; the authorizer applies the initiator cap here |
| `session.read`, `session.list` | `session` | `agent`, `owner`, `runner` | session get, list, events list, stream |
| `session.send` | `session` | `agent`, `owner`, `runner`, `sender`, `event_type` | sending a user event; the authorizer applies the sender rule here |
| `session.interrupt`, `session.end`, `session.delete` | `session` | `agent`, `owner` | those routes |
| `session.fork`, `session.rewind` | `session` | `agent`, `owner`, `seq` or `turn` | [[017-external-runners-handoff-fork]], [[034-checkpoints-and-rewind]] |
| `session.resume` | `session` | `agent`, `owner`, `stop_reason`, `max_cost_usd_micro` | resuming a session idle with `budget`; the decision's `limits` carry the raised cap ([[007-models]]) |
| `session.redact` | `session` | `agent`, `owner`, `event_id` | [[015-api]], [[018-credentials-and-secrets]] |
| `session.append`, `session.handoff` | `session` | `agent`, `owner`, `runner`, `writer`, and for a handoff `to` and `initiator` | [[017-external-runners-handoff-fork]]; a handoff to `hosted` gets the initiator cap of a create |
| `session.scope` | `session` | `agent`, `owner`, `old`, `new`, `until` | a scope change; the authorizer holds a widening to the agent's permissions and the widener's own rights |
| `trigger.create`, `trigger.read`, `trigger.list`, `trigger.update`, `trigger.delete` | `trigger` | `agent`, `owner` | the trigger routes |
| `credential.create`, `credential.read`, `credential.list`, `credential.delete` | `credential` | `name`, `owner`, `service` | the credential routes; `read` returns metadata only |
| `memory_store.create`, `memory_store.read`, `memory_store.list`, `memory_store.update`, `memory_store.delete` | `memory_store` | `name`, `owner`, `sharing`, `audience`, and `partition` on a read of another initiator's documents | the memory store routes; `shared` is allowed, by default, to an organization admin ([[020-memory-stores]]) |
| `memory_store.write` | `memory_store` | `name`, `owner`, `session`, `initiator`, `partition` | a document write, and a session attaching the store `read_write` ([[020-memory-stores]]) |

The initiator cap and the sender rule are the authorizer's policy, not
toposd's: toposd passes the initiator and each sender in the resource,
and the authorizer refuses a session whose agent's permissions exceed
what its initiator could do, and a send from someone who could not
have started the session.

### Limits

An allow of `session.create` may carry a `limits` object, decoded with
`Decision.DecodeLimits` into the members below. The session takes the
lowest of each figure against the agent's and the request's own
([[005-harness-loop]]), and merges the lists and thresholds with the
agent's so neither loosens the other ([[012-permissions-and-approvals]]).

| Member | Meaning | Used by |
|---|---|---|
| `always_confirm`, `always_allow` | lists of patterns | [[012-permissions-and-approvals]] |
| `thresholds` | `{flag_at, ask_at, block_at}` | [[012-permissions-and-approvals]] |
| `budget_usd_micro` | a ceiling on the session's budget | [[007-models]] |
| `turn_timeout`, `max_age` | Go durations, ceilings | [[005-harness-loop]], [[004-session-log]] |
| `scope` | the session's starting scope, a list of grants | [[018-credentials-and-secrets]] |
| `retention` | a Go duration: how long the session is kept after it ends; absent, it is kept until deleted | [[014-store]] |

A member toposd does not know is ignored; a member it knows that does
not decode refuses the create with `authorizer_unavailable`, because a
ceiling that cannot be read is not applied.

### Session attribution

A call a session makes to another core carries a token for the agent
whose `session` claim names the session and its workload
([[018-credentials-and-secrets]]). The claim is authority, not
attribution: the core forwards it among the verified `claims` of the
envelope, and the installation's authorizer checks on every decision
that the session is live and applies its scope. Nothing beside the
token names the session, so no caller can attach a session to a request
it makes. toposd forwards a `session` claim on its public listener like
any other claim.

### The owner policy

With `TOPOS_AUTHORIZER_URL` unset, toposd decides with `authz.Policy`:
`Admins` from `TOPOS_ADMIN_SUBJECTS` act on every object; the subject
that created an object owns it and may take every action on it;
`Create` is allowed on an object that does not exist, and a session is
created only on an agent whose owner is the caller (`agent_owner`);
everything else is denied as `not_owner`, and a denied read answers
`not_found`. The
owner policy carries no limits, so a session on it has the core's
defaults. The policy calls `authz.Restrict` on its own answer, so a
narrowed key is narrowed without an authorizer too.

### The local issuer and `toposd token`

With `TOPOS_LOCAL_ISSUER_KEY` set, toposd is also an issuer named
`TOPOS_PUBLIC_URL`: it publishes its key set at
`/.well-known/jwks.json` under its public URL, and accepts the tokens
it signed. A key's `kid` is its RFC 7638 thumbprint; an ECDSA P-256
key signs ES256 and an RSA key RS256. Every token carries `iat`,
`nbf`, `exp` and a random `jti`, and a subject holding `|` is refused,
because the bar separates the issuer from the `sub`. `toposd token` signs one token and prints it:

| Flag | Default | Meaning |
|---|---|---|
| `--subject` | `admin` | the `sub` claim |
| `--ttl` | `1h` | lifetime, at most `24h` |
| `--audience` | the first of `TOPOS_OIDC_AUDIENCE` | the `aud` claim |

It opens no store and writes nothing. A self-hoster runs toposd with
the local issuer, the owner policy and `TOPOS_ADMIN_SUBJECTS` set to
the token's subject, and needs no identity provider.

### Error codes

| Code | Meaning |
|---|---|
| `unauthenticated` | the bearer is missing or does not verify |
| `forbidden` | the authorizer denied a mutation of an object the caller may read |
| `authorizer_unavailable` | no decision could be had, or its limits could not be read |

## Not in this spec

The routes and their HTTP statuses ([[015-api]]); the agent's identity,
its session tokens and the session scope's grants
([[018-credentials-and-secrets]]); the
runner token ([[016-runners]]); how an installation writes its
authorizer.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `pkg/authz/conformance` passes against the shared stub told the vocabulary, and against the owner policy served through `authz/server` | `authorizer.TestAuthorizerConformanceStub`, `internal/auth.TestAuthorizerConformanceOwnerPolicy` | built |
| The vocabulary table here and `authorizer.Vocabulary()` are the same set, each action with its kind | `authorizer.TestVocabularyMatchesTheSpec` | built |
| The guard answers a denied read `not_found`, a denied mutation `forbidden` when the caller may read the object and `not_found` when it may not, and no decision `authorizer_unavailable` | `internal/auth.TestGuardAnswers` | built |
| A denied read of an existing session answers `not_found` with the same body as an absent one | `internal/server.TestDeniedReadAnswersAsMissing`, `internal/server.TestAnotherSubjectsAgentIsNotThere` | built |
| With the authorizer down every API action answers `authorizer_unavailable` and nothing is written | `internal/server.TestAuthorizerDownIsRefusal` | built |
| A token older than the age bound, with a wrong audience, or from an unlisted issuer is refused `unauthenticated` | `internal/auth.TestVerificationRules` | built |
| An allow's limits decode member by member, a member toposd does not know is ignored, and one that does not decode refuses the create `authorizer_unavailable` | `authorizer.TestDecodeLimitsReadsEveryMember`, `authorizer.TestDecodeLimitsRefusesWhatItCannotApply`, `internal/auth.TestLimitsAtCreate` | built |
| A `session.create` allow with limits lowers the session's budget, turn timeout and age, and sets its retention and scope | `internal/server.TestLimitsApplyAtCreate` | built |
| The limits' `always_confirm`, `always_allow` and `thresholds` reach the session's permission policy | `TestLimitListsReachThePolicy` | not built: the session header keeps no permission lists yet |
| `toposd token` prints a token the same toposd accepts, refuses a `--ttl` over 24 h, and opens no store | `cmd/toposd.TestTokenRoleRoundTrip` | built |
| The owner policy lets the creator and the admin subjects act and denies everyone else, narrowed by a key's grants | `internal/auth.TestOwnerPolicyRows` | built |
