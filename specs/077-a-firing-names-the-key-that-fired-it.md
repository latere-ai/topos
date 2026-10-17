---
title: "A firing names the key that fired it: a session a trigger's fire starts carries fired_by on its create, the token_use and authorization_details of the fire request's own token when a key minted it, so an authorizer can hold the session to that key"
status: testing
track: core
depends_on: [006-identity.md, 014-store.md, 022-triggers.md]
affects: [internal/auth/, internal/triggers/, internal/server/, internal/store/, internal/store/dir/, internal/store/postgres/, internal/store/postgres/migrations/, authorizer/]
effort: small
created: 2026-10-17
updated: 2026-10-17
author: changkun
---

# A firing names the key that fired it

## Overview

A person, or a CI job acting for them, fires a trigger with a token
minted from a key: a personal access token or a service account's key,
whose `authorization_details` say what the key may do. An installation
whose authorizer holds a session a key starts to that key's grants
wants the same for a session the key's fire starts, so a key given
`trigger.fire` alone starts sessions that reach no further than the
key.

The authorizer cannot do that today. A firing's `session.create` is
asked as the trigger's owner ([[022-triggers]]), with the claims the
apply kept, `org_id` alone, so the question names the trigger and the
firing and not the token that fired it.

This spec adds one field. The create a fire starts carries `fired_by`,
the fire request's own `token_use` and `authorization_details`, when a
key minted the token.

## Current state

- The fire route asks `trigger.fire` of its caller, then runs the
  firing in the trigger engine, which starts or continues the key's
  session through the server's actor (`internal/server/triggers.go`,
  `internal/triggers/engine.go`).
- The actor asks `session.create` as the owner: subject, issuer and sub
  the owner's, claims `{"org_id": ...}`, the resource the fields of
  every create with `initiator` the owner, `trigger_id` and `firing_id`
  (`internal/server/sessions.go`).
- The caller's verified claims, `token_use` and `authorization_details`
  among them, are read by the guard for the fire route's own question
  and reach nothing after it.
- A firing is a row of `trigger_firings` ([[014-store]]) that keeps its
  envelope, so a firing held while the session it continues waits for a person
  is rendered and started later, by the minute loop or by a later fire,
  when the token of the fire that recorded it is gone.

## Design

### The field

`session.create` of a firing's session carries, beside `trigger_id`
and `firing_id`:

```json
"fired_by": {
  "token_use": "pat",
  "authorization_details": [
    {"type": "latere-authz", "actions": ["topos:trigger.fire"]}
  ]
}
```

- `token_use` is the fire request's own `token_use` claim, and
  `authorization_details` its `authorization_details` claim, each as
  the verifier read it, unparsed. A token that carries no
  `authorization_details` gives a `fired_by` without the member.
- `fired_by` is present when the fire request's token carries a
  non-empty string `token_use`, which a token carries only when it was
  minted from a key (`authkit.Identity.TokenUse`). It is absent for a
  person's own sign-in and for a service's token, such as an
  installation's own delivery of an event to the fire route.
- It is a field of the resource, not claims of the request. The
  create's claims stay the owner's context alone, so the create is not
  narrowed as a request of the key's own would be: a key that grants
  `trigger.fire` and nothing else, the CI case, still fires. The
  authorizer reads the field and decides how the key bounds the
  session.
- toposd reads nothing in it. It is passed through as data, and the
  owner policy, which decides with no authorizer configured, does not
  read it.

### Which firings carry it

| Firing | `fired_by` |
|---|---|
| A fire whose token a key minted, of an event or of a schedule fired now, that starts a session | the token's two claims |
| The same fire continuing an open session under the continue policy | none: it asks `session.send`, and the session's create already named the key that started it, if any |
| A person's own sign-in, or a service's token, firing the trigger | none |
| A schedule's firing in the minute loop | none |
| A firing held while the session it continues waits for a person, whose session the minute loop or a later fire starts once that session has ended | the `fired_by` the firing was recorded with |
| A firing that failed, or that a replica left unrecorded, fired again | the `fired_by` it was first recorded with; when it has none, that of the fire that runs it again |

### Kept with the firing

