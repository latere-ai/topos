---
title: "Organization owners: an agent belongs to the context its first apply is made in, a person or an organization, and every agent name is read in the caller's context"
status: complete
track: core
depends_on: [006-identity.md, 014-store.md, 015-api.md, 018-credentials-and-secrets.md, 022-triggers.md]
affects: [internal/auth/, internal/server/, internal/store/, internal/store/dir/, internal/store/postgres/, internal/store/storetest/]
effort: medium
created: 2026-10-01
updated: 2026-10-01
author: changkun
---

# Organization owners

## Problem

An agent's owner is the subject of the person who first applied it
([[015-api]], "Agent names"). An organization reaches the core only
through the authorizer's allow of an apply, which names the owner the
agent's identity is created for, kept as `status.owner`
([[018-credentials-and-secrets]]). Everything else reads the applier:

- the name is unique within the applier's agents, so two admins of one
  organization who apply `reviewer` in its context hold two agents, and
  a member reads neither by name;
- every `agent.*` question names the applier as `owner`, so an
  authorizer cannot tell the organization's agent from the applier's
  own, and a list narrowed to the organization's subject finds nothing;
- `session.create` names the organization as `agent_owner` only when an
  identity provider created the identity for it.

An agent that belongs to an organization has to be held by the
organization in the store, named in it, and named as its owner in every
question, with the authorizer deciding who of the organization may do
what.

## Design

### The caller's context

A caller's context is the organization its token's `org_id` claim
names, the claim a trigger already keeps as the context of its apply
([[022-triggers]]), or the caller as a person when the claim is absent
or empty. The core reads the claim only to address objects, never to
decide: whether the caller may act in the organization stays the
authorizer's question ([[006-identity]]).

An organization is rendered as a subject the way a person is,
`authz.Subject(issuer, org_id)` with the caller's issuer, so it equals
the subject an authorizer of that issuer names the organization's space
by in a list's filter.

### The owner of an agent

An agent's owner is the context of the apply that creates it: the
organization, or the person. The store records the owner as a rendered
subject and its type, `user` or `organization`. The owner never
changes: an apply in another context does not reach the agent by name,
and an agent reached by its `agent_` id keeps its owner whoever applies
a version of it.

| Context of the creating apply | Owner (subject, type) | Who applied each version |
|---|---|---|
| personal | the caller's subject, `user` | `created_by` of the version |
| an organization | `<issuer>\|<org_id>`, `organization` | `created_by` of the version |

### Names

An agent's name is unique within its owner. Every agent name the API
reads is read among the agents of the caller's context: the `{name}`
of an apply, a `{ref}` that is not an `agent_` id, a session create's
`agent`, the `agent` filter of a session list, an Agent manifest's
subagent references and a Trigger manifest's `agent` ([[003-manifest]]).
A name the context does not hold answers as today, `not_found` or
`unknown_reference`, the same bytes whether another context holds one.
An `agent_` id names its agent in any context, and the authorizer
decides the action.

A trigger's owner stays the person who applies it ([[022-triggers]]);
trigger names stay unique per person.

### Lists

A list holds what the caller's context holds, narrowed further by the
owners the authorizer's allow names, so a context never lists another's
objects whatever the authorizer answers:

| List | Holds | The authorizer's owners narrow by |
|---|---|---|
| `GET /agents` | the agents the context owns | the agent's owner |
| `GET /sessions` | the sessions of the agents the context owns | the session's initiator |
| `GET /sessions/summary` | the counts of the sessions `GET /sessions` holds | the session's initiator |

A session list's question names the context it lists as `agent_owner`,
in the form an agent's `owner` takes below, so an authorizer may answer
a list with no initiator narrowing, an organization's admin seeing every
member's session of the organization's agents, knowing the core keeps
the list to the context. Topos reads the context's agents and lists the
sessions of those, so a session of an agent the context does not own is
in no list of it.

### The questions

| Question | Field | Value |
|---|---|---|
| `agent.read`, `agent.list`'s items, `agent.update`, `agent.archive` | `owner` | a person's agent: the owner's rendered subject, as before; an organization's: `{type: "organization", id: <org_id>}` |
| `agent.create` | `owner` | absent in a personal context, as before; in an organization's context `{type: "organization", id: <org_id>}` |
| `session.create`, `session.fork` | `agent_owner` | as `owner` above, from the agent's owner |
| `session.list` | `agent_owner` | the caller's context, as `owner` above: the caller's subject, or the organization |

An agent applied before this spec in an organization's context is
recorded as its applier's, while its identity was created for the
organization; its sessions keep naming the organization as
`agent_owner` from `status.owner`, as they did.

### The identity's owner

