---
title: "A name and an address for a published site: the publish call that creates a session's app gives the app its name and asks for its address, in place of a name and an address made from the session's title"
status: testing
track: core
depends_on: [043-publishing-a-folder.md, 059-publishing-to-a-chosen-app.md]
affects: [internal/publish/, internal/hosted/, prompts/, docs/]
effort: small
created: 2026-10-12
updated: 2026-10-12
author: changkun
---

# A name and an address for a published site

## Overview

The first `publish` of a session creates the session's app
([[043-publishing-a-folder]]). The create names the app after the
session's title, and the app host makes the app's address from that
name. A session's title names the conversation: a client sets it at the
create, commonly to what the person first typed or to a summary of it,
and a person may change it later
([[054-a-sessions-title-and-filing]]). A person who asked "Compare the
weather between Munich and Stockholm" and got a site got an app named
by that sentence, at an address made of its first words. Neither is
what the person would call the site, and the address is the part they
send to others.

A better name does not give every site a better address. A host makes
an address from the letters and digits of a name that an address can
hold, and a name in a script it holds none of leaves nothing to make it
from: a site named `潮汐表` gets an address the host generated.

This spec lets the agent say both. `publish` takes `name`, a short name
for the site as a person would call it, in the site's own language, and
`address`, a short address in Latin letters. The call that creates the
app sends them. The agent built the site and knows what it is. Nothing
else of the tool changes.

| Capability | What a client reads |
|---|---|
| a site's own name and address | `name` and `address` in the input of the `agent.tool_use` of the call that creates the app, and `meta.publish.name`, `app` and `url` of its result: the name, the slug and the address the app has at its host |

## Current state

The tool takes `path`, `app`, `release` and `access`
([[043-publishing-a-folder]], [[059-publishing-to-a-chosen-app]]). A
call that finds no app of the thread's own sends `POST {root}/apps`
with the session's title as `name`, and no `name` when the session has
no title. The host answers the app with the slug it derived, and every
result names the app as the host answered it. The tool's description,
`tools/publish-v4`, tells the model that the first call creates the
session's app "named after the session".

## Design

### The inputs

| Input | Type | Meaning |
|---|---|---|
| `name` | string, optional | a short name for a new site, two to four words, in the language of the site, as a person would call it: "Class of 2009", "Tide Tables", "潮汐表" |
| `address` | string, optional | a short address for a new site, two to four words in lowercase Latin letters and digits joined by single hyphens, made from what the site is: `class-of-2009`, `tide-tables`. In Latin letters whatever the site's language |

### The call that reads them

Both are read by the call that creates the thread's own app, and by no
other.

| The call | `name` and `address` |
|---|---|
| publishes or releases, and the thread has published no app of its own | sent with the create |
| publishes or releases, and the host no longer knows the app the thread published to, so the call creates one again | sent with the create |
| publishes to or releases the thread's own app, which exists | ignored |
| publishes to or releases an app the installation attached | ignored |
| names `access` | ignored: such a call creates no app |

No call is refused for either. The tool refuses a call whose inputs
contradict each other: `access` beside `path` or `release` asks for two
things of a call that does one, and is refused `access_alone`. A name
and an address ask nothing of a call that creates no app, so they
contradict nothing there, and a model that repeats them on every call
still publishes.

The tool renames no app and moves none to another address. Both are
the person's to change after the create, at the app host; a later call
that gives others leaves the app as it is, and its result names the app
as the host has it.

### The name

1. **One line.** Every run of whitespace in the name, a line break
   among them, becomes one space, and none is left around it. A name is
   shown on one line wherever the app is listed.
2. **The bound.** A name longer than `MaxName` characters is cut to
   that many. Characters are counted, not bytes, so a name in a script
   of several bytes a character is never cut inside one, and a space
   the cut would leave at the end is removed. The schema states no
   length: a name past the bound is cut and not refused, so a model
   that wrote a sentence where a name goes still publishes.
3. **No name.** A call that gives none, or one that is whitespace
   alone, sends the session's title, as every call did before the input
   was there: an agent whose model leaves it out keeps working. A
   session with no title sends no name, and the host names the app.

The app host's contract states no bound for a name
([[043-publishing-a-folder]]), so `MaxName` is the tool's own: room for
a few words in any script, and short of a sentence. The session's title
has its own bound ([[054-a-sessions-title-and-filing]]) and is sent as
it was.

### The address

The tool holds an address to one rule, the one an app host holds a slug
to: at most `MaxAddress` characters, in words of `a` to `z` and `0` to
`9` joined by single hyphens, with no hyphen at either end. The rule is
stated once in the tool, as `MaxAddress` and one pattern.

An address that fits is sent as the create's `slug`. One that does not
is dropped: a capital letter, a space, a character of another script,
two hyphens together, or one character too many, and the create goes
with no slug, as it does for a call that asks for no address. The tool
neither refuses the call nor repairs the address. The schema states no
pattern and no length, since a schema's refusal would cost the person a
publish for an input the host can do without.

