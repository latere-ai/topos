# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

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
- A hosted session that works in a repository on the git host of
  `TOPOS_ORIGO_URL` keeps each turn's checkpoint at that repository,
  under `refs/topos/checkpoints/<session>/latest`, pushed from its
  sandbox with the session's git credential and the push option
  `origo.event=off`, so a fork of the session restores the files it
  left, committed or not, after its sandbox is gone. A turn's checkpoint
  names that repository as `remote` once it is there. Whoever may read
  the repository may read these refs; they stay until the repository or
  the ref is deleted, and deleting the session leaves them.
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
