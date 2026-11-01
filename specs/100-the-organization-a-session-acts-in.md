---
title: "The organization a session acts in: a hosted session records the organization that owns its agent, TOPOS_DECISIONS_ORG=agent keys the session's calls at the decision service by that organization, and each agent.tool_use records the organization its decision was sent under"
status: testing
track: core
depends_on: [036-organization-owners.md, 099-hosted-sessions-decided-by-a-decision-service.md]
affects: [session/, harness/, internal/decisions/, internal/config/, internal/server/, cmd/toposd/, api/openapi.yaml, docs/]
effort: small
created: 2026-11-01
updated: 2026-11-01
author: changkun
---

# The organization a session acts in

## Overview

A decision service learns which calls a person approves, and it keys
each record by an organization, a subject and an action
([[037-decision-services]]). Hosted Topos sends each call with the
initiator's subject as the organization and as the subject
([[099-hosted-sessions-decided-by-a-decision-service]]). Thus each
person has one context at the service. An organization cannot state
the costs for the calls of its own agents. A call of an organization's
agent is learned under the person who started the session.

This spec records on each hosted session the organization that owns its
agent. An installation can then key the calls of such a session by that
organization. Each call records the organization that its decision was
sent under, so that a client can read the right record.

## Current state

- An agent is owned by a person or by an organization
  ([[036-organization-owners]]). `session.create` and `session.fork`
  name the owner as `agent_owner`. The session does not record it.
- `decisions.Decider` keys a call by `Options.Org`, or by the
  initiator's subject when it is empty. `toposd` never sets
  `Options.Org`.
- `agent.tool_use` records `decided_by` and the `suggestion`. It does
  not record the key that the call was sent under.

## Design

### The session's organization

`Session.Org`, `org` in the API and the log header, is the ID of the
organization that owns the session's agent. It is the `id` that
`session.create` and `session.fork` name in `agent_owner` as
`{type: organization, id}`. It is empty when a person owns the agent.

- The server sets it at the create and at the fork, from the same value
  that the question names. This includes an agent that a person applied
  in an organization's context before [[036-organization-owners]]: its
  identity is the organization's, and its sessions name the
  organization.
- No event changes it.
- A session created before this spec has no `org`. Topos reads it as a
  session of a person's agent.
- A local session (`topos run`) has no `org`.

The value is the organization's ID as the token's `org_id` claim names
it. It is not the rendered subject `<issuer>|<org_id>`. A decision
service that keys a person's own context by the person's rendered
subject, as Topos does, can tell an organization from a person only by
the `|`. An organization's ID has none.

### The organization a call is keyed by

`TOPOS_DECISIONS_ORG` selects the organization of each call's key. The
subject is always the initiator, and `action_id` does not change.

| Session | `initiator` (the default) | `agent` |
|---|---|---|
| of a person's agent | the initiator's subject | the initiator's subject |
| of an organization's agent | the initiator's subject | the session's `org` |

- The initiator of a hosted session is always a person. The initiator of
  a session that a trigger starts is the person who owns the trigger.
- The default keeps the keys of v0.67.0. Clients of a decision service
  read the person's own context today. A default that moved new calls
  to the organization would hide those calls from such a client. An
  installation sets `agent` after its clients and its authorizer of the
  decision service read organizations.
- `toposd serve` and `toposd runner` both read the variable. Every
  process of one installation must have the same value, because an
  answer is keyed as its decision is.
- The value is refused without `TOPOS_DECISIONS_URL`, as the token and
  the audience are.

`decisions.Options.Org` changes from a fixed organization for every
session, which no caller set, to this choice: `decisions.OrgInitiator`
or `decisions.OrgAgent`.

### What the log records

`agent.tool_use` gains `decision_org`: the organization of the key that
the call's decision was sent under. The decider sets it on
`harness.Decision.Org` wherever it sends the decision, and the harness
records it. It is empty when no decision service is configured, in
`plan`, where the service is not asked, and on an event written before
this spec.

A client that reads what the service learned about a call, such as the
explanation of one call, reads the organization from `decision_org`. On
an event without it, the client uses the initiator's subject, which was
the key before this spec.

