---
title: "What a create's allow attaches: repositories with the app each one publishes, read-only files fetched by the runner, and context text, each fixed for the session's life"
status: drafted
track: core
depends_on: [004-session-log.md, 006-identity.md, 010-context.md, 011-instructions-and-skills.md, 015-api.md, 016-runners.md, 018-credentials-and-secrets.md, 019-git.md, 043-publishing-a-folder.md, 053-the-initiators-instructions.md, 057-a-sessions-metadata-in-its-questions-and-lists.md]
affects: [authorizer/, internal/server/, session/, runner/, harness/, prompts/, internal/hosted/, internal/config/, api/, docs/]
effort: medium
created: 2026-10-07
updated: 2026-10-07
author: changkun
---

# What a create's allow attaches

## Overview

An installation that files sessions under a label ([[057-a-sessions-metadata-in-its-questions-and-lists]])
often holds things that belong to the label: the source of the apps a
person keeps working on, documents they keep, and a few lines that say
what the work is about. A session started under the label should begin
with them, and which of them a person may reach is the authorizer's
answer, not the client's request.

The authorizer already answers `session.create` with members the core
applies: a model, a network, a scope, the initiator's instructions
([[053-the-initiators-instructions]]). This spec adds three:

| Member | What the session gets |
|---|---|
| `repositories` | more repositories, each optionally naming the app at the installation's app host that it is the source of; delivered like the request's, with an app's checkout starting at the app's live commit |
| `files` | read-only files the runner fetches with the session's token and writes into the machine |
| `context` | titled text the model reads after the instructions, for the session's whole life |

Each is read on an allow of `session.create` and `session.fork` alone,
stored on the session, and changed by nothing afterwards. A new block
of the system prompt names the attached apps and files on every
request.

## Current state

- `authorizer.Limits` and `WireLimits` (`authorizer/limits.go`) carry
  the approvals, the budget, the timeouts, the scope, the retention, an
  owner, the model, the reasoning level, the network and
  `instructions`, at most `MaxInitiatorInstructions` (8 KiB). A member
  the core does not know is ignored, and a known member that does not
  decode refuses the request as `authorizer_unavailable`
  (`DecodeLimits`).
- A session's resources are fixed before the question: the request's
  repositories, or the agent version's when the request names none
  (`createSession` in `internal/server/sessions.go`); `repositoriesField`
  puts them in the question; `sess.Resources = resources` after the
  allow. Only `repository` resources pass `checkRepositories`, at most
  `session.MaxRepositories` (8).
- `runner/repos.go` `deliver` fetches each repository into the
  session's first machine, the first into the working directory and
  each further one into the directory `session.RepositoryDirs` derives
  from its URL's last segment, and checks out the session's branch
  `agents/<agent>/<session>` at `ref`, or at the default branch. A
  repository that cannot be delivered is a `repository_unavailable`
  error beside the machine, and the session goes on.
- `harness/request.go` `withRepositories` renders
  `context/repositories-v1` in the context block's place until a
  machine is recorded; `systemBlocks` puts the harness prompt, the
  agent's instructions, the initiator's instructions
  (`context/initiator-v1`) and then the parts of the machine.
- The files of a person's messages are written into each new machine
  under `attachments/` of the working directory and recorded by
  `attachments.delivered` (`session/attachment.go`,
  `harness.DeliverAttachments`).
- The app host's contract ([[043-publishing-a-folder]]) answers an
  app with `slug`, `name`, `url` and `repository.push_url`, and says
  nothing of which commit the app serves.

## Design

```mermaid
sequenceDiagram
  participant C as client
  participant T as toposd
  participant A as installation's authorizer
  participant R as runner
  participant H as app host
  participant F as storage service
  participant M as machine
  C->>T: POST /v1/sessions {agent, metadata}
  T->>A: session.create {..., repositories: [request's], metadata}
  A-->>T: allow {repositories: [{url, app}], files: [{url, path, size}], context: [{title, text}]}
  T->>T: Resources += repository and file resources; Session.Context = context
  R->>H: GET /apps/{slug} (session token for the app host)
  H-->>R: current_deploy.commit_sha
  R->>M: fetch the repository, branch agents/<agent>/<session> at that commit, into <workdir>/<slug>
  R->>F: GET each file (session token for TOPOS_FILES_AUDIENCE)
  R->>M: write $HOME/files/<path>, mode 0444
  R->>M: every request: instructions, context parts, attachments block
```

