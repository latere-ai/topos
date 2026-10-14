---
title: "One owner's triggers and agents, and an agent deleted: the trigger and agent lists take `owner=<subject>`, which their questions carry, and `DELETE /v1/agents/{ref}` deletes an agent with its versions once no session of it is left"
status: testing
track: core
depends_on: [003-manifest.md, 006-identity.md, 014-store.md, 015-api.md, 018-credentials-and-secrets.md, 022-triggers.md, 036-organization-owners.md, 040-session-deletion.md]
affects: [authorizer/, internal/auth/, internal/identity/, internal/server/, internal/store/, test/stubs/idpstub/, api/]
effort: small
created: 2026-10-14
updated: 2026-10-14
author: changkun
---

# One owner's triggers and agents, and an agent deleted

## Overview

An installation that deletes what a person kept when the person's
account is closed has to find that person's triggers and agents, and
remove each. Its authorizer can let a service account do it, the way
it lets one delete a session ([[040-session-deletion]]), but the API
gives the service no way to ask:

- A list cannot name whose objects it wants. `GET /v1/triggers` asks
  `trigger.list` naming no owner and lists the owners the allow's
  filter names, and `GET /v1/agents` lists the caller's own context
  whatever the filter names. A service account has no context of the
  person's, and an authorizer cannot tell which owner it is asked
  about.
- Nothing deletes an agent. `POST /v1/agents/{ref}/archive` keeps the
  agent and its versions, and archives its identity at the identity
  provider first ([[018-credentials-and-secrets]]). A provider deletes
  a person's agent identities with the person, so the archive of a
  closed account's agent is refused on every try.

This spec adds both:

| Capability | What a client sends |
|---|---|
| one owner's triggers, a page at a time | `GET /v1/triggers?owner=<subject>` |
| one owner's agents, a page at a time | `GET /v1/agents?owner=<subject>` |
| an agent deleted with its versions | `DELETE /v1/agents/{ref}`, asked as `agent.delete` |

## Current state

- `listTriggers` asks `trigger.list` with no fields and lists the
  owners the allow's filter names, every trigger for an allow with no
  filter (`internal/server/triggers.go`). A trigger's answer does not
  name its owner.
- `listAgents` asks `agent.list` with no fields and lists the agents of
  the caller's context; an allow whose filter does not name the
  context lists none (`internal/server/agents.go`).
- `deleteTrigger` asks `trigger.delete` of a trigger found by id,
  whoever owns it, and removes it with its firings and keys; the
  sessions it started keep running.
- `deleteSession` removes a session, its log and its blobs, and
  refuses a running session with `conflict`.
- `archiveAgent` archives the identity at the provider, then the agent
  in the store, then disables the identity once no session of the
  agent is left; `Reconcile` disables an archived identity whose agent
  the store does not hold or holds archived with no session left. The
  provider answers an identity it does not hold with 404
  `unknown_agent`, which the archive route answers as
  `identity_refused`.
- No store deletes an agent: `store.Agents` has no delete, and the
  Postgres store's `agent_versions` references `agents` without a
  cascade.
- A session pins its agent's version. The runner reads the version's
  bundle from the session's own blob, but a send, a change of the
  session, a resume and a failover read the version from the store,
  and the sessions list reaches every session through the agents of the
  caller's context.

## Design

### The owner on the lists

```http
GET /v1/triggers?owner=https%3A%2F%2Fid.example.com%7C3f2a9c1e&limit=50
GET /v1/agents?owner=https%3A%2F%2Fid.example.com%7C3f2a9c1e
```

- `owner` is a rendered subject, as an object's owner is stored and
  every question names it: the person who applied a trigger, and an
  agent's owner, a person's subject or an organization's rendered the
  same way. An empty value is no filter.
- The question carries it: `trigger.list` and `agent.list` name
  `owner` with the value when the request names one, and name nothing
  otherwise, as today.
- The list holds the named owner's objects when the allow names no
  filter or its filter names the owner, and is an empty page
  otherwise. The parameter narrows what the allow admits and widens
  nothing; a caller cannot tell an owner the allow left out from one
  that holds nothing.
- For agents, a named owner replaces the caller's context as what the
  list holds. Without `owner` both lists are as they were.
- The answer is unchanged: every item of a list that named an owner is
  that owner's.

### The delete

`DELETE /v1/agents/{ref}` takes no body and answers 204.

1. The agent is found as every agent route finds it, by its `agent_`
   id whoever owns it or by a name among the agents of the caller's
   context, and the route asks `agent.delete` of it, with the fields
   every agent question carries, `name` and `owner`. A deny is
   `forbidden` for a caller who may read the agent and `not_found`
   otherwise ([[006-identity]]).
2. A session of the agent that the store holds, of any status,
   archived or not, refuses the delete with `conflict`: delete the
   agent's sessions first. The question comes before the check, so a
   caller the authorizer refuses learns nothing of the agent's
   sessions, and an authorizer does not act on an allowed
   `agent.delete` alone, since the delete can still be refused after
   it.
3. On an installation with an identity provider, an agent that has an
   identity has it archived there first, as the archive route does: a
   refusal fails the delete as `identity_refused` and an unanswered
   call as `identity_unavailable`, and the agent stays. An identity the
   provider answers `unknown_agent` for is gone, and the delete goes
   on: a provider deletes a person's agent identities with the person.
