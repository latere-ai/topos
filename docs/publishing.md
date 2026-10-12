# Publishing: putting a session's folder online

An agent that holds the `publish` tool can put a folder of its sandbox
online as a web app, at an address of its own, and release it to the
app's own address when the person says so. Who can open the app is the
app host's to keep: the tool asks for no visibility when it creates an
app, records the one the host answers, and tells the model when the app
is private, so the agent does not hand out an address others cannot
open. The agent can open the app to anyone with its link, or close it
again, in a call the person approves every time. This page is for the
operator who connects an app host, and for the client that shows what
was published. Everything here is on the wire of `/v1`.

## What the agent does

One call publishes a folder:

```json
{"path": "site", "name": "Tide Tables"}
```

The first call creates the session's app at the installation's app
host, with the name the call gives it; every later call of the session
publishes to the same app. The folder is committed in the sandbox and
pushed to the session's own branch of the app's repository, the host
builds a preview of the push, and the call waits until the preview is
built. It answers with the preview's address, or with why the build
failed and the end of the build log, so the agent can fix it and
publish again. A call whose wait runs out says the preview is still
building; calling it again with the same folder waits on the same
build.

`name` is the site's name, as a person would call it: two to four
words, in the language of the site. The agent gives it the first time
it publishes a site. It becomes the app's name, and the app host makes
the app's address from it, so a site about tide tables is not named
after the question that started the conversation. The name is put on
one line and cut to 80 characters. A call that gives none creates the
app with the session's title.

The name is read once, by the call that creates the app. A later
publish or release of the session, a call that publishes an app the
installation attached, and a call with `access` ignore it, and none is
refused for it: the tool renames no app, and a person renames theirs at
the app host. The result names the app as the host made it, and the
agent gives the person the address the result gives, which may differ
from the name where the host had to choose another.

`release` puts a change live in one call:

```json
{"path": "site", "release": true}
```

The call publishes the folder as above, waits for its build, and
releases the commit it built to the app's address. A build that fails
releases nothing, and the call answers with why and the end of the
build log; a build still running when the wait ends is released by the
host once it is ready. The release is a `v` tag on the commit, `v1` and
up. Whether the session's agent may release is the app host's question
to the installation's authorizer. A preview first is for a person who
wants to look before the change goes live; a release after it pushes
nothing new while the folder is unchanged, and releases that preview.

With no `path`, a call publishes the folder the session last published
to its own app, else the working directory. A release that names
neither `path` nor `app` takes the app the session has a preview of,
else the one app it has published, and is refused `app_required` when
that could be several.

`publish` reaches outside the sandbox, so in the `confirm` and
`progressive` modes every call waits for the person's approval, and a
change that goes live in one call asks once. A person's message in
place of the approval denies the call with the message as the note,
which is how a person asks for a change instead of a release. A call
bound to be refused on what it names, such as an app the session was
not given, is answered at once and never asks.

An agent holds the tool only when its manifest names it, beside the
tools it builds the site with:

```yaml
spec:
  tools: [read, write, edit, bash, publish]
  machine: {kind: cella}
```

The tool belongs to the session's own thread: a thread a spawn starts
holds none. A thread publishes to its own app, created at its first such
call, and to each app the installation attached to the session. `topos
run` refuses an agent that names it, since a run on a person's machine
has no app host and no sandbox.

## Opening an app to others

`access` changes who can open an app, and nothing else:

```json
{"access": "link"}
{"access": "private", "app": "tide-tables"}
```

| `access` | Who opens the app | What the app host is asked for |
|---|---|---|
| `link` | anyone who has its address, without signing in | `unlisted` |
| `private` | the person and the people they share it with | `private` |

The call publishes and releases nothing, and opens no sandbox. It reads
the app, and when the app is not as asked already, asks the host for
the change and records the visibility the host answers. It never asks
for `public`, and for `link` it leaves an app that is `unlisted` or
`public` already as it is.

`app` names the app. With none, the call takes its app as a release
that names no folder does: the app the session has a preview of, else
the one app it has published. It is refused `app_required` when that
could be several or the session has published none, since it creates no
app, and `access_alone` when it also names `path` or `release`. Each is
refused before the call is decided.

**Every call with `access` waits for a person.** Nothing runs it without
one: not an `alwaysAllow` pattern of the agent, not an answer a person
gave earlier with `remember`, not the `progressive` mode's score, and
not a decision service. In `plan` it is blocked, as every call that is
not read-only is. A session nobody attends waits, and changes nothing.
This is stricter than a release, which an agent whose `alwaysAllow`
names `publish` makes without asking.

