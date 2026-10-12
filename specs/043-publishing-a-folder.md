---
title: "Publishing a folder: the publish tool, the session's own app at the installation's app host, its preview, its release and who can open it, and the route a session runs by"
status: in-progress
track: core
depends_on: [004-session-log.md, 008-tools.md, 010-context.md, 012-permissions-and-approvals.md, 018-credentials-and-secrets.md, 019-git.md, 038-routed-models.md, 039-questions.md]
affects: [internal/publish/, internal/hosted/, internal/config/, cmd/toposd/, harness/, harness/tools/, manifest/, session/, prompts/, internal/toposcli/, docs/]
effort: medium
created: 2026-10-05
updated: 2026-10-12
author: changkun
---

# Publishing a folder

## Overview

An agent asked to put a page online has no way to do it. It can write
the files and run a web server in its sandbox, but nothing outside the
sandbox reaches that server, and a path in the sandbox is no address a
person can open. An installation may run an app host beside the core:
a service that keeps one git repository per app, builds a preview of
every push at an address of its own, and puts a commit at the app's
own address when a version tag is pushed. Reaching it from the
sandbox takes several steps, each of which a weak model can get wrong:
create the app, add a remote, commit, push, poll for the build, read
its log, and tag.

This spec adds one tool, `publish`. Given a folder of the machine, it
publishes the folder as the session's own app and answers with the
preview's address once the preview is built, or with why the build
failed. Given `release`, it releases the last preview the session
published to the app's address. The tool runs in the runner: the app
host is reached with the session's own short token, which never enters
the sandbox, and the folder is pushed by the sandbox's own git, which
already reaches the installation's git host with the session's git
credential ([[019-git]]). Every action it takes is decided by the
installation's authorizer as the session's agent, as any other is.

So that an agent can say when a heavier model would serve the person
better, each request also carries the routed name the session runs by,
when it runs by one.

## Current state

There is no tool that reaches an app host, and no configuration names
one. A session's sandbox holds the git host's Secret and its git sends
the placeholder to that host ([[018-credentials-and-secrets]],
[[019-git]]); Cella joins a mounted Secret's scope to the sandbox's
egress allowlist, so the git host is reachable whenever the Secret is
mounted. A session's token is minted for any audience the runner asks
of the identity provider, which mints only the audiences the
installation is registered for. The model is never told the routed
name its session runs by ([[038-routed-models]]): the system prompt
names no model, and the context block is rendered once per machine.

[[012-permissions-and-approvals]] rules out a tool per action: an
action outside the machine is a request from the sandbox, bounded by
the scope its token is minted with and stepped up at Cella's egress.
`publish` is not an action of a core; it is a workflow over three of
them, and the reason it is a tool is the model, not the boundary: its
call asks before it runs in `confirm` and `progressive` alike, as any
call with an external effect does, and every request it makes is
decided at the core that receives it.

## Design

### The app host

The app host is a service with this contract, under one root URL:

| Request | Answer |
|---|---|
| `POST {root}/apps` with `{"name"}`, and with `{"name", "slug"}` since [[067-a-name-and-an-address-for-a-published-site]] | `201` with the app: `slug`, `name`, `url` (its own address), `visibility` (who may open it, when the host says), `repository.push_url` |
| `GET {root}/apps/{slug}` | the app, or `404` |
| `PATCH {root}/apps/{slug}` with `{"visibility": "unlisted"}` or `{"visibility": "private"}` | the app, with its `visibility` after the change (amended 2026-10-11, below) |
| `GET {root}/apps/{slug}/deploys` | `{"deploys": [...]}`, newest first, each with `id`, `status`, `preview`, `commit_sha`, `preview_url` (the deploy's own address) and `error` (`{code, message}` or null) |
| `GET {root}/apps/{slug}/deploys/{id}/logs` | the build log, one JSON object per line with its `line` |
| `GET {root}/apps/{slug}/releases` | `{"releases": [...]}`, each with `tag`, `commit_sha`, `status`, `reason` |
| `GET {root}/apps/{slug}/releases/{tag}` | `{"release": ..., "deploy": ...}`, or `404` until the host has seen the tag |

A push of a branch to the app's repository builds a preview deploy,
whose status moves through `waiting`, `queued`, `building` and `built`
to `ready`, `failed` or `canceled`. A pushed tag that starts with `v`
makes a release of its commit, whose status is `pending`, then
`released`, `refused` (with a `reason`) or `failed`. An error answers
`{"error": {"code", "message"}}`, and may name why in
`details.reason`, a token. Latere's Apps is one such host.

| Variable | Default | Meaning |
|---|---|---|
| `TOPOS_APPS_URL` | unset | the app host's root. Unset, no session is offered `publish`. Needs `TOPOS_ORIGO_URL`, the git host the app's repository must be on |
| `TOPOS_APPS_AUDIENCE` | `apps` | the audience of the session token the runner presents to the app host |

### The tool

| Name | Parallel | Effect | Input | Limits |
|---|---|---|---|---|
| `publish` | no | external | `path` (the folder; a relative path resolves against the working directory; default the working directory), `release` (default false), `access` (`link` or `private`, amended 2026-10-11, below), `name` and `address` (the new app's name and the address asked for it, [[067-a-name-and-an-address-for-a-published-site]]) | the wait below |

`publish` is a tool of the session's own thread, as `question` is
([[039-questions]]): the runner adds it to the registry when the
agent's `spec.tools` names it, the installation configures an app host
and the session runs on a Cella machine; a thread a spawn started
holds none, so a session has one app. Amended by
[[059-publishing-to-a-chosen-app]]: a thread publishes to its own app
and to each app its session's repositories attach, chosen by `app`. The manifest takes `publish` by
its name alone, like `question`; `topos run` refuses an agent that
names it, since a local run has no app host and no sandbox.

**Publishing a folder.**

1. **The app.** The session's app is the slug the thread's last
   `publish` result records. The runner reads it (`GET`); a slug the
   host no longer knows, and a session with none, creates one: `POST
   {root}/apps` with the session's title as `name`, from which the host
   derives the slug. The create names no visibility: who may open a new
   app is the host's default, and the person changes it at the host, or
   approves the call with `access` that changes it (amended 2026-10-11;
   the create asked for `public` before, which put every session's app
   online for anyone without its person choosing so). The tool's text is
   `tools/publish-v4`, which promises no public address, and the ready
   and released results are `results/publish/ready-v2` and
   `released-v2`, which say when the app is private and tell the model
   to offer to open it. The request carries the
   session's token for `TOPOS_APPS_AUDIENCE` and the workload
   `session`: the sandbox never holds it, and the authorizer decides
   the create as the session's agent within its initiator's reach.
   Amended by [[067-a-name-and-an-address-for-a-published-site]]: the
   call that creates the app gives the app its name and asks for its
   address, the session's title is what a call that gives no name
   sends, and the tool's text is `tools/publish-v5`.
2. **The push.** A push URL on another host than `TOPOS_ORIGO_URL`'s
   is refused before anything runs, since the sandbox's git sends its
   credential to that host alone. In the sandbox, a git directory of
   the session's own, `$HOME/.topos/publish/<slug>.git`, outside the
   workspace, takes the folder as its work tree: the first publish
   starts it from the session's branch on the app's repository when
   that branch exists, every change is committed with the session's
   author and trailers ([[019-git]]), and `HEAD` is pushed to the
   session's branch, `agents/<agent>/<session>`. `node_modules/` and
   `lost+found/` are left out; the folder's own `.gitignore` applies. A
   folder with nothing changed since the last publish is pushed again
   and committed only when the last deploy of its commit failed or was
   canceled, so a publish that ran out of time waits on the same build,
   and one after a failure builds again.
3. **The wait.** The runner reads the app's deploys every
   `PollEvery` until the preview deploy of the pushed commit is
   `ready`, `failed` or `canceled`, for at most `Wait`. A failed
   deploy's log is read and its last `LogTail` lines go to the model.

**Releasing.** `release: true` takes the commit of the thread's newest
preview that stands, and the app it was published to: the last whose
status is `ready` or `building`, passing over a failed or canceled one
and a call the person denied, which is the preview a client shows as
the one to release. The host releases a preview still building once it
is ready. A release of that
commit the host already lists answers with its status. Otherwise the
tag is `v<n>`, one past the highest `v` followed by digits among the
app's releases, pushed on the commit from the session's git directory,
which fetches the session's branch first when it no longer holds the
commit. The runner then reads the release every `PollEvery`, a `404`
meaning the host has not seen the tag yet, until it is `released`,
`refused` or `failed`, for at most `Wait`. Whether the session's agent
may release is the host's question to the installation's authorizer,
asked for the tag's pusher.

Amended by [[059-publishing-to-a-chosen-app]]: a release no longer
needs a preview before it. `release: true` publishes the folder as a
call without it would, waits for the build, and releases the commit it
built, so one approval puts a change live; a build that fails releases
nothing. A release right after a ready preview, with the folder
unchanged, pushes nothing new and releases that preview. The preview
the paragraph above takes is what a release of a thread whose results
record no folder still releases.

**Changing who can open the app** (amended 2026-10-11). A person who
builds a site in a conversation opens it to others without leaving the
conversation: the agent asks, and the person approves.

`access` takes two values. `link`: anyone who has the app's address
opens it, without signing in. `private`: only the person and the people
they share the app with. A call with `access` changes who can open one
app and does nothing else: it opens no machine, creates no app, and
pushes, builds and releases nothing.

1. **Alone.** `access` goes with `app` and nothing else. A call that
   also names `path` or `release` is refused `access_alone`.
2. **The app.** The one `app` names, by the rule of a release that
   names no folder ([[059-publishing-to-a-chosen-app]]): with no `app`,
   the app whose preview stands, else the one app the thread published
   to; `app_required` when that could be several, and when the thread
   has published none, since a call with `access` creates no app;
   `app_not_attached` for an app the session may not publish to.
   Each of these is refused before the call is decided.
3. **The approval.** Every call with `access` waits for a person. The
   tool holds it ([[012-permissions-and-approvals]]): no `always_allow`
   pattern, no pattern a person's answer added to the session, no score
   of the `progressive` mode and no decision service runs it without
   one. `plan` blocks it. An unattended session that reaches it waits,
   as for every ask, and changes nothing. A release is not held so: an
   agent whose `always_allow` names `publish` releases without asking,
   as before.
4. **The request.** The runner reads the app (`GET`). When the app is
   as asked already, it sends nothing: `private` for `private`, and for
   `link` an app anyone with its address opens, `unlisted` or `public`,
   so a public app keeps what being public gives it at its host.
   Otherwise it sends `PATCH {root}/apps/{slug}` with `unlisted` for
   `link` and `private` for `private`, with the session's token as
   every other request. It never sends `public`. Whether the session
   may change the app is the host's question to the installation's
   authorizer. The token names the session and not who approved the
   call, so who may approve it is the authorizer's too, where it
   answers `session.send` for the confirmation.
5. **The result.** Outcome `ok`, with `meta.publish` naming the app,
   its address, the `visibility` the host answers, the `folder` of the
   thread's last result for the app, and no `status`, `commit`,
   `deploy` or `release`. The thread's state folds it as the app's last
   result and keeps the app's standing preview, so a later release that
   names no folder publishes the same folder. The text is
   `results/publish/access-changed-v1`: for `link`, that anyone who has
   the address can open the app, never that it is public.
6. **A refusal.** The host's refusal, and any failure to change the
   app, an answer that names another visibility than the one asked
   among them, is the call's `error` result,
   `results/publish/access-refused-v1`, with the app and the visibility
   it still has in `meta.publish`.
   When the host refused in its envelope, `meta.publish.error` is
   `{code, reason, message}`: the host's code, the token its
   `details.reason` names when it names one, and its sentence. For the
   reason `role_insufficient`, the app is an organization's and the
   person's role there does not reach its settings; the text says that
   only the organization's owners and admins change who can open it.
   Every such text tells the model to pass it on and not to try another
   way.

The description tells the model the behavior: after publishing a
private app, say that it is online and private and offer to open it to
anyone with the link; call `access: "link"` when the person asks to
share the site, show it to others, send it to someone or make it
public, and once the publish call has answered when they said so
before; call `private` to close it again; never call it for a site the
person did not ask to share; after `link`, say that anyone with the
link can open it, and never that the app is public.

| Limit | Value |
|---|---|
| `Wait`, the longest a call waits for a preview or a release | 10 minutes |
| `PollEvery`, how often the host is read while waiting | 3 seconds |
| `LogTail`, the lines of a failed build's log the model reads | 40 |
| the commit and push in the sandbox | 5 minutes |

### The result

The text is the model's: what was published, the address, and the next
step. A ready preview's text tells the model to call `publish` with
`release: true` next, which waits for the person (a release needs no
preview before it since [[059-publishing-to-a-chosen-app]]; the
preview's text still names the release as the next step): in `confirm` the
release is an ask, and a person's message in its place denies it with
the message as the note ([[012-permissions-and-approvals]]), so asking
for a change is the other answer.

The result's `meta.publish` is the tool's record, which the thread's
state folds and a client reads:

| Field | Value |
|---|---|
| `app`, `name` | the app's slug and its name |
| `url` | the app's own address |
| `visibility` | who may open the app, as the host named it when the call ran (`private`, `unlisted` or `public` at a host that speaks those words); absent where the host names none. On the result of a call with `access`, the host's answer to the change, or what the app still has when the change was refused. A client shows it, and the result's text tells the model when the app is private |
| `preview` | the preview deploy's own address, once the host names it |
| `commit` | the commit published or released |
| `deploy` | the deploy's id |
| `status` | `ready`, `building` (the wait ran out), `failed`, `canceled`; for a release `released`, `pending` (the wait ran out), `refused`, `failed`; absent on the result of a call with `access` |
| `release` | the tag, on a release |
| `error` | `{code, message}` for `failed`, `canceled` and `refused`; `{code, reason, message}` for a change of who can open the app that the host refused |

The outcome is `ok` for `ready`, `building`, `released` and `pending`,
and `error` for the others and for every failure before a deploy
exists: a path that is no folder, a release with no preview, an
app host or git host that refused or could not be reached, and an
installation that mints no token for the app host. Those carry no
`meta.publish`, or one with the app alone once it exists. Since
[[059-publishing-to-a-chosen-app]], a release with no preview publishes
first, so only a release of an own app whose results record no folder
answers that there is no preview; an installation that mints no token
is answered before the call is decided, with no approval asked; and a
`meta.publish` names the `folder` published.

### The route a session runs by

A session whose model was asked by a routed name, `model.via`
([[038-routed-models]]), gets a system part after the agent's
instructions on every request: `<context>Model route: <via></context>`.
It changes only when the session's model changes, which already
changes the request's prefix, so it costs no cache. A session with no
routed name has none.

## Not in this spec

The app host itself, and the authorizer's rules that let a session
create an app and push to its repository; an app the person deletes,
which the next `publish` replaces with a new one; a folder that needs a
server process, which the host refuses with its own code and the model
reads; previews served beside the session in a client.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A session whose agent names `publish` publishes a folder of its sandbox: the app is created with the session's title and no visibility, the result records the one the host answers, with the session's token for the app host's audience and the workload `session`, the sandbox's git pushes the folder to the session's branch with the session's author and trailers, the result names the preview's address with `meta.publish` status `ready`, and no token reaches the sandbox, an event or a tool result | `internal/hosted.TestASessionPublishesAFolderAndReleasesIt` over the stub Cella, a stub app host and git's own http backend | built |
| A second publish reuses the app, pushes a new commit on the same branch, and a publish with nothing changed pushes no new commit | `internal/hosted.TestASessionPublishesAFolderAndReleasesIt` | built |
| `release: true` pushes `v1` on the newest standing preview's commit, the next release `v2`, and the result is `released` with the app's address; a preview still building is released, and a failed one is passed over | `internal/hosted.TestASessionPublishesAFolderAndReleasesIt`, `harness/tools.TestStateOfFoldsPublish`, `internal/publish.TestOutcomes` | built |
| A `publish` call on a Cella machine in `confirm` waits for a person, a release included, since its effect is external and not inside the sandbox; the tool's effect is `external` | `harness.TestAPublishCallAsksInConfirm`, `internal/publish.TestTheDescriptionStatesTheWait` | built |
| A failed build answers `failed` with its code, its message and the tail of its log; a refused release answers `refused` with its reason; a wait that runs out answers `building` or `pending` | `internal/publish.TestOutcomes` | built |
| A path that is no folder, a release with no preview, a push URL on another host, an app host that refuses, and an installation that mints no token for it each answer an error the model reads | `internal/publish.TestRefusals` | built |
| The runner offers `publish` only to the session's own thread, only when the agent names it, the app host is configured and the machine is Cella; the manifest takes it by name alone; `topos run` refuses it | `internal/hosted.TestPublishIsOfferedWhenConfigured`, `manifest.TestPublishIsAKnownToolName`, `harness.TestASpawnedThreadHoldsNoPublish`, `internal/toposcli.TestAgentManifestRefusals` | built |
| `TOPOS_APPS_URL` without `TOPOS_ORIGO_URL`, or one that is not an http URL, stops the start | `internal/config.TestTheAppHost` | built |
| A routed session's every request carries its route as a system part, and an unrouted one's none | `harness.TestTheRequestNamesTheRoute` | built |
| A model given a request for a page publishes it and answers with its address, instead of starting a server in its sandbox | a task of the suite ([[025-task-suite]]) against a stub app host | not built |
| A call with `access` asks the host for `unlisted` on `link` and `private` on `private` with the session's token, never `public`, and nothing when the app is as asked already, an unlisted or a public app for `link`; it opens no machine, creates no app and pushes nothing; its result is `ok` and names the app with the visibility the host answers, the thread's folder, and no status | `internal/publish.TestAccessChangesWhoCanOpenTheApp`, `internal/hosted.TestASessionChangesWhoCanOpenItsApp` | built |
| A call with `access` waits for a person in a session whose agent always allows `publish`, after a remembered allow, under a score below every threshold and under a decider that allows every call; `plan` blocks it; until the person allows it the host is asked for nothing; a denial changes nothing | `harness.TestACallItsToolHoldsWaitsForAPerson`, `harness.TestTheRulesHoldACallForAPerson`, `internal/hosted.TestASessionChangesWhoCanOpenItsApp`, `internal/publish.TestAccessIsRefusedBeforeItIsAsked` | built |
| A call with `access` and `path` or `release` is `access_alone`, one with no app to change or several `app_required`, and one naming another's app `app_not_attached`, each before the call is decided, with no request and no app in its meta | `internal/publish.TestAccessIsRefusedBeforeItIsAsked`, `internal/hosted.TestASessionChangesWhoCanOpenItsApp` | built |
| The host's refusal is an `error` result that names the app with the visibility it still has and the host's code, reason and sentence; for `role_insufficient` the text says that only the organization's owners and admins change who can open it; a reason that is no token is left out | `internal/publish.TestARefusedAccessSaysWhy`, `internal/hosted.TestASessionChangesWhoCanOpenItsApp` | built |
| The result of a call with `access` becomes the app's last result in the thread's state and leaves its standing preview, and a release after it that names no folder publishes the folder the thread published | `harness/tools.TestStateOfFoldsAnAccessChange`, `internal/hosted.TestASessionChangesWhoCanOpenItsApp` | built |
| The description says what `access` does, that the person approves it, when to call it and when not to, and never to call `link` public; the schema offers `link` and `private` alone; every text renders its bytes and none holds an em dash | `internal/publish.TestTheDescriptionExplainsAccess`, `prompts.TestEveryTextRendersItsCurrentBytes`, `prompts.TestNoTextHasAnEmDash` | built |
| A model that published a private site offers to open it, opens it when the person asked to share it, and never unprompted | a task of the suite ([[025-task-suite]]) against a stub app host | not built |

## Outcome

Built on 2026-10-11, released in v0.32.0: a call with `access` changes
who can open an app once a person approved it. The spec stays
in-progress for the tasks of the suite, which are not built.

- **No rule made a release ask before this.** A release waits for a
  person because the tool's effect is external and the mode asks; an
  agent whose `always_allow` names `publish`, and a person's answer
  with `remember`, run a release without asking
  (`internal/hosted.TestASessionPublishesAFolderAndReleasesIt` releases
  so). `access` needed a rule of its own, which
  [[012-permissions-and-approvals]] now has: the tool holds the call.
  A release is left as it was.
- **The app is read before it is changed**, so `link` sends nothing to
  an app that is public: asking such an app for `unlisted` would take
  from it what being public gives it at its host.
- **A result carries the thread's folder.** The fold takes the result
  of a call with `access` as the app's last, and a release that names
  no folder reads the folder there; without it, a release after a
  change of access would have taken the standing preview as it was.
- **`tools/publish-v4`, `results/publish/ready-v2` and `released-v2`
  were edited in place** and their pins moved with them. They were
  added on 2026-10-11 and are in no release, so no log records them; a
  fifth version would have left in the tree a fourth that no release
  sent, saying the tool does not change who can open an app. The four
  texts of `access` are new files.
- **`meta.publish.error` gains `reason`**, the token an app host's
  refusal names in `details.reason`, beside its code and sentence.