4. The store removes the agent and every version, each with its
   resolved document and its bundle, at once: Postgres in one
   transaction; the directory store the agent's file first, which is
   the commit record of its versions, then its versions' directory. The
   agent's name is then free in its owner's context, and an apply of
   the name creates a new agent with a new id.
5. The identity is disabled with `permanent: true`, since no session of
   the agent is left. An identity the provider no longer holds is
   gone. A disable that fails is logged, and the reconcile pass
   disables the identity, since it lists the archived identity and the
   store holds no agent of it ([[018-credentials-and-secrets]]).

An archived agent is deleted as any other. A second delete is
`not_found`. A session created between the check and the removal names
an agent the store no longer holds; the window is the time between two
store calls and is not closed.

A trigger that names the agent is not checked. It stays listed, since
triggers are listed by owner, and each of its firings is `refused`
with `not_found` at the session's create, as a firing of an archived
agent's trigger is refused. Sessions are checked because a session of
a deleted agent could be neither listed nor continued.

The owner policy ([[006-identity]]) decides `agent.delete` as every
action on an owned object: the agent's owner and the admins may.

### The vocabulary and the store

- `agent.delete` joins spec 006's table and `authorizer`'s after
  `agent.archive`, acting on `agent`.
- `store.Agents` gains `DeleteAgent(ctx, id) error`, `ErrNotFound` for
  an agent the store does not hold, which the memory, directory and
  Postgres stores implement and `storetest` holds.
- `identity.Unknown(err)` reports a provider's `unknown_agent`, so the
  server reads it without matching a string.

### Decisions

| Decision | Chosen | Alternatives and why not |
|---|---|---|
| How a list names the owner | `owner=<subject>`, carried as `owner` on the list's question | a route per owner: two new routes for what is a filter; the allow's filter alone: an authorizer cannot tell which owner a caller wants |
| An owner the allow leaves out | an empty page | `forbidden`: the answer would tell an owner the caller may not see from one that holds nothing |
| Sessions of the agent | `conflict` while any is held | delete them with the agent: a session of an organization's agent belongs to the member who started it; leave them: a session of a deleted agent is in no list and cannot take a turn |
| Triggers that name the agent | not checked | refuse: a trigger is listed by owner and already fires into a refusal for an archived agent |
| An identity the provider no longer holds | gone | refuse the delete: a closed account's agent could then never be deleted |
| What the store removes | the agent and every version with its document and bundle | keep the versions: a version is only read through its agent |

## Compatibility

A client that sends no `owner` sees no difference. A core before this
release ignores `owner` and asks the lists as before, and answers
`DELETE /v1/agents/{ref}` with `not_found`, as it answers every route
it does not hold, which reads like an agent already gone. A client
that relies on either checks the core's version first.

## Not in this spec

A list of every owner's triggers or agents for a service account. A
delete of an agent's sessions, or of the triggers that name it, with
the agent. A delete of an agent in the local CLI's store.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `GET /v1/triggers?owner=` lists only that owner's triggers when the allow names no filter or names the owner, an empty page otherwise, and `trigger.list` carries `owner`; without it the list and the question are as before | `internal/server.TestTheListsNameAnOwner` | built |
| `GET /v1/agents?owner=` lists that owner's agents in place of the caller's context when the allow names no filter or names the owner, an empty page otherwise, and `agent.list` carries `owner`; without it the list and the question are as before | `internal/server.TestTheListsNameAnOwner` | built |
| `DELETE /v1/agents/{ref}` asks `agent.delete`, removes the agent and every version, answers 204 and then `not_found`, frees the name for a new agent, and deletes an archived agent; another caller is refused | `internal/server.TestDeleteAnAgent` | built |
| An agent with a session of any status, archived included, is `conflict` and stays; once its sessions are deleted it is deleted | `internal/server.TestAnAgentWithSessionsIsNotDeleted` | built |
| The identity is archived, then disabled with `permanent: true`; one the provider no longer holds is gone and the delete succeeds; an unanswered provider refuses the delete and leaves the agent; a disable that fails is caught by the reconcile pass | `internal/server.TestDeleteAnAgentsIdentity` | built |
| Every store removes an agent with its versions, frees its name and answers `ErrNotFound` for one it does not hold: memory, directory and Postgres | `internal/store/storetest`'s `DeleteAgent` case, run by each store's conformance test | built |
| `agent.delete` is in the vocabulary and spec 006's table, and the owner policy lets the owner and the admins delete an agent and no one else | `authorizer.TestVocabularyMatchesTheSpec`, `internal/auth.TestOwnerPolicyRows` | built |
| The API document names the route and the parameter | `internal/server.TestOpenAPIIsGenerated` | built |

## Outcome

Built on 2026-10-14, in no release yet. Every criterion has its test,
and each test of the server's failed against the code with its part
taken out: the named owner on either list, the owner on the question,
the check of the agent's sessions, the read of an identity the provider
no longer holds, and the disable after the delete.

What was decided while building:

- **The helpers are shared by both lists.** `namedOwner`, `listFields`
  and `admits` read the parameter, build the question's fields and
  decide whether the allow admits the owner, so an owner the allow
  leaves out is an empty page on both lists alike.
- **The identity provider's code has a name.** `identity.CodeUnknownAgent`
  and `identity.Unknown` read the provider's 404 `unknown_agent`; any
  other refusal, a 404 of another code among them, still refuses the
  delete.
- **The stub provider deletes an owner's identities.**
  `idpstub.DeleteOwner` removes them as a provider does when it deletes
  the person, so the test of a closed account's agent runs against the
  answers a provider gives, not a queued failure.

