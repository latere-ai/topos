---
title: "Scaffold and configuration reference: layout, the toposd roles, listeners, every TOPOS_* variable"
status: drafted
track: core
depends_on: [001-architecture.md]
affects: [cmd/toposd/, cmd/topos/, internal/config/, internal/version/, internal/serve/, internal/runnerrole/, internal/check/, internal/token/, Makefile, .lateregate.yaml, Dockerfile, .github/workflows/, .githooks/, docs/]
effort: small
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Scaffold and configuration reference

## Overview

A compiling, testable repository that passes the family gate before
any agent code exists: the Go module, the `toposd` binary with its two
listeners, its probes and its four roles, the `topos` binary's entry
point, typed configuration, the gate configuration, the developer
image, the verify workflow, and the community files. Every later spec
lands into a tree that already enforces the bar.

This spec is also the configuration reference. It owns the table of
every `TOPOS_*` variable any binary or test tier reads, including the
ones later specs give a meaning to, so an operator has one table. The
Owner column names the spec that defines each variable's behavior.

## Current state

The module path `latere.ai/x/topos` is kept; every API may break, and
no version the checksum database holds (v0.0.2 to v0.7.0) is tagged
again, so every new tag sorts above v0.7.0. v0.7.0 was a library with
no binary, configured through Go option structs. The retired hosted
service had its own `toposd` with a different variable set; none of its
variables are carried, and a variable of that service set on a new
`toposd` is ignored like any unknown variable.

## Design

### Layout

Every entry either is in the tree or names the spec that builds it.

```
api/                     openapi.yaml, the committed API document (015)
cmd/toposd/              main: the role dispatcher, configuration, listeners, run group
cmd/topos/               main of the scripting and test client (024)
cmd/topos-machine/       the static helper a Cella machine runs for search (009)
session/                 the v1 schema, the fold, the Store interface (004)
session/dir/             the directory store (004)
session/inputcheck/      the input check for secrets a client or the resolver runs (018)
harness/                 the loop, context, prompt assembly, the permission policy (harness.Policy), hooks, threads (005, 010-013)
harness/prompt/          the versioned harness prompt files, embedded (011)
harness/tools/           the built-in tools and the registry (008)
harness/tools/mcp/       MCP servers as tools (021)
models/                  the Model interface, the connection, the catalog entry, cost, budget (007)
models/dialect/          the Model over llmdialect's backend codecs (007)
models/scripted/         the scripted model, for tests only (026)
machine/                 the Machine interface (009)
machine/host/            the host directory and its operating-system sandbox (009, 012)
machine/cella/           a Cella sandbox through latere.ai/x/cella/client (009)
runner/                  claim, lease, drive, append, recover (016)
runner/checkpoint/       each turn's checkpoint and rewind (034)
memory/, memory/dir/, memory/arca/   memory stores and their backends (020)
manifest/, manifest/v1/  the four kinds and the resolver (003)
client/                  the toposd API client and the Store over it (024)
authorizer/              the action vocabulary (006)
internal/config/         typed configuration from the environment; every problem in one message
internal/version/        build identity set by -ldflags
internal/serve/          the serve role: API, store, in-process runners, triggers (015)
internal/runnerrole/     the runner role: outbound to a toposd internal listener (016)
internal/check/          the check role (028)
internal/token/          the token role and the local issuer (006)
internal/server/         the /v1 handlers (015)
internal/auth/           the verifier, the authorizer client, the owner policy (006)
internal/store/postgres/ the Postgres store (014)
internal/store/dirobjects/ agents, triggers, credentials and memory stores on the directory store (014)
internal/blob/           the blob store (014)
internal/queue/          claims and leases on the server (016)
internal/triggers/       schedules (022)
internal/events/         the sink client (023)
internal/credentials/    credential custody (018)
internal/egressproxy/    the host's local proxy for named secrets (018)
internal/gitcache/       the runner's git cache (019)
internal/toposcli/       the topos command: flags, output, exit codes (024)
internal/arch/           the architecture tests (001)
tools/catalog/           the generator of models/catalog.json (007)
skills/topos/            the skill that teaches an agent the topos command (024)
test/stubs/luxstub/      the stub Lux, an in-process test server (026)
test/stubs/              the sink and Cella stubs beside it (026)
test/e2e/                binaries against the stubs (026)
test/tasks/              the task suite and its checkers (025)
test/conformance/        the importable conformance suite (029)
examples/                example agents and embedding programs (028)
deploy/                  the compose file (028)
docs/                    for people who run toposd or build on the core; docs/history/ holds v0.7.0
specs/                   this deck
```

### Binary, roles and exit codes

`toposd -version` prints `toposd <version> (<commit>, <date>)` and
exits 0. The first argument that does not start with `-` selects a
role; without one the binary serves.