### The members

```json
{
  "repositories": [
    {"url": "https://git.example.com/r/7f3c….git",
     "app": {"slug": "tide-tables", "name": "Tide tables", "url": "https://tide-tables.apps.example.com"}}
  ],
  "files": [
    {"url": "https://storage.example.com/v1/files/u-1/projects/p1/notes.md", "path": "notes.md", "size": 2048, "media_type": "text/markdown"}
  ],
  "context": [
    {"title": "Project", "text": "Tide tables for the harbor club. Keep the page readable on a phone."},
    {"title": "Memory", "text": "- ent_01…: The club's color is navy.\n"}
  ]
}
```

| Member | Field | Rule |
|---|---|---|
| `repositories[]` | `url`, `ref` | as a request's repository: `session.CheckRepository` with `https` |
| | `app.slug` | the app's slug at the app host: `^[a-z0-9][a-z0-9-]{0,62}$` |
| | `app.name` | one line, at most 200 characters, no control character |
| | `app.url` | the app's public address, an absolute `https` URL |
| `files[]` | `url` | an absolute `https` URL with no user information and no fragment; a query is allowed, since a storage service may need one |
| | `path` | relative, `/` separated, no empty or `..` segment, at most 512 bytes, unique among the files |
| | `size` | the file's size in bytes, at least 0 |
| | `media_type` | a media type, optional |
| `context[]` | `title` | one line, 1 to 100 characters |
| | `text` | 1 or more bytes of UTF-8 |

| Bound | Value | Holds |
|---|---|---|
| `session.MaxRepositories` | 8 (unchanged) | the request's or the agent's repositories and the allow's together |
| `session.MaxFiles` | 64 | `files` |
| `session.MaxFileBytes` | 268435456 (256 MiB) | the sum of `files[].size` |
| `session.MaxContext` | 32768 bytes | the sum of every part's `title` and `text` |

An allow past a bound, or a member that breaks its rule, does not
decode, and the create is refused `authorizer_unavailable` with the
member named in the developer detail: the core cannot apply what it
was told, which is `DecodeLimits`'s rule. The question already names
the request's repositories in `repositories`, so the authorizer knows
the room it has: `session.MaxRepositories` less that count. An allow
repository whose URL the request already names is dropped, and the
request's keeps its place and its `ref`.

An allow with `files` on an installation without `TOPOS_FILES_AUDIENCE`
is refused the same way, since the runner has no credential to fetch
them with.

### Where they are kept

- `session.Resource` gains `App *ResourceApp` (`slug`, `name`, `url`),
  for a repository, and the fields of a file: type `file`
  (`session.ResourceFile`), `url`, `path`, `size`, `media_type`. The
  allow's repositories and files are appended to `Session.Resources`
  after the request's and the agent's, in the allow's order.
- `Session.Context []ContextPart` (`title`, `text`), from the allow,
  beside `Instructions`. No event changes it, as no event changes the
  instructions.
- A fork is asked as a create is, and its own allow sets its members.
  The files and context of the session it forks are not carried, so a
  fork reaches what its own question was allowed.
- `POST /v1/sessions` still refuses a `file` resource and a
  repository's `app` from a request: a request that named a file would
  make the runner send the session's token to an address the client
  chose, and an `app` the installation never vouched for would steer
  `publish` ([[059-publishing-to-a-chosen-app]]).

### Repositories with an app

A repository with `app` is delivered as every repository is, into the
session's first machine, with the session's author, branch and trailer
hook ([[019-git]]), with two differences:

1. **Its directory is the app's slug**, `<workdir>/<slug>`, and never
   the working directory itself, made unique with `-<i>` like any
   other. The working directory stays the request's first repository,
   or an empty directory, whatever number of apps a session is given.
