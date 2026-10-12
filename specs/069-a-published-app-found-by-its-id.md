---
title: "A published app found by its id: a publish result records the app's id, and every later call finds the app by it, so a rename of the app at its host keeps a conversation publishing to the same app"
status: testing
track: core
depends_on: [043-publishing-a-folder.md, 059-publishing-to-a-chosen-app.md, 067-a-name-and-an-address-for-a-published-site.md]
affects: [session/, harness/tools/, internal/publish/, internal/hosted/, docs/]
effort: small
created: 2026-10-12
updated: 2026-10-12
author: changkun
---

# A published app found by its id

## Overview

The `publish` tool remembers the app a conversation published to by
its slug alone ([[043-publishing-a-folder]]): every result's
`meta.publish.app`, which the thread's state folds and the next call
reads back. An app's owner can now change its slug at the app host, and
the old slug answers `404` at once. The next `publish` of the
conversation then reads its app by the old slug, reads `404`, takes the
app for gone, and creates a second one: the conversation forks the site
without anyone asking, and its later results name an app the person
never made. An app a session was started on is named by its slug as
well ([[059-publishing-to-a-chosen-app]]), and is lost the same way.

An app's id never changes. An app host that finds an app by its id in
the slug's place, on every route that names an app, lets a client keep
the id and find the app after any rename. This spec has the tool keep
the id beside the slug and address the app by it.

| Capability | What a client reads |
|---|---|
| the app a result names, across a rename | `meta.publish.app_id`, the app's id at its host, beside `app`, its slug as the host answered it when the call ran |

## Current state

- `session.PublishMeta` records `app`, the slug, and no id
  (`session/publish.go`). The tool reads `id` from no answer.
- The tool reads the thread's own app by `last.App` in `ensureApp`, and
  creates one on a `404` (`internal/publish/publish.go`). A release reads
  the app by the slug its standing preview records, a call with `access`
  by the slug the call names, and an attached app by the slug of the
  session's resource; each later request of the call (`/deploys`,
  `/deploys/{id}/logs`, `/releases`, `/releases/{tag}`, the `PATCH`)
  names the slug again.
- `tools.State` keys `Apps` and `Standing` by the slug each result
  names (`harness/tools/tools.go`), and the tool's checks (`pick`,
  `may`, `publishable`, `which`) read them by slug.
- An app the platform attaches to a session is `{slug, name, url}`
  (`session.ResourceApp`), with no id: the authorizer's allow names the
  app so, and the platform's allow sends those three. The attached
  repository's URL is the platform's own address of it, which no rule
  of the core reads an id from.

## Design

### The app host

An app host of this release answers the app document with `id` beside
`slug` on every answer that names an app, as it did before, and finds
an app by its id in the slug's place on every route that names one:

| Request | Answer |
|---|---|
| `GET {root}/apps/{id}` | the app's document, as the read by slug answers it: `id`, `slug` as it is now, and every address naming that slug |
| `PATCH`, `/deploys`, `/deploys/{id}/logs`, `/releases`, `/releases/{tag}` under `/apps/{id}` | as under `/apps/{slug}` |
| an id of another account's app, of a deleted app, of no app, or one whose read is denied | `404` `app_not_found` |

A host before that release reads the id as a slug, which no app has,
and answers `404` `app_not_found`. The tool behaves correctly against
both.

### The record

`session.PublishMeta` gains `AppID`, `app_id` on the wire, omitted when
empty. Every result that names an app as the host answered it records
the `id` of that answer beside its slug, name and address: a preview, a
release, and a call with `access`. A failure before the host answered
records the id the tool read the app by, so the thread's state still
ties the failure to the app.

### Finding the app

Wherever the tool reads, publishes to, releases, or changes the access
of an app it knows from a result or an attachment, it finds it so:

1. **By its id**, when the result names one: `GET {root}/apps/{id}`.
2. **By its slug, once**, when the result names no id, as one recorded
   before this release does, or when the read by id answers `404`, as a
   host that does not read ids answers every id, and as every host
   answers for an app that is gone.
3. **Created again**, only for the thread's own app and only when both
   reads answer `404`, as the tool did before on its one read. An
   attached app the host knows by neither is the call's error, as
   before; so is a release whose app is gone.

Any other failure of the read by id, a `503` or a `403`, is the call's
error; the slug is not read, and nothing is created. A session never
creates a second app for an app that still exists under another slug,
whose id it holds, at a host that reads ids.

The slug, the name and the addresses each result records and its text
gives are the host's answer to that read, never the record's. Every
later request of the call names the app by what found it, its id or its
slug, so a rename between the read and a poll of the build does not
lose the build.

### The thread's state across a rename

`tools.State` keeps `Apps` and `Standing` keyed by slug. The model, the
`app` input, the attached resources and the checks speak slugs, and a
record without an id has nothing else to key by. A result that names an
app id becomes that app's last result under every slug a result named
for the same id, and the app's preview that stands is found by the new
slug as by the old ones. So:

- A slug the thread knew the app by before the rename still finds it,
  in `may`, in `app`, and in a release's standing preview, and the call
  then reads the app by its id.
- Lists the tool gives the model, `app_required`'s and
  `app_not_attached`'s, name each app once: an attached app by the slug
  the session was given it by, which names its checkout and is the slug
  the model's context names, and the thread's own app by the slug its
  host last answered.