An agent's identity is created for the agent's owner
([[018-credentials-and-secrets]]). An allow of the apply that names an
owner names that one; an allow that names another owner refuses the
apply `authorizer_unavailable`, since an identity owned by one party for
an agent held by another would act for neither. An allow that names
none creates the identity for the agent's owner.

### The owner policy

With no authorizer configured, an organization owns nothing the policy
can decide, since the policy knows no organization's members: an
`agent.create` naming an organization as `owner`, and every action on an
agent whose `owner` is an organization, are denied `not_owner` to every
subject but `TOPOS_ADMIN_SUBJECTS`. A list stays narrowed to the
caller's own subject. An installation whose tokens name organizations
runs an authorizer that knows them.

### The store

`store.Agent` gains `OwnerType`, `user` when empty. Postgres migration
`0006_agent_owner_type` adds `owner_type text NOT NULL DEFAULT 'user'
CHECK (owner_type IN ('user', 'organization'))`; every row it finds is a
person's. The directory store keeps `owner_type` in the agent's file,
read as `user` when absent. `UNIQUE (owner, name)` and the owners filter
of a list are unchanged: an organization's subject is one more owner.

### The API surface

No route, field or code is added. The OpenAPI document's description of
an agent's `name` and of `PUT /agents/{name}` states the rule: an agent
belongs to the organization the caller's token names, or to the caller
as a person when it names none. The document's sentence is the one a
client reads to know the core keeps organizations' agents.

### Migration

None moves. Every stored agent is a person's, and stays the person's.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| An apply in an organization's context creates the organization's agent; a second caller of the organization applying the name updates it; the person's own agent of the name stays apart | `internal/server.TestAnOrganizationHoldsItsAgents` | built |
| Every name read is the context's: a get, a version, an archive, a session create and a session list filter by name, an agent's subagent reference | `internal/server.TestNamesAreTheContexts` | built |
| A list holds its context's agents, and its context's agents' sessions, whatever owners the authorizer names; a session list names its context as `agent_owner` | `internal/server.TestListsHoldTheirContext`, `internal/server.TestASessionListReadsEveryPageOfTheContextsAgents`, the session store conformance suite's agents filter | built |
| `agent.*` name an organization's agent as `{type: organization, id}`; `agent.create` carries it in an organization's context and nothing in a personal one; `session.create` and `session.fork` name it as `agent_owner` | `internal/server.TestTheQuestionsNameTheOrganization` | built |
| The identity is created for the agent's owner; an allow naming another owner refuses the apply `authorizer_unavailable` | `internal/server.TestAgentIdentityLifecycle`, `internal/server.TestAnAllowNamingAnotherOwnerRefuses` | built |
| The owner policy denies an organization's create and every action on an organization's agent to all but its admins | `internal/auth.TestOwnerPolicyRows`, `internal/auth.TestTheContextIsTheTokensOrganization` | built |
| Every store keeps an agent's owner type, `user` by default; Postgres migrates a table that holds agents with each row a person's | the store conformance suite's owner type; `internal/store/postgres.TestOwnerTypeMigratesATableThatHoldsAgents` (postgres tier) | built |
| The OpenAPI document states the rule | `internal/server.TestOpenAPIStatesTheOwner` | built |

## Decisions

| # | Decision | Why |
|---|---|---|
| 1 | The context is the token's `org_id`, read by the core to address, never to decide | the claim a trigger already keeps; every core of the family scopes an organization's objects by it; an explicit owner parameter would make every client spell the organization's subject |
| 2 | An agent's owner never changes | moving an agent between owners moves its identity, its spend and its readers; that is a new agent |
| 3 | Without an authorizer an organization's agent is its admins' alone | the owner policy knows no members, and "organizations closed" is the family's rule |

## Outcome

Built as designed, in `internal/auth` (the context, the owner policy),
`internal/store` and its three stores (the owner type, the lookup's two
namespaces), `session` (a list's agents filter) and `internal/server`.

- **A list is the context's, whatever the authorizer answers.** The
  agent list's owners are the context's subject, and an allow whose
  filter names others lists none; the session list reads every agent
  the context owns, page by page, and lists their sessions, narrowed by
  the authorizer's owners. An admin of `TOPOS_ADMIN_SUBJECTS` therefore
  lists their own context's agents and sessions, where before they
  listed every subject's; they still reach any object by its id.
- **The pinned agent keeps its identity's owner.** An agent applied in
  an organization's context before this spec is its applier's in the
  store while its identity is the organization's; its sessions still
  name the organization from `status.owner`, as before.
- **The sessions' summary** counts within the same scope as the list,
  since both read one scope (`sessionScope`): an admin's summary counts
  their own context's sessions.
- **The OpenAPI sentence** is `server.OwnerRule`, carried on the `name`
  parameter of every route that takes one.
