---
title: "Identity: subjects, verification, the authorizer question, the action vocabulary, the owner policy, the local issuer"
status: complete
track: core
depends_on: [001-architecture.md, 002-scaffold-and-configuration.md, 004-session-log.md]
affects: [authorizer/, internal/auth/, internal/token/, internal/config/]
effort: medium
created: 2026-09-27
updated: 2026-09-29
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
answers `forbidden`. A `forbidden` carries the deny's reason, when it is
a snake_case token, in `details.reason` ([[015-api]]); a session
create's reason is withheld when it names another subject's agent the
caller may not read.

### The action vocabulary

The `authorizer` package publishes the vocabulary as
`authz.Vocabulary` with the core name `topos`, one constant per
action, the resource kind each acts on, and the heading per kind
(Agents, Sessions, Triggers, Credentials, Memory stores). An action
never changes its string and never disappears; a resource shape only
gains fields.

| Action | Kind | Resource fields | Asked at |
|---|---|---|---|
| `agent.create`, `agent.read`, `agent.list`, `agent.update`, `agent.archive` | `agent` | `name`, `owner` | the agent routes of [[015-api]] |
| `session.create` | `session` | `agent`, `agent_version`, `agent_owner` (the owner's subject, or `{type, id}` for an organization's agent whose identity the authorizer created for the organization), `runner`, `machine`, `initiator`, `permissions` (the pinned version's `{action, resource}` list, then `lux:model.use` on each model the agent names: its own, its advisor's and its subagents'), `session_id`, `agent_identity`, and `trigger_id` and `firing_id` for a trigger's session, asked as the trigger's owner (added by [[022-triggers]]) | session create, and a trigger's firing; the authorizer applies the initiator cap here |
| `session.read`, `session.list` | `session` | `agent`, `owner`, `runner` | session get, list, events list, stream |
| `session.send` | `session` | `agent`, `owner`, `runner`, `sender`, `event_type` | sending a user event; the authorizer applies the sender rule here |
| `session.interrupt`, `session.end`, `session.delete` | `session` | `agent`, `owner` | those routes |
| `session.fork` | `session` | the fields of `session.create` for the new session (its `session_id`, the forker as `initiator`, the agent version's `permissions`, the `repositories`), and `owner`, `parent` and `seq` of the session forked, which is the resource's id (changed by [[017-external-runners-handoff-fork]]) | a fork; the authorizer decides it as a create of its initiator, initiator cap included |
| `session.rewind` | `session` | `agent`, `owner`, `turn` | [[034-checkpoints-and-rewind]] |
| `session.resume` | `session` | `agent`, `owner`, `stop_reason`, `max_cost_usd_micro` | resuming a session idle with `budget`; the decision's `limits` carry the raised cap ([[007-models]]) |
| `session.redact` | `session` | `agent`, `owner`, `event_id` | [[015-api]], [[018-credentials-and-secrets]] |
| `session.append`, `session.handoff` | `session` | `agent`, `owner`, `runner`, `writer`, and for a handoff `to` and `initiator` | [[017-external-runners-handoff-fork]]; a handoff to `hosted` gets the initiator cap of a create |
| `session.scope` | `session` | `agent`, `owner`, `old`, `new`, `until` | a scope change; the authorizer holds a widening to the agent's permissions and the widener's own rights |
| `session.update` | `session` | `agent`, `owner`, `runner`, `session_id`, and what the change names: `model` (the name of the model the session's next turn runs), `effort` (the reasoning effort it runs at, `""` for the model's own default), or `archived` (`true` or `false`, added by [[015-api]]) | a change of the session's model, its effort or both ([[015-api]]); asked after the model resolved, so the authorizer decides whether the session may use a model that exists, and may widen the session's model key to it; an effort change alone carries no `model` and reaches no new model; an archive or an unarchive carries `archived` alone |
| `trigger.create`, `trigger.read`, `trigger.list`, `trigger.update`, `trigger.delete` | `trigger` | `name`, `agent`, `owner`, and on a create and an update the filter applied as `on` (changed by [[022-triggers]]) | the trigger routes |
| `trigger.fire` (added by [[022-triggers]]) | `trigger` | `name`, `agent`, `owner` | deliver an event to a trigger, or fire it now |
| `credential.create`, `credential.read`, `credential.list`, `credential.delete` | `credential` | `name`, `owner`, `service` | the credential routes; `read` returns metadata only |
| `memory_store.create`, `memory_store.read`, `memory_store.list`, `memory_store.update`, `memory_store.delete` | `memory_store` | `name`, `owner`, `sharing`, `audience`, and `partition` on a read of another initiator's documents | the memory store routes; `shared` is allowed, by default, to an organization admin ([[020-memory-stores]]) |
| `memory_store.write` | `memory_store` | `name`, `owner`, `session`, `initiator`, `partition` | a document write, and a session attaching the store `read_write` ([[020-memory-stores]]) |

The initiator cap and the sender rule are the authorizer's policy, not
toposd's: toposd passes the initiator, the permissions of the agent
version the session pins, read from its bundle, and each sender in the
resource, and the authorizer refuses a session whose agent's
permissions exceed what its initiator could do, and a send from
someone who could not have started the session.

### Limits

An allow of `session.create` may carry a `limits` object, decoded with
`Decision.DecodeLimits` into the members below; an allow of
`agent.create` or `agent.update` may carry `owner`. The session takes the
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
| `owner` | `{type, id}`, `type` `user` or `organization`: on an agent's apply, the owner of an agent that gets its identity at the identity provider; absent, the applier as a person | [[018-credentials-and-secrets]] |

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
| A `forbidden` carries the authorizer's reason token and none for a deny without one or with prose; a `not_found` carries none; the reason about an object the caller may not read is withheld | `internal/auth.TestAForbiddenCarriesTheReason` | built |
| A denied read of an existing session answers `not_found` with the same body as an absent one | `internal/server.TestDeniedReadAnswersAsMissing`, `internal/server.TestAnotherSubjectsAgentIsNotThere` | built |
| With the authorizer down every API action answers `authorizer_unavailable` and nothing is written | `internal/server.TestAuthorizerDownIsRefusal` | built |
| A token older than the age bound, with a wrong audience, or from an unlisted issuer is refused `unauthenticated` | `internal/auth.TestVerificationRules` | built |
| An allow's limits decode member by member, a member toposd does not know is ignored, and one that does not decode refuses the create `authorizer_unavailable` | `authorizer.TestDecodeLimitsReadsEveryMember`, `authorizer.TestDecodeLimitsRefusesWhatItCannotApply`, `internal/auth.TestLimitsAtCreate` | built |
| A `session.create` allow with limits lowers the session's budget, turn timeout and age, and sets its retention and scope | `internal/server.TestLimitsApplyAtCreate` | built |
| `session.create` carries the permissions of the agent version the session pins, an older pinned version its own and an agent with none an empty list | `internal/server.TestSessionCreateCarriesTheAgentsPermissions` | built |
| The limits' `always_confirm`, `always_allow` and `thresholds` merge with the agent's approvals into the Session's `policy` at create, and a turn is decided by that policy rather than the agent's own | `internal/server.TestLimitListsReachThePolicy`, `harness.TestPolicyMergeNeverLoosens`, `harness.TestTheSessionsPolicyDecides` | built |
| `toposd token` prints a token the same toposd accepts, refuses a `--ttl` over 24 h, and opens no store | `cmd/toposd.TestTokenRoleRoundTrip` | built |
| The owner policy lets the creator and the admin subjects act and denies everyone else, narrowed by a key's grants | `internal/auth.TestOwnerPolicyRows` | built |

## Outcome

Built on 2026-09-27. toposd verifies every bearer through the shared
verifier, asks one question per action through `authz`, decides with
the owner policy when no authorizer is configured, and signs local
tokens with `toposd token`. Every row of the acceptance table passes.

### What was built

| Piece | Where |
|---|---|
| the action vocabulary, the resource kinds and their headings | `authorizer/actions.go` |
| the `limits` of a `session.create` allow and their decoding | `authorizer/limits.go` |
| verification, the subject form and the grants a narrowed key carries | `internal/auth/verifier.go` |
| the guard: `not_found` for a denied read, `forbidden` for a denied mutation, `authorizer_unavailable` for no answer, and the caller's request id, address and user agent in each question | `internal/auth/guard.go` |
| the owner policy | `internal/auth/policy.go` |
| the local issuer, its key set and `toposd token` | `internal/token`, `cmd/toposd` |
| the question of each route, the limits and the initiator cap's fields at create | `internal/server` (`agents.go`, `sessions.go`, `events.go`, `resume.go`) |
| the approval policy merged at create and applied by the harness | `harness/permission.go` (`Policy.Merge`, `Policy.Under`), `session.Policy` |

### What diverges from the design as written

| What it said | What was built | Why |
|---|---|---|
| a call a session makes to another core carries a token whose `session` claim names the session | no such token is minted yet; toposd forwards whatever claims a bearer carries | the agent's session tokens are [[018-credentials-and-secrets]]'s, not built |
| every action of the table is asked at its route | the agent routes and `session.create`, `read`, `list`, `send`, `interrupt`, `end`, `delete`, `redact` and `resume` are asked; the other actions are published for the routes that are not built | fork, rewind over the API, append and handoff are [[017-external-runners-handoff-fork]]'s, scope [[018-credentials-and-secrets]]'s, triggers [[022-triggers]]'s, credentials [[018-credentials-and-secrets]]'s, memory stores [[020-memory-stores]]'s |
| the session takes the lowest of each limit against the agent's and the request's own | the figures take the lowest; the lists and thresholds merge with the agent's approvals by [[012-permissions-and-approvals]]'s rule, and the mode is the agent's, since an allow carries none | a list has no lowest; the merge is what keeps either side from loosening the other |
| the merged policy reaches every session's permission policy | a hosted session records it in its header; a local `topos` session records none and is decided by its agent's approvals and `--mode` | a local session has no authorizer to merge with |
| the initiator cap compares the agent's permissions with the initiator's | `session.create` carries the pinned version's `permissions` for the comparison, which the authorizer makes | toposd decides nothing about a person |

### What this leaves open

| Open | Why |
|---|---|
| the `session` claim on a session's calls to other cores | [[018-credentials-and-secrets]] |
| the questions of the routes not built | the specs that build those routes ask them |
