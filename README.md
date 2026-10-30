# Topos

[![verify](https://github.com/latere-ai/topos/actions/workflows/verify.yml/badge.svg)](https://github.com/latere-ai/topos/actions/workflows/verify.yml)
[![Go Reference](https://pkg.go.dev/badge/latere.ai/x/topos.svg)](https://pkg.go.dev/latere.ai/x/topos)
[![License](https://img.shields.io/github/license/latere-ai/topos)](LICENSE)

**An open core for running agents: agent definitions, the sessions people
have with them, and the loop that runs a session wherever it runs.**

## Problem

An agent session is a conversation, a set of tool calls against a machine,
and the model calls in between. Most harnesses keep that session in the
process that runs it: close the terminal or lose the pod and the work is
gone, and a session started on a laptop cannot continue in the cloud or be
watched from anywhere else.

## How it works

Topos is three parts over one session schema.

- **The session log** is an append-only record of typed events: the
  messages, every tool call and its result, every model request, and the
  session's status. The transcript a model sees is a pure function of the
  log, so any runner continues a session from it.
- **The harness** is the loop: it builds each model request from the log,
  calls the model, runs the tool calls on the session's machine under the
  session's permissions, and compacts the context when it grows.
- **A runner** holds one session's write and drives the harness. It runs
  inside the `topos` command on a laptop, embedded in an application, or as
  the `toposd runner` role beside a server.

A session's tools act on one machine: the runner's host, confined to a
working directory, a [Cella](https://github.com/latere-ai/cella) sandbox, or
a person's own computer, in a folder they granted, through the Cella
environment it serves.
Models are reached through [Lux](https://github.com/latere-ai/lux) or
directly through a provider.

## Project status

The repository restarted on 2026-09-26. The Go packages of v0.7.0, an
embeddable runtime, are gone from `main` and stay available at their tags
through the Go module proxy; [docs/history](docs/history) holds their specs.

The core's releases start at v0.9.0 (2026-09-29). Built and released as
of v0.36.0 (2026-10-13):

- the session log, kept in memory, in a directory or in Postgres, and the
  harness: the built-in tools, the permission modes, subagents, context
  compaction and a checkpoint of the working directory at every turn;
- `topos run`, `topos confirm` and `topos rewind`, which run a session on
  your machine with no server;
- `toposd`, which serves the API under `/v1` with its OpenAPI document and
  runs hosted sessions on Cella sandboxes, in its own process or as
  separate runners: agents owned by a person or an organization, sessions
  that can be forked, archived and summarized, and triggers that fire on a
  schedule or on delivered events;
- sessions a person attends: a `question` tool that waits for the person's
  answer, an approval mode and a title that change after a session starts,
  a message edited with the session's forks kept as a tree, search over
  what sessions said, and delete;
- more for the agent to work with: `web_search` through the search service
  the installation configures, a session's files, the images an answer
  shows and the files it links served to a client and, where the
  installation runs a files service, kept in a folder of the
  conversation's own, today's date on every turn, and the sites a
  session's machine may reach set per session;
- `publish` of a folder as a site: the agent gives a site its name and
  address the first time it publishes it, a session's app starts as the
  app host makes it, and opening an app to anyone who has its link, or
  closing it again, is a call the person approves every time;
- a machine that recovers: a session's sandbox that failed is started
  again with its files rather than replaced, a command that was running
  when its machine stopped says what happened, and a session whose
  machine was replaced, or a fork with no checkpoint, gets its working
  directory rebuilt from its own log;
- models an authorizer chooses: a routed model name, how much a model
  reasons, and a move to another model inside the turn when one cannot
  serve;
- connections: the services a person connected, named by the authorizer
  at the create, a fork and every message. A service such as a code host
  is reached from the sandbox, `git` and the service's API through the
  installation's connection service, with no credential in the machine;
  a remote MCP server's tools are offered to the model; and
  `request_connection` asks the person to connect a service the session
  needs;
- a connection to a decision service: `topos run` can get a suggested
  verdict for each tool call from an external service.

Built since and in no release yet: a connection service on the host the
model gateway publishes under no longer stops every hosted session. Serve
and the runner refuse that configuration at their start, and a sandbox
whose connections token would share a host with another of its secrets
opens without it. An agent that names `history` reads back what its
context cleared or summarized, from the session's log as it was written.

Not built yet: write-only credentials, memory stores, MCP servers an
agent's definition names, external runners and handoff, the event sink,
the conformance suite and the release archives. The task suite runs, but
no release has measured its bar against a pinned model yet. In the
[specs](specs) deck, 22 specs are complete, 39 in progress or in testing
and 8 drafted or vague, none of those 8 built yet;
[specs/README.md](specs/README.md) says which build phases are closed.

## Try it

Topos requires Go 1.27 or later.

```sh
git clone https://github.com/latere-ai/topos.git
cd topos
make build
./out/toposd -version
export TOPOS_MODELS_URL=https://lux.example/v1/models   # a Lux gateway's root, or a provider's API
export TOPOS_MODELS_KEY=...                             # its credential, for sessions that name none
make run     # the API on 127.0.0.1:8080, with a local issuer and the owner policy
make token   # in another shell: a token for that server's admin
```

`toposd` refuses to start without a model connection, and reads the
discovery document of a Lux root at start.
[docs/configuration.md](docs/configuration.md) lists every variable.

## Identity

`toposd` verifies a caller's token against the OIDC issuers an operator
lists and asks one authorizer for every action; with no authorizer
configured, a built-in owner policy decides. It decides nothing about a
person itself. Spec [006](specs/.archive/006-identity.md) is the contract.

## Documentation

- [specs/README.md](specs/README.md): the design, the index of specs and
  the build order.
- [docs/configuration.md](docs/configuration.md): every `TOPOS_*` variable.
- [docs/questions.md](docs/questions.md): how a client shows an agent's
  question and sends the person's answer.
- [docs/web-search.md](docs/web-search.md): turning on `web_search`, and
  the contract a search service answers.
- [docs/reading-back.md](docs/reading-back.md): the `history` tool, which
  reads back what a long session's context cleared or summarized.
- [docs/searching-sessions.md](docs/searching-sessions.md): finding a
  caller's sessions by what was said in them, and showing the match.
- [docs/editing-a-message.md](docs/editing-a-message.md): sending an
  edited message from a point in a conversation, and showing its
  versions.
- [docs/network.md](docs/network.md): where a session's sandbox may
  reach, how a person widens it, and the initiator's instructions.
- [docs/connections.md](docs/connections.md): the services a person
  connected, reached from a session without their credentials.
- [docs/titles.md](docs/titles.md): a session's title that follows its
  conversation, turned on by an agent's `spec.title`.
- [docs/offers.md](docs/offers.md): how a client shows an agent's offer of
  standing work, or of a change to it, and sends the answer of the person
  who started the session.
- [docs/holding-triggers.md](docs/holding-triggers.md): the one switch that
  pauses and resumes every trigger a person owns.
- [docs/quiet-runs.md](docs/quiet-runs.md): how a client tells a trigger's
  run that brought the person something from a quiet one, and shows the
  agent's notice.
- [docs/wakes.md](docs/wakes.md): how a client shows the wake an agent set
  to come back to a conversation later, the turn it starts, and how to
  cancel it.
- [docs/reading-files.md](docs/reading-files.md): reading the files a
  person sent, an answer kept and the working directory holds, with their
  media types and sizes.
- [docs/allowances.md](docs/allowances.md): a weekly allowance on a
  trigger, the week its runs have spent, and the pace each run is told.
- [docs/unattended-turns.md](docs/unattended-turns.md): how a turn
  nobody watches is decided, and the `attended` the log records.
- [docs/context-report.md](docs/context-report.md): how full a
  conversation's context is, and what fills it.
- [docs/cost-and-progress.md](docs/cost-and-progress.md): what a session
  has cost so far and how far its to-do list is, kept current from its
  event stream.
- [docs/trigger-runs.md](docs/trigger-runs.md): every trigger's runs with
  what each cost, the cost of a typical run, and when each fires next,
  read at once for a forecast.
- [docs/trigger-models.md](docs/trigger-models.md): the model and the
  reasoning level a trigger's runs start on, in place of the agent's.
- [docs/trigger-times.md](docs/trigger-times.md): a trigger that fires at
  a list of times, each once, for times no schedule names.
- [docs/memory-folders.md](docs/memory-folders.md): folders of a files
  service a session keeps across sessions, which its file tools reach at
  `/memory/<name>/` with no sandbox.
- [docs/trigger-reach.md](docs/trigger-reach.md): a trigger's reach, a
  kind the authorizer maps, and a firing that runs once per due scope or
  not at all when nothing is new.
- [docs/messages-while-working.md](docs/messages-while-working.md): a
  message sent while the agent works, read at its next step, sent to be
  read now, or withdrawn before the agent reads it.

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) covers the quality gate and the layout.
Report a vulnerability as [SECURITY.md](SECURITY.md) describes, never in a
public issue.

## License

[Apache-2.0](LICENSE).
