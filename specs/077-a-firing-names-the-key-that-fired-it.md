---
title: "A firing names the key that fired it: a session a trigger's fire starts carries fired_by on its create, the token_use and authorization_details of the fire request's own token when a key minted it, so an authorizer can hold the session to that key"
status: testing
track: core
depends_on: [006-identity.md, 022-triggers.md]
affects: [internal/auth/, internal/triggers/, internal/server/, authorizer/]
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
  minted from a key (`authkit.Identity.TokenUse`). It is absent for a person's own sign-in and
  for a service's token, such as an installation's own delivery of an
  event to the fire route.
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
| The same fire continuing the key's open session | none: it asks `session.send`, and the session's create already named the key that started it, if any |
| A person's own sign-in, or a service's token, firing the trigger | none |
| A schedule's firing in the minute loop | none |
| A firing held while the key's session waits for a person, whose session the minute loop or a later fire starts | none |
| A firing that failed and is fired again | the `fired_by` of the fire that runs it again, none for a sign-in's |

The engine hands the key to the Actor's start of the firing its fire
request runs, and to no other. A fire under the continue policy first
sends or starts the key's held firings, in order: those name no key,
whatever the fire's own token is, so a later fire never lends its key
to an earlier firing.

### Decisions

| Decision | Chosen | Alternatives and why not |
|---|---|---|
| Where the key rides | a field of the create's resource | claims on the create's request: the create would then be narrowed as the key's own request, and a key that grants `trigger.fire` alone would need `session.create` to fire |
| What it carries | the two claims verbatim | the grants parsed and re-encoded, or `token_use` checked against the key classes: what a grant and a class mean is the authorizer's, and passing the claims as read keeps toposd out of that decision |
| When it is sent | a non-empty `token_use` | a list of key classes in toposd: an empty value already means "not minted from a key", and a class an authorizer does not narrow it ignores |
| A held firing | names no key; nothing is stored | the claims kept on the firing row: the store would keep a credential's reach beside a delivered event, applied after the token that carried it has expired and the key may have changed |
| A failed firing fired again | the fire that runs it names its own key | the first fire's key: no fire's key is kept, and the fire that runs it is the request whose token was verified |

## Compatibility

An authorizer that does not read `fired_by` decides as before. An
authorizer that holds a key's session to the key rolls out before a
toposd that sends the field; toposd before this release sends none,
and the authorizer decides those firings as the owner's, as it did.

## Not in this spec

Keeping a key with a held firing. A field naming the key itself, such
as its id: the claims carry no key id, and an authorizer that needs one
reads it from the grants or the subject. The fire route's own question,
`trigger.fire`, which is asked with the caller's token as before.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A fire whose token a key minted asks its session's create with `fired_by`, that token's `token_use` and `authorization_details` as read, while the create's claims stay the owner's context alone; a key's manual fire of a schedule does the same | `internal/server.TestAFiringNamesTheKeyThatFiredIt` | built |
| A sign-in's fire and a schedule's firing in the minute loop name no key | `internal/server.TestAFiringNamesTheKeyThatFiredIt` | built |
| A held firing whose session a later key's fire starts, and one the minute loop starts, name no key | `internal/server.TestAFiringNamesTheKeyThatFiredIt` | built |
| A key's failed firing fired again by a sign-in names no key | `internal/server.TestAFiringNamesTheKeyThatFiredIt` | built |
| `fired_by` is the two claims verbatim, without `authorization_details` when the token has none, and absent for an empty or non-string `token_use` | `internal/auth.TestFiredByIsTheKeysClaimsVerbatim` | built |

## Outcome

Built on 2026-10-17, in no release yet. Every criterion has its test.
`TestAFiringNamesTheKeyThatFiredIt` failed against the code before the
change at its first case, a key's firing asked no `fired_by`, and its
held-firing case fails when a fire's key is lent to the held firings it
starts. Nothing diverged from the design.
