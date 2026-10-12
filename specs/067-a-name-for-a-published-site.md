---
title: "A name for a published site: the publish call that creates a session's app gives the app its name, from which the app host makes its address, in place of the session's title"
status: testing
track: core
depends_on: [043-publishing-a-folder.md, 059-publishing-to-a-chosen-app.md]
affects: [internal/publish/, internal/hosted/, prompts/, docs/]
effort: small
created: 2026-10-12
updated: 2026-10-12
author: changkun
---

# A name for a published site

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

This spec lets the agent name the site. `publish` takes `name`, a short
name for the site as a person would call it, and the call that creates
the app sends it as the app's name. The agent built the site and knows
what it is. Nothing else of the tool changes.

| Capability | What a client reads |
|---|---|
| a site's own name | `name` in the input of the `agent.tool_use` of the call that creates the app, and `meta.publish.name` and `url` of its result, the name and the address the app has at its host |

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

### The input

| Input | Type | Meaning |
|---|---|---|
| `name` | string, optional | a short name for a new site, two to four words, in the language of the site, as a person would call it: "Class of 2009", "Tide Tables" |

### The call that reads it

`name` is read by the call that creates the thread's own app, and by no
other.

| The call | `name` |
|---|---|
| publishes or releases, and the thread has published no app of its own | sent as the new app's name |
| publishes or releases, and the host no longer knows the app the thread published to, so the call creates one again | sent as the new app's name |
| publishes to or releases the thread's own app, which exists | ignored |
| publishes to or releases an app the installation attached | ignored |
| names `access` | ignored: such a call creates no app |

No call is refused for its `name`. The tool refuses a call whose inputs
contradict each other: `access` beside `path` or `release` asks for two
things of a call that does one, and is refused `access_alone`. A name
asks nothing of a call that creates no app, so it contradicts nothing
there, and a model that repeats the name on every call still
publishes.

The tool renames no app. An app's name after its create is its
person's to change, at the app host; a later call that gives another
name leaves the app as it is, and its result names the app as the host
has it.

### What is sent

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

| Bound | Value | Constant |
|---|---|---|
| characters of the name a call gives an app | 80 | `MaxName` |

The app host's contract states no bound for a name
([[043-publishing-a-folder]]), so the bound is the tool's own: room for
a few words in any script, and short of a sentence. The session's title
has its own bound ([[054-a-sessions-title-and-filing]]) and is sent as
it was.

The create is otherwise as before: `POST {root}/apps` with `{"name"}`
and no visibility.

### The host's answer

The host makes the app's address from the name by its own rule: which
characters of a name an address keeps, how long an address is, and what
it does when the address it made is reserved or another app has it. The
tool works out no address. The result of the call that creates the app
records the `app`, `name` and `url` the host answered, and its text
gives that address, as every result's does.

A create the host refuses is the call's error result, as it was: the
text carries the host's code and sentence, the result names no app, and
nothing is pushed. The model reads it and may call again with another
name.

### The description

`tools/publish-v5` is `tools/publish-v4` with three changes; every
other sentence is the same.

- The sentence on the first call says the app is created "with the
  `name` the call gives it".
- A paragraph tells the model to name the site the first time it
  publishes it, what a name is, that it becomes the app's name and the
  app's address is made from it, that the app takes the session's title
  without one, that it is used once and renaming is the person's, and
  to give the person the address a result gives, never one worked out
  from the name.
- The inputs list `name`.

`tools/publish-v4` stays in the tree as v0.32.0 released it. An agent
definition names `publish` in `spec.tools` and pins no version of a
tool's description ([[043-publishing-a-folder]]), so every agent that
holds the tool reads `tools/publish-v5` from the release that carries
it.

### Decisions