| Bound | Value | Constant |
|---|---|---|
| characters of the name a call gives an app | 80 | `MaxName` |
| characters of the address a call asks for | 40 | `MaxAddress` |

### The create, and the create without the address

The create is `POST {root}/apps` with `{"name", "slug"}`, each present
when the call gives it, and no visibility, as before. A host that takes
a slug gives the app that slug, and refuses one it cannot give with a
code that says why:

| Code | The slug |
|---|---|
| `invalid_slug` | is no address at this host, such as one shorter than the host gives this owner |
| `slug_taken` | is another app's |
| `slug_reserved` | is one the host keeps |
| `slug_held` | is held for the owner of an app that had it |
| `repository_name_taken` | names a repository the app's owner already has at the git host |

When a create that named a slug is refused with one of these, the tool
sends the create once more with the same name and no slug, so the host
makes the address itself, and the call goes on with the app that
answers. An address that is taken never fails a publish, and never
costs the person a second approval: both creates are inside the one
call they approved.

The second create is the last. When the host refuses it too, as a host
may that makes from the name the very address it just refused, that
refusal is the call's error result. A refusal with any other code, a
host that cannot be reached, and a slug's code on a create that named
no slug are the call's error result with one create, as they were: the
text carries the host's code and sentence, the result names no app, and
nothing is pushed.

### The host's answer

The result of the call that creates the app records the `app`, `name`
and `url` the host answered, and its text gives that address, as every
result's does. The tool works out no address: not from the name, and
not from the address the call asked for, which the app may not have.

### The description

`tools/publish-v5` is `tools/publish-v4` with three changes; every
other sentence is the same.

- The sentence on the first call says the app is created "from the
  `name` and the `address` the call gives it".
- A paragraph tells the model to name the site and give it an address
  the first time it publishes it: what each is, that the address is in
  plain Latin letters even when the site is in another script, that
  both come from what the site is and not from the conversation, what
  the app gets without them, that they are used once and changing them
  later is the person's, that an address that is not free does not stop
  the publish, and that the result names the address that exists, which
  is the one to give the person.
- The inputs list `name` and `address`, the address with its bound,
  which a test holds to `MaxAddress`.

`tools/publish-v4` stays in the tree as v0.32.0 released it. An agent
definition names `publish` in `spec.tools` and pins no version of a
tool's description ([[043-publishing-a-folder]]), so every agent that
holds the tool reads `tools/publish-v5` from the release that carries
it.

### Decisions

| Choice | Picked | Weighed against |
|---|---|---|
| Who names the site | the agent, in the call that creates the app | the session's title: it names the conversation; a question to the person before the first publish: one more answer before every site, for a name the person can change; a name the host reads from the site's files: the app is created before anything is pushed |
| The address | an input of its own, in Latin letters | the address the host makes from the name: a name in another script leaves it nothing to make one from; the name in Latin letters: the person reads the name, and reads it in their own language |
| An address the tool's rule does not fit | dropped | refused: a person would approve a call again for a slug the host can make itself; repaired by the tool: the result is an address nobody asked for, and the host's own from the name is as good |
| An address the host refuses | the create is sent once more without it | the call's error: the model would publish again, and the person approve again, for an address they never chose |
| How often the create is sent again | once | until one is taken: a host that refuses every create is asked without end |
| Which refusals send it again | the five codes a host refuses a slug by | every refusal: a create the host refuses for the owner's plan or its permissions would be sent twice, and its second refusal is no clearer than its first |
| A name or an address on a call that creates no app | ignored | refused: a person would be asked to approve a call again for an input that changes nothing; changing the app: a later call would rename an app or move it unasked |
| Either with `access` | ignored | `access_alone`: that refusal is for a call that asks for two actions, and these ask for none |
| A name past the bound | cut | refused by the schema: the model reads an invalid input and the site is not published |
| The result's text | unchanged, it gives the address the host answered | a text that also names the app: every result text of a publish and a release would need a new version to say what `meta.publish.name` already records |

## Compatibility

A model that gives neither, and an agent whose instructions say nothing
of them, publish as before, with the session's title. An app that
exists keeps its name and its address. A client that puts a waiting
`publish` call to the person in words of its own may show `name` and
`address`; one that does not shows what it showed before.

An app host takes `slug` in the body of the create. One that makes
every address itself may ignore the member, or refuse it with one of
the codes above, and is then asked again without it. A host that
refuses a body with a member it does not know, with any other code,
fails the first publish of a call that asks for an address, so such a
host learns `slug` before this release reaches it. An authorizer that
decides the create by what it names is asked about a create with a
slug as the host asks it; where it refuses one, the call's error result
carries the refusal.

## Not in this spec

