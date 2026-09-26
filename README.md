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

Today `toposd` serves its probes and reads its configuration, and nothing
else is built. The design is the numbered deck in [specs](specs), and the
build order is its README's phases. The first release above v0.7.0,
v0.8.0, is cut when phase 1 closes: a harness that meets the task suite's
bar against a real model.

## Try it

Topos requires Go 1.27 or later.

```sh
git clone https://github.com/latere-ai/topos.git
cd topos
make build
./out/toposd -version
make run   # serves /livez, /readyz and /version on 127.0.0.1:8080
```

## Identity

`toposd` verifies a caller's token against the OIDC issuers an operator
lists and asks one authorizer for every action; with no authorizer
configured, a built-in owner policy decides. It decides nothing about a
person itself. Spec [006](specs/006-identity.md) is the contract.

## Documentation

- [specs/README.md](specs/README.md): the design, the index of specs and
  the build order.
- [docs/configuration.md](docs/configuration.md): every `TOPOS_*` variable.

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) covers the quality gate and the layout.
Report a vulnerability as [SECURITY.md](SECURITY.md) describes, never in a
public issue.

## License

[Apache-2.0](LICENSE).
