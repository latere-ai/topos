# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

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
  exits, and background jobs that end with the session.

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
