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
working directory, or a [Cella](https://github.com/latere-ai/cella) sandbox.
Models are reached through [Lux](https://github.com/latere-ai/lux) or
directly through a provider.

## Project status

The repository restarted on 2026-09-26. The Go packages of v0.7.0, an
embeddable runtime, are gone from `main` and stay available at their tags
through the Go module proxy; [docs/history](docs/history) holds their specs.

The core's releases start at v0.9.0 (2026-09-29); the latest is v0.11.1
(2026-10-02). Built and released:

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
- a connection to a decision service: `topos run` can get a suggested
  verdict for each tool call from an external service.

Not built yet: write-only credentials and connections, memory stores, MCP
servers, external runners and handoff, the event sink, the conformance suite
and the release archives. The task suite runs, but no release has measured
its bar against a pinned model yet. In the [specs](specs) deck, 13 specs are
complete, 18 in progress and 8 drafted or vague, none of those 8 built yet;
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

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) covers the quality gate and the layout.
Report a vulnerability as [SECURITY.md](SECURITY.md) describes, never in a
public issue.

## License

[Apache-2.0](LICENSE).
