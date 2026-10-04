---
title: "Publishing a folder: the publish tool, the session's own app at the installation's app host, its preview and its release, and the route a session runs by"
status: drafted
track: core
depends_on: [004-session-log.md, 008-tools.md, 010-context.md, 012-permissions-and-approvals.md, 018-credentials-and-secrets.md, 019-git.md, 038-routed-models.md, 039-questions.md]
affects: [internal/publish/, internal/hosted/, internal/config/, cmd/toposd/, harness/, harness/tools/, manifest/, session/, prompts/, internal/toposcli/, docs/]
effort: medium
created: 2026-10-05
updated: 2026-10-05
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
public address when a version tag is pushed. Reaching it from the
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
| `POST {root}/apps` with `{"name", "visibility"}` | `201` with the app: `slug`, `name`, `url` (its public address), `repository.push_url` |
| `GET {root}/apps/{slug}` | the app, or `404` |
| `GET {root}/apps/{slug}/deploys` | `{"deploys": [...]}`, newest first, each with `id`, `status`, `preview`, `commit_sha`, `preview_url` (the deploy's own address) and `error` (`{code, message}` or null) |
| `GET {root}/apps/{slug}/deploys/{id}/logs` | the build log, one JSON object per line with its `line` |
| `GET {root}/apps/{slug}/releases` | `{"releases": [...]}`, each with `tag`, `commit_sha`, `status`, `reason` |
| `GET {root}/apps/{slug}/releases/{tag}` | `{"release": ..., "deploy": ...}`, or `404` until the host has seen the tag |

A push of a branch to the app's repository builds a preview deploy,
whose status moves through `waiting`, `queued`, `building` and `built`
to `ready`, `failed` or `canceled`. A pushed tag that starts with `v`
makes a release of its commit, whose status is `pending`, then
`released`, `refused` (with a `reason`) or `failed`. An error answers
`{"error": {"code", "message"}}`. Latere's Apps is one such host.

| Variable | Default | Meaning |
|---|---|---|
| `TOPOS_APPS_URL` | unset | the app host's root. Unset, no session is offered `publish`. Needs `TOPOS_ORIGO_URL`, the git host the app's repository must be on |
| `TOPOS_APPS_AUDIENCE` | `apps` | the audience of the session token the runner presents to the app host |

### The tool

| Name | Parallel | Effect | Input | Limits |
|---|---|---|---|---|
| `publish` | no | external | `path` (the folder; a relative path resolves against the working directory; default the working directory), `release` (default false) | the wait below |

`publish` is a tool of the session's own thread, as `question` is
([[039-questions]]): the runner adds it to the registry when the
agent's `spec.tools` names it, the installation configures an app host
and the session runs on a Cella machine; a thread a spawn started
holds none, so a session has one app. The manifest takes `publish` by
its name alone, like `question`; `topos run` refuses an agent that
names it, since a local run has no app host and no sandbox.

**Publishing a folder.**

1. **The app.** The session's app is the slug the thread's last
   `publish` result records. The runner reads it (`GET`); a slug the
   host no longer knows, and a session with none, creates one: `POST
   {root}/apps` with the session's title as `name`, from which the host
   derives the slug, and `visibility: public`. The request carries the
   session's token for `TOPOS_APPS_AUDIENCE` and the workload
   `session`: the sandbox never holds it, and the authorizer decides
   the create as the session's agent within its initiator's reach.
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

**Releasing.** `release: true` takes the commit of the thread's last
`ready` preview, and the app it was published to. A release of that
commit the host already lists answers with its status. Otherwise the
tag is `v<n>`, one past the highest `v` followed by digits among the
app's releases, pushed on the commit from the session's git directory,
which fetches the session's branch first when it no longer holds the
commit. The runner then reads the release every `PollEvery`, a `404`
meaning the host has not seen the tag yet, until it is `released`,
`refused` or `failed`, for at most `Wait`. Whether the session's agent
may release is the host's question to the installation's authorizer,
asked for the tag's pusher.

| Limit | Value |
|---|---|
| `Wait`, the longest a call waits for a preview or a release | 10 minutes |
| `PollEvery`, how often the host is read while waiting | 3 seconds |
| `LogTail`, the lines of a failed build's log the model reads | 40 |
| the commit and push in the sandbox | 5 minutes |

### The result

The text is the model's: what was published, the address, and the next
step. A ready preview's text tells the model to call `publish` with
`release: true` next, which waits for the person: in `confirm` the
release is an ask, and a person's message in its place denies it with
the message as the note ([[012-permissions-and-approvals]]), so asking
for a change is the other answer.

The result's `meta.publish` is the tool's record, which the thread's
state folds and a client reads:

| Field | Value |
|---|---|
| `app`, `name` | the app's slug and its name |
| `url` | the app's public address |
| `preview` | the preview deploy's own address, once the host names it |
| `commit` | the commit published or released |
| `deploy` | the deploy's id |
| `status` | `ready`, `building` (the wait ran out), `failed`, `canceled`; for a release `released`, `pending` (the wait ran out), `refused`, `failed` |
| `release` | the tag, on a release |
| `error` | `{code, message}` for `failed`, `canceled` and `refused` |

The outcome is `ok` for `ready`, `building`, `released` and `pending`,
and `error` for the others and for every failure before a deploy
exists: a path that is no folder, a release with no ready preview, an
app host or git host that refused or could not be reached, and an
installation that mints no token for the app host. Those carry no
`meta.publish`, or one with the app alone once it exists.

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
| A session whose agent names `publish` publishes a folder of its sandbox: the app is created with the session's title and `public`, with the session's token for the app host's audience and the workload `session`, the sandbox's git pushes the folder to the session's branch with the session's author and trailers, the result names the preview's address with `meta.publish` status `ready`, and no token reaches the sandbox, an event or a tool result | `internal/hosted.TestASessionPublishesAFolderAndReleasesIt` over the stub Cella, a stub app host and git's own http backend | not built |
| A second publish reuses the app, pushes a new commit on the same branch, and a publish with nothing changed pushes no new commit | `internal/hosted.TestASessionPublishesAFolderAndReleasesIt` | not built |
| `release: true` pushes `v1` on the last ready preview's commit, the next release `v2`, and the result is `released` with the app's address | `internal/hosted.TestASessionPublishesAFolderAndReleasesIt` | not built |
| A failed build answers `failed` with its code, its message and the tail of its log; a refused release answers `refused` with its reason; a wait that runs out answers `building` or `pending` | `internal/publish.TestOutcomes` | not built |
| A path that is no folder, a release with no ready preview, a push URL on another host, an app host that refuses, and an installation that mints no token for it each answer an error the model reads | `internal/publish.TestRefusals` | not built |
| The runner offers `publish` only to the session's own thread, only when the agent names it, the app host is configured and the machine is Cella; the manifest takes it by name alone; `topos run` refuses it | `internal/hosted.TestPublishIsOfferedWhenConfigured`, `manifest.TestValidationRules`, `harness.TestASpawnedThreadHoldsNoPublish`, `internal/toposcli.TestAgentManifestRefusals` | not built |
| `TOPOS_APPS_URL` without `TOPOS_ORIGO_URL`, or one that is not an http URL, stops the start | `internal/config.TestLoad` | not built |
| A routed session's every request carries its route as a system part, and an unrouted one's none | `harness.TestTheRequestNamesTheRoute` | not built |

## Outcome
