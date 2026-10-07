# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

### Added

- A session's `metadata` is a label a client files sessions under.
  `GET /v1/sessions` and `GET /v1/sessions/summary` take one
  `metadata.<key>=<value>` filter, which keeps the sessions holding
  that entry exactly, beside every other filter and under `group=tree`.
  `PATCH /v1/sessions/{id}` takes `metadata`, a merge in which a string
  sets a key and `null` deletes it; a body that names `metadata` alone
  is taken on an idle, a running and an ended session, and appends no
  event. A key is a letter or digit then at most 62 letters, digits,
  dots, underscores or hyphens, and a value at most 512 bytes
  (`session.MaxMetadataValue`) without a control character, at a create
  and at a change; a change is checked on the merged result.
- The authorizer reads a session's metadata: `session.create` and
  `session.fork` carry `metadata`, absent when the session has none, and
  a change asks `session.update` with `metadata`, the change as sent,
  and `current_metadata`, the values the named keys hold now.
- Postgres keeps each entry in `session_metadata`; migration 0009 adds
  the table and fills it from the sessions already stored.
- The authorizer's allow of `session.create` attaches what a session
  starts with beyond its request. `repositories`, each `{url, ref, app}`,
  follow the request's resources marked `attached`, at most 8 together;
  one with `app {slug, name, url}` is the source of that app at the
  installation's app host, checked out into `<workdir>/<slug>` on the
  session's branch at the commit the app serves
  (`current_deploy.commit_sha`, else `latest_preview.commit_sha`, else
  the default branch), fetched with its tags. `session.machine` records
  each app's `app`, `base` and, when the host could not be used,
  `base_error`. `context`, titled text of at most 32768 bytes together
  (`session.MaxContext`), is read by the model after the initiator's
  instructions for the session's life, and an attachments block names
  each app's checkout, name, address and slug. A fork carries what its
  parent's request named and what its own allow attaches. A request that
  names a repository's `app` or `attached` is `invalid_request`. An
  allow that breaks a member's rule is `authorizer_unavailable`, and the
  refusal's detail now names the member that did not decode.

### Upgrading

- Roll the authorizer first when clients will change a session's
  metadata: an authorizer that decides `session.update` by the fields it
  knows answers a change that names `metadata` alone with a deny, so the
  change is `forbidden` until it accepts `metadata` and
  `current_metadata`. An authorizer that ignores unknown fields reads the
  create and fork questions as before.
- Start filing and listing by a key once every replica runs this
  release: a session a replica of an earlier release creates writes no
  `session_metadata` rows, and a filtered list on Postgres misses it
  until its metadata changes.
- Have the authorizer send `repositories` and `context` only once every
  replica and runner runs this release; an earlier one ignores them and
  the session starts without them. This release does not read `files`.
  An app's checkout starts at the commit the app host serves once the
  host answers `commit_sha` on `current_deploy` and `latest_preview`,
  and at the default branch until then.

## v0.24.0 - 2026-10-07

### Added

- An answer's images are kept. Once a step of the session's own thread
  is committed, each local image its `agent.message` names, by a
  Markdown image or an HTML `img`, a path with no scheme or a `file:`
  URL inside the working directory, is read from the open machine,
  stored as a blob of the session, and recorded by `files.kept` before
  the next request or the turn's closing status. An image is PNG, JPEG,
  GIF or WebP by its bytes, never SVG, at most 5 MiB, 8192 pixels a side
  (`harness.MaxKeptImageSide`) and 40,000,000 pixels
  (`harness.MaxKeptImagePixels`); a message reads its first 8
  references (`tools.MaxImages`). A reference that is not kept is in
  `skipped` with its reason. A turn that opened no machine opens none to
  keep an image, and skips its references `no_machine`; a step that
  waits on a person keeps its images when its calls are answered.

### Upgrading

- Roll this release only once every runner, the external ones included,
  runs v0.23.0 or later: a runner before v0.23.0 does not know
  `files.kept` and stops a session whose log holds one with
  `schema_too_new`. The authorizer and the store need no change.
- Roll clients that draw an answer's images after it: a client reads
  `files.kept` and falls back to `GET /v1/sessions/{id}/files` where
  none was kept, and a client that does not know the type skips it.

## v0.23.0 - 2026-10-07

### Added

- A person can edit a message and ask again from that point. `POST
  /v1/sessions/{id}/fork` takes `before_seq`, the message a person sent
  that started a turn, and forks just before it: the new session holds
  the events before the message, a model change the person made after
  the last answer included, and `before_seq` 1 copies nothing. `message`,
  the edited message as a send takes it, is sent to the new session in
  the same call, and a runner starts its turn at once; a file of the
  original message is kept by naming its `blob`. `title` names the new
  session, in place of the continuation title. A message sent while a
  turn ran, a trigger's message, and any other event are
  `invalid_fork_point`.
- "Continue in a new conversation from here": a fork body's `tree:
  "new"` starts a tree of the new session's own. Its `root` is its own
  id and its `parent` still names the session it came from, so
  `group=tree` lists it as a conversation of its own, `root=` of the
  source leaves it out, and its edits are versions of it. Absent, a fork
  joins its parent's tree; any other `tree` is `invalid_request`.
  `session.fork` carries `root`, the root of the tree the new session
  joins.
- A fork with a message asks the authorizer `session.read`, then
  `session.fork`, then `session.send` of the new session, before anything
  is written; a deny of either writes nothing. A send refused after the
  fork was allowed is handed to the server's sink option as
  `session.fork` with the refusal's code as its outcome, and toposd logs
  it.
- A session made by a fork carries `root`, the session at the top of its
  tree of forks, beside `parent`. `GET /v1/sessions` takes `root`, a
  tree's sessions, `parent`, a session's forks, and `group=tree`, one
  session per tree, the newest, carrying `tree {root, sessions}`. The
  summary takes none of them. Postgres keeps `parent_id`, `parent_seq`
  and `root_id` in columns; migration 0008 adds them and fills them, and
  `root`, for the forks already stored.
- `docs/editing-a-message.md`: how a client sends an edited message and
  shows its versions.
- The `files.kept` event, read and not yet written: it follows an
  `agent.message` and maps each local image the answer names to a blob
  of the session, `{message, files, skipped}`. This release folds it
  into no prompt, so a log that holds one runs, and it is redactable.
  Redacting an `agent.message` redacts the `files.kept` that names it in
  the same append, one `event.redacted` each, and deletes an image no
  other event names; a redaction sent again for a redacted answer takes
  a `files.kept` that landed since. A redacted `files.kept` keeps the id
  of the answer it named, and no image. No runner of this release
  appends it.

### Changed

- A fork's budget counts its own spend. `budget.carried_cost_usd_micro`
  is the spend a fork copied from its parent, and the check before each
  model request holds `spent_cost_usd_micro` less it to the fork's
  ceiling, so a fork of a session that spent most of its budget runs on
  its own. This holds for a fork that continues an ended session too.
- A request's prompt cache key is the session's tree's root, its `root`
  or its id, so a fork and the session it came from share one key.
- `invalid_fork_point` says "A session is forked only at the end of a
  turn, or before a message of yours that started one."
- `GET /v1/sessions/{id}/blobs/{digest}` answers by the blob's leading
  bytes: PNG, JPEG, GIF or WebP as that image type, `inline`, and any
  other blob, SVG included, as an `application/octet-stream`
  `attachment`. Every blob carries `X-Content-Type-Options: nosniff`,
  `Content-Security-Policy: sandbox; default-src 'none'`,
  `Cache-Control: private, max-age=31536000, immutable` and its digest
  as `ETag`. An attachment's bytes are unchanged.

### Upgrading

- Roll toposd and every runner together; the authorizer needs no change.
  Migration 0008 runs at start, in one transaction. A replica of the
  earlier release that rewrites a fork's header during the roll drops
  `root` and `carried_cost_usd_micro`: a read fills `root` from its
  column, and that fork's budget counts its whole log, as before. A fork
  such a replica creates during the roll gets its `root` from the next
  release's migration.
- Roll every runner, the external ones included, onto this release
  before any runner onto the release that keeps images. A runner that
  does not know `files.kept` stops a session whose log holds one with
  `schema_too_new`, and leases move sessions between runners during a
  rolling update.
- Roll clients that edit messages or read trees after the server: an
  earlier server refuses `before_seq`, `message` and `title` in a fork
  body as unknown members and ignores the list's `root`, `parent` and
  `group`.

## v0.22.0 - 2026-10-07

### Added

- A session's title changes: `PATCH /v1/sessions/{id}` takes `title`,
  alone or beside `model` and `policy`. The title is trimmed, and one
  that is empty, longer than 200 characters (`session.MaxTitleLength`),
  or holds a control character or a line break is `invalid_request`.
  The authorizer is asked `session.update` with `title` beside the
  fields of the rest of the change, and an allowed change appends
  `session.title_changed` `{by, old, new}`, which the header's `title`
  follows. A rename does not move `updated_at`, so a list ordered by it
  keeps the session in its place, and a fork keeps the title its create
  gave it over the renames it copies.

### Changed

- An idle session can be archived, not only an ended one, so a client
  whose sessions stay open between turns can file one away. A running
  session is still `conflict`. An archived idle session takes a message
  as before, and stays archived while its turn runs.

## v0.21.0 - 2026-10-06

### Added

- A session has a network: where its sandbox may reach. An authorizer's
  allow of `session.create` may carry `limits.network`, `{mode, hosts,
  ask}`, with `mode` `open`, `allowlist` or `none`; the session records
  it as `network`, or its agent's mode with `ask` false when the allow
  names none. An agent names its own mode as `spec.machine.egressMode`
  beside `egress`, `allowlist` when absent. Under `allowlist` the agent's
  hosts, the hosts of the session's credentials and its repositories' git
  hosts are always joined, so a narrow answer never cuts a session off
  from its model gateway or its repositories. An allow of `session.send`
  that names another network replaces the session's before the next
  turn, recorded as `session.network_changed` straight before the sent
  event, and narrows or widens the running sandbox.
- A `web_fetch` of a host inside the network runs without asking, its
  reason `inside the session's network`. One outside it asks, its reason
  `outside the session's network`, when the network asks and the session
  is attended, and is blocked otherwise. A person's allow widens the
  running sandbox to the host before the fetch runs and records it as
  `session.network_changed`; the host stays reachable for the rest of the
  session. A widening Cella refuses closes the call with
  `network_unavailable` and changes nothing.