2. **Its branch starts at the commit the app serves.** With no `ref`,
   the runner reads the app at the installation's app host
   (`GET {TOPOS_APPS_URL}/apps/{slug}`, the session's token for
   `TOPOS_APPS_AUDIENCE`) and checks the session's branch out at
   `current_deploy.commit_sha`, else `latest_preview.commit_sha`, else
   the default branch. The delivery fetches tags beside branches, so a
   released commit is present wherever its branch went. A host that
   cannot be read, or an installation with no app host, starts at the
   default branch, and the delivery says so. A `ref` the allow names
   wins over the host's answer.

`session.DeliveredRepository` gains `app` (the slug) and `base`
(`live`, `preview` or `default`), so the session's log records which
version each checkout started from; `Commit` is that commit.

The app host's answer to `GET {root}/apps/{slug}` gains, beside the
members [[043-publishing-a-folder]] reads:

```json
{"slug": "tide-tables", "name": "Tide tables", "url": "https://tide-tables.apps.example.com",
 "repository": {"push_url": "https://git.example.com/u-1/tide-tables.git"},
 "current_deploy": {"id": "9f2c…", "commit_sha": "4b1e…"},
 "latest_preview": {"id": "a03b…", "commit_sha": "c77d…"}}
```

`current_deploy` is the deploy at the app's public address and `null`
before the first release; `latest_preview` the newest preview that is
ready, `null` when none is. A host that answers without them is read as
`null` for both.

The installation decides, through the scope its allow carries
([[018-credentials-and-secrets]]), whether the session may push to
each attached repository; the core attaches what it is told and the git
host refuses what the scope does not cover.

### Files

At every machine a session opens, after the machine is recorded and
before its first tool runs, the runner fetches each `file` resource
and writes it into the machine at `$HOME/files/<path>`, mode `0444`,
under a directory the agent's own commands cannot replace while the
fetch runs:

- the request is `GET <url>` with `Authorization: Bearer` and the
  session's token for `TOPOS_FILES_AUDIENCE`, workload `session`, from
  the drive's token source; a redirect to another host is followed
  without the header, as Go's client does, so a storage service may
  answer with a short-lived address of its own;
- the body is streamed into the machine and must be exactly `size`
  bytes; a longer body is cut and refused, a shorter one refused;
- each file has 5 minutes; a failure leaves that path absent;
- the runner appends `files.delivered` with the paths written and, for
  each one not written, `{path, code}` (`file_unavailable`,
  `file_size_mismatch`), and one `session.error` `file_unavailable` that
  is not retryable when any is missing, so the model and the person
  learn which file the machine lacks; the turn goes on.

Files are delivered on a Cella machine alone. A session on a host
machine lists them in the attachments block as not delivered, because a
host machine's home is the server's own.

No token reaches the machine: the runner holds it, as it does for the
app host ([[043-publishing-a-folder]]).

### Context

`context/attached-v1`, one system part per `context` entry, in the
allow's order, right after the initiator's instructions:

```
<installation_context title="Project">
Tide tables for the harbor club. Keep the page readable on a phone.
</installation_context>
```

The template says once, before the first part, that the installation
that runs the session gave this text, and that it ranks below the
agent's instructions and the person's messages, as the initiator's
instructions do ([[053-the-initiators-instructions]]). The parts change
with nothing, so they sit in the stable prefix of every request and
cost no cache ([[010-context]]).

### The attachments block

`context/attachments-v1`, rendered from the session's header on every
request, after the context parts and before the machine's parts, when
the session has a repository with `app` or a file:

```
<attachments>
Apps, each a checkout of its source on your branch agents/assistant/ses_01…:
- tide-tables/ : Tide tables, published at https://tide-tables.apps.example.com (publish with app "tide-tables")
Files, read-only:
- ~/files/notes.md (2 KB, text/markdown)
</attachments>
```

`context/repositories-v1` no longer lists a repository with `app`, so
no repository is named twice before the machine opens.

### Decisions