| Role | Reads | Does | Spec |
|---|---|---|---|
| `serve` (default) | the whole table | the API on the public listener, the probes and the runner routes on the internal listener, in-process runners, triggers | [[015-api]], [[016-runners]] |
| `runner` | `TOPOS_INTERNAL_URL`, `TOPOS_RUNNER_TOKEN`, `TOPOS_RUNNER_CAPACITY`, the model, machine, memory and git variables | claims sessions from a toposd internal listener and runs them; serves only the probes | [[016-runners]] |
| `check` | the whole table | one line per requirement of the installation, exit 1 on any failure | [[028-release-and-installation]] |
| `token` | `TOPOS_PUBLIC_URL`, `TOPOS_LOCAL_ISSUER_KEY`, `TOPOS_OIDC_AUDIENCE` | signs one API token with the local issuer's key, prints it, exits; opens no store | [[006-identity]] |

Until the spec that builds a role lands, the role prints one line
`toposd: <role>: not built; see specs/<NNN-name>.md` on stderr and
exits 1. An unknown role is a usage error.

| Exit code | Meaning |
|---|---|
| 0 | clean exit: a finished role, a drained shutdown, `-version` |
| 1 | failure: configuration, start-up, a failed check, a role not built |
| 2 | usage: an unknown role, a bad flag |

A configuration or start-up failure writes one line on stderr prefixed
`toposd:`.

### Listeners and probes

| Listener | Default address | Serves |
|---|---|---|
| public | `:8080` (`TOPOS_PUBLIC_ADDR`) | the API under `/v1` ([[015-api]]), or under `TOPOS_BASE_PATH` ([[030-shared-origin]]); `GET /livez`, `GET /readyz`, `GET /version` |
| internal | `:8081` (`TOPOS_INTERNAL_ADDR`) | the probes, `GET /metrics` ([[023-events-and-observability]]), and the runner routes under `/internal/v1` when `TOPOS_RUNNER_TOKEN` is set ([[016-runners]]) |

The probes are `latere.ai/x/pkg/health`.

| Method | Path | Body |
|---|---|---|
| GET | `/livez` | 200 `ok`, touches no dependency |
| GET | `/readyz` | 200 `ok` when every check passes; 503 `not ready: <check>: <error>` otherwise; `not ready: draining: shutting down` during shutdown |
| GET | `/version` | `{"version","commit","build_time"}` from `internal/version` |
| GET | `/metrics` | the metrics registry in the Prometheus text format; internal listener only |

Readiness runs `draining` and `store` with a 2 second budget. A model
provider, Cella or the git host being unreachable never fails
readiness: a session that needs one fails with a stop reason, and a
replica that cannot reach one serves every session that does not.

### Shutdown

On `SIGTERM` or `SIGINT`: readiness answers 503 at once; the process
waits a 3 second drain delay; the in-process runners stop claiming and
each running session is brought to its next step boundary within a 60
second drain grace, its events appended and its lease released. A tool
call still running at the end of the grace is canceled and its
`tool.result` records the cancellation ([[016-runners]]), so no step is
left without a result by a clean shutdown. Event streams to clients
close; a client reconnects from its last sequence. The HTTP servers
then close with a 10 second grace. A Deployment sets
`terminationGracePeriodSeconds` to 90.

### Configuration

Every variable is read once at start by `internal/config.Load`, which
collects every problem and fails with one message
`configuration: <problem>; <problem>` sorted by variable name. A blank
value is unset. An unknown variable is never an error. Durations are Go
durations (`90s`, `5m`). Rows marked "added by the deck" are variables
the behaviors of later specs cannot exist without.