Keying by id was weighed against this. The slug is the only key a
record from before this release has; an id key would split such an
app's results from the ones recorded after, and every input and check
that speaks a slug would map a slug to an id first. Keeping both, every
slug pointing at the app's last result, keeps each lookup a map read.

### The attached app

The platform attaches an app by its slug, with no id, so the tool has
no id for an attached app until a result of the session read it. From
then on the thread's last result for the attachment's slug names the
id, and the tool finds the attached app by it: a rename after that
keeps the checkout publishing to it, its new slug names it in a call as
the slug the session was given does, and the checkout is still pushed
to the repository the session was given. An app renamed before the
session's first result read it is not found by the slug the session was
given; an attachment that carries the app's id would close that, and is
not in this spec.

### The session's git directory

A thread's own app is published through `$HOME/.topos/publish/<slug>.git`
([[043-publishing-a-folder]]), named by the slug the host answers. After
a rename the next publish starts a new directory under the new slug,
which builds on the session's branch as the git host holds it, as a
lost directory does; the branch's history goes on. The directory stays
named by the slug, since `results/publish/behind-live-v2` names it to
the model by the app's slug.

### The texts

No text changes. Each text that names an app's slug or address after
the host answered takes it from that answer: `access-changed-v1`,
`behind-live-v2`, and the addresses in `ready-v2` and `released-v2`. A
change of who can open the app that the host refused named the slug the
call asked for; it now names the slug the host answered, in the same
template, `access-refused-v1`.

### Decisions

| Decision | Chosen | Alternatives and why not |
|---|---|---|
| What a result records | `app_id` beside `app` | the id in place of the slug: a client shows and links the slug, and a reader of an older log has only the slug |
| The read | by id, then by slug once on `404` | by id only: a host before the release that reads ids answers `404`, and every conversation would create a second app; a list of the account's apps to find the id: a question the session's token is not asked today, and an answer by page |
| A failure of the read by id other than `404` | the call's error | the slug read anyway: a host that is down for one read is down for the next, and a `403` is not a missing app |
| The state's keys | slugs, every slug of an app pointing at its last result | ids: a record from before has none, and every check speaks slugs |
| The requests after the read | by what found the app | by the slug of the answer: a rename while a build is polled loses the build |
| An attached app | its id from the thread's results | from the repository's URL: an address is the platform's, not a contract of the core |

## Compatibility

A client that reads `app` reads it as before, and the slug it names is
the app's slug as the host last answered it. A client that keeps an app
across results keys it by `app_id` where one is recorded.

Against an app host that does not read ids, every read by id answers
`404` and the slug is read once after it: the conversation publishes as
before, at the cost of one more request a call, and a rename there
still loses the app, as before. A host that answers no `id` gives no
`app_id`, and the tool reads by slug alone.

## Not in this spec

The app host's reading of an id. An id in the attachment a platform
sends. Renaming an app from a conversation. An app whose id is gone
while another app of the account took its slug: the read by slug finds
that app, as the one read did before.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A result records the id the host answers beside the slug; after a rename the next publish reads the app by its id, publishes to it under its new slug, creates nothing, and records the new slug with the same id; its later requests name the id | `internal/publish.TestARenamedAppIsFoundByItsID`, `internal/hosted.TestASessionKeepsPublishingToItsRenamedApp` | built |
| A release and a change of who can open the app read and reach the app by its id after a rename; a call that names the slug the app had finds it; the texts name the slug the host answered | `internal/publish.TestARenamedAppIsFoundByItsID` | built |
| A record without an id is read by its slug, and the result then names the id | `internal/publish.TestAnAppIsFoundByItsSlugWhenItsIDIsNot` | built |
| A host that reads no id answers the id `404`, the app is read by its slug once and published to, and the call's later requests name the slug | `internal/publish.TestAnAppIsFoundByItsSlugWhenItsIDIsNot`, `internal/hosted.TestASessionKeepsPublishingToItsRenamedApp` | built |
| An app gone by its id and its slug is created again; a read by id that fails otherwise fails the call, reads no slug and creates nothing | `internal/publish.TestAnAppIsFoundByItsSlugWhenItsIDIsNot` | built |
| An attached app a result read is found by its id after a rename: the checkout is pushed to the repository the session was given, the result names the new slug and the id, and the new slug, the given slug and a release that names no app each find it once | `internal/publish.TestAnAttachedAppIsFoundAfterARename`, `internal/hosted.TestASessionKeepsPublishingItsRenamedAttachedApp` | built |
| The thread's state maps every slug a result named for an app id to that app's last result and its standing preview; results with another id or none are other apps | `harness/tools.TestStateOfFoldsARenamedApp` | built |

## Outcome

Built on 2026-10-12, in no release yet, and not yet run against a live
app host. Every criterion has its test, and each new test failed
against the tool before the change. What shipped beyond the design
above:

- **The attachment carries no id.** The platform's allow sends an
  attached app as `{slug, name, url}`, so an attached app renamed before
  the session's first result read it is not found; the slug path works
  as before for it.
- **A read by slug answers whichever app has the slug.** When the read
  by id answers `404` because the app is gone and another app of the
  account took its slug, the read by slug finds that app, and the
  session publishes to it, as its one read by slug did before.