| Choice | Picked | Weighed against |
|---|---|---|
| Who names the site | the agent, in the call that creates the app | the session's title: it names the conversation; a question to the person before the first publish: one more answer before every site, for a name the person can change; a name the host reads from the site's files: the app is created before anything is pushed |
| A name on a call that creates no app | ignored | refused: a person would be asked to approve a call again for an input that changes nothing; renaming the app: a later call would move an app's name unasked |
| A name with `access` | ignored | `access_alone`: that refusal is for a call that asks for two actions, and a name asks for none |
| A name past the bound | cut | refused by the schema: the model reads an invalid input and the site is not published |
| The address | made by the host from the name | a `slug` input: the grammar of an address, its reserved words and what is taken are the host's to know |
| The result's text | unchanged, it gives the address the host answered | a text that also names the app: every result text of a publish and a release would need a new version to say what `meta.publish.name` already records |

## Compatibility

A model that gives no name, and an agent whose instructions say
nothing of it, publish as before, with the session's title. An app that
exists keeps its name and its address. The app host's contract is
unchanged. A client that puts a waiting `publish` call to the person
in words of its own may show `name`; one that does not shows what it
showed before.

## Not in this spec

Renaming an app, and choosing its address apart from its name. The
address a host makes from a name in a script it keeps none of in an
address, which is the host's rule: such a host may give the app an
address of its own making. A name for an attached app, which has one.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The call that creates the thread's own app sends the name its input gives, on one line with no whitespace around it; a preview and a release alike; and the create names nothing else | `internal/publish.TestTheCallNamesTheAppItCreates` | built |
| A call that gives no name, an empty one or whitespace alone sends the session's title, and a session with no title sends none | `internal/publish.TestTheCallNamesTheAppItCreates` | built |
| A name longer than `MaxName` characters is cut to `MaxName` characters, in a script of several bytes a character as in ASCII, and a space the cut leaves at the end is removed; a name of exactly `MaxName` characters is sent whole | `internal/publish.TestTheCallNamesTheAppItCreates` | built |
| The result of the call that creates the app records the slug, the name and the address the host answered, whatever the call named, and its text gives that address; a create the host refuses is the call's error result with the host's code and sentence, and nothing is pushed | `internal/publish.TestTheCallNamesTheAppItCreates` | built |
| A later publish or release of the thread's own app that gives a name, by default or named by its slug, and a publish of an attached app, send no create and no change of the app, and are not refused | `internal/publish.TestANameChangesNoAppThatExists` | built |
| A call with `access` and `name` changes who can open the app and nothing else, passes the check, and waits for a person; with no app to change it is `app_required`, before the call is decided and in `Run` alike | `internal/publish.TestANameChangesNoAppThatExists` | built |
| An app the host no longer knows is created again with the name the call gives | `internal/publish.TestANameChangesNoAppThatExists` | built |
| The description tells the model to name the site the first time it publishes one, what a name is, that it is used once, and to give the person the address a result gives; the schema takes `name` as a string with no length, and still refuses a field it does not know | `internal/publish.TestTheDescriptionAsksForAName`, `prompts.TestEveryTextRendersItsCurrentBytes`, `prompts.TestReleasedPromptsAreImmutable` | built |
| The page for operators and clients states the bound the constant holds | `internal/publish.TestThePublishingPageStatesTheNameBound` | built |
| A session whose title is a sentence about its conversation creates its app with the name its first publish gives, at an address the host made from that name; the recorded call carries the name; and a later publish with another name creates nothing and changes nothing of the app | `internal/hosted.TestASessionNamesTheSiteItPublishes` over the stub Cella, a stub app host and git's own http backend | built |
| A model asked for a site names it in its first publish, with a name of a few words that says what the site is | a task of the suite ([[025-task-suite]]) against a stub app host | not built |

## Outcome

Built on 2026-10-12, in no release yet, and not yet run against a live
app host or a model. Every criterion but the task of the suite has its
test; the task is not built, as for [[043-publishing-a-folder]]. What
shipped, beyond the design above:

- **A name is put on one line**, not only trimmed. A model may write a
  line break into a string, and an app's name is shown on one line.
- **The description names the address a result gives** as the one to
  pass on. A model that named a site "Tide Tables" would otherwise
  guess the address from the name, and the host may have made another.
- **A result's text does not name the app.** `meta.publish.name` holds
  the name the host answered, which a client reads; the model reads the
  address. A model that gave a name the tool cut, or gave one for an
  app that exists, reads no correction of the name in the text.