| Variable | Required | Default | Owner | Purpose |
|---|---|---|---|---|
| `TOPOS_PUBLIC_ADDR`, `TOPOS_INTERNAL_ADDR` | no | `:8080`, `:8081` | this spec | listen addresses; must differ unless both ask for port 0 |
| `TOPOS_PUBLIC_URL` | `serve`, `token` | none | [[030-shared-origin]] | the absolute URL clients reach the public listener at; the base of every URL toposd writes, and the local issuer's name |
| `TOPOS_BASE_PATH` | no | unset | [[030-shared-origin]] | the path the API answers under in the place of `/v1`; set, it equals the path of `TOPOS_PUBLIC_URL` |
| `TOPOS_TRUSTED_PROXIES` | no | unset | [[030-shared-origin]] | CIDR ranges whose `X-Forwarded-For` and `X-Forwarded-Proto` are believed; unset trusts no header |
| `TOPOS_OIDC_ISSUERS` | `serve`, unless `TOPOS_LOCAL_ISSUER_KEY` is set | none | [[006-identity]] | comma separated issuer URLs whose tokens the API accepts |
| `TOPOS_OIDC_AUDIENCE` | no | `topos` | [[006-identity]] | a comma list of audiences a token may carry, the first the primary |
| `TOPOS_OIDC_INSECURE_ISSUERS` | no | unset | [[006-identity]] | issuers from the list that may use `http://` on a host other than loopback; the stubs set it, production never does |
| `TOPOS_AUTHORIZER_URL`, `TOPOS_AUTHORIZER_TOKEN` | no | unset | [[006-identity]] | the authorizer endpoint and the bearer toposd sends it; unset selects the owner policy; the URL without the token is a start-up failure |
| `TOPOS_ADMIN_SUBJECTS` | no | unset | [[006-identity]] | subjects the owner policy lets act on every object; read and unused when an authorizer is set |
| `TOPOS_LOCAL_ISSUER_KEY` | no | unset | [[006-identity]] | a PEM PKCS#8 private key (ECDSA P-256 or RSA of at least 2048 bits); set, the API accepts tokens `toposd token` signs, issued as `TOPOS_PUBLIC_URL` |
| `TOPOS_DATA_DIR` (added by the deck) | no | `$XDG_STATE_HOME/topos`, falling back to `$HOME/.local/state/topos` | [[004-session-log]], [[014-store]] | the directory store's root, and the blob, worktree, checkpoint and memory directories beneath it when nothing else names them |
| `TOPOS_DB_URL`, `TOPOS_DB_POOL_URL` | no | unset | [[014-store]] | a Postgres URL, and an optional pooled URL for serving queries; unset, toposd runs on the directory store under `TOPOS_DATA_DIR` |
| `TOPOS_BLOB_URL` (added by the deck) | no | unset | [[014-store]] | where raw response bodies and captured requests are kept: `file:///path` or `s3://<host>/<bucket>/<prefix>`; unset keeps them in the store |
| `TOPOS_BLOB_ACCESS_KEY`, `TOPOS_BLOB_SECRET_KEY` (added by the deck) | when `TOPOS_BLOB_URL` is `s3://` | none | [[014-store]] | the object store's credential; there is no credential chain |
| `TOPOS_EVENTS_URL`, `TOPOS_EVENTS_SECRET` | no | unset | [[023-events-and-observability]] | the event sink and its HMAC key; events are off when the URL is unset; the URL without the secret is a start-up failure |
| `TOPOS_CREDENTIALS_KEY` | `serve`, from [[018-credentials-and-secrets]] | none | [[018-credentials-and-secrets]] | one to eight 32-byte keys, standard base64, comma separated; the first wraps every new data key, every one is tried to open |
| `TOPOS_RUNNER_TOKEN` | `runner`; `serve` to accept outside runners | unset | [[016-runners]] | bearers of the runner routes, comma separated; the first is sent, every one is accepted, so rotation is prepending; unset, `serve` mounts no runner route |
| `TOPOS_INTERNAL_URL` (added by the deck) | `runner` | none | [[016-runners]] | the URL the runner role reaches a toposd internal listener at |
| `TOPOS_RUNNER_CAPACITY` (added by the deck) | no | `16` | [[016-runners]] | sessions one runner process holds at once; `0` in `serve` runs no in-process runner |
| `TOPOS_MODELS_URL` | `serve`, `runner` | none | [[007-models]] | the default model connection's base URL, a Lux address or a provider's; a server role refuses to start without it |
| `TOPOS_MODELS_KEY` (added by the deck) | no | unset | [[007-models]] | the installation's model credential, used for a session whose agent names no model credential; unset, such a session fails at its first step with `model_credential_missing` |
| `TOPOS_CELLA_URL` | no | unset | [[009-machines]] | the Cella control plane a `cella` machine is created on; unset, a session asking for one is refused `machine_unavailable` |
| `TOPOS_ORIGO_URL` | no | unset | [[034-checkpoints-and-rewind]], [[019-git]] | the git host that holds session repositories for cloud checkpoints; unset, a cloud session without a repository keeps no checkpoint and cannot be handed off |
| `TOPOS_RUNNER_GIT_CACHE` | no | unset | [[019-git]] | a directory of bare mirrors the runner fetches through before cloning into a machine |
| `TOPOS_MEMORY_BACKEND` | no | `dir` | [[020-memory-stores]] | `dir` or `arca`: where memory store documents are kept |
| `TOPOS_MEMORY_ARCA_URL` | when the backend is `arca` | none | [[020-memory-stores]] | the Arca files plane's base URL |
| `TOPOS_MEMORY_DIR` | no | `$TOPOS_DATA_DIR/memory` | [[020-memory-stores]] | the root of the `dir` backend |
| `TOPOS_MEMORY_SYNC_INTERVAL` | no | `5m` | [[020-memory-stores]] | how often a running turn syncs its attached stores between the sync points |
| `TOPOS_URL`, `TOPOS_TOKEN` (added by the deck) | for `topos` commands that reach a server | unset | [[024-client-cli-skill]] | the toposd a client talks to and the bearer it sends; unset, `topos run` runs a local session |
| `TOPOS_TEST_URL` | no | unset | [[029-conformance]] | a running toposd the conformance suite targets; unset, the suite starts one in process |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_*` | no | unset | [[023-events-and-observability]] | the standard OpenTelemetry variables, read by `latere.ai/x/pkg/otel`; telemetry is off without the endpoint |

### Local run

`make run` builds both binaries and runs `toposd serve` on loopback on
the directory store under a temporary `TOPOS_DATA_DIR`, with the local
issuer and the owner policy. Once [[026-stubs-and-tiers]] lands it
starts the stub Lux beside it, sets `TOPOS_MODELS_URL` to it, and
prints a token, so a clean clone runs a session in one command with no
paid credential.

### The gate

`make` is `go tool lateregate`, pinned in `go.mod` as a tool.
`.lateregate.yaml` holds what this repository chose: the spec
vocabulary (the statuses of the lifecycle in `specs/README.md`, the nine
required frontmatter fields, the five required sections, wikilinks,
numbering, table shape, the `.archive` directory), the hermetic
allowance (`/bin` and `/usr/bin`, for `sh` and `git`, which the host
machine's tests execute), the `depcheck` allow lists, and the license
(Apache-2.0, holder Latere AI). A waiver is a line in that file with a
reason and a date.

### Images and workflows

`Dockerfile` builds the developer image: it compiles inside the image
and runs `toposd` as a non-root user with both ports exposed and a
volume at `/var/lib/topos`, which it sets as `TOPOS_DATA_DIR`. The
release images are [[028-release-and-installation]]'s. `verify.yml`
runs on every push to `main` and every pull request: the gate job calls
the shared lateregate workflow, `tidy` checks `go mod tidy -diff`, and
`image` builds the developer image and asks it for its version. Every
third-party action is pinned by commit.

### Community files

`README.md` says what the core is and what works today. `LICENSE` is
Apache-2.0. `CONTRIBUTING.md` says how to build, the bar, specs first,
and where a package belongs. `CODE_OF_CONDUCT.md` is the Contributor
Covenant 2.1. `SECURITY.md` names the reporting address and the
properties the design commits to. `CHANGELOG.md` has one section per
release, and a tag without one is refused. `AGENTS.md` carries the
conventions an agent working in the tree follows, the first being that
the repository is public.

## Not in this spec

The release pipeline, the release images and the compose file
([[028-release-and-installation]]); the stubs and tiers
([[026-stubs-and-tiers]]); the behavior behind each variable (its Owner
column); the routes ([[015-api]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| `toposd -version` prints the identity and exits 0; an unknown role and a bad flag exit 2 | `TestVersionFlagPrintsTheIdentity`, `TestUnknownRoleIsAUsageError`, `TestBadFlagIsAUsageError` | not built |
| `toposd runner`, `toposd check` and `toposd token` each exit 1 with one line naming their spec until that spec lands | `TestUnbuiltRoleNamesItsSpec` | not built |
| A configuration with two problems fails with one line sorted by variable name, exit 1 | `TestLoadReportsEveryProblemInOneSortedMessage` | not built |
| Every variable in the table is read, every unset variable takes its default, and a blank value is unset | `TestLoadReadsEveryVariable`, `TestLoadAppliesEveryDefault`, `TestLoadTreatsBlankAsUnset` | not built |
| `TOPOS_AUTHORIZER_URL` without its token, and `TOPOS_EVENTS_URL` without its secret, are start-up failures | `TestLoadRefusesURLWithoutItsSecret` | not built |
| The two listeners serve the probes; `/metrics` is on the internal listener only; the same address for both is refused | `TestProbesOnBothListeners`, `TestMetricsInternalOnly`, `TestLoadRefusesOneSocketForBothListeners` | not built |
| On SIGTERM readiness answers 503 at once and the process exits 0 within the drain grace with no session left mid-step | `TestSigtermDrains` | not built |
| The variable table in this spec and the variables the binaries and the test tiers read (`internal/config` for the server roles, `internal/toposcli` for `TOPOS_URL` and `TOPOS_TOKEN`, the conformance suite for `TOPOS_TEST_URL`) are the same set | `TestConfigurationTableMatchesTheSpec` | not built |
| The gate passes on the scaffold: the family gate's first green run | the `gate` job of `verify.yml` | not built |