| Decision | Chosen | Alternatives and why not |
|---|---|---|
| Who attaches | the allow of `session.create` | the client's request: which of a person's things a session reaches is the authorizer's to decide, and a client would have to be trusted with addresses it cannot verify |
| An app's checkout | its own directory named by its slug | the working directory for the first: a session's layout would depend on how many apps it was given, and work on a new app would start inside another's source |
| Where an app's branch starts | the commit the app serves, read by the runner at delivery | the default branch: the app host builds from pushed branches and tags, so the default branch need not hold what is live; a `ref` the authorizer computes at create: it would be stale by the time a machine opens, hours later in a long session |
| Files | fetched by the runner with the session's token, written read-only | mounted from the storage service: the core knows no storage service's protocol; carried in the context: binary files and sizes past a prompt's reach; fetched by the agent: a credential in the machine |
| A file's credential | a session token for `TOPOS_FILES_AUDIENCE` | the session's model key, as web search uses: a storage service is a core of the installation that verifies tokens, not a service that recognizes a key's hash; a short-lived signed address in the allow: it expires before a machine opens |
| Files on a host machine | listed, not delivered | written to the host's home: a self-hosted server's own account would receive them |
| Context text | its own member and template | joined into `instructions`: the initiator's instructions are the person's voice and bounded at 8 KiB for that reason; the installation's text is a different speaker |
| Changes after create | none | refreshing at each turn: every refresh would change the prompt's prefix and pay for the whole cache again, and a running agent would see its ground move |

### Roll order

1. The installation's authorizer sends the new members only to a core
   that reads them, which it learns from the core's version it pins.
   An older core ignores `repositories`, `files` and `context` as
   unknown members, so the session starts without them and nothing is
   refused.
2. The app host answers `commit_sha` on `current_deploy` and
   `latest_preview`. Until it does, an app's checkout starts at the
   default branch.
3. This release: the members, the delivery, the templates,
   `TOPOS_FILES_AUDIENCE`.
4. The installation sets `TOPOS_FILES_AUDIENCE` and registers the
   audience with its identity provider for the runner's host client,
   then turns `files` on at its authorizer.

### Configuration

| Variable | Read by | Default | Meaning |
|---|---|---|---|
| `TOPOS_FILES_AUDIENCE` | `serve`, `runner` | unset | the audience of the session token the runner presents to fetch a `file` resource; unset, an allow that names files is refused |

## Not in this spec

Which repositories, files and text an installation attaches, and the
scope that lets a session push to an attached repository: the
authorizer's. Writing a file back from the machine to the storage
service. Publishing an attached app, which is
[[059-publishing-to-a-chosen-app]]'s. A change to a session's
attachments after create.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| An allow's `repositories`, `files` and `context` are decoded with their rules and bounds, appended after the request's resources with a repeated URL dropped, and stored on the session; an allow past a bound or breaking a rule, and files with no `TOPOS_FILES_AUDIENCE`, refuse the create `authorizer_unavailable` naming the member | `authorizer.TestTheAttachMembers`, `internal/server.TestACreateTakesTheAllowsAttachments` | not built |
| A request naming a `file` resource or a repository's `app` is `invalid_request` | `internal/server.TestARequestCannotAttach` | not built |
| A fork's attachments are its own allow's, not its parent's | `internal/server.TestAForkIsAttachedByItsOwnAllow` | not built |
| A repository with `app` is delivered into `<workdir>/<slug>`, never the working directory, on the session's branch at the host's `current_deploy.commit_sha`, else `latest_preview.commit_sha`, else the default branch, and `DeliveredRepository` records `app`, `base` and the commit | `runner.TestAnAppsCheckoutStartsAtItsLiveCommit` over a stub app host and git's http backend | not built |
| Each file is fetched with the session's token for `TOPOS_FILES_AUDIENCE`, written at `$HOME/files/<path>` mode `0444`, a redirect followed without the header, a size mismatch refused, and `files.delivered` with one `file_unavailable` error names what is missing; no token reaches the machine | `runner.TestFilesAreFetchedIntoEachMachine` over the stub Cella | not built |
| The context parts follow the initiator's instructions in every request, unchanged across turns; the attachments block names each app's directory, name, address and slug and each file, and the repositories block names no repository with `app` | `harness.TestTheAllowsContextAndTheAttachmentsBlock` | not built |
| A model given an attached app and asked to change it edits its checkout and not the working directory | a task of the suite ([[025-task-suite]]) | not built |

## Open questions

None. What a session is given, and from where, is the installation's
decision; the core fixes only how each member is applied, bounded and
shown.