Renaming an app, and moving one to another address; a rename the
person makes at the host keeps the conversation publishing to the app
since [[071-a-published-app-found-by-its-id]]. An address for an
attached app, which has one. Telling the model in a result that the
address it asked for was not the one the app got: the result names the
address the app has, and the description tells the model to pass that
one on.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The call that creates the thread's own app sends the name its input gives, on one line with no whitespace around it; a preview and a release alike; and the create names nothing the call did not give | `internal/publish.TestTheCallNamesTheAppItCreates` | built |
| A call that gives no name, an empty one or whitespace alone sends the session's title, and a session with no title sends none | `internal/publish.TestTheCallNamesTheAppItCreates` | built |
| A name longer than `MaxName` characters is cut to `MaxName` characters, in a script of several bytes a character as in ASCII, and a space the cut leaves at the end is removed; a name of exactly `MaxName` characters is sent whole | `internal/publish.TestTheCallNamesTheAppItCreates` | built |
| An address of at most `MaxAddress` characters in words of lowercase Latin letters and digits joined by single hyphens is sent as the create's `slug`, beside a name in another script, with no name, and by a release; one with a capital, a space inside or around it, a line break, two hyphens together, a hyphen at either end, a character of another script or an underscore, one that is a URL, an empty one and one of `MaxAddress` and one characters are dropped, and the create names no slug | `internal/publish.TestTheCallNamesTheAppItCreates` | built |
| A create that named a slug and is refused `invalid_slug`, `slug_taken`, `slug_reserved`, `slug_held` or `repository_name_taken` is sent once more with the same name and no slug; the call publishes, and its result and its text name the address the app got | `internal/publish.TestAnAddressTheHostRefusesDoesNotFailThePublish` | built |
| The create is sent again once: a host that refuses the second create too, a refusal with another code, a host that is down, and a slug's code on a create that named no slug or whose address the tool dropped, are the call's error result, with two creates for the first and one for the others, and nothing is pushed | `internal/publish.TestAnAddressTheHostRefusesDoesNotFailThePublish`, `internal/publish.TestTheCallNamesTheAppItCreates` | built |
| The result of the call that creates the app records the slug, the name and the address the host answered, whatever the call named, and its text gives that address | `internal/publish.TestTheCallNamesTheAppItCreates` | built |
| A later publish or release of the thread's own app that gives a name and an address, by default or named by its slug, and a publish of an attached app, send no create and no change of the app, and are not refused | `internal/publish.TestANameAndAnAddressChangeNoAppThatExists` | built |
| A call with `access`, `name` and `address` changes who can open the app and nothing else, passes the check, and waits for a person; with no app to change it is `app_required`, before the call is decided and in `Run` alike | `internal/publish.TestANameAndAnAddressChangeNoAppThatExists` | built |
| An app the host no longer knows is created again with the name the call gives, at the address it asks for | `internal/publish.TestANameAndAnAddressChangeNoAppThatExists` | built |
| The description tells the model to name the site and give it an address the first time it publishes one, what each is, that the address is in Latin letters whatever the site's script, that both are used once, that an address that is not free does not stop the publish, and to give the person the address the result names; it states the bound `MaxAddress` holds; the schema takes both as strings with no length and no pattern, and still refuses a field it does not know | `internal/publish.TestTheDescriptionAsksForANameAndAnAddress`, `prompts.TestEveryTextRendersItsCurrentBytes`, `prompts.TestReleasedPromptsAreImmutable` | built |
| The page for operators and clients states the bounds the constants hold | `internal/publish.TestThePublishingPageStatesTheBounds` | built |
| A session whose title is a sentence about its conversation creates its app with the name, in another script, and at the address its first publish gives; the recorded call carries both; a later publish with others creates nothing and changes nothing of the app; and where another app has the address, the one call creates the app without it and publishes, the person asked once | `internal/hosted.TestASessionNamesTheSiteItPublishes` over the stub Cella, a stub app host and git's own http backend | built |
| A model asked for a site names it and gives it an address in its first publish: a name of a few words in the site's language, and an address in Latin letters, each saying what the site is | a task of the suite ([[025-task-suite]]) against a stub app host | not built |

## Outcome

Built on 2026-10-12, in no release yet, and not yet run against a live
app host or a model. Every criterion but the task of the suite has its
test; the task is not built, as for [[043-publishing-a-folder]]. What
shipped, beyond the design above:

- **The address came second.** The first build gave the agent a name
  alone and left the address to the host, which makes none from a name
  in a script an address holds none of. `address` and the second create
  were added the same day, before any release, and `tools/publish-v5`
  was edited in place with its pins, since no release had sent it.
- **A name is put on one line**, not only trimmed. A model may write a
  line break into a string, and an app's name is shown on one line.
- **An address is not trimmed or lowercased.** The tool's rule is the
  host's rule for a slug and nothing kinder; what does not fit it is
  left to the host.
- **A result's text does not name the app, or say that the address
  asked for was not given.** `meta.publish` holds the name and the
  address the host answered, which a client reads, and the text gives
  the address. A model that gave a name the tool cut, or gave either
  for an app that exists, reads no correction in the text.