### A person's answers in two contexts

A person's answers in an organization's context and in their own
context are separate. They have different keys, and the service keeps
each key's records apart. Nothing that a person answered in one context
changes a prediction in the other.

A person who starts to work in an organization has no answers there.
The service then has too few answers about them. In `learned`, the rules
and the risk score decide such a call, so the start asks the person no
more than it would without a service.

### Profiles

Topos sends `actor.id`, the agent's ID, on every call, as before. A
service whose cost profiles have an organization and an agent scope can
thus apply the agent's own profile, then the organization's, then its
own default. Which profile applies is the service's decision, not
Topos's.

### Migration

None. Records sent before the change stay under the initiator's
subject. A cost profile that a person saved for an organization's agent
in their own context stops applying to that agent's calls when
`agent` is set, because the calls then have the organization's key.

### Known limitation

A call that waits for a person while a restart changes
`TOPOS_DECISIONS_ORG` sends its answer under the new key. The service
has no decision for that key, so it refuses the answer. The decider
logs the failure, and the call continues as it does when an answer is
not sent.

## Not in this spec

- Who can read and set an organization's records at the decision
  service. That is the decision service's authorizer, which the
  installation runs.
- How a client shows an organization's context.
- A report over the members of an organization.
- A member's own cost profile in an organization.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A session of an organization's agent records the organization's ID as `org`, a fork of it records the same, the API answers it, and a session of a person's agent records none | `internal/server.TestASessionRecordsTheOrganizationOfItsAgent` | built |
| An agent applied in an organization's context before spec 036, whose identity the organization owns, gives its sessions the organization's `org` | `internal/server.TestAnOlderAgentsSessionNamesTheOrganization` | built |
| Each session store keeps `org` | the session store conformance suite's `CreateGet` | built |
| With `initiator`, every call is keyed by the initiator. With `agent`, a call of a session with an `org` is keyed by it and every other call by the initiator. An answer has the key of its decision | `internal/decisions.TestTheOrganizationOfACallsKey` | built |
| `agent.tool_use` records `decision_org` wherever the decision was sent, and none in `plan` or without a decision service | `internal/decisions.TestTheDecisionNamesItsOrganization`, `harness.TestToolUseRecordsTheDecisionsOrganization` | built |
| `TOPOS_DECISIONS_ORG` is read in serve and runner, defaults to `initiator`, and refuses another value and a value without `TOPOS_DECISIONS_URL` | `internal/config.TestDecisions`, `cmd/toposd.TestDeciderFromTheConfiguration` | built |
| The decider that toposd builds sends a call of an organization's session under the organization with `agent`, and under the initiator when unset or `initiator` | `cmd/toposd.TestTheDeciderKeysByTheConfiguredOrganization`; the unset case end to end in `cmd/toposd.TestServeAsksTheDecisionServiceWithItsOwnToken` | built |
| The OpenAPI document has `org` on a session and `decision_org` on `agent.tool_use`, and states the rule on a session's read and its events | `internal/server.TestOpenAPIIsGenerated`, `internal/server.TestOpenAPIStatesTheOrganization` | built |

## Outcome

Built as designed, behind the default `initiator`, so no key changes
until an installation sets `agent`. The spec stays at `testing` until
an installation runs with `agent` and its decision service shows a call
of an organization's agent under the organization.

- **The decider stamps the key once.** `Decide` handles `plan` first,
  which sends nothing, and every other path sends the decision under
  the call's key. It then sets `Decision.Org` from that key in one
  place, so no path can send a decision and record another
  organization.
- **The harness keeps the decider's organization.** The harness can
  narrow a decision after the decider returns it: a tool that holds the
  call, `plan` over a held call, a question. Each of these builds a new
  `Decision`. The harness reads the organization before it narrows, so
  the event names the key that the service holds.
- **No end-to-end test of `agent` through `toposd serve`.** The local
  issuer that `toposd token` signs with names no organization, so a
  test cannot apply an organization's agent through the served API.
  `internal/server` proves the session's `org` with an authorizer that
  knows an organization, and `cmd/toposd` proves that the decider
  `toposd` builds from the configuration keys such a session by it.