Whether the session may change the app is the app host's question to
the installation's authorizer, asked with the session's token. The
token names the session and not the person who approved the call, so
an installation that lets several people send to one session decides,
where it answers `session.send` for a `user.tool_confirmation`, that
nobody approves a change they could not make themselves. A
refusal is the call's error result: the model is told why, to tell the
person, and not to try another way. When the host names the reason
`role_insufficient`, the app belongs to an organization and the
person's role there does not reach its settings, and the model is told
that only the organization's owners and admins change who can open it.

The tool's description gives the model the behavior. After it
publishes a private app, it says that the app is online and private and
offers to open it to anyone with the link. It calls `access` with
`link` when the person asks to share the site, show it to others, send
it to someone or make it public, and once the publish has answered when
they said so before. It never calls it for a site the person did not
ask to share. After `link` it says that anyone with the link can open
the site, and never that the site is public.

## Keeping an app going across sessions

An installation's authorizer may attach existing apps to a session,
each the repository of its source with the app it publishes. The
session then starts with a checkout of each one, in the directory of
the app's slug under the working directory, on the session's own branch
at the commit the app serves: its live release, else its newest ready
preview, else the repository's default branch. The model reads which
apps it was given, with their slugs and addresses.

`app` names the app a call publishes to or releases:

```json
{"app": "tide-tables"}
{"app": "tide-tables", "release": true}
```

Publishing an attached app commits its checkout on the session's branch
and pushes that branch, so each publish builds on the history it
started from, and a merge or a conflict is plain git in the checkout.
`path` may name the checkout or a folder inside it; a folder outside it
is refused `not_in_checkout`. With no `app`, a folder inside an attached
app's checkout publishes that app, and any other folder the session's
own app. An `app` that is neither attached nor one the session made is
refused `app_not_attached`. A release that names neither `app` nor
`path` is refused `app_required` while previews of several apps stand,
and in a session given apps that has published none. Each of these is
refused before the call is decided, so nobody is asked to approve it.
The thread keeps a standing preview per app, so releasing one app never
takes another's.

Several sessions may build one app at once, each on its own branch.
Before it tags a release, the tool requires the commit the app serves
to be an ancestor of the commit it releases. Otherwise the release is
refused, `status` `refused` with `error.code` `behind_live`, no tag is
pushed, and the model is told to merge the live release into its
checkout and release again. The check and the tag are two
steps, so two releases a few seconds apart can still pass it together;
the later one then serves, and the earlier session's next release is
refused until it merges.

## What a client reads

Every `tool.result` of a `publish` call carries `meta.publish`:

| Field | Value |
|---|---|
| `app`, `name` | the app's slug and its name |
| `url` | the app's own address |
| `visibility` | who may open the app, as the host named it when the call ran; absent where the host names none. On the result of a call with `access`, the host's answer to the change, or what the app still has when the change was refused |
| `preview` | the preview's own address, once the host names it |
| `folder` | the folder published, relative to the working directory when it is inside it: an attached app's checkout, or the folder of the session's own app. The result of a call with `access` repeats the folder of the session's last result for the app |
| `commit` | the commit published or released |
| `deploy` | the deploy's id at the host |
| `status` | `ready`, `building`, `failed` or `canceled` for a preview; `released`, `pending`, `refused` or `failed` for a release, or the status of its build when the build failed, was canceled or had not started, and nothing was released. Absent on the result of a call with `access` |
| `release` | the tag, on a release |
| `error` | `{code, message}` for `failed`, `canceled` and `refused`; `{code, reason, message}` for a change of who can open the app that the host refused, `reason` being the token the host names, such as `role_insufficient`, absent when it names none; and the code of a call refused before any request: `app_not_attached`, `app_required`, `not_in_checkout` or `access_alone`, whose `app` is empty and which has no `agent.tool_use` |
| `attached` | true for an app the installation attached, absent for the session's own |

A client shows a `ready` preview with its address, and may frame it
beside the conversation. A `publish` call with `release: true` that
waits for the person's approval is the release a person makes with one
answer.

A client tells the three calls apart by the input of the call's
`agent.tool_use`: `access` set is a change of who can open the app,
`release` true is a release, and neither is a preview. A call with
`access` names neither `path` nor `release`. The input of the call that
creates the session's app may carry `name`, the name the app is created
with; `meta.publish.name` and `url` of its result are the name and the
address the app has.

A call with `access` that waits is a question to the person. Its
`agent.tool_use` has `verdict` `ask`, and the session is idle with
`stop_reason` `tool_confirmation`:

```json
{"type": "agent.tool_use", "payload": {"tool_use_id": "toolu_3", "name": "publish", "input": {"access": "link"}, "verdict": "ask", "reason": "changes who can open the app, which a person approves every time", "mode": "confirm"}}
{"type": "session.status", "payload": {"status": "idle", "stop_reason": "tool_confirmation"}}
```

A client asks it in the person's words: for `link`, whether anyone with
the link may open the app; for `private`, whether only the person and
the people they share it with may. The app is the one `input.app`
names, else the app of the session's last `publish` result that names
one. The person's answer is a `user.tool_confirmation` for the call,
`POST /v1/sessions/{id}/events`:

```json
{"type": "user.tool_confirmation", "payload": {"tool_use_id": "toolu_3", "decision": "allow"}}
{"type": "user.tool_confirmation", "payload": {"tool_use_id": "toolu_3", "decision": "deny", "note": "leave it"}}
```

A client offers nothing to remember with it: a remembered pattern never
runs a later call with `access`, and one that names `publish` would run
later previews and releases without asking. The call's result names the
app and who can open it now, with no `status`, which is how a client
tells it from a preview's and a release's:

```json
{"type": "tool.result", "payload": {"tool_use_id": "toolu_3", "outcome": "ok", "meta": {"publish": {"app": "a-poem", "name": "A poem", "url": "https://a-poem.apps.example", "visibility": "unlisted", "folder": "site"}}}}
```

A refused change is an error result whose `meta.publish` keeps the
visibility the app still has and carries the host's refusal:

```json
{"type": "tool.result", "payload": {"tool_use_id": "toolu_7", "is_error": true, "outcome": "error", "meta": {"publish": {"app": "a-poem", "name": "A poem", "url": "https://a-poem.apps.example", "visibility": "private", "folder": "site", "error": {"code": "forbidden", "reason": "role_insufficient", "message": "You do not have permission to do that."}}}}}
```

A denied call's result has outcome `denied` and no `meta.publish`.

Every request of a session that runs by a routed name, such as a tier
an installation declares, carries that name to the model as
`Model route: <name>`, so an agent can say when another route would
serve the person better.

## Connecting an app host

| Variable | Meaning |
|---|---|
| `TOPOS_APPS_URL` | the app host's root; unset, no session is offered `publish` |
| `TOPOS_APPS_AUDIENCE` | the audience of the session token the runner presents to it; `apps` by default |

The host needs `TOPOS_ORIGO_URL`, the git host its app repositories are
on, since the sandbox's git sends the session's credential to that host
alone, and an identity provider, since the runner reaches the host with
a token minted for the session. The token never enters the sandbox.

The host answers this contract under its root:

| Request | Answer |
|---|---|
| `POST /apps` with `{"name"}` | `201` with the app: `slug`, `name`, `url`, `repository.push_url`, and `visibility` when the host keeps one |
| `GET /apps/{slug}` | the app, or `404`; with `current_deploy` and `latest_preview`, each `{"id", "commit_sha"}` or `null`, the commit its address serves and its newest ready preview's, which the start of an attached app's checkout and the check before a release read |
| `PATCH /apps/{slug}` with `{"visibility": "unlisted"}` or `{"visibility": "private"}` | the app, with its `visibility` after the change |
| `GET /apps/{slug}/deploys` | `{"deploys": [...]}`, newest first, each with `id`, `status`, `preview`, `commit_sha`, `preview_url` and `error` |
| `GET /apps/{slug}/deploys/{id}/logs` | the build log, one JSON object with its `line` per line |
| `GET /apps/{slug}/releases` | `{"releases": [...]}`, each with `tag`, `commit_sha`, `status` and `reason` |
| `GET /apps/{slug}/releases/{tag}` | `{"release": ..., "deploy": ...}`, or `404` until the host has seen the tag |

A push of a branch builds a preview deploy, which ends `ready`,
`failed` or `canceled`; a pushed `v` tag releases its commit, which
ends `released`, `refused` or `failed`. A refusal answers
`{"error": {"code", "message"}}`, and may name why in `details.reason`,
a lower-case token of letters, digits and underscores, which a result
records beside the code. The tool reads one such reason itself:
`role_insufficient`, for an organization's app whose settings the
caller's role there does not reach.

`unlisted` is an app anyone with its address opens, `private` one that
opens for its owner and the people it is shared with, and `public` one
the host also shows or lists, which the tool reads and never asks for.
A change answers the visibility that was asked; an answer that names
another one, or none, is a change that failed.
