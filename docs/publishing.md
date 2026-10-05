# Publishing: putting a session's folder online

An agent that holds the `publish` tool can put a folder of its sandbox
online as a web app, at an address anyone can open, and release it to
the app's own address when the person says so. This page is for the
operator who connects an app host, and for the client that shows what
was published. Everything here is on the wire of `/v1`.

## What the agent does

One call publishes a folder:

```json
{"path": "site"}
```

The first call creates the session's app at the installation's app
host, named after the session's title; every later call of the session
publishes to the same app. The folder is committed in the sandbox and
pushed to the session's own branch of the app's repository, the host
builds a preview of the push, and the call waits until the preview is
built. It answers with the preview's address, or with why the build
failed and the end of the build log, so the agent can fix it and
publish again. A call whose wait runs out says the preview is still
building; calling it again with the same folder waits on the same
build.

A second call releases the newest preview to the app's address:

```json
{"release": true}
```

The newest preview is the last one that is ready or still building; a
failed or canceled one, and a call the person denied, are passed over. A
preview still building is released once it is ready. The release is a
`v` tag on the preview's commit, `v1` and up. Whether
the session's agent may release is the app host's question to the
installation's authorizer.

`publish` reaches outside the sandbox, so in the `confirm` and
`progressive` modes every call waits for the person's approval, the
release included. A person's message in place of the approval denies
the call with the message as the note, which is how a person asks for
a change instead of a release.

An agent holds the tool only when its manifest names it, beside the
tools it builds the site with:

```yaml
spec:
  tools: [read, write, edit, bash, publish]
  machine: {kind: cella}
```

The tool belongs to the session's own thread: a thread a spawn starts
holds none, so a session has one app. `topos run` refuses an agent that
names it, since a run on a person's machine has no app host and no
sandbox.

## What a client reads

Every `tool.result` of a `publish` call carries `meta.publish`:

| Field | Value |
|---|---|
| `app`, `name` | the app's slug and its name |
| `url` | the app's own address |
| `preview` | the preview's own address, once the host names it |
| `commit` | the commit published or released |
| `deploy` | the deploy's id at the host |
| `status` | `ready`, `building`, `failed` or `canceled` for a preview; `released`, `pending`, `refused` or `failed` for a release |
| `release` | the tag, on a release |
| `error` | `{code, message}` for `failed`, `canceled` and `refused` |

A client shows a `ready` preview with its address, and may frame it
beside the conversation. A `publish` call with `release: true` that
waits for the person's approval is the release a person makes with one
answer.

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
| `POST /apps` with `{"name", "visibility"}` | `201` with the app: `slug`, `name`, `url`, `repository.push_url` |
| `GET /apps/{slug}` | the app, or `404` |
| `GET /apps/{slug}/deploys` | `{"deploys": [...]}`, newest first, each with `id`, `status`, `preview`, `commit_sha`, `preview_url` and `error` |
| `GET /apps/{slug}/deploys/{id}/logs` | the build log, one JSON object with its `line` per line |
| `GET /apps/{slug}/releases` | `{"releases": [...]}`, each with `tag`, `commit_sha`, `status` and `reason` |
| `GET /apps/{slug}/releases/{tag}` | `{"release": ..., "deploy": ...}`, or `404` until the host has seen the tag |

A push of a branch builds a preview deploy, which ends `ready`,
`failed` or `canceled`; a pushed `v` tag releases its commit, which
ends `released`, `refused` or `failed`. A refusal answers
`{"error": {"code", "message"}}`.
