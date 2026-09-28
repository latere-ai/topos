# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

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
  every sandbox Secret carries, for a Cella whose authorizer places what
  a caller creates by labels, such as its tenant.

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