- A command's connection the egress gateway refused becomes an
  `approval.requested` after the command's result, `{approval_id,
  tool_use_id, source: egress, destination {host, port}, reason, verdict,
  more}`, at most 3 per command. With `verdict: ask` the session waits
  for a `user.tool_confirmation` that names the `approval_id`; an allow
  widens the network and appends `approval.decided`, and the agent may
  run the command again. A deny, or a message in place of an answer, is
  not asked again in the session. `topos confirm` takes an `apr_` id.
- An authorizer's allow of `session.create` may carry
  `limits.instructions`, the initiator's standing instructions, at most
  8 KiB of UTF-8. The session records them as `instructions`, and the
  agent reads them after its own instructions in the prompt's cached
  prefix, the same bytes on every turn; a send never changes them, and a
  fork takes its own allow's.
- `docs/network.md` is the contract an authorizer and a client follow.

### Changed

- On a Cella machine, a `web_fetch` of a host outside the session's
  network is blocked, and the model reads that the host is outside the
  network this session may reach. Before, it asked the person in
  `confirm` mode and then failed at the gateway. This holds for a
  session whose authorizer names no network too: it runs on its agent's
  mode and hosts with `ask` false, so a fetch outside the agent's
  `spec.machine.egress` is blocked where it was asked. A session on the
  host machine decides a fetch as before.

### Upgrading

- Roll the authorizer that answers `network` and `instructions` first: a
  server before this release ignores both members. Then roll toposd and
  every runner together, since a runner that reads a log must know
  `session.network_changed`, `approval.requested` and `approval.decided`
  before any is appended. Until a client answers by `approval_id`, a
  command's refused connection in a session whose network asks leaves
  the session waiting until the person's next message, which denies the
  host for the rest of the session; answer `ask` false until the client
  rolls.

## v0.20.0 - 2026-10-06

### Fixed

- A routed turn whose request the model's provider rejects can move to
  another model. When the gateway answers `upstream_rejected`, the
  provider's own 4xx and not one of the gateway's refusals, the turn asks
  the authorizer the same `session.update` it asks for a model that
  cannot serve, with the new `failed_reason: rejected` beside
  `failed_model`, and the gateway's code and developer detail as
  `failed_detail`, such as `upstream_rejected: upstream status 404: ...`.
  An allow that names another model moves the turn there, as one of its 3
  moves, and the conversation records the change with `model_busy`. An
  allow that names none, or a deny, ends the turn with `model_error` as
  before. Providers withdraw and change free models without notice, and
  until now such a turn ended with an error while the routed name stood
  for models that would have answered. The gateway's own refusals,
  `invalid_request`, `model_not_allowed`, `rate_limited`,
  `budget_exhausted` and the rest, never move a turn. An authorizer that
  reads `failed_model` but not `failed_reason` would read the question as
  one about a model that cannot serve and move the turn on any rejection,
  so the authorizer that reads the field rolls first, then toposd and
  every runner. The runner protocol's failover request carries `reason`
  when there is one; a server before it refuses that request as
  `invalid_request`, and the turn ends with `model_error` as before.
- Every model failure's `session.error` carries the gateway's developer
  detail. A `model_error`, a spend refusal and a `compaction_failed` read
  only the HTTP status and type, such as `HTTP 400 upstream_rejected`,
  while the provider's own status and words were on the failed
  `model.request` alone. Their `detail` now adds the gateway's
  `Lux-Error-Detail` in parentheses, at most 1024 bytes, as
  `HTTP 400 upstream_rejected (upstream status 404: {...})`; a turn the
  authorizer kept on its model adds why it did not move. The `message`
  is unchanged.

## v0.19.1 - 2026-10-05

### Fixed

- A turn that moves off a model that cannot serve now reaches the model
  the authorizer names. The authorizer widens the session's key to that
  model as it answers, and a gateway with several replicas applies the
  change on each within a moment of its own, so the runner could read
  the door's model list before the change reached it, find the model
  missing, and end the turn with `model_busy` and `model_unknown` in the
  detail. While a door lists other models and not the one the runner
  connects, the runner now reads the list again for up to 3 seconds
  (`hosted.DoorSettle`), which also covers a model a send moves the
  session to.
- A model the authorizer names that still cannot be connected no longer
  ends the turn. It counts as one of the turn's 3 moves, and the next
  `session.update` names it as `failed_model`, beside the model the
  session stands on as `current_model`, with why as `failed_detail`, so
  the authorizer passes over it too. The change the turn records says
  which models were passed over and why. An authorizer that holds
  `failed_model` to `current_model` refuses that question, and the turn
  ends `model_busy` as before, so such an authorizer rolls first to
  accept it. The runner protocol's failover request carries `standing`,
  the model the turn stands on, when it is not `failed`; a request
  without it stands on `failed`.

## v0.19.0 - 2026-10-05

### Added

- `GET /v1/sessions/search?q=` finds the sessions `GET /v1/sessions`
  would list for the caller by what was said in them: the text of the
  messages a person sent and the answers the agent wrote on the
  session's own thread, and a person's answers to the agent's
  questions. Each word of `q` is found as the start of a word, whatever
  its case, and characters of Han, Hiragana and Katakana typed in a row
  as those characters in a row; a message matches when it holds every
  one. Tool output, the model's thinking, a subagent's thread, files and
  redacted messages are never searched, and markdown's marks and a
  link's address are not either. Each session found carries its 3
  newest matches, each with its `seq` in the log and an excerpt of at
  most 160 characters with the matched words marked. Pages hold 20
  sessions at most, newest first by id, and take the list's `agent`,
  `status`, `runner` and `archived` filters. The route asks
  `session.list` as the list does and `session.read` of each session it
  finds, leaving out one the caller may not read; an authorizer that
  answers both needs no change. `docs/searching-sessions.md` is the
  contract a client follows.
- A turn on a routed name moves off a model that cannot serve. When the
  gateway answers `upstream_error`, `provider_unavailable` or
  `upstream_timeout`, the turn asks the authorizer at once for another
  model and continues on the one it names, inside the same turn, at most
  3 times a turn. The log records the failed `model.request`, then
  `session.model_changed` by the service with the new `reason`
  `model_busy` ("The model was busy, so another one answered.") and the
  gateway's answer in `detail`, then the request that answered.
- The question is `session.update`, asked as the session's initiator in
  the session's context, with `model` the routed name, `current_model`
  and `current_model_via`, and two new fields: `failed_model`, the model
  that failed, and `failed_detail`, the gateway's developer detail of the
  failure, at most 1024 bytes, such as the upstream's own status. A
  runner process asks it over the internal listener at
  `POST /internal/v1/leases/{session}/failover`.
- The model client keeps a gateway's developer detail
  (`Lux-Error-Detail`), and a failed `model.request`'s `error` and the
  turn's `session.error` detail show it, so a provider's rate limit is
  told from its outage.

### Changed

- A gateway's answer that the model cannot serve is no longer retried
  for about a minute. A turn that can move asks for another model with no
  retry; one that cannot, on a model named itself or out of moves, retries
  once a second later. Every other failure keeps its retries.
- Such a turn ends with `session.error` `model_busy`, retryable, with the
  sentence "The model is busy right now. Send your message again in a
  moment." in `message`, where it ended with `model_error` and the
  gateway's words.

### Upgrading

- The release carries a Postgres migration, which adds a column and two
  indexes to `events`. A server indexes the messages written before it
  in the background after it starts, and again every ten minutes for
  those a replica of an earlier release writes during the rollout, so a
  search finds an older session a short while after the first replica
  of this release starts. The directory store needs no migration: a
  search reads each session's log.
- Roll the authorizer that reads `failed_model` before this release. An
  authorizer that does not read it keeps the session on its model, and
  such a turn ends at once with `model_busy`.

## v0.18.0 - 2026-10-05

### Added

- An authorizer's allow may set the reasoning level a session runs at,
  as `limits.reasoning`, read where `limits.model` is: at a session's
  create, at a change of its model or level, and at each send. Absent
  keeps the session's level, one of `minimal`, `low`, `medium` and
  `high` sets it for the next turn, and `""` returns the session to its
  agent's own. A send whose allow names another level appends
  `session.model_changed` by the service and keeps the model; a session
  already at the level appends nothing. A fork starts at the level its
  parent stood on. A level outside the four refuses the request as
  `authorizer_unavailable`.
- A refusal of the authorizer's carries the deny's limits beside its
  reason, as `details.limits`, so a client can say when a request
  refused for a bound that resets can be made again (`resets_at`). A
  refusal whose reason is withheld carries no limits either.

### Changed

- The API names a model's reasoning level `reasoning`, where it said
  `effort`: an agent's `spec.model.reasoning`, its advisor's and each
  inline subagent's, a session's `model.reasoning`, the `PATCH
  /v1/sessions/{id}` body's `model.reasoning`, and `old` and `new` of
  `session.model_changed`, in the events list and the stream, events
  stored before this release included. Every answer names `reasoning`
  alone. A manifest and a `PATCH` body may still name `effort`, which is
  read on input through every v0.x release and dropped in v1.0; both
  names with two levels are refused as `invalid_manifest` or
  `invalid_request`.
- What is stored keeps `effort`: an agent version keeps its digest and
  its version, and an agent applied again unchanged keeps its version
  under either name. An agent read answers a spec that is not byte for
  byte the spec its digest covers. A server rolled back to an earlier
  release reads every agent, header and event this one stored, its
  levels included.
- The `session.update` question carries a change's level under both
  `effort` and `reasoning`, so an authorizer that reads either name
  decides it.

### Upgrading

- Move a client that reads a session's, an agent's or an event's level
  to `reasoning` before rolling this release: from it, no answer names
  `effort`. A client that writes `effort` keeps working through v0.x.
- Roll an authorizer that answers `limits.reasoning` before or after
  this release: an earlier server ignores the member, and this one keeps
  every level when the member is absent.

## v0.17.0 - 2026-10-05

### Added

- An agent can put a folder of its sandbox online with the `publish`
  tool. The first call creates the session's own app at the
  installation's app host, named after the session; the folder is
  pushed from the sandbox to the session's branch of the app's
  repository, and the call waits for the preview the host builds and
  answers with its address, or with the failed build's code and the
  end of its log. `publish` with `release: true` releases the newest
  preview that is ready or still building to the app's address with
  the next `v` tag. Each call
  asks the person in `confirm` and `progressive`, the runner reaches
  the host with the session's own token, which never enters the
  sandbox, and every result carries `meta.publish` for a client to show
  the preview and the release. An agent holds the tool only when its
  manifest names it; a client tool may no longer be named `publish`,
  and `topos run` refuses an agent that names it. See
  `docs/publishing.md`.
- `TOPOS_APPS_URL` and `TOPOS_APPS_AUDIENCE` connect the app host and
  name the audience of the session's token there. The URL needs
  `TOPOS_ORIGO_URL`.
- Every request of a session that runs by a routed name carries it to
  the model as `Model route: <name>`, after the other system parts, so
  an agent can say when another route would serve the person better.

### Changed

- A hosted session's sandbox starts when the model's response begins a
  call of a tool that acts on a machine, while the call's arguments
  stream, instead of as each turn begins. A turn that only talks creates
  and starts no sandbox, so a conversation that never uses a tool holds
  none of the sandboxes Cella allows its owner. The session still
  records its machine at the first tool that uses it. `machine.Starter`
  and `machine.Start` begin the open of a machine opened on demand.
- A session that already has a sandbox no longer starts it as a drive
  begins: it starts at the first call of a tool that acts on it, as a
  new session's does, and the turn's requests carry the machine context
  the session recorded. A turn that only talks leaves a stopped sandbox
  stopped, and a start Cella refuses is the result of the call that
  needed it, carrying Cella's sentence to the model, rather than a
  failed turn. A turn that opens no machine takes no checkpoint, so a
  rewind to it answers `checkpoint_missing`.

## v0.16.0 - 2026-10-05

### Added

- An agent can search the web with the `web_search` tool: each call
  sends a query to the search service the installation configures with
  `TOPOS_SEARCH_URL` and returns results the agent can cite, a title, a
  URL and a snippet each, which it reads in full with `web_fetch`. An
  agent holds the tool only when its manifest names it in `spec.tools`;
  an agent that names no tools holds the eight built-ins as before, and
  its version and digest do not change. A client tool may no longer be
  named `web_search`: a manifest that declares one is refused as
  `invalid_manifest` at its next apply.
- Topos names no search provider: the contract a service answers is in
  `docs/web-search.md`. On an installation with session keys each search
  carries the session's own model key, so the service can tell which
  session searched; without them it carries `TOPOS_SEARCH_KEY`. A
  service's refusal reaches the agent as its own sentence for the
  person, and its code is the result's `refusal` meta.
- A `tool.result` records `cost_usd_micro` when a service charged for
  the call, and the session's spend, its budget check before each model
  request and the task suite's run cost count it beside model requests.
  A redacted result keeps its cost, so no redaction lowers the spend.

## v0.15.0 - 2026-10-05

### Added

- `GET /v1/sessions/{id}/files?path=<path>` answers one file of a
  session's working directory as it is now, such as a page the agent
  wrote, to anyone who may read the session: the authorizer is asked
  `session.read`, with no new field. The path is absolute, as a write or
  edit result names it, or relative to the working directory. The answer
  is a download of at most 10 MiB with its media type, its length, an
  attachment disposition with its name, `nosniff`, the policy
  `sandbox; default-src 'none'` and `no-store`; `HEAD` answers the
  headers alone. The file is read from the session's machine while it
  runs, and the read never starts or creates one: a sandbox that is
  stopped, never started or gone is `file_unavailable` (409), a larger
  file `file_too_large` (413), and a path outside the working directory,
  a directory or a credential path `invalid_request`. A Cella sandbox is
  read with the session's own Cella token where the installation mints
  one, and with `TOPOS_CELLA_TOKEN_FILE`'s bearer otherwise; a host
  session's directory is read on the server's disk.
- A `write` or `edit` result's `meta` carries `size`, the length in
  bytes of the file it left, beside `path` and `sha256`.

### Changed

- A hosted session's sandbox starts as a turn of an agent whose tools
  act on a machine begins, beside the turn's first model call, instead
  of at the first tool call, so the sandbox's start and the model's
  first answer overlap. The session still records its machine at the
  first tool that uses it. A turn that only talks starts the sandbox
  too and records none; Cella stops it after its idle time, and the
  drive does not wait for it. `machine.Deferred` gains `Start`.
- A server started in the foreground no longer holds the turn until the
  command's timeout. A `bash` command still running after 3 seconds
  whose process group listens on a TCP port it chose is moved to the
  background: the call returns at once with the output so far, the
  port, the job's pid and its log, and the server keeps running until
  the model stops it or the session ends. A listener on a port in the
  system's ephemeral range, as a test's server bound to port 0, does
  not count, and every other long command keeps its timeout. This
  works on Linux, in Cella sandboxes, and on macOS; a sandboxed host
  and Windows keep the timeout alone. `machine.ExecRequest` gains
  `ServerGrace`, and `machine.ExecResult` gains `Moved`, `Ports` and
  `ServerErr`.

## v0.14.0 - 2026-10-05

### Added

- `PATCH /v1/sessions/{id}` takes `{"policy": {"mode": "plan" | "confirm" |
  "progressive"}}`, alone or beside `model`, so a person can change how a
  session asks after it started. The authorizer is asked `session.update`
  with `approval_mode`, `current_approval_mode` and `agent_approval_mode`;
  an allowed change appends the new event `session.policy_changed`
  `{by, old: {mode}, new: {mode}}`, in one batch with a model change, and
  the Session's `policy.mode` reads the new mode. The mode holds from the
  next step of the turn that runs. A call already waiting for a
  confirmation keeps waiting, whichever way the mode moved. A thread
  never runs looser than the modes its own agents name, a host with no
  operating-system sandbox runs a switch to `progressive` as `confirm`,
  and a fork starts in its agent's mode. The agent's
  `spec.approvals.mode` is the mode a session starts in. It does not
  limit the modes a person can switch to: the lists, the thresholds and
  the hard boundaries apply in every mode.

### Changed

- `DELETE /v1/sessions/{id}` asks `session.read` before `session.delete`,
  and refuses a running session as `conflict` before it asks
  `session.delete`. Before, an authorizer that acts on an allowed delete,
  as one that revokes the session's credentials does, acted for a delete
  the store then refused, and the running session was left cut off.
  Interrupt a running session, then delete it once it is idle. A delete
  removes the session's rows at once and its outside blob bodies at
  once or within the reaper's next pass. It leaves the hosted session's
  sandbox, its Secrets and a checkpoint ref at the git host to the
  installation, which the API reference now says.

### Upgrading

- Roll this release after the installation's authorizer answers
  `approval_mode` on `session.update`. An authorizer that requires a model
  or an effort refuses a change of the mode alone, which is safe, and one
  that reads only the model would allow a mode changed beside a model on
  the model's rules alone.

## v0.13.0 - 2026-10-04

### Added

- An agent can put a decision to the person it works for with the
  `question` tool. One call asks one to four questions, each with a
  short header and two to four options; an option has a label, a
  description, an optional preview and may be marked recommended, and
  the person can always answer in their own words. An agent holds the
  tool only when its manifest names it in `spec.tools`; an agent that
  names no tools holds the eight built-ins as before, so its version
  and digest do not change. A client tool may no longer be named
  `question`: a manifest that declares one is refused as
  `invalid_manifest` at its next apply.
- A session created with `"attended": true` on `POST /v1/sessions`, or
  forked with it on `POST /v1/sessions/{id}/fork`, waits for the
  person's answer: it goes idle with the new stop reason `question` and
  holds no runner while it waits. A session created without it, which
  is every session a client created before, answers a question at once
  with the outcome `unanswered`, and the agent decides and says what it
  assumed. `topos run --attended` does the same on your machine, stops
  with exit code 3 on a question, and prints it with its options.
- `POST /v1/sessions/{id}/events` takes `user.answer`,
  `{"tool_use_id", "answers": [{"selected", "text"}]}`, one entry per
  question. The server checks it against the open question:
  `invalid_request` for an answer that does not fit, `conflict`, with
  what closed the question in the detail, for one that names no open
  question. The authorizer is asked `session.send` with `event_type`
  `user.answer`; an authorizer that lists the event types it allows
  must accept it before a client sets `attended`. A person's message
  in place of an answer closes the question, and an interrupt dismisses
  it.
- A question's `tool.result` carries `meta.closed_by` (`answer`,
  `message`, `interrupt` or `unattended`) and `meta.event_id`, and the
  new outcome `unanswered`, which is not an error.
- `user.answer` is redactable, and redacting it also redacts the result
  rendered from it.
- `docs/questions.md` is the contract a client follows to show a
  question and send its answer; `api/openapi.yaml` gains the schemas
  `QuestionInput`, `UserAnswer` and `QuestionResultMeta`.
- The compaction prompt asks the summary to list each question put to
  the person with its answer, so a long session does not ask again. A
  summary records the prompt it was asked with in `context.compacted`'s
  `prompt`.
- A tool's input schema may use `minItems` and `maxItems`.

### Changed

- A `user.tool_confirmation` or a `user.tool_result` is appended only
  after the log it was checked against. Two sent at once for the same
  call no longer both land: one is appended and the other is
  `conflict`.
- A trigger's `continue` firing to a session that waits on a question
  is held, as it is for a confirmation.
- A person's `user.message` sent while a call waits for a confirmation
  denies the call, as the session log's contract states. The call's
  `tool.result` has outcome `denied` and carries the message's text as
  the person's note, the model reads the denial and then the message,
  and the turn goes on. Before, the session went back to idle
  `tool_confirmation` and nothing read the message. The same holds for a
  call a subagent's thread waits on. A `user.tool_confirmation` sent
  after such a message is `conflict`. A trigger's message denies
  nothing.
- When a confirmation, a question and a client tool's result wait at
  once, the session's stop reason names the first of them in that
  order. A client finds the open calls from the log, not from the stop
  reason.

### Fixed

- A tool call that a person allowed was lost when one step left two
  calls waiting and they were answered one at a time. The first call
  never ran, and after the second answer its `tool.result` came back
  with outcome `unknown_effect`, telling the model to inspect a command
  that had not started. Each call now runs in the turn that reads its
  confirmation, while the other still waits. The same loss hit a
  confirmed call beside a subagent's thread that still waited for a
  confirmation of its own.
- Redacting a `tool.result` made its call look unanswered. The next
  turn appended a second `tool.result` for the same call with outcome
  `unknown_effect`. A redacted `tool.result` or `user.tool_result` now
  keeps its `tool_use_id` beside the tombstone mark,
  `{"tombstone":true,"tool_use_id":"..."}`, and still answers its call.
  The id is the model's name for the call and holds nothing of the
  result. A result redacted before this release keeps a tombstone
  without the id and is read as before.

## v0.12.0 - 2026-10-04

### Added

- An installation's authorizer can name the model a session runs. An
  allow may carry `limits.model`, the name of the model to run in place
  of the one that was asked. A session starts on it at its create,
  changes to it at a `PATCH /v1/sessions/{id}` that names a model, and
  changes to it before its next turn at a send. An agent can
  therefore name a choice, such as `tier/quick`, that no gateway lists
  and the authorizer resolves for each person. An allow without
  `limits.model` leaves every route as it was. `authorizer.Limits` and
  `authorizer.WireLimits` gain the member `Model`.
- A session's `model` has `via`: the name that was asked, when the
  authorizer answered another. It is absent when the session runs the
  name that was asked. `session.model_changed` carries `via` in `old`
  and `new`. A client that offers a choice of models shows `via`.
  `model.request` records the model that ran, as before.
- A model change that a send's allow made is recorded as
  `session.model_changed` with `by` set to
  `{"subject": "service:authorizer", "kind": "service"}`, straight
  before the event that was sent. A change that a `PATCH` made is the
  person's, as before.
- The questions tell an authorizer what it routes from. `session.create`
  carries `model`, the agent's name for its model. `session.update`
  carries `current_model`, the model the session runs, and
  `current_model_via`, beside `model`, which is still the name the
  change asks. `session.send` carries `model`, the model the session
  runs, `model_via`, and `idle_seconds`: the whole seconds since the
  session's last model request ended, absent before its first. Topos
  itself knows nothing about how long a provider keeps a prompt cache.
- A fork starts on the model that the forked session ran at the fork
  point, with its `via`, a model that the authorizer named at the
  session's create included. `session.fork` carries it as `model` and
  `model_via`.

### Changed

- `POST /v1/sessions` and `PATCH /v1/sessions/{id}` check that the
  installation runs the model after they ask the authorizer. They
  checked before. The model checked is the one the allow names, or the
  one asked when it names none, so a name that only the authorizer
  resolves is not refused unread. A request that the authorizer denies
  for a model the installation does not run now answers `forbidden`; it
  answered `model_unknown`. An authorizer is now asked about a model
  name that the installation may not run.
- An allow of `session.update` or `session.send` whose `limits` do not
  decode refuses the request as `authorizer_unavailable`. Their limits
  were not read before.
- A fork checks that the installation runs the model the fork starts
  on. It checked the agent's model, whatever the forked session had
  changed to.
- A send reads the session's agent version and the last events of its
  log before it asks the authorizer. The question of `session.send`
  changes as `idle_seconds` changes, so a cached allow of it is seldom
  reused.

- The text for a decision service is shorter and more precise. This
  includes the reason that a person sees for each verdict, the help of
  `--mode`, and two errors. The errors are for an unknown mode and for a
  missing `TOPOS_DECISIONS_TOKEN`. The configuration page describes
  `TOPOS_DECISIONS_URL` and `TOPOS_DECISIONS_TOKEN` in the same words.

## v0.11.1 - 2026-10-02

### Changed

- In the API document, an operation's `summary` is the name of its
  action in at most four words, such as `Apply an agent` or
  `List sessions`, so a reference can list operations by it. What a
  summary said is in the operation's `description`, which every
  operation now has. No route, parameter or answer changed.
- The API document shows a wire example for every operation: the request
  body where a route takes one, an apply's manifest as JSON and as YAML,
  and each success answer, as JSON, as the frames of the session
  stream, or as a binary schema for a blob. Each example is what the
  route answers to the request beside it.
- The API document lists both answers of an apply: `PUT
  /v1/agents/{name}` and `PUT /v1/triggers/{name}` answer 201 when they
  create the object and 200 when they change an existing one or leave it
  as it was, and the document listed 200 alone. It lists no request body
  for `POST /v1/sessions/{id}/archive` and `/unarchive`, whose body is
  empty. The routes answer as they did.

### Fixed

- `POST /v1/sessions/{id}/end` of a running or ended session answers
  `conflict` before it asks the authorizer `session.end`. It asked first,
  and an authorizer that revokes a session's credentials when it allows
  an end, as the platform's does, revoked them for an end the route then
  refused, so the running turn failed. The route now asks `session.read`
  first, so a caller who may not read the session still hears
  `not_found`. `POST /v1/sessions/{id}/archive` likewise refuses a
  session that has not ended before it asks `session.update`.

## v0.11.0 - 2026-10-02

### Added

- A tool call is decided by a `Decider`; without one configured, the
  rules of spec 012 decide as before. Every `agent.tool_use` now records
  `review_probability`, the probability, fixed before the call ran, that
  a person sees it.
- `topos run` can consult a decision service (`TOPOS_DECISIONS_URL`,
  `TOPOS_DECISIONS_TOKEN`; spec 037). In `progressive` mode its
  suggestion decides the calls the rules leave open, with a share of
  automatic verdicts drawn for review; in `confirm` mode the rules decide
  and the service learns. Every decision and every confirmation is sent
  to it; a service that fails or does not answer in two seconds makes the
  call wait for a person.
- `topos run --mode` takes `manual` and `auto`, the names other harnesses
  use, for `confirm` and `progressive`. Sessions, manifests and events
  keep the spec's names.

### Fixed

- A verdict outside allow, flag, ask and block now composes as block. The
  harness's verdicts are the shared vocabulary of `latere.ai/x/pkg/verdict`
  (v0.91.0), and `harness.Stricter` is its `Least`: ranked by its index in
  a list, an unknown verdict was the most permissive, so composing one
  with allow allowed. No caller in this release composed verdicts, so no
  call was affected.

## v0.10.1 - 2026-10-02

### Fixed

- A session that ended with its turn, as one created with `end_on_idle`
  does (a run-once session from the API, or a trigger's session), can be
  continued. `POST /v1/sessions/{id}/fork` forks it at the end of that
  turn instead of answering `invalid_fork_point`, and the new session
  waits for its next message (`idle`, `end_turn`) with the whole
  conversation and that turn's files. A session ended `failed`,
  `canceled` or `expired` while its turn was still running forks at the
  last turn it finished, or is `invalid_fork_point` when it finished
  none.
- A session route given an id that is not in a session's form, such as
  `POST /v1/sessions/ses_doesnotexist/end`, answers 404 `not_found`, as
  an id no session has does, instead of 500 `internal`. Every route that
  names a session answered 500 for such an id on the Postgres and the
  directory stores.

## v0.10.0 - 2026-10-02

- toposd builds with OpenTelemetry Go v1.46.0 and its log modules
  v0.22.0, past GO-2026-6615 (the log batch processor could spin when
  its export buffer was full) and GO-2026-6505 (exporter configuration
  logging could carry endpoint URLs).
- toposd builds against `latere.ai/x/pkg` v0.90.2, whose telemetry
  resource uses semantic conventions v1.43.0, the schema of that SDK.
  With the earlier schema, resource detection fails at start and every
  OTLP export (traces, metrics, logs) is disabled.
- An agent can belong to an organization. An agent applied with a token
  that names an organization in its `org_id` claim is the
  organization's, held under the organization's subject, and its name is
  unique within the organization: every member's apply of the name
  changes the one agent, and every name the API reads is read among the
  agents of the caller's context. A list of agents holds the context's,
  and a list of sessions, and the sessions' summary, the sessions of the
  context's agents, narrowed further by the authorizer. Every question
  names an organization's agent as `{type: "organization", id}`,
  `agent.create` names the organization in its context, and
  `session.list` names the context it lists as `agent_owner`. The
  agent's identity is created for its owner, and an allow naming another
  refuses the apply. Without an authorizer, an organization's agent is
  the admins' alone, and an admin lists their own context's objects.
  Every agent stored before is its person's; a Postgres database
  migrates with each row kept.
- A hosted session that works in a private repository on the git host
  of `TOPOS_ORIGO_URL` keeps each turn's checkpoint at that repository,
  under `refs/topos/checkpoints/<session>/latest`, pushed from its
  sandbox with the session's git credential and the push option
  `origo.event=off`, so a fork of the session restores the files it
  left, committed or not, after its sandbox is gone. A turn's checkpoint
  names that repository as `remote` once it is there. Before each push
  the runner reads the repository with no credential: only a refusal
  for want of one counts as private. A public repository, and one whose
  answer says anything else, never gets a checkpoint, since whoever
  reads a repository reads these refs; its fork starts from its
  repositories. The refs stay until the repository or the ref is
  deleted, deleting the session leaves them, and a repository made
  public later exposes the ones already there.
- A fork whose fork point's files the runner cannot restore starts from
  its repositories and records a `session.error` `checkpoint_missing`
  beside its first machine, naming the checkpoint and why; the call that
  opened the machine runs as it would have. Before, a checkpoint the
  runner did not find was recorded nowhere, and one it found and could
  not check out failed that call.
- A fork's first `bash` call starts in the fork's own working directory.
  It started in the directory its parent's last call ended in, which
  belonged to the parent's machine.

## v0.9.7 - 2026-10-01

- Ships the changes listed under v0.9.6, which was tagged but never
  released: its release pipeline refused the tag because the tagged
  commit never ran verify on a push. No image or binary of v0.9.6 exists.
- `GET /v1/sessions/summary` counts the sessions `GET /v1/sessions`
  would list for the caller, in one read: how many are running, waiting
  for approval (idle on `tool_confirmation`), otherwise idle, and ended,
  and how many distinct agents they belong to. It takes the list's
  `agent`, `runner` and `archived` filters and is asked of the
  authorizer as `session.list`, so it counts exactly what the caller
  may list. The Postgres store counts in one query; a session store
  that implements `session.Summarizer` counts itself, and one that does
  not is counted through its list.

## v0.9.6 - 2026-10-01

Tagged but never released; v0.9.7 ships these changes.

- `POST /v1/sessions/{id}/fork` continues a session, an ended or
  expired one included, as a new session of the same agent version: its
  log starts as a copy of the old one's up to a turn boundary, the last
  one unless `at_seq` names another, so its first turn has the whole
  conversation, and it has a lifetime, a budget and credentials of its
  own, with `parent` naming where it came from. Its title is the old
  session's marked as its continuation, "Notes (continued)" and then
  "Notes (continued 2)", so the two read apart in a list. It is asked of the
  authorizer as `session.fork` with a create's fields and the forked
  session's. The fork point's files are restored into the new session's
  working directory when the runner can reach that turn's checkpoint;
  a hosted session on Cella does not keep its checkpoints past its
  sandbox yet, so its fork starts from its repositories.
- `POST /v1/sessions/{id}/archive` and `/unarchive` file an ended
  session away from the lists and back, asked of the authorizer as
  `session.update` with `archived`. `GET /v1/sessions` leaves archived
  sessions out unless `archived=true` or `archived=any`; an archived
  session stays readable, streamable and forkable by id. The Postgres
  store adds the `archived_at` column.
- A tool call whose arguments are not valid JSON no longer ends the turn
  with `model_error`. A model can break off inside a string argument at
  its output limit, and a provider can report that stop as a tool call;
  the step is now logged as it came, with the call's input recorded as
  `{}` and the raw response kept, and the call is answered
  `invalid_input` with what was wrong and the first 200 characters of
  the arguments, so the model sends the call again on its next step.
- A tool call whose arguments arrive after its block closed, as
  interleaved parallel calls can over Chat Completions, gets its whole
  arguments instead of failing the stream, and a call the stream never
  closed is closed when the message ends with the arguments it
  received, where it ran with an empty input.
- A model request that fails part way through its stream keeps the
  bytes it received as the `response_blob` of its `model.request`, so a
  failed step can be read back as the model sent it.
- A response that stops at its output limit inside a tool call's
  arguments is no longer sent again at the model's full output limit or
  continued: a model that runs away inside an argument only runs further
  with more room. The step keeps the response, runs the calls before the
  cut one, and answers the cut call `invalid_input` with the limit it
  reached and the start of its arguments, asking for shorter arguments,
  and the next step asks the usual cap. A response cut in text or
  thinking is still sent again at the output limit and then continued.
- Over Chat Completions, a response a provider reports as `tool_calls`
  while its native finish reason names the output limit, as OpenRouter
  does, is read as stopped at the output limit, and parallel tool calls
  whose arguments interleave are decoded one call after another
  (`latere.ai/x/pkg` v0.90.1).
- `GET /v1/sessions/{id}/stream?deltas=1` carries a session's live
  output while a response arrives, so a client shows thinking and text
  as the model writes them instead of waiting for the whole
  `agent.message`. Each is a frame of `event: delta` with no `id`, whose
  data is `{thread, turn, step, block, kind, text}`: `kind` is `text`,
  `thinking` or `tool_input`, and `text` is the next run of the content
  block `block`, to append to what the step's earlier deltas carried for
  it, at most 1024 bytes. `{thread, turn, step, reset: true}` says the
  step's request is sent again and what its deltas carried is
  discarded. `thread` is absent for the session's own thread, as on
  events. A frame without an `id` leaves a browser's last event id as it
  was, so a reconnect resumes the log where it was.
- Deltas are best effort. They reach a stream on any replica, whichever
  replica or runner process drives the session, joined per content
  block every 50 ms. They are never appended and never replayed, and a
  stream that falls behind loses deltas rather than slowing the turn.
  The step's `agent.message` stays the record: show a step's deltas
  until its `agent.message` arrives, and ignore a delta of a step whose
  message is already there.
- On Postgres, deltas cross replicas as notifications on the channel
  `topos_deltas`, which each replica's existing listener connection also
  listens on; a replica sends them from its serving pool one statement
  at a time, and opens no connection for them.
- The OpenAPI document describes the stream's frames, its `from_seq`
  and `deltas` parameters, and the `Delta` schema.
- `PATCH /v1/sessions/{id}` changes a session's reasoning effort as
  well as its model: `{"model": {"effort": "high"}}` keeps the model,
  `{"model": {"name": "...", "effort": "low"}}` changes both, and
  `"effort": ""` returns to the agent's own `spec.model.effort`. The
  effort is `minimal`, `low`, `medium` or `high`, holds across a change
  of the model, and takes effect at the next turn, in which a thread
  whose agent names no effort runs at it too. `session.model_changed`
  records it in `old` and `new`, each now `{name, effort}`, and the
  session's `model` carries the effort the next turn runs at.
- The authorizer's `session.update` question carries what the change
  names: `model` when the body names a model, and `effort`, the effort
  the next turn runs at, when it names an effort. An effort change alone
  carries no `model`, so an authorizer that requires one refuses it
  until it decides effort changes.

## v0.9.5 - 2026-09-30

- A trigger's `trigger.create` question carries the `trg_` id the
  trigger is stored under as its resource id, where it carried none, so
  an authorizer that delivers events to triggers knows a new trigger by
  the id `POST /v1/triggers/{ref}/fire` takes from its first apply.

## v0.9.4 - 2026-09-30

- Triggers start and continue an agent's sessions with no person
  present. `PUT /v1/triggers/{name}` applies a Trigger manifest that
  fires on `spec.schedule`, a five-field cron expression or `@hourly`,
  `@daily`, `@weekly` read in `spec.timeZone` (a local time a change
  to daylight saving time skips fires at the next valid minute, and
  one it repeats fires once), or on the events delivered to
  `POST /v1/triggers/{ref}/fire` that `spec.on` selects by product,
  verbs, resources and payload values. An event is one envelope
  `{id, product, verb, resource, actor, subject, time, payload}`; the
  core verifies no provider's signature and polls nothing, so whatever
  produces an installation's events calls the route.
- A trigger's `session.message`, `title` and `key`, and a repository's
  `url` and `ref`, are templates of `{{event.*}}`, `{{trigger.id}}`,
  `{{trigger.name}}`, `{{firing.id}}` and `{{firing.time}}`: path
  lookup and nothing else. Each value is cut at 16 KiB and marked
  `[cut]`, and a message past 64 KiB refuses its firing. An unknown
  root, a `{{` that opens no placeholder, and an event path in a
  schedule trigger are refused at apply as `invalid_manifest` with the
  field's path.
- `session.policy: new`, the default, starts a session per firing and,
  under `skipIfActive`, skips a firing while its key's session is
  active; `continue` sends each firing to the open session its key
  names, and holds it while that session waits on a confirmation or
  on its budget, sending it once the session runs on. `maxActive` (5
  by default, at most 100) bounds the trigger's active sessions, and
  `maxAge` skips a firing more than that late. A redelivered event
  answers its first firing and starts nothing; a failed one answers
  503 and runs again when it is delivered again. `GET
  /v1/triggers/{ref}/firings` lists the firings newest first, and a
  trigger's `status` carries `lastFiredAt`, `lastSessionId`,
  `nextFireAt` and one count per outcome.
- A firing's session is created by the code of `POST /v1/sessions`,
  with the trigger's owner, the person who applied it, as its
  initiator and `trigger:<trg_id>` as the sender of its messages, each
  of which names the firing as `firing_id`. Its `session.create`, and
  a continued message's `session.send`, are asked of the authorizer as
  the owner in the context the owner's token named at apply
  (`org_id`), with `trigger_id` and `firing_id` in the create; the
  fire route asks the new action `trigger.fire` of its caller. A
  trigger's firings act one at a time across replicas, so two
  deliveries of one key never start two sessions.
- `toposd serve` runs the minute loop that fires the schedules due and
  sends the held firings. On Postgres the store's migration 0004 adds
  the `triggers`, `trigger_firings` and `trigger_sessions` tables; the
  directory store keeps triggers under `objects/trigger*`.

## v0.9.3 - 2026-09-30

- An agent knows its session's repositories from its first turn. A
  hosted session's sandbox opens only when a tool first acts on it, and
  its repositories were named to the model only then, so an agent asked
  what it works on answered that it had none. Until the session's first
  machine is recorded, every request's context block now names each
  repository, the ref it starts from, the session's branch and the
  directory it is cloned into; the machine's own context block replaces
  it once the sandbox opens, and the sandbox still opens only when a
  tool needs it.
- A session's model can change between turns. `PATCH /v1/sessions/{id}`
  with `{"model": {"name": "..."}}` sets the model the session's next
  turn runs. The name is checked as a session's create checks its
  agent's model (below), so a switch takes every model a session could
  have been created with, then asked of the authorizer as the new
  action `session.update` with `session_id` and `model`, whose deny is
  `forbidden` with its reason. An allowed switch appends
  `session.model_changed` `{by, old, new}` and the Session carries
  `model`; a turn already running keeps its model, and the next turn
  runs on the new one with its own window, output limit and prices. The
  agent's own model's name switches back to the agent's model as it
  names it.
- A session's create checks that the installation runs its agent's
  model, by the rule the runner connects it with: `model_unknown` (422)
  for a model no source gives an input window and an output limit, and
  `model_unavailable` (503) for a gateway that does not answer, where
  such a session was created and failed its first turn. On an
  installation without `TOPOS_MODELS_KEY`, whose runners read Lux's
  doors with each session's own key, a model that goes through a Lux
  door passes and its figures are read at the turn. `toposd serve`
  reads the Lux root's doors at start whether or not it runs sessions
  itself.
- A person's message can carry images and files. A `user.message`'s
  `content` takes inline images, PNG, JPEG, GIF or WebP as base64
  `data`, at most 8 of at most 5 MiB each, which reach the model as
  image blocks when its figures say it takes images and as a note that
  it cannot see them otherwise, without failing the turn. Its
  `attachments` take files, `{name, media_type, data}`, at most 8 of at
  most 5 MiB each: the server stores each as a blob of the session and
  records `{name, media_type, size, blob, path}` with the path
  `attachments/<event id>/<name>`, in a directory of the message's own
  so that no two messages write one path, and the runner writes it
  there in the working directory when the machine opens, or before the
  next step when it is open, recorded as `attachments.delivered`; the
  model reads the paths after the message's content, and a repository
  excludes the directory from git. An image or a file past its limit is
  `attachment_too_large` (413). An image by URL, a block that is neither
  text nor an image, and an image whose bytes are not the format its
  media type names, which were stored as sent, are now
  `invalid_request`.
- The embedded model catalog knows which models take images, from
  OpenRouter's model list, the source of every figure of what a model
  is, and holds Gemini's models, on the OpenAI Chat dialect Lux's OpenAI
  door takes, so a message's image reaches the Claude, GPT and Gemini
  models that take images on any gateway, a Lux that lists its Models
  without their modalities included.
- An agent applied without `spec.machine` runs its hosted sessions on a
  Cella sandbox of the default image when the server has
  `TOPOS_CELLA_URL` and host sessions are off. The manifest's default
  machine is the host, which is a local run's, so such a session was
  refused at create with `machine_unavailable`. The same holds for an
  agent whose machine is `kind: host` alone, which its resolved spec
  cannot tell apart, and for agents applied before this release. With
  `TOPOS_HOST_SESSIONS=on`, or without Cella, such an agent keeps the
  host, and an agent that names any other machine field keeps its kind.
- A hosted session that ends in a drive deletes the Cella Secrets its
  sandbox mounted, the `-lux` key and the `-origo` git credential, after
  the sandbox, and stops their renewal first so none is applied again.
  They had outlived every session, since a Cella Secret has no lifetime of
  its own. A delete Cella refuses fails the release and is tried again at
  the next one. A session that ends outside a drive still leaves them;
  each carries `topos.latere.ai/session`, so the installation's host can
  sweep them.

## v0.9.2 - 2026-09-29

- A hosted session's `session.create` question names the session's
  repositories, its own or its agent's, as `repositories: [{type, url,
  ref}]`, so an authorizer that holds the git host's registry can grant the
  session read and write on exactly those, without the agent listing them in
  its permissions.
- A toposd or runner shutting down leaves a running hosted session
  running, with nothing appended, for the next runner to claim and
  resume. On a busy machine the stop could reach the turn before the
  session's log was closed to further writes, so the turn was recorded
  as `idle` with stop reason `interrupted` and every rollout interrupted
  the sessions it was running.
- A hosted session whose model neither the embedded catalog nor the agent
  gives a family, such as a provider's model a Lux installation routes,
  goes through the Lux root's OpenAI door, which serves every model Lux
  routes, and takes its context window and output limit from that door's
  model list. It had no door to ask, so it was refused as `model_unknown`
  though Lux served its figures.
- An agent takes an optional display name, `metadata.displayName`: the
  name a person reads, one line of any text up to 200 characters, while
  `metadata.name` stays the identifier routes and references use. It is
  stored with the agent and returned by the apply, every read by name
  or id, each version and the list; an agent without one reads exactly
  as before. A blank one, a longer one, or one holding a control
  character, a line break or a bidirectional control is refused as
  `invalid_manifest` at `metadata.displayName`. The display name, like
  labels and annotations, is not part of the spec: changing it creates
  no version and moves no digest. Such an apply used to answer the new
  metadata and store none of it; now it replaces the latest version's
  metadata, so the next read returns what was applied.
- A `forbidden` answer says why. It keeps its code and its sentence and
  now carries the installation's authorizer's reason for the deny in
  `details.reason`, such as `agents_not_enabled`,
  `agent_budget_unassigned` or `agent_exceeds_initiator`, so a client
  can tell the person what to do. A deny without a reason, or whose
  reason is not a snake_case token, carries none, and `details.detail`
  reads `the authorizer denied <action>` without it. A session create
  of another subject's agent passes its reason only when the caller may
  read that agent, since the reason may be about it; a denied read still
  answers `not_found` with no details. The authorization wire carries
  only the reason, so which permission exceeded the initiator's is not
  yet part of the answer.

## v0.9.1 - 2026-09-29

- An agent's name is unique per owner. It was unique across the whole
  installation: once one person applied `coding-agent`, nobody else
  could, and the refusal told them that someone's agent of that name
  existed. Now each subject has their own names. An apply creates or
  updates the caller's own agent of the name; `GET`, `versions` and
  `archive` by name, a session create's `agent` and a session list's
  `agent` filter read the name among the caller's own agents, as a
  manifest's references do. A name only another subject holds answers
  `not_found`, or `unknown_reference` in a manifest, exactly as a name
  nobody holds. An `agent_` id still names its agent whoever owns it,
  and the authorizer decides as before; an administrator reaches
  another subject's agent by its id, and an administrator's apply by
  name acts on the administrator's own agent. An organization's agent
  is named within the subject that applied it. On Postgres, migration
  0003 replaces the agents table's `UNIQUE (name)` with
  `UNIQUE (owner, name)`; every stored agent already satisfies it, so
  the migration keeps every row. A toposd of v0.9.0 refuses to start on
  the migrated schema; the down migration restores the old constraint
  while no two owners hold one name.

## v0.9.0 - 2026-09-29

- A session's `budget.spent_cost_usd_micro` reports what it has spent:
  every store adds each `model.request`'s cost to the session as the
  event is appended, and the directory store writes the session when a
  request's cost arrives, not only at a status change. It stayed 0 for
  every session, so the console and the API showed nothing spent, though
  the budget ceiling itself was enforced from the log.
- A model request asks at most 8192 output tokens as its `max_tokens`,
  or the model's output limit when that is lower, rather than the whole
  output limit. A gateway that reserves a request's `max_tokens` at the
  output price against the caller's budget before running it, as Lux
  does, held back many times what a step spends, so an agent on a small
  budget was refused with `budget_exhausted` after its first request. A
  response that stops at 8192 tokens is sent again once at the model's
  output limit, in its place, and one that stops at the output limit is
  continued as before. Every `model.request` records the `max_tokens` its
  request asked; a response that stopped at 8192 tokens leaves a
  `model.request` with outcome `escalated`, whose cost counts, and no
  `agent.message`.
- A session's first repository is fetched into the working directory as
  it is instead of cloned into it, so a sandbox whose workspace volume
  starts with `lost+found` gets its repository; `lost+found` is excluded
  from git, and a fetch that fails leaves the directory as it found it.
  A clone refused the non-empty directory, so no Cella session received
  its repository. Every open of a Cella sandbox also names the workspace
  a safe directory in git's global configuration: the volume's root
  belongs to another user than the sandbox's process, and git refused to
  work in it.
- The Cella Secrets a hosted session's sandbox uses, its `-lux` model key
  and its `-origo` git host token, carry the session's and the agent's
  labels, `topos.latere.ai/session` and `topos.latere.ai/agent`, as the
  sandbox does, at their create and at every renewal. An authorizer that
  binds a session's token to the session refused the first Secret, since
  it named no session, so no sandbox opened; `TOPOS_CELLA_LABELS` still
  joins them for a self-hosted installation.
- A Cella sandbox's `HOME` is the machine's own directory,
  `TOPOS_MACHINE_DIR/home`, made on every open, unless the agent's
  machine environment names one. Cella mounts the image's root filesystem
  read-only, so writing git's global configuration into the image's home
  directory failed and a session's first machine never opened; git,
  package managers and build caches now write there.
- An Agent manifest names the repositories its sessions work in as
  `spec.repositories`, a list of `{url, ref}` with `ref` optional, checked
  as a session's are: at most 8, each an `https` URL that names its host
  and holds no credential, and a ref that is a branch, a tag or a commit.
  A session whose create names no repositories works in those of the
  agent version it pins; one that names its own works in those alone. A
  subagent's repositories are not used, and a local `topos run` refuses an
  agent that names any.
- The `session.machine` of a session's first machine lists the
  repositories delivered into it as `repositories: [{url, branch,
  commit}]`, `commit` the HEAD checked out on the session's branch, so a
  reader of the log sees which checkout the agent started from.
- A refused repository URL that holds a credential is no longer quoted
  in the error.
- An installation without an identity provider gives its sandboxes a git
  credential of its own: `TOPOS_ORIGO_TOKEN_FILE` names the file holding
  it, and each Cella sandbox's git sends it to the host of
  `TOPOS_ORIGO_URL` through the `ORIGO_TOKEN` Secret that Cella's egress
  gateway swaps in, so a session clones and pushes its repositories there
  without the credential entering the sandbox. The file is read at each
  sandbox's open. It needs `TOPOS_ORIGO_URL` and is refused beside
  `TOPOS_IDENTITY_URL`, whose sandboxes push with their agents' tokens.
- `TOPOS_CELLA_LABELS` names `key=value` labels that every sandbox and
  every sandbox Secret carries beside the session's and the agent's, for
  a Cella whose authorizer places what a caller creates by labels, such
  as its tenant.

- **Breaking:** the repository restarts as the Topos core. Every package of
  v0.7.0 is removed: the root `topos` package, `graph`, `billing`, `harness`,
  `models`, `runtime`, `sandbox` and `adversarial`. A program pinned to v0.7.0
  or earlier keeps building, because those versions stay in the Go module
  proxy; the v0.7.0 tree is at its tag, and its specs are under
  `docs/history/`. The adversarial review engine continues as its own module,
  `latere.ai/x/adversarial`.
- `session` is the session log of schema v1: the `Session` header, the
  typed events a session appends, and `Fold`, which renders one thread's
  transcript as Lux wire messages and is byte-identical for the same log.
  `session.NewMemoryStore` keeps sessions in memory, and `session/dir`
  keeps each in a directory with an fsync before every acknowledged append
  and a single-writer lock held with `flock`. `session/storetest` is the
  suite every `session.Store` passes.
- `models` is the model boundary: a `Connection` that must name a base URL
  and a model, the `Model` a request streams through, retry classification
  of HTTP answers, stream errors and cut streams, a catalog of model
  windows and prices built into the binary, and exact cost in micro-USD.
  `models/dialect` sends requests over HTTP in the Anthropic Messages,
  OpenAI Responses and Chat Completions wire formats through the
  llmdialect codecs, and reports a stream the server closed before its
  last frame as incomplete, never as a finished answer.
- `machine` is where a session's tools run: the `Machine` interface, the
  credential deny-list (home credential files, `.env` files, key files,
  and secret-named environment variables), and grep and glob that honor
  `.gitignore`. `machine/host` is the person's own computer: file access
  confined to the session's roots through `os.Root`, each command in its
  own process group, killed on timeout, on cancel and when its shell
  exits, and background jobs that end with the session. A command longer
  than 64 KiB, such as a long heredoc, runs from a script file, since
  Linux refuses a single argument past 128 KiB.
- `harness/tools` holds the built-in tools: `read`, `write`, `edit`,
  `bash`, `grep`, `glob`, `web_fetch` and `todo`. `write` and `edit` refuse
  a file that changed since the thread last read it, `bash` keeps its
  directory between calls and runs servers as background jobs, and output
  past 32 KiB goes to a spill file with its head and tail kept. The
  registry validates every call against the tool's JSON Schema before it
  is scored.
- `harness` runs one agent's turns over a session: no step cap, the
  model's own output limit from the catalog, retries with backoff that
  honor `Retry-After`, thinking replayed with its signature, a risk score
  and a verdict (`allow`, `flag`, `ask`, `block`) on every call under the
  `plan`, `confirm` and `progressive` modes, confirmations that survive a
  restart, and old tool results cleared and the conversation summarized
  when a session nears the model's window.
- `runner` drives a session in process: it holds the session's lock,
  attaches the machine with the context block, the project's `AGENTS.md`
  and `CLAUDE.md` files and its skills index, and starts the next turn at
  once when a message arrived during the last one.
- `models/scripted` plays a YAML script as a model, for tests; a
  connection names it with the `scripted:` scheme.
- `topos run` runs a turn of a local session in the working directory,
  and `topos confirm` answers a pending call and continues it. The exit
  code says how the turn ended: 0 done, 1 error, 2 usage, 3 waiting for a
  person, 4 at a limit, 5 interrupted.
- Every turn ends with a checkpoint: a git commit of the working
  directory's tree under `refs/topos/checkpoints/<session>/<turn>`, written
  through a temporary index so HEAD, branches and the index are never
  touched, with credential files and files over 100 MiB left out. Outside
  a repository the objects go to a session repository under the data
  directory. `topos rewind <session> <turn>` restores the working
  directory to a turn's checkpoint, saving the current state first.
- An agent with subagents gets the `spawn` and `message` tools. A spawned
  thread runs a subagent on the same machine with its own model and
  instructions, the tools it is granted narrowed from its parent's, and
  the stricter permission mode; it answers with its final text and keeps
  taking work through `message`. Several spawns in one step run at once,
  threads nest two deep by default, and a thread waiting for a
  confirmation pauses the session until the answer arrives.
- A thread spawned with `isolation: worktree` works in a git worktree of
  its own on the branch `agents/<agent>/<session>.<thread>`; each of its
  turns is committed there, and the result names the branch and commit
  for the parent to merge.
- An agent configured with an advisor gets the `advisor` tool: a stronger
  model that sees the conversation so far, acts on nothing, and answers
  with advice.
- An interrupt cancels the model request in flight and the running tool
  calls at once, instead of waiting for the next step.
- A redacted message no longer stops a session: the next turn first
  summarizes the conversation up to it from a transcript that leaves the
  redacted content out, so the removed value never reaches the model
  again.
- Sessions can live in Postgres: `TOPOS_DB_URL` names the database the
  migrations and change notifications use, and `TOPOS_DB_POOL_URL`, when
  set, serves queries through a transaction-pooling proxy. An append is
  one transaction, watchers follow `LISTEN` with a poll as the fallback,
  and a session's one-writer lease expires and is taken over when its
  holder stops renewing it. `go test -tags postgres` runs the store's
  tests against `DATABASE_URL` or a Postgres container it starts.
- `authorizer` is the vocabulary an installation's authorizer is written
  against: the 33 actions toposd asks, each with the kind it acts on
  (`agent`, `session`, `trigger`, `credential`, `memory_store`), as the
  `authz.Vocabulary` the shared client, endpoint scaffold and conformance
  suite read, and the `limits` an allow of `session.create` may carry,
  with `DecodeLimits` to read them as toposd does.
- toposd verifies bearer tokens from the issuers `TOPOS_OIDC_ISSUERS` lists,
  for an audience of `TOPOS_OIDC_AUDIENCE` (`topos` by default), and
  refuses a token older than a day or without `iat`. An issuer whose key
  set cannot be read at start stops the start, and an `http://` issuer is
  refused unless it is loopback or named in `TOPOS_OIDC_INSECURE_ISSUERS`.
  `TOPOS_PUBLIC_URL` is now required to serve.
- With `TOPOS_AUTHORIZER_URL` unset, toposd decides with the owner policy:
  the subjects of `TOPOS_ADMIN_SUBJECTS` act on every object, the creator
  of an object on it, and everyone else is denied, narrowed further by a
  key's grants. `TOPOS_AUTHORIZER_URL` without `TOPOS_AUTHORIZER_TOKEN` is
  a configuration error.
- `toposd token` signs a token with the local issuer's key,
  `TOPOS_LOCAL_ISSUER_KEY` (a PEM PKCS#8 ECDSA P-256 or RSA key), and
  prints it: `--subject` (default `admin`), `--ttl` (default `1h`, at most
  `24h`) and `--audience`. It opens no store. With the key set, toposd
  accepts those tokens and serves the key set at
  `/.well-known/jwks.json` under its public URL, so a self-hoster needs
  no identity provider.
- `manifest/v1` holds the four kinds of the `topos.latere.ai/v1` API
  group, `Agent`, `Trigger`, `MemoryStore` and `Connection`, and
  `manifest.Resolve` is the one resolver of them. A manifest is refused
  as a whole with every problem and its field path: an unsupported
  version, unknown fields, wrong types, out-of-range values, a secret
  pasted into a free-text field, and an unknown reference. The resolved
  spec has its fixed defaults written out and a digest that stays the same
  on every build; applying an unchanged spec keeps its version.
- `topos run --agent <file>` runs an agent from a manifest: its model,
  instructions (inline or from `instructionsFile`), tools, approval mode,
  lists and thresholds, egress, subagents, thread limits, budget and
  limits. With no `--agent`, `$XDG_CONFIG_HOME/topos/agent.yaml` (or
  `~/.config/topos/agent.yaml`) runs when it exists. The session keeps the
  resolved agent, so `--session` and `topos confirm` continue with the same
  agent after the file changes. `--model` and `--mode` override the
  manifest. A field a local run cannot apply yet, such as hooks, MCP
  servers, memory stores or a Cella machine, is refused rather than
  ignored, and every refusal exits 2 before a session is created.
- `--mode` no longer defaults to `confirm` at the flag: continuing a
  session without it keeps the mode the session was created with, where it
  was silently reset to `confirm` before.
- `test/tasks` is the task suite: each task is a directory with a prompt,
  its starting files and a checker, run in process through the runner and
  the harness. The tasks cover the failures of v0.7.0 (more than 16 steps,
  an answer past 4096 tokens, a subagent reading its parent's file,
  absolute paths, the person's `PATH`), ordinary coding work, and one
  instruction test per built-in tool. Every task is proven against a
  scripted right and wrong solution; `go test -tags tasks ./test/tasks/`
  runs the suite against the model `TOPOS_MODELS_URL`, `TOPOS_MODELS_KEY`
  and `TOPOS_TASKS_MODEL` name and writes a JSON and Markdown report.
- A subagent's own reasoning effort from its manifest reaches its requests,
  where it took the parent's.
- `prompts` holds every text a model reads as a versioned file: the
  harness prompt, the compaction prompt, the tool descriptions, the tool
  results, the transcript's framing and the context block, each rendered
  from `prompts/<area>/<name>-v<N>.md`. A released file never changes; a
  new wording is a new version.
- `machine/cella` runs a session's tools in a Cella sandbox through
  `latere.ai/x/cella/client`: one sandbox per session named `ses-<ulid>`,
  found by name or created and held until it runs, with the session's
  labels, an egress allowlist that includes the named secrets' hosts, and
  a persistent lifecycle (stops after 15 minutes idle, never deleted until
  the session ends). Commands run through `topos-machine`, a static helper
  the machine uploads into the sandbox, which gives each command its own
  process group, a timeout, an input that ends and its final directory,
  and runs grep and glob where the files are. A sandbox that is gone
  answers `machine_lost`, and Cella's refusals `machine_unavailable`. The
  machine needs a Cella Environment that offers the Attach capability,
  because every command runs over the exec socket; Cella's `local` driver
  does not. `test/stubs/cellastub` is the stub Cella its tests run
  against.
- `toposd serve` answers the API under `/v1`: `PUT /v1/agents/{name}`
  applies an Agent manifest and makes a new version only when the spec
  changed, and agents are listed, read by name or id, read at a version
  and archived. `POST /v1/sessions` creates a hosted session of an
  agent's version with an optional first message, its budget, turn
  timeout and age the lowest of the request's, the agent's and the
  authorizer's; sessions are listed by agent, status and runner, read,
  ended and deleted. `POST /v1/sessions/{id}/events` sends a user event
  with the verified subject as its sender, and
  `GET /v1/sessions/{id}/stream` replays the log as Server-Sent Events and
  follows it live from any replica, resuming after `Last-Event-ID`.
  Every route asks the authorizer its action, a denied read answers
  exactly as a missing object does, every error is one JSON envelope with
  a code, lists page with `cursor` and `next_cursor`, every `POST` takes
  an `Idempotency-Key`, and each subject may make 600 requests a minute.
  The OpenAPI document is `api/openapi.yaml` and is served at
  `/v1/openapi.yaml`. No runner claims a hosted session yet, so its turns
  wait for the runners to come.
- toposd keeps agents, their versions and idempotency records in both
  storage modes. Without `TOPOS_DB_URL` they are JSON files under
  `$TOPOS_DATA_DIR/objects/`, each written atomically, sessions live under
  `$TOPOS_DATA_DIR/sessions/`, and one `toposd serve` holds the directory:
  a second is refused with `data directory in use by pid <n>`. On Postgres
  a new migration adds the `agents`, `agent_versions` and
  `idempotency_keys` tables; replicas that race on one agent version or
  one idempotency key store it once. Readiness checks the store.
- A hosted session runs on a Cella machine: an agent whose
  `machine.kind` is `host` is refused `machine_unavailable` by the server,
  and the owner policy lets a subject start sessions of its own agents
  only.
- A reasoning model served over OpenAI Responses keeps its reasoning
  across turns: the harness asks for the encrypted reasoning items, keeps
  each as an opaque block in the session log, and sends it back verbatim
  on the next request (`latere.ai/x/pkg` v0.88.0).
- `write` and `edit` refuse an existing file the thread never read with
  a result that says so, `<path> exists and this thread has not read it;
  read it before writing to it.`, where they said the file had changed
  since it was last read; the tools' descriptions (version 2) name both
  refusals and ask for a `read` first.
- The model catalog also knows each model by the name the hosted Lux
  serves it under, with dots in its version (`anthropic/claude-haiku-4.5`
  beside `anthropic/claude-haiku-4-5`).
- `toposd serve` runs hosted sessions: up to `TOPOS_RUNNER_CAPACITY` (16)
  of them at once, claimed from its own store as soon as a session gets a
  message. Each runs its agent from the session's bundle, on a Cella
  sandbox created at `TOPOS_CELLA_URL` with the bearer in
  `TOPOS_CELLA_TOKEN_FILE`, against the model at `TOPOS_MODELS_URL` with
  `TOPOS_MODELS_KEY`, which serve now requires. The image carries the
  `topos-machine` helper for `linux/amd64` and `linux/arm64`. A turn that
  cannot start closes with a `session.error` naming why
  (`model_credential_missing`, `machine_unavailable`, ...), and a server
  that stops mid-turn leaves the session for the next runner to resume.
  On Postgres a runner writes through its lease, and a batch from a runner
  whose lease another replica has taken over is refused with
  `lease_lost`, so two runners never interleave in one session.
- `toposd runner` runs hosted sessions as a separate deployment: it claims
  from the internal listener at `TOPOS_INTERNAL_URL` with the first bearer
  of `TOPOS_RUNNER_TOKEN`, keeps each claim's lease by renewing it, and
  writes the log only through it. `toposd serve` mounts the runner routes
  on its internal listener when `TOPOS_RUNNER_TOKEN` is set, frees the
  claims of a runner that stopped renewing, and with
  `TOPOS_RUNNER_CAPACITY=0` runs no runner of its own.
- A model gateway's refusal for spend, `budget_exhausted` or
  `spend_exceeded` (Lux sends it as HTTP 429), is no longer retried like a
  rate limit: the turn stops with `budget` and a `session.error` naming the
  refusal, where it was retried six times and ended as `model_error`.
- A tool confirmation or a client tool's result is accepted only for a
  call waiting for that answer, once; a second answer, or one naming a
  call that never asked, is `conflict`. Only events that carry content
  (messages, tool calls and results, summaries) can be redacted, so a
  redaction can no longer erase a verdict or a confirmation, or reset a
  session's spend by removing its model requests.
- On Postgres, toposd holds one `LISTEN` connection per replica on
  `TOPOS_DB_URL`, however many event streams and runners watch sessions,
  where each stream and each runner's interrupt watch opened its own. A
  replica uses its serving pool plus that one connection, and one more at
  start for migrations. A dropped listener reconnects with backoff while
  watchers poll every 2 s, so no event is missed. A subject's 17th
  concurrent `GET /v1/sessions/{id}/stream` on a replica is refused
  `rate_limited` until one of its streams ends.
- `TOPOS_MODELS_URL` may name a Lux gateway's root, such as
  `https://lux.example/v1/models`: toposd and `topos` read its discovery
  document and send each model to its own family's door, Anthropic models
  to `/anthropic` and OpenAI models to `/openai`. A URL that names a door,
  and a provider's API, are used as before. A server that cannot reach a
  URL naming no door refuses to start.
- `session.create` tells the authorizer the permissions of the agent
  version the session pins, so an installation can refuse a session whose
  agent may do more than the person starting it.
- The directory store takes its single-writer lock on Windows with
  `LockFileEx`, as it does with `flock` on Unix.
- `topos` builds for Windows. On a Windows host each command runs in a Job
  Object of its own, which a timeout, a cancel or the session's end
  terminates with every process the command started; a cancel ends the
  command at once, since Windows has no SIGTERM. Commands run under `sh`
  on `PATH`, or the `sh.exe` of Git for Windows beside `git`, and are
  refused with that remedy when there is neither. A Windows host records
  `sandbox: none`, since it has no host sandbox, and a session there that
  asks for `progressive` is refused with `sandbox_unavailable`; `plan`
  and `confirm` run. With `HOME` unset, `USERPROFILE` places the data and
  configuration directories and the credential deny-list. `toposd` with
  `TOPOS_HOST_SESSIONS=on` refuses to start on Windows.
- The credential deny-list of the file tools and search matches without
  regard to case, so `~/.SSH/ID_RSA`, `SERVER.PEM` or `.ENV` on a
  case-insensitive filesystem, macOS's and Windows' default, is refused
  like the entry it spells; before, such a path read the file. On a
  case-sensitive filesystem a name that differs from an entry only in
  case is now refused too.
- A hosted session records its approval policy at create: the agent's
  `spec.approvals` merged with the lists and thresholds the authorizer's
  allow carries, the confirm lists joined, the allow lists narrowed to
  what both allow, each threshold the lower. Every runner decides the
  session's calls by that policy.
- `topos run` in a git checkout another session still owns writes a
  worktree of its own, under the data directory on
  `agents/<agent>/<session>`, instead of the checkout, so two sessions
  never write one working directory. At a session's end its worktree is
  removed, and its branch kept, when it is clean and merged or pushed.
- `toposd serve` ends every idle session past its maximum age as
  `expired` and deletes every ended session past the retention its
  authorizer gave it, every ten minutes.
- A model reached through a Lux door takes its window, output limit and
  prices from the door's own model list where the list gives them,
  before the built-in catalog's and after the agent's own `spec.model`
  figures, so a model the catalog does not know runs when Lux serves it.
  A door that does not answer its model list fails the turn's setup with
  `model_unavailable`.
- Cella refusing a session's sandbox, or a command in it, because the
  session's allowance is spent (`budget_exhausted` or `spend_exceeded`)
  stops the turn with `budget` and a `session.error` naming the refusal,
  as a model gateway's refusal does, where a refused create was
  `machine_unavailable` and a refused command a failed call the model
  retried. `POST /v1/sessions/{id}/resume` continues the session once
  the allowance is raised.
- Every `model.request` records `fold_seq`, the log's last sequence when
  its request was built, and `models.Replay` builds every request of a
  recorded session again, on its own thread, re-encodes it with the
  current build and compares its hash with the recorded one, reporting
  each step `match`, `mismatch`, `codec_mismatch` or `skipped`.
- `TOPOS_BLOB_URL` keeps raw model responses and captured requests
  outside the database or the data directory: in a directory,
  `file:///<path>`, or in an S3 compatible object store,
  `s3://<host>/<bucket>/<prefix>` with `TOPOS_BLOB_ACCESS_KEY` and
  `TOPOS_BLOB_SECRET_KEY`. A deleted session's objects go after its
  records, and the reaper removes objects a failed delete left behind.
- **Breaking:** an Agent manifest no longer takes `spec.identity`, and one
  that names it is refused with that field's path: the agent's owner, a
  person or an organization, decides whose authority it acts with. An
  agent or a session stored by an earlier build of this release whose
  agent carries the field no longer reads; apply the agent again.
- With `TOPOS_IDENTITY_URL`, every agent applied is an identity at the
  installation's identity provider, owned by the organization the
  authorizer's allow names for it (the limits member `owner`), or else by
  its applier, and holds no key. Its hosted
  sessions reach Cella with tokens minted for the agent, at most 15
  minutes long and naming the session, and a session's sandbox pushes to
  `TOPOS_ORIGO_URL` with its own token, which the sandbox holds only as a
  placeholder that Cella's egress gateway swaps in. Archiving the agent
  archives its identity, which is disabled for good once the agent's last
  session ends.
- With `TOPOS_SESSION_KEYS_URL`, each hosted session asks models with a
  Lux key of its own, and its sandbox holds a second one as `LUX_KEY`
  beside `LUX_URL`; toposd generates both and sends the authorizer only
  their hashes. `toposd runner` reaches a session's tokens and keys from
  its server, only while it holds the session's lease.
- A hosted session's Cella sandbox is created when the agent first calls
  a tool that acts on a machine (`read`, `write`, `edit`, `bash`, `grep`,
  `glob` or `web_fetch`), and its `session.machine` is recorded then, so
  a session that only talks has no sandbox and costs none. A session
  whose sandbox exists reopens it by name when a runner claims it. A
  sandbox that cannot be created answers the call that needed it, with
  a `session.error` naming the code, and a later call tries again.
- `POST /v1/sessions` takes `resources`, up to 8 repositories as
  `{"type":"repository","url","ref"}` with `https` URLs. When the
  session's machine first exists the runner clones the first into the
  working directory and the rest beside it, each on the branch
  `agents/<agent>/<session>` from `ref`, with the agent as the author and
  a hook that adds `Topos-Session` and `Topos-Agent` trailers to every
  commit; `git push` publishes the branch. A repository that cannot be
  cloned is reported as `repository_unavailable` and the session goes on
  without it.
- A `v*` tag publishes the image `ghcr.io/<owner>/topos` for `linux/amd64`
  and `linux/arm64`, under the namespace of the account that pushed the
  tag: `toposd` and the `topos-machine` helper builds on the distroless
  base, the same runtime as the developer image, signed and with its bill
  of materials and provenance attested. The image is pushed by digest,
  tagged with the release's tag only once it reports that tag's version,
  and the release notes follow.
- With `TOPOS_MODELS_URL` a Lux root, the runners reach each door Lux's
  discovery document names under that URL, so an installation that
  points it at Lux's in-cluster address keeps every model request inside
  its network even though Lux publishes its doors under its public URL.
  A sandbox's `LUX_URL`, the host its Lux key is swapped in for, and the
  host its egress admits are that published root, since a sandbox
  reaches only public hosts.
- `TOPOS_BASE_PATH` serves the API under a prefix of an origin toposd
  shares with other services, in the place of `/v1`: with `/v1/agents`
  and `TOPOS_PUBLIC_URL=https://api.example.com/v1/agents`, agents are at
  `/v1/agents/agents`, sessions at `/v1/agents/sessions`, and the document
  at `/v1/agents/openapi.yaml`, whose server is the public URL. The base
  path must equal the public URL's path, and a public URL with a path
  needs one, so a mismatch stops the start instead of writing URLs toposd
  does not answer on. A session's stream `Link`, a list's next-page
  `Link` and the document's server are built from the public URL, never
  from the request's host. A path the API does not route, outside the
  base path as well as under it, answers the `not_found` envelope; the
  probes and the build identity stay at the listener's root.

## v0.7.0 - 2026-09-26

- **Breaking:** `sandbox/cella` speaks to a Cella control plane, the open source
  runtime hosted at `https://api.latere.ai/v1/environments`, through its
  exported client `latere.ai/x/cella/client`. The hosted service at
  `cella.latere.ai` that it spoke to before is retired. Set
  `Options.BaseURL` to the control plane's address including that path.
- `Create` holds the request until the sandbox runs, so the first command can
  follow at once. A sandbox that fails to start is deleted and `Create` returns
  an error naming it and the reason.
- A created sandbox reaches only `api.latere.ai`, where the hosted models are
  served, unless the new `Options.AllowedHosts` names the hosts it may reach.
  The image defaults to `base` from the control plane's catalog. An `ephemeral`
  sandbox is deleted 24 hours after it was created; every sandbox still stops
  after 15 minutes idle.
- `Exec` returns the command's standard output followed by its standard error
  in `Stdout`, each cut at a mebibyte, and the command's standard input is at
  end of file. `StreamExec` delivers the same output as one chunk when the
  command ends.
- **Breaking:** the Cella provider refuses `CreateOptions.Policy`, a non-empty
  `CreateOptions.SecretMounts` and `ExecOptions.SecretEnv`, which have no
  counterpart on the control plane, instead of sending them.
- `examples/sandbox` runs on hosted Cella when `TOPOS_CELLA_TOKEN` is set, at
  `https://api.latere.ai/v1/environments` unless `TOPOS_CELLA_URL` names
  another address.
- A Lux usage figure the gateway did not measure counts as zero.

- `ModelLux` with no `BaseURL` reaches `https://api.latere.ai/v1/models`,
  Latere's Lux core under the platform origin. The default was
  `https://lux.latere.ai`, the retired hosted gateway, whose host no longer
  resolves. A `BaseURL` set by the caller is unaffected.

## v0.6.0 - 2026-09-23

- **Breaking:** the PreToolUse hook payload names its validated tool input
  `normalized_input`, and the Go field is `PreToolUsePayload.NormalizedInput`.
  A hook that reads `normalised_input`, or a modified payload that sets it,
  must use the new key; a payload stored before this release carries the old
  one.

## v0.5.0 - 2026-09-18

- The identity gate now also reads the frontend for the retired admin flag
  and refuses a second copy of the authorizer envelope, the question and the
  decision that `latere.ai/x/pkg` declares once (ci-gate v0.42.0). Nothing
  changes for a user of Topos.

- The Cella provider's documentation describes the credential that exists.
  Cella issues no bearer of its own: present the short-lived token your issuer
  mints for the audience `sandboxd`. Because that token lives for minutes
  rather than days, a run that outlasts one token needs `TokenFunc`, which is
  asked per request, rather than a bearer bridged once at run start. No code
  changed; the `TokenSource` interface and its three implementations are
  unchanged.

- Agent tool grants now restrict the available tools and their execution,
  including delegated agents. Traces show the concrete tools offered.
- Native critics have no tools by default. Explicit grants enable selected
  tools; persisted empty grants remain empty after a JSON round-trip.