The firing keeps its `fired_by` beside its envelope, so the session it
starts names the key whenever it starts. `trigger_firings` gains a
`fired_by` column (migration `0012_firing_fired_by`), the JSON of the
object, `''` for none and for every firing written before it; the
directory store's firing file gains `fired_by`, omitted when empty. A
claim stores the fire's `fired_by` with a new firing, and a claim that
takes a failed or unrecorded firing again keeps the one stored when it
has one, and stores the new claim's only when it has none. Nothing
reads it but the start of the firing's session; the firings list does
not answer it.

Each firing names its own key and never another's. A fire under the
continue policy first sends or starts the held firings of its session
key ([[022-triggers]]), in order, and each names the key it was recorded with, whatever the fire's
own token is: a later fire never lends its key to an earlier firing,
and never takes a held firing's key away.

### Decisions

| Decision | Chosen | Alternatives and why not |
|---|---|---|
| Where the key rides | a field of the create's resource | claims on the create's request: the create would then be narrowed as the key's own request, and a key that grants `trigger.fire` alone would need `session.create` to fire |
| What it carries | the two claims verbatim | the grants parsed and re-encoded, or `token_use` checked against the key classes: what a grant and a class mean is the authorizer's, and passing the claims as read keeps toposd out of that decision |
| When it is sent | a non-empty `token_use` | a list of key classes in toposd: an empty value already means "not minted from a key", and a class an authorizer does not narrow it ignores |
| A held firing | names the key it was recorded with, kept on the firing row | none: the session would be decided as the owner's, wider than the key that fired it. The grants kept are the ones the key had when it fired, which can only narrow the session against the owner's reach; an authorizer that wants a key's later change to count reads the key's current grants by what `fired_by` names |
| A failed firing fired again | the key it was first recorded with; the rerunning fire's when it has none | the rerunning fire's always: a sign-in's rerun of a key's firing would widen it to the owner's reach. Comparing two keys' grants to keep the narrower: what a grant covers is the authorizer's to read, so toposd keeps the first, the key whose fire recorded the event |

## Compatibility

An authorizer that does not read `fired_by` decides as before. An
authorizer that holds a key's session to the key rolls out before a
toposd that sends the field; toposd before this release sends none,
and the authorizer decides those firings as the owner's, as it did.

## Not in this spec

A field naming the key itself, such as its id: the claims carry no key id, and an authorizer that needs one
reads it from the grants or the subject. The fire route's own question,
`trigger.fire`, which is asked with the caller's token as before.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A fire whose token a key minted asks its session's create with `fired_by`, that token's `token_use` and `authorization_details` as read, while the create's claims stay the owner's context alone; a key's manual fire of a schedule does the same | `internal/server.TestAFiringNamesTheKeyThatFiredIt` | built |
| A sign-in's fire and a schedule's firing in the minute loop name no key | `internal/server.TestAFiringNamesTheKeyThatFiredIt` | built |
| A key's held firing whose session a sign-in's later fire starts, and one the minute loop starts, name the held firing's key; a sign-in's held firing whose session a key's later fire starts names none | `internal/server.TestAFiringNamesTheKeyThatFiredIt` | built |
| A key's failed firing fired again by a sign-in keeps its key, and a sign-in's failed firing fired again by a key names that key | `internal/server.TestAFiringNamesTheKeyThatFiredIt` | built |
| Every store keeps a firing's `fired_by` through the claim, the held list and the firings list, and a failed or unrecorded firing taken again keeps the stored one over the new claim's: memory, directory and Postgres | `internal/store/storetest`'s `the key that fired a firing` case, run by each store's conformance test | built |
| A firing written before the column, or by a replica of the earlier release after it, reads back with no key | `internal/store/postgres.TestAFiringBeforeTheKeyColumnNamesNoKey` (postgres tier) | built |
| `fired_by` is the two claims verbatim, without `authorization_details` when the token has none, and absent for an empty or non-string `token_use` | `internal/auth.TestFiredByIsTheKeysClaimsVerbatim` | built |

## Outcome

Built on 2026-10-17, in no release yet. Every criterion has its test.
`TestAFiringNamesTheKeyThatFiredIt` failed against the code before the
change at its first case, a key's firing asked no `fired_by`; against a
first build that kept no key with a firing, at its held-firing case;
and with the store's retake rule removed, at the sign-in's rerun of a
key's firing, as the conformance case does. Nothing diverged from the
design.
